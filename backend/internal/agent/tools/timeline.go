package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/li904338203/tabtv/backend/internal/agent/app"
	"github.com/li904338203/tabtv/backend/internal/agent/domain"
	"github.com/li904338203/tabtv/backend/internal/canvasgen"
	collab "github.com/li904338203/tabtv/backend/internal/collaboration/domain"
)

// 时间线工具（AGT-50）：timeline_assemble（免费）→ render（付费，export）。
//
// ⚠️ 装配先 DryRun：缺素材 / 失败的镜头**逐条**带镜号与原因回给模型（NEW-09 不静默跳过），
// 默认不带着缺口装进去 —— 少了两镜的成片看起来是完整的，用户要逐镜比对才发现。
// ⚠️ 渲染：预检（免费）→ 报价 → PaidCall(StartRender, IdempotencyKey=agent:…)；预检有问题一分钱不扣，
// 逐条指出是第几段。同一步骤重入回那一次渲染任务（额度条目挂着它），不建第二个。

// ---------------------------------------------------------------- timeline_assemble

// TimelineAssemble 是 timeline_assemble。
//
// ⚠️ 装配出来的时间线要**在画布上看得到**（SCK-19，SCD-39）：建 / 复用一个智能剪辑节点承载它 ——
// 原先只建时间线不碰画布，用户在界面上找不到它，只能凭 timelineId 去渲染。
//   - 走脚本：时间线已经绑在节点上就复用；没绑就新建智能剪辑节点、把时间线绑上去，并连上画布上的脚本节点；
//   - 不走脚本：新建智能剪辑节点（给了 timelineId 时复用它所在的节点），把这些节点按顺序连进来，按连线即入轨铺好。
type TimelineAssemble struct{ d MultiDeps }

func (TimelineAssemble) Name() domain.ToolName { return domain.ToolTimelineAssemble }
func (TimelineAssemble) Paid() bool            { return false }
func (TimelineAssemble) Action() collab.Action { return collab.ActionEdit }

func (TimelineAssemble) Description() string {
	return "把视频装配到时间线（免费），之后用 render 出成片。装配出来的时间线挂在画布上的一个智能剪辑节点上（返回 nodeId），" +
		"用户可以打开它继续剪。两种用法：" +
		"① 走脚本：给 scriptId，按分镜把视频组的成片装进该脚本的时间线（没有就新建，并在画布上建智能剪辑节点、连上脚本节点）；" +
		"② 不走脚本：给 nodeIds（按播放顺序列出 generate 产出的视频节点），新建一个智能剪辑节点（给 timelineId 时用它所在的节点），" +
		"把这些节点按顺序连进来、铺上时间线（已经连着的不重复连）。" +
		"有缺口（缺素材 / 失败的镜头 / 还没出片的节点）时默认不装配，逐条返回原因；确认接受缺口时传 allowGaps=true。只补缺口，不动已有片段。" +
		"不要用 canvas_apply 画时间线节点，也不要用 generate 渲染。"
}

