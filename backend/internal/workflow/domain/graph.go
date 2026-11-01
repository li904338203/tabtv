// Package domain 是整图 DAG 的领域层（NEW-01~NEW-06）。
//
// ⚠️ 这一层**不碰数据库、不发网络**：图的形状、能不能跑、跑哪些、
// 哪些能复用，全是纯计算。校验失败一律在这里就拦下 ——
// NEW-01 要求「校验失败不调用模型或扣分」，而唯一能保证这一点的做法
// 就是让校验发生在任何副作用之前。
package domain

import (
	"encoding/json"
	"fmt"
	"sort"
)

// NodeKind 是节点大类。
type NodeKind string

const (
	KindText   NodeKind = "text"
	KindImage  NodeKind = "image"
	KindVideo  NodeKind = "video"
	KindAudio  NodeKind = "audio"
	KindScript NodeKind = "script"
	KindGroup  NodeKind = "group"
	// KindStyle / KindEffect 是**素材节点**：它们没有自己的产出，
	// 只在下游生成时贡献提示词。
	KindStyle  NodeKind = "style"
	KindEffect NodeKind = "effect"
	// KindDirector 是 3D 导演台节点（docs/agent/director）：没有生成操作，产出是用户在导演台里截下来的图。
	KindDirector NodeKind = "director"
	// KindSmartcut 是智能剪辑节点（docs/agent/smartcut SCT-09）：没有生成操作，产出是导出的成片。
	// 整图里不自动剪、不自动导出（SCK-17）：导出过且没过期的当静态素材，否则 blocked 并说清原因。
	KindSmartcut NodeKind = "smartcut"
)

// IsMaterial 判断是不是素材节点。
//
// ⚠️ 这个判断是整个调度里最容易漏的一条。素材节点**没有 operation**，
// 永远跑不出东西 —— 但它有出边。把"上游没成功就阻塞下游"这条规则
// 一视同仁地套上去的话，**任何挂了风格节点的画布都跑不起来**，
// 而用户完全看不出为什么（那个节点本来就不该跑）。
func IsMaterial(k NodeKind) bool { return k == KindStyle || k == KindEffect }

// IsLiteralInput 判断这是不是一个**静态文本输入**节点。
//
// ⚠️ 文本节点有两种用法：配了模型的会真跑（text2text / 图片反推），
// **没配模型的就是用户手打的一段提示词** —— 它是下游的输入，本来就不该跑。
// 前端的媒体计数也正是这么算的（`text` 源不计入媒体数量）。
//
// ⚠️ 不认这一条的后果：**每一张带手打提示词的画布都跑不起来** ——
// 那个文本节点因为"没有模型"被判 blocked，下游跟着 blocked，
// 而用户完全看不出为什么（他那里本来就没什么要生成的）。
//
// ⚠️ 只对文本成立：图片/视频/音频节点没有模型就是真的产不出东西，
// 那是配置没做完，必须 blocked 而不是悄悄跳过 ——
// 跳过的话下游拿不到媒体，跑到一半才失败，而钱已经花在上游了。
func IsLiteralInput(n Node) bool {
	return n.Kind == KindText && n.ModelCode == ""
}

// Node 是计划器需要的节点信息。
type Node struct {
	ID   string
	Kind NodeKind
	// Operation 为空表示这个节点不可执行（素材节点，或还没选模式）。
	Operation string
	ModelCode string
	// HasSuccess 表示该节点已有一次成功产出。
	HasSuccess bool
	// Fingerprint 是当前参数与输入的指纹。与 ResultFingerprint 一致
	// 才说明"那次产出还对得上现在的设置"。
	Fingerprint string
	// ResultFingerprint 是上次成功产出时的指纹（没有产出时为空）。
	ResultFingerprint string
	// ResultGenerationID 是上次成功产出的任务 ID（复用时下游要引用它）。
	ResultGenerationID int64
	// Params / Inputs 原样带着走，进运行快照（见 PlanNode 的注释）。
	Params json.RawMessage
	Inputs json.RawMessage
	// StaticMediaID 非 0 表示这是一个**静态素材节点**（DRD-19）：它的产出就是这个媒体，永远不跑。
	// 导演台取 `params.outputMediaId`（输出机位的最新截图），发布到画布的截图 / 预演节点取
	// `params.directorSource.mediaId`。
	StaticMediaID int64
	// StaticKind 是静态素材的来源种类（capture / depth / pose / preview / smartcut / smartcut_output），只用于说明文字。
	StaticKind string
	// StaticStale 智能剪辑导出过、但之后剪辑又改过（SC-F-154）：成片过期，不能当静态素材用。
	// 这时 StaticMediaID 为 0 —— 过期的成片不能让下游以为输入已经就绪。
	StaticStale bool
}

