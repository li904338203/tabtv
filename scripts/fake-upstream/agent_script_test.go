package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

var (
	planningTools  = []string{"read_canvas", "list_assets", "quote", "ask_user", "read_doc", "write_doc", "submit_plan", "load_skill", "read_skill_ref"}
	executingTools = []string{"read_canvas", "list_assets", "quote", "ask_user", "read_doc", "write_doc", "canvas_apply", "generate", "cancel_task", "load_skill", "read_skill_ref"}
)

func sysPrompt(stateNote string) string {
	return agentSkeletonMarker + "，帮助用户……\n\n" + stateNote + "\n\n## 当前视频模型\n- 模型：vm-1\n- 单次最长时长：10 秒\n\n## 画布摘要\n（画布为空）"
}

func asst(id, name, args string) agentMsg {
	return agentMsg{Role: "assistant", ToolCalls: []agentCall{{ID: id, Name: name, Args: args}}}
}

func toolRes(id, content string) agentMsg {
	return agentMsg{Role: "tool", ToolCallID: id, Text: content}
}

func user(text string) agentMsg { return agentMsg{Role: "user", Text: text} }

func argsOf(t *testing.T, r agentReply) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(r.ToolArgs), &m); err != nil {
		t.Fatalf("工具参数不是 JSON：%q", r.ToolArgs)
	}
	return m
}

func TestAgentScript_无工具结果先加载技能(t *testing.T) {
	in := agentTurnInput{System: sysPrompt(agentPlanningMarker), Tools: planningTools,
		Messages: []agentMsg{user("做一条雨夜告别的短片 #tag:t1")}}
	r := agentScript(in)
	if r.ToolName != "load_skill" || argsOf(t, r)["code"] != "video_single" {
		t.Fatalf("应先加载 video_single，实际 %+v", r)
	}
	// 技能结果到达 → 选择卡。
	in.Messages = append(in.Messages, asst("c1", "load_skill", `{"code":"video_single"}`),
		toolRes("c1", `{"code":"video_single","loaded":true}`))
	r = agentScript(in)
	if r.ToolName != "ask_user" || argsOf(t, r)["kind"] != "choice" {
		t.Fatalf("技能加载后应发选择卡，实际 %+v", r)
	}
}

func TestAgentScript_卡片提交后提交规划(t *testing.T) {
	in := agentTurnInput{System: sysPrompt(agentPlanningMarker), Tools: planningTools, Messages: []agentMsg{
		user("做一条短片"), asst("c1", "load_skill", `{}`), toolRes("c1", `{"loaded":true}`),
		asst("c2", "ask_user", `{}`), toolRes("c2", `{"status":"awaiting_input"}`),
		user("我选择了：悲壮离别"),
	}}
	r := agentScript(in)
	if r.ToolName != "write_doc" || argsOf(t, r)["path"] != "/plan.md" {
		t.Fatalf("卡片提交后应写 /plan.md，实际 %+v", r)
	}
	in.Messages = append(in.Messages, asst("c3", "write_doc", r.ToolArgs),
		toolRes("c3", `{"path":"/plan.md","docId":"9","version":1,"nodeId":"doc-node"}`))
	r = agentScript(in)
	if r.ToolName != "submit_plan" {
		t.Fatalf("写完文档应提交规划，实际 %+v", r)
	}
	items := argsOf(t, r)["items"].([]any)
	it := items[0].(map[string]any)
	if it["modelCode"] != "vm-1" || it["type"] != "video" || it["durationSec"] != float64(5) {
		t.Fatalf("计划项应取系统提示词里的视频模型：%v", it)
	}
	if _, has := it["credits"]; has {
		t.Fatal("计划项不该带金额（金额由平台计算）")
	}
}

