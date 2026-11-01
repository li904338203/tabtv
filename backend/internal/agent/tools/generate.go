package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/li904338203/tabtv/backend/internal/agent/app"
	"github.com/li904338203/tabtv/backend/internal/agent/domain"
	"github.com/li904338203/tabtv/backend/internal/canvasgen"
	collab "github.com/li904338203/tabtv/backend/internal/collaboration/domain"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// generate（AGT-24，dev/01 §5.3）：建 / 改占位节点 → **节点落盘之后**报价 → PaidCall(受理)。
//
// ⚠️ 顺序不可换：受理侧的输入解析按 (project, node) 查画布 —— 节点还没落盘就受理的话，
// 生成任务挂在一个画布上不存在的节点上，前端永远对不上这条任务。
// ⚠️ 报价由服务端算（Quoter，与受理同一条 QuoteWithFacts），不信模型给的金额。
// ⚠️ agent 不另扣生成费：受理在自己的事务里扣，额度条目只是会话级的授权记账。
// ⚠️ 余额不足：条目已释放，回给模型「还差多少」，**不重试**（重试只会再失败一次）。
// ⚠️ 受理成功后发 link_generation 把任务挂到节点 result 上（缺陷修复 2026-10-16）：不挂的话画布
// 无从知道节点上有任务，节点一直显示成空的。这一步失败只告警 —— 任务已受理、钱已扣，
// 回给模型一个错误只会让它再生成一次（再扣一次钱）。
// ⚠️ 节点参数用画布的参数编码（canvasgen.NormalizeGenerateParams）：面板读不到的键等于没写。
//
// MCPT-08：「归一 → 建节点 → 报价 → 受理 → link_generation」已下沉到 internal/canvasgen（与 MCP 共用），
// 本文件只剩 Agent 独有的部分：参数解析、额度外壳（PaidCallTool）、给模型的文案、wait。

// PaidCaller 是付费调用外壳（app.Budget 实现）。
type PaidCaller interface {
	PaidCallTool(ctx context.Context, rc app.RunContext, tool string, quote int,
		run func(idemKey string) (app.PaidResult, error)) (app.PaidResult, *app.Suspension, error)
}

// Generate 是 generate 工具。
type Generate struct {
	Canvas    app.Canvas
	Quoter    app.Quoter
	Generator app.Generator
	Budget    PaidCaller
	// Params 画布节点的参数编码（目录）；为 nil 或取不到时退回 seed 的副本。
	Params app.NodeParams
	// Outputs 操作的产出节点类型（目录 operation.out_node_type）：不给 nodeId 也不给 nodeType 时据此推导
	// （canvasgen.ResolveNewNodeType，与 MCP 共用）。为 nil 时退回「nodeType 必须显式给」。
	Outputs canvasgen.OperationOutputs
	// Plans 营销会话的已批准计划（MKT-T-22）：营销会话里的 generate 必须对应已批准的计划项。
	// 为 nil 时只能走通用路径；会话是营销场景而它未接入时一律拒绝（不放过一条计划外的付费生成）。
	Plans app.PlanSource
	// Ops 模型的可用生成方式（目录）：输入与生成方式对不上时，把该模型能用的方式写进错误给模型看。
	Ops    app.OperationCatalog
	Logger *slog.Logger
}

func (Generate) Name() domain.ToolName { return domain.ToolGenerate }
func (Generate) Paid() bool            { return true }
func (Generate) Action() collab.Action { return collab.ActionGenerate }

func (Generate) Description() string {
	return "提交一次生成（图片 / 视频 / 音频 / 文本），会扣积分。不给 nodeId 时先在画布上新建一个节点再提交；" +
		"给 nodeId 时只能是你在本会话里建的节点。费用由服务端报价，超出用户批准的额度时会请用户追加。" +
		"wait=true（默认）提交后暂停等待任务完成；要并行提交多个时除最后一个外传 wait=false。" +
		"生成方式（operationCode）要与输入一致：带参考图用图生视频 / 参考生视频，纯文字才用文生视频；规划卡上写了哪种就用哪种。" +
		"营销会话里必须带 planItemKey（已批准计划里那一项的 key），模型、生成方式、时长、商品图都按那一项，不用再填（inputs 也不用填，服务端按那一项组装）。" +
		"成片渲染不走这个工具：先 timeline_assemble 把片段装进时间线，再用 render。"
}

