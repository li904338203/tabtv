package wiring

import (
	"context"
	"encoding/json"
	"strconv"
	"time"

	catalogapp "github.com/li904338203/tabtv/backend/internal/catalog/app"
	genapp "github.com/li904338203/tabtv/backend/internal/generation/app"
	gendomain "github.com/li904338203/tabtv/backend/internal/generation/domain"
	genrepo "github.com/li904338203/tabtv/backend/internal/generation/repo"
	mediaapp "github.com/li904338203/tabtv/backend/internal/media/app"
	mediadomain "github.com/li904338203/tabtv/backend/internal/media/domain"
	mediarepo "github.com/li904338203/tabtv/backend/internal/media/repo"
	"github.com/li904338203/tabtv/backend/internal/provider/protocol"
	smartcutapp "github.com/li904338203/tabtv/backend/internal/smartcut/app"
	timelineapp "github.com/li904338203/tabtv/backend/internal/timeline/app"
)

// 语音识别的装配（docs/agent/smartcut SCT-33）：目录（报价）、生成（受理）、媒体（音轨与识别行）、时间线（选材）。
// 智能剪辑不 import 这些域的 app / repo，类型在这里逐字段转换（08 §1）。

// SmartcutSpeechInput 是识别端口的入参。
type SmartcutSpeechInput struct {
	Catalog  *catalogapp.Service
	Gen      *genapp.Service
	Media    *mediaapp.Service
	Timeline *timelineapp.Service
	Events   smartcutapp.PlanEvents
}

// SmartcutSpeechPorts 装出识别的端口（两个进程共用：API 受理与续接，Worker 只用到回写那几个方法）。
func SmartcutSpeechPorts(in SmartcutSpeechInput) smartcutapp.TranscribePorts {
	p := smartcutapp.TranscribePorts{Media: smartcutSpeechMedia{in.Media}, Events: in.Events}
	if in.Catalog != nil {
		p.Catalog = smartcutSpeechCatalog{in.Catalog}
	}
	if in.Gen != nil {
		p.Gen = smartcutSpeechGen{in.Gen}
	}
	if in.Timeline != nil {
		p.Timelines = SmartcutPlanTimelines{Svc: in.Timeline}
	}
	return p
}

// ---------------------------------------------------------------- 目录

type smartcutSpeechCatalog struct{ svc *catalogapp.Service }

func (a smartcutSpeechCatalog) SpeechModels(ctx context.Context, memberLevelID int64) ([]smartcutapp.ModelChoice, error) {
	list, err := a.svc.SpeechModels(ctx, memberLevelID)
	if err != nil {
		return nil, err
	}
	out := make([]smartcutapp.ModelChoice, 0, len(list))
	for _, m := range list {
		out = append(out, smartcutapp.ModelChoice{Code: m.Code, Name: m.Name, Granularity: m.Granularity,
			UnitPrice: m.UnitPrice, Unit: m.Unit})
	}
	return out, nil
}

func (a smartcutSpeechCatalog) QuoteSpeech(ctx context.Context, modelCode string, memberLevelID int64, durationSec int) (
	smartcutapp.SpeechQuote, *smartcutapp.Unavailable, error) {
	q, unavailable, err := a.svc.QuoteSpeech(ctx, modelCode, memberLevelID, durationSec)
	if err != nil {
		return smartcutapp.SpeechQuote{}, nil, err
	}
	if unavailable != nil {
		return smartcutapp.SpeechQuote{}, &smartcutapp.Unavailable{Reason: unavailable.Reason, UserFixable: unavailable.UserFixable}, nil
	}
	return smartcutapp.SpeechQuote{Credits: q.Credits, UnitPrice: q.PriceCredits, Unit: q.PriceUnit}, nil, nil
}

// ---------------------------------------------------------------- 生成受理

type smartcutSpeechGen struct{ svc *genapp.Service }

