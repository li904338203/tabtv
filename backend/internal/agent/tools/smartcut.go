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
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// 智能剪辑工具（docs/agent/smartcut SCT-41，SC-F-160~162）：smartcut_create 建节点（可同时把素材节点按顺序连进来，
// 连上即铺上时间线）、smartcut_get 只读详情、smartcut_plan 免费的 AI 剪辑（结果直接写入并存快照，来源 agent）。
//
// ⚠️ 三个都**免费**（SCK-2），不走 PaidCall；导出沿用 render（导出也免费，0 积分时不弹追加额度卡）。
// ⚠️ 按**节点**取时间线，不只信 params.timelineId（canvasgen.SmartcutTimelineOfNode / SmartcutBoundTimeline）：
// 复制出来还没打开过的节点带的是原节点的时间线，拿它剪会把内容写进原节点 —— 改一个、另一个跟着变。
// ⚠️ AI 剪辑的业务错误（限流、素材不合适、没有可用模型、写入冲突）原样交给模型转述，见 smartcutError。

// smartcutModes 剪辑方式（与网页同一组，smartcut/app 的 planModes）。
var smartcutModes = map[string]string{"mix": "素材混剪", "board": "分镜成片", "talk": "口播精剪", "narr": "解说成片"}

// ---------------------------------------------------------------- smartcut_create

// SmartcutCreate 是 smartcut_create：建智能剪辑节点与时间线，可同时把素材节点按顺序连进来并铺上时间线。
type SmartcutCreate struct{ d MultiDeps }

func (SmartcutCreate) Name() domain.ToolName { return domain.ToolSmartcutCreate }
func (SmartcutCreate) Paid() bool            { return false }
func (SmartcutCreate) Action() collab.Action { return collab.ActionEdit }

func (t SmartcutCreate) Description() string {
	next := "之后可以用 smartcut_plan 让 AI 按要求剪，用 smartcut_get 查看，用 render 导出成片（免费）。"
	if t.d.SmartcutAIDisabled {
		next = "之后请用户在剪辑台里手动剪（AI 剪辑暂未开放），用 smartcut_get 查看，用 render 导出成片（免费）。"
	}
	return "在画布上新建一个智能剪辑节点并建好它的时间线（免费）。可以同时给 inputNodeIds（按播放顺序列出视频 / 图片 / 音频 / " +
		"文本 / 脚本 / 视频组节点），它们会按顺序连进来，并按「连线即入轨」铺上时间线（视频与图片进画面轨，音频进音频轨）。" +
		"返回 nodeId 与 timelineId；" + next
}

