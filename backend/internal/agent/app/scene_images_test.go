package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/li904338203/tabtv/backend/internal/agent/domain"
	"github.com/li904338203/tabtv/backend/internal/agent/repo"
)

// fakeImages 是 SceneImages 的假实现：mediaId → data: 地址；记录取了哪些。
type fakeImages struct {
	limit    int
	limitErr error
	fail     map[int64]bool
	fetched  []int64
	limitQs  int
}

func (f *fakeImages) ReasoningImageLimit(context.Context, string) (int, error) {
	f.limitQs++
	return f.limit, f.limitErr
}

func (f *fakeImages) InlineImage(_ context.Context, ws, id int64) (string, error) {
	f.fetched = append(f.fetched, id)
	if f.fail[id] {
		return "", errors.New("取不到")
	}
	return fmt.Sprintf("data:image/jpeg;base64,IMG%d", id), nil
}

func imgOf(id int) string { return fmt.Sprintf("data:image/jpeg;base64,IMG%d", id) }

// marketingHarness 建一个营销会话：商品图 n 张（mediaId 9001 起）。
func marketingHarness(t *testing.T, n int, imgs *fakeImages) (*harness, *fakeSceneCtx) {
	t.Helper()
	h := newHarness(domain.StatePlanning, domain.TriggerUser, 2, 100)
	products := make([]string, n)
	for i := range products {
		products[i] = strconv.Itoa(9001 + i)
	}
	si, _ := json.Marshal(SceneInput{Products: products, Product: SceneProduct{Name: "山野冷萃"},
		Characters: []SceneRef{}, Pins: []ScenePinEntry{}})
	s := h.store.sessions[hSession]
	s.Scene, s.SceneInput = domain.SceneMarketing, si
	sc := &fakeSceneCtx{}
	h.runner.d.Scene = sc
	h.runner.d.Images = imgs
	return h, sc
}

func countImages(msgs []ChatMessage) int {
	n := 0
	for _, m := range msgs {
		n += len(m.Images)
	}
	return n
}

// 上传 20 张、推理模型上限 4：首条请求恰好附 #1~#4，营销输入写「能看到 4 张」，其余只以序号出现。
func TestToChatMessages_营销首条附图不超上限(t *testing.T) {
	imgs := &fakeImages{limit: 4}
	h, sc := marketingHarness(t, 20, imgs)
	h.chat.replies = []fakeReply{
		{out: ChatOutput{ToolCalls: []ToolCall{toolCall("c1", "read_canvas", `{}`)}}},
		{out: ChatOutput{Text: "我看到了商品。"}},
	}
	if _, err := h.run(); err != nil {
		t.Fatal(err)
	}
	calls := h.chat.calls()
	if len(calls) != 2 {
		t.Fatalf("应调用模型 2 次，实际 %d", len(calls))
	}
	for i, in := range calls {
		first := in.Messages[0]
		want := []string{imgOf(9001), imgOf(9002), imgOf(9003), imgOf(9004)}
		if first.Role != "user" || !reflect.DeepEqual(first.Images, want) {
			t.Fatalf("第 %d 次请求首条消息附图不对：%v", i+1, first.Images)
		}
		if countImages(in.Messages) != 4 {
			t.Errorf("第 %d 次请求总图数应为 4，实际 %d", i+1, countImages(in.Messages))
		}
		for _, m := range in.Messages {
			for _, img := range m.Images {
				if !strings.HasPrefix(img, "data:image/") {
					t.Errorf("只发内联图，不发外链（MKT-AC-05）：%s", img)
				}
			}
		}
	}
	if sc.visible != 4 {
		t.Errorf("营销输入的「能看到」张数应为 4，实际 %d", sc.visible)
	}
	// 一轮里只取一次（两次模型调用不重复取图）。
	if !reflect.DeepEqual(imgs.fetched, []int64{9001, 9002, 9003, 9004}) || imgs.limitQs != 1 {
		t.Errorf("取图次数不对：fetched=%v limitQs=%d", imgs.fetched, imgs.limitQs)
	}
	// 库里的用户消息不带图（图只在请求里）。
	if n := len(h.store.messagesByRole(repo.RoleUser)); n != 1 {
		t.Errorf("不应多出用户消息：%d", n)
	}
}

