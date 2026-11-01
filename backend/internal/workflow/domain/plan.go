package domain

import (
	"encoding/json"
	"fmt"
)

// 执行计划（NEW-02）：跑哪些、复用哪些、跳过哪些、哪些跑不了、一共多少钱。

// Action 是计划给某个节点的处置。
type Action string

const (
	// ActionRun 要真跑，要扣费。
	ActionRun Action = "run"
	// ActionReuse 已有成功产出且设置没变 —— 直接用，**不扣费**（NEW-05）。
	ActionReuse Action = "reuse"
	// ActionSkip 这个节点本来就不该跑（素材节点）。
	//
	// ⚠️ skip 与 blocked 必须分开：skip 是"不需要跑"，blocked 是"想跑跑不了"。
	// 混成一个状态的话，界面无法区分"这条风格线是正常的"和
	// "这一支因为上游失败瘫了"。
	ActionSkip Action = "skip"
	// ActionBlocked 依赖的上游产不出东西，这个节点跑不了（NEW-06）。
	ActionBlocked Action = "blocked"
)

// PlanNode 是计划里的一个节点。
type PlanNode struct {
	ID     string
	Kind   NodeKind
	Action Action
	// Reason 一定要能回答"为什么是这个处置"。
	Reason string
	// Operation / ModelCode 供前端显示"这一步会用哪个模型"。
	Operation string
	ModelCode string
	// Credits 是这个节点的预估费用；reuse/skip/blocked 一律 0。
	Credits int
	// EstimatedSeconds 预估耗时，用于整体预估。
	EstimatedSeconds int
	// Level 是并行层号（从 0 起）。同层可并发。
	Level int
	// DependsOn 本次运行内的直接上游（已过滤掉不在子图里的）。
	DependsOn []string
	// Blockers 让这个节点跑不了的具体原因（可执行性检查的结果）。
	Blockers []string
	// Params / Inputs 是**执行时真正要用的**参数与输入。
	//
	// ⚠️ 它们随计划一起存进运行快照（NEW-03）：运行期间用户改画布，
	// 后半段按新图跑、前半段按旧图跑的话，产出对不上，事后也复现不了。
	// 快照是"这次运行到底执行了什么"的唯一答案。
	Params json.RawMessage
	Inputs json.RawMessage
	// Fingerprint 是"这次要用的设置"的指纹，受理时存进 generation。
	Fingerprint string
	// ReuseGenerationID 是被复用的那次产出（action=reuse 时非 0）。
	//
	// ⚠️ 下游要拿它当输入：复用的节点没有新任务，不把这个 ID 记下来，
	// 下游就找不到"上游的产出在哪"，一律受理失败。
	ReuseGenerationID int64
	// StaticMediaID 是静态素材节点的媒体（DRD-19），随计划进运行快照：派发时按它改写下游输入，
	// **不重读画布** —— 运行中途重新截图不影响这一次运行（NEW-03）。
	StaticMediaID int64 `json:",omitempty"`
}

// Plan 是一次运行前预览。
type Plan struct {
	// Issues 非空时**整个计划不可执行**（NEW-01：校验失败不调用模型、不扣分）。
	Issues []Issue
	Levels [][]string
	Nodes  []PlanNode
	// RunCount / ReuseCount / SkipCount / BlockedCount 供一句话摘要。
	RunCount     int
	ReuseCount   int
	SkipCount    int
	BlockedCount int
	// TotalCredits 只累计 ActionRun 的节点。
	TotalCredits int
	// EstimatedSeconds 是**按层取最大值再相加**的关键路径估算，
	// 不是所有节点耗时之和 —— 同层并行，加起来会把预估吹成好几倍。
	EstimatedSeconds int
}

// Executable 判断这个计划能不能提交执行。
//
// ⚠️ 有 Issue 就一律不可执行；一个 run 都没有也不可执行 ——
// 让用户点一个什么都不会发生的按钮，比禁用它更糟。
func (p Plan) Executable() bool { return len(p.Issues) == 0 && p.RunCount > 0 }

// Quote 是某个节点的报价（由目录域算，计划器只接收）。
type Quote struct {
	Credits          int
	EstimatedSeconds int
	// Blockers 是这个节点**不可调用的具体原因**（模型停用、没配价、
	// 会员等级不够、协议不支持该操作…）。
	//
	// ⚠️ 要给具体原因而不是一个 bool：可调用能力是一个合取式，
	// 只说"不可用"的话用户不知道该去后台改哪一项。
	Blockers []string
}

// PlanInput 是一次计划请求。
type PlanInput struct {
	Graph Graph
	// Targets 为空表示整图。
	Targets []string
	// Quotes 按节点 ID 给出报价。缺失表示该节点报不了价。
	Quotes map[string]Quote
	// Force 为真时连"可复用"的也重跑（用户显式要求重新生成）。
	Force bool
}