func (TimelineAssemble) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{` +
		`"scriptId":{"type":"string"},"timelineId":{"type":"string"},` +
		`"nodeIds":{"type":"array","items":{"type":"string"},"description":"不走脚本时：按播放顺序列出视频节点 ID（generate 返回的 nodeId）"},` +
		`"name":{"type":"string"},"aspectRatio":{"type":"string","description":"新建时的画幅，如 16:9 / 9:16"},` +
		`"force":{"type":"boolean","description":"重建自动装配出来的片段（手工加的不动）"},` +
		`"allowGaps":{"type":"boolean","description":"接受缺口照样装配"}}}`)
}

type assembleArgs struct {
	ScriptID    flexID   `json:"scriptId"`
	TimelineID  flexID   `json:"timelineId"`
	NodeIDs     []string `json:"nodeIds"`
	Name        string   `json:"name"`
	AspectRatio string   `json:"aspectRatio"`
	Force       bool     `json:"force"`
	AllowGaps   bool     `json:"allowGaps"`
}

func (t TimelineAssemble) Run(ctx context.Context, rc app.RunContext, raw json.RawMessage) (app.ToolResult, error) {
	if err := requireExecuting(rc, domain.ToolTimelineAssemble); err != nil {
		return app.ToolResult{}, err
	}
	if t.d.Timelines == nil {
		return app.ToolResult{}, errors.New("时间线服务未接入")
	}
	var a assembleArgs
	if err := multiDecode(raw, &a); err != nil {
		return app.ToolResult{}, err
	}
	if len(a.NodeIDs) > 0 {
		if a.ScriptID != 0 {
			return app.ToolResult{}, errors.New("scriptId 与 nodeIds 只能给一个：走脚本按分镜装配，不走脚本按节点拼")
		}
		return t.appendNodes(ctx, rc, a)
	}
	if a.ScriptID == 0 && a.TimelineID == 0 {
		return app.ToolResult{}, errors.New("scriptId、timelineId、nodeIds 至少给一个（不走脚本时用 nodeIds 按顺序列出视频节点）")
	}
	tl, err := t.d.Timelines.Ensure(ctx, app.TimelineEnsureRequest{WorkspaceID: rc.WorkspaceID,
		ProjectID: rc.ProjectID, ScriptID: int64(a.ScriptID), TimelineID: int64(a.TimelineID),
		Name: strings.TrimSpace(a.Name), AspectRatio: strings.TrimSpace(a.AspectRatio)})
	if err != nil {
		return app.ToolResult{}, fmt.Errorf("取 / 建时间线失败：%v", err)
	}
	// 先挂到画布上再装配：有缺口不装配时，时间线也已经建好了，同样要让用户看得到。
	nodeID, err := t.scriptNode(ctx, rc, tl, int64(a.ScriptID))
	if err != nil {
		return app.ToolResult{}, err
	}
	req := app.AssembleRequest{WorkspaceID: rc.WorkspaceID, TimelineID: tl.ID, Version: tl.Version,
		Force: a.Force, DryRun: true, UserID: rc.UserID, EditorKind: domain.EditorAgent}
	plan, err := t.d.Timelines.Assemble(ctx, req)
	if err != nil {
		return app.ToolResult{}, fmt.Errorf("装配预览失败：%v", err)
	}
	if len(plan.Gaps) > 0 && !a.AllowGaps {
		// 缺口在第一句、逐条给镜号与原因；不装配（不改用户的时间线）。
		content := withNodeID(map[string]any{"timelineId": idS(tl.ID), "assembled": false, "gaps": plan.Gaps,
			"wouldAppend": plan.Append, "message": gapMessage(plan.Gaps) +
				"。先把这几镜补出来（group_execute 重试失败项），或确认接受缺口后传 allowGaps=true"}, nodeID)
		return app.ToolResult{Content: ptJSON(content), Display: ptJSON(withNodeID(map[string]any{
			"timelineId": idS(tl.ID), "gaps": len(plan.Gaps)}, nodeID)), Outputs: 1}, nil
	}
	req.DryRun = false
	done, err := t.d.Timelines.Assemble(ctx, req)
	if err != nil {
		return app.ToolResult{}, fmt.Errorf("装配失败：%v", err)
	}
	content := withNodeID(map[string]any{"timelineId": idS(tl.ID), "assembled": true, "summary": done.Summary,
		"appended": done.Append, "version": done.Timeline.Version, "clipCount": done.Timeline.ClipCount,
		"totalMs": done.Timeline.TotalMs, "effectiveMs": done.Timeline.EffectiveMs,
		"next": "用 render 渲染成片；用户可以在画布上打开这个智能剪辑节点继续剪"}, nodeID)
	if len(done.Gaps) > 0 {
		content["gaps"] = done.Gaps
		content["summary"] = gapMessage(done.Gaps) + "；" + done.Summary
	}
	return app.ToolResult{Content: ptJSON(content), Display: ptJSON(withNodeID(map[string]any{"timelineId": idS(tl.ID),
		"appended": done.Append, "gaps": len(done.Gaps)}, nodeID)), Outputs: 1}, nil
}

// withNodeID 结果里多一个 nodeId（承载时间线的智能剪辑节点；没接智能剪辑时不给）。原有字段一个不改。
func withNodeID(m map[string]any, nodeID string) map[string]any {
	if nodeID != "" {
		m["nodeId"] = nodeID
	}
	return m
}

// scriptNode 走脚本时承载时间线的智能剪辑节点（SCD-39）：已经绑在节点上就复用；没绑就新建节点、把时间线绑上去，
// 再连上画布上的脚本节点（连不上只告警：装配照样能做，节点已经在画布上了）。智能剪辑没接入时返回空串（照旧只装配）。
func (t TimelineAssemble) scriptNode(ctx context.Context, rc app.RunContext, tl app.TimelineView,
	scriptID int64) (string, error) {
	if t.d.Smartcuts == nil || t.d.Canvas == nil {
		return "", nil
	}
	owner, err := t.d.Smartcuts.TimelineNode(ctx, rc.WorkspaceID, rc.ProjectID, tl.ID)
	if err == nil {
		return owner, nil
	}
	if !errors.Is(err, app.ErrSmartcutTimelineNotFound) {
		return "", fmt.Errorf("查时间线挂在哪个智能剪辑节点上失败：%v", err)
	}
	o := objectOrch(t.d.Canvas)
	o.Logger = t.d.logger()
	c := multiCaller(rc)
	nodeID, err := t.bindNewNode(ctx, o, c, tl)
	if err != nil {
		return "", err
	}
	if scriptID != 0 {
		if v, err := o.Canvas.Read(ctx, rc.WorkspaceID, rc.ProjectID); err == nil {
			if src := canvasgen.ScriptNodeOf(v, scriptID); src != "" {
				if _, err := canvasgen.ConnectSmartcutInputs(ctx, o, c, nodeID, []string{src}); err != nil {
					t.d.logger().Warn("脚本节点没能连进智能剪辑（装配照常）", "session", rc.SessionID, "node", nodeID,
						"script", src, "err", err)
				}
			}
		}
	}
	return nodeID, nil
}

// bindNewNode 为一条还没绑节点的时间线新建智能剪辑节点并绑上。
func (t TimelineAssemble) bindNewNode(ctx context.Context, o canvasgen.Orchestrator, c canvasgen.Caller,
	tl app.TimelineView) (string, error) {
	nodeID, err := canvasgen.BindSmartcutNode(ctx, o, c, canvasgen.SmartcutNodeInput{Title: tl.Name}, tl.ID,
		func(ctx context.Context, nodeID string) error {
			return t.d.Smartcuts.BindTimeline(ctx, c.WorkspaceID, c.ProjectID, tl.ID, nodeID)
		})
	var oe *canvasgen.ObjectError
	if errors.As(err, &oe) {
		return "", fmt.Errorf("智能剪辑节点 %s 已建好，但没能把时间线 %s 挂上去：%v。不要再建节点：把时间线 ID 告诉用户",
			oe.NodeID, idS(tl.ID), oe.Err)
	}
	if err != nil {
		return "", ensureNodeError(err)
	}
	return nodeID, nil
}

// appendNodes 是不走脚本的装配：建（或复用）智能剪辑节点，把这些节点按顺序连进来，按连线即入轨铺上时间线（SC-F-164）。
//
// ⚠️ 有节点还没出片（失败 / 还在跑 / 不是视频）时默认不装配，逐个说出是哪个节点、为什么 ——
// 少一段的成片看起来是完整的，用户要逐段比对才发现（同按脚本装配的缺口规则，NEW-09）。接受缺口时只连能用的。
// ⚠️ 连线即入轨只在还没剪过时铺（SC-F-13 / 14）：时间线已经剪过的话新连的素材只进素材条，结果里说清楚。
func (t TimelineAssemble) appendNodes(ctx context.Context, rc app.RunContext, a assembleArgs) (app.ToolResult, error) {
	if t.d.Nodes == nil || t.d.Smartcuts == nil || t.d.Canvas == nil {
		return app.ToolResult{}, errors.New("按节点拼时间线未接入，请走脚本（scriptId）装配")
	}
	latest, err := t.d.Nodes.LatestByNode(ctx, rc.WorkspaceID, rc.ProjectID)
	if err != nil {
		return app.ToolResult{}, fmt.Errorf("读取节点的生成结果失败：%v", err)
	}
	var ready []string
	var gaps []string
	seen := map[string]bool{}
	for i, raw := range a.NodeIDs {
		id := strings.TrimSpace(raw)
		if id == "" || seen[id] {
			return app.ToolResult{}, fmt.Errorf("nodeIds 第 %d 个为空或重复", i+1)
		}
		seen[id] = true
		g, ok := latest[id]
		switch {
		case !ok:
			gaps = append(gaps, fmt.Sprintf("第 %d 段（节点 %s）：还没有生成过", i+1, id))
		case g.Status != "success":
			gaps = append(gaps, fmt.Sprintf("第 %d 段（节点 %s）：最近一次生成%s", i+1, id, statusLabel(g.Status, g.ErrorCode)))
		case g.OutputType != "video" || g.OutputMediaID == 0:
			gaps = append(gaps, fmt.Sprintf("第 %d 段（节点 %s）：产出不是视频，不能放进时间线", i+1, id))
		default:
			ready = append(ready, id)
		}
	}
	if len(gaps) > 0 && !a.AllowGaps {
		msg := fmt.Sprintf("有 %d 段还不能装配（%s）。先把这几段补出来，或确认接受缺口后传 allowGaps=true",
			len(gaps), strings.Join(gaps, "；"))
		return app.ToolResult{Content: ptJSON(map[string]any{"assembled": false, "gaps": gaps, "message": msg}),
			Display: ptJSON(map[string]any{"gaps": len(gaps)}), Outputs: 1}, nil
	}
	if len(ready) == 0 {
		return app.ToolResult{}, errors.New("没有可以装配的视频")
	}
	o := objectOrch(t.d.Canvas)
	o.Logger = t.d.logger()
	c := multiCaller(rc)
	nodeID, timelineID, err := t.nodeForAppend(ctx, rc, o, c, a)
	if err != nil {
		return app.ToolResult{}, err
	}
	conn, err := canvasgen.ConnectSmartcutInputs(ctx, o, c, nodeID, ready)
	if err != nil {
		return app.ToolResult{}, fmt.Errorf("把视频节点连进智能剪辑 %s 失败：%v", nodeID, smartcutCanvasError(err))
	}
	sync, err := t.d.Smartcuts.Sync(ctx, rc.WorkspaceID, rc.ProjectID, timelineID)
	if err != nil {
		return app.ToolResult{}, fmt.Errorf("视频节点已连进智能剪辑 %s，但铺上时间线失败（用户打开剪辑台时会再铺一次）：%v", nodeID, err)
	}
	sum := sync.Summary
	content := map[string]any{"timelineId": idS(timelineID), "nodeId": nodeID, "assembled": true,
		"appended": sync.Appended, "skipped": len(conn.Existing), "version": sum.Version, "clipCount": sum.ClipCount,
		"totalMs": sum.TotalMs, "effectiveMs": sum.EffectiveMs,
		"next": "用 render（给 timelineId）渲染成片；用户可以在画布上打开这个智能剪辑节点继续剪"}
	if len(gaps) > 0 {
		content["gaps"] = gaps
	}
	if sync.Appended == 0 && len(conn.Added) > 0 && sum.IsArranged {
		content["message"] = "这条时间线已经剪过：新连进来的视频只出现在素材条里，没有自动铺上时间线。" +
			"用 smartcut_plan 按要求重新剪，或请用户在剪辑台里把它们拖进来"
	}
	return app.ToolResult{Content: ptJSON(content), Display: ptJSON(map[string]any{"timelineId": idS(timelineID),
		"nodeId": nodeID, "appended": sync.Appended, "gaps": len(gaps)}), Outputs: 1}, nil
}

// nodeForAppend 按节点拼时承载时间线的智能剪辑节点：给了 timelineId 时用它所在的节点（没绑节点的旧时间线新建节点绑上）；
// 没给就新建节点与时间线（给了画幅时先把新时间线定成那个画幅，再铺素材）。
func (t TimelineAssemble) nodeForAppend(ctx context.Context, rc app.RunContext, o canvasgen.Orchestrator,
	c canvasgen.Caller, a assembleArgs) (string, int64, error) {
	if a.TimelineID != 0 {
		tid := int64(a.TimelineID)
		owner, err := t.d.Smartcuts.TimelineNode(ctx, rc.WorkspaceID, rc.ProjectID, tid)
		if err == nil {
			return owner, tid, nil
		}
		if !errors.Is(err, app.ErrSmartcutTimelineNotFound) {
			return "", 0, fmt.Errorf("查时间线挂在哪个智能剪辑节点上失败：%v", err)
		}
		tl, err := t.d.Timelines.Ensure(ctx, app.TimelineEnsureRequest{WorkspaceID: rc.WorkspaceID,
			ProjectID: rc.ProjectID, TimelineID: tid})
		if err != nil {
			return "", 0, fmt.Errorf("取时间线失败：%v", err)
		}
		nodeID, err := t.bindNewNode(ctx, o, c, tl)
		return nodeID, tid, err
	}
	nodeID, timelineID, err := canvasgen.CreateSmartcutNode(ctx, o, c,
		canvasgen.SmartcutNodeInput{Title: strings.TrimSpace(a.Name)}, genSmartcutTimelines{t.d.Smartcuts})
	var oe *canvasgen.ObjectError
	if errors.As(err, &oe) {
		return "", 0, fmt.Errorf("智能剪辑节点 %s 已建好，但建时间线失败：%v。不要再建一个：对它调用 smartcut_plan 时会先补建",
			oe.NodeID, oe.Err)
	}
	if err != nil {
		return "", 0, ensureNodeError(err)
	}
	if aspect := strings.TrimSpace(a.AspectRatio); aspect != "" {
		if err := t.d.Smartcuts.SetAspectRatio(ctx, rc.WorkspaceID, rc.ProjectID, timelineID, aspect); err != nil {
			return "", 0, fmt.Errorf("智能剪辑节点 %s 已建好，但没能把画幅定成 %s：%v", nodeID, aspect, err)
		}
	}
	return nodeID, timelineID, nil
}

func statusLabel(status, code string) string {
	switch status {
	case "queued", "running":
		return "还没结束（" + status + "）"
	case "failed":
		if code != "" {
			return "失败了（" + code + "）"
		}
		return "失败了"
	}
	return "状态为 " + status
}

func gapMessage(gaps []app.AssembleGap) string {
	parts := make([]string, 0, len(gaps))
	for _, g := range gaps {
		parts = append(parts, fmt.Sprintf("第 %d 镜：%s", g.ShotNo, g.Reason))
	}
	return fmt.Sprintf("有 %d 个镜头缺素材（%s）", len(gaps), strings.Join(parts, "；"))
}

// ---------------------------------------------------------------- render

// Render 是 render：渲染成片（付费，export 权限）。
type Render struct{ d MultiDeps }

func (Render) Name() domain.ToolName { return domain.ToolRender }
func (Render) Paid() bool            { return true }
func (Render) Action() collab.Action { return collab.ActionExport }

func (Render) Description() string {
	return "把时间线渲染成成片（按秒扣费）。先免费预检，有问题时不扣费并逐条指出是第几段。" +
		"wait=true（默认）提交后暂停等渲染结束。"
}

func (Render) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","required":["timelineId"],"properties":{` +
		`"timelineId":{"type":"string"},"wait":{"type":"boolean","default":true}}}`)
}

