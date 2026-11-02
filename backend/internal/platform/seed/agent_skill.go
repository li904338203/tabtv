package seed

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// 首批 Agent 技能（AGT-30；AG-13 运行时加载部分、AG-PARAM-05）。
//
// ⚠️ 正文是**设计建议的占位文案，不是运营已批准的内容**（AG-PARAM-05 未定稿，CLAUDE.md §5.1）。
// 每个技能与附属文档的首行固定写「【待运营定稿】」；定稿走管理端（AGT-60），**不是改这里**。
//
// ⚠️ 幂等是 `on conflict do nothing`，**不是 do update**：运营在后台改过的正文，
// 再跑一次 `make seed` 就被这份占位文案盖回去，而没有任何人做过这个决定
// （同 seedMemberCatalog 不覆盖 is_enabled 的考量）。代价是这里改了文案不会自动下发到
// 已有环境 —— 那本来就该走后台。
//
// ⚠️ 正文里**只写创作要求与流程指引，不写格式契约**（design/01 §10、CLAUDE.md §5.7）：
// 卡片结构、计划项字段、规划文档骨架、单段提示词的分段版式都在代码里
// （agent/domain/prompt.go 与各工具的 Schema）。写进技能的话运营改一个字解析就挂，
// 而那要扣费之后才发现。
//
// ⚠️ 「付费前必须经用户确认」是代码规则（服务端不下发付费工具），技能里**不得**出现
// 「跳过确认」之类的说法 —— 写了也关不掉，只会让模型白白尝试。
//
// 渲染变量（必须与 agent/app.SkillVars 逐字一致；load_skill / read_skill_ref / Runner 共用那一份）：
//
//	{{state}}           当前会话状态（planning / executing …）
//	{{reasoningModel}}  本轮推理模型 code（可能为空串）
//
// 正文里出现这两个以外的占位符会让渲染失败、技能加载不上（单测 TestAgentSkillSeed_正文渲染无残留变量 钉住）。
// ⚠️ 单段 / 多段的分界**不写死秒数**（design/01 §9）：正文只说「以模型目录的单次最长时长为准」。

// agentSkillPlaceholder 是每个技能正文与附属文档的首行。
const agentSkillPlaceholder = "【待运营定稿】本技能内容为占位文案（AG-PARAM-05）"

type agentSkillStep struct {
	No         int    `json:"no"`
	Name       string `json:"name"`
	Checkpoint bool   `json:"checkpoint"`
}

type agentSkillRef struct {
	name string
	body string
	sort int
}

type agentSkillSeed struct {
	code        string
	name        string
	category    string
	description string
	tags        []string
	body        string
	steps       []agentSkillStep
	// tools 工具白名单：空 = 不收窄。只能收窄状态机允许的范围（domain.AllowedTools 取交集）。
	tools      []string
	quickEntry bool
	sort       int
	refs       []agentSkillRef
}

// agentSkillSeeds 返回全部通用技能：首批三个 + 2026-11-01 借鉴 GitHub 调研补充的一批
// （docs/agent/skills/01-GitHub调研清单.md，按方向分三个文件：影片 film / 视频 video / 图片 image）。
// 补充给已有技能的附属文档也在那三个文件里（*ExtraRefs），按技能 code 挂上去。
//
// ⚠️ 已有环境靠 seed 补齐：技能是 `on conflict (code) do nothing`、附属文档是 `on conflict (skill_id, name) do nothing`，
// 所以**新技能与新附属文档**再跑一次 seed 就会下发，而改动已有正文不会（那要走迁移或后台）。
// 因此给已有技能补内容一律**新增一份附属文档**，不改旧的那份。
func agentSkillSeeds() []agentSkillSeed {
	seeds := baseAgentSkillSeeds()
	seeds = append(seeds, filmSkillSeeds()...)
	seeds = append(seeds, videoSkillSeeds()...)
	seeds = append(seeds, imageSkillSeeds()...)
	return withExtraRefs(seeds, filmExtraRefs(), videoExtraRefs(), imageExtraRefs())
}

