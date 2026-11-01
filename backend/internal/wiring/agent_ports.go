package wiring

// 画布 Agent 多段工具（AGT-50）的端口适配器：脚本线 / 整图 / 时间线，以及扫描循环按业务引用查终态。
//
// ⚠️ 适配器只做参数转换与报价组合，**不复制任何业务规则**：分镜的结算晚于校验、轮次随调用
// 次数变、资产图带 scriptAssetId、组子项带 groupItemId、余额不足停掉后续项……全部发生在被调用的
// 那个域里。这里调的就是 HTTP handler 调的同一个方法。
// ⚠️ 报价与真实扣费同一个口径：没有只读报价入口的（分镜、资产、建分镜图组、组执行）按
// 「提交时真正会发出去的那组参数」逐项问目录（同 QuoteWithFacts / 受理侧的输入解析），
// 会员等级也与真实受理一致（组子项 / 资产图的受理不带会员等级，这里同样不带）。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	agentapp "github.com/li904338203/tabtv/backend/internal/agent/app"
	agenttools "github.com/li904338203/tabtv/backend/internal/agent/tools"
	billingrepo "github.com/li904338203/tabtv/backend/internal/billing/repo"
	catalogapp "github.com/li904338203/tabtv/backend/internal/catalog/app"
	directorapp "github.com/li904338203/tabtv/backend/internal/director/app"
	gendomain "github.com/li904338203/tabtv/backend/internal/generation/domain"
	genrepo "github.com/li904338203/tabtv/backend/internal/generation/repo"
	scriptapp "github.com/li904338203/tabtv/backend/internal/script/app"
	scriptdomain "github.com/li904338203/tabtv/backend/internal/script/domain"
	scriptrepo "github.com/li904338203/tabtv/backend/internal/script/repo"
	smartcutapp "github.com/li904338203/tabtv/backend/internal/smartcut/app"
	timelineapp "github.com/li904338203/tabtv/backend/internal/timeline/app"
	timelinedomain "github.com/li904338203/tabtv/backend/internal/timeline/domain"
	timelinerepo "github.com/li904338203/tabtv/backend/internal/timeline/repo"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
	workflowapp "github.com/li904338203/tabtv/backend/internal/workflow/app"
	workflowdomain "github.com/li904338203/tabtv/backend/internal/workflow/domain"
	workflowrepo "github.com/li904338203/tabtv/backend/internal/workflow/repo"
)

// ---------------------------------------------------------------- 脚本

// AgentScripts 把脚本域接成 agentapp.Scripts。
type AgentScripts struct {
	Svc  *scriptapp.Service
	Repo *scriptrepo.Repo
	// Quoter 逐项报价（与受理同一条输入解析 + QuoteWithFacts）。
	Quoter AgentGenQuoter
}

func (a AgentScripts) CreateScript(ctx context.Context, in agentapp.ScriptCreateInput) (agentapp.ScriptView, error) {
	d, err := a.Svc.Create(ctx, scriptapp.CreateInput{
		WorkspaceID: in.WorkspaceID, ProjectID: in.ProjectID, CanvasNodeID: in.CanvasNodeID,
		SourceType: in.SourceType, RawInput: in.RawInput, GlobalStyle: in.GlobalStyle,
	})
	if err != nil {
		return agentapp.ScriptView{}, err
	}
	return scriptView(d), nil
}

func (a AgentScripts) Script(ctx context.Context, ws, scriptID int64) (agentapp.ScriptView, error) {
	d, err := a.Svc.Get(ctx, ws, scriptID)
	if err != nil {
		return agentapp.ScriptView{}, err
	}
	return scriptView(d), nil
}

// QuoteStoryboard 与 GenerateStoryboard 第①步同一个报价（同一段文本、同一会员等级、同一操作码）。
func (a AgentScripts) QuoteStoryboard(ctx context.Context, in agentapp.StoryboardRequest) (int, error) {
	d, err := a.Svc.Get(ctx, in.WorkspaceID, in.ScriptID)
	if err != nil {
		return 0, err
	}
	q, err := a.Quoter.Catalog.QuoteWithFacts(ctx, catalogapp.QuoteRequest{
		ModelCode: in.ModelCode, OperationCode: string(scriptdomain.PurposeStoryboard),
		Count: 1, Text: d.Script.RawInput + in.ExtraContext, MemberLevelID: in.MemberLevelID,
	})
	if err != nil {
		return 0, err
	}
	return q.Credits, nil
}

func (a AgentScripts) Storyboard(ctx context.Context, in agentapp.StoryboardRequest) (agentapp.StoryboardOutcome, error) {
	res, err := a.Svc.GenerateStoryboard(ctx, scriptapp.StoryboardInput{
		WorkspaceID: in.WorkspaceID, PayerAccountID: in.PayerAccountID, UserID: in.UserID,
		ScriptID: in.ScriptID, ModelCode: in.ModelCode, MemberLevelID: in.MemberLevelID,
		ShotCount: in.ShotCount, ExtraContext: in.ExtraContext,
	})
	if err != nil {
		return agentapp.StoryboardOutcome{}, translateInsufficient(ctx, err)
	}
	return agentapp.StoryboardOutcome{Script: scriptView(res.Detail), CallID: res.CallID, Credits: res.Credits}, nil
}