func TestAgentScript_批准后生成(t *testing.T) {
	in := agentTurnInput{System: sysPrompt(agentExecutingMarker), Tools: executingTools,
		Messages: []agentMsg{user("做一条短片"), user("确认规划，授权最多花费 ✦5")}}
	r := agentScript(in)
	a := argsOf(t, r)
	if r.ToolName != "generate" || a["modelCode"] != "vm-1" || a["wait"] != true || a["nodeType"] != "video" {
		t.Fatalf("批准后应生成并等待，实际 %+v", r)
	}
	// 规划态即使收到同样的话也不会调 generate（工具没下发就不调）。
	in.System, in.Tools = sysPrompt(agentPlanningMarker), planningTools
	if r := agentScript(in); r.ToolName == "generate" {
		t.Fatal("规划态不得调用 generate")
	}
}

func TestAgentScript_超额连续提交两个生成(t *testing.T) {
	in := agentTurnInput{System: sysPrompt(agentExecutingMarker), Tools: executingTools,
		Messages: []agentMsg{user("做一条短片 #超额"), user("确认规划，授权最多花费 ✦5")}}
	r := agentScript(in)
	if r.ToolName != "generate" || argsOf(t, r)["wait"] != false {
		t.Fatalf("#超额 时第一个生成不等待，实际 %+v", r)
	}
	in.Messages = append(in.Messages, asst("g1", "generate", r.ToolArgs),
		toolRes("g1", `{"generationId":"1","nodeId":"n1","waiting":false}`))
	r = agentScript(in)
	if r.ToolName != "generate" || argsOf(t, r)["wait"] != true {
		t.Fatalf("#超额 时应提交第二个生成，实际 %+v", r)
	}
	in.Messages = append(in.Messages, asst("g2", "generate", r.ToolArgs),
		toolRes("g2", `{"status":"over_budget","nodeId":"n2","needed":5}`))
	if r := agentScript(in); r.ToolName != "" {
		t.Fatalf("第二个之后不该再提交，实际 %+v", r)
	}
	// 追加额度后用超额那次的节点再提交。
	in.Messages = append(in.Messages, user("追加额度 ✦5"))
	r = agentScript(in)
	if a := argsOf(t, r); r.ToolName != "generate" || a["nodeId"] != "n2" || a["wait"] != true {
		t.Fatalf("追加额度后应在原节点上提交，实际 %+v", r)
	}
}

func TestAgentScript_任务完成后画框并总结(t *testing.T) {
	in := agentTurnInput{System: sysPrompt(agentExecutingMarker), Tools: executingTools, Messages: []agentMsg{
		asst("w1", "write_doc", `{}`), toolRes("w1", `{"path":"/plan.md","nodeId":"doc-node"}`),
		user("确认规划，授权最多花费 ✦5"),
		asst("g1", "generate", `{}`), toolRes("g1", `{"generationId":"77","nodeId":"video-node","waiting":true}`),
		user(agentSystemEventPrefix + "\n所等的 1 个任务已全部结束：成功 1、失败 0、取消 0。\n- generate（任务 77）：成功，产出 1 个"),
	}}
	r := agentScript(in)
	if r.ToolName != "canvas_apply" {
		t.Fatalf("任务完成后应画框，实际 %+v", r)
	}
	op := argsOf(t, r)["ops"].([]any)[0].(map[string]any)
	members := op["frame"].(map[string]any)["members"].([]any)
	if op["kind"] != "add_frame" || len(members) != 2 || members[0] != "video-node" || members[1] != "doc-node" {
		t.Fatalf("画框成员应为视频节点 + 文档节点：%v", op)
	}
	in.Messages = append(in.Messages, asst("f1", "canvas_apply", r.ToolArgs), toolRes("f1", `{"applied":1}`))
	r = agentScript(in)
	if r.ToolName != "" || !strings.Contains(r.Text, "已按计划生成") || !strings.Contains(r.Text, "接下来你可以：\n- ") {
		t.Fatalf("画框后应纯文本总结并给建议，实际 %+v", r)
	}
	// 全部失败：不画框，直接说明。
	in.Messages = append(in.Messages[:5], user(agentSystemEventPrefix+"\n所等的 1 个任务已全部结束：成功 0、失败 1、取消 0。"))
	if r := agentScript(in); r.ToolName != "" || !strings.Contains(r.Text, "退款") {
		t.Fatalf("全部失败时应纯文本说明，实际 %+v", r)
	}
}