// withExtraRefs 把补充附属文档按技能 code 挂到对应技能上。code 对不上的忽略（单测会把它点出来）。
func withExtraRefs(seeds []agentSkillSeed, extras ...map[string][]agentSkillRef) []agentSkillSeed {
	for _, extra := range extras {
		for i := range seeds {
			seeds[i].refs = append(seeds[i].refs, extra[seeds[i].code]...)
		}
	}
	return seeds
}

// baseAgentSkillSeeds 返回首批三个技能。函数而不是包级变量：「节点与连线」由
// nodeTypeRows / connectionRuleRows 生成，放进初始化顺序里不直观。
func baseAgentSkillSeeds() []agentSkillSeed {
	return []agentSkillSeed{
		{
			code:        "canvas_basics",
			name:        "画布技能",
			category:    "画布与工作流",
			description: "读写项目画布的通用说明：先看再动、只新建不改用户的节点、摆放与画框、节点与连线规则。",
			tags:        []string{"画布", "工作流"},
			body:        canvasBasicsBody,
			sort:        10,
			refs: []agentSkillRef{
				{name: "节点与连线", body: nodesAndEdgesRef(), sort: 1},
			},
		},
		{
			code:        "story_topic",
			name:        "故事选题",
			category:    "创意策划",
			description: "把一句模糊的想法变成可执行的创作规划：给出创意方向请你选择，补齐关键细节，写进规划文档。不生成任何内容。",
			tags:        []string{"选题", "规划", "短视频"},
			body:        storyTopicBody,
			steps: []agentSkillStep{
				{No: 1, Name: "选方向", Checkpoint: true},
				{No: 2, Name: "补细节", Checkpoint: true},
				{No: 3, Name: "写规划", Checkpoint: false},
			},
			tools: []string{"read_canvas", "list_assets", "quote", "ask_user", "read_doc", "write_doc",
				"submit_plan", "load_skill", "read_skill_ref"},
			quickEntry: true,
			sort:       20,
			refs: []agentSkillRef{
				{name: "选题思路", body: storyTopicRef, sort: 1},
			},
		},
		{
			code:        "video_single",
			name:        "视频生成（单段）",
			category:    "视频生成",
			description: "一次生成完成一条短视频：规划 → 确认费用 → 生成一个视频节点 → 把规划与成片框在一起并总结。时长不超过所选模型的单次最长时长。",
			tags:        []string{"视频", "单段", "生成"},
			body:        videoSingleBody,
			steps: []agentSkillStep{
				{No: 1, Name: "规划", Checkpoint: true},
				{No: 2, Name: "确认费用", Checkpoint: true},
				{No: 3, Name: "生成", Checkpoint: false},
				{No: 4, Name: "归组与总结", Checkpoint: false},
			},
			quickEntry: true,
			sort:       30,
			refs: []agentSkillRef{
				{name: "单段流程工作指引", body: singleFlowGuideRef, sort: 1},
				{name: "Prompt 编写指南", body: videoPromptGuideRef, sort: 2},
			},
		},
	}
}

func seedAgentSkills(ctx context.Context, tx *sql.Tx, nextID func() int64) error {
	for _, s := range agentSkillSeeds() {
		tags, err := json.Marshal(s.tags)
		if err != nil {
			return err
		}
		steps, err := json.Marshal(nonNilSteps(s.steps))
		if err != nil {
			return err
		}
		tools, err := json.Marshal(nonNilStrings(s.tools))
		if err != nil {
			return err
		}
		var skillID int64
		err = tx.QueryRowContext(ctx, `
			insert into agent_skill (id, code, name, category, description, tags, body, steps, tools,
				is_quick_entry, sort)
			values ($1, $2, $3, $4, $5, $6::jsonb, $7, $8::jsonb, $9::jsonb, $10, $11)
			on conflict (code) do nothing
			returning id`,
			nextID(), s.code, s.name, s.category, s.description, string(tags), s.body,
			string(steps), string(tools), s.quickEntry, s.sort).Scan(&skillID)
		if errors.Is(err, sql.ErrNoRows) {
			// 已存在（可能被运营改过）：不动它，只取 ID 补齐缺的附属文档。
			err = tx.QueryRowContext(ctx, `select id from agent_skill where code = $1`, s.code).Scan(&skillID)
		}
		if err != nil {
			return fmt.Errorf("技能 %s: %w", s.code, err)
		}
		for _, r := range s.refs {
			if _, err := tx.ExecContext(ctx, `
				insert into agent_skill_reference (id, skill_id, name, body, sort)
				values ($1, $2, $3, $4, $5)
				on conflict (skill_id, name) do nothing`,
				nextID(), skillID, r.name, r.body, r.sort); err != nil {
				return fmt.Errorf("技能 %s 的附属文档 %s: %w", s.code, r.name, err)
			}
		}
	}
	return nil
}

