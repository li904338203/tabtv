package app

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/li904338203/tabtv/backend/internal/agent/domain"
	"github.com/li904338203/tabtv/backend/internal/agent/repo"
)

// 上下文组装（dev/01 §4.4）。全部是纯函数：给定会话的快照，产出发给模型的
// system + messages —— 截断边界、摘要、画布摘要都能被单测钉住。

// MemoryDocPath 是项目记忆文档的固定路径（canvas/domain 的白名单里有它）。
const MemoryDocPath = "/.memory.md"

const (
	// maxCanvasSummaryNodes 画布摘要最多列多少个节点：再多模型也用不上，要细节调 read_canvas。
	maxCanvasSummaryNodes = 200
	// maxSummaryRunes 早先对话摘要的上限。
	maxSummaryRunes = 2000
	// summaryLineRunes 摘要里每条被截消息保留的字数。
	summaryLineRunes = 80
	// unfinishedToolResult 是「助手发起了工具调用、却没有对应结果」时补的结果：
	// OpenAI 要求每个 tool_call 都有一条 tool 消息回应，缺一条整轮请求 400。
	unfinishedToolResult = `{"error":"该调用没有执行（本轮被停止、挂起或中断）"}`
)

// SkillPrompt 是一个已加载技能的主说明（已渲染）。
type SkillPrompt struct {
	Code string
	Name string
	Body string
}

// ContextInput 是组装上下文的输入快照。
type ContextInput struct {
	State  domain.State
	Skills []SkillPrompt
	// Memory 项目记忆（/.memory.md 正文）；空 = 没有。
	Memory string
	// CanvasData 画布原文；只取摘要，不含 params 全文。
	CanvasData json.RawMessage
	// Messages 会话消息，按 seq 升序（可能只是最近的一段）。
	Messages []repo.Message
	// HasOlder：Messages 之前还有更早的消息没有取出来（同样算「被截掉」）。
	HasOlder bool
	// PrevSummary 会话上缓存的早先对话摘要。
	PrevSummary string
	// Budget 会话消息的字符预算（按 Unicode 码点）。
	Budget int
	// VideoModel 「当前视频模型」一节（VideoModelSection 的结果）；空 = 不写这一节。
	VideoModel string
	// Prefs 「创作偏好」一节（PrefsSection 的结果）；空 = 不写这一节。
	Prefs string
	// Scene 会话场景（MKT-T-18）。general / 空 = 不写场景段与营销输入段（通用会话请求体逐字不变）。
	Scene domain.Scene
	// SceneNote 场景段正文（模板 agent.scene.marketing 已渲染；见 sceneSections）。
	SceneNote string
	// SceneInput 「## 营销输入」正文（marketing/domain.SlotSummary 的结果，不含标题）。
	SceneInput string
	// FirstUserImages 附在首条用户消息（ID = FirstUserMessageID）上的图（MKT-T-19，data: 内联）。
	// 只有营销会话会给；该消息不在本次上下文里时不附（调用方用 firstUserKept 判，张数与提示词一致）。
	FirstUserMessageID int64
	FirstUserImages    []string
}

// ContextOutput 是组装结果。
type ContextOutput struct {
	System   string
	Messages []ChatMessage
	// Truncated 发生了截断（被截部分以摘要代替）。
	Truncated bool
	// Summary 本次使用的早先对话摘要（未截断时为空）。
	Summary string
}