// QuoteAssets 先对账（免费、幂等），再按「待出图资产 × 每张的价」报。每张按它**真正会发出去**的
// 提示词报（per_kchar 时才对得上）；会员等级与资产图受理一致（不带）。
func (a AgentScripts) QuoteAssets(ctx context.Context, in agentapp.AssetsRequest) (agentapp.AssetsQuote, error) {
	rec, err := a.Svc.ReconcileAssets(ctx, in.WorkspaceID, in.ScriptID)
	if err != nil {
		return agentapp.AssetsQuote{}, err
	}
	pending, err := a.Svc.PendingAssets(ctx, in.WorkspaceID, in.ScriptID, in.AssetIDs)
	if err != nil {
		return agentapp.AssetsQuote{}, err
	}
	out := agentapp.AssetsQuote{Unmatched: rec.Unmatched}
	for _, p := range pending {
		q, err := a.Quoter.Catalog.QuoteWithFacts(ctx, catalogapp.QuoteRequest{
			ModelCode: in.ModelCode, OperationCode: "asset_gen", Count: 1, Text: p.Prompt,
		})
		if err != nil {
			return agentapp.AssetsQuote{}, err
		}
		if out.Unit == 0 {
			out.Unit = q.Credits
		}
		out.Total += q.Credits
		out.Pending = append(out.Pending, agentapp.AssetBrief{ID: p.Asset.ID, Name: p.Asset.Name,
			Type: p.Asset.Type, Status: p.Asset.Status})
	}
	return out, nil
}

func (a AgentScripts) GenerateAssets(ctx context.Context, in agentapp.AssetsRequest) (agentapp.AssetsOutcome, error) {
	items, err := a.Svc.GenerateAssets(ctx, scriptapp.BatchAssetInput{
		WorkspaceID: in.WorkspaceID, PayerAccountID: in.PayerAccountID, UserID: in.UserID,
		ScriptID: in.ScriptID, ModelCode: in.ModelCode, AssetIDs: in.AssetIDs, Params: in.Params,
	})
	if err != nil {
		return agentapp.AssetsOutcome{}, translateInsufficient(ctx, err)
	}
	var out agentapp.AssetsOutcome
	for _, it := range items {
		out.Items = append(out.Items, agentapp.AssetItemOutcome{AssetID: it.AssetID, Name: it.Name,
			GenerationID: it.GenerationID, Skipped: it.Skipped, Reason: it.Reason})
		// 跳过的（含「这段描述已经生成过」那种命中旧任务的）这次没有扣费，不进这一笔的引用。
		if it.GenerationID != 0 && !it.Skipped {
			out.GenerationIDs = append(out.GenerationIDs, it.GenerationID)
		}
	}
	return out, nil
}

func (a AgentScripts) composeInput(in agentapp.ComposeRequest, dry bool) scriptapp.ComposeInput {
	kinds := make([]scriptdomain.PromptKind, 0, len(in.Kinds))
	for _, k := range in.Kinds {
		kinds = append(kinds, scriptdomain.PromptKind(k))
	}
	return scriptapp.ComposeInput{
		WorkspaceID: in.WorkspaceID, PayerAccountID: in.PayerAccountID, UserID: in.UserID,
		ScriptID: in.ScriptID, ShotIDs: in.ShotIDs, Kinds: kinds,
		Source: scriptdomain.PromptSource(in.Source), ModelCode: in.ModelCode, Force: in.Force, DryRun: dry,
	}
}

// QuoteCompose 走脚本域自己的 DryRun（GAP-21：费用由服务端统一给）。
func (a AgentScripts) QuoteCompose(ctx context.Context, in agentapp.ComposeRequest) (agentapp.ComposeQuote, error) {
	res, err := a.Svc.Compose(ctx, a.composeInput(in, true))
	if err != nil {
		return agentapp.ComposeQuote{}, err
	}
	return composeQuote(res.Quote), nil
}

// Compose 合成，并把这一次合成产生的全部同步调用挂出来（额度条目按它们逐笔对账本）。
func (a AgentScripts) Compose(ctx context.Context, in agentapp.ComposeRequest) (agentapp.ComposeOutcome, error) {
	purpose := string(scriptdomain.PurposeCompose)
	before, err := a.Repo.MaxCallID(ctx, in.ScriptID, purpose)
	if err != nil {
		return agentapp.ComposeOutcome{}, err
	}
	res, err := a.Svc.Compose(ctx, a.composeInput(in, false))
	if err != nil {
		return agentapp.ComposeOutcome{}, translateInsufficient(ctx, err)
	}
	out := agentapp.ComposeOutcome{Quote: composeQuote(res.Quote)}
	for _, it := range res.Items {
		out.Items = append(out.Items, agentapp.ComposeItemOutcome{ShotID: it.ShotID, ShotNo: it.ShotNo,
			Kind: string(it.Kind), Status: string(it.Status), Skipped: it.Skipped, Reason: it.Reason})
	}
	// 模型返回之后的查询脱离请求取消（钱已经扣了，引用不能因为 ctx 取消而丢）。
	if out.CallIDs, err = a.Repo.CallIDsAfter(context.WithoutCancel(ctx), in.WorkspaceID, in.ScriptID,
		purpose, in.UserID, before); err != nil {
		return out, err
	}
	return out, nil
}

