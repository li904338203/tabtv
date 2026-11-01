package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/li904338203/tabtv/backend/internal/agent/domain"
	"github.com/li904338203/tabtv/backend/internal/agent/repo"
)

// 04 文档第 5 节第一批修复（P-01 / P-02 / P-03 / P-09 / P-10 / P-11）的行为测试。

func ids(msgs []repo.Message) []int64 {
	out := make([]int64, len(msgs))
	for i, m := range msgs {
		out[i] = m.ID
	}
	return out
}

func sameIDs(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// P-01：运行中用户又发的消息（下一轮）seq 夹在本轮的 tool_calls 与 tool 结果之间。
func TestConversationOrder_排队的后续轮不进本轮且按轮分组(t *testing.T) {
	msgs := []repo.Message{
		{ID: 1, Seq: 1, TurnID: 10, Role: repo.RoleUser, Content: "A 的问题"},
		{ID: 2, Seq: 2, TurnID: 10, Role: repo.RoleAssistant,
			ToolCalls: json.RawMessage(`[{"id":"c1","name":"read_canvas","arguments":{}}]`)},
		{ID: 3, Seq: 3, TurnID: 20, Role: repo.RoleUser, Content: "B 的问题（排队）"},
		{ID: 4, Seq: 4, TurnID: 10, Role: repo.RoleTool, ToolCallID: "c1", Content: `{}`},
		{ID: 5, Seq: 5, TurnID: 10, Role: repo.RoleAssistant, Content: "A 的回答"},
		{ID: 6, Seq: 6, Role: repo.RoleSystemNote, Content: "没有轮的消息跟着前一条"},
	}
	if got := ids(ConversationOrder(msgs, 10)); !sameIDs(got, []int64{1, 2, 4, 5, 6}) {
		t.Fatalf("本轮（10）不该看到排队的下一轮的消息：%v", got)
	}
	if got := ids(ConversationOrder(msgs, 20)); !sameIDs(got, []int64{1, 2, 4, 5, 6, 3}) {
		t.Fatalf("轮到 20 时应按轮分组，上一轮整段在前：%v", got)
	}
	if got := ids(ConversationOrder(msgs, 0)); len(got) != len(msgs) {
		t.Fatalf("currentTurnID=0 不过滤：%v", got)
	}

	// 发给模型时 tool 结果紧跟它的 tool_calls（OpenAI 系协议的硬要求）。
	chat := toChatMessages(ConversationOrder(msgs, 20))
	for i, m := range chat {
		if m.Role == "assistant" && len(m.ToolCalls) > 0 {
			if i+1 >= len(chat) || chat[i+1].Role != "tool" || chat[i+1].ToolCallID != "c1" {
				t.Fatalf("tool 结果没有紧跟 tool_calls：%+v", chat)
			}
		}
	}
}

func TestRunTurn_运行中排队的消息不进本轮上下文(t *testing.T) {
	h := newHarness(domain.StatePlanning, domain.TriggerUser, 2, 100)
	h.store.msgs = append(h.store.msgs, repo.Message{ID: 2, SessionID: hSession, Seq: 2, TurnID: hTurn + 1,
		Role: repo.RoleUser, Content: "这是下一轮才该回答的问题", Refs: json.RawMessage(`[]`)})
	h.chat.replies = []fakeReply{{out: ChatOutput{Text: "好的"}}}
	if _, err := h.run(); err != nil {
		t.Fatal(err)
	}
	for _, m := range h.chat.calls()[0].Messages {
		if strings.Contains(m.Content, "下一轮才该回答") {
			t.Fatal("本轮的模型请求里出现了排队给下一轮的消息")
		}
	}
}

// P-03：消费者上下文被退出信号取消时，不能记成 model_error 把会话打成 failed。
func TestRunTurn_Worker退出时不结算并交还租约(t *testing.T) {
	h := newHarness(domain.StatePlanning, domain.TriggerUser, 2, 100)
	h.chat.replies = []fakeReply{{deltas: []string{"正在想"}, waitCtx: true}}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	out, err := h.runner.RunTurn(ctx, hSession, hTurn)
	if out != OutcomeRequeue || err == nil {
		t.Fatalf("应放回队列交给接手者：outcome=%v err=%v", out, err)
	}
	tr := h.turn()
	if tr.Status != domain.TurnRunning {
		t.Fatalf("轮次应保持 running（接手者续跑），实际 %s / %s", tr.Status, tr.ErrorCode)
	}
	s := h.session()
	if s.State != domain.StatePlanning {
		t.Fatalf("会话状态不该变（原先会进 failed），实际 %s", s.State)
	}
	if s.RunnerLeaseUntil != nil {
		t.Fatal("租约应交还，接手者不必等租约过期")
	}
	if len(h.ledger.refunded) != 0 || h.ledger.consumed["agent_turn:200"] != 2 {
		t.Fatalf("不结算：不退款、已扣的照旧：%+v %+v", h.ledger.consumed, h.ledger.refunded)
	}
	if as := h.store.messagesByRole(repo.RoleAssistant); len(as) != 0 {
		t.Fatalf("被打断的半截文本不落档：%+v", as)
	}

	// 接手的 Worker 用原轮次续跑：不再扣费，正常结束。
	h.chat.replies = []fakeReply{{out: ChatOutput{Text: "接着刚才的说"}}}
	if out, err := h.run(); err != nil || out != OutcomeDone {
		t.Fatalf("接手续跑：outcome=%v err=%v", out, err)
	}
	if tr := h.turn(); tr.Status != domain.TurnSuccess {
		t.Fatalf("续跑后应成功：%+v", tr)
	}
	if h.ledger.consumed["agent_turn:200"] != 2 || h.ledger.balance != 98 {
		t.Fatalf("不能扣第二次：%+v 余额 %d", h.ledger.consumed, h.ledger.balance)
	}
}

type fixedLevel int64

func (l fixedLevel) MemberLevelID(context.Context, int64) int64 { return int64(l) }

// recordTool 记下每次被调用时的运行上下文。
type recordTool struct {
	fakeTool
	seen []RunContext
}

func (t *recordTool) Run(_ context.Context, rc RunContext, _ json.RawMessage) (ToolResult, error) {
	t.seen = append(t.seen, rc)
	return ToolResult{Content: json.RawMessage(`{"ok":true}`)}, nil
}

// P-02：接手 running 的轮会跳过 charge()，会员等级原先只在 charge 里取。
func TestRunTurn_接手的轮按付费主体的会员等级调用工具(t *testing.T) {
	h := newHarness(domain.StatePlanning, domain.TriggerUser, 2, 100)
	tr := h.store.turns[hTurn]
	tr.Status, tr.CreditsCost, tr.ModelCode = domain.TurnRunning, 2, "m1"
	rec := &recordTool{fakeTool: fakeTool{name: domain.ToolReadCanvas}}
	h.tools[domain.ToolReadCanvas] = rec
	h.runner = NewRunner(RunnerDeps{
		Store: h.store, Chat: h.chat, Quoter: fakeQuoter{credits: 2}, Ledger: h.ledger,
		Members: h.members, MemberLevels: fixedLevel(7), Events: h.events, Tools: h.tools,
		NextID: func() int64 { h.nextID++; return h.nextID },
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}, RunnerConfig{MaxCalls: 5, ContextChars: 60000})
	h.chat.replies = []fakeReply{
		{out: ChatOutput{ToolCalls: []ToolCall{toolCall("c1", "read_canvas", `{}`)}}},
		{out: ChatOutput{Text: "看完了"}},
	}
	if _, err := h.run(); err != nil {
		t.Fatal(err)
	}
	if len(rec.seen) != 1 || rec.seen[0].MemberLevelID != 7 {
		t.Fatalf("接手的轮应按会员等级 7 调工具，实际 %+v", rec.seen)
	}
}

// P-09：截断时工具参数多半不完整，照着执行会产出残缺的文档 / 计划。
func TestRunTurn_输出被截断时这批工具调用不执行并告诉模型(t *testing.T) {
	h := newHarness(domain.StatePlanning, domain.TriggerUser, 2, 100)
	h.chat.replies = []fakeReply{
		{out: ChatOutput{FinishReason: FinishLength,
			ToolCalls: []ToolCall{toolCall("c1", "write_doc", `{"path":"/plan.md","content":"写到一半`)}}},
		{out: ChatOutput{Text: "我拆小一点再写。"}},
	}
	if _, err := h.run(); err != nil {
		t.Fatal(err)
	}
	if ft := h.tools[domain.ToolWriteDoc].(*fakeTool); ft.runs != 0 {
		t.Fatalf("被截断的调用不该执行，实际执行了 %d 次", ft.runs)
	}
	calls := h.chat.calls()
	if len(calls) != 2 {
		t.Fatalf("应接着调一次模型让它拆小重来，实际 %d 次", len(calls))
	}
	var answered bool
	for _, m := range calls[1].Messages {
		if m.Role == "tool" && m.ToolCallID == "c1" && strings.Contains(m.Content, "长度上限") {
			answered = true
		}
	}
	if !answered {
		t.Fatal("模型应收到「被截断、没有执行」的工具结果")
	}
	if failed := h.store.stepsByStatus(repo.StepFailed); len(failed) != 1 || failed[0].Tool != "write_doc" {
		t.Fatalf("应留下一个失败步骤：%+v", failed)
	}
}

func TestRunTurn_纯文本被截断时写说明后结束(t *testing.T) {
	h := newHarness(domain.StatePlanning, domain.TriggerUser, 2, 100)
	h.chat.replies = []fakeReply{{out: ChatOutput{Text: "一段很长的回答……", FinishReason: FinishLength}}}
	if _, err := h.run(); err != nil {
		t.Fatal(err)
	}
	if n := len(h.chat.calls()); n != 1 {
		t.Fatalf("纯文本截断不再继续调模型，实际 %d 次", n)
	}
	notes := h.store.messagesByRole(repo.RoleSystemNote)
	if len(notes) != 1 || !strings.Contains(notes[0].Content, "长度上限") {
		t.Fatalf("应给用户写一条截断说明：%+v", notes)
	}
	if tr := h.turn(); tr.Status != domain.TurnSuccess {
		t.Fatalf("有输出就算成功：%+v", tr)
	}
}

// P-11：有的中转不回 tool_call 的 id；不补的话下一次请求回填结果时构造请求失败，会话从此坏掉。
func TestRunTurn_缺id的工具调用补上id且结果对得上(t *testing.T) {
	h := newHarness(domain.StatePlanning, domain.TriggerUser, 2, 100)
	h.chat.replies = []fakeReply{
		{out: ChatOutput{ToolCalls: []ToolCall{toolCall("", "read_canvas", `{}`), toolCall("", "read_doc",
			`{"path":"/plan.md"}`)}}},
		{out: ChatOutput{Text: "好"}},
	}
	if _, err := h.run(); err != nil {
		t.Fatal(err)
	}
	second := h.chat.calls()[1].Messages
	var callIDs []string
	results := map[string]bool{}
	for _, m := range second {
		for _, c := range m.ToolCalls {
			callIDs = append(callIDs, c.ID)
		}
		if m.Role == "tool" {
			results[m.ToolCallID] = true
		}
	}
	if len(callIDs) != 2 || callIDs[0] == "" || callIDs[0] == callIDs[1] {
		t.Fatalf("应补出两个不同的 id：%v", callIDs)
	}
	for _, id := range callIDs {
		if !results[id] {
			t.Fatalf("调用 %s 没有对应的结果：%v", id, results)
		}
	}
}

func TestRunner_Fork每个消费者一个租约标识(t *testing.T) {
	h := newHarness(domain.StatePlanning, domain.TriggerUser, 2, 100)
	a, b := h.runner.Fork(), h.runner.Fork()
	if a.RunnerID() == h.runner.RunnerID() || a.RunnerID() == b.RunnerID() {
		t.Fatalf("租约标识应各不相同：%s %s %s", h.runner.RunnerID(), a.RunnerID(), b.RunnerID())
	}
}

type countingModels struct {
	calls int
	list  map[string][]ModelBrief
}

func (c *countingModels) UsableModels(_ context.Context, output string, _ int64) ([]ModelBrief, error) {
	c.calls++
	return c.list[output], nil
}

type noVideoInfo struct{}

func (noVideoInfo) VideoModel(context.Context, string) (VideoModelInfo, error) {
	return VideoModelInfo{IsVideo: true}, nil
}

// P-59 / P-60：智能匹配时列出可选模型，偏好写进「创作偏好」；目录一轮只查一次。
func TestRunTurn_系统提示词带可选模型与创作偏好且一轮只查一次目录(t *testing.T) {
	h := newHarness(domain.StatePlanning, domain.TriggerUser, 2, 100)
	h.store.sessions[hSession].Prefs = json.RawMessage(`{"reasoning":{"model":"m1"},"video":{"ratio":"9:16"}}`)
	models := &countingModels{list: map[string][]ModelBrief{
		"video": {{Code: "sd2", Name: "Seedance", Operations: []string{"text2video", "img2video"}}},
		"image": {{Code: "img1", Operations: []string{"text2img"}}},
	}}
	h.runner = NewRunner(RunnerDeps{
		Store: h.store, Chat: h.chat, Quoter: fakeQuoter{credits: 2}, Ledger: h.ledger,
		Members: h.members, Events: h.events, Tools: h.tools, VideoModels: noVideoInfo{}, Models: models,
		NextID: func() int64 { h.nextID++; return h.nextID },
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}, RunnerConfig{MaxCalls: 5, ContextChars: 60000})
	h.chat.replies = []fakeReply{
		{out: ChatOutput{ToolCalls: []ToolCall{toolCall("c1", "read_canvas", `{}`)}}},
		{out: ChatOutput{Text: "好"}},
	}
	if _, err := h.run(); err != nil {
		t.Fatal(err)
	}
	calls := h.chat.calls()
	if len(calls) != 2 {
		t.Fatalf("应调用 2 次模型，实际 %d", len(calls))
	}
	for i, in := range calls {
		for _, want := range []string{"可以选的视频模型", "- sd2（Seedance）：text2video、img2video",
			"## 创作偏好\n- 画幅：9:16", "img1：text2img"} {
			if !strings.Contains(in.System, want) {
				t.Errorf("第 %d 次请求的系统提示词缺 %q", i+1, want)
			}
		}
	}
	if models.calls != 2 {
		t.Fatalf("目录一轮只查一次（视频 + 图片各一次），实际 %d 次", models.calls)
	}
}

// P-14：模型报错时本轮已提交的任务还在跑，原先会话直接进 failed，任务结束后没人接手。
func TestRunTurn_模型报错但还有任务在跑时去等任务(t *testing.T) {
	h := newHarness(domain.StateExecuting, domain.TriggerUser, 2, 100)
	g := h.tools[domain.ToolGenerate].(*fakeTool)
	g.result = ToolResult{Content: json.RawMessage(`{"generationId":"9"}`), GenerationID: 9}
	h.chat.replies = []fakeReply{
		{out: ChatOutput{ToolCalls: []ToolCall{toolCall("c1", "generate", `{"wait":false}`)}}},
		{err: errors.New("上游 502")},
	}
	if _, err := h.run(); err == nil {
		t.Fatal("模型报错应带错误返回（只用于日志）")
	}
	if s := h.session(); s.State != domain.StateWaitingTask {
		t.Fatalf("还有任务在跑：应去等任务而不是 failed，实际 %s", s.State)
	}

	// 没有任务在等时照旧进 failed。
	h2 := newHarness(domain.StateExecuting, domain.TriggerUser, 2, 100)
	h2.chat.replies = []fakeReply{{err: errors.New("上游 502")}}
	_, _ = h2.run()
	if s := h2.session(); s.State != domain.StateFailed {
		t.Fatalf("没有在等的任务时进 failed，实际 %s", s.State)
	}
}

// P-22①：发消息带技能是追加，不覆盖已加载的（原先覆盖：营销技能会被画布面板选的通用技能整组换掉）。
func TestMergeSkillCodes_追加去重且有上限(t *testing.T) {
	got, err := mergeSkillCodes(json.RawMessage(`["mk_ad","story"]`), json.RawMessage(`["video_single"]`))
	if err != nil || string(got) != `["mk_ad","story","video_single"]` {
		t.Fatalf("应追加：%s %v", got, err)
	}
	got, _ = mergeSkillCodes(json.RawMessage(`["story"]`), json.RawMessage(`["story"]`))
	if string(got) != `["story"]` {
		t.Fatalf("重复的不再追加：%s", got)
	}
	got, _ = mergeSkillCodes(nil, json.RawMessage(`["a"]`))
	if string(got) != `["a"]` {
		t.Fatalf("会话还没有技能时就是这一个：%s", got)
	}
	full := json.RawMessage(`["a","b","c","d","e","f","g","h"]`)
	if _, err := mergeSkillCodes(full, json.RawMessage(`["i"]`)); err == nil {
		t.Fatal("超过上限要拒")
	}
}

// rcTool 记下工具收到的 RunContext。
type rcTool struct {
	fakeTool
	got RunContext
}

func (t *rcTool) Run(_ context.Context, rc RunContext, _ json.RawMessage) (ToolResult, error) {
	t.got = rc
	return ToolResult{Content: json.RawMessage(`{"ok":true}`)}, nil
}

// 会话 107419662995685376：规格三问按固定题目 id 认回答；确认过的写进「创作偏好」并随 RunContext 交给工具，
// 没问过的（这里是清晰度）仍要求先确认。
func TestRunTurn_问答卡里确认过的规格进提示词与工具上下文(t *testing.T) {
	h := newHarness(domain.StatePlanning, domain.TriggerUser, 2, 100)
	h.store.sessions[hSession].Prefs = json.RawMessage(`{"reasoning":{"model":"m1"},"video":{"model":"sd2"}}`)
	payload, _ := json.Marshal(domain.QuestionsPayload{Title: "确认", Questions: []domain.Question{
		{ID: domain.SpecRatio, Type: domain.QuestionSingle, Options: []domain.Option{{ID: "1", Label: "9:16 竖屏（推荐）"}}},
		{ID: domain.SpecDuration, Type: domain.QuestionText},
	}})
	sub, _ := json.Marshal(domain.QuestionsSubmission{Answers: []domain.Answer{
		{QuestionID: domain.SpecRatio, Selected: []string{"1"}}, {QuestionID: domain.SpecDuration, Text: "15 秒"}}})
	h.store.answered = []repo.Card{{Kind: domain.CardQuestions, Payload: payload, Submission: sub}}
	tool := &rcTool{fakeTool: fakeTool{name: "read_canvas"}}
	h.tools["read_canvas"] = tool
	h.chat.replies = []fakeReply{
		{out: ChatOutput{ToolCalls: []ToolCall{toolCall("c1", "read_canvas", `{}`)}}},
		{out: ChatOutput{Text: "好"}},
	}
	if _, err := h.run(); err != nil {
		t.Fatal(err)
	}
	sys := h.chat.calls()[0].System
	for _, want := range []string{"- 画幅：9:16", "- 视频时长：15 秒", "清晰度：用户没有指定", "题目 id 分别用 resolution"} {
		if !strings.Contains(sys, want) {
			t.Errorf("系统提示词缺 %q：\n%s", want, sys)
		}
	}
	if s := tool.got.Spec; s.Ratio != "9:16" || s.Duration != 15 || len(s.Missing) != 1 || s.Missing[0] != domain.SpecResolution {
		t.Fatalf("工具收到的规格：%+v", s)
	}
}