// IsStaticMedia 判断是不是静态素材节点（DRD-19）。
//
// ⚠️ 它与素材节点（IsMaterial）一样 skip、不阻塞下游，但**有产出**：下游拿到的是那张截图。
// 不认这一条的后果有两种，都花用户的钱：当普通图片节点处理的话，它被判「没有模型」blocked，
// 下游跟着 blocked；或者它配了模型，被重新生成一次 —— 截图被一张模型画的图顶掉。
func IsStaticMedia(n Node) bool { return n.StaticMediaID != 0 }

// Edge 是一条连线。Order 决定首尾帧与参考图的锚定顺序。
type Edge struct {
	ID     string
	Source string
	Target string
	Order  int
}

// Graph 是一张待执行的画布。
type Graph struct {
	Nodes []Node
	Edges []Edge
}

func (g Graph) index() map[string]Node {
	m := make(map[string]Node, len(g.Nodes))
	for _, n := range g.Nodes {
		m[n.ID] = n
	}
	return m
}

// incoming / outgoing 邻接表（按 order 排序，顺序即语义）。
func (g Graph) adjacency() (in, out map[string][]string) {
	in, out = map[string][]string{}, map[string][]string{}
	edges := append([]Edge(nil), g.Edges...)
	sort.SliceStable(edges, func(i, j int) bool { return edges[i].Order < edges[j].Order })
	for _, e := range edges {
		in[e.Target] = append(in[e.Target], e.Source)
		out[e.Source] = append(out[e.Source], e.Target)
	}
	return in, out
}

// ---------------------------------------------------------------- 校验

// IssueCode 是校验问题的种类。
type IssueCode string

const (
	IssueCycle       IssueCode = "cycle"
	IssueDangling    IssueCode = "dangling_edge"
	IssueNoOperation IssueCode = "no_operation"
	IssueNoModel     IssueCode = "no_model"
	IssueUnknownNode IssueCode = "unknown_node"
	IssueEmptyTarget IssueCode = "empty_target"
)

// Issue 是一条校验问题。
type Issue struct {
	Code IssueCode
	// Nodes 是**具体涉及哪些节点**。
	//
	// ⚠️ 只说"图里有环"在 100 个节点的画布上等于没说 ——
	// 用户没有任何办法找到它。必须把环上的节点原样列出来，
	// 前端据此高亮。
	Nodes   []string
	Message string
}

// Validate 做整图静态校验（NEW-01）。
//
// ⚠️ 全部问题**一次报完**，不是遇到第一个就返回：用户改一个跑一次、
// 再报下一个，十个问题要来回十趟。
func Validate(g Graph, targets []string) []Issue {
	var out []Issue
	idx := g.index()

	// 悬空边：保存时已经拦过一道（canvas/domain.ValidateCanvas），
	// 这里再拦是因为**计划器可能拿到的是运行快照**，
	// 而快照是历史数据，不保证还满足今天的保存规则。
	for _, e := range g.Edges {
		if _, ok := idx[e.Source]; !ok {
			out = append(out, Issue{Code: IssueDangling, Nodes: []string{e.Source},
				Message: fmt.Sprintf("连线 %s 的起点节点已不存在", e.ID)})
		}
		if _, ok := idx[e.Target]; !ok {
			out = append(out, Issue{Code: IssueDangling, Nodes: []string{e.Target},
				Message: fmt.Sprintf("连线 %s 的终点节点已不存在", e.ID)})
		}
	}

	for _, id := range targets {
		if _, ok := idx[id]; !ok {
			out = append(out, Issue{Code: IssueUnknownNode, Nodes: []string{id},
				Message: fmt.Sprintf("指定的执行目标 %s 不在这张画布上", id)})
		}
	}

	for _, c := range DetectCycles(g) {
		out = append(out, Issue{Code: IssueCycle, Nodes: c,
			Message: "这些节点构成了一个环，无法确定执行顺序：" + joinIDs(c)})
	}
	return out
}

