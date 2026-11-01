package main

// 画布 Agent 单段流程的「脚本化模型」（AGT-31，AG-E2E-01~06）。
//
// 请求带 tools 且系统提示词含 Agent 骨架标记时，按对话里**最后一条消息的类型与内容**
// 无状态地决定下一步（同一份对话永远得到同一个回答 —— 重放、重试、Worker 重启接手都不会走偏）：
//
//	最后一条                               → 回复
//	用户消息（规划态，还没加载过技能）       → load_skill(video_single)
//	load_skill 的结果                       → ask_user(choice)
//	用户消息「我选择了：…」（规划态）       → write_doc(/plan.md)
//	write_doc 的结果                        → submit_plan（一条视频）
//	用户消息（执行态：批准 / 追加额度）     → generate（默认 wait=true）
//	generate 的结果（#超额 且只提交过一个）  → 第二个 generate（必然超出额度）
//	系统通知「…成功 N…」（N ≥ 1）          → canvas_apply(add_frame，成员 = 视频节点 + 规划文档节点)
//	canvas_apply 的结果 / 系统通知全失败    → 纯文本总结 + 「接下来你可以：」建议
//
// 魔法词（写在用户消息里）：
//   - `#超额`：执行态连续提交两个 generate，第一个 wait=false，第二个超出已批准的额度；
//   - `#对话失败`：最后一条是含它的用户消息时回 HTTP 500（验对话费退款）；
//   - `#tag:XXX`：探针按它归类请求（同一个假上游上并行跑的会话互不干扰）；
//   - `#model=CODE`：系统提示词里没有「当前视频模型」一节时用它指定视频模型。
//
// 探针 `GET /_probe/agent-tools?tag=XXX`：最近的 Agent 请求里**解析出的工具名**与阶段。
// ⚠️ 断言「规划态没有 generate」只能用它，不能用 Provider 请求日志：后者脱敏 + 截断，
// 工具定义排在长系统提示词之后，可能正好落在截断点外 —— 那样断言会**假通过**。
//
// ⚠️ 只模拟「一个照着流程走的模型」，不模拟模型的判断力；真实推理模型的工具调用稳定性
// 要另行验收（同 PARAM-04：假上游的绿不等于真实上游验收）。

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// 以下字面量与后端逐字一致，改一边必须改另一边：
//   - agentSkeletonMarker / 阶段标记：backend/internal/agent/domain/prompt.go
//     （SystemPromptSkeleton 首句、PlanningModeNote / ExecutingModeNote 标题行）；
//   - agentSystemEventPrefix：backend/internal/agent/app/context.go 的 SystemEventPrefix；
//   - 「我选择了：」：agent/app/service.go 的 describeSubmission；
//   - 「- 模型：」：agent/app/video_model.go 的 VideoModelSection；
//   - agentMarketingMarker / 「商品：」：agent/domain/prompt.go 的 SceneHeading(marketing)（MKT-T-18）
//     与 marketing/domain.SlotSummary 的首行标签。
//
// ⚠️ 后端的 TestSystemPromptSkeleton_首句未变 / TestSceneHeading_与假上游标记同源 会**读这个文件**
// 核对前四个常量：改了任何一边，两边同时红。常量必须保持「名字 = "字面量"」的单行写法。
const (
	agentSkeletonMarker    = "你是 seeAitv 画布里的创作助手"
	agentPlanningMarker    = "## 当前阶段：规划"
	agentExecutingMarker   = "## 当前阶段：执行"
	agentMarketingMarker   = "## 场景：营销"
	agentSystemEventPrefix = "【系统通知，非用户发言】"

	magicOverBudget = "#超额"
	magicChatFail   = "#对话失败"

	agentSkillCode = "video_single"
)

var (
	reAgentTag        = regexp.MustCompile(`#tag:([A-Za-z0-9_\-]+)`)
	reAgentModelHint  = regexp.MustCompile(`#model=([A-Za-z0-9_.\-]+)`)
	reVideoModelLine  = regexp.MustCompile(`(?m)^- 模型：(\S+)`)
	reVideoModelUnk   = regexp.MustCompile(`视频模型是 ([A-Za-z0-9_.\-]+)，`)
	reWakeSucceeded   = regexp.MustCompile(`成功 (\d+)`)
	reWakeTaskSummary = regexp.MustCompile(`所等的 \d+ 个任务已全部结束`)
)

// agentCall 是一次助手工具调用。
type agentCall struct {
	ID   string
	Name string
	Args string
}

// agentMsg 是对话里的一条消息（system 已单独取出）。
type agentMsg struct {
	Role       string // user / assistant / tool
	Text       string
	ToolCallID string      // role=tool
	ToolCalls  []agentCall // role=assistant
	// Images / HTTPImages：这条消息里的图片块数与其中以 http(s) 外链发出的张数（营销 E2E 用，MKT-T-30）。
	Images     int
	HTTPImages int
	// Audios：这条消息里的音频块数（营销 E2E 用，MKT-T-41：参考音频不发给模型）。
	Audios int
}

// agentTurnInput 是一次 Agent 请求的快照。
type agentTurnInput struct {
	System   string
	Tools    []string
	Messages []agentMsg
	// BodyURLs 是请求体里出现的 http(s) 地址（去重，最多 5 个）；营销请求里应为空（MKT-AC-05）。
	BodyURLs []string
	// BodyAudioData 是请求体里内联音频（data:audio/）出现的次数；营销请求里应为 0（MKT-T-41）。
	BodyAudioData int
}

