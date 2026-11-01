// Package app 是整图 DAG 的应用层：取画布、报价、组装执行计划（P08-01）。
package app

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sort"
	"strconv"

	catalogdomain "github.com/li904338203/tabtv/backend/internal/catalog/domain"
	imagetooldomain "github.com/li904338203/tabtv/backend/internal/imagetool/domain"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
	"github.com/li904338203/tabtv/backend/internal/workflow/domain"
)

// ---------------------------------------------------------------- 端口

// CanvasSource 由画布域实现：取项目的画布数据（**带归属校验**）。
//
// ⚠️ 归属校验在这一侧做，计划器拿到的已经是"这个工作区有权看的画布" ——
// 不然整图执行就成了一个用 projectId 探测别人画布的接口（GO-27）。
type CanvasSource interface {
	CanvasOf(ctx context.Context, workspaceID, projectID int64) (json.RawMessage, error)
}

// OperationResolver 由目录域实现：按连线规则判定操作码。
//
// ⚠️ 复用目录域**给连线校验用的那一个** `ResolveOperation`，不另起一套：
// 另起的话会出现"连线时说这条能走 ref2video、执行时按 img2video 跑"，
// 而两边各自都"没错"。视频节点的 mode 覆盖也由它统一处理。
type OperationResolver interface {
	ResolveOperation(ctx context.Context, targetType, mode string,
		sourceTypes []string) (string, error)
}

// QuoteAsk 是一次节点报价请求。
type QuoteAsk struct {
	ModelCode     string
	OperationCode string
	Count         int
	DurationSec   int
	Text          string
	MemberLevelID int64
	RoleCounts    map[string]int
}

// Quoter 由目录域实现：算价并给出**不可调用的具体原因**。
type Quoter interface {
	QuoteNode(ctx context.Context, ask QuoteAsk) (domain.Quote, error)
}

// NodeResult 是某个节点最近一次成功产出的信息。
type NodeResult struct {
	GenerationID int64
	// Fingerprint 由 **domain.Fingerprint** 从那次任务的快照算出。
	//
	// ⚠️ 实现方必须调用同一个函数，不能"照着实现一遍"：
	// 两份实现永远对不上，于是每次运行都把所有节点重跑一遍 ——
	// 用户每跑一次图就多花一次钱，而系统没有任何报错。
	Fingerprint string
}

// ResultLookup 由生成域实现：取项目内每个节点最近一次成功任务的快照指纹。
type ResultLookup interface {
	LatestResults(ctx context.Context, projectID int64) (map[string]NodeResult, error)
}

// SmartcutOutputs 智能剪辑节点的成片（docs/agent/smartcut SCT-09）：按项目一次取全部节点。
//
// ⚠️ 成片**不写进节点 params**（SCK-13：导出在 Worker 里异步完成，写进节点的值会被画布的自动保存覆盖），
// 所以这里不读 params，问时间线域要「最近一次成功导出的成片、导出时的版本、当前版本」。
type SmartcutOutputs interface {
	Outputs(ctx context.Context, workspaceID, projectID int64) (map[string]SmartcutOutput, error)
}

// SmartcutOutput 一个智能剪辑节点的最近一次成功导出（MediaID 为 0 = 还没导出过）。
type SmartcutOutput struct {
	MediaID        int64
	RenderVersion  int
	CurrentVersion int
}

// ---------------------------------------------------------------- 服务

type Service struct {
	canvas  CanvasSource
	ops     OperationResolver
	quoter  Quoter
	results ResultLookup
	// 运行相关（P08-02）。
	runs      Runs
	nodeGen   NodeGenerator
	canceller GenerationCanceller
	nextID    func() int64
	log       *slog.Logger
	smartcuts SmartcutOutputs
}

func New() *Service { return &Service{} }

func (s *Service) SetCanvas(c CanvasSource)          { s.canvas = c }
func (s *Service) SetOperations(o OperationResolver) { s.ops = o }
func (s *Service) SetQuoter(q Quoter)                { s.quoter = q }
func (s *Service) SetResults(r ResultLookup)         { s.results = r }