func composeQuote(q scriptdomain.BatchQuote) agentapp.ComposeQuote {
	return agentapp.ComposeQuote{AICount: q.AICount, TemplateCount: q.TemplateCount, SkipCount: q.SkipCount,
		Unit: q.UnitCost, Total: q.Total}
}

func (a AgentScripts) createInput(in agentapp.GroupCreateRequest) scriptapp.CreateGroupInput {
	return scriptapp.CreateGroupInput{
		WorkspaceID: in.WorkspaceID, PayerAccountID: in.PayerAccountID, UserID: in.UserID,
		ScriptID: in.ScriptID, CanvasNodeID: in.CanvasNodeID, GroupType: scriptdomain.GroupType(in.GroupType),
		Name: in.Name, ShotIDs: in.ShotIDs, ModelCode: in.ModelCode, Operation: in.Operation,
		DurationSec: in.DurationSec, Params: in.Params, Durations: in.Durations,
	}
}

// QuoteGroupCreate 只对分镜图组报价（建组即提交）。视频组 / 资产组建组不扣费，返回空报价。
func (a AgentScripts) QuoteGroupCreate(ctx context.Context, in agentapp.GroupCreateRequest) (agentapp.GroupQuote, error) {
	if in.GroupType != string(scriptdomain.GroupStoryboardImage) {
		return agentapp.GroupQuote{}, nil
	}
	plan, err := a.Svc.PlanStoryboardImageGroup(ctx, a.createInput(in))
	if err != nil {
		return agentapp.GroupQuote{}, err
	}
	return a.quotePlan(ctx, plan)
}

func (a AgentScripts) CreateGroup(ctx context.Context, in agentapp.GroupCreateRequest) (agentapp.GroupView, error) {
	g, err := a.Svc.CreateGroup(ctx, a.createInput(in))
	if err != nil {
		return agentapp.GroupView{}, translateInsufficient(ctx, err)
	}
	return a.groupView(ctx, g), nil
}

func (a AgentScripts) executeInput(in agentapp.GroupExecuteRequest) scriptapp.ExecuteGroupInput {
	return scriptapp.ExecuteGroupInput{
		WorkspaceID: in.WorkspaceID, PayerAccountID: in.PayerAccountID, UserID: in.UserID,
		GroupID: in.GroupID, ModelCode: in.ModelCode, Operation: in.Operation, DurationSec: in.DurationSec,
		Force: in.Force, ItemIDs: in.ItemIDs,
	}
}

func (a AgentScripts) QuoteGroupExecute(ctx context.Context, in agentapp.GroupExecuteRequest) (agentapp.GroupQuote, error) {
	plan, err := a.Svc.PlanGroupExecution(ctx, a.executeInput(in))
	if err != nil {
		return agentapp.GroupQuote{}, err
	}
	return a.quotePlan(ctx, plan)
}

func (a AgentScripts) ExecuteGroup(ctx context.Context, in agentapp.GroupExecuteRequest) (agentapp.GroupView, error) {
	g, err := a.Svc.ExecuteGroup(ctx, a.executeInput(in))
	if err != nil {
		return agentapp.GroupView{}, translateInsufficient(ctx, err)
	}
	return a.groupView(ctx, g), nil
}

func (a AgentScripts) Group(ctx context.Context, ws, groupID int64) (agentapp.GroupView, error) {
	g, err := a.Svc.Group(ctx, ws, groupID)
	if err != nil {
		return agentapp.GroupView{}, err
	}
	return a.groupView(ctx, g), nil
}

// quotePlan 逐项报价：每项用它**真正会发出去的**受理请求（同一条输入解析 + QuoteWithFacts），
// 会员等级与组子项的受理一致（不带）。
func (a AgentScripts) quotePlan(ctx context.Context, plan scriptapp.GroupExecutionPlan) (agentapp.GroupQuote, error) {
	out := agentapp.GroupQuote{Skipped: plan.Skipped}
	for _, p := range plan.Items {
		r := p.Request
		q, err := a.Quoter.QuoteGeneration(ctx, agentapp.QuoteInput{
			ModelCode: r.ModelCode, OperationCode: r.OperationCode, Text: r.Prompt, Count: 1,
			DurationSec: r.DurationSec, WorkspaceID: r.WorkspaceID, ProjectID: r.ProjectID, NodeID: r.NodeID,
			DeclaredInputs: r.DeclaredInputs,
		})
		if err != nil {
			label := fmt.Sprintf("第 %d 镜", p.ShotNo)
			return agentapp.GroupQuote{}, fmt.Errorf("%s 报不出价：%w", label, err)
		}
		out.Items = append(out.Items, agentapp.GroupItemQuote{ItemID: p.ItemID, ShotID: r.ShotID, ShotNo: p.ShotNo,
			Operation: r.OperationCode, DurationSec: r.DurationSec, Inputs: countArray(r.DeclaredInputs),
			Credits: q.Credits})
		out.Total += q.Credits
	}
	return out, nil
}