// ConversationOrder 把按 seq 取出的消息整理成发给模型的会话顺序（04 文档 P-01）：
//
//   - 丢掉**本轮之后才建的轮**的消息。运行中用户又发的消息会排队成下一轮，它的 seq 当场就分配了，
//     但还没轮到它：本轮看到它就会提前回答，下一轮（又一次计费）再答一遍。
//   - 按轮分组，组内按 seq 排。轮 ID 是雪花号，随创建时间递增。排队消息的 seq 会夹在上一轮的
//     assistant(tool_calls) 与它的 tool 结果之间；原样发出去的话，OpenAI 系协议要求 tool 紧跟
//     tool_calls，会回 400。而且这个顺序已经落库，此后每一轮都 400，直到它被截出窗口。
//
// 没有轮的消息（TurnID=0）跟着它前一条消息所在的组。currentTurnID 为 0 时不过滤。
func ConversationOrder(msgs []repo.Message, currentTurnID int64) []repo.Message {
	type keyed struct {
		m   repo.Message
		key int64
	}
	items := make([]keyed, 0, len(msgs))
	var last int64
	for _, m := range msgs {
		k := m.TurnID
		if k == 0 {
			k = last
		} else {
			last = k
		}
		if currentTurnID != 0 && k > currentTurnID {
			continue
		}
		items = append(items, keyed{m: m, key: k})
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].key < items[j].key })
	out := make([]repo.Message, len(items))
	for i, it := range items {
		out[i] = it.m
	}
	return out
}

// BuildContext 组装一次模型调用的上下文。
//
// 会话消息从最新往最旧取，直到字符预算；被截掉的部分以「早先对话摘要」代替。
// ⚠️ 截断的单位是**整条消息**，且保留下来的第一条不能是 tool 消息（它回应的助手消息
// 被截掉了，单独发出去两家协议都会拒）。
func BuildContext(in ContextInput) ContextOutput {
	kept, dropped := truncateMessages(in.Messages, in.Budget)
	truncated := len(dropped) > 0 || in.HasOlder
	summary := ""
	if truncated {
		summary = BuildSummary(dropped, in.PrevSummary)
	}
	return ContextOutput{
		System:    buildSystem(in, summary),
		Messages:  toChatMessagesWith(kept, in.FirstUserMessageID, in.FirstUserImages),
		Truncated: truncated,
		Summary:   summary,
	}
}

func buildSystem(in ContextInput, summary string) string {
	var b strings.Builder
	b.WriteString(domain.SystemPromptSkeleton)
	if note := domain.ModeNote(in.State); note != "" {
		b.WriteString("\n\n")
		b.WriteString(note)
	}
	// ③④ 场景段与营销输入段（MKT-T-18，dev/01 §3.3）：**只追加**，骨架与阶段标题不动（MKD-14）。
	// 标题由代码写（假上游按它分流），通用会话 SceneHeading 为空、两段都不写。
	if h := domain.SceneHeading(in.Scene); h != "" {
		b.WriteString("\n\n")
		b.WriteString(h)
		if t := strings.TrimSpace(in.SceneNote); t != "" {
			b.WriteString("\n")
			b.WriteString(t)
		}
		if t := strings.TrimSpace(in.SceneInput); t != "" {
			b.WriteString("\n\n")
			b.WriteString(domain.MarketingInputHeading)
			b.WriteString("\n")
			b.WriteString(t)
		}
	}
	for _, s := range in.Skills {
		if strings.TrimSpace(s.Body) == "" {
			continue
		}
		fmt.Fprintf(&b, "\n\n## 已加载技能：%s\n%s", firstNonEmpty(s.Name, s.Code), s.Body)
	}
	if m := strings.TrimSpace(in.Memory); m != "" {
		b.WriteString("\n\n## 项目记忆（" + MemoryDocPath + "）\n")
		b.WriteString(m)
	}
	if v := strings.TrimSpace(in.VideoModel); v != "" {
		b.WriteString("\n\n## 当前视频模型\n")
		b.WriteString(v)
	}
	if v := strings.TrimSpace(in.Prefs); v != "" {
		b.WriteString("\n\n## 创作偏好\n")
		b.WriteString(v)
	}
	b.WriteString("\n\n## 画布摘要\n")
	b.WriteString(CanvasSummaryText(in.CanvasData))
	if summary != "" {
		b.WriteString("\n\n## 早先对话摘要\n")
		b.WriteString(summary)
	}
	return b.String()
}

