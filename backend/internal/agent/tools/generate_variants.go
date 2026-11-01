package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/li904338203/tabtv/backend/internal/agent/app"
	"github.com/li904338203/tabtv/backend/internal/agent/domain"
)

// 批量变体在画布上的排列（MKT-T-27、MKT-F-54）：同一张已批准计划的变体节点按变体序排成一行、
// 归进同一个画框（复用画布域的 add_frame / set_frame_members，与 canvas_apply 同一套操作）。
//
//   - 画框按规划卡认（frame 的 agent.planCardId）：同一张计划的第二个变体找到第一个建的那个框，
//     而不是再建一个；用户把框挪走了，后面的变体跟着框的当前位置排。
//   - 列 = 变体（按 variant 升序），行 = 同一变体里的条目（按计划顺序）；格子被占了（重试新建的节点）
//     往下顺延，不叠在旧节点上。
//   - 只排**新建**的节点：模型给了 nodeId 的是在原节点上重跑，不挪用户看惯了的位置。
//
// ⚠️ 排列是展示，不是正确性：读画布 / 建框 / 挂成员任一步失败只记日志，不影响受理与扣费 ——
// 为一个画框让一次已经扣费的生成报错不值。只在 ≥ 2 个变体时排（单条计划框起来只是噪声）。

// 格子与画框的留白（设计建议值）：列宽按最宽的视频节点（390）+ 间距，行高给节点展开后的预览留余量。
const (
	variantCellW    = 430
	variantCellH    = 320
	variantPadX     = 40
	variantPadTop   = 60
	variantPadBot   = 40
	variantFrameGap = 120
	maxFrameTitle   = 30
)

// variantPlacement 是一个变体节点的落点。FrameID 为空表示没有画框（只排位置）。
type variantPlacement struct {
	FrameID string
	X, Y    float64
}

// variantCell 给出条目 key 在网格里的列 / 行，以及网格的列数 / 行数。ok=false：不是批量计划或条目不在计划里。
func variantCell(items []domain.PlanItem, key string) (col, row, cols, rows int, ok bool) {
	if domain.CountVariants(items) < 2 {
		return 0, 0, 0, 0, false
	}
	// 列：有 variant 的按 variant 升序；没写 variant 的条目各占一列，排在后面（按计划顺序）。
	type column struct {
		variant int
		first   int
		keys    []string
	}
	byVariant := map[int]*column{}
	var columns []*column
	for i, it := range items {
		if it.Variant > 0 {
			c := byVariant[it.Variant]
			if c == nil {
				c = &column{variant: it.Variant, first: i}
				byVariant[it.Variant] = c
				columns = append(columns, c)
			}
			c.keys = append(c.keys, it.Key)
			continue
		}
		columns = append(columns, &column{variant: 1 << 30, first: i, keys: []string{it.Key}})
	}
	sort.SliceStable(columns, func(i, j int) bool {
		if columns[i].variant != columns[j].variant {
			return columns[i].variant < columns[j].variant
		}
		return columns[i].first < columns[j].first
	})
	col, row = -1, -1
	for ci, c := range columns {
		if len(c.keys) > rows {
			rows = len(c.keys)
		}
		for ri, k := range c.keys {
			if k == key && col < 0 {
				col, row = ci, ri
			}
		}
	}
	if col < 0 {
		return 0, 0, 0, 0, false
	}
	return col, row, len(columns), rows, true
}

// frameSize 是容纳 cols × rows 个格子的画框尺寸（多留一行给重试新建的节点）。
func frameSize(cols, rows int) (w, h float64) {
	gap := variantCellW - 390 // 最后一列不留列间距
	w = float64(variantPadX*2 + cols*variantCellW - gap)
	h = float64(variantPadTop + (rows+1)*variantCellH + variantPadBot)
	return w, h
}

// canvasNode 是排列用到的节点字段。
type canvasNode struct {
	ID       string                  `json:"id"`
	Type     string                  `json:"type"`
	Position *struct{ X, Y float64 } `json:"position"`
	Size     *struct{ W, H float64 } `json:"size"`
	Members  []string                `json:"members"`
	Agent    map[string]any          `json:"agent"`
}

func canvasNodes(data json.RawMessage) []canvasNode {
	var doc struct {
		Nodes []json.RawMessage `json:"nodes"`
	}
	_ = json.Unmarshal(data, &doc)
	out := make([]canvasNode, 0, len(doc.Nodes))
	for _, raw := range doc.Nodes {
		var n canvasNode
		if json.Unmarshal(raw, &n) == nil && n.ID != "" {
			out = append(out, n)
		}
	}
	return out
}

func (n canvasNode) agentField(k string) string {
	v, _ := n.Agent[k].(string)
	return v
}

// planFrame 找本会话为这张计划建的画框。
func planFrame(nodes []canvasNode, sessionID, cardID int64) (canvasNode, bool) {
	sid, cid := strconv.FormatInt(sessionID, 10), strconv.FormatInt(cardID, 10)
	for _, n := range nodes {
		if n.Type == "frame" && n.agentField("planCardId") == cid && n.agentField("sessionId") == sid {
			return n, true
		}
	}
	return canvasNode{}, false
}

// newFramePosition 是新画框的位置：现有内容的右侧（不压住用户的节点）。
func newFramePosition(nodes []canvasNode) (x, y float64) {
	first := true
	for _, n := range nodes {
		if n.Position == nil {
			continue
		}
		right := n.Position.X
		if n.Size != nil {
			right += n.Size.W
		} else {
			right += 330
		}
		if first || right > x {
			x = right
		}
		if first || n.Position.Y < y {
			y = n.Position.Y
		}
		first = false
	}
	if first {
		return 0, 0
	}
	return x + variantFrameGap, y
}