func nonNilSteps(s []agentSkillStep) []agentSkillStep {
	if s == nil {
		return []agentSkillStep{}
	}
	return s
}

func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// nodesAndEdgesRef 由 node_type / connection_rule 的 seed 行生成「节点与连线」，
// 保证技能里说的连线规则与平台真正执行的那份一致（不另编，AGT-30）。
//
// ⚠️ 已有环境靠迁移更新（技能以 `on conflict do nothing` 写入，改这里不会下发）：00066（导演台）→ 00068（智能剪辑节点）→
// 00072（智能剪辑的 Agent 工具，SCT-41），每条只覆盖仍是上一版原文的行。改这份文本时要再写一条迁移，
// 并把上一版冻结进 agent_skill_ref_migration_test.go（同 sct08NodesAndEdgesRef）。
func nodesAndEdgesRef() string {
	names := map[string]string{}
	var b strings.Builder
	b.WriteString(agentSkillPlaceholder + "\n\n# 节点与连线\n\n## 节点类型\n")
	kindNote := map[string]string{
		"generator": "能独立生成内容",
		"composite": "组合 / 批量类节点，由对应流程创建，不要手工新建",
		"material":  "没有独立生成能力",
	}
	// typeNote 按类型覆盖 kindNote 的通用说法：「由对应流程创建」对导演台不成立 —— 它有自己的建节点工具（DRT-29）。
	typeNote := map[string]string{
		"director": "用 director_create 建、director_build 搭景；**不能截图**，搭完要提醒用户打开导演台截图；" +
			"截图之后才能给下游当输入；连视频节点时建议用全能参考",
		// 智能剪辑（docs/agent/smartcut SCT-08 建节点表与连线，SCT-41 补上 Agent 工具）。
		"smartcut": "用 smartcut_create 建（可同时把素材节点按顺序连进来）、smartcut_plan 让 AI 按要求剪（免费，按用户限流）、" +
			"smartcut_get 查看时间线与素材条；连进来的视频 / 图片 / 音频 / 文本按连线顺序自动铺上时间线（还没剪过时）；" +
			"用户也可以在剪辑台里接着剪；导出成片用 render（给 smartcut_get 返回的 timelineId，导出免费）；" +
			"导出成片之后才能给下游当输入",
	}
	for _, n := range nodeTypeRows {
		names[n.code] = n.name
		note := kindNote[n.kind]
		if t, ok := typeNote[n.code]; ok {
			note = t
		}
		fmt.Fprintf(&b, "- %s（%s）：%s\n", n.name, n.code, note)
	}
	b.WriteString("- 画框（frame）：纯视觉容器，只用来把相关节点框在一起；与「组」不同，没有批量执行与计费语义\n")

	// edgeNote 是没有默认操作、又不是素材修饰的连线（图片 → 导演台：全景背景或搭景参考，不叠加提示词）。
	edgeNote := map[[2]string]string{
		{"image", "director"}: "作导演台的全景背景或搭景参考图，不产生生成任务",
	}
	// 连进脚本的连线（2026-10-30）：剧本原文 / 参考文本与角色参考图，在脚本工作台里使用。
	edgeNote[[2]string{"text", "script"}] = "作脚本的剧本原文或参考文本，在脚本工作台里生成分镜时使用，不产生生成任务"
	edgeNote[[2]string{"image", "script"}] = "作脚本的角色参考图，在脚本工作台里使用，不产生生成任务"
	// 连进智能剪辑的连线：作剪辑素材，连上即铺上时间线（SC-F-04），不产生生成任务。
	for _, src := range []string{"video", "image", "audio", "text", "script", "group"} {
		edgeNote[[2]string{src, "smartcut"}] = "作剪辑素材，连上即铺上时间线，不产生生成任务"
	}
	b.WriteString("\n## 允许的连线（来源 → 目标）\n")
	for _, r := range connectionRuleRows {
		switch note, ok := edgeNote[[2]string{r.src, r.dst}]; {
		case r.defaultOp != "":
			fmt.Fprintf(&b, "- %s → %s（默认操作 %s）\n", names[r.src], names[r.dst], r.defaultOp)
		case ok:
			fmt.Fprintf(&b, "- %s → %s（%s）\n", names[r.src], names[r.dst], note)
		default:
			fmt.Fprintf(&b, "- %s → %s（只作为修饰，叠加到目标节点的提示词）\n", names[r.src], names[r.dst])
		}
	}
	b.WriteString(`
不在上表里的连线一律不允许。视频节点的实际操作以所选模式为准（文生视频 / 图生视频 / 首尾帧 / 全能参考 / 视频生视频），表里的默认操作只是建议。

## 规则
- 连线有执行语义：连上的上游会作为下游生成的输入，顺序按连线的 order。不要用连线表达「来源」「参考过」这类关系。
- 文档节点不能连到任何节点上当输入；规划文档与产物的关系用画框表达。
- 导演台没有截图时，下游生成会被拦下；你不能截图，搭完景要提醒用户打开导演台截图，不要用 generate 去补。
- 智能剪辑没有导出成片时，下游生成会被拦下；导出用 render（给 smartcut_get 返回的 timelineId），或请用户在剪辑台里导出。
- 不修改用户的节点：要改用户建的或用户改过的节点时，在旁边新建一个，并在回复里说明它与原节点的关系。
- 不删除任何节点；确实需要删除时告诉用户，由用户自己删。
`)
	return b.String()
}