// BuildPlan 生成执行计划。
//
// 顺序是固定的：**先校验 → 再选子图 → 再分层 → 最后逐层定处置**。
// 前三步任何一步出问题都不会走到报价，也就不可能扣费（NEW-01）。
func BuildPlan(in PlanInput) Plan {
	plan := Plan{Issues: Validate(in.Graph, in.Targets)}
	if len(plan.Issues) > 0 {
		return plan
	}

	include := Ancestors(in.Graph, in.Targets)
	levels := TopoLevels(in.Graph, include)
	if levels == nil {
		// 兜底：Validate 应该已经报过环了，走到这里说明两者不一致。
		plan.Issues = append(plan.Issues, Issue{Code: IssueCycle,
			Message: "无法确定执行顺序（图中存在环）"})
		return plan
	}
	plan.Levels = levels

	idx := in.Graph.index()
	inEdges, _ := in.Graph.adjacency()
	decided := make(map[string]Action, len(include))
	nodes := make(map[string]PlanNode, len(include))

	for lvl, layer := range levels {
		for _, id := range layer {
			n := idx[id]
			pn := PlanNode{
				ID: id, Kind: n.Kind, Level: lvl,
				Operation: n.Operation, ModelCode: n.ModelCode,
				Params: n.Params, Inputs: n.Inputs, Fingerprint: n.Fingerprint,
			}
			for _, up := range inEdges[id] {
				if include[up] {
					pn.DependsOn = append(pn.DependsOn, up)
				}
			}

			switch {
			case IsMaterial(n.Kind):
				// ⚠️ 素材节点永远 skip，且**不阻塞下游**：它没有产出，
				// 只在下游生成时贡献提示词。按"上游没成功就阻塞"处理的话，
				// 任何挂了风格节点的画布都跑不起来。
				pn.Action, pn.Reason = ActionSkip, "素材节点没有独立产出，只为下游贡献提示词"

			case IsLiteralInput(n):
				// ⚠️ 同理：没配模型的文本节点是用户手打的提示词，
				// 它是下游的输入而不是一个待执行的任务。
				pn.Action, pn.Reason = ActionSkip, "静态文本输入，作为下游的提示词使用"

			case IsStaticMedia(n):
				// ⚠️ 静态素材（导演台截图 / 发布出来的截图节点，DRD-19）：产出就是那张图，0 积分、
				// 对下游算满足。排在「上游被卡」与「无操作」之前：导演台的入边只是全景背景的来源，
				// 而它本来就没有操作码；发布节点即使配了模型也不重跑（重跑会用模型的图顶掉截图）。
				pn.Action, pn.Reason = ActionSkip, staticReason(n.StaticKind)
				pn.StaticMediaID = n.StaticMediaID

			case n.Kind == KindDirector:
				// 导演台还没有截图：下游要的那张图根本不存在，这一支跑不了。
				pn.Action, pn.Reason = ActionBlocked, "导演台还没有截图"
				pn.Blockers = []string{"没有截图"}

			case n.Kind == KindSmartcut && n.StaticStale:
				// 导出之后剪辑又改过（SC-F-154）：拿旧成片去跑下游，出来的东西与用户眼前的剪辑对不上，
				// 而钱已经花了。整图不替他重新导出（SCK-17），说清原因让他去导出。
				pn.Action, pn.Reason = ActionBlocked, "剪辑已改，成片未更新"
				pn.Blockers = []string{"成片未更新"}

			case n.Kind == KindSmartcut:
				pn.Action, pn.Reason = ActionBlocked, "智能剪辑还没有导出成片"
				pn.Blockers = []string{"没有成片"}

			case blockedByUpstream(pn.DependsOn, decided, idx):
				pn.Action = ActionBlocked
				pn.Reason = "上游没有产出：" + joinIDs(failedUpstreams(pn.DependsOn, decided, idx))

			case n.Operation == "":
				// 不是素材节点却没有 operation：用户还没选模式。
				// 这是**可修复**的配置问题，不是图的结构问题 ——
				// 所以是 blocked 而不是 Issue，其余分支照常预览。
				pn.Action, pn.Reason = ActionBlocked, "还没有选择生成方式"
				pn.Blockers = []string{"未选择操作"}

			case !in.Force && canReuse(n):
				pn.Action = ActionReuse
				pn.Reason = "已有成功产出且参数与输入都没变，直接复用（不扣费）"
				pn.ReuseGenerationID = n.ResultGenerationID

			default:
				q, ok := in.Quotes[id]
				if !ok {
					pn.Action, pn.Reason = ActionBlocked, "报不出价：模型或操作未配置"
					pn.Blockers = []string{"缺少报价"}
					break
				}
				if len(q.Blockers) > 0 {
					pn.Action, pn.Reason = ActionBlocked, "当前不可调用"
					pn.Blockers = q.Blockers
					break
				}
				pn.Action = ActionRun
				pn.Reason = reasonForRun(n, in.Force)
				pn.Credits = q.Credits
				pn.EstimatedSeconds = q.EstimatedSeconds
			}

			decided[id] = pn.Action
			nodes[id] = pn
		}
	}

	// 按层序输出，层内按 ID —— 顺序稳定，界面才不跳。
	for _, layer := range levels {
		for _, id := range layer {
			pn := nodes[id]
			plan.Nodes = append(plan.Nodes, pn)
			switch pn.Action {
			case ActionRun:
				plan.RunCount++
				plan.TotalCredits += pn.Credits
			case ActionReuse:
				plan.ReuseCount++
			case ActionSkip:
				plan.SkipCount++
			case ActionBlocked:
				plan.BlockedCount++
			}
		}
	}

	// ⚠️ 关键路径估算：**每层取最大值**再相加。所有节点耗时之和会把
	// 一张两条并行支线的图吹成两倍时间，用户照着那个数去安排等待。
	for _, layer := range levels {
		max := 0
		for _, id := range layer {
			if s := nodes[id].EstimatedSeconds; s > max {
				max = s
			}
		}
		plan.EstimatedSeconds += max
	}
	return plan
}