// agentReply 是脚本的决定。
type agentReply struct {
	chatScript
	// Fail 为 true 时回 HTTP 500（#对话失败）。
	Fail bool
	// Action 记进探针：load_skill / ask_user / write_doc / submit_plan / generate /
	// canvas_apply / text / fail。
	Action string
}

// isAgentPrompt 判定系统提示词是不是画布 Agent 的骨架。
func isAgentPrompt(system string) bool { return strings.Contains(system, agentSkeletonMarker) }

// isMarketingAgent 判定是不是营销会话（系统提示词里有营销场景段，MKT-T-18）。
func isMarketingAgent(system string) bool { return strings.Contains(system, agentMarketingMarker) }

// agentScene 是探针记录的会话场景。
func agentScene(system string) string {
	if isMarketingAgent(system) {
		return "marketing"
	}
	return "general"
}

var reMarketingProduct = regexp.MustCompile(`(?m)^商品：(.*)$`)

// 营销 E2E 的魔法词（MKT-T-30）。写在**开启本轮的那条用户消息**里的只影响本轮规划
// （同一会话里「再来一版」不会继承上一轮的魔法词）；执行期的魔法词（#变体失败N）看全部用户消息。
//   - `#超限选图`：第一次 submit_plan 选 4 张商品图（超过模型上限），收到 too_many_images 后改选 2 张重交；
//   - `#看图N`：写规划之前先 view_media 看商品图 #N（下一次请求会多带一张图）；
//   - `#变体N`：规划 N 个变体（v1..vN，每个用商品图 #1）；
//   - `#变体失败N`：执行时第 N 个条目的画面描述带 `#上游失败`，假 Seedance 提交即 400；
//   - `#裂变`：一条视频、不选商品图（会话有源视频时由服务端判路线）；
//   - `#出示意图`（顾问）：提交一张图片条目，模型取 `#image=CODE`。
const (
	magicOverImages   = "#超限选图"
	magicFission      = "#裂变"
	magicSketch       = "#出示意图"
	magicUpstreamFail = "#上游失败"
)

var (
	reMagicViewMedia   = regexp.MustCompile(`#看图(\d+)`)
	reMagicVariants    = regexp.MustCompile(`#变体(\d+)`)
	reMagicVariantFail = regexp.MustCompile(`#变体失败(\d+)`)
	reMagicImageModel  = regexp.MustCompile(`#image=([A-Za-z0-9_.\-]+)`)
	// reProductCount 取「## 营销输入」里的商品图张数（marketing/domain.SlotSummary 的「商品图：共 N 张」）。
	reProductCount = regexp.MustCompile(`(?m)^商品图：共 (\d+) 张`)
)

// productIndexes 取前 n 张商品图的序号（不超过会话里的商品图张数，避免选到不存在的 #k）。
func productIndexes(system string, n int) []int {
	total := 0
	if m := reProductCount.FindStringSubmatch(system); m != nil {
		total, _ = strconv.Atoi(m[1])
	}
	if n > total {
		n = total
	}
	out := []int{}
	for i := 1; i <= n; i++ {
		out = append(out, i)
	}
	return out
}

// turnStart 是本轮起点：最后一条真用户消息（不是系统通知）的下标；没有返回 -1。
func (in agentTurnInput) turnStart() int {
	for i := len(in.Messages) - 1; i >= 0; i-- {
		m := in.Messages[i]
		if m.Role == "user" && !strings.HasPrefix(m.Text, agentSystemEventPrefix) {
			return i
		}
	}
	return -1
}

// startText 是开启本轮的那条用户消息。
func (in agentTurnInput) startText() string {
	if i := in.turnStart(); i >= 0 {
		return in.Messages[i].Text
	}
	return ""
}

// callsSince 取下标 start 之后某个工具的全部调用（按出现顺序）。
func (in agentTurnInput) callsSince(start int, name string) []agentCall {
	var out []agentCall
	for i := start + 1; i < len(in.Messages) && i >= 0; i++ {
		for _, c := range in.Messages[i].ToolCalls {
			if c.Name == name {
				out = append(out, c)
			}
		}
	}
	return out
}

// resultsSince 取下标 start 之后某个工具的全部结果（解析成对象；解不开的跳过）。
func (in agentTurnInput) resultsSince(start int, name string) []map[string]any {
	var out []map[string]any
	for i := start + 1; i < len(in.Messages) && i >= 0; i++ {
		m := in.Messages[i]
		if m.Role != "tool" || in.toolOf(m.ToolCallID) != name {
			continue
		}
		var obj map[string]any
		if json.Unmarshal([]byte(m.Text), &obj) == nil {
			out = append(out, obj)
		}
	}
	return out
}

// planKeys 是最近一次**发出卡片**的 submit_plan 结果里的条目键（被退回的那几次没有 items）。
func (in agentTurnInput) planKeys() []string {
	rs := in.toolResults("submit_plan")
	for i := len(rs) - 1; i >= 0; i-- {
		items, ok := rs[i]["items"].([]any)
		if !ok || rs[i]["error"] != nil {
			continue
		}
		var keys []string
		for _, x := range items {
			it, _ := x.(map[string]any)
			if k, _ := it["key"].(string); k != "" {
				keys = append(keys, k)
			}
		}
		return keys
	}
	return nil
}

