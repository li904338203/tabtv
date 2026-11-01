package domain

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// 画布操作（AGT-10，开发 01 §6.1）：服务端在**最新版本**上应用一批操作。
//
// ⚠️ 写的是**落库形状**，不是 Vue Flow 的运行时形状。`stores/canvas.ts document()` 存进
// `canvas_data` 的节点是扁平的 `{id, type:<节点类型>, title, position, size, params, result}`；
// `type:"aitv"` + `data:{...}` 只存在于前端内存。Agent 元数据放在节点**顶层**
// `agent:{sessionId, stepId, sourceDocId, userTouched}`（AGT-07 保证前端往返不丢）；
// MCP 建的节点写顶层 `origin:{kind:"mcp", grantId, clientName, userTouched}`（MCPT-09），
// 两种字段的解析只经 NodeOrigin。
//
// ⚠️ 纯函数：不碰库、不发网络。一批操作要么全部生效、要么一个都不生效 ——
// 任何一步失败整批拒绝，结果还要过 ValidateCanvas。
//
// ⚠️ 在 `map[string]any` 上**只改涉及的键**，其余原样保留（GO-35）；解码用 UseNumber，
// 否则节点 params 里的雪花号（大整数）会被变成 float64、存回去就丢了精度。
//
// 前后端各有一份实现（Go 应用 / 前端重放），由 api/fixtures/canvas-ops 的共享 fixture 钉住。

// 操作种类。
const (
	OpAddNode         = "add_node"
	OpUpdateNode      = "update_node"
	OpMoveNode        = "move_node"
	OpAddEdge         = "add_edge"
	OpAddFrame        = "add_frame"
	OpSetFrameMembers = "set_frame_members"
	// OpLinkGeneration 把一次**已受理**的生成挂到节点上（节点 `result` = `{id, status:"queued", progress:0}`）。
	//
	// ⚠️ 只由 generate 工具在受理成功之后产生，canvas_apply 的允许清单里**没有**它：
	// result 是「这个节点上有一次生成」的快照，让模型自己写的话它能把任意 generationId
	// 伪造到节点上。生成域那一行的 node_id 已经指向这个节点，这一步只是把同一个事实
	// 告诉画布 —— 不写的话前端无从知道节点上有任务（restore() 靠 result.id 接上跟踪），
	// 节点就一直显示成空的，而任务明明成功了、积分也扣了。
	OpLinkGeneration = "link_generation"
)

// 操作者种类。agent = 站内 Agent 会话；mcp = 外部 AI 客户端经 MCP 授权写入（MCPT-09）；
// user 留给将来的服务端写入方（不做所有权限制）。
const (
	ActorAgent = "agent"
	ActorUser  = "user"
	ActorMCP   = "mcp"
)

const (
	// FrameNodeType 是画框节点的 type。⚠️ 与「组节点」（group，带批量执行与计费语义）
	// **严格区分**（总体设计 §7.5），不复用 group。
	FrameNodeType = "frame"
	// RefPrefix 标识批内引用：add_node / add_frame 用 `ref` 给新节点起名，
	// 同批后续操作用 `ref:xxx` 指代它，应用时替换成服务端分配的真实 ID。
	RefPrefix = "ref:"
	// MaxOpsPerBatch 是**设计建议值**：一批操作对应 Agent 一次工具调用，
	// 几百个已经远超任何正常场景；不设上限的话一次异常输出能把整张画布撑到上限。
	MaxOpsPerBatch = 200
)

var (
	// ErrNotOwned Agent 试图修改不是本会话创建、或用户之后动过的节点（设计 §7.1）。
	// Agent 侧应改为在旁边新建一个，而不是重试。
	ErrNotOwned = errors.New("不能修改非本会话创建或用户改过的节点")
	// ErrNodeNotFound 操作引用的节点不存在（可能已被用户删掉）。
	ErrNodeNotFound = errors.New("节点不存在")
	// ErrOpInvalid 操作本身不合法（种类未知、字段缺失、引用未定义……）。
	ErrOpInvalid = errors.New("画布操作非法")
)

// Point 是画布坐标。
type Point struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}