func (a AgentScripts) groupView(ctx context.Context, g scriptapp.GroupView) agentapp.GroupView {
	shotNo := map[int64]int{}
	if a.Repo != nil && g.Group.ScriptID != 0 {
		if shots, err := a.Repo.ListShots(ctx, g.Group.ScriptID); err == nil {
			for _, sh := range shots {
				shotNo[sh.ID] = sh.ShotNo
			}
		}
	}
	out := agentapp.GroupView{ID: g.Group.ID, ProjectID: g.Group.ProjectID, ScriptID: g.Group.ScriptID,
		CanvasNodeID: g.Group.CanvasNodeID, GroupType: g.Group.GroupType, Name: g.Group.Name,
		Status: g.Group.Status, Version: g.Group.Version}
	for _, it := range g.Items {
		v := agentapp.GroupItemView{ID: it.ID, ItemNo: it.ItemNo, ShotID: it.ShotID, ShotNo: shotNo[it.ShotID],
			AssetID: it.ScriptAssetID, GenerationID: it.GenerationID, Status: it.Status, Credits: it.CreditsCost,
			Inputs: countArray(it.InputRefs), Error: it.ErrorMessage}
		var params map[string]any
		if json.Unmarshal(it.ParamsSnapshot, &params) == nil {
			v.Operation, _ = params["operation"].(string)
			if d, ok := params["duration"].(float64); ok {
				v.DurationSec = int(d)
			}
		}
		out.Items = append(out.Items, v)
	}
	return out
}

func scriptView(d scriptapp.Detail) agentapp.ScriptView {
	v := agentapp.ScriptView{ID: d.Script.ID, ProjectID: d.Script.ProjectID, CanvasNodeID: d.Script.CanvasNodeID,
		SourceType: d.Script.SourceType, Status: d.Script.Status, CurrentStep: d.Script.CurrentStep,
		Version: d.Script.Version}
	for _, sh := range d.Shots {
		v.Shots = append(v.Shots, agentapp.ScriptShot{ID: sh.ID, No: sh.ShotNo, DurationSec: sh.DurationSec,
			Summary: cutRunes(sh.Description, 40), ImagePromptStatus: sh.ImagePromptStatus,
			VideoPromptStatus: sh.VideoPromptStatus})
	}
	return v
}

func countArray(raw json.RawMessage) int {
	var arr []json.RawMessage
	if json.Unmarshal(raw, &arr) != nil {
		return 0
	}
	return len(arr)
}

func cutRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "…"
}

// translateInsufficient 把业务码 INSUFFICIENT_CREDITS 翻成 agent 的余额不足错误
// （工具据此回给模型「余额不足，不要重试」，而不是一句原样的业务错误）。
func translateInsufficient(_ context.Context, err error) error {
	if bizCode(err) == errcode.InsufficientCredits {
		return &agentapp.InsufficientCreditsError{Balance: -1, Cause: err}
	}
	return err
}

// ---------------------------------------------------------------- 整图

// AgentWorkflows 把整图域接成 agentapp.Workflows。
type AgentWorkflows struct{ Svc *workflowapp.Service }

func (a AgentWorkflows) planInput(in agentapp.WorkflowPlanRequest) workflowapp.PlanInput {
	return workflowapp.PlanInput{WorkspaceID: in.WorkspaceID, ProjectID: in.ProjectID,
		MemberLevelID: in.MemberLevelID, TargetNodeIDs: in.TargetNodeIDs, Force: in.Force}
}

func (a AgentWorkflows) Plan(ctx context.Context, in agentapp.WorkflowPlanRequest) (agentapp.WorkflowPlanView, error) {
	p, err := a.Svc.Plan(ctx, a.planInput(in))
	if err != nil {
		return agentapp.WorkflowPlanView{}, err
	}
	out := agentapp.WorkflowPlanView{Executable: p.Executable(), Summary: p.Summary(), RunCount: p.RunCount,
		ReuseCount: p.ReuseCount, SkipCount: p.SkipCount, TotalCredits: p.TotalCredits,
		EstimatedSeconds: p.EstimatedSeconds}
	for _, is := range p.Issues {
		out.Issues = append(out.Issues, agentapp.WorkflowIssue{Message: is.Message, Nodes: is.Nodes})
	}
	for _, n := range p.Nodes {
		if n.Action == workflowdomain.ActionBlocked {
			reason := n.Reason
			if len(n.Blockers) > 0 {
				reason = strings.Join(n.Blockers, "；")
			}
			out.Blocked = append(out.Blocked, agentapp.WorkflowBlocked{NodeID: n.ID, Reason: reason})
		}
	}
	return out, nil
}

func (a AgentWorkflows) Start(ctx context.Context, in agentapp.WorkflowStartRequest) (agentapp.WorkflowRunView, error) {
	v, err := a.Svc.Start(ctx, workflowapp.StartInput{
		WorkspaceID: in.WorkspaceID, PayerAccountID: in.PayerAccountID, UserID: in.UserID,
		ProjectID: in.ProjectID, MemberLevelID: in.MemberLevelID, TargetNodeIDs: in.TargetNodeIDs,
		Force: in.Force,
	})
	if err != nil {
		return agentapp.WorkflowRunView{}, err
	}
	return runView(v), nil
}

func (a AgentWorkflows) Run(ctx context.Context, ws, runID int64) (agentapp.WorkflowRunView, error) {
	v, err := a.Svc.Run(ctx, ws, runID)
	if err != nil {
		return agentapp.WorkflowRunView{}, err
	}
	return runView(v), nil
}

