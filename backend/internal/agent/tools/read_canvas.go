package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/li904338203/tabtv/backend/internal/agent/app"
	"github.com/li904338203/tabtv/backend/internal/agent/domain"
	collab "github.com/li904338203/tabtv/backend/internal/collaboration/domain"
)

// read_canvas（dev/01 §5.3）：节点摘要 + 每节点最近一次生成（任意状态）+ 第一个产出。
//
// ⚠️ 不带 nodeIds 时**不含 params 全文**（dev/01 §4.4）：一张画布几十个节点、每个节点几百字
// 提示词，全量回给模型会把上下文撑爆，而它多半只关心其中几个。带 nodeIds 时才回 params。
// ⚠️ 超过 8000 字时从尾部丢节点并注明截断（保持 JSON 合法 —— 截半个 JSON 模型读不懂）。

// maxReadNodeIDs 是一次精确读取的节点数上限。
const maxReadNodeIDs = 50

// maxParamsRunes 是单个节点 params 的上限；超出时给前缀 + 截断标记。
const maxParamsRunes = 3000

// ReadCanvas 是 read_canvas 工具。
type ReadCanvas struct {
	Canvas      app.Canvas
	Generations app.NodeGenerations
}

func (ReadCanvas) Name() domain.ToolName { return domain.ToolReadCanvas }
func (ReadCanvas) Paid() bool            { return false }
func (ReadCanvas) Action() collab.Action { return collab.ActionView }

func (ReadCanvas) Description() string {
	return "读取当前画布：节点 ID / 类型 / 标题 / 状态 / 是否由你创建、每个节点最近一次生成的状态与第一个产出，以及连线。" +
		"默认不含节点参数全文；需要某几个节点的完整参数时传 nodeIds。"
}