const canvasBasicsBody = agentSkillPlaceholder + `

# 画布技能

画布是用户的项目工作台。你通过工具读写它，用户随时可以接管、修改、重跑你建的任何东西。

## 先看再动
- 动手前用 read_canvas 看一遍画布：有哪些节点、哪些已有产出、哪些失败了。摘要不够时带上 nodeIds 精确读取。
- 用户在对话里提到某个节点时，以 read_canvas 读到的为准，不要凭记忆描述。
- 需要用户的素材（上传的图片、角色 / 场景 / 道具资产）时用 list_assets 查，不要编造素材 ID。

## 只新建，不改用户的东西
- 你只能修改本会话里由你新建、且用户之后没动过的节点。
- 想改用户的节点时，在旁边新建一个，并说明新节点与原节点的关系；不删除任何节点。
- canvas_apply 提示「该节点由用户创建或已被用户修改」时，照提示改为新建，不要重试同一个操作。

## 摆放与画框
- 新节点放在已有内容旁边的空白处（通常是右侧或下方），不要压住用户的节点。
- 一次任务的产物（规划文档、生成的节点）完成后，用 canvas_apply 加一个画框把它们框在一起，标题用规划标题。画框只是视觉分组，不影响生成。

## 节点与连线
节点类型与允许的连线见附属文档「节点与连线」（read_skill_ref 读取）。连线有执行语义：连上就会成为下游生成的输入。
`