func runView(v workflowapp.RunView) agentapp.WorkflowRunView {
	return agentapp.WorkflowRunView{ID: v.Run.ID, Status: v.Run.Status, TotalCredits: v.Run.TotalCredits,
		Nodes: len(v.Nodes)}
}

// ---------------------------------------------------------------- 时间线

// AgentTimelines 把时间线域接成 agentapp.Timelines。
type AgentTimelines struct{ Svc *timelineapp.Service }

func (a AgentTimelines) Ensure(ctx context.Context, in agentapp.TimelineEnsureRequest) (agentapp.TimelineView, error) {
	v, found, err := a.Find(ctx, in.WorkspaceID, in.ProjectID, in.ScriptID, in.TimelineID)
	if err != nil || found {
		return v, err
	}
	created, err := a.Svc.Create(ctx, timelineapp.CreateInput{
		WorkspaceID: in.WorkspaceID, ProjectID: in.ProjectID, ScriptID: in.ScriptID,
		Name: in.Name, AspectRatio: in.AspectRatio,
	})
	if err != nil {
		return agentapp.TimelineView{}, err
	}
	return timelineView(created, true), nil
}

// Find 只读地找时间线（Ensure 的前半段，MCPT-16 的空跑装配也用它 —— 空跑不能为了看一眼计划就建一条）：
// timelineID 非 0 取那一条（且必须属于 scriptID，若给了）；否则取该脚本最新的一条。都没有时 found=false。
func (a AgentTimelines) Find(ctx context.Context, ws, projectID, scriptID, timelineID int64) (
	agentapp.TimelineView, bool, error) {
	id := timelineID
	if id == 0 && scriptID != 0 {
		// 同一个脚本已经有时间线就用它：每次都新建的话，模型重试一次装配就多一条空时间线，
		// 而用户手工在旧的那条上做的调整没人管了。
		list, err := a.Svc.List(ctx, ws, projectID)
		if err != nil {
			return agentapp.TimelineView{}, false, err
		}
		for _, t := range list {
			if t.ScriptID == scriptID && t.ID > id {
				id = t.ID
			}
		}
	}
	if id == 0 {
		return agentapp.TimelineView{}, false, nil
	}
	v, err := a.Svc.Get(ctx, ws, id)
	if err != nil {
		return agentapp.TimelineView{}, false, err
	}
	if scriptID != 0 && v.Timeline.ScriptID != scriptID && timelineID != 0 {
		return agentapp.TimelineView{}, false, httpx.Errf(errcode.InvalidArgument, "这条时间线不属于该脚本")
	}
	return timelineView(v, false), true, nil
}

// Get 取时间线摘要（MCPT-16 的 timeline.get；Agent 端口不需要它）。
func (a AgentTimelines) Get(ctx context.Context, ws, timelineID int64) (agentapp.TimelineView, error) {
	v, err := a.Svc.Get(ctx, ws, timelineID)
	if err != nil {
		return agentapp.TimelineView{}, err
	}
	return timelineView(v, false), nil
}

func (a AgentTimelines) Assemble(ctx context.Context, in agentapp.AssembleRequest) (agentapp.AssembleOutcome, error) {
	res, err := a.Svc.Assemble(ctx, timelineapp.AssembleInput{
		WorkspaceID: in.WorkspaceID, TimelineID: in.TimelineID, Version: in.Version,
		Force: in.Force, DryRun: in.DryRun,
		Editor: timelinerepo.Editor{UserID: in.UserID, Kind: in.EditorKind},
	})
	if err != nil {
		return agentapp.AssembleOutcome{}, err
	}
	out := agentapp.AssembleOutcome{Summary: res.Plan.Summary(), Append: len(res.Plan.Append),
		Skipped: res.Plan.Skipped, Timeline: timelineView(res.View, false)}
	for _, g := range res.Plan.Gaps {
		out.Gaps = append(out.Gaps, agentapp.AssembleGap{ShotNo: g.ShotNo, Reason: g.Reason})
	}
	return out, nil
}

func (a AgentTimelines) renderInput(in agentapp.RenderRequest, dry bool) timelineapp.RenderInput {
	return timelineapp.RenderInput{WorkspaceID: in.WorkspaceID, PayerAccountID: in.PayerAccountID,
		UserID: in.UserID, TimelineID: in.TimelineID, MemberLevelID: in.MemberLevelID, DryRun: dry,
		IdempotencyKey: in.IdempotencyKey}
}

// PrecheckRender 预检 + 报价（与 StartRender 第一步同一个函数）。
func (a AgentTimelines) PrecheckRender(ctx context.Context, in agentapp.RenderRequest) (agentapp.RenderPrecheck, error) {
	pre, _, _, err := a.Svc.PrecheckRender(ctx, a.renderInput(in, true))
	if err != nil {
		return agentapp.RenderPrecheck{}, err
	}
	return renderPrecheck(pre), nil
}