// SetSmartcutOutputs 接上智能剪辑的成片查询（SCT-09）。为 nil 时智能剪辑节点一律按「还没有导出成片」处理。
func (s *Service) SetSmartcutOutputs(o SmartcutOutputs) { s.smartcuts = o }
func (s *Service) SetLogger(l *slog.Logger)             { s.log = l }

// PlanInput 是一次运行前预览请求。
type PlanInput struct {
	WorkspaceID   int64
	ProjectID     int64
	MemberLevelID int64
	// TargetNodeIDs 为空表示整图。
	TargetNodeIDs []string
	Force         bool
}

// Plan 生成执行计划（NEW-02）。
//
// ⚠️ 它**只读不写**：不建 run、不扣费、不发 MQ。用户点"预览"应当可以
// 反复点而不产生任何后果 —— 这也是 NEW-01「校验失败不调用模型或扣分」
// 能够成立的前提：预览与执行是两个动作，预览永远不花钱。
func (s *Service) Plan(ctx context.Context, in PlanInput) (domain.Plan, error) {
	if s.canvas == nil {
		return domain.Plan{}, httpx.Errf(errcode.Internal, "画布链路未接入")
	}
	raw, err := s.canvas.CanvasOf(ctx, in.WorkspaceID, in.ProjectID)
	if err != nil {
		return domain.Plan{}, err
	}

	// 智能剪辑的成片按项目一次取（不在解析循环里逐个查）。查不到时按「还没导出」处理：
	// 宁可这一支 blocked，也不能拿一份不知道是不是最新的成片去跑下游。
	var outputs map[string]SmartcutOutput
	if s.smartcuts != nil {
		if outputs, err = s.smartcuts.Outputs(ctx, in.WorkspaceID, in.ProjectID); err != nil {
			s.warn(ctx, "取智能剪辑成片失败，按未导出处理", "error", err)
			outputs = nil
		}
	}
	g, parsed, err := s.parseGraphWith(ctx, raw, outputs)
	if err != nil {
		return domain.Plan{}, err
	}

	// 复用判定要的"上次产出指纹"。查不到时按"没有产出"处理 ——
	// 宁可多跑一次，也不能把一张对不上的图当成本次结果。
	results := map[string]NodeResult{}
	if s.results != nil {
		if results, err = s.results.LatestResults(ctx, in.ProjectID); err != nil {
			s.warn(ctx, "取历史产出失败，本次全部按需重跑", "error", err)
			results = map[string]NodeResult{}
		}
	}
	for i := range g.Nodes {
		if r, ok := results[g.Nodes[i].ID]; ok {
			g.Nodes[i].HasSuccess = true
			g.Nodes[i].ResultFingerprint = r.Fingerprint
			g.Nodes[i].ResultGenerationID = r.GenerationID
		}
	}

	// ⚠️ 先跑一次校验：有结构性问题就**不报价**。
	// 报价本身不花钱，但它会去查模型与定价，在一张有环的图上毫无意义 ——
	// 更重要的是让"校验失败 → 什么都不做"成为代码里看得见的顺序（NEW-01）。
	if issues := domain.Validate(g, in.TargetNodeIDs); len(issues) > 0 {
		return domain.Plan{Issues: issues}, nil
	}

	include := domain.Ancestors(g, in.TargetNodeIDs)
	quotes := map[string]domain.Quote{}
	if s.quoter != nil {
		for _, node := range g.Nodes {
			if !include[node.ID] || domain.IsMaterial(node.Kind) || node.Operation == "" {
				continue
			}
			p := parsed[node.ID]
			ask := QuoteAsk{
				ModelCode: node.ModelCode, OperationCode: node.Operation,
				Count: p.count(), DurationSec: p.durationSec(), Text: p.text(),
				MemberLevelID: in.MemberLevelID, RoleCounts: p.roleCounts,
			}
			if imagetooldomain.ToolOf(p.paramsMap()) != "" {
				ask.Count = imagetooldomain.PlanEstimate(p.paramsMap())
			}
			q, err := s.quoter.QuoteNode(ctx, ask)
			if err != nil {
				// ⚠️ 单个节点报不出价**不中断整张图的预览**：
				// 其余节点照常给出计划，这一个显示为 blocked 并带原因。
				// 整体失败的话用户只看到一句"预览失败"，不知道是哪一个节点的问题。
				quotes[node.ID] = domain.Quote{Blockers: []string{errText(err)}}
				continue
			}
			quotes[node.ID] = q
		}
	}

	return domain.BuildPlan(domain.PlanInput{
		Graph: g, Targets: in.TargetNodeIDs, Quotes: quotes, Force: in.Force,
	}), nil
}

