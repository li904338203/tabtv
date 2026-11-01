package main

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// 营销 E2E 分支（MKT-T-30）：魔法词决定走哪条路，每条都是 scripts/e2e-provider.sh 营销段依赖的形状。

func marketingSysN(stateNote string, products int) string {
	return agentSkeletonMarker + "，帮助用户……\n\n" + stateNote + "\n\n" + agentMarketingMarker + "\n场景说明\n\n" +
		"## 营销输入\n商品：山野冷萃咖啡液\n卖点：0 蔗糖\n商品图：共 " + strconv.Itoa(products) + " 张，#1 为主图。\n\n" +
		"## 当前视频模型\n- 模型：vm-1\n\n## 画布摘要\n（画布为空）"
}

func indexesOf(t *testing.T, r agentReply) []any {
	t.Helper()
	items, _ := argsOf(t, r)["items"].([]any)
	if len(items) == 0 {
		t.Fatalf("submit_plan 应带 items：%+v", r)
	}
	it, _ := items[0].(map[string]any)
	idx, _ := it["productIndexes"].([]any)
	return idx
}

func TestMarketingScript_看图再写规划_超限选图被退回后重交(t *testing.T) {
	tools := append(append([]string{}, planningTools...), "view_media", "list_marketing_options")
	in := agentTurnInput{System: marketingSysN(agentPlanningMarker, 5), Tools: tools,
		Messages: []agentMsg{user("做一条带货短片 #超限选图 #看图5 #tag:mk")}}
	r := agentScript(in)
	if r.ToolName != "view_media" || argsOf(t, r)["index"] != float64(5) {
		t.Fatalf("#看图5 应先 view_media 商品图 #5：%+v", r)
	}
	// Runner 在工具结果之后追加一条带图的系统通知 → 写规划。
	in.Messages = append(in.Messages, asst("c1", "view_media", r.ToolArgs), toolRes("c1", `{"attached":true}`),
		user(agentSystemEventPrefix+"商品图 #5"))
	if r = agentScript(in); r.ToolName != "write_doc" {
		t.Fatalf("看完图应写规划：%+v", r)
	}
	in.Messages = append(in.Messages, asst("c2", "write_doc", r.ToolArgs), toolRes("c2", `{"nodeId":"d1"}`))
	r = agentScript(in)
	if r.ToolName != "submit_plan" || len(indexesOf(t, r)) != 4 {
		t.Fatalf("#超限选图 第一次应选 4 张：%+v", r)
	}
	in.Messages = append(in.Messages, asst("c3", "submit_plan", r.ToolArgs),
		toolRes("c3", `{"error":"too_many_images","max":3,"selected":4,"total":5}`))
	r = agentScript(in)
	if r.ToolName != "submit_plan" || len(indexesOf(t, r)) != 2 {
		t.Fatalf("被退回后应改选 2 张重交：%+v", r)
	}
	in.Messages = append(in.Messages, asst("c4", "submit_plan", r.ToolArgs),
		toolRes("c4", `{"cardId":"9","items":[{"key":"v1"}]}`))
	if r = agentScript(in); r.ToolName != "" {
		t.Fatalf("发卡之后应纯文本收尾：%+v", r)
	}
}

func TestMarketingScript_选图不超过会话里的商品图张数(t *testing.T) {
	in := agentTurnInput{System: marketingSysN(agentPlanningMarker, 1), Tools: planningTools, Messages: []agentMsg{
		user("做一条"), asst("c1", "write_doc", `{}`), toolRes("c1", `{"nodeId":"d1"}`)}}
	if idx := indexesOf(t, agentScript(in)); len(idx) != 1 {
		t.Fatalf("只有 1 张商品图时只能选 #1：%v", idx)
	}
}

func TestMarketingScript_批量按条目键逐条生成_最后一条才等待(t *testing.T) {
	sys := marketingSysN(agentExecutingMarker, 1)
	in := agentTurnInput{System: sys, Tools: executingTools, Messages: []agentMsg{
		user("做 3 个变体 #变体3 #变体失败2"),
		asst("c1", "submit_plan", `{}`),
		toolRes("c1", `{"cardId":"9","items":[{"key":"v1"},{"key":"v2"},{"key":"v3"}]}`),
		user("批准规划"),
	}}
	want := []struct {
		key  string
		wait bool
		fail bool
	}{{"v1", false, false}, {"v2", false, true}, {"v3", true, false}}
	for i, w := range want {
		r := agentScript(in)
		a := argsOf(t, r)
		if r.ToolName != "generate" || a["planItemKey"] != w.key || a["wait"] != w.wait {
			t.Fatalf("第 %d 次应 generate %s wait=%v：%+v", i+1, w.key, w.wait, r)
		}
		if p, _ := a["prompt"].(string); strings.Contains(p, magicUpstreamFail) != w.fail {
			t.Fatalf("%s 的画面描述带不带 %s 不对：%q", w.key, magicUpstreamFail, p)
		}
		id := "g" + w.key
		in.Messages = append(in.Messages, asst(id, "generate", r.ToolArgs), toolRes(id, `{"generationId":"1","nodeId":"n`+w.key+`"}`))
	}
	if r := agentScript(in); r.ToolName != "" {
		t.Fatalf("条目都提交后应纯文本：%+v", r)
	}
	// 多变体唤醒：服务端已排好画框，不再自己画框。
	in.Messages = append(in.Messages, user(agentSystemEventPrefix+"所等的 3 个任务已全部结束：成功 2、失败 1、取消 0。"))
	if r := agentScript(in); r.ToolName != "" || !strings.Contains(r.Text, "成功 2") {
		t.Fatalf("多变体唤醒应纯文本总结：%+v", r)
	}
}