func (a AgentTimelines) StartRender(ctx context.Context, in agentapp.RenderRequest) (agentapp.RenderView, error) {
	job, _, err := a.Svc.StartRender(ctx, a.renderInput(in, in.DryRun))
	if err != nil {
		return agentapp.RenderView{}, translateInsufficient(ctx, err)
	}
	return agentapp.RenderView{ID: job.ID, Status: job.Status, Credits: job.CreditsCost}, nil
}

func renderPrecheck(p timelineapp.RenderPrecheck) agentapp.RenderPrecheck {
	out := agentapp.RenderPrecheck{DurationMs: p.DurationMs, Credits: p.Credits}
	for _, is := range p.Issues {
		out.Issues = append(out.Issues, agentapp.RenderIssue{Code: is.Code, Detail: is.Detail, ClipIdx: is.ClipIdx})
	}
	return out
}

func timelineView(v timelineapp.View, created bool) agentapp.TimelineView {
	out := agentapp.TimelineView{ID: v.Timeline.ID, ScriptID: v.Timeline.ScriptID, Version: v.Timeline.Version,
		Name: v.Timeline.Name, AspectRatio: v.Timeline.AspectRatio, TotalMs: v.TotalMs,
		EffectiveMs: v.EffectiveMs, Created: created}
	for _, tr := range v.Tracks {
		if tr.Track.Kind == timelinedomain.TrackVideo {
			out.ClipCount += len(tr.Clips)
		}
	}
	return out
}

// ---------------------------------------------------------------- 按引用查终态

// AgentRefStatuses 把各域的任务状态与账本接成 agentapp.RefStatuses（扫描循环用）。
//
// ⚠️ 实扣与退回**只认账本**（credit_transaction），不认业务表上的 credits_cost（那是报价）。
type AgentRefStatuses struct {
	Gen      *genrepo.Repo
	Script   *scriptrepo.Repo
	Workflow *workflowrepo.Repo
	Timeline *timelinerepo.Repo
	Billing  *billingrepo.Repo
}

func (a AgentRefStatuses) RefStatus(ctx context.Context, ws int64, ref agentapp.BudgetRef) (agentapp.RefResult, error) {
	switch ref.Type {
	case "script_llm_call", "script_compose":
		return a.calls(ctx, ws, ref)
	case "asset_batch", "script_group":
		return a.generations(ctx, ws, ref.PayerAccountID, idsOf(ref), "")
	case "workflow_run":
		return a.workflowRun(ctx, ws, ref)
	case "render_job":
		return a.render(ctx, ws, ref)
	}
	return agentapp.RefResult{}, fmt.Errorf("不认识的额度引用类型 %q", ref.Type)
}

func idsOf(ref agentapp.BudgetRef) []int64 {
	if len(ref.IDs) > 0 {
		return ref.IDs
	}
	if ref.ID != 0 {
		return []int64{ref.ID}
	}
	return nil
}

// ledgerSums 按引用取账本上每一笔的实扣 / 退回。
func (a AgentRefStatuses) ledgerSums(ctx context.Context, payerID int64, refType string, ids []int64) (
	consumed, refunded map[int64]int, err error) {
	consumed, refunded = map[int64]int{}, map[int64]int{}
	if len(ids) == 0 {
		return consumed, refunded, nil
	}
	refs := make([]billingrepo.TxRef, 0, len(ids))
	for _, id := range ids {
		refs = append(refs, billingrepo.TxRef{Type: refType, ID: strconv.FormatInt(id, 10)})
	}
	rows, err := a.Billing.TransactionsByRefs(ctx, payerID, refs)
	if err != nil {
		return nil, nil, err
	}
	for _, t := range rows {
		id, err := strconv.ParseInt(t.RefID, 10, 64)
		if err != nil {
			continue
		}
		amount := t.Change
		if amount < 0 {
			amount = -amount
		}
		switch t.Type {
		case "consume":
			consumed[id] += amount
		case "refund":
			refunded[id] += amount
		}
	}
	return consumed, refunded, nil
}

// tally 是一批子任务的结局计数。
type tally struct{ ok, failed, cancelled, running int }

func (t tally) status() string {
	switch {
	case t.running > 0:
		return "running"
	case t.failed == 0 && t.cancelled == 0:
		return "success"
	case t.ok > 0:
		return "partial_success"
	case t.failed > 0:
		return "failed"
	}
	return "cancelled"
}