// ---------------------------------------------------------------- 画布解析

// parsedNode 是从画布 JSON 里读出来的、报价要用的那几项。
type parsedNode struct {
	params     json.RawMessage
	roleCounts map[string]int
}

func (p parsedNode) count() int {
	if v := p.numParam("count"); v > 0 {
		return v
	}
	return 1
}

func (p parsedNode) durationSec() int { return p.numParam("durationSec") }

// paramsMap 整份参数（解不开为空表）。
func (p parsedNode) paramsMap() map[string]any {
	var m map[string]any
	_ = json.Unmarshal(p.params, &m)
	return m
}

func (p parsedNode) text() string {
	if len(p.params) == 0 {
		return ""
	}
	var m map[string]any
	if json.Unmarshal(p.params, &m) != nil {
		return ""
	}
	s, _ := m["prompt"].(string)
	return s
}

func (p parsedNode) numParam(key string) int {
	if len(p.params) == 0 {
		return 0
	}
	var m map[string]any
	if json.Unmarshal(p.params, &m) != nil {
		return 0
	}
	if f, ok := m[key].(float64); ok {
		return int(f)
	}
	return 0
}

// canvasNode / canvasEdge 是画布 JSON 里我们**只读**的那几个字段。
//
// ⚠️ 只读不写：画布数据的所有权在前端，未知字段原样保留（GO-35）。
// 计划器解析它只是为了知道"要跑什么"，绝不回写。
type canvasNode struct {
	ID     string          `json:"id"`
	Type   string          `json:"type"`
	Params json.RawMessage `json:"params"`
}

type canvasEdge struct {
	ID     string `json:"id"`
	Source string `json:"source"`
	Target string `json:"target"`
	Order  int    `json:"order"`
}

// nonExecutableTypes 是画布上**根本不属于执行图**的节点类型（AGT-09、dev 01 §6.1）。
//
// 文档节点只是一份 Markdown 的引用，画框只是视觉容器 —— 两者都没有 operation。
// ⚠️ 不能交给 IsMaterial 那条「skip 但不阻塞下游」：那仍会让它们进计划、
// 走 operationFor 去问一个不存在的操作码，预览里多出一行「缺少操作」的 blocked，
// 而用户看不出一份文档为什么"跑不了"。所以在解析阶段就整体剔除，连带两端触及它们的连线。
var nonExecutableTypes = map[string]bool{"document": true, "frame": true}

func dropNonExecutable(nodes []canvasNode, edges []canvasEdge) ([]canvasNode, []canvasEdge) {
	dropped := map[string]bool{}
	keptNodes := nodes[:0:0]
	for _, n := range nodes {
		if nonExecutableTypes[n.Type] {
			dropped[n.ID] = true
			continue
		}
		keptNodes = append(keptNodes, n)
	}
	if len(dropped) == 0 {
		return nodes, edges
	}
	keptEdges := edges[:0:0]
	for _, e := range edges {
		if dropped[e.Source] || dropped[e.Target] {
			continue
		}
		keptEdges = append(keptEdges, e)
	}
	return keptNodes, keptEdges
}

// parseGraph 把画布 JSON 解析成计划器要的图（没有智能剪辑成片的信息时用）。
func (s *Service) parseGraph(ctx context.Context, raw json.RawMessage) (
	domain.Graph, map[string]parsedNode, error) {
	return s.parseGraphWith(ctx, raw, nil)
}