// DetectCycles 找出图中的环，返回**环上的节点**。
//
// ⚠️ 返回节点列表而不是 bool：见 Issue.Nodes 的注释。
// ⚠️ 一张图里可能有**多个**独立的环，全部找出来 ——
// 只报第一个的话用户修完再跑，又冒出来一个。
func DetectCycles(g Graph) [][]string {
	_, out := g.adjacency()
	idx := g.index()

	const (
		white = 0 // 没访问过
		gray  = 1 // 在当前这条路径上
		black = 2 // 已经走完
	)
	color := map[string]int{}
	var stack []string
	var cycles [][]string
	seen := map[string]bool{} // 同一个环别报两次

	var dfs func(id string)
	dfs = func(id string) {
		color[id] = gray
		stack = append(stack, id)
		for _, next := range out[id] {
			if _, ok := idx[next]; !ok {
				continue // 悬空边单独报，不在这里重复
			}
			switch color[next] {
			case white:
				dfs(next)
			case gray:
				// 找到回边：从栈里截出这个环。
				for i := len(stack) - 1; i >= 0; i-- {
					if stack[i] == next {
						cycle := append([]string(nil), stack[i:]...)
						key := cycleKey(cycle)
						if !seen[key] {
							seen[key] = true
							cycles = append(cycles, cycle)
						}
						break
					}
				}
			}
		}
		stack = stack[:len(stack)-1]
		color[id] = black
	}

	// 按 ID 排序遍历：不排的话 map 顺序每次不同，
	// 同一张图报出来的环顺序会随机变，测试与界面都跳。
	ids := make([]string, 0, len(g.Nodes))
	for _, n := range g.Nodes {
		ids = append(ids, n.ID)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if color[id] == white {
			dfs(id)
		}
	}
	return cycles
}

// cycleKey 让同一个环（起点不同但节点相同）只报一次。
func cycleKey(c []string) string {
	s := append([]string(nil), c...)
	sort.Strings(s)
	return joinIDs(s)
}

func joinIDs(ids []string) string {
	out := ""
	for i, id := range ids {
		if i > 0 {
			out += " → "
		}
		out += id
	}
	return out
}

// ---------------------------------------------------------------- 子图

// Ancestors 求目标节点的**祖先闭包**（含目标自身）。
//
// ⚠️ 是祖先不是后代：要产出 X，必须先跑完 X 依赖的一切。
// 取后代的话跑的是"X 之后的东西"，而 X 自己还没有产出。
//
// targets 为空表示整图。
//
// ⚠️ 走到**冻结的来源**就停（frozenSource：静态素材、导演台、智能剪辑）：它们的产出是用户截下来的图或导出的成片，
// 这次运行里跑不跑它们的上游都改变不了它（整图不替用户截图、不替用户重新导出，SCK-17）。把那些上游拉进子图
// 只会让「只跑这一个节点」多扣一串钱 —— 智能剪辑的上游重新生成之后，它还会平白变成「素材有变化」（SCT-42 实测：
// 只点下游的一个视频节点，整条素材链 5 个生成任务跟着重跑）。整图（targets 为空）不受影响。
func Ancestors(g Graph, targets []string) map[string]bool {
	if len(targets) == 0 {
		all := make(map[string]bool, len(g.Nodes))
		for _, n := range g.Nodes {
			all[n.ID] = true
		}
		return all
	}
	in, _ := g.adjacency()
	idx := g.index()
	out := map[string]bool{}

	var walk func(id string)
	walk = func(id string) {
		if out[id] {
			return
		}
		if _, ok := idx[id]; !ok {
			return
		}
		out[id] = true
		if frozenSource(idx[id]) {
			return
		}
		for _, up := range in[id] {
			walk(up)
		}
	}
	for _, t := range targets {
		walk(t)
	}
	return out
}

// frozenSource 产出不由这次运行决定的节点：静态素材（发布出来的截图 / 成片）、导演台、智能剪辑。
// 导演台还没截图、智能剪辑还没导出（或已过期）时同样算：它们照样是 blocked，跑它们的上游也补不出那张图 / 那条成片。
func frozenSource(n Node) bool {
	return IsStaticMedia(n) || n.Kind == KindDirector || n.Kind == KindSmartcut
}

// TopoLevels 把子图按依赖分层：同一层内的节点互不依赖，可以并行。
//
// ⚠️ 分层而不是给一条线性序：NEW-04 要求"独立分支可并行"。
// 给线性序的话调度器只能一个一个跑，一张两条支线的图要跑两倍时间。
//
// 有环时返回 nil —— 调用方应当已经用 Validate 拦下了。
func TopoLevels(g Graph, include map[string]bool) [][]string {
	in, out := g.adjacency()
	idx := g.index()

	indeg := map[string]int{}
	for id := range include {
		if _, ok := idx[id]; !ok {
			continue
		}
		n := 0
		for _, up := range in[id] {
			if include[up] {
				n++
			}
		}
		indeg[id] = n
	}

	var levels [][]string
	remaining := len(indeg)
	for remaining > 0 {
		var layer []string
		for id, d := range indeg {
			if d == 0 {
				layer = append(layer, id)
			}
		}
		if len(layer) == 0 {
			return nil // 有环
		}
		// 层内按 ID 排序：顺序稳定，测试与界面才不跳。
		sort.Strings(layer)
		for _, id := range layer {
			delete(indeg, id)
			remaining--
			for _, down := range out[id] {
				if _, ok := indeg[down]; ok {
					indeg[down]--
				}
			}
		}
		levels = append(levels, layer)
	}
	return levels
}