func TestMarketingScript_单条唤醒只框本次生成与首份规划文档(t *testing.T) {
	in := agentTurnInput{System: marketingSysN(agentExecutingMarker, 2), Tools: executingTools, Messages: []agentMsg{
		user("做一条"), asst("c1", "write_doc", `{}`), toolRes("c1", `{"nodeId":"doc"}`),
		asst("c2", "submit_plan", `{}`), toolRes("c2", `{"items":[{"key":"v1"}]}`),
		user("批准"), asst("c3", "generate", `{"planItemKey":"v1"}`), toolRes("c3", `{"generationId":"1","nodeId":"n1"}`),
		user(agentSystemEventPrefix + "所等的 1 个任务已全部结束：成功 1、失败 0、取消 0。"),
	}}
	r := agentScript(in)
	if r.ToolName != "canvas_apply" || !strings.Contains(r.ToolArgs, `"n1"`) || !strings.Contains(r.ToolArgs, `"doc"`) {
		t.Fatalf("单条唤醒应把成片与规划文档框在一起：%+v", r)
	}
	// 同会话第二次：已画过框，只框新的成片。
	in.Messages = append(in.Messages, asst("c4", "canvas_apply", r.ToolArgs), toolRes("c4", `{"ok":true}`),
		user("再来一版"), asst("c5", "submit_plan", `{}`), toolRes("c5", `{"items":[{"key":"v1"}]}`),
		user("批准"), asst("c6", "generate", `{"planItemKey":"v1"}`), toolRes("c6", `{"generationId":"2","nodeId":"n2"}`),
		user(agentSystemEventPrefix+"所等的 1 个任务已全部结束：成功 1、失败 0、取消 0。"))
	r = agentScript(in)
	if r.ToolName != "canvas_apply" || !strings.Contains(r.ToolArgs, `"n2"`) ||
		strings.Contains(r.ToolArgs, `"n1"`) || strings.Contains(r.ToolArgs, `"doc"`) {
		t.Fatalf("第二次只框新的成片：%+v", r)
	}
}

func TestMarketingScript_顾问出示意图走确认卡(t *testing.T) {
	in := agentTurnInput{System: advisorSys(agentPlanningMarker), Tools: planningTools, Messages: []agentMsg{
		user("分析人群"), asst("c1", "write_doc", `{"path":"/notes/增长建议.md"}`), toolRes("c1", `{"nodeId":"d1"}`),
		user("出一张示意图 #出示意图 #image=img-1"),
	}}
	r := agentScript(in)
	if r.ToolName != "submit_plan" || !strings.Contains(r.ToolArgs, `"img-1"`) || !strings.Contains(r.ToolArgs, `"img1"`) {
		t.Fatalf("顾问出示意图应提交一张图片条目：%+v", r)
	}
}

func TestSeedance_画面描述带上游失败标记时提交即400(t *testing.T) {
	s := &server{}
	rec := httptest.NewRecorder()
	body := `{"model":"m","content":[{"type":"text","text":"画面 ` + magicUpstreamFail + `"}],"resolution":"720p","duration":5}`
	s.seedanceSubmit(rec, httptest.NewRequest(http.MethodPost, "/v1/seedance/submit", strings.NewReader(body)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("带 %s 应 400，实际 %d %s", magicUpstreamFail, rec.Code, rec.Body.String())
	}
}

func TestAgentInput_统计图片块与外链(t *testing.T) {
	in := imagePartsOfMsgs([]any{
		map[string]any{"type": "text", "text": "看图"},
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,AAAA"}},
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://cdn.example.com/a.png"}},
	})
	if in[0] != 2 || in[1] != 1 {
		t.Fatalf("应 2 张图、其中 1 张外链：%v", in)
	}
	if u := bodyURLs([]byte(`{"a":"https://x.example/p?q=1","b":"https://x.example/p?q=1"}`)); len(u) != 1 {
		t.Fatalf("地址应去重：%v", u)
	}
}

func imagePartsOfMsgs(content any) [2]int {
	a, b := imagePartsOf(content)
	return [2]int{a, b}
}

func TestAgentProbe_统计音频块与内联音频(t *testing.T) {
	content := []any{
		map[string]any{"type": "text", "text": "听一下"},
		map[string]any{"type": "input_audio", "input_audio": map[string]any{"data": "AAAA", "format": "wav"}},
		map[string]any{"type": "audio_url", "audio_url": map[string]any{"url": "data:audio/wav;base64,AAAA"}},
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,AAAA"}},
	}
	if n := audioPartsOf(content); n != 2 {
		t.Fatalf("应数出 2 个音频块：%d", n)
	}
	if n := bodyAudioData([]byte(`{"a":"data:audio/wav;base64,AA","b":"data:image/png;base64,AA"}`)); n != 1 {
		t.Fatalf("应数出 1 处内联音频：%d", n)
	}
	var p agentProbe
	p.record(agentTurnInput{Messages: []agentMsg{{Role: "user", Text: "x", Audios: 2}}, BodyAudioData: 1}, agentReply{})
	if got := p.list("", 10); len(got) != 1 || got[0].Audios != 3 {
		t.Fatalf("探针应记 audios=3：%+v", got)
	}
	p.record(agentTurnInput{Messages: []agentMsg{{Role: "user", Text: "y", Images: 1}}}, agentReply{})
	if got := p.list("", 10); got[1].Audios != 0 {
		t.Fatalf("无音频的请求 audios 应为 0：%+v", got[1])
	}
}
