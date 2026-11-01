package app

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/li904338203/tabtv/backend/internal/agent/domain"
	"github.com/li904338203/tabtv/backend/internal/agent/repo"
)

func msg(seq int, role, content string) repo.Message {
	return repo.Message{Seq: seq, Role: role, Content: content, Refs: json.RawMessage(`[]`)}
}

func TestBuildContext_按字符预算从新往旧截断(t *testing.T) {
	msgs := []repo.Message{
		msg(1, repo.RoleUser, strings.Repeat("甲", 10)),
		msg(2, repo.RoleAssistant, strings.Repeat("乙", 10)),
		msg(3, repo.RoleUser, strings.Repeat("丙", 10)),
		msg(4, repo.RoleAssistant, strings.Repeat("丁", 10)),
	}
	// 预算 20：只能放下最新的两条（按码点，不按字节）。
	out := BuildContext(ContextInput{State: domain.StatePlanning, Messages: msgs, Budget: 20})
	if !out.Truncated || len(out.Messages) != 2 {
		t.Fatalf("应截成 2 条：truncated=%v n=%d", out.Truncated, len(out.Messages))
	}
	if out.Messages[0].Content != strings.Repeat("丙", 10) || out.Messages[1].Content != strings.Repeat("丁", 10) {
		t.Errorf("应保留最新的两条且保持升序：%+v", out.Messages)
	}
	// 边界：恰好等于预算不截。
	if out := BuildContext(ContextInput{Messages: msgs, Budget: 40}); out.Truncated || len(out.Messages) != 4 {
		t.Errorf("恰好用满预算不应截断：%v %d", out.Truncated, len(out.Messages))
	}
	// 边界：预算 39 截掉最旧的一条。
	if out := BuildContext(ContextInput{Messages: msgs, Budget: 39}); !out.Truncated || len(out.Messages) != 3 {
		t.Errorf("差一个字符应截掉最旧的一条：%v %d", out.Truncated, len(out.Messages))
	}
	// 最新一条超长也保留（那是用户刚说的话）。
	long := []repo.Message{msg(1, repo.RoleUser, "旧"), msg(2, repo.RoleUser, strings.Repeat("新", 100))}
	if out := BuildContext(ContextInput{Messages: long, Budget: 10}); len(out.Messages) != 1 ||
		out.Messages[0].Content != strings.Repeat("新", 100) {
		t.Errorf("最新一条应无条件保留：%+v", out.Messages)
	}
	// 保留段不能以 tool 消息开头。
	calls, _ := json.Marshal([]ToolCall{{ID: "c1", Name: "read_canvas", Arguments: json.RawMessage(`{}`)}})
	withTool := []repo.Message{
		msg(1, repo.RoleUser, strings.Repeat("甲", 30)),
		{Seq: 2, Role: repo.RoleAssistant, ToolCalls: calls},
		{Seq: 3, Role: repo.RoleTool, ToolCallID: "c1", Content: "{}"},
		msg(4, repo.RoleAssistant, "好"),
	}
	out = BuildContext(ContextInput{Messages: withTool, Budget: 5})
	if len(out.Messages) == 0 || out.Messages[0].Role == "tool" {
		t.Errorf("保留段不得以 tool 开头：%+v", out.Messages)
	}
	// system_note 不进模型上下文。
	notes := []repo.Message{msg(1, repo.RoleSystemNote, "提示"), msg(2, repo.RoleUser, "你好")}
	if out := BuildContext(ContextInput{Messages: notes, Budget: 100}); len(out.Messages) != 1 {
		t.Errorf("system_note 不应发给模型：%+v", out.Messages)
	}
}