func TestAgentScript_对话失败回500(t *testing.T) {
	in := agentTurnInput{System: sysPrompt(agentPlanningMarker), Tools: planningTools,
		Messages: []agentMsg{user("做一条短片 #对话失败")}}
	if r := agentScript(in); !r.Fail {
		t.Fatalf("#对话失败 应回 500，实际 %+v", r)
	}
	// 只看最后一条：之后的工具结果不再失败（否则一个会话永远走不动）。
	in.Messages = append(in.Messages, asst("c1", "load_skill", `{}`), toolRes("c1", `{}`))
	if r := agentScript(in); r.Fail {
		t.Fatal("最后一条不是用户消息时不该失败")
	}
}

func newAgentTestServer(t *testing.T) (*httptest.Server, *server) {
	t.Helper()
	s := &server{addr: "http://fake"}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", s.chat)
	mux.HandleFunc("/_probe/agent-tools", s.agentProbe.handle)
	mux.HandleFunc("/v1/gated/submit", s.gatedSubmit)
	mux.HandleFunc("/v1/gated/query/", s.gatedQuery)
	mux.HandleFunc("/_probe/gated/release", s.gatedRelease)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, s
}

func openAITools(names []string) []any {
	out := make([]any, 0, len(names))
	for _, n := range names {
		out = append(out, map[string]any{"type": "function", "function": map[string]any{"name": n}})
	}
	return out
}

