package wiring

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	catalogapp "github.com/li904338203/tabtv/backend/internal/catalog/app"
	catalogdomain "github.com/li904338203/tabtv/backend/internal/catalog/domain"
	genapp "github.com/li904338203/tabtv/backend/internal/generation/app"
	gendomain "github.com/li904338203/tabtv/backend/internal/generation/domain"
	genrepo "github.com/li904338203/tabtv/backend/internal/generation/repo"
	mediaapp "github.com/li904338203/tabtv/backend/internal/media/app"
	smartcutapp "github.com/li904338203/tabtv/backend/internal/smartcut/app"
	timelineapp "github.com/li904338203/tabtv/backend/internal/timeline/app"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// 智能剪辑的片段级生成（docs/agent/smartcut SCT-37~39）：生成域、时间线域、媒体域的适配。

// SmartcutClipInput 片段级生成的装配。
type SmartcutClipInput struct {
	Gen      *genapp.Service
	GenRepo  *genrepo.Repo
	Media    *mediaapp.Service
	Timeline *timelineapp.Service
	// Render 回填写入之后把时间线渲染成契约形状（API 侧给；Worker 侧不需要）。
	Render func(timelineapp.View) ([]byte, error)
	// Catalog 列片段级工具可选的模型（`/smartcut/config`，API 侧给；Worker 侧不需要）。
	Catalog *catalogapp.Service
}

// SmartcutClipPorts 装出片段级生成的端口（两个进程共用：API 受理与取消路径的回写，Worker 成功 / 失败路径的回写）。
func SmartcutClipPorts(in SmartcutClipInput) smartcutapp.ClipToolPorts {
	p := smartcutapp.ClipToolPorts{
		Gen:       smartcutClipGen{svc: in.Gen, repo: in.GenRepo},
		Timelines: SmartcutPlanTimelines{Svc: in.Timeline, Render: in.Render},
		Pending:   in.Timeline,
		Media:     smartcutClipMedia{in.Media},
	}
	if in.Catalog != nil {
		p.Models = smartcutClipCatalog{in.Catalog}
	}
	return p
}

// ---------------------------------------------------------------- 目录

type smartcutClipCatalog struct{ svc *catalogapp.Service }

func (a smartcutClipCatalog) ClipModels(ctx context.Context, op string, memberLevelID int64, roles map[string]int) (
	[]smartcutapp.ModelChoice, error) {
	rc := catalogdomain.RoleCount{}
	for k, v := range roles {
		rc[catalogdomain.InputRole(k)] = v
	}
	list, err := a.svc.ClipModels(ctx, op, memberLevelID, rc)
	if err != nil {
		return nil, err
	}
	out := make([]smartcutapp.ModelChoice, 0, len(list))
	for _, m := range list {
		out = append(out, smartcutapp.ModelChoice{Code: m.Code, Name: m.Name, MinSec: m.MinSec, MaxSec: m.MaxSec,
			UnitPrice: m.UnitPrice, Unit: m.Unit})
	}
	return out, nil
}

// ---------------------------------------------------------------- 生成域

type smartcutClipGen struct {
	svc  *genapp.Service
	repo *genrepo.Repo
}

func (a smartcutClipGen) Sources(ctx context.Context, ws int64, ids []int64) (map[int64]smartcutapp.ClipSource, error) {
	got, err := a.repo.SourcesByOutputMedia(ctx, ws, ids)
	if err != nil {
		return nil, err
	}
	out := make(map[int64]smartcutapp.ClipSource, len(got))
	for id, s := range got {
		out[id] = smartcutapp.ClipSource{GenerationID: s.GenerationID, ProjectID: s.ProjectID, OperationCode: s.OperationCode,
			ModelCode: s.ModelCode, ModelName: s.ModelName, Prompt: s.Prompt, Params: genapp.UserParams(s.Params),
			Inputs: s.Inputs, Origin: s.Origin, SmartcutKind: s.SmartcutKind, SmartcutTool: s.SmartcutTool}
	}
	return out, nil
}

// acceptInput 片段级生成 → 生成域的受理请求。以原任务为模板时照 Retry 的做法：参数取用户参数部分、
// 输入还原成声明形态（受理时重新解析：归属可能变了、产物可能被删了），不挂画布节点。
func (a smartcutClipGen) acceptInput(ctx context.Context, in smartcutapp.ClipAccept) (genapp.AcceptInput, error) {
	params := map[string]any{}
	inputs := in.Inputs
	count := 1
	if in.FromGenerationID != 0 {
		orig, err := a.repo.Get(ctx, in.WorkspaceID, in.FromGenerationID)
		if err != nil {
			return genapp.AcceptInput{}, err
		}
		_ = json.Unmarshal(genapp.UserParams(orig.ParamsSnapshot), &params)
		inputs = gendomain.RedeclareInputs(orig.InputsSnapshot)
		if orig.RequestCount > 0 {
			count = orig.RequestCount
		}
	}
	if params == nil {
		params = map[string]any{}
	}
	for k, v := range in.Params {
		params[k] = v
	}
	// 提示词：重拍时换掉模板里的那一段；去字幕、配音效这类没有提示词的工具不写这个键。
	if in.Prompt != "" || in.FromGenerationID != 0 {
		params["prompt"] = in.Prompt
	}
	raw, _ := json.Marshal(params)
	duration := in.DurationSec
	if duration == 0 {
		duration = paramSeconds(params["duration"])
	}
	return genapp.AcceptInput{WorkspaceID: in.WorkspaceID, PayerAccountID: in.PayerAccountID, UserID: in.UserID,
		ProjectID: in.ProjectID, OperationCode: in.OperationCode, ModelCode: in.ModelCode, IdempotencyKey: in.IdempotencyKey,
		MemberLevelID: in.MemberLevelID, Count: count, DurationSec: duration, Text: in.Prompt, Params: raw,
		DeclaredInputs: inputs, Origin: gendomain.OriginSmartcut, SmartcutTarget: in.Target}, nil
}

