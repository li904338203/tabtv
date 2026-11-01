package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

const testFixture = `{"objects":[{"op":"add","ref":"老刀","kind":"character"}],"camera":{"angle":"low"}}`

func newDirectorServer(t *testing.T) (*server, *httptest.Server) {
	t.Helper()
	s := &server{addr: "http://fake", directorReply: []byte(testFixture)}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", s.chat)
	mux.HandleFunc("/v1/messages", s.anthropicMessages)
	mux.HandleFunc("/_probe/director-build", s.directorProbe.handle)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return s, srv
}

func chatText(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	b, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(b, &out); err != nil || len(out.Choices) == 0 {
		t.Fatalf("回包不是 chat.completion：%s", b)
	}
	return out.Choices[0].Message.Content
}

// buildChat 是一次搭景请求：系统提示词带搭景标记（`withStoryboard` 时再带上「分镜」二字）。
func buildChat(user string, images []string, withStoryboard bool) map[string]any {
	system := "你是一名导演。\n\n" + directorBuildMarker + "\n只输出 JSON。"
	if withStoryboard {
		system = "你是一名导演，参考分镜的写法摆人物。\n\n" + directorBuildMarker + "\n只输出 JSON。"
	}
	parts := []any{map[string]any{"type": "text", "text": user}}
	for _, u := range images {
		parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": u}})
	}
	return map[string]any{"model": "m-scene", "messages": []any{
		map[string]any{"role": "system", "content": system},
		map[string]any{"role": "user", "content": parts},
	}}
}

func TestDirectorBuild_标记优先于分镜启发式(t *testing.T) {
	_, srv := newDirectorServer(t)
	// 系统提示词里同时有「分镜」二字与搭景标记：回的必须是搭景方案，不是分镜 JSON
	got := chatText(t, post(t, srv.URL+"/v1/chat/completions", nil, buildChat("三个人在街头对峙", nil, true)))
	if got != testFixture {
		t.Fatalf("应回搭景方案，实际：%.120s", got)
	}
	// 没有搭景标记、只有「分镜」：照旧回分镜 JSON（脚本线不受影响）
	plain := map[string]any{"model": "m", "messages": []any{
		map[string]any{"role": "system", "content": "你是分镜导演"},
		map[string]any{"role": "user", "content": "一段剧情"},
	}}
	if got := chatText(t, post(t, srv.URL+"/v1/chat/completions", nil, plain)); got != storyboardJSON {
		t.Fatalf("没有搭景标记时应回分镜 JSON，实际：%.120s", got)
	}
}

func TestDirectorBuild_解析失败魔法词(t *testing.T) {
	_, srv := newDirectorServer(t)
	got := chatText(t, post(t, srv.URL+"/v1/chat/completions", nil, buildChat("对峙 "+magicBuildParseFail, nil, false)))
	if json.Valid([]byte(got)) || got == "" {
		t.Fatalf("解析失败的魔法词应回一段不是 JSON 的文字，实际：%q", got)
	}
}

func TestDirectorBuild_慢回包(t *testing.T) {
	old := directorSlowDelay
	directorSlowDelay = 150 * time.Millisecond
	t.Cleanup(func() { directorSlowDelay = old })
	_, srv := newDirectorServer(t)
	start := time.Now()
	got := chatText(t, post(t, srv.URL+"/v1/chat/completions", nil, buildChat("对峙 "+magicBuildSlow, nil, false)))
	if time.Since(start) < 150*time.Millisecond {
		t.Fatalf("慢回包应先等再回，只用了 %v", time.Since(start))
	}
	if got != testFixture {
		t.Fatalf("慢回包等完照常回方案，实际：%.120s", got)
	}
}

func TestDirectorBuild_没有回包时报错不兜底(t *testing.T) {
	s, srv := newDirectorServer(t)
	s.directorReply = nil
	resp := post(t, srv.URL+"/v1/chat/completions", nil, buildChat("对峙", nil, false))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("没有回包时应 500，实际 %d", resp.StatusCode)
	}
	if b, _ := io.ReadAll(resp.Body); !strings.Contains(string(b), "fake-reply.json") {
		t.Fatalf("错误要说清缺哪个文件：%s", b)
	}
}

func TestDirectorBuild_Anthropic协议同样回方案(t *testing.T) {
	_, srv := newDirectorServer(t)
	body := map[string]any{"model": "m-scene-claude", "max_tokens": 1024,
		"system": "你是一名导演。\n\n" + directorBuildMarker,
		"messages": []any{map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "text", "text": "三个人在街头对峙"},
			map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": "iVBORw0KGgo="}},
		}}}}
	resp := post(t, srv.URL+"/v1/messages", map[string]string{"anthropic-version": "2023-06-01"}, body)
	defer resp.Body.Close()
	var out struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	b, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(b, &out); err != nil || len(out.Content) != 1 || out.Content[0].Text != testFixture {
		t.Fatalf("Anthropic 搭景应回方案文字：%s", b)
	}
}

