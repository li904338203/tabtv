package tools

import (
	"bytes"
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
)

// 多段工具（AGT-50）的公共部分：依赖、ID 解析、付费外壳的结果分流、把业务对象写回 Agent 建的节点。
//
// ⚠️ 付费工具一律「报价 → PaidCall（额度检查 → 预留 → 执行 → charged / released）」，
// 报价由服务端算（端口按真实受理的同一组参数问目录），不信模型给的金额。
// ⚠️ 新建的脚本节点 / 组节点在业务对象建好之后**必须把对象 ID 写回节点参数**（同 generate 的
// link_generation，AGT-24 缺陷修复）：前端按 params.scriptId / params.groupId 取脚本与组，
// 不写的话节点在画布上一直是空的。写回用独立批次（复用建节点那一批会命中幂等、什么都不写），
// 失败只告警 —— 对象已经建好（钱可能已经扣了），报错只会让模型再做一遍。

// MultiDeps 是多段工具的依赖。
type MultiDeps struct {
	Canvas    app.Canvas
	Scripts   app.Scripts
	Workflows app.Workflows
	Timelines app.Timelines
	// Nodes：timeline_assemble 不走脚本、按视频节点拼时先判每个节点出没出片（nil = 不支持，只能按脚本装配）。
	Nodes app.NodeGenerations
	// Directors：3D 导演台的建场景与 AI 搭景（DRT-29；nil = 未接入，两个导演台工具回「未接入」）。
	Directors app.Directors
	// Smartcuts：智能剪辑的时间线、详情与 AI 剪辑（SCT-41；nil = 未接入，三个智能剪辑工具回「未接入」，
	// timeline_assemble 照旧装配但不在画布上建节点）。
	Smartcuts app.Smartcuts
	Budget    PaidCaller
	Logger    *slog.Logger
	// SmartcutAIDisabled 为 true 时不装 smartcut_plan（这一版智能剪辑只保留手动剪辑，SEEAITV_SMARTCUT_AI_ENABLED）。
	SmartcutAIDisabled bool
}

func (d MultiDeps) logger() *slog.Logger {
	if d.Logger != nil {
		return d.Logger
	}
	return slog.Default()
}

// MultiTools 装出全部多段工具。
func MultiTools(d MultiDeps) []Tool {
	tools := []Tool{
		ScriptCreate{d}, ScriptStoryboard{d}, ScriptAssets{d}, ScriptCompose{d},
		GroupCreate{d}, GroupExecute{d}, RunWorkflow{d}, TimelineAssemble{d}, Render{d},
		DirectorCreate{d}, DirectorBuild{d},
		SmartcutCreate{d}, SmartcutGet{d},
	}
	if !d.SmartcutAIDisabled {
		tools = append(tools, SmartcutPlan{d})
	}
	return tools
}

// flexID 收字符串或数字形式的 ID（模型两种都会给；雪花号按字符串给才不丢精度）。
type flexID int64

func (f *flexID) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "null" || s == `""` {
		*f = 0
		return nil
	}
	s = strings.Trim(s, `"`)
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil || v < 0 {
		return fmt.Errorf("ID 必须是正整数（字符串形式），实际 %s", string(b))
	}
	*f = flexID(v)
	return nil
}

func ids64(in []flexID) []int64 {
	out := make([]int64, 0, len(in))
	for _, v := range in {
		if v > 0 {
			out = append(out, int64(v))
		}
	}
	return out
}

// multiDecode 严格解码（未知字段报错，键名写错时让模型改）。
func multiDecode(raw json.RawMessage, v any) error {
	if len(bytes.TrimSpace(raw)) == 0 {
		raw = json.RawMessage(`{}`)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	dec.UseNumber()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("参数格式错误：%v", err)
	}
	return nil
}

func requireExecuting(rc app.RunContext, name domain.ToolName) error {
	if rc.State != domain.StateExecuting {
		return fmt.Errorf("%s 只能在执行阶段使用（当前 %s）", name, rc.State)
	}
	return nil
}

func wantWait(w *bool) bool { return w == nil || *w }

func idS(v int64) string { return strconv.FormatInt(v, 10) }

// overBudgetResult 是超额度挂起时回给模型的结果（追加额度卡已由外壳写好）。
func overBudgetResult(susp *app.Suspension, needed int, extra map[string]any) app.ToolResult {
	c := map[string]any{"status": "over_budget", "needed": needed,
		"message": "超出用户批准的额度，已请用户追加额度。批准后用同样的参数再调用一次。"}
	d := map[string]any{"needed": needed, "overBudget": true}
	for k, v := range extra {
		c[k], d[k] = v, v
	}
	return app.ToolResult{Content: ptJSON(c), Display: ptJSON(d), Suspend: susp, Outputs: 1}
}

// paidError 把付费调用失败翻成给模型的话：余额不足说清还差多少且不要重试。
func paidError(what string, quote int, err error) error {
	var ins *app.InsufficientCreditsError
	if errors.As(err, &ins) {
		if ins.Balance >= 0 && quote > ins.Balance {
			return fmt.Errorf("余额不足，还差 %d 积分（%s 需要 %d，当前余额 %d）。请告诉用户充值后再继续，不要重试",
				quote-ins.Balance, what, quote, ins.Balance)
		}
		return fmt.Errorf("余额不足（%s 需要约 %d 积分）。请告诉用户充值后再继续，不要重试", what, quote)
	}
	if errors.Is(err, app.ErrInsufficientCredits) {
		return fmt.Errorf("余额不足（%s 需要约 %d 积分）。请告诉用户充值后再继续，不要重试", what, quote)
	}
	return fmt.Errorf("%s失败：%v", what, err)
}

