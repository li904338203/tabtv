package wiring

// 对外 MCP 多段工具集（MCPT-16，MCP-F-33~37）的端口适配器：脚本线 / 整图 / 时间线 / 技能提示词。
//
// ⚠️ **委托** AGT-50 的 Agent 适配器（AgentScripts / AgentWorkflows / AgentTimelines）：报价口径
// （「真正提交时会发出去的那组参数」逐项问目录）与参数组合逻辑只有一份（dev/01 §12 R2）。这里只做
// 类型转换（mcpapp ↔ agentapp）与 MCP 的画布来源（建节点 → 建对象 → 写回走 internal/canvasgen，与 Agent 共用）。
// ⚠️ 发现要写业务判断时，那条规则应该放进对应的域，不在这里补。
// ⚠️ 租户一律 Principal.WorkspaceID、操作者 Principal.UserID、付费主体 Principal.PayerAccountID、
// 会员等级 Principal.MemberLevelID（与真实受理同一个解析器的结果，Resolver 已注入）。

import (
	"context"
	"errors"
	"log/slog"
	"strconv"

	agentapp "github.com/li904338203/tabtv/backend/internal/agent/app"
	agentdomain "github.com/li904338203/tabtv/backend/internal/agent/domain"
	agentrepo "github.com/li904338203/tabtv/backend/internal/agent/repo"
	"github.com/li904338203/tabtv/backend/internal/canvasgen"
	mcpapp "github.com/li904338203/tabtv/backend/internal/mcp/app"
	scriptapp "github.com/li904338203/tabtv/backend/internal/script/app"
	timelinerepo "github.com/li904338203/tabtv/backend/internal/timeline/repo"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// mcpCaller 是 MCP 写画布的调用方（来源 = 本授权，所有权按 GrantID 判）。
func mcpCaller(p mcpapp.Principal, projectID int64, batchID string) canvasgen.Caller {
	return canvasgen.Caller{
		WorkspaceID: p.WorkspaceID, ProjectID: projectID, UserID: p.UserID, Role: p.Role,
		PayerAccountID: p.PayerAccountID, MemberLevelID: p.MemberLevelID,
		Origin:   canvasgen.Origin{Kind: canvasgen.OriginMCP, GrantID: p.GrantID, ClientName: p.ClientName},
		BatchID:  batchID,
		LogAttrs: []any{"mcpGrant", p.GrantID},
	}
}

// unAgentErr 把 Agent 适配器翻出来的余额不足（agentapp.InsufficientCreditsError）还原成业务错误：
// MCP 要的是原样的 42201 + 差额说明，不是给站内模型的那句话。
func unAgentErr(err error) error {
	var ins *agentapp.InsufficientCreditsError
	if errors.As(err, &ins) && ins.Cause != nil {
		return ins.Cause
	}
	return err
}

// ---------------------------------------------------------------- 脚本

// MCPScripts 实现 mcpapp.Scripts。
type MCPScripts struct {
	Agent AgentScripts
	// Orch 是共享画布编排（建节点 / 写回）；与 generate 用的是同一个实例。
	Orch *canvasgen.Orchestrator
}

func (a MCPScripts) Create(ctx context.Context, p mcpapp.Principal, in mcpapp.ScriptCreateInput) (
	mcpapp.ScriptView, string, error) {
	var sv agentapp.ScriptView
	nodeID, err := a.Orch.CreateObjectNode(ctx, mcpCaller(p, in.ProjectID, in.BatchID), canvasgen.ObjectNode{
		NodeType: "script", Title: in.Title,
		Params: map[string]any{"sourceType": in.SourceType, "sourceText": in.RawInput},
	}, "script", func(ctx context.Context, nodeID string) (map[string]any, error) {
		v, err := a.Agent.CreateScript(ctx, agentapp.ScriptCreateInput{WorkspaceID: p.WorkspaceID,
			ProjectID: in.ProjectID, CanvasNodeID: nodeID, SourceType: in.SourceType, RawInput: in.RawInput,
			GlobalStyle: in.GlobalStyle})
		if err != nil {
			return nil, err
		}
		sv = v
		return canvasgen.ScriptNodeParams(scriptFacts(v)), nil
	})
	if err != nil {
		return mcpapp.ScriptView{}, nodeID, err
	}
	return mcpScriptView(sv, nil), nodeID, nil
}

func (a MCPScripts) Get(ctx context.Context, p mcpapp.Principal, scriptID int64) (mcpapp.ScriptDetail, error) {
	svc := a.Agent.Svc
	d, err := svc.Get(ctx, p.WorkspaceID, scriptID)
	if err != nil {
		return mcpapp.ScriptDetail{}, err
	}
	out := mcpapp.ScriptDetail{Script: detailView(d)}
	assets, _, err := svc.Assets(ctx, p.WorkspaceID, scriptID)
	if err != nil {
		return mcpapp.ScriptDetail{}, err
	}
	for _, x := range assets {
		out.Assets = append(out.Assets, mcpapp.ScriptAssetView{ID: x.ID, Name: x.Name, Type: x.Type, Status: x.Status,
			MediaID: x.MediaObjectID})
	}
	groups, err := svc.Groups(ctx, p.WorkspaceID, scriptID)
	if err != nil {
		return mcpapp.ScriptDetail{}, err
	}
	for _, g := range groups {
		out.Groups = append(out.Groups, mcpapp.ScriptGroupBrief{ID: g.ID, GroupType: g.GroupType, Name: g.Name,
			Status: g.Status, CanvasNodeID: g.CanvasNodeID})
	}
	calls, err := svc.Calls(ctx, p.WorkspaceID, scriptID)
	if err != nil {
		return mcpapp.ScriptDetail{}, err
	}
	for i, c := range calls {
		if i >= 10 { // 最近 10 次：够看出「分镜还在跑 / 刚失败了」，不把整段历史塞进结果
			break
		}
		out.Calls = append(out.Calls, mcpapp.ScriptCallView{ID: c.ID, Purpose: c.Purpose, Status: c.Status,
			Credits: c.CreditsCost, Error: c.ErrorMessage})
	}
	return out, nil
}

func (a MCPScripts) storyboardReq(p mcpapp.Principal, in mcpapp.StoryboardInput) agentapp.StoryboardRequest {
	return agentapp.StoryboardRequest{WorkspaceID: p.WorkspaceID, PayerAccountID: p.PayerAccountID, UserID: p.UserID,
		ScriptID: in.ScriptID, MemberLevelID: p.MemberLevelID, ModelCode: in.ModelCode, ShotCount: in.ShotCount,
		ExtraContext: in.ExtraContext}
}

func (a MCPScripts) QuoteStoryboard(ctx context.Context, p mcpapp.Principal, in mcpapp.StoryboardInput) (int, error) {
	return a.Agent.QuoteStoryboard(ctx, a.storyboardReq(p, in))
}

func (a MCPScripts) Storyboard(ctx context.Context, p mcpapp.Principal, in mcpapp.StoryboardInput) (
	mcpapp.StoryboardResult, error) {
	r, err := a.Agent.Storyboard(ctx, a.storyboardReq(p, in))
	if err != nil {
		return mcpapp.StoryboardResult{}, unAgentErr(err)
	}
	a.writeBackScript(ctx, p, r.Script, in.BatchID)
	return mcpapp.StoryboardResult{Script: mcpScriptView(r.Script, nil), CallID: r.CallID, Credits: r.Credits}, nil
}

// writeBackScript 把脚本状态写回脚本节点（本授权建的才写得进去；用户的节点由画布域拒绝、只告警）。
func (a MCPScripts) writeBackScript(ctx context.Context, p mcpapp.Principal, v agentapp.ScriptView, batch string) {
	if batch == "" || a.Orch == nil || v.CanvasNodeID == "" {
		return
	}
	a.Orch.WriteBack(ctx, mcpCaller(p, v.ProjectID, batch), v.CanvasNodeID, "script",
		canvasgen.ScriptNodeParams(scriptFacts(v)))
}

func (a MCPScripts) assetsReq(p mcpapp.Principal, in mcpapp.AssetsInput) agentapp.AssetsRequest {
	return agentapp.AssetsRequest{WorkspaceID: p.WorkspaceID, PayerAccountID: p.PayerAccountID, UserID: p.UserID,
		ScriptID: in.ScriptID, MemberLevelID: p.MemberLevelID, ModelCode: in.ModelCode, AssetIDs: in.AssetIDs,
		Params: in.Params}
}

func (a MCPScripts) QuoteAssets(ctx context.Context, p mcpapp.Principal, in mcpapp.AssetsInput) (mcpapp.AssetsQuote, error) {
	q, err := a.Agent.QuoteAssets(ctx, a.assetsReq(p, in))
	if err != nil {
		return mcpapp.AssetsQuote{}, err
	}
	out := mcpapp.AssetsQuote{Total: q.Total, Unmatched: q.Unmatched}
	for _, b := range q.Pending {
		out.Pending = append(out.Pending, mcpapp.ScriptAssetView{ID: b.ID, Name: b.Name, Type: b.Type, Status: b.Status})
	}
	return out, nil
}

func (a MCPScripts) GenerateAssets(ctx context.Context, p mcpapp.Principal, in mcpapp.AssetsInput) (
	[]mcpapp.AssetItemResult, error) {
	r, err := a.Agent.GenerateAssets(ctx, a.assetsReq(p, in))
	if err != nil {
		return nil, unAgentErr(err)
	}
	out := make([]mcpapp.AssetItemResult, 0, len(r.Items))
	for _, it := range r.Items {
		out = append(out, mcpapp.AssetItemResult{AssetID: it.AssetID, Name: it.Name, GenerationID: it.GenerationID,
			Skipped: it.Skipped, Reason: it.Reason})
	}
	return out, nil
}

func (a MCPScripts) composeReq(p mcpapp.Principal, in mcpapp.ComposeInput) agentapp.ComposeRequest {
	return agentapp.ComposeRequest{WorkspaceID: p.WorkspaceID, PayerAccountID: p.PayerAccountID, UserID: p.UserID,
		ScriptID: in.ScriptID, ShotIDs: in.ShotIDs, Kinds: in.Kinds, Source: in.Source, ModelCode: in.ModelCode,
		Force: in.Force}
}

func (a MCPScripts) QuoteCompose(ctx context.Context, p mcpapp.Principal, in mcpapp.ComposeInput) (mcpapp.ComposeQuote, error) {
	q, err := a.Agent.QuoteCompose(ctx, a.composeReq(p, in))
	if err != nil {
		return mcpapp.ComposeQuote{}, err
	}
	return mcpapp.ComposeQuote{AICount: q.AICount, TemplateCount: q.TemplateCount, SkipCount: q.SkipCount,
		Total: q.Total}, nil
}

func (a MCPScripts) Compose(ctx context.Context, p mcpapp.Principal, in mcpapp.ComposeInput) (mcpapp.ComposeResult, error) {
	r, err := a.Agent.Compose(ctx, a.composeReq(p, in))
	if err != nil {
		return mcpapp.ComposeResult{}, unAgentErr(err)
	}
	out := mcpapp.ComposeResult{CallIDs: r.CallIDs}
	for _, it := range r.Items {
		out.Items = append(out.Items, mcpapp.ComposeItemResult{ShotID: it.ShotID, ShotNo: it.ShotNo, Kind: it.Kind,
			Status: it.Status, Skipped: it.Skipped, Reason: it.Reason})
	}
	if sc, err := a.Agent.Script(ctx, p.WorkspaceID, in.ScriptID); err == nil {
		a.writeBackScript(ctx, p, sc, in.BatchID)
	}
	return out, nil
}

// quoteNodeID 是报价分镜图组时的占位节点：MCP 在用户确认之前不在画布上建节点（同 generate，MCPT-10），
// 而脚本域的预演要一个节点 ID 来判「这个节点上是不是已经有组了」—— 占位 ID 不可能是真实节点，判定恒为「没有」，
// 与真正建组时新建的节点结论一致；输入解析（ResolveForAccept）不要求节点已落盘。
const quoteNodeID = "mcp-quote"

func (a MCPScripts) groupReq(p mcpapp.Principal, in mcpapp.GroupCreateInput, nodeID string) agentapp.GroupCreateRequest {
	return agentapp.GroupCreateRequest{WorkspaceID: p.WorkspaceID, PayerAccountID: p.PayerAccountID, UserID: p.UserID,
		ScriptID: in.ScriptID, CanvasNodeID: nodeID, GroupType: in.GroupType, Name: in.Name, ShotIDs: in.ShotIDs,
		ModelCode: in.ModelCode, Operation: in.Operation, DurationSec: in.DurationSec, Durations: in.Durations,
		Params: in.Params}
}

func (a MCPScripts) QuoteGroupCreate(ctx context.Context, p mcpapp.Principal, in mcpapp.GroupCreateInput) (
	mcpapp.GroupQuote, error) {
	q, err := a.Agent.QuoteGroupCreate(ctx, a.groupReq(p, in, quoteNodeID))
	if err != nil {
		return mcpapp.GroupQuote{}, err
	}
	return mcpGroupQuote(q), nil
}

func (a MCPScripts) CreateGroup(ctx context.Context, p mcpapp.Principal, in mcpapp.GroupCreateInput) (
	mcpapp.GroupView, string, error) {
	sc, err := a.Agent.Script(ctx, p.WorkspaceID, in.ScriptID)
	if err != nil {
		return mcpapp.GroupView{}, "", err
	}
	params := map[string]any{"groupType": in.GroupType, "scriptId": strconv.FormatInt(in.ScriptID, 10),
		"projectId": strconv.FormatInt(sc.ProjectID, 10)}
	if in.ModelCode != "" {
		params["modelCode"] = in.ModelCode
	}
	var gv agentapp.GroupView
	nodeID, err := a.Orch.CreateObjectNode(ctx, mcpCaller(p, sc.ProjectID, in.BatchID),
		canvasgen.ObjectNode{NodeType: "group", Title: in.Name, Params: params}, "group",
		func(ctx context.Context, nodeID string) (map[string]any, error) {
			v, err := a.Agent.CreateGroup(ctx, a.groupReq(p, in, nodeID))
			if err != nil {
				return nil, unAgentErr(err)
			}
			gv = v
			return canvasgen.GroupNodeParams(groupFacts(v), in.ModelCode), nil
		})
	if err != nil {
		return mcpapp.GroupView{}, nodeID, err
	}
	return mcpGroupView(gv), nodeID, nil
}

func (a MCPScripts) executeReq(p mcpapp.Principal, in mcpapp.GroupExecuteInput) agentapp.GroupExecuteRequest {
	return agentapp.GroupExecuteRequest{WorkspaceID: p.WorkspaceID, PayerAccountID: p.PayerAccountID, UserID: p.UserID,
		GroupID: in.GroupID, ModelCode: in.ModelCode, Operation: in.Operation, DurationSec: in.DurationSec,
		Force: in.Force, ItemIDs: in.ItemIDs}
}

func (a MCPScripts) QuoteGroupExecute(ctx context.Context, p mcpapp.Principal, in mcpapp.GroupExecuteInput) (
	mcpapp.GroupQuote, error) {
	q, err := a.Agent.QuoteGroupExecute(ctx, a.executeReq(p, in))
	if err != nil {
		return mcpapp.GroupQuote{}, err
	}
	return mcpGroupQuote(q), nil
}

func (a MCPScripts) ExecuteGroup(ctx context.Context, p mcpapp.Principal, in mcpapp.GroupExecuteInput) (
	mcpapp.GroupView, error) {
	g, err := a.Agent.ExecuteGroup(ctx, a.executeReq(p, in))
	if err != nil {
		return mcpapp.GroupView{}, unAgentErr(err)
	}
	if in.BatchID != "" && a.Orch != nil && g.CanvasNodeID != "" {
		// 组节点若是本授权建的，把子项状态写回去（用户的组节点由前端自己轮询刷新，画布域会拒绝、只告警）。
		a.Orch.WriteBack(ctx, mcpCaller(p, g.ProjectID, in.BatchID), g.CanvasNodeID, "group",
			canvasgen.GroupNodeParams(groupFacts(g), in.ModelCode))
	}
	return mcpGroupView(g), nil
}

func (a MCPScripts) Group(ctx context.Context, p mcpapp.Principal, groupID int64) (mcpapp.GroupView, error) {
	g, err := a.Agent.Group(ctx, p.WorkspaceID, groupID)
	if err != nil {
		return mcpapp.GroupView{}, err
	}
	return mcpGroupView(g), nil
}

func scriptFacts(v agentapp.ScriptView) canvasgen.ScriptFacts {
	f := canvasgen.ScriptFacts{ID: v.ID, SourceType: v.SourceType, Status: v.Status, CurrentStep: v.CurrentStep,
		Version: v.Version}
	for _, sh := range v.Shots {
		f.Shots = append(f.Shots, canvasgen.ShotPromptFacts{ImagePromptStatus: sh.ImagePromptStatus,
			VideoPromptStatus: sh.VideoPromptStatus})
	}
	return f
}

func groupFacts(g agentapp.GroupView) canvasgen.GroupFacts {
	f := canvasgen.GroupFacts{ID: g.ID, ProjectID: g.ProjectID, ScriptID: g.ScriptID, GroupType: g.GroupType,
		Status: g.Status, Version: g.Version}
	for _, it := range g.Items {
		f.Items = append(f.Items, canvasgen.GroupItemFacts{ID: it.ID, ShotID: it.ShotID, ShotNo: it.ShotNo,
			AssetID: it.AssetID, GenerationID: it.GenerationID, Status: it.Status, Operation: it.Operation,
			DurationSec: it.DurationSec, Error: it.Error})
	}
	return f
}

func mcpScriptView(v agentapp.ScriptView, assetIDs map[int64][]int64) mcpapp.ScriptView {
	out := mcpapp.ScriptView{ID: v.ID, ProjectID: v.ProjectID, CanvasNodeID: v.CanvasNodeID, SourceType: v.SourceType,
		Status: v.Status, CurrentStep: v.CurrentStep, Version: v.Version}
	for _, sh := range v.Shots {
		out.Shots = append(out.Shots, mcpapp.ScriptShotView{ID: sh.ID, No: sh.No, DurationSec: sh.DurationSec,
			Description: sh.Summary, ImagePromptStatus: sh.ImagePromptStatus, VideoPromptStatus: sh.VideoPromptStatus,
			AssetIDs: assetIDs[sh.ID]})
	}
	return out
}

// detailView 是 script.get 的镜头表：描述给全文（Agent 视图只给 40 字摘要），引用资产按锚定顺序。
func detailView(d scriptapp.Detail) mcpapp.ScriptView {
	out := mcpapp.ScriptView{ID: d.Script.ID, ProjectID: d.Script.ProjectID, CanvasNodeID: d.Script.CanvasNodeID,
		SourceType: d.Script.SourceType, Status: d.Script.Status, CurrentStep: d.Script.CurrentStep,
		Version: d.Script.Version}
	for _, sh := range d.Shots {
		out.Shots = append(out.Shots, mcpapp.ScriptShotView{ID: sh.ID, No: sh.ShotNo, DurationSec: sh.DurationSec,
			Description: sh.Description, ImagePromptStatus: sh.ImagePromptStatus,
			VideoPromptStatus: sh.VideoPromptStatus, AssetIDs: d.AssetIDs[sh.ID]})
	}
	return out
}

func mcpGroupQuote(q agentapp.GroupQuote) mcpapp.GroupQuote {
	out := mcpapp.GroupQuote{Skipped: q.Skipped, Total: q.Total}
	for _, it := range q.Items {
		out.Items = append(out.Items, mcpapp.GroupItemQuote{ItemID: it.ItemID, ShotID: it.ShotID, ShotNo: it.ShotNo,
			Operation: it.Operation, DurationSec: it.DurationSec, Inputs: it.Inputs, Credits: it.Credits})
	}
	return out
}

func mcpGroupView(g agentapp.GroupView) mcpapp.GroupView {
	out := mcpapp.GroupView{ID: g.ID, ProjectID: g.ProjectID, ScriptID: g.ScriptID, CanvasNodeID: g.CanvasNodeID,
		GroupType: g.GroupType, Name: g.Name, Status: g.Status, Version: g.Version}
	for _, it := range g.Items {
		out.Items = append(out.Items, mcpapp.GroupItemView{ID: it.ID, ItemNo: it.ItemNo, ShotID: it.ShotID,
			ShotNo: it.ShotNo, AssetID: it.AssetID, GenerationID: it.GenerationID, Status: it.Status,
			Credits: it.Credits, Operation: it.Operation, DurationSec: it.DurationSec, Inputs: it.Inputs, Error: it.Error})
	}
	return out
}

// ---------------------------------------------------------------- 整图

// MCPWorkflows 实现 mcpapp.Workflows（取消走 MCPTaskControl，与 tasks.cancel 同一条路）。
type MCPWorkflows struct{ Agent AgentWorkflows }

func (a MCPWorkflows) planReq(p mcpapp.Principal, in mcpapp.WorkflowPlanInput) agentapp.WorkflowPlanRequest {
	return agentapp.WorkflowPlanRequest{WorkspaceID: p.WorkspaceID, ProjectID: in.ProjectID,
		MemberLevelID: p.MemberLevelID, TargetNodeIDs: in.TargetNodeIDs, Force: in.Force}
}

func (a MCPWorkflows) Plan(ctx context.Context, p mcpapp.Principal, in mcpapp.WorkflowPlanInput) (mcpapp.WorkflowPlan, error) {
	v, err := a.Agent.Plan(ctx, a.planReq(p, in))
	if err != nil {
		return mcpapp.WorkflowPlan{}, err
	}
	out := mcpapp.WorkflowPlan{Executable: v.Executable, Summary: v.Summary, RunCount: v.RunCount,
		ReuseCount: v.ReuseCount, SkipCount: v.SkipCount, TotalCredits: v.TotalCredits,
		EstimatedSeconds: v.EstimatedSeconds}
	for _, is := range v.Issues {
		out.Issues = append(out.Issues, mcpapp.WorkflowIssue{Message: is.Message, Nodes: is.Nodes})
	}
	for _, b := range v.Blocked {
		out.Blocked = append(out.Blocked, mcpapp.WorkflowBlocked{NodeID: b.NodeID, Reason: b.Reason})
	}
	return out, nil
}

func (a MCPWorkflows) Start(ctx context.Context, p mcpapp.Principal, in mcpapp.WorkflowPlanInput) (
	mcpapp.WorkflowRunView, error) {
	v, err := a.Agent.Start(ctx, agentapp.WorkflowStartRequest{WorkflowPlanRequest: a.planReq(p, in),
		PayerAccountID: p.PayerAccountID, UserID: p.UserID})
	if err != nil {
		return mcpapp.WorkflowRunView{}, unAgentErr(err)
	}
	return a.Run(ctx, p, v.ID)
}

func (a MCPWorkflows) Run(ctx context.Context, p mcpapp.Principal, runID int64) (mcpapp.WorkflowRunView, error) {
	v, err := a.Agent.Svc.Run(ctx, p.WorkspaceID, runID)
	if err != nil {
		return mcpapp.WorkflowRunView{}, err
	}
	out := mcpapp.WorkflowRunView{ID: v.Run.ID, ProjectID: v.Run.ProjectID, Status: v.Run.Status,
		TotalCredits: v.Run.TotalCredits}
	for _, n := range v.Nodes {
		out.Nodes = append(out.Nodes, mcpapp.WorkflowNodeView{NodeID: n.NodeID, Status: n.Status,
			GenerationID: n.GenerationID, Credits: n.CreditsCost, Reused: n.IsReused, Error: n.ErrorMessage})
	}
	return out, nil
}

// ---------------------------------------------------------------- 时间线

// MCPTimelines 实现 mcpapp.Timelines。
type MCPTimelines struct {
	Agent AgentTimelines
	// Scripts 只用来由 scriptId 推出项目（只给 scriptId 时）。
	Scripts *scriptapp.Service
}

func (a MCPTimelines) Assemble(ctx context.Context, p mcpapp.Principal, ref mcpapp.TimelineRef, force, dryRun bool) (
	mcpapp.AssembleOutcome, error) {
	ws := p.WorkspaceID
	if ref.ProjectID == 0 && ref.ScriptID != 0 && a.Scripts != nil {
		d, err := a.Scripts.Get(ctx, ws, ref.ScriptID)
		if err != nil {
			return mcpapp.AssembleOutcome{}, err
		}
		ref.ProjectID = d.Script.ProjectID
	}
	tl, found, err := a.Agent.Find(ctx, ws, ref.ProjectID, ref.ScriptID, ref.TimelineID)
	if err != nil {
		return mcpapp.AssembleOutcome{}, err
	}
	if !found && ref.TimelineID != 0 {
		return mcpapp.AssembleOutcome{}, httpx.Errf(errcode.NotFound, "时间线不存在")
	}
	if !found && dryRun {
		// 空跑不建时间线：按「还没有任何片段」给计划（与装配同一个 PlanAssemble）。
		plan, err := a.Agent.Svc.PreviewAssemble(ctx, ws, ref.ScriptID, force)
		if err != nil {
			return mcpapp.AssembleOutcome{}, err
		}
		out := mcpapp.AssembleOutcome{Summary: plan.Summary(), Append: len(plan.Append), Skipped: plan.Skipped,
			DryRun: true}
		for _, g := range plan.Gaps {
			out.Gaps = append(out.Gaps, mcpapp.AssembleGap{ShotNo: g.ShotNo, Reason: g.Reason})
		}
		return out, nil
	}
	if !found {
		if tl, err = a.Agent.Ensure(ctx, agentapp.TimelineEnsureRequest{WorkspaceID: ws, ProjectID: ref.ProjectID,
			ScriptID: ref.ScriptID, Name: ref.Name, AspectRatio: ref.AspectRatio}); err != nil {
			return mcpapp.AssembleOutcome{}, err
		}
	}
	res, err := a.Agent.Assemble(ctx, agentapp.AssembleRequest{WorkspaceID: ws, TimelineID: tl.ID, Version: tl.Version,
		Force: force, DryRun: dryRun, UserID: p.UserID, EditorKind: timelinerepo.EditorMCP})
	if err != nil {
		return mcpapp.AssembleOutcome{}, err
	}
	view := mcpTimelineView(res.Timeline)
	view.Created = tl.Created
	out := mcpapp.AssembleOutcome{Summary: res.Summary, Append: res.Append, Skipped: res.Skipped, Timeline: &view,
		DryRun: dryRun}
	for _, g := range res.Gaps {
		out.Gaps = append(out.Gaps, mcpapp.AssembleGap{ShotNo: g.ShotNo, Reason: g.Reason})
	}
	return out, nil
}

func (a MCPTimelines) renderReq(p mcpapp.Principal, timelineID int64, key string) agentapp.RenderRequest {
	return agentapp.RenderRequest{WorkspaceID: p.WorkspaceID, PayerAccountID: p.PayerAccountID, UserID: p.UserID,
		TimelineID: timelineID, MemberLevelID: p.MemberLevelID, IdempotencyKey: key}
}

func (a MCPTimelines) PrecheckRender(ctx context.Context, p mcpapp.Principal, timelineID int64) (mcpapp.RenderPrecheck, error) {
	pre, err := a.Agent.PrecheckRender(ctx, a.renderReq(p, timelineID, ""))
	if err != nil {
		return mcpapp.RenderPrecheck{}, err
	}
	out := mcpapp.RenderPrecheck{DurationMs: pre.DurationMs, Credits: pre.Credits}
	for _, is := range pre.Issues {
		out.Issues = append(out.Issues, mcpapp.RenderIssue{Code: is.Code, Detail: is.Detail, ClipIdx: is.ClipIdx})
	}
	return out, nil
}

func (a MCPTimelines) StartRender(ctx context.Context, p mcpapp.Principal, timelineID int64, key string) (
	mcpapp.RenderView, error) {
	v, err := a.Agent.StartRender(ctx, a.renderReq(p, timelineID, key))
	if err != nil {
		return mcpapp.RenderView{}, unAgentErr(err)
	}
	if r, err := a.Render(ctx, p, v.ID); err == nil {
		return r, nil
	}
	return mcpapp.RenderView{ID: v.ID, TimelineID: timelineID, Status: v.Status, Credits: v.Credits}, nil
}

func (a MCPTimelines) Get(ctx context.Context, p mcpapp.Principal, timelineID int64) (mcpapp.TimelineView,
	*mcpapp.RenderView, error) {
	tl, err := a.Agent.Get(ctx, p.WorkspaceID, timelineID)
	if err != nil {
		return mcpapp.TimelineView{}, nil, err
	}
	list, err := a.Agent.Svc.ListRenders(ctx, p.WorkspaceID, timelineID, 1)
	if err != nil {
		return mcpapp.TimelineView{}, nil, err
	}
	view := mcpTimelineView(tl)
	if len(list) == 0 {
		return view, nil, nil
	}
	r := mcpRenderView(list[0].ID, list[0].TimelineID, list[0].Status, list[0].Progress, list[0].Stage,
		list[0].CreditsCost, list[0].OutputMediaID, list[0].ErrorMessage)
	return view, &r, nil
}

func (a MCPTimelines) Render(ctx context.Context, p mcpapp.Principal, renderID int64) (mcpapp.RenderView, error) {
	j, err := a.Agent.Svc.GetRender(ctx, p.WorkspaceID, renderID)
	if err != nil {
		return mcpapp.RenderView{}, err
	}
	return mcpRenderView(j.ID, j.TimelineID, j.Status, j.Progress, j.Stage, j.CreditsCost, j.OutputMediaID,
		j.ErrorMessage), nil
}

func mcpRenderView(id, timelineID int64, status string, progress int, stage string, credits int, media int64,
	errMsg string) mcpapp.RenderView {
	return mcpapp.RenderView{ID: id, TimelineID: timelineID, Status: status, Progress: progress, Stage: stage,
		Credits: credits, OutputMediaID: media, Error: errMsg}
}

func mcpTimelineView(v agentapp.TimelineView) mcpapp.TimelineView {
	return mcpapp.TimelineView{ID: v.ID, ScriptID: v.ScriptID, Version: v.Version, Name: v.Name,
		AspectRatio: v.AspectRatio, ClipCount: v.ClipCount, TotalMs: v.TotalMs, EffectiveMs: v.EffectiveMs,
		Created: v.Created}
}

// ---------------------------------------------------------------- 技能提示词（F-37）

// SkillLister 列出启用的技能（agent/app.Service.Skills 满足：读的是 repo.ListEnabledSkills）。
type SkillLister interface {
	Skills(ctx context.Context) ([]agentrepo.Skill, error)
}

// MCPSkills 实现 mcpapp.Skills：启用的技能 + 渲染后的正文（与站内 Agent 同一个渲染器、同一份变量）。
//
// ⚠️ 变量要给全（CLAUDE.md §5.7）：SkillVars 依赖会话状态与模型码，MCP 没有会话 —— 状态给 executing
// （外部客户端直接执行，没有站内的规划阶段），推理模型给空串（技能正文本来就要容忍它为空）。
// ⚠️ 渲染失败的技能**不出现**并告警：原样给出的话外部模型会照着 `{{var}}` 字面量执行，而这不报错。
type MCPSkills struct {
	Lister SkillLister
	Render agentapp.SkillRenderer
	Logger *slog.Logger
}

// mcpSkillVars 是 MCP 场景下渲染技能用的变量（agentapp.SkillVars 的全部键，值见上）。
func mcpSkillVars() map[string]string { return agentapp.SkillVars(agentdomain.StateExecuting, "") }

func (a MCPSkills) Prompts(ctx context.Context) ([]mcpapp.SkillPrompt, error) {
	rows, err := a.Lister.Skills(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]mcpapp.SkillPrompt, 0, len(rows))
	for _, s := range rows {
		if !s.IsEnabled {
			continue // 列表本来只含启用的；这里再挡一道（停用的技能绝不出现）
		}
		// ⚠️ 只给通用场景的技能（MKD-22：营销不对 MCP 开放，MKT-T-32）。营销专用技能的正文假定了
		// 营销会话才有的东西（钉住的选择项、商品槽、view_media 等营销工具），外部客户端照着执行的话
		// 第一步就会调一个它没有的工具。判定复用 SkillInScene（唯一实现），不另写一份。
		if !agentdomain.SkillInScene(s.SceneList(), agentdomain.SkillSceneGeneral) {
			continue
		}
		body := s.Body
		if a.Render != nil {
			r, err := a.Render.RenderSkill(s.Body, mcpSkillVars())
			if err != nil {
				a.logger().Warn("技能正文渲染失败，不作为 MCP 提示词提供", "skill", s.Code, "err", err)
				continue
			}
			body = r
		}
		out = append(out, mcpapp.SkillPrompt{Code: s.Code, Name: s.Name, Description: s.Description, Body: body})
	}
	return out, nil
}

func (a MCPSkills) logger() *slog.Logger {
	if a.Logger != nil {
		return a.Logger
	}
	return slog.Default()
}

var (
	_ mcpapp.Scripts   = MCPScripts{}
	_ mcpapp.Workflows = MCPWorkflows{}
	_ mcpapp.Timelines = MCPTimelines{}
	_ mcpapp.Skills    = MCPSkills{}
)