func (ReadCanvas) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{` +
		`"nodeIds":{"type":"array","items":{"type":"string"},"maxItems":50,"description":"只读这些节点，并返回它们的完整参数"}` +
		`},"additionalProperties":false}`)
}

type readCanvasArgs struct {
	NodeIDs []string `json:"nodeIds"`
}

type rcOutput struct {
	MediaID string `json:"mediaId,omitempty"`
	Type    string `json:"type,omitempty"`
}

type rcGeneration struct {
	ID        string    `json:"id"`
	Status    string    `json:"status"`
	ErrorCode string    `json:"errorCode,omitempty"`
	Output    *rcOutput `json:"output,omitempty"`
}

type rcNode struct {
	ID         string          `json:"id"`
	Type       string          `json:"type"`
	Title      string          `json:"title,omitempty"`
	Status     string          `json:"status,omitempty"`
	ByAgent    bool            `json:"byAgent,omitempty"`
	Generation *rcGeneration   `json:"generation,omitempty"`
	Params     json.RawMessage `json:"params,omitempty"`
}

type rcEdge struct {
	Source string `json:"source"`
	Target string `json:"target"`
	Order  int    `json:"order,omitempty"`
}

type rcResult struct {
	Version   int      `json:"version"`
	NodeCount int      `json:"nodeCount"`
	EdgeCount int      `json:"edgeCount"`
	Nodes     []rcNode `json:"nodes"`
	Edges     []rcEdge `json:"edges,omitempty"`
	Missing   []string `json:"missing,omitempty"`
	Truncated bool     `json:"truncated,omitempty"`
	Note      string   `json:"note,omitempty"`
}

func (t ReadCanvas) Run(ctx context.Context, rc RunContext, args json.RawMessage) (ToolResult, error) {
	var a readCanvasArgs
	if err := ptDecode(args, &a, false); err != nil {
		return ToolResult{}, err
	}
	if len(a.NodeIDs) > maxReadNodeIDs {
		return ToolResult{}, fmt.Errorf("nodeIds 一次最多 %d 个", maxReadNodeIDs)
	}
	view, err := t.Canvas.Read(ctx, rc.WorkspaceID, rc.ProjectID)
	if err != nil {
		return ToolResult{}, fmt.Errorf("读取画布失败：%w", err)
	}
	var gens map[string]app.NodeGeneration
	if t.Generations != nil {
		if gens, err = t.Generations.LatestByNode(ctx, rc.WorkspaceID, rc.ProjectID); err != nil {
			return ToolResult{}, fmt.Errorf("读取节点生成记录失败：%w", err)
		}
	}
	doc := parseCanvasDoc(view.CanvasData)

	res := rcResult{Version: view.Version, NodeCount: len(view.Nodes), EdgeCount: view.EdgeCount}
	want := map[string]bool{}
	for _, id := range a.NodeIDs {
		want[id] = true
	}
	found := map[string]bool{}
	for _, n := range view.Nodes {
		if len(want) > 0 && !want[n.ID] {
			continue
		}
		found[n.ID] = true
		node := rcNode{ID: n.ID, Type: n.Type, Title: n.Title, Status: n.Status, ByAgent: n.ByAgent}
		if g, ok := gens[n.ID]; ok {
			rg := &rcGeneration{ID: fmt.Sprint(g.GenerationID), Status: g.Status, ErrorCode: g.ErrorCode}
			if g.OutputMediaID != 0 || g.OutputType != "" {
				rg.Output = &rcOutput{Type: g.OutputType}
				if g.OutputMediaID != 0 {
					rg.Output.MediaID = fmt.Sprint(g.OutputMediaID)
				}
			}
			node.Generation = rg
		}
		if len(want) > 0 {
			node.Params = cappedParams(doc.params[n.ID])
		}
		res.Nodes = append(res.Nodes, node)
	}
	for _, id := range a.NodeIDs {
		if !found[id] {
			res.Missing = append(res.Missing, id)
		}
	}
	for _, e := range doc.edges {
		if len(want) > 0 && !want[e.Source] && !want[e.Target] {
			continue
		}
		res.Edges = append(res.Edges, e)
	}
	content := fitCanvasResult(res)
	return ToolResult{
		Content: content,
		Display: ptJSON(map[string]any{"nodeCount": res.NodeCount}),
	}, nil
}

// fitCanvasResult 超过上限时先丢连线、再从尾部丢节点，直到放得下。
func fitCanvasResult(res rcResult) json.RawMessage {
	b := ptJSON(res)
	if ptRunes(b) <= MaxContentRunes {
		return b
	}
	res.Truncated, res.Note = true, TruncatedNote
	res.Edges = nil
	for {
		b = ptJSON(res)
		if ptRunes(b) <= MaxContentRunes || len(res.Nodes) == 0 {
			return b
		}
		// 一次丢掉超出比例的那部分节点（至少一个），避免几千个节点时逐个重算。
		over := ptRunes(b) - MaxContentRunes
		drop := over * len(res.Nodes) / ptRunes(b)
		if drop < 1 {
			drop = 1
		}
		if drop > len(res.Nodes) {
			drop = len(res.Nodes)
		}
		res.Nodes = res.Nodes[:len(res.Nodes)-drop]
	}
}

// cappedParams 单个节点的 params 太长时给前缀（字符串）+ 截断标记，保证结果仍是合法 JSON。
func cappedParams(p json.RawMessage) json.RawMessage {
	if len(p) == 0 {
		return nil
	}
	if ptRunes(p) <= maxParamsRunes {
		return p
	}
	head, _ := ptCutRunes(string(p), maxParamsRunes)
	return ptJSON(map[string]any{"_truncated": true, "_prefix": head})
}

type canvasDoc struct {
	params map[string]json.RawMessage
	edges  []rcEdge
}

// parseCanvasDoc 取节点 params 与连线（落库形状：扁平节点 + source/target/order 连线）。
// 解析失败返回空：画布摘要仍由 CanvasView.Nodes 给出，不因一个坏字段整体失败。
func parseCanvasDoc(raw json.RawMessage) canvasDoc {
	out := canvasDoc{params: map[string]json.RawMessage{}}
	var doc struct {
		Nodes []struct {
			ID     string          `json:"id"`
			Params json.RawMessage `json:"params"`
		} `json:"nodes"`
		Edges []struct {
			Source string  `json:"source"`
			Target string  `json:"target"`
			Order  float64 `json:"order"`
		} `json:"edges"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &doc) != nil {
		return out
	}
	for _, n := range doc.Nodes {
		out.params[n.ID] = n.Params
	}
	for _, e := range doc.Edges {
		out.edges = append(out.edges, rcEdge{Source: e.Source, Target: e.Target, Order: int(e.Order)})
	}
	return out
}

var _ app.Tool = ReadCanvas{}