const storyTopicBody = agentSkillPlaceholder + `

# 故事选题

当前会话阶段：{{state}}。
本技能只负责把一句模糊的想法变成可执行的创作规划，不生成任何内容；规划写好后再加载生成类技能（如「视频生成（单段）」video_single）去报价和提交确认。

## 步骤
1. 选方向（需要用户确认）：先弄清用户的需求、目标倾向（讲故事 / 涨粉 / 带货 …）和输出形式（时长、画幅、风格）。给出 2~3 个差异明显的创意方向，用 ask_user 发选择卡请用户选，并允许他提出自己的想法。
2. 补细节（需要用户确认）：围绕选定的方向，只问真正影响成片的问题（主角、关键情节、结尾、风格基调），用 ask_user 一次问完；用户已经说过的不要再问。
3. 写规划：把结论写进项目文档 /plan.md（write_doc），之后按需要用 load_skill 加载生成类技能。

## 创作要求
- 每个方向用一句话说清核心看点或冲突，再用一句话说明为什么适合这位用户的目标。
- 开头要有钩子：头几秒就给出画面、动作或悬念，而不是片名或铺垫。
- 涉及真实人物、品牌、敏感事件时提醒用户注意版权与合规，不编造事实。
- 你没有联网搜索工具：不要声称「当前热点」「播放量」这类你无法核实的数据。

## 规划里要说清楚（版式不限）
- 标题与一句话梗概；目标时长与画幅
- 按时间顺序的情节节拍，每一拍的画面与情绪
- 风格与声音（配乐 / 对白 / 旁白）的取向
- 还没确定、需要用户在确认时注意的地方

详细的选题方法见附属文档「选题思路」。
`

const storyTopicRef = agentSkillPlaceholder + `

# 选题思路

## 识别需求
- 需求：用户想做什么（一个故事、一个产品展示、一个梗）。
- 目标倾向：叙事表达、涨粉、带货、品牌形象 —— 决定开头钩子和结尾落点。
- 输出形式：时长、画幅（竖屏 / 横屏）、风格（写实、动画、国风 …）。用户没说时给出建议值，在规划里写明，让他确认时能看到。

## 给方向
- 方向之间要真的不同：换主角视角、换基调（燃 / 虐 / 搞笑）、换结构（倒叙、反转、对比），而不是同一个故事换几个形容词。
- 短视频的故事要小：一个人物、一个冲突、一次转折就够；铺太多支线会在几十秒里讲不完。
- 经典题材（如名著片段）优先找一个新角度：配角视角、现代化改编、只拍高潮一场。

## 补细节
- 能用选项就不用文本题；每题只问一件事。
- 只问会改变画面的问题。「想表达什么情感」不如「结尾是团圆还是离别」好回答。
- 用户说「你决定」时，选最稳妥的默认并写进规划。
`

const videoSingleBody = agentSkillPlaceholder + `

# 视频生成（单段）

当前会话阶段：{{state}}。
本技能把已经想清楚的创意做成一条视频：一个视频节点、一次生成。

## 先判断是不是单段
- 一支视频一次生成：一个视频节点、一次生成，镜头切换写进同一段提示词；不要把一支视频拆成几段分别生成再拼接。
- 单次最长时长以平台的模型目录为准，不要假设一个固定秒数。拿不准时用 quote 按计划时长报价：报不出价、原因指向时长或参数，就说明超出了这个模型的能力。模型目录标注不支持多镜头时，仍然一次生成，把几个画面写成一个连续镜头里的动作与运镜变化。
- 超出单次最长时长时用 ask_user 让用户选：缩短时长，或换一个支持更长时长的模型；用户明确要做更长的片子时才走多段流程。不要自行拆段，也不要硬塞进一次生成。
- 画幅、清晰度、时长在用户偏好里是「智能」时，规划之前用 ask_user 确认（题目 id 用 aspect_ratio / resolution / duration）。
- 不知道用哪个视频模型时，用 ask_user 请用户选或确认，不要编造模型 code。

## 步骤
1. 规划（需要用户确认）：先读附属文档「单段流程工作指引」（read_skill_ref）。信息不够时用 ask_user 问；规划写进 /plan.md（write_doc）。
2. 确认费用（需要用户确认）：用 submit_plan 提交，一条视频列一项，写清模型与时长。金额由平台逐项报价，你不写价格；用户确认之前不会开始生成。
3. 生成：用户确认后，用 generate 新建一个视频节点并提交。提示词写法见附属文档「Prompt 编写指南」。提交后等待平台通知结果，不要重复提交同一条。
4. 归组与总结：结果出来后，用 canvas_apply 加一个画框，把 /plan.md 的文档节点和这次的视频节点框在一起，标题用规划标题；然后总结。

## 总结
- 说「已按以下计划生成，请查看效果」，再复述计划要点。
- 你没有看过成片：不要声称视频里一定出现了什么画面、台词或效果。
- 失败的项说清平台给出的原因，以及失败会按原批次退款；给出下一步建议（例如调整某个阶段的描述后重试）。
`