type renderArgs struct {
	TimelineID flexID `json:"timelineId"`
	Wait       *bool  `json:"wait"`
}

func (t Render) Run(ctx context.Context, rc app.RunContext, raw json.RawMessage) (app.ToolResult, error) {
	if err := requireExecuting(rc, domain.ToolRender); err != nil {
		return app.ToolResult{}, err
	}
	if t.d.Timelines == nil || t.d.Budget == nil {
		return app.ToolResult{}, errors.New("时间线服务未接入")
	}
	var a renderArgs
	if err := multiDecode(raw, &a); err != nil {
		return app.ToolResult{}, err
	}
	if a.TimelineID == 0 {
		return app.ToolResult{}, errors.New("timelineId 必填")
	}
	req := app.RenderRequest{WorkspaceID: rc.WorkspaceID, PayerAccountID: rc.PayerAccountID, UserID: rc.UserID,
		TimelineID: int64(a.TimelineID), MemberLevelID: rc.MemberLevelID}
	pre, err := t.d.Timelines.PrecheckRender(ctx, req)
	if err != nil {
		return app.ToolResult{}, fmt.Errorf("渲染预检失败：%v", err)
	}
	if len(pre.Issues) > 0 {
		content := map[string]any{"timelineId": idS(req.TimelineID), "renderable": false, "issues": pre.Issues,
			"message": "预检没通过，没有扣费。按 issues 逐条修正后再渲染"}
		return app.ToolResult{Content: ptJSON(content), Display: ptJSON(map[string]any{
			"timelineId": idS(req.TimelineID), "issues": len(pre.Issues)}), Outputs: 1}, nil
	}
	var job app.RenderView
	res, susp, err := t.d.Budget.PaidCallTool(ctx, rc, string(domain.ToolRender), pre.Credits,
		func(key string) (app.PaidResult, error) {
			r := req
			r.IdempotencyKey = key
			v, err := t.d.Timelines.StartRender(ctx, r)
			if err != nil {
				return app.PaidResult{}, err
			}
			job = v
			return app.PaidResult{Credits: v.Credits, RefType: "render_job", RefID: v.ID}, nil
		})
	if susp != nil {
		return overBudgetResult(susp, pre.Credits, map[string]any{"timelineId": idS(req.TimelineID)}), nil
	}
	if err != nil {
		return app.ToolResult{}, paidError("渲染", pre.Credits, err)
	}
	if res.Replayed {
		job = app.RenderView{ID: res.RefID, Status: "queued", Credits: res.Credits}
	}
	wait := wantWait(a.Wait)
	content := map[string]any{"renderId": idS(job.ID), "timelineId": idS(req.TimelineID), "status": job.Status,
		"credits": job.Credits, "durationMs": pre.DurationMs, "waiting": wait}
	if res.Replayed {
		content["replayed"] = true
	}
	out := app.ToolResult{Content: ptJSON(content), Display: ptJSON(map[string]any{"renderId": idS(job.ID),
		"credits": job.Credits}), Outputs: 1, WaitRef: true}
	if wait {
		out.Suspend = waitSuspension("等待成片渲染完成")
	}
	return out, nil
}

var (
	_ app.Tool = TimelineAssemble{}
	_ app.Tool = Render{}
)