func TestToChatMessages_通用会话不附图(t *testing.T) {
	h := newHarness(domain.StatePlanning, domain.TriggerUser, 2, 100)
	imgs := &fakeImages{limit: 4}
	h.runner.d.Images = imgs
	if _, err := h.run(); err != nil {
		t.Fatal(err)
	}
	in := h.chat.calls()[0]
	if countImages(in.Messages) != 0 {
		t.Errorf("通用会话不得附图")
	}
	if imgs.limitQs != 0 || len(imgs.fetched) != 0 {
		t.Errorf("通用会话不应调用看图端口：%+v", imgs)
	}
	// 与不接端口时的请求逐字相同。
	h2 := newHarness(domain.StatePlanning, domain.TriggerUser, 2, 100)
	if _, err := h2.run(); err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(in)
	b, _ := json.Marshal(h2.chat.calls()[0])
	if string(a) != string(b) {
		t.Errorf("通用会话请求体应逐字不变：\n%s\n%s", a, b)
	}
}

func TestToChatMessages_推理模型不收图退化为文字序号(t *testing.T) {
	for name, imgs := range map[string]*fakeImages{
		"上限为0":  {limit: 0},
		"查询出错":  {limit: 4, limitErr: errors.New("库挂了")},
		"首张取不到": {limit: 4, fail: map[int64]bool{9001: true}},
	} {
		h, sc := marketingHarness(t, 5, imgs)
		if _, err := h.run(); err != nil {
			t.Fatalf("%s：%v", name, err)
		}
		if countImages(h.chat.calls()[0].Messages) != 0 || sc.visible != 0 {
			t.Errorf("%s：应不附图且写「未附图」，visible=%d", name, sc.visible)
		}
		if tr := h.turn(); tr.Status != domain.TurnSuccess {
			t.Errorf("%s：轮次应正常完成：%+v", name, tr)
		}
	}
	// 中间一张取不到：停在它之前，保证「#1~#k」连续。
	imgs := &fakeImages{limit: 4, fail: map[int64]bool{9003: true}}
	h, sc := marketingHarness(t, 5, imgs)
	if _, err := h.run(); err != nil {
		t.Fatal(err)
	}
	if got := h.chat.calls()[0].Messages[0].Images; !reflect.DeepEqual(got, []string{imgOf(9001), imgOf(9002)}) ||
		sc.visible != 2 {
		t.Errorf("应只附 #1~#2：%v visible=%d", got, sc.visible)
	}
}

// view_media{index:7} 后的下一次请求恰好多带 #7 一张，再下一次不带；agent_message 里没有这条临时消息。
func TestRunner_viewMedia临时消息不落库(t *testing.T) {
	imgs := &fakeImages{limit: 4}
	h, _ := marketingHarness(t, 20, imgs)
	vm := h.tools[domain.ToolViewMedia].(*fakeTool)
	vm.result = ToolResult{Content: json.RawMessage(`{"ok":true,"label":"商品图 #7"}`),
		AttachImages: []string{imgOf(9007)}}
	h.chat.replies = []fakeReply{
		{out: ChatOutput{ToolCalls: []ToolCall{
			toolCall("c1", "view_media", `{"slot":"product","index":7}`),
			toolCall("c2", "read_canvas", `{}`)}}},
		{out: ChatOutput{ToolCalls: []ToolCall{toolCall("c3", "read_canvas", `{}`)}}},
		{out: ChatOutput{Text: "看完了。"}},
	}
	if _, err := h.run(); err != nil {
		t.Fatal(err)
	}
	calls := h.chat.calls()
	if len(calls) != 3 {
		t.Fatalf("应调用 3 次，实际 %d", len(calls))
	}
	second := calls[1].Messages
	if n := countImages(second); n != 5 {
		t.Fatalf("第二次请求应恰好多带 1 张（4+1），实际 %d", n)
	}
	// 临时消息紧跟在同一批两条工具结果之后（不夹在中间）。
	idx := -1
	for i, m := range second {
		if m.Role == "user" && reflect.DeepEqual(m.Images, []string{imgOf(9007)}) {
			idx = i
		}
	}
	if idx < 2 || second[idx-1].Role != "tool" || second[idx-2].Role != "tool" ||
		second[idx-2].ToolCallID != "c1" || second[idx-1].ToolCallID != "c2" {
		t.Fatalf("临时附图消息位置不对：%d %+v", idx, second)
	}
	if !strings.HasPrefix(second[idx].Content, SystemEventPrefix) || !strings.Contains(second[idx].Content, "商品图 #7") {
		t.Errorf("临时消息应标明非用户发言与序号：%q", second[idx].Content)
	}
	if n := countImages(calls[2].Messages); n != 4 {
		t.Errorf("再下一次请求不应再带 #7：%d", n)
	}
	for _, m := range h.store.msgs {
		if m.Role == repo.RoleUser && m.ID != 1 {
			t.Errorf("临时附图消息不得落库：%+v", m)
		}
		if strings.Contains(m.Content, "IMG9007") {
			t.Errorf("库里不应出现图片内容：%+v", m)
		}
	}
	if st := h.store.stepsByStatus(repo.StepSuccess); len(st) != 3 {
		t.Errorf("三个工具步骤都应成功：%+v", st)
	}
}