// agentMarketingScript 是营销会话的分支（MKT-T-18 建立分流，MKT-T-30 扩成完整的单段流程）。
//
//	规划态
//	  本轮第一条用户消息          → view_media（#看图N）或 write_doc(/plan.md)
//	  view_media 的结果 / 附图通知 → write_doc(/plan.md)
//	  write_doc 的结果            → submit_plan（营销条目：key / productIndexes / variant）
//	  submit_plan 被 too_many_images 退回 → 改选 2 张重交；其余结果 → 纯文本
//	执行态
//	  批准消息 / generate 的结果   → 按已批准的条目键逐条 generate（planItemKey；除最后一条外 wait=false）
//	  系统通知「…成功 N…」        → 单条：canvas_apply 画框（成员 = 本次生成的节点 + 首次的规划文档）；多变体：纯文本
//	  canvas_apply 的结果         → 纯文本总结
//
// 不加载技能、不发方向卡：营销输入（商品、所选项）已经在「## 营销输入」里。
// 返回 false = 交给通用分支。
func agentMarketingScript(in agentTurnInput, call func(name, text string, args any) agentReply,
	model string) (agentReply, bool) {
	state := agentState(in.System)
	var last agentMsg
	if n := len(in.Messages); n > 0 {
		last = in.Messages[n-1]
	}
	if isAdvisorAgent(in.System) {
		if r, ok := agentAdvisorScript(in, call, state, last); ok {
			return r, true
		}
	}
	switch state {
	case "planning":
		return marketingPlanning(in, call, model, last)
	case "executing":
		return marketingExecuting(in, call, last)
	}
	return agentReply{}, false
}

func marketingPlanning(in agentTurnInput, call func(name, text string, args any) agentReply,
	model string, last agentMsg) (agentReply, bool) {
	text := func(s string) (agentReply, bool) {
		return agentReply{chatScript: chatScript{Text: s}, Action: "text"}, true
	}
	start := in.turnStart()
	startText := in.startText()
	writePlan := func() (agentReply, bool) {
		product := "未填写"
		if m := reMarketingProduct.FindStringSubmatch(in.System); m != nil {
			product = strings.TrimSpace(m[1])
		}
		return call("write_doc", "把营销规划写进文档。", map[string]any{"path": "/plan.md",
			"content": "# 营销规划\n\n- 商品：" + product + "\n- 视频，5 秒，模型 " + model + "\n"}), true
	}
	switch {
	case last.Role == "user" && !strings.HasPrefix(last.Text, agentSystemEventPrefix):
		if m := reMagicViewMedia.FindStringSubmatch(last.Text); m != nil && len(in.callsSince(start, "view_media")) == 0 {
			idx, _ := strconv.Atoi(m[1])
			return call("view_media", "先看一眼商品图。", map[string]any{"slot": "product", "index": idx}), true
		}
		return writePlan()
	case last.Role == "user": // 系统通知：view_media 之后 Runner 追加的带图消息
		if len(in.callsSince(start, "view_media")) > 0 && len(in.callsSince(start, "write_doc")) == 0 {
			return writePlan()
		}
		return text("收到。")
	case last.Role == "tool":
		switch in.toolOf(last.ToolCallID) {
		case "view_media":
			return writePlan()
		case "write_doc":
			if model == "" {
				return text("没有可用的视频模型信息，无法提交规划。")
			}
			over := strings.Contains(startText, magicOverImages)
			return call("submit_plan", "营销规划已写好，请确认。", map[string]any{
				"title": "营销视频规划", "items": marketingPlanItems(in.System, startText, model, over)}), true
		case "submit_plan":
			rs := in.resultsSince(start, "submit_plan")
			if n := len(rs); n > 0 && rs[n-1]["error"] == "too_many_images" && n == 1 {
				return call("submit_plan", "超过了模型可收的张数，改选 2 张重交。", map[string]any{
					"title": "营销视频规划", "items": marketingPlanItems(in.System, startText, model, false)}), true
			}
			if n := len(rs); n > 0 && rs[n-1]["error"] != nil {
				return text("规划没有提交成功：" + fmt.Sprint(rs[n-1]["error"]) + "。请调整后再试。")
			}
			return text("规划已提交，等你确认。")
		}
	}
	return agentReply{}, false
}

// marketingPlanItems 按本轮魔法词拼计划项（营销条目：key / productIndexes / variant）。
func marketingPlanItems(system, startText, model string, over bool) []any {
	if m := reMagicVariants.FindStringSubmatch(startText); m != nil {
		n, _ := strconv.Atoi(m[1])
		items := make([]any, 0, n)
		for i := 1; i <= n; i++ {
			items = append(items, map[string]any{"key": fmt.Sprintf("v%d", i), "type": "video",
				"modelCode": model, "operationCode": "ref2video", "durationSec": 5,
				"title": fmt.Sprintf("变体 %d", i), "variant": i, "variantLabel": fmt.Sprintf("变体 %d", i),
				"productIndexes": productIndexes(system, 1)})
		}
		return items
	}
	item := map[string]any{"key": "v1", "type": "video", "modelCode": model, "operationCode": "ref2video",
		"durationSec": 5, "title": "成片"}
	switch {
	case strings.Contains(startText, magicFission):
		item["key"] = "f1"
	case over:
		item["productIndexes"] = productIndexes(system, 4)
	default:
		if idx := productIndexes(system, 2); len(idx) > 0 {
			item["productIndexes"] = idx
		}
	}
	return []any{item}
}