// 唤醒轮靠 system_event 知道所等任务的成败与产出；漏了它，续跑的总结就是盲写的。
func TestBuildContext_系统事件以user角色进模型而system_note不进(t *testing.T) {
	msgs := []repo.Message{
		msg(1, repo.RoleUser, "做个视频"),
		msg(2, repo.RoleSystemNote, "上下文已截断"),
		msg(3, repo.RoleSystemEvent, "所等的 1 个任务已全部结束：成功 1"),
	}
	out := BuildContext(ContextInput{Messages: msgs, Budget: 1000})
	if len(out.Messages) != 2 {
		t.Fatalf("应只有用户消息与系统事件两条：%+v", out.Messages)
	}
	ev := out.Messages[1]
	if ev.Role != "user" || !strings.HasPrefix(ev.Content, SystemEventPrefix) ||
		!strings.Contains(ev.Content, "成功 1") {
		t.Errorf("系统事件应以 user 角色、带前缀交给模型：%+v", ev)
	}
	for _, m := range out.Messages {
		if strings.Contains(m.Content, "上下文已截断") {
			t.Errorf("system_note 不应发给模型：%+v", m)
		}
	}
}

func TestBuildContext_截断后带早先摘要(t *testing.T) {
	msgs := []repo.Message{
		msg(1, repo.RoleUser, "我想做一个关于橘猫冒险的短片"),
		msg(2, repo.RoleAssistant, "好的，先确认风格"),
		msg(3, repo.RoleUser, strings.Repeat("新", 30)),
	}
	out := BuildContext(ContextInput{State: domain.StatePlanning, Messages: msgs, Budget: 30})
	if !out.Truncated || !strings.Contains(out.Summary, "橘猫冒险") || !strings.Contains(out.Summary, "先确认风格") {
		t.Fatalf("摘要应包含被截的内容：%q", out.Summary)
	}
	if !strings.Contains(out.System, "## 早先对话摘要") || !strings.Contains(out.System, "橘猫冒险") {
		t.Errorf("系统提示词应带上摘要")
	}
	// 只是没取出更早的（HasOlder）而本次没有新截掉的：沿用缓存的摘要。
	out = BuildContext(ContextInput{Messages: msgs[2:], HasOlder: true, PrevSummary: "缓存的摘要", Budget: 100})
	if !out.Truncated || out.Summary != "缓存的摘要" {
		t.Errorf("应沿用缓存摘要：%+v", out)
	}
	// 未截断时不带摘要段。
	out = BuildContext(ContextInput{Messages: msgs, Budget: 1000, PrevSummary: "旧摘要"})
	if out.Truncated || strings.Contains(out.System, "早先对话摘要") {
		t.Errorf("未截断不应带摘要")
	}
}

func TestBuildContext_画布摘要不含params全文(t *testing.T) {
	canvas := json.RawMessage(`{"nodes":[
		{"id":"n1","type":"image","title":"橘猫","params":{"prompt":"一只非常长的提示词秘密内容XYZ"},
		 "result":{"status":"success"},"agent":{"sessionId":"9"}},
		{"id":"n2","type":"video","title":"冒险","params":{"model":"seedance"}}
	],"edges":[{"id":"e1"}]}`)
	out := BuildContext(ContextInput{State: domain.StateExecuting, CanvasData: canvas, Budget: 100})
	if strings.Contains(out.System, "秘密内容XYZ") || strings.Contains(out.System, "seedance") {
		t.Fatalf("画布摘要不得含 params：%s", out.System)
	}
	for _, want := range []string{`"id":"n1"`, `"title":"橘猫"`, `"status":"success"`, `"byAgent":true`,
		`"id":"n2"`, "共 2 个节点、1 条连线"} {
		if !strings.Contains(out.System, want) {
			t.Errorf("摘要缺少 %s", want)
		}
	}
	if !strings.Contains(out.System, domain.ExecutingModeNote) {
		t.Errorf("应带执行阶段说明")
	}
	if got := CanvasSummaryText(nil); got != "（画布为空）" {
		t.Errorf("空画布：%q", got)
	}
}

func TestBuildContext_技能与记忆进系统提示词(t *testing.T) {
	out := BuildContext(ContextInput{State: domain.StatePlanning, Budget: 100, Memory: "用户喜欢水彩风",
		Skills: []SkillPrompt{{Code: "story", Name: "故事选题", Body: "先给三个方向"}}})
	for _, want := range []string{"## 已加载技能：故事选题", "先给三个方向", "## 项目记忆", "用户喜欢水彩风"} {
		if !strings.Contains(out.System, want) {
			t.Errorf("系统提示词缺少 %s", want)
		}
	}
}