// parseGraphWith 同 parseGraph，带上智能剪辑节点的成片（按节点 ID）。
func (s *Service) parseGraphWith(ctx context.Context, raw json.RawMessage, smartcuts map[string]SmartcutOutput) (
	domain.Graph, map[string]parsedNode, error) {
	var tree struct {
		Nodes []canvasNode `json:"nodes"`
		Edges []canvasEdge `json:"edges"`
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &tree); err != nil {
			return domain.Graph{}, nil, httpx.Errf(errcode.InvalidArgument,
				"画布数据不是合法 JSON")
		}
	}
	tree.Nodes, tree.Edges = dropNonExecutable(tree.Nodes, tree.Edges)

	g := domain.Graph{
		Nodes: make([]domain.Node, 0, len(tree.Nodes)),
		Edges: make([]domain.Edge, 0, len(tree.Edges)),
	}
	parsed := make(map[string]parsedNode, len(tree.Nodes))
	kinds := make(map[string]string, len(tree.Nodes))

	for _, e := range tree.Edges {
		g.Edges = append(g.Edges, domain.Edge{
			ID: e.ID, Source: e.Source, Target: e.Target, Order: e.Order,
		})
	}
	static := map[string]int64{}
	for _, cn := range tree.Nodes {
		kinds[cn.ID] = cn.Type
		if id, _ := staticMediaOf(cn); id != 0 {
			static[cn.ID] = id
		}
		// 智能剪辑：导出过、且导出时的版本就是当前版本，才当静态素材（SC-F-154）。
		if domain.NodeKind(cn.Type) == domain.KindSmartcut {
			if out, ok := smartcuts[cn.ID]; ok && out.MediaID != 0 && out.RenderVersion >= out.CurrentVersion {
				static[cn.ID] = out.MediaID
			}
		}
	}

	// 入边按 order 归集：既用来定操作，也用来算指纹。
	inbound := map[string][]canvasEdge{}
	for _, e := range tree.Edges {
		inbound[e.Target] = append(inbound[e.Target], e)
	}
	for id := range inbound {
		sort.SliceStable(inbound[id], func(i, j int) bool {
			return inbound[id][i].Order < inbound[id][j].Order
		})
	}

	for _, cn := range tree.Nodes {
		kind := domain.NodeKind(cn.Type)
		node := domain.Node{ID: cn.ID, Kind: kind}
		node.StaticMediaID, node.StaticKind = staticMediaOf(cn)
		if kind == domain.KindSmartcut {
			out := smartcuts[cn.ID]
			node.StaticMediaID, node.StaticKind = static[cn.ID], "smartcut"
			node.StaticStale = node.StaticMediaID == 0 && out.MediaID != 0
		}
		p := parsedNode{params: cn.Params, roleCounts: map[string]int{}}

		// 导演台与智能剪辑没有生成操作（DRD-19、SCT-09）：不去问一个不存在的操作码，也不读模型。
		// 发布出来的静态素材同理（它永远 skip）：发布的成片节点没有 mode，去问的话每次预览都打一条「未知视频模式」告警。
		if !domain.IsMaterial(kind) && kind != domain.KindDirector && kind != domain.KindSmartcut &&
			node.StaticMediaID == 0 {
			node.Operation = s.operationFor(ctx, cn, kinds, inbound[cn.ID])
			node.ModelCode = nodeModelCode(kind, cn.Params)
		}

		// ⚠️ 角色计数由**服务端数**，不信客户端报的数：信它的话
		// 能力矩阵的 min/max 形同虚设，少报一张图就能绕过去。
		var refs []domain.InputRef
		var sources []string
		for _, e := range inbound[cn.ID] {
			upKind := kinds[e.Source]
			if domain.IsMaterial(domain.NodeKind(upKind)) {
				// 素材不算媒体数量，但连线本身是允许的。
				refs = append(refs, domain.InputRef{
					Order: e.Order, Kind: "material", Ref: e.Source})
				sources = append(sources, e.Source)
				continue
			}
			role := roleOf(domain.NodeKind(upKind))
			p.roleCounts[role]++
			ref := domain.InputRef{Order: e.Order, Kind: role, Ref: e.Source}
			if id := static[e.Source]; id != 0 {
				// ⚠️ 静态上游的指纹按**媒体**算，不按节点 ID（DR-F-97）：静态节点永远不「跑」，
				// 按节点 ID 算的话换了截图下游仍被判复用，用的是旧截图的产出。
				ref = domain.InputRef{Order: e.Order, Kind: "reference", Ref: "media:" + itoa(id)}
			}
			refs = append(refs, ref)
			sources = append(sources, e.Source)
		}
		node.Fingerprint = domain.Fingerprint(
			node.Operation, node.ModelCode, cn.Params, refs)
		// 把执行时要用的参数与输入一并带上 —— 它们会进运行快照（NEW-03）。
		node.Params = cn.Params
		if declared, err := json.Marshal(declaredFrom(refs, sources)); err == nil {
			node.Inputs = declared
		}

		g.Nodes = append(g.Nodes, node)
		parsed[cn.ID] = p
	}
	return g, parsed, nil
}

