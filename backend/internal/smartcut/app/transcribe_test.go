package app

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/li904338203/tabtv/backend/internal/smartcut/domain"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

type quoteMaterial struct {
	MaterialFacts
	have map[int64]domain.Transcript
}

func (m quoteMaterial) Transcripts(context.Context, int64, []int64, TranscriptNeed) (map[int64]domain.Transcript,
	map[int64]string, error) {
	return m.have, nil, nil
}

type quoteCatalog struct{ models []ModelChoice }

func (c quoteCatalog) SpeechModels(context.Context, int64) ([]ModelChoice, error) {
	return c.models, nil
}
func (c quoteCatalog) QuoteSpeech(_ context.Context, _ string, _ int64, sec int) (SpeechQuote, *Unavailable, error) {
	return SpeechQuote{Credits: sec * 2, UnitPrice: 2, Unit: "per_second"}, nil, nil
}

// 42217 asr_missing 补上「先识别语音 ✦N」要的信息：缺结果的素材编号、按第一个字级 / 词级模型的报价（逐份向上取整到秒）。
func TestWithTranscribeQuote_缺识别结果时带报价(t *testing.T) {
	d := func(ms int) *int { return &ms }
	items := []domain.Input{
		{No: "1", Kind: domain.InputVideo, Status: "ready", MediaID: "11", DurationMs: d(10500)},
		{No: "2", Kind: domain.InputVideo, Status: "ready", MediaID: "12", DurationMs: d(4000)},
		{No: "3", Kind: domain.InputVideo, Status: "ready", MediaID: "13", DurationMs: d(3000)},
		{No: "4", Kind: domain.InputImage, Status: "ready", MediaID: "14"},
	}
	s := &Service{}
	s.plan.Material = quoteMaterial{have: map[int64]domain.Transcript{13: {Granularity: "char"}}}
	s.transcribe.Catalog = quoteCatalog{models: []ModelChoice{{Code: "oai-text", Granularity: "text"}, {Code: "volc", Granularity: "char"}}}
	orig := unsuitableErr(&domain.ErrUnsuitable{Reason: domain.UnsuitableASRMissing, Detail: "口播精剪要先识别语音"})
	err := s.withTranscribeQuote(context.Background(), PlanRequest{Mode: domain.ModeTalk}, items, orig)
	var be *httpx.BizError
	if !errors.As(err, &be) || be.Code != errcode.SmartcutInputUnsuitable || be.Message != "口播精剪要先识别语音" {
		t.Fatalf("仍是 42217、原文不变: %v", err)
	}
	if !reflect.DeepEqual(be.Details["missing"], []string{"1", "2"}) {
		t.Fatalf("缺识别结果的编号: %v", be.Details["missing"])
	}
	q, _ := be.Details["quote"].(map[string]any)
	// 11 秒 + 4 秒 = 15 秒 × 2 = 30；只有整段文字的模型不能用于口播精剪，跳过。
	if q["credits"] != 30 || q["modelCode"] != "volc" || q["durationSec"] != 15 || q["keepFillers"] != true {
		t.Fatalf("报价: %v", q)
	}
	if !reflect.DeepEqual(be.Details["options"], []string{"transcribe"}) {
		t.Fatalf("原来的出路要保留: %v", be.Details["options"])
	}

	// 没有字级 / 词级的模型：只带缺的编号，不带报价（前端照样给出路，点了再选模型）。
	s.transcribe.Catalog = quoteCatalog{models: []ModelChoice{{Code: "oai-text", Granularity: "text"}}}
	err = s.withTranscribeQuote(context.Background(), PlanRequest{Mode: domain.ModeTalk}, items, orig)
	if errors.As(err, &be); be.Details["quote"] != nil || be.Details["missing"] == nil {
		t.Fatalf("没有可用模型时不报价: %v", be.Details)
	}
	// 别的错误原样返回。
	other := httpx.Errf(errcode.InvalidArgument, "x")
	if got := s.withTranscribeQuote(context.Background(), PlanRequest{Mode: domain.ModeTalk}, items, other); got != other {
		t.Fatal("不是 asr_missing 的错误原样返回")
	}
}