// Op 是一条画布操作。JSON 形状与共享 fixture、canvas_op_log.ops、canvas.ops_applied 事件一致。
type Op struct {
	Kind string `json:"kind"`
	// NodeID 被操作的节点（update_node / move_node / set_frame_members）；可以是 `ref:xxx`。
	NodeID string `json:"nodeId,omitempty"`
	// Ref 给本批新建的节点 / 画框起名（`ref:xxx`），供同批后续操作引用。
	Ref string `json:"ref,omitempty"`
	// Node add_node：存储形状的节点（id 由服务端分配，传了也会被覆盖）。
	Node json.RawMessage `json:"node,omitempty"`
	// Patch update_node：只允许 `title` 与 `params`（params 按 JSON merge patch 合并）；
	// link_generation：只允许 `{generationId}`（字符串形式的正整数）。
	Patch json.RawMessage `json:"patch,omitempty"`
	// Edge add_edge：存储形状的连线（id 由服务端分配）。
	Edge json.RawMessage `json:"edge,omitempty"`
	// Frame add_frame：`{title, position, size, members}`；set_frame_members：`{members}`。
	Frame json.RawMessage `json:"frame,omitempty"`
	// Position move_node 的目标位置；add_node / add_frame 给了则覆盖节点里的 position。
	Position *Point `json:"position,omitempty"`
}

// Actor 是这一批操作的发起方。
//
// SessionID 只对 agent 有意义，GrantID / ClientName 只对 mcp 有意义（MCPT-09）。
// ClientName 只是展示用（写进节点 origin.clientName），所有权一律按 GrantID 判。
type Actor struct {
	Kind       string
	UserID     int64
	SessionID  int64
	GrantID    int64
	ClientName string
}

// Applied 是应用结果。
//
// Ops 是**已解析**的操作：批内引用都换成了真实 ID，add_node / add_edge / add_frame
// 带着最终落库的完整节点 / 连线。它就是写进 canvas_op_log、推给前端重放的那一份 ——
// 前端重放时不需要再分配 ID，也不需要知道批内引用。
type Applied struct {
	Doc     json.RawMessage
	NodeIDs []string
	Ops     []Op
}

// ApplyOps 应用一批操作，返回新文档与本批新建的节点 ID（含画框，按创建顺序）。
// newID 给每个新建对象（节点、连线、画框）分配 ID，按操作顺序逐个调用。
func ApplyOps(doc json.RawMessage, ops []Op, actor Actor, newID func() string) (
	json.RawMessage, []string, error) {
	a, err := Apply(doc, ops, actor, newID)
	if err != nil {
		return nil, nil, err
	}
	return a.Doc, a.NodeIDs, nil
}

// Apply 同 ApplyOps，额外返回已解析的操作（写日志与发事件用）。
func Apply(doc json.RawMessage, ops []Op, actor Actor, newID func() string) (Applied, error) {
	switch actor.Kind {
	case ActorAgent, ActorUser, ActorMCP:
	default:
		return Applied{}, opErr("操作者种类非法: %q", actor.Kind)
	}
	if actor.Kind == ActorAgent && actor.SessionID <= 0 {
		return Applied{}, opErr("Agent 操作必须带会话 ID")
	}
	if actor.Kind == ActorMCP && actor.GrantID <= 0 {
		// 没有授权 ID 的 MCP 写入无从判定所有权：放行的话它建的节点谁都认领不了，
		// 而它能不能改别人的节点就取决于比较时恰好拿到的零值。
		return Applied{}, opErr("MCP 操作必须带授权 ID")
	}
	if len(ops) == 0 {
		return Applied{}, opErr("操作为空")
	}
	if len(ops) > MaxOpsPerBatch {
		return Applied{}, opErr("一批最多 %d 条操作，实际 %d", MaxOpsPerBatch, len(ops))
	}

	st, err := loadState(doc)
	if err != nil {
		return Applied{}, err
	}
	st.actor, st.newID = actor, newID

	applied := make([]Op, 0, len(ops))
	for i, op := range ops {
		out, err := st.apply(op)
		if err != nil {
			return Applied{}, fmt.Errorf("第 %d 条操作（%s）: %w", i+1, op.Kind, err)
		}
		applied = append(applied, out)
	}

	st.tree["nodes"] = st.nodes
	if st.edges != nil || st.hadEdges {
		if st.edges == nil {
			st.edges = []any{}
		}
		st.tree["edges"] = st.edges
	}
	raw, err := marshal(st.tree)
	if err != nil {
		return Applied{}, err
	}
	// 结果非法时整批拒绝：存进去以后打不开的画布比一次失败的工具调用糟得多。
	if err := ValidateCanvas(raw); err != nil {
		return Applied{}, err
	}
	return Applied{Doc: raw, NodeIDs: st.created, Ops: applied}, nil
}