// operationFor 定这个节点的操作码。
//
// ⚠️ 优先级：**节点自己选的 mode/operation > 连线规则的默认值**。
// 连线规则给的只是"建议"，多义时（图→视频可以是图生视频、首尾帧、参考生视频）
// 必须以用户选的模式为准 —— 拿默认值去跑，出来的片子不是他要的那种，
// 而钱已经花了。
func (s *Service) operationFor(ctx context.Context, cn canvasNode,
	kinds map[string]string, inbound []canvasEdge) string {
	// 图片节点工具条的子节点（docs/agent/imagetools IMGD-3）：操作码只由 params.tool 定，与受理同一个函数 ——
	// 计划器按 img2img 报价、受理按多角度扣费的话，「预览说 4 积分、真跑扣 8」而两边各自都「没错」。
	var p map[string]any
	if json.Unmarshal(cn.Params, &p) == nil {
		if op := imagetooldomain.OperationOf(p); op != "" {
			return op
		}
	}
	if op := stringParam(cn.Params, "mode"); op != "" {
		return op
	}
	if op := stringParam(cn.Params, "operation"); op != "" {
		return op
	}
	if s.ops == nil {
		return ""
	}
	// ⚠️ 上游类型**按 order 顺序**传下去：规则表取"第一条有默认操作的入边"，
	// 顺序错了会在多入边的节点上判出另一个操作码。
	// 没有入边时按 text 源问（文生图/文生视频这类没有上游节点的情况）。
	sources := make([]string, 0, len(inbound))
	for _, e := range inbound {
		if k := kinds[e.Source]; !domain.IsMaterial(domain.NodeKind(k)) {
			sources = append(sources, k)
		}
	}
	if len(sources) == 0 {
		sources = []string{"text"}
	}
	op, err := s.ops.ResolveOperation(ctx, cn.Type, stringParam(cn.Params, "mode"), sources)
	if err != nil {
		// ⚠️ 判不出操作码不是错误，是"这个节点还没配好"：
		// 返回空串让计划器把它标成 blocked 并说明原因，其余分支照常预览。
		s.warn(ctx, "判定操作码失败", "nodeId", cn.ID, "error", err)
		return ""
	}
	return op
}

// declaredFrom 把解析出来的输入记成"来自哪个上游节点"。
//
// ⚠️ 这里**只记来源节点**，不决定它最终是什么形态（产出引用 / 字面量 /
// 素材码）：那取决于上游到底跑没跑、是不是静态文本，而那在算计划时
// 还不知道。形态统一在派发时决定（见 run.go 的 resolveInputs）——
// 两处都判的话，快照里写的和真正发出去的会不一致。
//
// ⚠️ 只带 ID 不带 URL（CD-24）：受理侧会自己按可信资源 ID 解析。
// sources 与 refs 一一对应，是每条输入的上游节点 ID（静态上游的 ref 是 `media:{id}`，不是节点 ID）。
func declaredFrom(refs []domain.InputRef, sources []string) []map[string]any {
	out := make([]map[string]any, 0, len(refs))
	for i, r := range refs {
		out = append(out, map[string]any{
			"order": r.Order, "sourceNodeId": sources[i], "kind": "generation",
		})
	}
	return out
}

// roleOf 把上游节点类型映射成能力矩阵里的输入角色。
//
// ⚠️ 节点类型 → 媒体角色只有一处映射（catalog/domain.MediaRoleOf，DRD-20）：连线校验、视频模式校验与
// 这里共用。各写一份的话会出现「连线时能连、整图执行时不认」—— 导演台连图片节点就是这样（导演台算 image）。
// 不是媒体的（文本等）按 text 算。
func roleOf(k domain.NodeKind) string {
	if role := catalogdomain.MediaRoleOf(string(k)); role != "" {
		return role
	}
	return "text"
}