const singleFlowGuideRef = agentSkillPlaceholder + `

# 单段流程工作指引

## 什么时候问用户
- 时长、画幅、视频模型、主角形象、结尾走向 —— 任何一项缺失且会明显影响成片时，先用 ask_user 问。
- 用户已经在对话或 /plan.md 里说清的，不再重复问；需要问的一次问完，不要一轮问一个。
- 用户说「你决定」时，选最稳妥的默认值并写进规划，让他在确认时能看到。

## 什么时候 submit_plan
- /plan.md 已写好、模型与时长已定、该问的都问过了，才 submit_plan。
- 提交前可以先用 quote 估价。报不出价时按原因处理：用户自己能解决的（例如缺一张参考图）直接告诉他怎么做；不能解决的换模型或时长，或如实说明。
- 用户在确认卡上选「需要调整」时，按他的意见改 /plan.md（改已存在的文档要带上读到的版本号），再重新 submit_plan。旧规划已作废，不能拿它继续执行。
- 余额不足时平台会在确认卡上显示差额，不要自己估算或承诺金额。

## 生成
- 只建一个视频节点，提示词一次写完整条视频；不要先单独出角色图或场景图。
- 用户提供了参考图时，把它作为输入传给 generate，并选用能接收图片的操作（例如图生视频）。
- 提交后会话会等待结果，平台通知你之后再继续；用户要求停止时用 cancel_task 取消这个任务。
- 超出已批准额度时平台会请用户追加，你只需说明为什么需要更多。

## 归组
- 结果出来后（成功或失败都要做）用 canvas_apply 加一个画框：标题用规划标题，成员是 /plan.md 的文档节点和这次的视频节点。
- 两个节点的 ID 分别在 write_doc 与 generate 的返回结果里；不确定时先 read_canvas。
- 画框放在两者外围，不要遮住其他节点。

## 总结
- 第一句：「已按以下计划生成，请查看效果。」后面按阶段复述计划。
- 不描述你没看过的画面细节，不保证效果。
- 最后给 1~2 条下一步建议，例如调整某个阶段的描述后重试、换画幅再出一版。
`

const videoPromptGuideRef = agentSkillPlaceholder + `

# Prompt 编写指南（单段视频）

分阶段的编号与时间标注由平台约定，以系统提示词里的要求为准；这里只讲每个阶段的内容怎么写。

## 写什么
- 按时间顺序把整条视频分成几个阶段，每个阶段写清：画面主体、动作、镜头（景别 / 运镜）、光线与氛围。
- 主体先行：先写清是谁 / 什么、长什么样，再写动作，最后才是风格词。识别信息要排在审美词前面，否则模型先照风格画，把主体挤掉。
- 动作要看得见：写「转身拔剑」，不写「内心挣扎」。一个阶段只写一个主要动作。
- 同一个角色在每个阶段的外貌描述保持一致（发型、服装、标志物），不要中途换说法。

## 镜头与节奏
- 镜头用常见说法：特写、近景、中景、全景、推、拉、摇、跟拍、俯拍、仰拍。
- 第一阶段就给钩子；最后一阶段给落点（定格、转身离去、字幕位留白）。

## 声音
- 所选模型支持自带音轨时，可以写配乐或环境声的取向；对白尽量少、尽量短。

## 不要写
- 真实人物姓名、商标与受版权保护的角色名。
- 一长串「不要出现……」的负面清单。
- 堆砌风格词：两三个就够。
`

// AgentSkillCount 是内置通用技能的条数（测试按它断言，别再写死一个会随技能库增长而过期的数）。
func AgentSkillCount() int { return len(agentSkillSeeds()) }