// ---------------------------------------------------------------- 内部状态

type state struct {
	tree     map[string]any
	nodes    []any
	edges    []any
	hadEdges bool
	byID     map[string]map[string]any
	refs     map[string]string
	created  []string
	actor    Actor
	newID    func() string
}

func loadState(doc json.RawMessage) (*state, error) {
	st := &state{tree: map[string]any{}, byID: map[string]map[string]any{}, refs: map[string]string{}}
	if t := bytes.TrimSpace(doc); len(t) > 0 && !bytes.Equal(t, []byte("null")) {
		v, err := decode(t)
		if err != nil {
			return nil, invalid("画布数据必须是 JSON 对象")
		}
		m, ok := v.(map[string]any)
		if !ok {
			return nil, invalid("画布数据必须是 JSON 对象")
		}
		st.tree = m
	}
	if v, ok := st.tree["nodes"]; ok && v != nil {
		arr, ok := v.([]any)
		if !ok {
			return nil, invalid("nodes 必须是数组")
		}
		st.nodes = arr
	}
	if st.nodes == nil {
		st.nodes = []any{}
	}
	if v, ok := st.tree["edges"]; ok && v != nil {
		arr, ok := v.([]any)
		if !ok {
			return nil, invalid("edges 必须是数组")
		}
		st.edges, st.hadEdges = arr, true
	}
	for i, raw := range st.nodes {
		n, ok := raw.(map[string]any)
		if !ok {
			return nil, invalid("nodes[%d] 必须是对象", i)
		}
		if id, _ := n["id"].(string); id != "" {
			st.byID[id] = n
		}
	}
	return st, nil
}

func (st *state) apply(op Op) (Op, error) {
	switch op.Kind {
	case OpAddNode:
		return st.addNode(op)
	case OpUpdateNode:
		return st.updateNode(op)
	case OpMoveNode:
		return st.moveNode(op)
	case OpAddEdge:
		return st.addEdge(op)
	case OpAddFrame:
		return st.addFrame(op)
	case OpSetFrameMembers:
		return st.setFrameMembers(op)
	case OpLinkGeneration:
		return st.linkGeneration(op)
	default:
		return Op{}, opErr("未知的操作种类 %q", op.Kind)
	}
}

func (st *state) addNode(op Op) (Op, error) {
	n, err := object(op.Node, "node")
	if err != nil {
		return Op{}, err
	}
	typ, _ := n["type"].(string)
	if typ == "" {
		return Op{}, opErr("node.type 必填（节点类型，如 text / image / video）")
	}
	if typ == FrameNodeType {
		return Op{}, opErr("画框用 add_frame 创建")
	}
	if v, ok := n["title"]; !ok || v == nil {
		n["title"] = typ
	} else if _, ok := v.(string); !ok {
		return Op{}, opErr("node.title 必须是字符串")
	}
	params, ok := n["params"]
	if !ok || params == nil {
		params = map[string]any{}
		n["params"] = params
	}
	pm, ok := params.(map[string]any)
	if !ok {
		return Op{}, opErr("node.params 必须是对象")
	}
	if err := st.placeNew(n, op.Position); err != nil {
		return Op{}, err
	}
	if _, ok := n["size"]; !ok {
		w, h := DefaultNodeSize(typ, pm)
		n["size"] = map[string]any{"w": num(w), "h": num(h)}
	}
	id, err := st.register(n, op.Ref)
	if err != nil {
		return Op{}, err
	}
	st.nodes = append(st.nodes, n)
	st.created = append(st.created, id)
	return Op{Kind: OpAddNode, Node: mustMarshal(n)}, nil
}