// generations 一批生成任务（资产图批次 / 组这一次提交的子项）。
func (a AgentRefStatuses) generations(ctx context.Context, ws, payerID int64, ids []int64, prefix string) (
	agentapp.RefResult, error) {
	res := agentapp.RefResult{Found: true}
	if len(ids) == 0 {
		// 一项都没受理成功（全部在受理时就失败了，没扣费）。
		res.Terminal, res.Settled, res.Status = true, true, "failed"
		res.Summary = prefix + "没有任何一项受理成功"
		return res, nil
	}
	snaps, err := a.Gen.TaskSnapshots(ctx, ws, ids)
	if err != nil {
		return res, err
	}
	consumed, refunded, err := a.ledgerSums(ctx, payerID, "generation", ids)
	if err != nil {
		return res, err
	}
	byID := map[int64]genrepo.TaskSnapshot{}
	for _, s := range snaps {
		byID[s.ID] = s
	}
	var t tally
	settled := true
	var errs []string
	for _, id := range ids {
		s, ok := byID[id]
		res.Consumed += consumed[id]
		res.Refunded += refunded[id]
		switch {
		case !ok:
			t.failed++
			errs = append(errs, fmt.Sprintf("任务 %d 不存在", id))
		case !gendomain.IsTerminal(s.Status):
			t.running++
		case s.Status == "success":
			t.ok++
		case s.Status == "cancelled":
			t.cancelled++
		default:
			t.failed++
			if msg := strings.TrimSpace(s.ErrorMessage); msg != "" && len(errs) < 5 {
				errs = append(errs, msg)
			}
		}
		// 失败 / 取消且扣过费却还没退：生成域的补退款作业还没跑到，先不结算。
		if ok && gendomain.IsTerminal(s.Status) && s.Status != "success" && consumed[id] > 0 && refunded[id] == 0 {
			settled = false
		}
	}
	res.Status = t.status()
	res.Terminal = t.running == 0
	res.Settled = res.Terminal && settled
	res.Summary = fmt.Sprintf("%s共 %d 项：成功 %d、失败 %d、取消 %d", prefix, len(ids), t.ok, t.failed, t.cancelled)
	if t.running > 0 {
		res.Summary += fmt.Sprintf("、进行中 %d", t.running)
	}
	if len(errs) > 0 {
		res.Error = strings.Join(errs, "；")
		res.Summary += "。失败原因：" + res.Error
	}
	return res, nil
}

// calls 同步文本调用（分镜 / 合成）。调用本身是同步的，通常一进来就已终态。
func (a AgentRefStatuses) calls(ctx context.Context, ws int64, ref agentapp.BudgetRef) (agentapp.RefResult, error) {
	ids := idsOf(ref)
	res := agentapp.RefResult{Found: true}
	if len(ids) == 0 {
		res.Terminal, res.Settled, res.Status, res.Summary = true, true, "success", "没有调用模型（模板模式或全部跳过）"
		return res, nil
	}
	st, err := a.Script.CallStatuses(ctx, ws, ids)
	if err != nil {
		return res, err
	}
	consumed, refunded, err := a.ledgerSums(ctx, ref.PayerAccountID, agentapp.LedgerRefScriptLLM, ids)
	if err != nil {
		return res, err
	}
	var t tally
	settled := true
	for _, id := range ids {
		res.Consumed += consumed[id]
		res.Refunded += refunded[id]
		c, ok := st[id]
		switch {
		case !ok:
			t.failed++
		case c.Status == string(scriptdomain.LLMRunning):
			t.running++
		case c.Status == string(scriptdomain.LLMSuccess):
			t.ok++
		default:
			t.failed++
			if res.Error == "" {
				res.Error = c.ErrorMessage
			}
			if consumed[id] > 0 && refunded[id] == 0 {
				settled = false // 脚本域的恢复扫描（RecoverStaleCalls）会补退款
			}
		}
	}
	res.Status = t.status()
	res.Terminal = t.running == 0
	res.Settled = res.Terminal && settled
	res.Summary = fmt.Sprintf("共 %d 次模型调用：成功 %d、失败 %d", len(ids), t.ok, t.failed)
	return res, nil
}

func (a AgentRefStatuses) workflowRun(ctx context.Context, ws int64, ref agentapp.BudgetRef) (agentapp.RefResult, error) {
	run, err := a.Workflow.GetRun(ctx, ws, ref.ID)
	if errors.Is(err, workflowrepo.ErrNotFound) {
		return agentapp.RefResult{Found: false, Terminal: true, Settled: true, Status: "failed",
			Error: "整图运行不存在"}, nil
	}
	if err != nil {
		return agentapp.RefResult{}, err
	}
	nodes, err := a.Workflow.ListNodes(ctx, run.ID)
	if err != nil {
		return agentapp.RefResult{}, err
	}
	var ids []int64
	var failed []string
	for _, n := range nodes {
		// 复用的节点挂的是上一次的产出，这次没有扣费，不进这一笔。
		if n.GenerationID != 0 && !n.IsReused {
			ids = append(ids, n.GenerationID)
		}
		if n.Status == string(workflowdomain.NodeFailed) || n.Status == string(workflowdomain.NodeBlocked) {
			msg := n.NodeID
			if n.ErrorMessage != "" {
				msg += "：" + n.ErrorMessage
			}
			failed = append(failed, msg)
		}
	}
	res, err := a.generations(ctx, ws, ref.PayerAccountID, ids, "")
	if err != nil {
		return res, err
	}
	switch run.Status {
	case string(workflowdomain.RunSuccess), string(workflowdomain.RunPartialSuccess),
		string(workflowdomain.RunFailed), string(workflowdomain.RunCancelled):
		res.Terminal = true
		res.Status = run.Status
	default:
		// 运行还在推进（下一批节点由 API 的扫描循环派发）：子任务全部终态也不算完。
		res.Terminal, res.Status = false, "running"
	}
	res.Settled = res.Settled && res.Terminal
	res.LedgerIDs = ids
	res.Summary = fmt.Sprintf("整图运行 %s，子任务 %d 个", run.Status, len(ids))
	if len(failed) > 0 {
		if len(failed) > 5 {
			failed = append(failed[:5], "…")
		}
		res.Error = "未完成的节点：" + strings.Join(failed, "；")
		res.Summary += "。" + res.Error
	}
	return res, nil
}