func marketingExecuting(in agentTurnInput, call func(name, text string, args any) agentReply,
	last agentMsg) (agentReply, bool) {
	text := func(s string) (agentReply, bool) {
		return agentReply{chatScript: chatScript{Text: s}, Action: "text"}, true
	}
	start := in.turnStart()
	keys := in.planKeys()
	nextGenerate := func() (agentReply, bool) {
		if len(keys) == 0 {
			return text("没有找到已批准的计划条目。")
		}
		done := map[string]bool{}
		for _, c := range in.callsSince(start, "generate") {
			var a map[string]any
			_ = json.Unmarshal([]byte(c.Args), &a)
			if k, _ := a["planItemKey"].(string); k != "" {
				done[k] = true
			}
		}
		failAt := 0
		for _, t := range in.userTexts() {
			if m := reMagicVariantFail.FindStringSubmatch(t); m != nil {
				failAt, _ = strconv.Atoi(m[1])
			}
		}
		for i, k := range keys {
			if done[k] {
				continue
			}
			prompt := "商品展示短片：突出商品外观与卖点，镜头缓慢推近"
			if failAt == i+1 {
				prompt += " " + magicUpstreamFail
			}
			return call("generate", "开始生成 "+k+"。", map[string]any{"planItemKey": k, "prompt": prompt,
				"wait": i == len(keys)-1}), true
		}
		return text("计划里的条目都已提交。")
	}
	switch {
	case last.Role == "user" && !strings.HasPrefix(last.Text, agentSystemEventPrefix):
		return nextGenerate()
	case last.Role == "tool" && in.toolOf(last.ToolCallID) == "generate":
		return nextGenerate()
	case last.Role == "user": // 系统唤醒
		ok := 0
		if m := reWakeSucceeded.FindStringSubmatch(last.Text); m != nil {
			ok, _ = strconv.Atoi(m[1])
		}
		if !reWakeTaskSummary.MatchString(last.Text) || ok == 0 {
			return text("生成没有成功，平台会按原批次退款。\n接下来你可以：\n- 换一个视频模型再试一次")
		}
		if len(keys) >= 2 {
			return text(fmt.Sprintf("%d 个变体已结束，成功 %d 个，已按变体排进画框。\n接下来你可以：\n- 重试失败的变体", len(keys), ok))
		}
		var members []string
		for _, r := range in.resultsSince(start, "generate") {
			if _, has := r["generationId"]; has {
				if s, _ := r["nodeId"].(string); s != "" {
					members = append(members, s)
				}
			}
		}
		if in.countCalls("canvas_apply") == 0 {
			if rs := in.toolResults("write_doc"); len(rs) > 0 {
				if s, _ := rs[0]["nodeId"].(string); s != "" {
					members = append(members, s)
				}
			}
		}
		if len(members) == 0 {
			return text("任务已完成，但找不到要归组的节点。")
		}
		return call("canvas_apply", "把规划与成片框在一起。", map[string]any{
			"ops": []any{map[string]any{"kind": "add_frame",
				"frame": map[string]any{"title": "营销成片", "members": members}}},
		}), true
	case last.Role == "tool" && in.toolOf(last.ToolCallID) == "canvas_apply":
		return text("已生成营销成片，并把规划文档与成片框在一起。\n接下来你可以：\n- 换一个 Hook 再出一版")
	}
	return agentReply{}, false
}

// 生意增长顾问（MKT-T-26，03 S-13）：营销会话里加载了顾问技能。
//
// agentAdvisorMarker 是后端系统提示词里已加载技能的标题（agent/app/context.go 的
// 「## 已加载技能：%s」，%s = 技能名称）。技能名称是**测试数据**（管理端自建，MKT-T-30 E2E 用同名），
// 不是格式契约：正式内容改名后这里跟着改，否则顾问会话会落回营销规划分支。
const (
	agentAdvisorMarker  = "## 已加载技能：生意增长顾问"
	agentAdvisorDocPath = "/notes/增长建议.md"
)

// isAdvisorAgent 判定是不是加载了顾问技能的营销会话。
func isAdvisorAgent(system string) bool {
	return isMarketingAgent(system) && strings.Contains(system, agentAdvisorMarker)
}