func TestAgentScript_流式分片拼出完整参数(t *testing.T) {
	srv, _ := newAgentTestServer(t)
	resp := post(t, srv.URL+"/v1/chat/completions", nil, map[string]any{
		"model": "m", "stream": true, "tools": openAITools(executingTools),
		"messages": []any{
			map[string]any{"role": "system", "content": sysPrompt(agentExecutingMarker)},
			map[string]any{"role": "user", "content": "确认规划，授权最多花费 ✦5"},
		},
	})
	var args strings.Builder
	name, pieces := "", 0
	for _, ev := range sseData(t, resp) {
		if ev[1] == "[DONE]" {
			continue
		}
		var c struct {
			Choices []struct {
				Delta struct {
					ToolCalls []struct {
						Function struct{ Name, Arguments string } `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
		}
		_ = json.Unmarshal([]byte(ev[1]), &c)
		for _, ch := range c.Choices {
			for _, tc := range ch.Delta.ToolCalls {
				if tc.Function.Name != "" {
					name = tc.Function.Name
				}
				if tc.Function.Arguments != "" {
					pieces++
					args.WriteString(tc.Function.Arguments)
				}
			}
		}
	}
	var a map[string]any
	if name != "generate" || pieces < 2 || json.Unmarshal([]byte(args.String()), &a) != nil || a["modelCode"] != "vm-1" {
		t.Fatalf("流式工具参数应分片到达且拼回合法 JSON：name=%q pieces=%d args=%q", name, pieces, args.String())
	}
}

func TestAgentProbe_记录每次请求的工具名(t *testing.T) {
	srv, _ := newAgentTestServer(t)
	send := func(tag, note string, tools []string, stream bool) *http.Response {
		return post(t, srv.URL+"/v1/chat/completions", nil, map[string]any{
			"model": "m", "stream": stream, "tools": openAITools(tools),
			"messages": []any{
				map[string]any{"role": "system", "content": sysPrompt(note)},
				map[string]any{"role": "user", "content": "做一条短片 #tag:" + tag},
			},
		})
	}
	send("p1", agentPlanningMarker, planningTools, false).Body.Close()
	send("p1", agentExecutingMarker, executingTools, true).Body.Close()
	send("other", agentPlanningMarker, planningTools, false).Body.Close()
	// 非 Agent 请求（脚本线）不进探针。
	post(t, srv.URL+"/v1/chat/completions", nil, map[string]any{"model": "m",
		"messages": []any{map[string]any{"role": "user", "content": "你好 #tag:p1"}}}).Body.Close()

	resp, err := http.Get(srv.URL + "/_probe/agent-tools?tag=p1")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Items []agentProbeRecord `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Items) != 2 {
		t.Fatalf("tag=p1 应有 2 条，实际 %+v", out.Items)
	}
	if out.Items[0].State != "planning" || contains(out.Items[0].Tools, "generate") ||
		!contains(out.Items[0].Tools, "submit_plan") || out.Items[0].TurnHint != "load_skill" {
		t.Fatalf("规划态记录不对：%+v", out.Items[0])
	}
	if out.Items[1].State != "executing" || !contains(out.Items[1].Tools, "generate") {
		t.Fatalf("执行态记录不对：%+v", out.Items[1])
	}
}

func TestAgentScript_对话失败时HTTP500(t *testing.T) {
	srv, _ := newAgentTestServer(t)
	resp := post(t, srv.URL+"/v1/chat/completions", nil, map[string]any{
		"model": "m", "stream": true, "tools": openAITools(planningTools),
		"messages": []any{
			map[string]any{"role": "system", "content": sysPrompt(agentPlanningMarker)},
			map[string]any{"role": "user", "content": "做一条短片 #对话失败"},
		},
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("应回 500，实际 %d", resp.StatusCode)
	}
}

func TestGatedVideo_放行之前一直处理中(t *testing.T) {
	srv, _ := newAgentTestServer(t)
	resp := post(t, srv.URL+"/v1/gated/submit", nil, map[string]any{})
	var sub struct {
		TaskID string `json:"task_id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&sub)
	resp.Body.Close()
	status := func() string {
		r, err := http.Get(srv.URL + "/v1/gated/query/" + sub.TaskID)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		var q struct {
			Status string `json:"status"`
		}
		_ = json.NewDecoder(r.Body).Decode(&q)
		return q.Status
	}
	for i := 0; i < 3; i++ {
		if st := status(); st != "processing" {
			t.Fatalf("放行前应一直 processing，实际 %q", st)
		}
	}
	rel := post(t, srv.URL+"/_probe/gated/release", nil, map[string]any{})
	var out struct {
		Released int `json:"released"`
	}
	_ = json.NewDecoder(rel.Body).Decode(&out)
	rel.Body.Close()
	if out.Released != 1 || status() != "succeeded" {
		t.Fatalf("放行后应成功：released=%d", out.Released)
	}
}

func marketingSys(stateNote string) string {
	return agentSkeletonMarker + "，帮助用户……\n\n" + stateNote + "\n\n" + agentMarketingMarker + "\n场景说明\n\n" +
		"## 营销输入\n商品：山野冷萃咖啡液\n卖点：0 蔗糖\n\n## 当前视频模型\n- 模型：vm-1\n\n## 画布摘要\n（画布为空）"
}

func TestAgentScript_营销会话按标记分流(t *testing.T) {
	if isMarketingAgent(sysPrompt(agentPlanningMarker)) || agentScene(sysPrompt(agentPlanningMarker)) != "general" {
		t.Fatal("通用会话不应被认成营销会话")
	}
	sys := marketingSys(agentPlanningMarker)
	if !isMarketingAgent(sys) || agentScene(sys) != "marketing" {
		t.Fatal("营销会话应按「## 场景：营销」识别")
	}
	// 规划态第一条用户消息：不加载技能，直接写营销规划（商品取自「## 营销输入」）。
	in := agentTurnInput{System: sys, Tools: append(planningTools, "list_marketing_options"),
		Messages: []agentMsg{user("帮我做条带货视频 #tag:mk")}}
	r := agentScript(in)
	if r.ToolName != "write_doc" {
		t.Fatalf("营销会话规划态应先写规划，得到 %+v", r)
	}
	if c, _ := argsOf(t, r)["content"].(string); !strings.Contains(c, "商品：山野冷萃咖啡液") ||
		!strings.Contains(c, "vm-1") {
		t.Errorf("规划内容应带商品与模型：%q", c)
	}
	// write_doc 之后沿用通用分支：提交规划。
	in.Messages = append(in.Messages, asst("c1", "write_doc", `{}`), toolRes("c1", `{"nodeId":"d1"}`))
	if r := agentScript(in); r.ToolName != "submit_plan" {
		t.Errorf("写完规划应提交，得到 %+v", r)
	}
	// 通用会话同一条消息仍先加载技能（分流不影响通用流程）。
	gen := agentTurnInput{System: sysPrompt(agentPlanningMarker), Tools: planningTools,
		Messages: []agentMsg{user("帮我做条带货视频")}}
	if r := agentScript(gen); r.ToolName != "load_skill" {
		t.Errorf("通用会话应先加载技能，得到 %+v", r)
	}
}

func advisorSys(stateNote string) string {
	return agentSkeletonMarker + "，帮助用户……\n\n" + stateNote + "\n\n" + agentMarketingMarker + "\n场景说明\n\n" +
		"## 营销输入\n商品：手工皂\n卖点：未填写\n\n" + agentAdvisorMarker + "\n先问清生意情况……\n\n## 画布摘要\n（画布为空）"
}

func TestAgentScript_顾问只对话并写增长建议(t *testing.T) {
	if isAdvisorAgent(marketingSys(agentPlanningMarker)) {
		t.Fatal("没加载顾问技能的营销会话不应被认成顾问")
	}
	sys := advisorSys(agentPlanningMarker)
	if !isAdvisorAgent(sys) {
		t.Fatal("加载了顾问技能的营销会话应被识别")
	}
	tools := append(append([]string{}, planningTools...), "list_marketing_options")
	// 第 1 轮：写增长建议（路径在 Agent 可写的 /notes/ 下），内容带「需你核实」。
	in := agentTurnInput{System: sys, Tools: tools, Messages: []agentMsg{user("帮我分析人群和内容方向 #tag:adv")}}
	r := agentScript(in)
	a := argsOf(t, r)
	if r.ToolName != "write_doc" || a["path"] != agentAdvisorDocPath {
		t.Fatalf("顾问应先写增长建议，得到 %+v", r)
	}
	if c, _ := a["content"].(string); !strings.Contains(c, "商品：手工皂") || !strings.Contains(c, "需你核实") {
		t.Errorf("增长建议应带商品与「需你核实」：%q", c)
	}
	// 写完之后纯文本总结，不提交规划（营销 / 通用分支在这里会 submit_plan）。
	in.Messages = append(in.Messages, asst("c1", "write_doc", r.ToolArgs), toolRes("c1", `{"nodeId":"d1"}`))
	if r := agentScript(in); r.ToolName != "" || r.Action != "text" || !strings.Contains(r.Text, "需你核实") {
		t.Fatalf("写完文档应纯文本总结、不提交规划：%+v", r)
	}
	// 之后的追问：纯文本回答，不再写文档。
	in.Messages = append(in.Messages, agentMsg{Role: "assistant", Text: "已写进。"}, user("那先测什么？"))
	if r := agentScript(in); r.ToolName != "" || r.Action != "text" {
		t.Fatalf("追问应纯文本回答：%+v", r)
	}
	// 执行态不归顾问分支管（交还营销 / 通用分支）。
	if _, ok := agentAdvisorScript(agentTurnInput{System: advisorSys(agentExecutingMarker), Tools: executingTools,
		Messages: []agentMsg{user("开始")}}, nil, "executing", user("开始")); ok {
		t.Error("执行态不应走顾问分支")
	}
}