func TestDirectorProbe_记录内联图与外链数(t *testing.T) {
	s, srv := newDirectorServer(t)
	images := []string{"data:image/png;base64,iVBORw0KGgo=", "data:image/jpeg;base64,/9j/4AAQ", "https://cdn.example/ref.png"}
	_ = chatText(t, post(t, srv.URL+"/v1/chat/completions", nil, buildChat("对峙", images, false)))
	recs := s.directorProbe.list("m-scene", 0)
	if len(recs) != 1 {
		t.Fatalf("应记一条，实际 %d", len(recs))
	}
	r := recs[0]
	if r.Images != 3 || r.DataImages != 2 || r.HTTPImages != 1 || !r.Marker || r.Protocol != "openai_chat" {
		t.Fatalf("探针记录不对：%+v", r)
	}
	if len(r.URLs) != 1 || r.URLs[0] != "https://cdn.example/ref.png" {
		t.Fatalf("请求体里的外链应记下来：%v", r.URLs)
	}
	resp, err := http.Get(srv.URL + "/_probe/director-build?model=m-scene&limit=5")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var listed struct {
		Items []directorBuildRecord `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&listed); err != nil || len(listed.Items) != 1 {
		t.Fatalf("探针端点应回同一条：%v %+v", err, listed)
	}
	// 只保留最近 50 条
	for i := 0; i < directorProbeKeep+5; i++ {
		s.directorProbe.record(directorBuildInput{Model: "m-other"})
	}
	if n := len(s.directorProbe.list("", 0)); n != directorProbeKeep {
		t.Fatalf("应只保留最近 %d 条，实际 %d", directorProbeKeep, n)
	}
}

func TestAgentScript_导演台分支依次调两个工具(t *testing.T) {
	tools := append(append([]string{}, executingTools...), "director_create", "director_build")
	// 规划态：直接提交一条视频计划项（导演台工具只在执行态下发）
	in := agentTurnInput{System: sysPrompt(agentPlanningMarker), Tools: append([]string{}, planningTools...),
		Messages: []agentMsg{user("在画布上搭一个对峙的场景 " + magicDirector)}}
	r := agentScript(in)
	if r.ToolName != "submit_plan" {
		t.Fatalf("规划态应直接提交规划，实际 %+v", r)
	}
	if it := argsOf(t, r)["items"].([]any)[0].(map[string]any); it["modelCode"] != "vm-1" || it["type"] != "video" {
		t.Fatalf("计划项应取当前视频模型：%v", it)
	}
	// 执行态：director_create → director_build（用 create 回的 nodeId）→ 提醒截图
	in = agentTurnInput{System: sysPrompt(agentExecutingMarker), Tools: tools, Messages: []agentMsg{
		user("在画布上搭一个对峙的场景 " + magicDirector), user("确认规划，授权最多花费 ✦5")}}
	r = agentScript(in)
	if r.ToolName != "director_create" {
		t.Fatalf("批准后先建导演台，实际 %+v", r)
	}
	in.Messages = append(in.Messages, asst("d1", "director_create", r.ToolArgs),
		toolRes("d1", `{"nodeId":"n_director_1","sceneId":"123","next":"用 director_build 搭景"}`))
	r = agentScript(in)
	if a := argsOf(t, r); r.ToolName != "director_build" || a["nodeId"] != "n_director_1" || a["prompt"] == "" {
		t.Fatalf("建好之后用它的 nodeId 搭景，实际 %+v", r)
	}
	in.Messages = append(in.Messages, asst("d2", "director_build", r.ToolArgs),
		toolRes("d2", `{"sceneId":"123","version":2,"added":3}`))
	r = agentScript(in)
	if r.ToolName != "" || !strings.Contains(r.Text, "打开导演台截图") {
		t.Fatalf("搭完应提醒用户打开导演台截图，实际 %+v", r)
	}
	// 没有魔法词时不走这条分支
	plain := agentTurnInput{System: sysPrompt(agentExecutingMarker), Tools: tools,
		Messages: []agentMsg{user("做一条短片"), user("确认规划，授权最多花费 ✦5")}}
	if r := agentScript(plain); r.ToolName != "generate" {
		t.Fatalf("没有 #导演台 时照旧生成，实际 %+v", r)
	}
}

// 回包与求解用例是同一份方案：E2E 第 265 步按「3 个角色、1 个机位」断言，两份分叉的话断言对不上。
func TestDirectorReply_与对峙用例同一份方案(t *testing.T) {
	root := filepath.Join("..", "..", "api", "fixtures", "director", "build")
	var caseFile struct {
		Draft json.RawMessage `json:"draft"`
	}
	replyRaw, err := os.ReadFile(filepath.Join(root, "fake-reply.json"))
	if err != nil {
		t.Fatal(err)
	}
	caseRaw, err := os.ReadFile(filepath.Join(root, "case-default-scene-standoff.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(caseRaw, &caseFile); err != nil {
		t.Fatal(err)
	}
	var a, b any
	_ = json.Unmarshal(replyRaw, &a)
	_ = json.Unmarshal(caseFile.Draft, &b)
	if !reflect.DeepEqual(a, b) {
		t.Fatal("fake-reply.json 应与 case-default-scene-standoff.json 的 draft 相同")
	}
	// 从模块目录往上能找到它（`go run .` 时的默认路径）
	if p := findDirectorReply(); p == "" {
		t.Fatal("从模块目录往上应找得到 " + directorReplyRel)
	}
}

// SCT-41：#智能剪辑 分支 —— 规划态提交一条计划项；执行态 smartcut_create（连上 #素材=）→ smartcut_plan → smartcut_get →
// render（给 timelineId，不等）→ 收尾；哪一步没拿到想要的字段就原样转述、不重试。
func TestAgentScript_智能剪辑分支依次调工具(t *testing.T) {
	tools := append(append([]string{}, executingTools...), "smartcut_create", "smartcut_plan", "smartcut_get", "render")
	in := agentTurnInput{System: sysPrompt(agentPlanningMarker), Tools: append([]string{}, planningTools...),
		Messages: []agentMsg{user("把三段视频剪成一条 " + magicSmartcut + " #素材=n_v1,n_v2,n_v3")}}
	if r := agentScript(in); r.ToolName != "submit_plan" {
		t.Fatalf("规划态应直接提交规划，实际 %+v", r)
	}
	in = agentTurnInput{System: sysPrompt(agentExecutingMarker), Tools: tools, Messages: []agentMsg{
		user("把三段视频剪成一条 " + magicSmartcut + " #素材=n_v1,n_v2,n_v3"), user("确认规划，授权最多花费 ✦5")}}
	r := agentScript(in)
	if a := argsOf(t, r); r.ToolName != "smartcut_create" || fmt.Sprint(a["inputNodeIds"]) != "[n_v1 n_v2 n_v3]" {
		t.Fatalf("批准后先建智能剪辑并连上素材，实际 %+v", r)
	}
	in.Messages = append(in.Messages, asst("s1", "smartcut_create", r.ToolArgs),
		toolRes("s1", `{"nodeId":"a_sc1","timelineId":"901","appended":3}`))
	r = agentScript(in)
	if a := argsOf(t, r); r.ToolName != "smartcut_plan" || a["nodeId"] != "a_sc1" || a["mode"] != "mix" {
		t.Fatalf("建好之后用它的 nodeId 剪，实际 %+v", r)
	}
	in.Messages = append(in.Messages, asst("s2", "smartcut_plan", r.ToolArgs),
		toolRes("s2", `{"nodeId":"a_sc1","timelineId":"901","callId":"77","appliedVariant":0}`))
	r = agentScript(in)
	if a := argsOf(t, r); r.ToolName != "smartcut_get" || a["nodeId"] != "a_sc1" {
		t.Fatalf("剪完看一下结果，实际 %+v", r)
	}
	in.Messages = append(in.Messages, asst("s3", "smartcut_get", r.ToolArgs),
		toolRes("s3", `{"nodeId":"a_sc1","timelineId":"901","clipCount":3}`))
	r = agentScript(in)
	if a := argsOf(t, r); r.ToolName != "render" || a["timelineId"] != "901" || a["wait"] != false {
		t.Fatalf("有片段就导出（给 timelineId、不等），实际 %+v", r)
	}
	in.Messages = append(in.Messages, asst("s4", "render", r.ToolArgs), toolRes("s4", `{"renderId":"5","status":"queued"}`))
	if r = agentScript(in); r.ToolName != "" || !strings.Contains(r.Text, "导出") {
		t.Fatalf("收尾，实际 %+v", r)
	}
	// AI 剪辑被限流：原样转述，不重试。
	bad := agentTurnInput{System: in.System, Tools: tools, Messages: append(append([]agentMsg{}, in.Messages[:4]...),
		asst("s2", "smartcut_plan", `{}`), toolRes("s2", `{"error":"RATE_LIMITED","retry_after_seconds":600}`))}
	if r := agentScript(bad); r.ToolName != "" || !strings.Contains(r.Text, "RATE_LIMITED") {
		t.Fatalf("没成功就转述，实际 %+v", r)
	}
}
