package app

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/li904338203/tabtv/backend/internal/smartcut/domain"
	"github.com/li904338203/tabtv/backend/internal/smartcut/repo"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// smartcut.updated 的载荷与 api/events/smartcut-updated.schema.json 逐字段一致（必填都在、不多出 schema 外的键）。
func TestSmartcutUpdated_与schema一致(t *testing.T) {
	raw, err := os.ReadFile("../../../../api/events/smartcut-updated.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Required   []string                   `json:"required"`
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	id, stage, status := "12", repo.StagePlan, repo.StatusRunning
	b, _ := json.Marshal(SmartcutUpdated{Type: SmartcutUpdatedType, OccurredAt: "2026-10-28T00:00:00Z", ProjectID: "1",
		TimelineID: "2", CanvasNodeID: "sc", CallID: &id, Stage: &stage, Status: &status})
	var got map[string]any
	_ = json.Unmarshal(b, &got)
	for k := range got {
		if _, ok := schema.Properties[k]; !ok {
			t.Errorf("载荷多出 schema 外的键 %q", k)
		}
	}
	for _, k := range schema.Required {
		if _, ok := got[k]; !ok {
			t.Errorf("载荷缺必填键 %q", k)
		}
	}
	var stages struct {
		Enum []any `json:"enum"`
	}
	_ = json.Unmarshal(schema.Properties["stage"], &stages)
	for _, s := range []string{repo.StageSync, repo.StageUnderstand, repo.StagePlan, repo.StageCompile, repo.StageApply, repo.StageDone} {
		found := false
		for _, e := range stages.Enum {
			found = found || e == s
		}
		if !found {
			t.Errorf("阶段 %s 不在 schema 里", s)
		}
	}
}

// 识别状态的事件（SCT-33）带 analysis 段：键与取值都在 schema 里（analysis 的 additionalProperties 是 false）。
func TestSmartcutUpdated_识别事件的analysis段与schema一致(t *testing.T) {
	raw, err := os.ReadFile("../../../../api/events/smartcut-updated.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Properties struct {
			Analysis struct {
				Required   []string `json:"required"`
				Properties map[string]struct {
					Enum []any `json:"enum"`
				} `json:"properties"`
			} `json:"analysis"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(SmartcutUpdated{Type: SmartcutUpdatedType, OccurredAt: "2026-10-28T00:00:00Z", ProjectID: "1",
		TimelineID: "2", CanvasNodeID: "sc", Analysis: &SmartcutAnalysisEvent{MediaID: "9", Kind: "asr", Status: "success"}})
	var got struct {
		Analysis map[string]any `json:"analysis"`
	}
	_ = json.Unmarshal(b, &got)
	a := schema.Properties.Analysis
	for k, v := range got.Analysis {
		p, ok := a.Properties[k]
		if !ok {
			t.Errorf("analysis 多出 schema 外的键 %q", k)
			continue
		}
		if len(p.Enum) > 0 {
			found := false
			for _, e := range p.Enum {
				found = found || e == v
			}
			if !found {
				t.Errorf("analysis.%s = %v 不在 schema 的枚举里", k, v)
			}
		}
	}
	for _, k := range a.Required {
		if _, ok := got.Analysis[k]; !ok {
			t.Errorf("analysis 缺必填键 %q", k)
		}
	}
	for _, st := range []string{"running", "success", "failed"} {
		found := false
		for _, e := range a.Properties["status"].Enum {
			found = found || e == st
		}
		if !found {
			t.Errorf("识别推送的状态 %s 不在 schema 里", st)
		}
	}
}

func TestSpecOf_字幕缺省按模式(t *testing.T) {
	yes, no := true, false
	cases := []struct {
		mode, instruction string
		with              *bool
		want              bool
	}{
		{domain.ModeMix, "剪一版", nil, false},
		{domain.ModeMix, "剪一版，加上字幕", nil, true},
		{domain.ModeMix, "剪一版", &yes, true},
		{domain.ModeBoard, "", nil, true},
		{domain.ModeTalk, "", nil, true},
		{domain.ModeTalk, "", &no, false},
		{domain.ModeNarr, "", nil, true},
	}
	for _, c := range cases {
		got := specOf(PlanRequest{Mode: c.mode, Instruction: c.instruction, WithSubtitles: c.with, TargetSec: 20})
		if got.WithSubtitles != c.want || got.TargetMs != 20000 {
			t.Errorf("%s %q %v：期望字幕 %v，实际 %+v", c.mode, c.instruction, c.with, c.want, got)
		}
	}
}

func TestUnsuitableErr_给出路(t *testing.T) {
	cases := map[string]string{
		domain.UnsuitableNoMusic: "connect_audio,ignore_beat", domain.UnsuitableNoVoice: "connect_audio",
		domain.UnsuitableASRMissing: "transcribe", domain.UnsuitableNoShots: "fill_shot",
		domain.UnsuitableNoPicture: "connect_material",
	}
	for reason, want := range cases {
		err := unsuitableErr(&domain.ErrUnsuitable{Reason: reason, Detail: "说明"})
		var be *httpx.BizError
		if !errors.As(err, &be) || be.Code != errcode.SmartcutInputUnsuitable || be.Details["reason"] != reason ||
			strings.Join(be.Details["options"].([]string), ",") != want {
			t.Errorf("%s：%v %+v", reason, err, be)
		}
	}
}

func TestWithCallID_冲突详情带上调用记录(t *testing.T) {
	base := httpx.Errf(errcode.VersionConflict, "林澈 修改了这条时间线").WithDetails(map[string]any{"currentVersion": 7, "bySelf": false})
	err := withCallID(base, 42)
	var be *httpx.BizError
	if !errors.As(err, &be) || be.Code != errcode.VersionConflict || be.Details["callId"] != "42" ||
		be.Details["currentVersion"] != 7 || !strings.Contains(be.Message, "AI 剪辑期间时间线有改动") {
		t.Fatalf("%+v", be)
	}
	if base.Details["callId"] != nil {
		t.Fatal("不改原来的错误")
	}
}

// compileRuns：一套失败不影响其余（SC-AC-45 后端部分）；全是调用失败就报错；有解析不合规的走兜底。
func TestCompileRuns_部分失败与兜底(t *testing.T) {
	s := &Service{}
	cards := []domain.Card{{No: "1", Kind: domain.InputVideo, MediaID: 71, DurationMs: 4000},
		{No: "2", Kind: domain.InputVideo, MediaID: 72, DurationMs: 4000}}
	f := planFacts{in: domain.PlanInput{Mode: domain.ModeMix, Cards: cards, Base: testContent()}}
	ok := variantRun{ok: true, parsed: domain.ParseResult{Draft: domain.Draft{Mode: domain.ModeMix, Segments: []domain.Segment{
		{Clip: "2", Take: domain.Take{Kind: domain.TakeFull}, Role: domain.SegBody, Weight: 1, Transition: domain.TransCut}}}}}
	r := &planRun{}
	out, applied, err := s.compileRuns(r, f, []variantRun{{callErr: errors.New("上游 502")}, ok,
		{invalid: &domain.ErrDraftInvalid{Reason: "坏了", Snippet: "x"}}})
	if err != nil || applied != 1 || r.fallback {
		t.Fatalf("第二套成功就写它：%d %v %v", applied, err, r.fallback)
	}
	if out.Variants[0].Status != repo.StatusFailed || !strings.Contains(out.Variants[0].Error, "上游 502") ||
		out.Variants[2].Status != repo.StatusFailed || !strings.Contains(out.Variants[2].Error, "坏了") ||
		out.Variants[1].Status != repo.StatusSuccess || out.Variants[1].Draft == nil {
		t.Fatalf("各套的结果：%+v", out.Variants)
	}
	if _, _, err := s.compileRuns(&planRun{}, f, []variantRun{{callErr: &callTimeout{}}}); !errors.As(err, new(*httpx.BizError)) ||
		err.(*httpx.BizError).Code != errcode.ProviderTimeout {
		t.Fatalf("全是调用失败：报超时，不写时间线：%v", err)
	}
	r = &planRun{}
	out, applied, err = s.compileRuns(r, f, []variantRun{{invalid: &domain.ErrDraftInvalid{Reason: "坏了", Snippet: "原文"}}})
	if err != nil || applied != 0 || !r.fallback || !out.Variants[0].Fallback || !out.Variants[0].UsedFallback ||
		!strings.Contains(r.errDetail, "原文开头：原文") {
		t.Fatalf("解析不合规走兜底：%+v %v %+v", out.Variants, err, r)
	}
}

func TestPlanUserMessage_按模式组上下文(t *testing.T) {
	f := planFacts{in: domain.PlanInput{Mode: domain.ModeMix, Spec: domain.Spec{WithSubtitles: true},
		Cards: []domain.Card{{No: "1", Kind: domain.InputVideo, DurationMs: 4120, HasAudio: true, Description: "产品\n特写"},
			{No: "2", Kind: domain.InputImage}, {No: "9", Kind: domain.InputAudio, AudioRole: domain.RoleMusic, DurationMs: 30000}},
		Beats: map[string]domain.Beats{"9": {BeatsMs: []int{0, 500, 1000, 1500}}}}}
	msg := planUserMessage(PlanRequest{Mode: domain.ModeMix, TargetSec: 15, AspectRatio: "9:16", Variants: 2, AllowSlow: true}, f,
		[]Mosaic{{Rows: []string{"2"}}}, 1, 2)
	for _, want := range []string{"（没有写要求）", "- 目标时长：15 秒", "- 画幅：9:16", "- 字幕：要",
		"卡点：配乐编号 9，约 120 BPM", "允许放慢（最低 0.75 倍）", "- 编号 1：视频，4.1 秒，有声音。描述：产品 特写",
		"- 编号 2：图片。画面：见拼图 1 第 1 行", "- 编号 9：音频（配乐），30 秒，约 120 BPM", "拼图 1：第 1 行是编号 2",
		"这是第 2 套剪法（共 2 套）"} {
		if !strings.Contains(msg, want) {
			t.Errorf("缺「%s」：\n%s", want, msg)
		}
	}
}