func (st *state) addFrame(op Op) (Op, error) {
	f, err := object(op.Frame, "frame")
	if err != nil {
		return Op{}, err
	}
	f["type"] = FrameNodeType
	if v, ok := f["title"]; !ok || v == nil {
		f["title"] = "画框"
	} else if _, ok := v.(string); !ok {
		return Op{}, opErr("frame.title 必须是字符串")
	}
	if err := st.placeNew(f, op.Position); err != nil {
		return Op{}, err
	}
	if _, ok := f["size"]; !ok {
		f["size"] = map[string]any{"w": num(DefaultFrameWidth), "h": num(DefaultFrameHeight)}
	}
	members, err := st.members(f["members"])
	if err != nil {
		return Op{}, err
	}
	f["members"] = members
	id, err := st.register(f, op.Ref)
	if err != nil {
		return Op{}, err
	}
	st.nodes = append(st.nodes, f)
	st.created = append(st.created, id)
	return Op{Kind: OpAddFrame, Frame: mustMarshal(f)}, nil
}

// placeNew 处理新建节点 / 画框的公共部分：位置与来源字段（agent / origin）。
func (st *state) placeNew(n map[string]any, pos *Point) error {
	switch {
	case pos != nil:
		n["position"] = point(*pos)
	default:
		p, ok := n["position"].(map[string]any)
		if !ok {
			if v, has := n["position"]; has && v != nil {
				return opErr("position 必须是 {x, y}")
			}
			n["position"] = map[string]any{"x": num(0), "y": num(0)}
		} else if !isNumber(p["x"]) || !isNumber(p["y"]) {
			return opErr("position 必须是 {x, y}")
		}
	}
	// ⚠️ 来源由服务端按操作者**强制写入**，不信节点里带来的值：信它的话，
	// 一个会话 / 授权可以把节点标成别人的，从而绕过所有权规则去改别人的节点。
	// 读路径认两种字段（NodeOrigin），所以两种都要剥 —— 只剥一种的话，伪造另一种照样生效。
	switch st.actor.Kind {
	case ActorAgent:
		// ⚠️ Agent 新建节点**继续只写 agent**（MCPT-09 风险 1、dev/01 R8）：改写成 origin 等于
		// 顺手改了已上线的 Agent 行为（fixture 001/004/007、E2E 第 187 步都断言 agent.sessionId）。
		delete(n, OriginKey)
		meta, _ := n["agent"].(map[string]any)
		if meta == nil {
			meta = map[string]any{}
		}
		// ID 对外一律 string（防 JS 精度丢失，与前端 AgentMeta.sessionId 同型）。
		meta["sessionId"] = strconv.FormatInt(st.actor.SessionID, 10)
		delete(meta, "userTouched")
		n["agent"] = meta
	case ActorMCP:
		// MCP 只写 origin，客户端带来的 agent / origin 整个丢掉（不像 Agent 那样保留 stepId 之类的键：
		// 外部客户端写的任何来源键都没有可信的语义）。
		delete(n, "agent")
		meta := map[string]any{
			"kind":    ActorMCP,
			"grantId": strconv.FormatInt(st.actor.GrantID, 10),
		}
		if st.actor.ClientName != "" {
			meta["clientName"] = st.actor.ClientName
		}
		n[OriginKey] = meta
	default:
		// 非机器写入的节点不该自称由 Agent / MCP 创建 —— 那会把它变成机器可改的节点。
		delete(n, "agent")
		delete(n, OriginKey)
	}
	return nil
}

// register 分配 ID、登记批内引用。
func (st *state) register(n map[string]any, ref string) (string, error) {
	if ref != "" {
		if !strings.HasPrefix(ref, RefPrefix) || len(ref) == len(RefPrefix) {
			return "", opErr("ref 必须形如 %sxxx，实际 %q", RefPrefix, ref)
		}
		if _, dup := st.refs[ref]; dup {
			return "", opErr("批内引用 %s 重复定义", ref)
		}
	}
	id := st.newID()
	if id == "" || strings.HasPrefix(id, RefPrefix) {
		return "", opErr("分配的节点 ID 非法: %q", id)
	}
	if _, dup := st.byID[id]; dup {
		return "", opErr("分配的节点 ID 与已有节点重复: %s", id)
	}
	n["id"] = id
	st.byID[id] = n
	if ref != "" {
		st.refs[ref] = id
	}
	return id, nil
}