// paramSeconds 参数里的时长（节点参数可能存成字符串 "5"）。
func paramSeconds(v any) int {
	switch x := v.(type) {
	case float64:
		return int(x)
	case string:
		n, _ := strconv.Atoi(strings.TrimSpace(x))
		return n
	}
	return 0
}

func (a smartcutClipGen) Preview(ctx context.Context, in smartcutapp.ClipAccept) (smartcutapp.ClipQuote, error) {
	acc, err := a.acceptInput(ctx, in)
	if err != nil {
		return smartcutapp.ClipQuote{}, err
	}
	res, err := a.svc.Preview(ctx, acc)
	if err != nil {
		return smartcutapp.ClipQuote{}, err
	}
	q := smartcutapp.ClipQuote{Credits: res.Quote.Credits, UnitPrice: res.Quote.PriceCredits, Unit: res.Quote.PriceUnit,
		ModelCode: in.ModelCode}
	if res.QuoteErr != nil {
		b := &smartcutapp.QuoteBlocker{Code: "unavailable", Message: res.QuoteErr.Error()}
		var be *httpx.BizError
		if errors.As(res.QuoteErr, &be) {
			b.Code, b.Message = strconv.Itoa(int(be.Code)), be.Error()
		}
		q.Credits, q.Blocker = 0, b
	}
	return q, nil
}

func (a smartcutClipGen) Accept(ctx context.Context, in smartcutapp.ClipAccept) (smartcutapp.ClipAccepted, error) {
	acc, err := a.acceptInput(ctx, in)
	if err != nil {
		return smartcutapp.ClipAccepted{}, err
	}
	res, err := a.svc.Accept(ctx, acc)
	if err != nil {
		return smartcutapp.ClipAccepted{}, err
	}
	g := res.Generation
	return smartcutapp.ClipAccepted{GenerationID: g.ID, Status: g.Status, Credits: g.CreditsCost, CreatedAt: g.CreatedAt,
		Replayed: res.Replayed}, nil
}

func (a smartcutClipGen) Status(ctx context.Context, ws, id int64) (string, error) {
	g, err := a.repo.Get(ctx, ws, id)
	if errors.Is(err, genrepo.ErrNotFound) {
		return "", nil
	}
	return g.Status, err
}

// List 生成记录。游标是上一页最后一条的「受理时间（Unix 纳秒）:ID」。
func (a smartcutClipGen) List(ctx context.Context, ws, tid int64, cursor string, limit int) ([]smartcutapp.ClipGenerationRow,
	string, error) {
	var at time.Time
	var before int64
	if ts, id, ok := strings.Cut(cursor, ":"); ok {
		n, err1 := strconv.ParseInt(ts, 10, 64)
		m, err2 := strconv.ParseInt(id, 10, 64)
		if err1 == nil && err2 == nil {
			at, before = time.Unix(0, n), m
		}
	}
	if limit <= 0 || limit > 100 {
		limit = 30
	}
	rows, err := a.repo.ListBySmartcutTimeline(ctx, ws, tid, at, before, limit)
	if err != nil {
		return nil, "", err
	}
	out := make([]smartcutapp.ClipGenerationRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, smartcutapp.ClipGenerationRow{GenerationID: r.ID, Target: r.SmartcutTarget, Status: r.Status,
			Progress: r.Progress, Credits: r.CreditsCost, OutputMediaID: r.OutputMediaID, Error: r.ErrorMessage,
			CreatedAt: r.CreatedAt})
	}
	next := ""
	if len(rows) == limit {
		last := rows[len(rows)-1]
		next = strconv.FormatInt(last.CreatedAt.UnixNano(), 10) + ":" + strconv.FormatInt(last.ID, 10)
	}
	return out, next, nil
}

func (a smartcutClipGen) MarkLanded(ctx context.Context, id int64) error {
	return a.repo.MarkSmartcutLanded(ctx, id)
}

// ---------------------------------------------------------------- 媒体

type smartcutClipMedia struct{ svc *mediaapp.Service }

func (a smartcutClipMedia) Info(ctx context.Context, ws int64, ids []int64) (map[int64]smartcutapp.ClipMediaInfo, error) {
	facts, err := a.svc.InputFacts(ctx, ws, ids)
	if err != nil {
		return nil, err
	}
	out := make(map[int64]smartcutapp.ClipMediaInfo, len(facts))
	for id, f := range facts {
		if f.Invalid != "" {
			continue
		}
		info := smartcutapp.ClipMediaInfo{Kind: f.Kind, DurationMs: f.DurationMs, LastFrameID: f.LastFrameID}
		if m, err := a.svc.Info(ctx, ws, id); err == nil {
			info.Purpose = m.Purpose
		}
		out[id] = info
	}
	return out, nil
}