// AcceptSpeech 走统一受理入口：Origin=smartcut、不挂画布节点；输入是识别用音轨（参考文件，与源素材同一个工作区）；
// 时长由智能剪辑按音轨的实测时长给（不收客户端报的数）。
func (a smartcutSpeechGen) AcceptSpeech(ctx context.Context, in smartcutapp.SpeechAccept) (smartcutapp.SpeechAccepted, error) {
	inputs, _ := json.Marshal([]map[string]any{{"kind": "reference", "order": 1,
		"referenceId": strconv.FormatInt(in.AudioMediaID, 10)}})
	params := map[string]any{protocol.ASRParamKeepFillers: in.KeepFillers}
	if in.Language != "" {
		params[protocol.ASRParamLanguage] = in.Language
	}
	rawParams, _ := json.Marshal(params)
	res, err := a.svc.Accept(ctx, genapp.AcceptInput{
		WorkspaceID: in.WorkspaceID, PayerAccountID: in.PayerAccountID, UserID: in.UserID, ProjectID: in.ProjectID,
		OperationCode: catalogapp.OpSpeech2Text, ModelCode: in.ModelCode, IdempotencyKey: in.IdempotencyKey,
		MemberLevelID: in.MemberLevelID, Count: 1, DurationSec: in.DurationSec, Params: rawParams,
		DeclaredInputs: inputs, Origin: gendomain.OriginSmartcut, SmartcutTarget: in.Target,
	})
	if err != nil {
		return smartcutapp.SpeechAccepted{}, err
	}
	g := res.Generation
	return smartcutapp.SpeechAccepted{GenerationID: g.ID, Status: g.Status, Credits: g.CreditsCost,
		CreatedAt: g.CreatedAt, Replayed: res.Replayed}, nil
}

// ---------------------------------------------------------------- 媒体

type smartcutSpeechMedia struct{ svc *mediaapp.Service }

func (a smartcutSpeechMedia) Facts(ctx context.Context, ws int64, ids []int64) (map[int64]smartcutapp.SpeechFact, error) {
	got, err := a.svc.InputFacts(ctx, ws, ids)
	if err != nil {
		return nil, err
	}
	out := make(map[int64]smartcutapp.SpeechFact, len(got))
	for id, f := range got {
		if f.Invalid != "" {
			continue
		}
		out[id] = smartcutapp.SpeechFact{Kind: f.Kind, DurationMs: f.DurationMs, HasAudio: f.HasAudio}
	}
	return out, nil
}

func (a smartcutSpeechMedia) SpeechAudio(ctx context.Context, ws int64, ids []int64) (map[int64]smartcutapp.SpeechAudio, error) {
	got, err := a.svc.SpeechAudio(ctx, ws, ids)
	if err != nil {
		return nil, err
	}
	out := make(map[int64]smartcutapp.SpeechAudio, len(got))
	for id, s := range got {
		out[id] = speechOf(s)
	}
	return out, nil
}

func speechOf(s mediarepo.SpeechAudioState) smartcutapp.SpeechAudio {
	return smartcutapp.SpeechAudio{Status: s.Status, DerivedID: s.DerivedID, DurationMs: s.DurationMs, NoAudio: s.NoAudio,
		Error: s.Error}
}

func (a smartcutSpeechMedia) EnqueueSpeechAudio(ctx context.Context, ws int64, ids []int64) error {
	_, err := a.svc.EnqueueAnalyses(ctx, ws, ids, []string{mediadomain.AnalysisSpeechAudio})
	return err
}

func asrRowOf(r mediarepo.Analysis) smartcutapp.ASRRow {
	return smartcutapp.ASRRow{ID: r.ID, MediaID: r.MediaObjectID, Engine: r.Engine, ParamsKey: r.ParamsKey,
		Status: r.Status, Result: r.Result, CreatedAt: r.CreatedAt}
}