func (SmartcutCreate) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{` +
		`"title":{"type":"string","description":"节点标题（默认「智能剪辑」）"},` +
		`"position":{"type":"object","properties":{"x":{"type":"number"},"y":{"type":"number"}}},` +
		`"inputNodeIds":{"type":"array","items":{"type":"string"},"description":"要连进来的素材节点 ID，按播放顺序（read_canvas 里的节点）"}}}`)
}

type smartcutCreateArgs struct {
	Title        string    `json:"title"`
	Position     *position `json:"position"`
	InputNodeIDs []string  `json:"inputNodeIds"`
}

func (t SmartcutCreate) Run(ctx context.Context, rc app.RunContext, raw json.RawMessage) (app.ToolResult, error) {
	if err := requireExecuting(rc, domain.ToolSmartcutCreate); err != nil {
		return app.ToolResult{}, err
	}
	if t.d.Canvas == nil || t.d.Smartcuts == nil {
		return app.ToolResult{}, errors.New("智能剪辑服务未接入")
	}
	var a smartcutCreateArgs
	if err := multiDecode(raw, &a); err != nil {
		return app.ToolResult{}, err
	}
	inputs := trimIDs(a.InputNodeIDs)
	o := objectOrch(t.d.Canvas)
	o.Logger = t.d.logger()
	c := multiCaller(rc)
	// 先校验要连的节点再建节点：给错一个 ID 就留下一个空节点的话，模型重试时会再建一个。
	if len(inputs) > 0 {
		v, err := o.Canvas.Read(ctx, rc.WorkspaceID, rc.ProjectID)
		if err != nil {
			return app.ToolResult{}, fmt.Errorf("读取画布失败：%v", err)
		}
		if err := canvasgen.CheckSmartcutInputs(v, "", inputs); err != nil {
			return app.ToolResult{}, fmt.Errorf("%v。先用 read_canvas 确认节点 ID 与类型，再调用一次", err)
		}
	}
	var pos *canvasgen.Position
	if a.Position != nil {
		pos = &canvasgen.Position{X: a.Position.X, Y: a.Position.Y}
	}
	nodeID, timelineID, err := canvasgen.CreateSmartcutNode(ctx, o, c,
		canvasgen.SmartcutNodeInput{Title: strings.TrimSpace(a.Title), Position: pos}, genSmartcutTimelines{t.d.Smartcuts})
	var oe *canvasgen.ObjectError
	if errors.As(err, &oe) {
		return app.ToolResult{}, fmt.Errorf("智能剪辑节点 %s 已建好，但建时间线失败：%v。不要再建一个：用户打开剪辑台时会自动补上",
			oe.NodeID, oe.Err)
	}
	if err != nil {
		return app.ToolResult{}, ensureNodeError(err)
	}
	next := "用 smartcut_plan 让 AI 按要求剪（免费）；用 smartcut_get 查看片段与素材条；用 render（给 timelineId）导出成片"
	if t.d.SmartcutAIDisabled {
		next = "素材已按顺序铺上时间线；AI 剪辑暂未开放，请用户在剪辑台里手动剪。用 smartcut_get 查看片段与素材条；用 render（给 timelineId）导出成片"
	}
	content := map[string]any{"nodeId": nodeID, "timelineId": idS(timelineID), "next": next}
	display := map[string]any{"nodeId": nodeID, "timelineId": idS(timelineID)}
	if len(inputs) > 0 {
		conn, err := canvasgen.ConnectSmartcutInputs(ctx, o, c, nodeID, inputs)
		if err != nil {
			return app.ToolResult{}, fmt.Errorf("智能剪辑节点 %s 已建好（时间线 %s），但把素材节点连进来失败：%v。不要再建一个："+
				"用 canvas_apply 给它补连线（连上即铺上时间线）", nodeID, idS(timelineID), smartcutCanvasError(err))
		}
		content["connected"], display["connected"] = conn.Added, len(conn.Added)
		sync, err := t.d.Smartcuts.Sync(ctx, rc.WorkspaceID, rc.ProjectID, timelineID)
		if err != nil {
			// 连线已经在画布上了：铺轨失败的话，用户打开剪辑台时会再同步一次，不能让模型因此再建一个节点。
			t.d.logger().Warn("智能剪辑的素材已连上，但铺上时间线失败", "session", rc.SessionID, "node", nodeID, "err", err)
			content["syncError"] = "素材已连上，但铺上时间线失败（用户打开剪辑台时会再铺一次）：" + err.Error()
		} else {
			content["appended"], content["unused"] = sync.Appended, sync.Unused
			content["clipCount"], content["effectiveMs"] = sync.Summary.ClipCount, sync.Summary.EffectiveMs
			display["appended"] = sync.Appended
		}
	}
	return app.ToolResult{Content: ptJSON(content), Display: ptJSON(display), Outputs: 1}, nil
}

// ---------------------------------------------------------------- smartcut_get

// SmartcutGet 是 smartcut_get：只读详情（摘要、素材条、片段清单、导出状态）。不建、不写任何东西。
type SmartcutGet struct{ d MultiDeps }

func (SmartcutGet) Name() domain.ToolName { return domain.ToolSmartcutGet }
func (SmartcutGet) Paid() bool            { return false }
func (SmartcutGet) Action() collab.Action { return collab.ActionView }

func (SmartcutGet) Description() string {
	return "查看智能剪辑节点的时间线（只读）：总时长、片段清单（哪条轨第几段、来源节点、成片起点与时长）、素材条" +
		"（连进来的素材是否就绪、是否已用上）、素材有没有变化、最近一次导出的状态。导出成片用 render（给这里返回的 timelineId）。"
}

func (SmartcutGet) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","required":["nodeId"],"properties":{` +
		`"nodeId":{"type":"string","description":"智能剪辑节点 ID（smartcut_create 返回的 nodeId，或 read_canvas 里 type 为 smartcut 的节点）"}}}`)
}