func (st *state) updateNode(op Op) (Op, error) {
	id, n, err := st.owned(op.NodeID)
	if err != nil {
		return Op{}, err
	}
	patch, err := object(op.Patch, "patch")
	if err != nil {
		return Op{}, err
	}
	if len(patch) == 0 {
		return Op{}, opErr("patch 为空")
	}
	for k, v := range patch {
		switch k {
		case "title":
			if _, ok := v.(string); !ok {
				return Op{}, opErr("patch.title 必须是字符串")
			}
		case "params":
			if _, ok := v.(map[string]any); !ok {
				return Op{}, opErr("patch.params 必须是对象（按 JSON merge patch 合并，null 删除某个键）")
			}
		default:
			// 只开放标题与参数：type / position / result / agent 改了都会改变节点的语义
			// （移动走 move_node；result 是 UI 快照，不该由 Agent 伪造）。
			return Op{}, opErr("update_node 只允许修改 title 与 params，不允许 %q", k)
		}
	}
	if t, ok := patch["title"]; ok {
		n["title"] = t
	}
	if p, ok := patch["params"]; ok {
		n["params"] = mergePatch(n["params"], p)
	}
	return Op{Kind: OpUpdateNode, NodeID: id, Patch: mustMarshal(patch)}, nil
}

// linkGeneration 把已受理的生成挂到节点上。
//
// 规则（取舍见 OpLinkGeneration 与 AGT-24 缺陷修复记录）：
//   - 节点必须由**本主体创建**（agent 比会话、mcp 比授权，CreatedByActor），但**不看 userTouched**：
//     userTouched 保护的是用户改过的**内容**（标题 / 参数 / 位置 / 连线），这一步一个都不碰，
//     只写「这里有一次生成」这个事实 —— 而那次生成的 node_id 早已指向它、钱也已经扣了。
//     拒绝的话节点照样显示成空的，正是要修的那个缺陷。
//   - 同一个 generationId 已经挂着时**不动**：重放 / 重试受理命中同一个任务时，
//     不能把已经 success 的结果打回 queued。
//   - 其余情况**整体替换** result：保留上一次的 mediaId / outputUrl 的话，节点会拿着
//     新任务的 ID 显示旧产物，下游还会按旧 mediaId 引用那张图。
func (st *state) linkGeneration(op Op) (Op, error) {
	id, n, err := st.lookup(op.NodeID)
	if err != nil {
		return Op{}, err
	}
	if st.actor.Kind != ActorUser && !CreatedByActor(n, st.actor) {
		return Op{}, fmt.Errorf("%w: %s", ErrNotOwned, id)
	}
	if isFrame(n) {
		return Op{}, opErr("画框不能挂生成任务")
	}
	patch, err := object(op.Patch, "patch")
	if err != nil {
		return Op{}, err
	}
	for k := range patch {
		if k != "generationId" {
			return Op{}, opErr("link_generation 只允许 generationId，不允许 %q", k)
		}
	}
	genID, _ := patch["generationId"].(string)
	if !isPositiveDecimal(genID) {
		return Op{}, opErr("patch.generationId 必须是字符串形式的正整数")
	}
	if cur, ok := n["result"].(map[string]any); !ok || cur["id"] != genID {
		n["result"] = map[string]any{"id": genID, "status": "queued", "progress": num(0)}
	}
	return Op{Kind: OpLinkGeneration, NodeID: id,
		Patch: mustMarshal(map[string]any{"generationId": genID})}, nil
}