func (a smartcutSpeechMedia) ASRRows(ctx context.Context, ws int64, ids []int64, engine, paramsKey string) (
	map[int64]smartcutapp.ASRRow, error) {
	got, err := a.svc.ASRRows(ctx, ws, ids, engine, paramsKey)
	if err != nil {
		return nil, err
	}
	out := make(map[int64]smartcutapp.ASRRow, len(got))
	for id, r := range got {
		out[id] = asrRowOf(r)
	}
	return out, nil
}

func (a smartcutSpeechMedia) InsertASR(ctx context.Context, ws, mediaID int64, engine, paramsKey string,
	pending json.RawMessage) (smartcutapp.ASRRow, bool, error) {
	r, inserted, err := a.svc.InsertASR(ctx, ws, mediaID, engine, paramsKey, pending)
	return asrRowOf(r), inserted, err
}

func (a smartcutSpeechMedia) AttachASR(ctx context.Context, id, generationID int64) (bool, error) {
	return a.svc.AttachASR(ctx, id, generationID)
}

func (a smartcutSpeechMedia) FailASR(ctx context.Context, id int64, msg string) (bool, error) {
	return a.svc.FailASR(ctx, id, msg)
}

func (a smartcutSpeechMedia) SucceedASR(ctx context.Context, id int64, result json.RawMessage) (bool, error) {
	return a.svc.SucceedASR(ctx, id, result)
}

func (a smartcutSpeechMedia) FailedASRCount(ctx context.Context, mediaID int64, engine, paramsKey string) (int, error) {
	return a.svc.FailedASRCount(ctx, mediaID, engine, paramsKey)
}

func (a smartcutSpeechMedia) ListPendingASR(ctx context.Context, limit int) ([]smartcutapp.PendingASR, error) {
	got, err := a.svc.ListPendingASR(ctx, limit)
	if err != nil {
		return nil, err
	}
	out := make([]smartcutapp.PendingASR, 0, len(got))
	for _, p := range got {
		out = append(out, smartcutapp.PendingASR{ASRRow: asrRowOf(p.Analysis), Speech: speechOf(p.Speech)})
	}
	return out, nil
}

func (a smartcutSpeechMedia) ClaimPendingASR(ctx context.Context, id int64, lease time.Duration) (bool, error) {
	return a.svc.ClaimPendingASR(ctx, id, lease)
}

func (a smartcutSpeechMedia) FinishASR(ctx context.Context, generationID, ws, mediaID int64, engine, paramsKey,
	status string, result json.RawMessage, msg string) (bool, error) {
	return a.svc.FinishASR(ctx, generationID, ws, mediaID, engine, paramsKey, status, result, msg)
}

// ---------------------------------------------------------------- 回写

// SmartcutBinder 把智能剪辑接成生成域的回写端口（genapp.SmartcutBinder）。两个 main 各装一次：
// Worker 的成功 / 失败路径，API 的取消与运维补偿路径（同 GroupItemBinder，漏一边就有一条终态路径不回写）。
type SmartcutBinder struct{ Svc *smartcutapp.Service }

var _ genapp.SmartcutBinder = SmartcutBinder{}

func (b SmartcutBinder) BindSmartcut(ctx context.Context, g genrepo.Generation, status string, outputs []genrepo.Output,
	errMsg string) error {
	in := smartcutapp.BindInput{GenerationID: g.ID, WorkspaceID: g.WorkspaceID, ProjectID: g.ProjectID,
		Target: g.SmartcutTarget, Status: status, ErrMsg: errMsg}
	textSeen := false
	for _, o := range outputs {
		switch {
		case o.OutputType == "text" && !textSeen:
			in.Text, in.Meta, textSeen = o.TextContent, o.Meta, true
		case o.MediaObjectID != 0:
			in.MediaIDs = append(in.MediaIDs, o.MediaObjectID)
		}
	}
	return b.Svc.BindSmartcut(ctx, in)
}