type smartcutNodeArgs struct {
	NodeID string `json:"nodeId"`
}

func (t SmartcutGet) Run(ctx context.Context, rc app.RunContext, raw json.RawMessage) (app.ToolResult, error) {
	if err := requireExecuting(rc, domain.ToolSmartcutGet); err != nil {
		return app.ToolResult{}, err
	}
	if t.d.Canvas == nil || t.d.Smartcuts == nil {
		return app.ToolResult{}, errors.New("智能剪辑服务未接入")
	}
	var a smartcutNodeArgs
	if err := multiDecode(raw, &a); err != nil {
		return app.ToolResult{}, err
	}
	a.NodeID = strings.TrimSpace(a.NodeID)
	if a.NodeID == "" {
		return app.ToolResult{}, errors.New("nodeId 必填：要查看的智能剪辑节点")
	}
	o := objectOrch(t.d.Canvas)
	timelineID, ok, err := canvasgen.SmartcutBoundTimeline(ctx, o, multiCaller(rc), a.NodeID,
		genSmartcutTimelines{t.d.Smartcuts})
	if err != nil {
		return app.ToolResult{}, smartcutNodeError(a.NodeID, err)
	}
	if !ok {
		content := map[string]any{"nodeId": a.NodeID, "hasTimeline": false,
			"message": smartcutNoTimelineMessage(!t.d.SmartcutAIDisabled)}
		return app.ToolResult{Content: ptJSON(content), Display: ptJSON(map[string]any{"nodeId": a.NodeID,
			"hasTimeline": false}), Outputs: 1}, nil
	}
	d, err := t.d.Smartcuts.Detail(ctx, rc.WorkspaceID, rc.ProjectID, timelineID)
	if err != nil {
		return app.ToolResult{}, fmt.Errorf("读取智能剪辑失败：%v", err)
	}
	content := smartcutDetailContent(a.NodeID, d, !t.d.SmartcutAIDisabled)
	display := map[string]any{"nodeId": a.NodeID, "timelineId": idS(timelineID), "clipCount": d.ClipCount,
		"effectiveMs": d.EffectiveMs}
	return app.ToolResult{Content: ptJSON(content), Display: ptJSON(display), Outputs: 1}, nil
}

func smartcutDetailContent(nodeID string, d app.SmartcutDetail, aiEnabled bool) map[string]any {
	inputs := make([]map[string]any, 0, len(d.Inputs))
	for _, it := range d.Inputs {
		m := map[string]any{"no": it.No, "kind": it.Kind, "status": it.Status, "sourceNodeId": it.SourceNodeID,
			"used": it.Used}
		if it.Title != "" {
			m["title"] = it.Title
		}
		if it.Reason != "" {
			m["reason"] = it.Reason
		}
		if it.DurationMs > 0 {
			m["durationMs"] = it.DurationMs
		}
		inputs = append(inputs, m)
	}
	clips := make([]map[string]any, 0, len(d.Clips))
	for _, c := range d.Clips {
		m := map[string]any{"track": c.TrackKind, "no": c.No, "mediaId": idS(c.MediaID), "startMs": c.StartMs,
			"durationMs": c.DurationMs}
		if c.TrackName != "" {
			m["trackName"] = c.TrackName
		}
		if c.TrackRole != "" {
			m["role"] = c.TrackRole
		}
		if c.SourceNodeID != "" {
			m["sourceNodeId"] = c.SourceNodeID
		}
		if c.Origin != "" {
			m["origin"] = c.Origin
		}
		if c.Hidden {
			m["hidden"] = true
		}
		clips = append(clips, m)
	}
	out := map[string]any{"nodeId": nodeID, "timelineId": idS(d.TimelineID), "hasTimeline": true,
		"version": d.Version, "isArranged": d.IsArranged, "clipCount": d.ClipCount, "totalMs": d.TotalMs,
		"effectiveMs": d.EffectiveMs, "inputsStale": d.InputsStale, "renderStale": d.RenderStale,
		"inputs": inputs, "clips": clips}
	if d.Omitted > 0 {
		out["inputsOmitted"] = d.Omitted
	}
	if r := d.LatestRender; r != nil {
		lr := map[string]any{"id": idS(r.ID), "status": r.Status, "progress": r.Progress}
		if r.Stage != "" {
			lr["stage"] = r.Stage
		}
		if r.OutputMediaID != 0 {
			lr["outputMediaId"] = idS(r.OutputMediaID)
		}
		if r.Error != "" {
			lr["error"] = r.Error
		}
		out["latestRender"] = lr
	}
	if c := d.ActiveCall; c != nil {
		out["activeCall"] = map[string]any{"id": idS(c.ID), "kind": c.Kind, "stage": c.Stage}
	}
	switch {
	case d.ClipCount == 0:
		out["next"] = "时间线还是空的：把素材节点连进来（smartcut_create 的 inputNodeIds，或 canvas_apply 连线）"
		if aiEnabled {
			out["next"] = out["next"].(string) + "，或用 smartcut_plan 让 AI 剪"
		}
	case d.LatestRender == nil || d.RenderStale:
		out["next"] = "需要成片时用 render（给 timelineId）导出（免费）"
	}
	return out
}