func (a AgentRefStatuses) render(ctx context.Context, ws int64, ref agentapp.BudgetRef) (agentapp.RefResult, error) {
	job, err := a.Timeline.Render(ctx, ws, ref.ID)
	if errors.Is(err, timelinerepo.ErrNotFound) {
		return agentapp.RefResult{Found: false, Terminal: true, Settled: true, Status: "failed",
			Error: "渲染任务不存在"}, nil
	}
	if err != nil {
		return agentapp.RefResult{}, err
	}
	consumed, refunded, err := a.ledgerSums(ctx, ref.PayerAccountID, agentapp.LedgerRefRender, []int64{job.ID})
	if err != nil {
		return agentapp.RefResult{}, err
	}
	res := agentapp.RefResult{Found: true, Status: job.Status, Consumed: consumed[job.ID], Refunded: refunded[job.ID]}
	switch job.Status {
	case timelinedomain.RenderSuccess:
		res.Terminal, res.Settled = true, true
		res.Summary = fmt.Sprintf("成片已渲染完成（媒体 %d）", job.OutputMediaID)
	case timelinedomain.RenderFailed, timelinedomain.RenderCancelled:
		res.Terminal = true
		res.Error = strings.TrimSpace(job.ErrorMessage)
		res.Summary = "渲染" + map[bool]string{true: "已取消", false: "失败"}[job.Status == timelinedomain.RenderCancelled]
		res.Settled = res.Consumed == 0 || res.Refunded > 0
	default:
		res.Status = "running"
	}
	return res, nil
}

// ---------------------------------------------------------------- 装配

// AgentMultiToolsInput 是多段工具装配的入参。
type AgentMultiToolsInput struct {
	Paid AgentPaidToolsInput
	// Script / Workflow / Timeline 用与 HTTP 侧同一组服务（wiring.BuildCore / NewTimelineService）。
	Script     *scriptapp.Service
	ScriptRepo *scriptrepo.Repo
	Workflow   *workflowapp.Service
	Timeline   *timelineapp.Service
	// GenRepo 按节点取最近一次生成（timeline_assemble 不走脚本、按节点拼片段时用）；nil = 不支持按节点拼。
	GenRepo *genrepo.Repo
	// Director 是 3D 导演台的 AI 搭景（BuildCore 装的那一份，DRT-29）。为 nil 时 director_create 照常可用，
	// director_build 回「AI 搭景链路未接入」。
	Director *directorapp.Service
	// Smartcut 是智能剪辑服务（带 AI 剪辑的端口，SCT-41）。为 nil 时三个智能剪辑工具回「未接入」，
	// timeline_assemble 走脚本时照旧只装配（不在画布上建节点），按节点拼回「未接入」。
	Smartcut *smartcutapp.Service
	// SmartcutAIEnabled=false（SEEAITV_SMARTCUT_AI_ENABLED 关）时不装 smartcut_plan：模型看不到它，也就不会去调。
	SmartcutAIEnabled bool
}

// NewAgentMultiTools 装出 script_create / script_storyboard / script_assets / script_compose /
// group_create / group_execute / run_workflow / timeline_assemble / render，以及 3D 导演台的
// director_create / director_build（DRT-29）。
func NewAgentMultiTools(in AgentMultiToolsInput) []agentapp.Tool {
	return newAgentMultiTools(in)
}

var (
	_ agentapp.Scripts     = AgentScripts{}
	_ agentapp.Workflows   = AgentWorkflows{}
	_ agentapp.Timelines   = AgentTimelines{}
	_ agentapp.RefStatuses = AgentRefStatuses{}
)

func newAgentMultiTools(in AgentMultiToolsInput) []agentapp.Tool {
	p := in.Paid
	budget := agentapp.NewBudget(p.AgentRepo, p.NextID, p.Logger)
	if p.Bus != nil {
		budget.SetEvents(AgentEvents{p.Bus})
	}
	var timelines agentapp.Timelines
	if in.Timeline != nil {
		timelines = AgentTimelines{in.Timeline}
	}
	var smartcuts agentapp.Smartcuts
	if in.Smartcut != nil {
		smartcuts = AgentSmartcuts{Svc: in.Smartcut, Timeline: in.Timeline}
	}
	var nodes agentapp.NodeGenerations
	if in.GenRepo != nil {
		nodes = AgentNodeGenerations{Repo: in.GenRepo}
	}
	var workflows agentapp.Workflows
	if in.Workflow != nil {
		workflows = AgentWorkflows{in.Workflow}
	}
	return agenttools.MultiTools(agenttools.MultiDeps{
		Canvas: AgentCanvas{p.Canvas},
		Scripts: AgentScripts{Svc: in.Script, Repo: in.ScriptRepo,
			Quoter: AgentGenQuoter{Catalog: p.Catalog, Canvas: p.Canvas}},
		Workflows:          workflows,
		Timelines:          timelines,
		Nodes:              nodes,
		Directors:          AgentDirectors{Canvas: p.Canvas, Director: in.Director},
		Smartcuts:          smartcuts,
		SmartcutAIDisabled: !in.SmartcutAIEnabled,
		Budget:             budget,
		Logger:             p.Logger,
	})
}