// canReuse 判断能不能直接复用上次的产出（NEW-05）。
//
// ⚠️ 必须**同时**满足"有成功产出"与"指纹一致"。只看有没有产出的话，
// 用户改完提示词再点运行，拿回来的还是旧图 —— 而界面显示"已完成"。
// ⚠️ 指纹为空（老数据、没记过）一律**不复用**：宁可多跑一次，
// 也不能把一张对不上的图当成本次结果。
func canReuse(n Node) bool {
	if !n.HasSuccess || n.ResultFingerprint == "" || n.Fingerprint == "" {
		return false
	}
	return n.ResultFingerprint == n.Fingerprint
}

func reasonForRun(n Node, force bool) string {
	switch {
	case force && n.HasSuccess:
		return "已有产出，但本次要求重新生成"
	case n.HasSuccess:
		return "参数或输入变了，需要重新生成"
	default:
		return "尚无产出"
	}
}

// blockedByUpstream 判断是否被上游卡住。
//
// ⚠️ **素材上游不算**：它天然是 skip，把它算进来等于让每张带风格节点的
// 画布都 blocked。这是这段逻辑唯一容易写错的地方。
func blockedByUpstream(deps []string, decided map[string]Action, idx map[string]Node) bool {
	return len(failedUpstreams(deps, decided, idx)) > 0
}

func failedUpstreams(deps []string, decided map[string]Action, idx map[string]Node) []string {
	var out []string
	for _, up := range deps {
		// 素材、静态文本、静态素材本来就不跑，不能算作"上游没产出"。
		// （静态素材已经判成 skip，这里再显式排除一次：处置的顺序将来被调整时，它也不会被误算成 blocked。）
		if IsMaterial(idx[up].Kind) || IsLiteralInput(idx[up]) || IsStaticMedia(idx[up]) {
			continue
		}
		if decided[up] == ActionBlocked {
			out = append(out, up)
		}
	}
	return out
}

// staticReason 说明静态素材节点为什么不跑：发布的是预演视频时说视频，其余都是截图（含控制图）。
func staticReason(kind string) string {
	switch kind {
	case "preview":
		return "静态素材：导演台预演视频"
	case "smartcut":
		return "智能剪辑已导出，直接使用成片"
	case "smartcut_output":
		return "静态素材：智能剪辑成片"
	}
	return "静态素材：导演台截图"
}

// Summary 给一句话摘要，供按钮旁显示。
//
// ⚠️ 复用与跳过的条数**要说出来**：用户选了 12 个节点只扣 3 个的钱，
// 不解释的话他会以为漏跑了，然后去点"强制重新生成" —— 那才是真花钱。
func (p Plan) Summary() string {
	if len(p.Issues) > 0 {
		return fmt.Sprintf("有 %d 处问题需要先修复", len(p.Issues))
	}
	s := fmt.Sprintf("将执行 %d 个节点，预计消耗 %d 积分", p.RunCount, p.TotalCredits)
	if p.ReuseCount > 0 {
		s += fmt.Sprintf("；%d 个可直接复用（不扣费）", p.ReuseCount)
	}
	if p.SkipCount > 0 {
		s += fmt.Sprintf("；%d 个节点无需执行（素材、静态文本与导演台截图）", p.SkipCount)
	}
	if p.BlockedCount > 0 {
		s += fmt.Sprintf("；%d 个暂时跑不了", p.BlockedCount)
	}
	if p.EstimatedSeconds > 0 {
		s += fmt.Sprintf("，预计约 %d 秒", p.EstimatedSeconds)
	}
	return s
}