// ---------------------------------------------------------------- smartcut_plan

// SmartcutPlan 是 smartcut_plan：按要求让 AI 剪辑并直接写入时间线（写入前存快照，用户可以在剪辑台里撤销这轮 AI）。
type SmartcutPlan struct{ d MultiDeps }

func (SmartcutPlan) Name() domain.ToolName { return domain.ToolSmartcutPlan }
func (SmartcutPlan) Paid() bool            { return false }
func (SmartcutPlan) Action() collab.Action { return collab.ActionEdit }

func (SmartcutPlan) Description() string {
	return "让 AI 按要求剪辑智能剪辑节点里的素材（免费，但按用户限流：同时只能跑一个、每小时有次数上限，与网页共用）。" +
		"mode：mix 素材混剪 / board 分镜成片（连着脚本或视频组）/ talk 口播精剪（要有人声）/ narr 解说成片。" +
		"结果直接写入时间线，写入前存快照（用户在剪辑台里能「撤销这轮 AI」）；会覆盖当前剪辑，对用户建的节点先征得同意。" +
		"素材不合适、被限流或剪辑期间用户改过时，结果里有原因（限流还有 retry_after_seconds），原样转告用户，不要立刻重试。"
}

func (SmartcutPlan) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","required":["nodeId","mode"],"properties":{` +
		`"nodeId":{"type":"string","description":"智能剪辑节点 ID"},` +
		`"mode":{"type":"string","enum":["mix","board","talk","narr"],"description":"剪辑方式"},` +
		`"instruction":{"type":"string","description":"剪辑要求，例如：剪一条 15 秒的新品卡点视频，开头放产品特写"},` +
		`"targetSec":{"type":"integer","minimum":0,"description":"目标时长（秒）；0 或不给 = 按素材与要求自动"},` +
		`"aspectRatio":{"type":"string","description":"画幅：16:9 / 9:16 / 1:1；不给 = 保持时间线当前的画幅"},` +
		`"variants":{"type":"integer","minimum":1,"maximum":3,"description":"方案数（默认 1；多套时写入第一套成功的，其余可在剪辑台里切换）"}}}`)
}

type smartcutPlanArgs struct {
	NodeID      string `json:"nodeId"`
	Mode        string `json:"mode"`
	Instruction string `json:"instruction"`
	TargetSec   int    `json:"targetSec"`
	AspectRatio string `json:"aspectRatio"`
	Variants    int    `json:"variants"`
}