// cellPosition 是格子在画框里的坐标；被别的节点占了就往下顺延（重试新建的节点不叠在旧节点上）。
func cellPosition(nodes []canvasNode, fx, fy float64, col, row int) (float64, float64) {
	x := fx + float64(variantPadX+col*variantCellW)
	y := fy + float64(variantPadTop+row*variantCellH)
	occupied := map[float64]bool{}
	for _, n := range nodes {
		if n.Type != "frame" && n.Position != nil && n.Position.X == x {
			occupied[n.Position.Y] = true
		}
	}
	for i := 0; i < 20 && occupied[y]; i++ {
		y += variantCellH
	}
	return x, y
}

// placeVariant 算出条目 key 的新节点落点，必要时为这张计划建画框。不是批量计划、或画布读写失败时返回 nil。
func (t Generate) placeVariant(ctx context.Context, rc app.RunContext, sp app.SessionPlan,
	key string) *variantPlacement {
	col, row, cols, rows, ok := variantCell(sp.Items, key)
	if !ok || t.Canvas == nil || sp.CardID == 0 {
		return nil
	}
	view, err := t.Canvas.Read(ctx, rc.WorkspaceID, rc.ProjectID)
	if err != nil {
		t.logger().Warn("批量变体排列：读画布失败（不影响生成）", "session", rc.SessionID, "step", rc.StepID,
			"error", err)
		return nil
	}
	nodes := canvasNodes(view.CanvasData)
	frame, found := planFrame(nodes, rc.SessionID, sp.CardID)
	var fx, fy float64
	if found && frame.Position != nil {
		fx, fy = frame.Position.X, frame.Position.Y
	} else {
		fx, fy = newFramePosition(nodes)
		w, h := frameSize(cols, rows)
		title, _ := ptCutRunes("批量变体 · "+strings.TrimSpace(sp.Title), maxFrameTitle)
		ops, _ := json.Marshal([]map[string]any{{
			"kind": "add_frame",
			"frame": map[string]any{"title": title, "position": map[string]float64{"x": fx, "y": fy},
				"size": map[string]float64{"w": w, "h": h}, "members": []string{},
				"agent": map[string]any{"planCardId": strconv.FormatInt(sp.CardID, 10)}},
		}})
		res, err := t.Canvas.Apply(ctx, app.ApplyOpsInput{WorkspaceID: rc.WorkspaceID, ProjectID: rc.ProjectID,
			UserID: rc.UserID, Role: rc.Role, AgentSessionID: rc.SessionID, BatchID: BatchID(rc) + ":frame",
			Ops: ops})
		if err != nil || len(res.NodeIDs) == 0 {
			t.logger().Warn("批量变体排列：建画框失败（只排位置，不影响生成）", "session", rc.SessionID,
				"step", rc.StepID, "error", err)
			x, y := cellPosition(nodes, fx, fy, col, row)
			return &variantPlacement{X: x, Y: y}
		}
		frame.ID = res.NodeIDs[0]
	}
	x, y := cellPosition(nodes, fx, fy, col, row)
	return &variantPlacement{FrameID: frame.ID, X: x, Y: y}
}

// attachVariant 把本步骤建的节点挂进画框（节点按 agent.stepId 认；在原节点上重跑的用 nodeID）。
// 受理失败（如余额不足）时节点照样保留在画布上，所以无论成败都挂。失败只记日志。
func (t Generate) attachVariant(ctx context.Context, rc app.RunContext, p *variantPlacement, nodeID string) {
	if p == nil || p.FrameID == "" {
		return
	}
	view, err := t.Canvas.Read(ctx, rc.WorkspaceID, rc.ProjectID)
	if err != nil {
		t.logger().Warn("批量变体排列：读画布失败，节点未归入画框", "step", rc.StepID, "error", err)
		return
	}
	nodes := canvasNodes(view.CanvasData)
	step := strconv.FormatInt(rc.StepID, 10)
	target := strings.TrimSpace(nodeID)
	var members []string
	frameFound := false
	for _, n := range nodes {
		if n.Type != "frame" && n.agentField("stepId") == step {
			target = n.ID
		}
		if n.ID == p.FrameID {
			members, frameFound = n.Members, true
		}
	}
	if target == "" || !frameFound {
		return
	}
	for _, m := range members {
		if m == target {
			return
		}
	}
	ops, _ := json.Marshal([]map[string]any{{"kind": "set_frame_members", "nodeId": p.FrameID,
		"frame": map[string]any{"members": append(append([]string{}, members...), target)}}})
	if _, err := t.Canvas.Apply(ctx, app.ApplyOpsInput{WorkspaceID: rc.WorkspaceID, ProjectID: rc.ProjectID,
		UserID: rc.UserID, Role: rc.Role, AgentSessionID: rc.SessionID, BatchID: BatchID(rc) + ":frame-members",
		Ops: ops}); err != nil {
		t.logger().Warn("批量变体排列：节点未归入画框（不影响生成）", "step", rc.StepID, "frame", p.FrameID,
			"node", target, "error", fmt.Sprint(err))
	}
}

// variantTitle 是变体节点的标题：有变体说明用变体说明（MKT-F-54「Hook：反问开场 · 风格：极简白」），
// 没有时用条目 label。
func variantTitle(it domain.PlanItem) string {
	if v := strings.TrimSpace(it.VariantLabel); v != "" && it.Variant > 0 {
		return v
	}
	return strings.TrimSpace(it.Label)
}