// truncateMessages 从最新往最旧取，直到预算用完。返回保留的（升序）与被截掉的（升序）。
//
// ⚠️ 最新的一条**无论多长都保留**：那是用户刚说的话，截掉它模型就不知道要做什么。
func truncateMessages(msgs []repo.Message, budget int) (kept, dropped []repo.Message) {
	var visible []repo.Message
	for _, m := range msgs {
		if m.Role == repo.RoleSystemNote {
			continue // 只给用户看的提示，不进模型上下文
		}
		visible = append(visible, m)
	}
	used, cut := 0, 0
	for i := len(visible) - 1; i >= 0; i-- {
		n := messageChars(visible[i])
		if i < len(visible)-1 && used+n > budget {
			cut = i + 1
			break
		}
		used += n
	}
	// 保留段不能以 tool 消息开头（它回应的助手消息被截掉了）。
	for cut < len(visible) && visible[cut].Role == repo.RoleTool {
		cut++
	}
	return visible[cut:], visible[:cut]
}

func messageChars(m repo.Message) int {
	n := utf8.RuneCountInString(m.Content)
	if len(m.ToolCalls) > 0 {
		n += utf8.RuneCount(m.ToolCalls)
	}
	if len(m.Refs) > 2 {
		n += utf8.RuneCount(m.Refs)
	}
	return n
}

// BuildSummary 生成被截部分的摘要（不调用模型：每条取开头若干字）。
//
// ⚠️ 不调用模型：摘要在截断发生时每轮都可能变，为它再扣一次费或再等一次上游都不值。
// 被截的消息为空（只因为没取出更早的）时沿用会话上缓存的摘要。
func BuildSummary(dropped []repo.Message, prev string) string {
	if len(dropped) == 0 {
		return prev
	}
	var b strings.Builder
	b.WriteString("（较早的对话已省略，以下为要点节选）")
	for _, m := range dropped {
		if m.Role != repo.RoleUser && m.Role != repo.RoleAssistant {
			continue
		}
		text := strings.TrimSpace(strings.ReplaceAll(m.Content, "\n", " "))
		if text == "" {
			continue
		}
		who := "用户"
		if m.Role == repo.RoleAssistant {
			who = "助手"
		}
		line := "\n- " + who + "：" + truncRunes(text, summaryLineRunes)
		if utf8.RuneCountInString(b.String())+utf8.RuneCountInString(line) > maxSummaryRunes {
			b.WriteString("\n- ……")
			break
		}
		b.WriteString(line)
	}
	return b.String()
}

// SystemEventPrefix 标在系统事件前面，让模型知道这不是用户说的话。
const SystemEventPrefix = "【系统通知，非用户发言】\n"

// toChatMessages 把库里的消息转成模型消息，并补齐没有结果的工具调用。
func toChatMessages(msgs []repo.Message) []ChatMessage {
	return toChatMessagesWith(msgs, 0, nil)
}

// toChatMessagesWith 同 toChatMessages，并把 images 挂到 ID = imageMsgID 的那条用户消息上（MKT-T-19，
// 营销会话首条消息附商品图）。imageMsgID 为 0 / 没有图时与 toChatMessages 逐字相同。
func toChatMessagesWith(msgs []repo.Message, imageMsgID int64, images []string) []ChatMessage {
	out := make([]ChatMessage, 0, len(msgs))
	answered := map[string]bool{}
	for _, m := range msgs {
		if m.Role == repo.RoleTool && m.ToolCallID != "" {
			answered[m.ToolCallID] = true
		}
	}
	for _, m := range msgs {
		switch m.Role {
		case repo.RoleUser:
			c := m.Content
			if len(m.Refs) > 2 { // 非空数组
				c += "\n\n[引用] " + refsForModel(m.Refs)
			}
			cm := ChatMessage{Role: "user", Content: c}
			if imageMsgID != 0 && m.ID == imageMsgID && len(images) > 0 {
				cm.Images = append([]string(nil), images...)
			}
			out = append(out, cm)
		case repo.RoleAssistant:
			calls := decodeToolCalls(m.ToolCalls)
			out = append(out, ChatMessage{Role: "assistant", Content: m.Content, ToolCalls: calls})
			for _, c := range calls {
				if !answered[c.ID] {
					out = append(out, ChatMessage{Role: "tool", ToolCallID: c.ID, Content: unfinishedToolResult})
					answered[c.ID] = true
				}
			}
		case repo.RoleTool:
			out = append(out, ChatMessage{Role: "tool", ToolCallID: m.ToolCallID, Content: m.Content})
		case repo.RoleSystemEvent:
			// 协议里没有「系统事件」角色，中途的 system 消息 Anthropic 也不收；
			// 以 user 角色交给模型，前缀说明这不是用户的发言。
			out = append(out, ChatMessage{Role: "user", Content: SystemEventPrefix + m.Content})
		}
	}
	return out
}