func (t SmartcutPlan) Run(ctx context.Context, rc app.RunContext, raw json.RawMessage) (app.ToolResult, error) {
	if err := requireExecuting(rc, domain.ToolSmartcutPlan); err != nil {
		return app.ToolResult{}, err
	}
	if t.d.Canvas == nil || t.d.Smartcuts == nil {
		return app.ToolResult{}, errors.New("智能剪辑服务未接入")
	}
	var a smartcutPlanArgs
	if err := multiDecode(raw, &a); err != nil {
		return app.ToolResult{}, err
	}
	a.NodeID, a.Mode = strings.TrimSpace(a.NodeID), strings.TrimSpace(a.Mode)
	if a.NodeID == "" {
		return app.ToolResult{}, errors.New("nodeId 必填：要剪辑的智能剪辑节点")
	}
	if _, ok := smartcutModes[a.Mode]; !ok {
		return app.ToolResult{}, errors.New("mode 只能是 mix（素材混剪）/ board（分镜成片）/ talk（口播精剪）/ narr（解说成片）")
	}
	o := objectOrch(t.d.Canvas)
	o.Logger = t.d.logger()
	timelineID, repair, err := canvasgen.SmartcutTimelineOfNode(ctx, o, multiCaller(rc), a.NodeID,
		genSmartcutTimelines{t.d.Smartcuts})
	if err != nil {
		return app.ToolResult{}, smartcutNodeError(a.NodeID, err)
	}
	out, err := t.d.Smartcuts.Plan(ctx, app.SmartcutPlanRequest{WorkspaceID: rc.WorkspaceID, ProjectID: rc.ProjectID,
		TimelineID: timelineID, UserID: rc.UserID, SessionID: rc.SessionID, MemberLevelID: rc.MemberLevelID,
		Mode: a.Mode, Instruction: strings.TrimSpace(a.Instruction), TargetSec: a.TargetSec,
		AspectRatio: strings.TrimSpace(a.AspectRatio), Variants: a.Variants})
	if err != nil {
		return app.ToolResult{}, smartcutError("AI 剪辑", err)
	}
	variants := make([]map[string]any, 0, len(out.Variants))
	var warnings []string
	for _, v := range out.Variants {
		m := map[string]any{"index": v.Index, "status": v.Status, "summary": v.Summary, "totalMs": v.TotalMs}
		if len(v.Warnings) > 0 {
			m["warnings"] = v.Warnings
		}
		if v.UsedFallback {
			m["usedFallback"] = true
		}
		if v.Error != "" {
			m["error"] = v.Error
		}
		if v.Index == out.AppliedVariant {
			warnings = append(warnings, v.Warnings...)
		}
		variants = append(variants, m)
	}
	content := map[string]any{"nodeId": a.NodeID, "timelineId": idS(timelineID), "callId": idS(out.CallID),
		"appliedVariant": out.AppliedVariant, "variants": variants, "version": out.Summary.Version,
		"clipCount": out.Summary.ClipCount, "effectiveMs": out.Summary.EffectiveMs,
		"next": "把剪辑结果（与警告）告诉用户；需要成片时用 render（给 timelineId）导出（免费）。不满意可以换个要求再调用一次，" +
			"或请用户在剪辑台里「撤销这轮 AI」"}
	if out.SnapshotID != 0 {
		content["snapshotId"] = idS(out.SnapshotID)
	}
	if len(warnings) > 0 {
		content["warnings"] = warnings
	}
	if repair != "" {
		content["repair"] = repair
	}
	display := map[string]any{"nodeId": a.NodeID, "timelineId": idS(timelineID), "mode": a.Mode,
		"clipCount": out.Summary.ClipCount, "effectiveMs": out.Summary.EffectiveMs, "variants": len(out.Variants)}
	return app.ToolResult{Content: ptJSON(content), Display: ptJSON(display), Outputs: 1}, nil
}

// ---------------------------------------------------------------- 公共

func trimIDs(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		out = append(out, strings.TrimSpace(s))
	}
	return out
}

// smartcutNodeError 把「按节点取时间线」的错误翻成给模型的话：节点不存在、不是智能剪辑说清该怎么办；
// 时间线服务的业务错误走 smartcutError。
func smartcutNodeError(nodeID string, err error) error {
	var ns *canvasgen.NotSmartcutError
	switch {
	case errors.Is(err, canvasgen.ErrNodeNotFound):
		return fmt.Errorf("画布上没有节点 %s：先用 read_canvas 确认，或用 smartcut_create 新建一个智能剪辑", nodeID)
	case errors.As(err, &ns):
		return fmt.Errorf("节点 %s 是 %s，不是智能剪辑：先用 smartcut_create 新建一个，把它连进去", nodeID, ns.Type)
	}
	return smartcutError("取智能剪辑的时间线", err)
}