func isPositiveDecimal(s string) bool {
	if s == "" || len(s) > 19 || s[0] == '0' {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	_, err := strconv.ParseInt(s, 10, 64)
	return err == nil
}

func (st *state) moveNode(op Op) (Op, error) {
	id, n, err := st.owned(op.NodeID)
	if err != nil {
		return Op{}, err
	}
	if op.Position == nil {
		return Op{}, opErr("move_node 缺少 position")
	}
	n["position"] = point(*op.Position)
	return Op{Kind: OpMoveNode, NodeID: id, Position: op.Position}, nil
}

func (st *state) addEdge(op Op) (Op, error) {
	e, err := object(op.Edge, "edge")
	if err != nil {
		return Op{}, err
	}
	src, _ := e["source"].(string)
	dst, _ := e["target"].(string)
	if src == "" || dst == "" {
		return Op{}, opErr("edge 缺少 source 或 target")
	}
	srcID, srcNode, err := st.lookup(src)
	if err != nil {
		return Op{}, err
	}
	// ⚠️ 连线**进入**一个节点就改变了它生成时的输入 —— 那等于修改了这个节点。
	// 所以目标节点要过所有权：Agent 可以拿用户的图当自己节点的输入，
	// 但不能往用户的节点上接线（设计 §7.1：Agent 与用户的操作不落在同一个节点上）。
	dstID, dstNode, err := st.owned(dst)
	if err != nil {
		return Op{}, err
	}
	if isFrame(srcNode) || isFrame(dstNode) {
		return Op{}, opErr("画框是纯视觉容器，不能连线")
	}
	e["source"], e["target"] = srcID, dstID
	if v, ok := e["sourceHandle"].(string); !ok || v == "" {
		e["sourceHandle"] = "out"
	}
	if v, ok := e["targetHandle"].(string); !ok || v == "" {
		e["targetHandle"] = "in"
	}
	// order 决定首尾帧与参考图的锚定顺序：不给就排在该目标现有入边之后
	// （与前端 nextInputOrder 同一规则）；给了就照用，重复由 ValidateCanvas 拦。
	if v, ok := e["order"]; !ok || v == nil {
		e["order"] = num(st.nextOrder(dstID))
	} else if o, ok := intOf(v); !ok || o < 1 {
		return Op{}, opErr("edge.order 必须是 ≥1 的整数")
	}
	if v, ok := e["operation"]; ok && v != nil {
		if _, ok := v.(string); !ok {
			return Op{}, opErr("edge.operation 必须是字符串")
		}
	}
	id := st.newID()
	if id == "" || strings.HasPrefix(id, RefPrefix) {
		return Op{}, opErr("分配的连线 ID 非法: %q", id)
	}
	e["id"] = id
	st.edges = append(st.edges, e)
	return Op{Kind: OpAddEdge, Edge: mustMarshal(e)}, nil
}

func (st *state) setFrameMembers(op Op) (Op, error) {
	id, n, err := st.owned(op.NodeID)
	if err != nil {
		return Op{}, err
	}
	if !isFrame(n) {
		return Op{}, opErr("节点 %s 不是画框", id)
	}
	f, err := object(op.Frame, "frame")
	if err != nil {
		return Op{}, err
	}
	for k := range f {
		if k != "members" {
			return Op{}, opErr("set_frame_members 只允许 members，不允许 %q", k)
		}
	}
	members, err := st.members(f["members"])
	if err != nil {
		return Op{}, err
	}
	n["members"] = members
	return Op{Kind: OpSetFrameMembers, NodeID: id,
		Frame: mustMarshal(map[string]any{"members": members})}, nil
}

// members 解析画框成员：可以是真实 ID 或批内引用，必须存在、不能是画框、不能重复。
// 画框只是"框住"，不改变成员节点本身，所以成员**不要求**归 Agent 所有。
func (st *state) members(v any) ([]any, error) {
	if v == nil {
		return []any{}, nil
	}
	arr, ok := v.([]any)
	if !ok {
		return nil, opErr("members 必须是节点 ID 数组")
	}
	out := make([]any, 0, len(arr))
	seen := map[string]bool{}
	for _, m := range arr {
		s, ok := m.(string)
		if !ok || s == "" {
			return nil, opErr("members 的每一项必须是节点 ID 字符串")
		}
		id, n, err := st.lookup(s)
		if err != nil {
			return nil, err
		}
		if isFrame(n) {
			return nil, opErr("画框不能嵌套画框（%s）", id)
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out, nil
}

// lookup 把真实 ID 或批内引用解析成节点。
func (st *state) lookup(ref string) (string, map[string]any, error) {
	if ref == "" {
		return "", nil, opErr("缺少节点 ID")
	}
	id := ref
	if strings.HasPrefix(ref, RefPrefix) {
		real, ok := st.refs[ref]
		if !ok {
			return "", nil, opErr("批内引用 %s 未定义（只能引用本批中之前创建的节点）", ref)
		}
		id = real
	}
	n, ok := st.byID[id]
	if !ok {
		return "", nil, fmt.Errorf("%w: %s", ErrNodeNotFound, id)
	}
	return id, n, nil
}

// owned 解析节点并校验所有权：Agent / MCP 只能改**本主体创建且用户之后没动过**的节点。
func (st *state) owned(ref string) (string, map[string]any, error) {
	id, n, err := st.lookup(ref)
	if err != nil {
		return "", nil, err
	}
	if st.actor.Kind == ActorUser {
		return id, n, nil
	}
	if !OwnedByActor(n, st.actor) {
		return "", nil, fmt.Errorf("%w: %s", ErrNotOwned, id)
	}
	return id, n, nil
}

// OriginKey 是节点顶层的新来源字段（MCPT-09）：`origin:{kind:"mcp", grantId, clientName, userTouched?}`。
// 旧的 `agent:{sessionId,…}` 仍然有效（Agent 继续写它），读取一律经 NodeOrigin。
const OriginKey = "origin"

// Origin 是从节点上解析出来的来源主体。
type Origin struct {
	// Kind 是 agent / mcp；节点没有任何来源字段时 NodeOrigin 返回 ok=false。
	Kind       string
	SessionID  string
	GrantID    string
	ClientName string
	// UserTouched 是两处字段（agent.userTouched、origin.userTouched）**任一**为 true。
	UserTouched bool
}

// NodeOrigin 解析节点的来源（读路径**只有这一个函数**，前端对应 utils/canvasOrigin.ts nodeOrigin）。
//
//   - 有 `origin`（对象）以它为准；只有旧的 `agent`（对象）时视为 kind=agent。
//   - userTouched 两处取「或」：只读 origin 的话，一个同时带两个字段且旧字段被标过的节点
//     会被重新判成可改 —— 倒向保护用户。
//   - ID 按字符串比较，同时容忍历史上可能以数字写入的值。
func NodeOrigin(n map[string]any) (Origin, bool) {
	agent, hasAgent := n["agent"].(map[string]any)
	origin, hasOrigin := n[OriginKey].(map[string]any)
	var o Origin
	switch {
	case hasOrigin:
		o.Kind, _ = origin["kind"].(string)
		o.SessionID = idString(origin["sessionId"])
		o.GrantID = idString(origin["grantId"])
		o.ClientName, _ = origin["clientName"].(string)
	case hasAgent:
		o.Kind = ActorAgent
		o.SessionID = idString(agent["sessionId"])
	default:
		return Origin{}, false
	}
	if t, _ := agent["userTouched"].(bool); t {
		o.UserTouched = true
	}
	if t, _ := origin["userTouched"].(bool); t {
		o.UserTouched = true
	}
	return o, true
}

func idString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case json.Number:
		return x.String()
	case float64:
		if x == float64(int64(x)) {
			return strconv.FormatInt(int64(x), 10)
		}
	}
	return ""
}

// CreatedByActor 判断节点是否由这个主体创建（agent 比会话 ID、mcp 比授权 ID），**不看 userTouched**。
// 只给 link_generation 用：其余操作一律走 OwnedByActor。user 不是来源主体，恒为 false。
func CreatedByActor(n map[string]any, actor Actor) bool {
	o, ok := NodeOrigin(n)
	if !ok || o.Kind != actor.Kind {
		return false
	}
	switch actor.Kind {
	case ActorAgent:
		return actor.SessionID > 0 && o.SessionID == strconv.FormatInt(actor.SessionID, 10)
	case ActorMCP:
		return actor.GrantID > 0 && o.GrantID == strconv.FormatInt(actor.GrantID, 10)
	default:
		return false
	}
}

// OwnedByActor 判断节点是否归这个主体所有：来源主体一致且 userTouched != true。
// Agent 与 MCP 互不可改对方的节点（Kind 不同即不归属）。
func OwnedByActor(n map[string]any, actor Actor) bool {
	o, ok := NodeOrigin(n)
	if !ok || o.UserTouched {
		return false
	}
	return CreatedByActor(n, actor)
}

// CreatedBySession 判断节点是否由某个 Agent 会话创建（只看来源主体，**不看 userTouched**）。
// 是 CreatedByActor 的 agent 特例，保留给既有调用点。
func CreatedBySession(n map[string]any, sessionID int64) bool {
	return CreatedByActor(n, Actor{Kind: ActorAgent, SessionID: sessionID})
}

// OwnedBy 判断节点是否归某个 Agent 会话所有。是 OwnedByActor 的 agent 特例，保留给既有调用点。
func OwnedBy(n map[string]any, sessionID int64) bool {
	return OwnedByActor(n, Actor{Kind: ActorAgent, SessionID: sessionID})
}

func (st *state) nextOrder(target string) int64 {
	var max int64
	for _, raw := range st.edges {
		e, ok := raw.(map[string]any)
		if !ok || e["target"] != target {
			continue
		}
		if o, ok := intOf(e["order"]); ok && o > max {
			max = o
		}
	}
	return max + 1
}

// ---------------------------------------------------------------- 默认尺寸

// 画框的默认尺寸（设计建议值）。
const (
	DefaultFrameWidth  = 640
	DefaultFrameHeight = 400
	// defaultNodeHeight 与前端 document() 的兜底高度一致（未测量过的节点存 180）。
	defaultNodeHeight = 180
)

// mediaCollectionGroups 与 frontend/stores/canvas.ts 的 MEDIA_COLLECTION_GROUPS 一致。
var mediaCollectionGroups = map[string]bool{"asset": true, "storyboard_image": true, "video_generator": true}

// DefaultNodeSize 是新建节点没给 size 时的默认值。
//
// ⚠️ 与前端 `stores/canvas.ts nodeDefaultWidth` 是同一条规则：Agent 建的节点要和用户在
// 画布上点出来的长得一样（字段集合与默认值都一样），否则前端打开时会按自己的最小宽度
// 把它撑开，下一次自动保存就产生一个用户什么都没做的 diff。
// 共享 fixture `addnode-shape.json` 钉住两边一致。
func DefaultNodeSize(nodeType string, params map[string]any) (w, h int64) {
	h = defaultNodeHeight
	switch {
	case nodeType == "group":
		g, _ := params["groupType"].(string)
		if mediaCollectionGroups[g] {
			return 368, h
		}
		return 460, h
	case nodeType == "video" || nodeType == "script":
		return 390, h
	case nodeType == "director" || nodeType == "smartcut":
		// 3D 导演台（DRT-05，dev/02 §3.2）与智能剪辑（SCT-23）：卡片放一张 16:9 封面，高度比普通节点多一截。
		// ⚠️ 与前端 nodeDefaultHeight 同值，共享 fixture 钉住两边。
		return 330, 240
	default:
		return 330, h
	}
}

// ---------------------------------------------------------------- 小工具

func opErr(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrOpInvalid, fmt.Sprintf(format, args...))
}