func TestRunner_viewMedia模型不收图按工具错误回给模型(t *testing.T) {
	h, _ := marketingHarness(t, 3, &fakeImages{limit: 0})
	vm := h.tools[domain.ToolViewMedia].(*fakeTool)
	vm.result = ToolResult{Content: json.RawMessage(`{"ok":true}`), AttachImages: []string{imgOf(9002)}}
	h.chat.replies = []fakeReply{
		{out: ChatOutput{ToolCalls: []ToolCall{toolCall("c1", "view_media", `{"slot":"product","index":2}`)}}},
		{out: ChatOutput{Text: "按卖点继续。"}},
	}
	if _, err := h.run(); err != nil {
		t.Fatal(err)
	}
	if countImages(h.chat.calls()[1].Messages) != 0 {
		t.Error("不收图的模型不得附图")
	}
	tools := h.store.messagesByRole(repo.RoleTool)
	if len(tools) != 1 || !strings.Contains(tools[0].Content, "不支持看图") {
		t.Errorf("应回一条工具错误：%+v", tools)
	}
	if tr := h.turn(); tr.Status != domain.TurnSuccess {
		t.Errorf("本轮应继续完成：%+v", tr)
	}
}

func TestRunner_viewMedia一次看太多拒收超出部分(t *testing.T) {
	h, _ := marketingHarness(t, 10, &fakeImages{limit: 1})
	vm := h.tools[domain.ToolViewMedia].(*fakeTool)
	vm.result = ToolResult{Content: json.RawMessage(`{"ok":true}`), AttachImages: []string{imgOf(9005)}}
	h.chat.replies = []fakeReply{
		{out: ChatOutput{ToolCalls: []ToolCall{
			toolCall("c1", "view_media", `{"slot":"product","index":5}`),
			toolCall("c2", "view_media", `{"slot":"product","index":6}`)}}},
		{out: ChatOutput{Text: "好"}},
	}
	if _, err := h.run(); err != nil {
		t.Fatal(err)
	}
	// 首条 1 张 + 临时 1 张；第二次 view 被拒。
	if n := countImages(h.chat.calls()[1].Messages); n != 2 {
		t.Errorf("第二次请求应带 2 张，实际 %d", n)
	}
	if f := h.store.stepsByStatus(repo.StepFailed); len(f) != 1 {
		t.Errorf("超出的那次应失败：%+v", f)
	}
}

func TestFirstUserKept_首条被截断时不附图(t *testing.T) {
	msgs := []repo.Message{msg(1, repo.RoleUser, strings.Repeat("长", 50)), msg(2, repo.RoleAssistant, "好"),
		msg(3, repo.RoleUser, "继续")}
	for i := range msgs {
		msgs[i].ID = int64(i + 1)
	}
	if id, ok := firstUserKept(msgs, false, 1000); !ok || id != 1 {
		t.Errorf("未截断时应命中首条：%d %v", id, ok)
	}
	if _, ok := firstUserKept(msgs, false, 10); ok {
		t.Error("首条被截掉时不应附图")
	}
	if _, ok := firstUserKept(msgs, true, 1000); ok {
		t.Error("还有更早的消息没取出来时首条不在手上")
	}
	// BuildContext 只把图挂在那一条上。
	out := BuildContext(ContextInput{Messages: msgs, Budget: 1000, FirstUserMessageID: 1,
		FirstUserImages: []string{imgOf(1)}})
	if len(out.Messages[0].Images) != 1 || len(out.Messages[2].Images) != 0 {
		t.Errorf("图只应挂在首条用户消息上：%+v", out.Messages)
	}
}

func TestInjectViewImages_同批工具结果之后合并插入(t *testing.T) {
	msgs := []ChatMessage{{Role: "user", Content: "a"}, {Role: "assistant"},
		{Role: "tool", ToolCallID: "x"}, {Role: "tool", ToolCallID: "y"}, {Role: "tool", ToolCallID: "z"}}
	got := injectViewImages(msgs, []pendingView{
		{afterCallID: "x", images: []string{"i1"}, labels: []string{"商品图 #1"}},
		{afterCallID: "z", images: []string{"i2"}, labels: []string{"商品图 #2"}},
	})
	if len(got) != 6 || got[5].Role != "user" || !reflect.DeepEqual(got[5].Images, []string{"i1", "i2"}) ||
		!strings.Contains(got[5].Content, "商品图 #1、商品图 #2") {
		t.Fatalf("应在三条工具结果之后合并成一条：%+v", got)
	}
	if !reflect.DeepEqual(injectViewImages(msgs, nil), msgs) {
		t.Error("没有附图时原样返回")
	}
}