// smartcutCanvasError 连线时画布的错误：节点不是本会话建的（用户的节点不能往上接线）说清怎么办。
func smartcutCanvasError(err error) error {
	if errors.Is(err, app.ErrNodeNotOwned) {
		return errors.New("这个智能剪辑节点由用户创建或已被用户修改，不能往上连线；请用户自己把素材连进去，或新建一个智能剪辑")
	}
	return err
}

// smartcutError 把智能剪辑的业务错误原样交给模型（同导演台 directorError）：错误码、原因与详情（限流的
// retry_after_seconds、素材不合适的 reason 与出路）都保留，再附一句该怎么做。不是业务错误的照常报错。
func smartcutError(what string, err error) error {
	var be *httpx.BizError
	if !errors.As(err, &be) {
		return fmt.Errorf("%s失败：%v", what, err)
	}
	details := make(map[string]any, len(be.Details)+2)
	for k, v := range be.Details {
		details[k] = v
	}
	details["code"] = int(be.Code)
	if next := smartcutNext(be); next != "" {
		details["next"] = next
	}
	code := be.Code.Name()
	if code == "" {
		code = errcode.Internal.Name()
	}
	return &domain.ToolRejection{Code: code, Message: be.Error(), Details: details}
}

func smartcutNext(be *httpx.BizError) string {
	switch be.Code {
	case errcode.RateLimited:
		return "把原因与需要等待的时间（retry_after_seconds 秒）告诉用户；到时间之前不要再调用 smartcut_plan"
	case errcode.ConcurrencyLimited:
		return "上一次 AI 剪辑还没结束：等 retry_after_seconds 秒再试，不要连续重试"
	case errcode.SmartcutInputUnsuitable:
		return "素材与剪辑方式不匹配：按 message 说明补齐素材（例如连一个音频节点当配乐、先识别语音），或换一种剪辑方式；不要原样重试"
	case errcode.SmartcutPlanUnavailable:
		return "AI 剪辑现在用不了（没有可用的模型）：把原因告诉用户，不要重试；用户仍可以在剪辑台里手动剪"
	case errcode.VersionConflict:
		return "剪辑期间用户在剪辑台里改过，这次没有写入（不会覆盖用户的改动）：告诉用户，等他确认后再调用一次"
	case errcode.InvalidStateTransition:
		return "当前状态下不能剪辑（例如项目在回收站里）：把原因告诉用户"
	}
	return ""
}

// genSmartcutTimelines 是 canvasgen.SmartcutTimelines 的 Agent 实现（经 agent 的端口到智能剪辑服务）。
type genSmartcutTimelines struct{ s app.Smartcuts }

func (p genSmartcutTimelines) Create(ctx context.Context, c canvasgen.Caller, nodeID string, source int64) (int64, error) {
	return p.s.CreateTimeline(ctx, c.WorkspaceID, c.ProjectID, nodeID, source)
}

func (p genSmartcutTimelines) TimelineNode(ctx context.Context, c canvasgen.Caller, timelineID int64) (string, error) {
	node, err := p.s.TimelineNode(ctx, c.WorkspaceID, c.ProjectID, timelineID)
	if errors.Is(err, app.ErrSmartcutTimelineNotFound) {
		return "", canvasgen.ErrSmartcutTimelineNotFound
	}
	return node, err
}

// smartcutNoTimelineMessage 节点还没有自己的时间线时的说明：AI 剪辑开着时 smartcut_plan 也会先补上。
func smartcutNoTimelineMessage(aiEnabled bool) string {
	if aiEnabled {
		return "这个智能剪辑还没有自己的时间线（新建后还没打开过，或是复制出来的）：调用 smartcut_plan 时会先补上，" +
			"用户打开剪辑台时也会自动补上"
	}
	return "这个智能剪辑还没有自己的时间线（新建后还没打开过，或是复制出来的）：用户打开剪辑台时会自动补上"
}