func decode(raw []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if dec.More() {
		return nil, errors.New("多余的 JSON 内容")
	}
	return v, nil
}

func object(raw json.RawMessage, field string) (map[string]any, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, opErr("缺少 %s", field)
	}
	v, err := decode(raw)
	if err != nil {
		return nil, opErr("%s 不是合法 JSON", field)
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, opErr("%s 必须是对象", field)
	}
	return m, nil
}

// marshal 序列化且**不转义 HTML 字符**：标题与提示词里的 `<` `&` 原样保留。
func marshal(v any) (json.RawMessage, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return json.RawMessage(bytes.TrimRight(buf.Bytes(), "\n")), nil
}

func mustMarshal(v any) json.RawMessage {
	raw, err := marshal(v)
	if err != nil {
		// 输入来自 decode 的结果，序列化不会失败；真失败说明这里写错了。
		panic(err)
	}
	return raw
}

// mergePatch 是 RFC 7386 JSON Merge Patch：对象逐键递归合并，null 删除该键，其余替换。
func mergePatch(target, patch any) any {
	pm, ok := patch.(map[string]any)
	if !ok {
		return patch
	}
	tm, ok := target.(map[string]any)
	if !ok {
		tm = map[string]any{}
	}
	for k, v := range pm {
		if v == nil {
			delete(tm, k)
			continue
		}
		tm[k] = mergePatch(tm[k], v)
	}
	return tm
}

func num(v int64) json.Number { return json.Number(strconv.FormatInt(v, 10)) }

func point(p Point) map[string]any {
	return map[string]any{
		"x": json.Number(strconv.FormatFloat(p.X, 'f', -1, 64)),
		"y": json.Number(strconv.FormatFloat(p.Y, 'f', -1, 64)),
	}
}

func isNumber(v any) bool {
	_, ok := v.(json.Number)
	return ok
}

func intOf(v any) (int64, bool) {
	switch n := v.(type) {
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return i, true
		}
		f, err := n.Float64()
		if err != nil || f != float64(int64(f)) {
			return 0, false
		}
		return int64(f), true
	case float64:
		if n != float64(int64(n)) {
			return 0, false
		}
		return int64(n), true
	default:
		return 0, false
	}
}

func isFrame(n map[string]any) bool {
	t, _ := n["type"].(string)
	return t == FrameNodeType
}