// agentAdvisorScript 是顾问分支：**只对话、写一份文档**，不提交规划、不生成（纯咨询只按轮计费，MKT-AC-30）。
//
//	最后一条                                   → 回复
//	用户消息（规划态，还没写过增长建议）         → write_doc(/notes/增长建议.md)
//	write_doc 的结果                           → 纯文本总结（不 submit_plan）
//	之后的用户消息（规划态）                     → 纯文本回答
//
// ⚠️ 文档路径是 /notes/增长建议.md 而不是根目录的「增长建议.md」：Agent 只能写 /plan.md、/shots.md、
// /.memory.md 与 /notes/<名>.md（agent/tools/docs.go ValidAgentDocPath）。
// 返回 false = 交给营销 / 通用分支（执行态、系统通知等顾问不管的情形）。
func agentAdvisorScript(in agentTurnInput, call func(name, text string, args any) agentReply,
	state string, last agentMsg) (agentReply, bool) {
	text := func(s string) agentReply { return agentReply{chatScript: chatScript{Text: s}, Action: "text"} }
	switch {
	case last.Role == "tool" && in.toolOf(last.ToolCallID) == "write_doc":
		return text("已把分析写进 增长建议.md：人群切片、每个人群的动机、内容方向与测试假设。" +
			"文中涉及行业 / 竞品 / 平台的数据需你核实；以上不构成 ROI 或销量承诺。"), true
	case last.Role == "user" && !strings.HasPrefix(last.Text, agentSystemEventPrefix) && state == "planning" &&
		strings.Contains(last.Text, magicSketch):
		// 出示意图（MKT-AC-31）：照常走确认卡，批准后由营销执行分支按 planItemKey 生成。
		img := ""
		if m := reMagicImageModel.FindStringSubmatch(last.Text); m != nil {
			img = m[1]
		}
		return call("submit_plan", "按你的意思出一张示意图，请确认。", map[string]any{"title": "示意图",
			"items": []any{map[string]any{"key": "img1", "type": "image", "modelCode": img,
				"operationCode": "text2img", "count": 1, "title": "示意图"}}}), true
	case last.Role == "user" && !strings.HasPrefix(last.Text, agentSystemEventPrefix) && state == "planning":
		for _, m := range in.Messages {
			for _, c := range m.ToolCalls {
				if c.Name == "write_doc" && strings.Contains(c.Args, agentAdvisorDocPath) {
					return text("收到。基于你补充的信息，建议一次只测一个变量：先换开头，看 3 秒停留率是否上升。"), true
				}
			}
		}
		product := "未填写"
		if m := reMarketingProduct.FindStringSubmatch(in.System); m != nil {
			product = strings.TrimSpace(m[1])
		}
		return call("write_doc", "把增长建议写进项目文档。", map[string]any{"path": agentAdvisorDocPath,
			"content": "# 增长建议\n\n- 商品：" + product + "\n- 人群切片：待你补充渠道与价格带后细化\n" +
				"- 测试假设：换开头后 3 秒停留率上升（与当前版本对比）\n- 行业 / 竞品数据：需你核实\n"}), true
	}
	return agentReply{}, false
}

// agentState 从系统提示词里的阶段说明解析会话阶段；没有阶段说明时为空串。
func agentState(system string) string {
	switch {
	case strings.Contains(system, agentPlanningMarker):
		return "planning"
	case strings.Contains(system, agentExecutingMarker):
		return "executing"
	}
	return ""
}

func (in agentTurnInput) toolOf(callID string) string {
	for _, m := range in.Messages {
		for _, c := range m.ToolCalls {
			if c.ID == callID {
				return c.Name
			}
		}
	}
	return ""
}

func (in agentTurnInput) userTexts() []string {
	var out []string
	for _, m := range in.Messages {
		if m.Role == "user" && !strings.HasPrefix(m.Text, agentSystemEventPrefix) {
			out = append(out, m.Text)
		}
	}
	return out
}

func (in agentTurnInput) hasMagic(word string) bool {
	for _, t := range in.userTexts() {
		if strings.Contains(t, word) {
			return true
		}
	}
	return false
}

// agentTag 取对话里第一个 #tag:XXX（探针按它归类）。
func agentTag(in agentTurnInput) string {
	for _, t := range in.userTexts() {
		if m := reAgentTag.FindStringSubmatch(t); m != nil {
			return m[1]
		}
	}
	return ""
}

func (in agentTurnInput) countCalls(name string) int {
	n := 0
	for _, m := range in.Messages {
		for _, c := range m.ToolCalls {
			if c.Name == name {
				n++
			}
		}
	}
	return n
}

// toolResults 按出现顺序取某个工具的全部结果（解析成对象；解不开的跳过）。
func (in agentTurnInput) toolResults(name string) []map[string]any {
	var out []map[string]any
	for _, m := range in.Messages {
		if m.Role != "tool" || in.toolOf(m.ToolCallID) != name {
			continue
		}
		var obj map[string]any
		if json.Unmarshal([]byte(m.Text), &obj) == nil {
			out = append(out, obj)
		}
	}
	return out
}

// videoModel 取本次该用的视频模型：系统提示词「当前视频模型」一节优先，其次用户消息里的 #model=。
func (in agentTurnInput) videoModel() string {
	if m := reVideoModelLine.FindStringSubmatch(in.System); m != nil {
		return m[1]
	}
	if m := reVideoModelUnk.FindStringSubmatch(in.System); m != nil {
		return m[1]
	}
	for _, t := range in.userTexts() {
		if m := reAgentModelHint.FindStringSubmatch(t); m != nil {
			return m[1]
		}
	}
	return ""
}