func (Generate) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{` +
		`"nodeId":{"type":"string","description":"已有的、本会话建的节点；不给则新建"},` +
		`"nodeType":{"type":"string","enum":["image","video","audio","text"],"description":"新建节点的类型；不给时按操作的产出类型自动确定（给了必须与之一致）"},` +
		`"title":{"type":"string"},` +
		`"operationCode":{"type":"string","description":"操作码，如 text2img / img2img / text2video / img2video / tts"},` +
		`"modelCode":{"type":"string"},` +
		`"prompt":{"type":"string","description":"描述文字"},` +
		`"params":{"type":"object","description":"其余节点参数（画幅 aspect_ratio、清晰度 resolution、风格等）。新建节点没写画幅 / 清晰度时服务端补：营销视频先按所选创意的推荐画幅，其次按创作偏好，最后按节点默认；要别的规格就显式写"},` +
		`"inputs":{"type":"array","description":"输入（按 order）：{kind:text|generation|reference|material, order, sourceNodeId?, text?, generationId?, referenceId?, materialCode?}","items":{"type":"object"}},` +
		`"durationSec":{"type":"integer","minimum":0},` +
		`"count":{"type":"integer","minimum":0},` +
		`"position":{"type":"object","properties":{"x":{"type":"number"},"y":{"type":"number"}}},` +
		`"wait":{"type":"boolean","default":true},` +
		`"planItemKey":{"type":"string","description":"仅营销会话（必填）：已批准计划里这一项的 key，模型 / 生成方式 / 时长 / 商品图按该项填，可省略"}` +
		`},"required":["operationCode","modelCode"]}`)
}

// GenerateArgs 是 generate 的参数。
type GenerateArgs struct {
	NodeID        string          `json:"nodeId"`
	NodeType      string          `json:"nodeType"`
	Title         string          `json:"title"`
	OperationCode string          `json:"operationCode"`
	ModelCode     string          `json:"modelCode"`
	Prompt        string          `json:"prompt"`
	Params        json.RawMessage `json:"params"`
	Inputs        json.RawMessage `json:"inputs"`
	DurationSec   int             `json:"durationSec"`
	Count         int             `json:"count"`
	Position      *struct {
		X float64 `json:"x"`
		Y float64 `json:"y"`
	} `json:"position"`
	Wait *bool `json:"wait"`
}

// ParseGenerateArgs 校验参数（缺模型 / 操作码一律拒，不猜默认值）。不查目录：新建节点必须显式给合法的 nodeType。
func ParseGenerateArgs(raw json.RawMessage) (GenerateArgs, map[string]any, error) {
	return parseGenerateArgs(context.Background(), raw, nil)
}

// parseGenerateArgs 同 ParseGenerateArgs；outputs 非 nil 时，新建节点的类型按目录的产出类型推导 / 核对
// （canvasgen.ResolveNewNodeType，不按操作码字符串猜）。
func parseGenerateArgs(ctx context.Context, raw json.RawMessage, outputs canvasgen.OperationOutputs) (
	GenerateArgs, map[string]any, error) {
	var a GenerateArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return a, nil, fmt.Errorf("参数不合法：%v", err)
	}
	a.ModelCode = strings.TrimSpace(a.ModelCode)
	a.OperationCode = strings.TrimSpace(a.OperationCode)
	a.NodeID = strings.TrimSpace(a.NodeID)
	if a.ModelCode == "" {
		return a, nil, errors.New("缺少 modelCode：先用 quote 确认可用的模型")
	}
	if a.OperationCode == "" {
		return a, nil, errors.New("缺少 operationCode")
	}
	if a.Count < 0 || a.DurationSec < 0 {
		return a, nil, errors.New("count / durationSec 不能为负")
	}
	if a.NodeID == "" {
		t, err := canvasgen.ResolveNewNodeType(ctx, outputs, a.OperationCode, a.NodeType)
		if err != nil {
			return a, nil, err
		}
		a.NodeType = t
	}
	params := map[string]any{}
	if len(a.Params) > 0 && string(a.Params) != "null" {
		m, err := decodeObject(a.Params)
		if err != nil {
			return a, nil, errors.New("params 必须是对象")
		}
		params = m
	}
	if len(a.Inputs) > 0 && string(a.Inputs) != "null" {
		var arr []json.RawMessage
		if err := json.Unmarshal(a.Inputs, &arr); err != nil {
			return a, nil, errors.New("inputs 必须是数组")
		}
	} else {
		a.Inputs = nil
	}
	// 节点参数：与前端节点同形（model / prompt；视频的操作由 mode 决定 —— 同「直接生成」）。
	params = canvasgen.BuildNodeParams(a.NodeType, a.ModelCode, a.OperationCode, a.Prompt, params)
	return a, params, nil
}

func (t Generate) Run(ctx context.Context, rc app.RunContext, raw json.RawMessage) (app.ToolResult, error) {
	if rc.State != domain.StateExecuting {
		return app.ToolResult{}, fmt.Errorf("generate 只能在执行阶段使用（当前 %s）", rc.State)
	}
	if t.Canvas == nil || t.Quoter == nil || t.Generator == nil || t.Budget == nil {
		return app.ToolResult{}, errors.New("生成服务未接入")
	}
	// 成片渲染不归 generate（04 文档）：在建任何节点之前拦下并指路，原先要先建一个空节点、再报价失败。
	var peek struct {
		OperationCode string `json:"operationCode"`
	}
	_ = json.Unmarshal(raw, &peek)
	if fix := domain.WrongToolFor(peek.OperationCode, ""); fix != "" {
		return app.ToolResult{}, &domain.ToolRejection{Code: domain.ToolErrWrongTool,
			Message: "未提交（没有建节点、没有扣费）：成片渲染不用 generate，" + fix + "。"}
	}
	marketing, err := t.isMarketing(ctx, rc)
	if err != nil {
		return app.ToolResult{}, err
	}
	if marketing {
		return t.runMarketing(ctx, rc, raw)
	}
	a, params, err := parseGenerateArgs(ctx, raw, t.Outputs)
	if err != nil {
		return app.ToolResult{}, err
	}
	if a.NodeID == "" {
		params = fillGenerateDefaults(params, a.NodeType, "", rc.Spec)
	}
	return t.submit(ctx, rc, a, params, "")
}

// submit 是通用与营销共用的「建节点 → 报价 → 额度内受理 → 挂节点」。
//
// planItemKey 为空 = 通用会话：错误原样返回（逐字不变）。非空 = 营销会话：错误包成 *app.StepError，
// 带上计划项键与错误码（受理错误码名，如 INSUFFICIENT_CREDITS），成功结果也带键。
// capabilityHint 生成方式与输入对不上（或该模型没有这个生成方式）时，附上该模型能用的生成方式，
// 让模型照着换，而不是把参考图去掉硬走文生视频（04 文档「技能和模型参数」）。
func (t Generate) capabilityHint(ctx context.Context, err error, a GenerateArgs) string {
	var be *httpx.BizError
	if !errors.As(err, &be) || be.Code != errcode.CapabilityUnsupported {
		return ""
	}
	kind := a.NodeType
	if kind == "" {
		kind = "video"
	}
	return newOpMatcher(t.Ops).hint(ctx, domain.PlanItem{Kind: kind, ModelCode: a.ModelCode})
}

func (t Generate) submit(ctx context.Context, rc app.RunContext, a GenerateArgs, params map[string]any,
	planItemKey string) (app.ToolResult, error) {
	// tag：cause 定错误码（判定依据），shown 是给模型看的话。
	tag := func(cause, shown error) error {
		if planItemKey == "" {
			return shown
		}
		return &app.StepError{Err: shown, PlanItemKey: planItemKey, ErrorCode: stepErrorCode(cause)}
	}
	orch, c := t.orchestrator(), t.caller(rc)
	prep := orch.Prepare(ctx, c, a.request(params))

	// ① 节点先落盘（新建，或在自己的节点上更新参数 —— 后者顺带过一次所有权校验）。
	nodeID, err := orch.EnsureNode(ctx, c, prep)
	if err != nil {
		return app.ToolResult{}, tag(err, ensureNodeError(err))
	}

	// ② 报价（节点已落盘，输入解析能按节点找到它）。
	q, err := orch.Quote(ctx, c, prep, nodeID)
	if err != nil {
		return app.ToolResult{}, tag(err, fmt.Errorf("节点 %s 已建好，但报价失败：%v%s", nodeID, err,
			t.capabilityHint(ctx, err, a)))
	}

	// ③ 额度内受理。
	var accepted canvasgen.AcceptResult
	res, susp, err := t.Budget.PaidCallTool(ctx, rc, string(domain.ToolGenerate), q.Credits,
		func(key string) (app.PaidResult, error) {
			r, err := orch.Accept(ctx, c, prep, nodeID, key)
			if err != nil {
				return app.PaidResult{}, err
			}
			accepted = r
			return app.PaidResult{GenerationID: r.GenerationID, Credits: r.Credits}, nil
		})
	if susp != nil {
		content, _ := json.Marshal(map[string]any{
			"status": "over_budget", "nodeId": nodeID, "needed": q.Credits,
			"message": "超出用户批准的额度，已请用户追加额度。批准后用同一个 nodeId 再调用 generate。",
		})
		display, _ := json.Marshal(map[string]any{"nodeId": nodeID, "needed": q.Credits, "overBudget": true})
		// ⚠️ 不带计划项键（04 文档 P-04）：这一步什么都没提交，它会被记成一个成功的步骤；
		// 带上键的话计划进度会把这一项判成「已成功」，用户拒绝追加额度后仍一直显示成功，
		// 之后重试失败也被它盖住。批准额度后模型带同一个键再调一次，才是这一项真正的步骤。
		return app.ToolResult{Content: content, Display: display, Suspend: susp, Outputs: 1}, nil
	}
	if err != nil {
		var ins *app.InsufficientCreditsError
		if errors.As(err, &ins) {
			return app.ToolResult{}, tag(err, errors.New(insufficientMessage(nodeID, q.Credits, ins.Balance)))
		}
		return app.ToolResult{}, tag(err, fmt.Errorf("节点 %s 已建好，但提交生成失败：%v", nodeID, err))
	}

	wait := a.Wait == nil || *a.Wait
	genID := strconv.FormatInt(res.GenerationID, 10)
	orch.Link(ctx, c, nodeID, res.GenerationID)
	content, _ := json.Marshal(map[string]any{
		"generationId": genID, "nodeId": nodeID, "credits": accepted.Credits,
		"status": accepted.Status, "waiting": wait,
	})
	display, _ := json.Marshal(map[string]any{
		"generationId": genID, "nodeId": nodeID, "credits": accepted.Credits,
		"modelCode": a.ModelCode, "operationCode": a.OperationCode,
	})
	out := app.ToolResult{Content: content, Display: display, Outputs: 1, GenerationID: res.GenerationID,
		PlanItemKey: planItemKey}
	if wait {
		out.Suspend = &app.Suspension{Event: domain.EvWaitTask, Reason: "等待生成任务完成"}
	}
	return out, nil
}

// request 把解析好的参数交给编排（Params 已经过 BuildNodeParams）。
func (a GenerateArgs) request(params map[string]any) canvasgen.Request {
	r := canvasgen.Request{
		NodeID: a.NodeID, NodeType: a.NodeType, Title: a.Title,
		OperationCode: a.OperationCode, ModelCode: a.ModelCode, Prompt: a.Prompt,
		Params: params, Inputs: a.Inputs, DurationSec: a.DurationSec, Count: a.Count,
	}
	if a.Position != nil {
		r.Position = &canvasgen.Position{X: a.Position.X, Y: a.Position.Y}
	}
	return r
}

// orchestrator 把 Agent 的端口包成 canvasgen 的端口。
func (t Generate) orchestrator() canvasgen.Orchestrator {
	o := canvasgen.Orchestrator{
		Canvas:   genCanvasPort{t.Canvas},
		Quoter:   genQuoterPort{t.Quoter},
		Accepter: genAccepterPort{t.Generator},
		Outputs:  t.Outputs,
		Logger:   t.logger(),
	}
	if t.Params != nil {
		o.Params = t.Params
	}
	return o
}

// caller 用 RunContext 组调用方：批次 = BatchID(rc)（link 那一批由 canvasgen 派生为 LinkBatchID(rc)），
// 新节点上写 agent.stepId，告警日志带 session / step。
func (t Generate) caller(rc app.RunContext) canvasgen.Caller {
	return canvasgen.Caller{
		WorkspaceID: rc.WorkspaceID, ProjectID: rc.ProjectID, UserID: rc.UserID, Role: rc.Role,
		PayerAccountID: rc.PayerAccountID, MemberLevelID: rc.MemberLevelID,
		Origin:    canvasgen.Origin{Kind: canvasgen.OriginAgent, SessionID: rc.SessionID},
		BatchID:   BatchID(rc),
		StampNode: func(op map[string]any) { stampAgentStep(op, "node", rc.StepID) },
		LogAttrs:  []any{"session", rc.SessionID, "step", rc.StepID},
	}
}

// ensureNodeError 把建节点的错误翻成给模型的话（同 applyCanvas：所有权冲突 → NotOwnedHint，其余加「写画布失败」）。
func ensureNodeError(err error) error {
	if errors.Is(err, canvasgen.ErrNoNodeID) {
		return err
	}
	if errors.Is(err, app.ErrNodeNotOwned) {
		return errors.New(NotOwnedHint)
	}
	return fmt.Errorf("写画布失败：%w", err)
}

// LinkBatchID 是 link_generation 那一批的幂等键：`agent:{session}:{step}:{attempt}:link`。
func LinkBatchID(rc app.RunContext) string { return canvasgen.LinkBatchID(BatchID(rc)) }

func (t Generate) logger() *slog.Logger {
	if t.Logger != nil {
		return t.Logger
	}
	return slog.Default()
}

// ---------------------------------------------------------------- Agent 端口 → canvasgen 端口（私有适配器）

type genCanvasPort struct{ c app.Canvas }

func (p genCanvasPort) Read(ctx context.Context, ws, projectID int64) (canvasgen.CanvasView, error) {
	v, err := p.c.Read(ctx, ws, projectID)
	if err != nil {
		return canvasgen.CanvasView{}, err
	}
	params := canvasgen.ParamsByNode(v.CanvasData)
	nodes := make([]canvasgen.CanvasNode, 0, len(v.Nodes))
	for _, n := range v.Nodes {
		nodes = append(nodes, canvasgen.CanvasNode{ID: n.ID, Type: n.Type, Params: params[n.ID]})
	}
	return canvasgen.CanvasView{Nodes: nodes, Edges: canvasgen.EdgesOf(v.CanvasData)}, nil
}

func (p genCanvasPort) Apply(ctx context.Context, in canvasgen.ApplyInput) (canvasgen.ApplyResult, error) {
	r, err := p.c.Apply(ctx, app.ApplyOpsInput{
		WorkspaceID: in.WorkspaceID, ProjectID: in.ProjectID, UserID: in.UserID, Role: in.Role,
		AgentSessionID: in.Origin.SessionID, BatchID: in.BatchID, Ops: in.Ops,
	})
	if err != nil {
		return canvasgen.ApplyResult{}, err
	}
	return canvasgen.ApplyResult{FromVersion: r.FromVersion, ToVersion: r.ToVersion, NodeIDs: r.NodeIDs,
		Replayed: r.Replayed}, nil
}

type genQuoterPort struct{ q app.Quoter }

func (p genQuoterPort) QuoteGeneration(ctx context.Context, in canvasgen.QuoteInput) (canvasgen.Quote, error) {
	q, err := p.q.QuoteGeneration(ctx, app.QuoteInput{
		ModelCode: in.ModelCode, OperationCode: in.OperationCode, Params: in.Params,
		Text: in.Text, MemberLevelID: in.MemberLevelID, Count: in.Count, DurationSec: in.DurationSec,
		WorkspaceID: in.WorkspaceID, ProjectID: in.ProjectID, NodeID: in.NodeID, DeclaredInputs: in.DeclaredInputs,
	})
	if err != nil {
		return canvasgen.Quote{}, err
	}
	return canvasgen.Quote{Credits: q.Credits, ModelID: q.ModelID}, nil
}

// genAccepterPort 的错误原样透传：余额不足的 *app.InsufficientCreditsError 要让 Run 用 errors.As 认出来。
type genAccepterPort struct{ g app.Generator }

func (p genAccepterPort) Accept(ctx context.Context, in canvasgen.AcceptInput) (canvasgen.AcceptResult, error) {
	r, err := p.g.Accept(ctx, app.GenerateInput{
		WorkspaceID: in.WorkspaceID, ProjectID: in.ProjectID, NodeID: in.NodeID, UserID: in.UserID,
		PayerAccountID: in.PayerAccountID, MemberLevelID: in.MemberLevelID,
		ModelCode: in.ModelCode, OperationCode: in.OperationCode, Params: in.Params,
		Inputs: in.Inputs, IdempotencyKey: in.IdempotencyKey, Count: in.Count, DurationSec: in.DurationSec,
		Text: in.Text,
	})
	if err != nil {
		return canvasgen.AcceptResult{}, err
	}
	return canvasgen.AcceptResult{GenerationID: r.GenerationID, Credits: r.Credits, Status: r.Status,
		Replayed: r.Replayed}, nil
}

func insufficientMessage(nodeID string, need, balance int) string {
	if balance >= 0 && need > balance {
		return fmt.Sprintf("余额不足，还差 %d 积分（本次需要 %d，当前余额 %d）。节点 %s 已保留；"+
			"请告诉用户充值后再继续，不要重试。", need-balance, need, balance, nodeID)
	}
	return fmt.Sprintf("余额不足（本次需要 %d 积分）。节点 %s 已保留；请告诉用户充值后再继续，不要重试。", need, nodeID)
}

var _ app.Tool = Generate{}