// waitSuspension 是「提交后等终态」的挂起。
func waitSuspension(reason string) *app.Suspension {
	return &app.Suspension{Event: domain.EvWaitTask, Reason: reason}
}

// addAgentNode 建一个 Agent 节点（带 agent.stepId），返回落盘后的节点 ID。
//
// 「建节点 → 业务对象 → 写回」下沉到 internal/canvasgen（MCPT-16，与 MCP 共用）；这里只组 Agent 的调用方
// （批次 BatchID(rc)、新节点写 agent.stepId），错误按 applyCanvas 的规则翻成给模型的话。
func addAgentNode(ctx context.Context, c app.Canvas, rc app.RunContext, nodeType, title string,
	params map[string]any, pos *position) (string, error) {
	if c == nil {
		return "", errors.New("画布服务未接入")
	}
	var p *canvasgen.Position
	if pos != nil {
		p = &canvasgen.Position{X: pos.X, Y: pos.Y}
	}
	id, err := objectOrch(c).AddNode(ctx, multiCaller(rc),
		canvasgen.ObjectNode{NodeType: nodeType, Title: title, Params: params, Position: p})
	if err != nil {
		return "", ensureNodeError(err)
	}
	return id, nil
}

// position 是节点位置。
type position struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}

// objectOrch 是只用到画布的 canvasgen 编排（建节点 / 写回不报价、不受理）。
func objectOrch(c app.Canvas) canvasgen.Orchestrator {
	return canvasgen.Orchestrator{Canvas: genCanvasPort{c}}
}

// multiCaller 是多段工具写画布的调用方：批次 BatchID(rc)，新节点写 agent.stepId，告警带 session / step。
func multiCaller(rc app.RunContext) canvasgen.Caller {
	return canvasgen.Caller{
		WorkspaceID: rc.WorkspaceID, ProjectID: rc.ProjectID, UserID: rc.UserID, Role: rc.Role,
		PayerAccountID: rc.PayerAccountID, MemberLevelID: rc.MemberLevelID,
		Origin:    canvasgen.Origin{Kind: canvasgen.OriginAgent, SessionID: rc.SessionID},
		BatchID:   BatchID(rc),
		StampNode: func(op map[string]any) { stampAgentStep(op, "node", rc.StepID) },
		LogAttrs:  []any{"session", rc.SessionID, "step", rc.StepID},
	}
}

// syncNodeParams 用独立批次把业务对象的状态写回 Agent 建的节点（merge patch）。失败只告警。
//
// suffix 区分同一步骤里的不同写回（批次 ID = BatchID(rc) + ":" + suffix）。
// 节点不是本会话建的、或用户改过（所有权规则拒绝）时同样只告警：那是用户的节点，前端自己会刷新。
func syncNodeParams(ctx context.Context, d MultiDeps, rc app.RunContext, nodeID, suffix string,
	params map[string]any) {
	if d.Canvas == nil {
		return
	}
	o := objectOrch(d.Canvas)
	o.Logger = d.logger()
	o.WriteBack(ctx, multiCaller(rc), nodeID, suffix, params)
}

// scriptNodeParams 是脚本节点的参数（canvasgen.ScriptNodeParams，与前端写的键一致）。
func scriptNodeParams(v app.ScriptView) map[string]any {
	f := canvasgen.ScriptFacts{ID: v.ID, SourceType: v.SourceType, Status: v.Status, CurrentStep: v.CurrentStep,
		Version: v.Version}
	for _, sh := range v.Shots {
		f.Shots = append(f.Shots, canvasgen.ShotPromptFacts{ImagePromptStatus: sh.ImagePromptStatus,
			VideoPromptStatus: sh.VideoPromptStatus})
	}
	return canvasgen.ScriptNodeParams(f)
}

// groupNodeParams 是组节点的参数（canvasgen.GroupNodeParams，与前端写的键一致）。
func groupNodeParams(g app.GroupView, modelCode string) map[string]any {
	f := canvasgen.GroupFacts{ID: g.ID, ProjectID: g.ProjectID, ScriptID: g.ScriptID, GroupType: g.GroupType,
		Status: g.Status, Version: g.Version}
	for _, it := range g.Items {
		f.Items = append(f.Items, canvasgen.GroupItemFacts{ID: it.ID, ShotID: it.ShotID, ShotNo: it.ShotNo,
			AssetID: it.AssetID, GenerationID: it.GenerationID, Status: it.Status, Operation: it.Operation,
			DurationSec: it.DurationSec, Error: it.Error})
	}
	return canvasgen.GroupNodeParams(f, modelCode)
}

// groupItemsView 是回给模型的组子项摘要。
func groupItemsView(g app.GroupView) []map[string]any {
	out := make([]map[string]any, 0, len(g.Items))
	for _, it := range g.Items {
		m := map[string]any{"itemId": idS(it.ID), "status": it.Status}
		if it.ShotNo > 0 {
			m["shotNo"] = it.ShotNo
		}
		if it.Operation != "" {
			m["operation"] = it.Operation
		}
		if it.DurationSec > 0 {
			m["durationSec"] = it.DurationSec
		}
		m["inputs"] = it.Inputs
		if it.Credits > 0 {
			m["credits"] = it.Credits
		}
		if it.Error != "" {
			m["error"] = it.Error
		}
		out = append(out, m)
	}
	return out
}