func mustArgs(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// magicDirector：对话里出现它就走 3D 导演台分支（DRT-30）。
const magicDirector = "#导演台"

// agentDirectorScript 是 3D 导演台的短流程（DRT-30，E2E 第 271 步）：
//   - 规划态：直接提交一条视频计划项（导演台工具只在执行态下发，得先批准才进得去）；
//   - 执行态：director_create → 用它回的 nodeId 调 director_build → 提醒用户打开导演台截图
//     （Agent 不能截图，DRK-10；下游要等截图才有输出）。
//
// 返回 false 表示这一步不归它管，交回通用流程。
func agentDirectorScript(in agentTurnInput, call func(name, text string, args any) agentReply,
	text func(string) agentReply, model, state string, last agentMsg) (agentReply, bool) {
	if last.Role == "tool" {
		switch in.toolOf(last.ToolCallID) {
		case "director_create":
			var res map[string]any
			_ = json.Unmarshal([]byte(last.Text), &res)
			nodeID, _ := res["nodeId"].(string)
			if nodeID == "" {
				return text("导演台节点没有建出来：" + truncate(last.Text, 80)), true
			}
			return call("director_build", "按描述搭景。", map[string]any{
				"nodeId": nodeID, "prompt": "三个人在街头对峙，低角度仰拍"}), true
		case "director_build":
			return text("场景已经搭好：三个人在街头对峙，机位放低仰拍。\n" +
				"请打开导演台截图，下游节点才有输出（我不能截图）。"), true
		}
		return agentReply{}, false
	}
	if last.Role != "user" || strings.HasPrefix(last.Text, agentSystemEventPrefix) {
		return agentReply{}, false
	}
	switch state {
	case "planning":
		if in.countCalls("submit_plan") > 0 {
			return agentReply{}, false
		}
		if model == "" {
			return text("没有可用的视频模型信息，无法提交规划。"), true
		}
		return call("submit_plan", "先确认这次的规划。", map[string]any{"title": "导演台搭景",
			"items": []any{map[string]any{"type": "video", "modelCode": model,
				"operationCode": "text2video", "durationSec": 5, "title": "成片"}}}), true
	case "executing":
		if in.countCalls("director_create") == 0 {
			return call("director_create", "先在画布上建一个导演台。", map[string]any{"title": "导演台 E2E"}), true
		}
	}
	return agentReply{}, false
}

// magicSmartcut：对话里出现它就走智能剪辑分支（docs/agent/smartcut SCT-41，E2E 第 290 步）。
const magicSmartcut = "#智能剪辑"

// reSmartcutInputs 用户消息里的「#素材=n1,n2」：要连进智能剪辑的节点（按播放顺序）。
var reSmartcutInputs = regexp.MustCompile(`#素材=([A-Za-z0-9_,\-]+)`)

// agentSmartcutScript 是智能剪辑的短流程（SCT-41）：
//   - 规划态：直接提交一条计划项（智能剪辑工具只在执行态下发，得先批准才进得去）；
//   - 执行态：smartcut_create（连上 #素材= 给的节点）→ smartcut_plan（素材混剪）→ smartcut_get →
//     有片段就 render（给 timelineId，导出免费、不等）→ 收尾说一句。
//
// 某一步没拿到想要的字段（被限流、素材不合适……）时把结果原样转述给用户，不重试。返回 false 表示这一步不归它管。
func agentSmartcutScript(in agentTurnInput, call func(name, text string, args any) agentReply,
	text func(string) agentReply, model, state string, last agentMsg) (agentReply, bool) {
	if last.Role == "tool" {
		var res map[string]any
		_ = json.Unmarshal([]byte(last.Text), &res)
		str := func(k string) string { v, _ := res[k].(string); return v }
		switch in.toolOf(last.ToolCallID) {
		case "smartcut_create":
			if str("nodeId") == "" {
				return text("智能剪辑节点没有建出来：" + truncate(last.Text, 80)), true
			}
			// ⚠️ 要求里不写「卡点」：素材里没有配乐时卡点会被拒（42217，SCT-42 首跑踩到），这一步验的是链路不是卡点。
			return call("smartcut_plan", "按要求剪一版。", map[string]any{"nodeId": str("nodeId"), "mode": "mix",
				"instruction": "剪成 10 秒的短片，开头放最抓人的一段", "targetSec": 10}), true
		case "smartcut_plan":
			if str("callId") == "" {
				return text("AI 剪辑没有成功：" + truncate(last.Text, 120)), true
			}
			return call("smartcut_get", "看一下剪出来的结果。", map[string]any{"nodeId": str("nodeId")}), true
		case "smartcut_get":
			if n, _ := res["clipCount"].(float64); n == 0 || str("timelineId") == "" {
				return text("时间线还是空的：" + truncate(last.Text, 80)), true
			}
			return call("render", "导出成片。", map[string]any{"timelineId": str("timelineId"), "wait": false}), true
		case "render":
			return text("已经剪好并提交导出（导出免费）。可以在画布上打开这个智能剪辑节点继续调整。"), true
		}
		return agentReply{}, false
	}
	if last.Role != "user" || strings.HasPrefix(last.Text, agentSystemEventPrefix) {
		return agentReply{}, false
	}
	switch state {
	case "planning":
		if in.countCalls("submit_plan") > 0 {
			return agentReply{}, false
		}
		if model == "" {
			return text("没有可用的视频模型信息，无法提交规划。"), true
		}
		return call("submit_plan", "先确认这次的规划。", map[string]any{"title": "智能剪辑",
			"items": []any{map[string]any{"type": "video", "modelCode": model,
				"operationCode": "text2video", "durationSec": 5, "title": "成片"}}}), true
	case "executing":
		if in.countCalls("smartcut_create") == 0 {
			args := map[string]any{"title": "智能剪辑 E2E"}
			for _, t := range in.userTexts() {
				if m := reSmartcutInputs.FindStringSubmatch(t); m != nil {
					args["inputNodeIds"] = strings.Split(m[1], ",")
				}
			}
			return call("smartcut_create", "先在画布上建一个智能剪辑，把素材连进来。", args), true
		}
	}
	return agentReply{}, false
}

// agentScript 是决策表本体（纯函数：不碰网络、不读时钟）。
func agentScript(in agentTurnInput) agentReply {
	state := agentState(in.System)
	var last agentMsg
	if n := len(in.Messages); n > 0 {
		last = in.Messages[n-1]
	}
	if last.Role == "user" && strings.Contains(last.Text, magicChatFail) &&
		!strings.HasPrefix(last.Text, agentSystemEventPrefix) {
		return agentReply{Fail: true, Action: "fail"}
	}

	call := func(name, text string, args any) agentReply {
		if !contains(in.Tools, name) {
			// 本次没下发这个工具：老老实实说用不了，不凭历史幻觉出一个工具名。
			return agentReply{chatScript: chatScript{Text: fmt.Sprintf("（当前阶段没有工具 %s，无法继续）", name)},
				Action: "text"}
		}
		return agentReply{chatScript: chatScript{Text: text, ToolName: name, ToolArgs: mustArgs(args)}, Action: name}
	}
	text := func(s string) agentReply { return agentReply{chatScript: chatScript{Text: s}, Action: "text"} }

	model := in.videoModel()
	if isMarketingAgent(in.System) {
		if r, ok := agentMarketingScript(in, call, model); ok {
			return r
		}
	}
	if in.hasMagic(magicSmartcut) {
		if r, ok := agentSmartcutScript(in, call, text, model, state, last); ok {
			return r
		}
	}
	if in.hasMagic(magicDirector) {
		if r, ok := agentDirectorScript(in, call, text, model, state, last); ok {
			return r
		}
	}
	generateArgs := func(wait bool, nodeID string) map[string]any {
		a := map[string]any{"operationCode": "text2video", "modelCode": model,
			"prompt": "雨夜的站台，两人隔着车窗告别，镜头缓慢推近", "durationSec": 5, "wait": wait}
		if nodeID != "" {
			a["nodeId"] = nodeID
		} else {
			a["nodeType"], a["title"] = "video", "成片"
		}
		return a
	}

	switch {
	case last.Role == "tool":
		switch in.toolOf(last.ToolCallID) {
		case "load_skill":
			return call("ask_user", "先确认创作方向。", map[string]any{
				"kind": "choice", "title": "选一个创作方向", "allowOther": true,
				"options": []any{
					map[string]any{"id": "a", "label": "悲壮离别", "detail": "雨夜站台，克制的告别"},
					map[string]any{"id": "b", "label": "温暖重逢", "detail": "清晨车站，久别重逢"},
				},
			})
		case "write_doc":
			if model == "" {
				return text("没有可用的视频模型信息，无法提交规划。")
			}
			return call("submit_plan", "规划已写好，请确认。", map[string]any{
				"title": "单段视频规划",
				"items": []any{map[string]any{"type": "video", "modelCode": model,
					"operationCode": "text2video", "durationSec": 5, "title": "成片"}},
			})
		case "generate":
			if in.hasMagic(magicOverBudget) && in.countCalls("generate") == 1 {
				return call("generate", "再提交一个版本。", generateArgs(true, ""))
			}
			return text("生成已提交。")
		case "canvas_apply":
			return text("已按计划生成 1 条视频，并把规划文档与成片框在一起。\n接下来你可以：\n" +
				"- 把时长改成 10 秒再生成一版\n- 给成片配一段背景音乐")
		}
		return text("已收到工具结果：" + truncate(last.Text, 60) + "。")

	case last.Role == "user" && strings.HasPrefix(last.Text, agentSystemEventPrefix):
		ok := 0
		if m := reWakeSucceeded.FindStringSubmatch(last.Text); m != nil {
			ok, _ = strconv.Atoi(m[1])
		}
		if !reWakeTaskSummary.MatchString(last.Text) || ok == 0 {
			return text("生成没有成功，平台会按原批次退款。\n接下来你可以：\n- 换一个视频模型再试一次\n- 缩短时长后重新规划")
		}
		var members []string
		seen := map[string]bool{}
		add := func(id any) {
			if s, _ := id.(string); s != "" && !seen[s] {
				seen[s] = true
				members = append(members, s)
			}
		}
		for _, r := range in.toolResults("generate") {
			if _, has := r["generationId"]; has {
				add(r["nodeId"])
			}
		}
		for _, r := range in.toolResults("write_doc") {
			add(r["nodeId"])
		}
		if len(members) == 0 {
			return text("任务已完成，但找不到要归组的节点。")
		}
		return call("canvas_apply", "把规划与成片框在一起。", map[string]any{
			"ops": []any{map[string]any{"kind": "add_frame",
				"frame": map[string]any{"title": "单段成片", "members": members}}},
		})

	case last.Role == "user" && state == "executing":
		if model == "" {
			return text("没有可用的视频模型信息，无法生成。")
		}
		// 追加额度之后：用超额那次留下的节点再提交一次。
		nodeID := ""
		for _, r := range in.toolResults("generate") {
			if r["status"] == "over_budget" {
				nodeID, _ = r["nodeId"].(string)
			}
		}
		wait := !in.hasMagic(magicOverBudget) || nodeID != ""
		return call("generate", "开始生成。", generateArgs(wait, nodeID))

	case last.Role == "user" && state == "planning":
		if in.countCalls("load_skill") == 0 {
			return call("load_skill", "先加载单段视频技能。", map[string]any{"code": agentSkillCode})
		}
		if strings.HasPrefix(last.Text, "我选择了：") {
			return call("write_doc", "把规划写进文档。", map[string]any{"path": "/plan.md",
				"content": "# 单段视频规划\n\n- 视频 1 条，5 秒，模型 " + model + "\n- 方向：" +
					strings.TrimPrefix(last.Text, "我选择了：") + "\n"})
		}
		return call("ask_user", "需要再确认一下方向。", map[string]any{
			"kind": "choice", "title": "选一个创作方向",
			"options": []any{map[string]any{"id": "a", "label": "悲壮离别"}, map[string]any{"id": "b", "label": "温暖重逢"}},
		})
	}
	return text(fmt.Sprintf("假上游 Agent 回应（阶段 %q，收到 %d 条消息）。", state, len(in.Messages)))
}

// ---------------------------------------------------------------- 探针

// agentProbeRecord 是一次 Agent 请求的记录。
type agentProbeRecord struct {
	Seq      int64    `json:"seq"`
	Tag      string   `json:"tag"`
	State    string   `json:"state"`
	Scene    string   `json:"scene"`
	Tools    []string `json:"tools"`
	TurnHint string   `json:"turnHint"`
	LastRole string   `json:"lastRole"`
	At       string   `json:"at"`
	// Images：本次请求里的图片块总数；FirstUserImages：首条用户消息上的图片块数；
	// HTTPImages：以外链发出的图片块数；URLs：请求体里出现的 http(s) 地址（MKT-T-30）。
	Images          int      `json:"images"`
	FirstUserImages int      `json:"firstUserImages"`
	HTTPImages      int      `json:"httpImages"`
	URLs            []string `json:"urls"`
	// Audios：本次请求里的音频块总数 + 请求体里内联音频（data:audio/）的次数（MKT-T-41，MKT-AC-45）。
	Audios int `json:"audios"`
	// ToolArgs 是本次回的工具参数（营销 E2E 核对 submit_plan / generate 的形状）。
	ToolArgs string `json:"toolArgs,omitempty"`
}

const agentProbeKeep = 500

type agentProbe struct {
	mu   sync.Mutex
	seq  atomic.Int64
	recs []agentProbeRecord
}

func (p *agentProbe) record(in agentTurnInput, reply agentReply) {
	lastRole := ""
	if n := len(in.Messages); n > 0 {
		lastRole = in.Messages[n-1].Role
		if lastRole == "user" && strings.HasPrefix(in.Messages[n-1].Text, agentSystemEventPrefix) {
			lastRole = "system_event"
		}
	}
	tools := append([]string{}, in.Tools...)
	rec := agentProbeRecord{Seq: p.seq.Add(1), Tag: agentTag(in), State: agentState(in.System),
		Scene: agentScene(in.System), Tools: tools, TurnHint: reply.Action, LastRole: lastRole,
		At: time.Now().UTC().Format(time.RFC3339Nano), URLs: append([]string{}, in.BodyURLs...),
		ToolArgs: reply.ToolArgs}
	if rec.URLs == nil {
		rec.URLs = []string{}
	}
	rec.Audios = in.BodyAudioData
	firstUser := true
	for _, m := range in.Messages {
		rec.Images += m.Images
		rec.HTTPImages += m.HTTPImages
		rec.Audios += m.Audios
		if m.Role == "user" && firstUser {
			rec.FirstUserImages = m.Images
			firstUser = false
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.recs = append(p.recs, rec)
	if len(p.recs) > agentProbeKeep {
		p.recs = append([]agentProbeRecord(nil), p.recs[len(p.recs)-agentProbeKeep:]...)
	}
}

// list 按 tag 过滤（空 = 全部），返回最近 limit 条（旧在前）。
func (p *agentProbe) list(tag string, limit int) []agentProbeRecord {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := []agentProbeRecord{}
	for _, r := range p.recs {
		if tag == "" || r.Tag == tag {
			out = append(out, r)
		}
	}
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out
}

func (p *agentProbe) handle(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	writeJSON(w, map[string]any{"items": p.list(r.URL.Query().Get("tag"), limit)})
}

// ---------------------------------------------------------------- 门控视频（AGT-31）

// 门控异步视频：提交后一直「处理中」，直到脚本调用 POST /_probe/gated/release 放行。
// 用来把会话**确定地**停在 waiting_task —— 在那段时间里移除发起人（E2E-06）、
// 或由用户自己保存画布（E2E-03），而不靠 sleep 去碰运气。
// ⚠️ release 放行的是**当时已提交**的全部门控任务；之后提交的仍然挂着。

func (s *server) gatedSubmit(w http.ResponseWriter, _ *http.Request) {
	id := fmt.Sprintf("gated-%d", s.seq.Add(1))
	s.gated.Store(id, new(atomic.Bool))
	writeJSON(w, map[string]any{"task_id": id})
}

func (s *server) gatedQuery(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/v1/gated/query/")
	v, ok := s.gated.Load(id)
	if !ok {
		http.Error(w, "任务不存在", http.StatusNotFound)
		return
	}
	if !v.(*atomic.Bool).Load() {
		writeJSON(w, map[string]any{"status": "processing", "progress": 10})
		return
	}
	writeJSON(w, map[string]any{"status": "succeeded",
		"content": map[string]any{"video_url": s.addr + "/files/out.mp4"}})
}

func (s *server) gatedRelease(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	n := 0
	s.gated.Range(func(_, v any) bool {
		if v.(*atomic.Bool).CompareAndSwap(false, true) {
			n++
		}
		return true
	})
	writeJSON(w, map[string]any{"released": n})
}