// staticMediaOf 读静态素材节点的媒体（DRD-19）：导演台取 `params.outputMediaId`，
// 发布到画布的图片 / 视频节点取 `params.directorSource.mediaId`。第二个返回值是来源种类（只用于说明文字）。
//
// ⚠️ 发布节点凭 `params.directorSource` 判断，不凭 `result`：计划器只解析 params（`result` 是 UI 快照）。
// 转不成正整数的一律视为 0 —— 当成没有截图，而不是去引用一个不存在的媒体。
func staticMediaOf(cn canvasNode) (int64, string) {
	if len(cn.Params) == 0 {
		return 0, ""
	}
	var p struct {
		OutputMediaID  string `json:"outputMediaId"`
		DirectorSource *struct {
			Kind    string `json:"kind"`
			MediaID string `json:"mediaId"`
		} `json:"directorSource"`
		// SmartcutSource 智能剪辑把成片发布到画布的视频节点（SC-F-155）：键与前端发布时写的一致（SCR-11）。
		SmartcutSource *struct {
			MediaID string `json:"mediaId"`
		} `json:"smartcutSource"`
	}
	if json.Unmarshal(cn.Params, &p) != nil {
		return 0, ""
	}
	switch domain.NodeKind(cn.Type) {
	case domain.KindDirector:
		return positiveID(p.OutputMediaID), "capture"
	case domain.KindImage, domain.KindVideo:
		if p.DirectorSource != nil {
			if id := positiveID(p.DirectorSource.MediaID); id != 0 {
				return id, p.DirectorSource.Kind
			}
		}
		if domain.NodeKind(cn.Type) == domain.KindVideo && p.SmartcutSource != nil {
			if id := positiveID(p.SmartcutSource.MediaID); id != 0 {
				return id, "smartcut_output"
			}
		}
	}
	return 0, ""
}

func positiveID(s string) int64 {
	id, err := strconv.ParseInt(s, 10, 64)
	if err != nil || id <= 0 {
		return 0
	}
	return id
}

// nodeModelCode 取节点选中的模型。
//
// ⚠️ 画布节点的模型键是 `model`（用户端 ParamPanel、MCP canvas_apply、快速生成都写它），
// `modelCode` 只出现在组节点与早期手写的联调画布里。只认 `modelCode` 的话，用户真正画出来的
// 画布跑整图时**每个生成节点都被判成「该模型不支持操作」**（模型代码是空串），而联调脚本恰好
// 自己写的是 `modelCode`，一直是绿的（MCPT-11E 第 218 步实测）。
// 两个都有时以 `model` 为准：那是用户在面板上选的那一个。
//
// ⚠️ 文本节点**不**回落到 `model`：用户端面板一打开就会给节点自动选上第一个可用模型，
// 文本节点也不例外。认了它的话，用户手打的每一段词都会从「静态文本、跳过」（domain.IsStaticText）
// 变成「要执行、要扣费」的任务 —— 跑一次整图就把他写的词重新生成一遍。文本节点要不要执行
// 是另一个产品决定，这里维持原口径（只认显式的 `modelCode`）。
func nodeModelCode(kind domain.NodeKind, raw json.RawMessage) string {
	if kind == domain.KindText {
		return stringParam(raw, "modelCode")
	}
	if m := stringParam(raw, "model"); m != "" {
		return m
	}
	return stringParam(raw, "modelCode")
}

func stringParam(raw json.RawMessage, key string) string {
	if len(raw) == 0 {
		return ""
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return ""
	}
	s, _ := m[key].(string)
	return s
}

// errText 取给用户看的那句话。
//
// ⚠️ 用业务错误的 Message 而不是 err.Error()：后者可能是内部细节，
// 而这段文字会直接显示在"为什么这个节点跑不了"旁边。
func errText(err error) string {
	var be *httpx.BizError
	if errors.As(err, &be) && be.Message != "" {
		return be.Message
	}
	return err.Error()
}

func (s *Service) warn(ctx context.Context, msg string, args ...any) {
	if s.log != nil {
		s.log.WarnContext(ctx, msg, args...)
	}
}