// refsForModel 是附在用户消息后面给模型看的引用：商品库 / 角色库的引用去掉 id，其余逐字不变。
//
// ⚠️ 起因（会话 107398945801502720）：原样附上 `{"id":"107396993168441344","kind":"marketing_product",…}`，
// 模型把这个商品 ID 当成 referenceId 写进 generate 的 inputs，被判 plan_item_mismatch。
// 没有任何工具收商品库 / 角色库的 ID：商品与角色的内容在「营销输入」里，商品图随首条消息附上，
// 生成时服务端按计划项组装输入 —— 这个 ID 给了模型只会被误用。
func refsForModel(raw json.RawMessage) string {
	var refs []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &refs); err != nil {
		return string(raw)
	}
	changed := false
	for _, r := range refs {
		var kind string
		_ = json.Unmarshal(r["kind"], &kind)
		if _, has := r["id"]; has && (kind == RefMarketingProduct || kind == RefMarketingCharacter) {
			delete(r, "id")
			changed = true
		}
	}
	if !changed {
		return string(raw)
	}
	b, err := json.Marshal(refs)
	if err != nil {
		return string(raw)
	}
	return string(b)
}

func decodeToolCalls(raw json.RawMessage) []ToolCall {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var calls []ToolCall
	if err := json.Unmarshal(raw, &calls); err != nil {
		return nil
	}
	return calls
}

// SummarizeCanvas 取画布摘要：节点 ID / 类型 / 标题 / 状态 / 是否 Agent 创建，**不含 params**。
//
// ⚠️ params 全文不进上下文：提示词、参考图 ID、预设配置加起来一个节点就能上千字，
// 几十个节点就把字符预算吃光；需要细节时模型调 read_canvas。
func SummarizeCanvas(raw json.RawMessage) (nodes []CanvasNodeSummary, edges int) {
	var doc struct {
		Nodes []map[string]any  `json:"nodes"`
		Edges []json.RawMessage `json:"edges"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &doc) != nil {
		return nil, 0
	}
	for _, n := range doc.Nodes {
		s := CanvasNodeSummary{ID: str(n["id"]), Type: str(n["type"]), Title: str(n["title"])}
		if r, ok := n["result"].(map[string]any); ok {
			s.Status = str(r["status"])
		}
		if _, ok := n["agent"].(map[string]any); ok {
			s.ByAgent = true
		}
		nodes = append(nodes, s)
	}
	return nodes, len(doc.Edges)
}

// CanvasSummaryText 把画布摘要写成模型可读的文本（每行一个 JSON 节点）。
func CanvasSummaryText(raw json.RawMessage) string {
	nodes, edges := SummarizeCanvas(raw)
	if len(nodes) == 0 {
		return "（画布为空）"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "共 %d 个节点、%d 条连线。", len(nodes), edges)
	for i, n := range nodes {
		if i >= maxCanvasSummaryNodes {
			fmt.Fprintf(&b, "\n……其余 %d 个节点省略，需要时调用 read_canvas", len(nodes)-i)
			break
		}
		line, _ := json.Marshal(n)
		b.WriteString("\n")
		b.Write(line)
	}
	return b.String()
}

func str(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case json.Number:
		return x.String()
	case float64:
		return fmt.Sprintf("%v", x)
	}
	return ""
}

func truncRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n]) + "…"
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}
