package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/li904338203/tabtv/backend/internal/agent/app"
	"github.com/li904338203/tabtv/backend/internal/agent/domain"
	"github.com/li904338203/tabtv/backend/internal/agent/repo"
)

func run(t *testing.T, tool Tool, rc RunContext, args string) (ToolResult, error) {
	t.Helper()
	return tool.Run(context.Background(), rc, json.RawMessage(args))
}

func planPayload(t *testing.T, c repo.Card) domain.PlanConfirmPayload {
	t.Helper()
	var p domain.PlanConfirmPayload
	if err := json.Unmarshal(c.Payload, &p); err != nil {
		t.Fatal(err)
	}
	return p
}

// ---------------------------------------------------------------- 装配与动作

func TestPlanningTools_九个工具且动作与规格一致(t *testing.T) {
	ts := PlanningTools(newPtEnv().deps)
	if len(ts) != 9 {
		t.Fatalf("应有 9 个工具，实际 %d", len(ts))
	}
	reg, err := NewRegistry(ts...)
	if err != nil {
		t.Fatal(err)
	}
	for _, tl := range ts {
		spec, ok := domain.SpecOf(tl.Name())
		if !ok {
			t.Fatalf("%s 不在规格表", tl.Name())
		}
		if tl.Paid() || spec.Paid || spec.CanvasWrite {
			t.Fatalf("%s 必须是免费且非画布写工具", tl.Name())
		}
		if string(tl.Action()) != spec.Action {
			t.Fatalf("%s 自报动作 %s 与规格 %s 不一致", tl.Name(), tl.Action(), spec.Action)
		}
		var schema map[string]any
		if err := json.Unmarshal(tl.Schema(), &schema); err != nil {
			t.Fatalf("%s 的 schema 不是合法 JSON: %v", tl.Name(), err)
		}
	}
	// 规划态下发的工具全部已实现。
	if got := reg.Schemas(domain.AllowedTools(domain.StatePlanning, nil)); len(got) != 9 {
		t.Fatalf("规划态应下发 9 个工具，实际 %d", len(got))
	}
}

// ---------------------------------------------------------------- submit_plan

func TestSubmitPlan_金额由服务端报价不信模型(t *testing.T) {
	e := newPtEnv()
	res, err := run(t, e.tool(domain.ToolSubmitPlan), ptRC(domain.StatePlanning), `{"title":"西游","items":[
		{"type":"video","modelCode":"seedance","durationSec":15,"title":"主片","credits":1,"price":1},
		{"type":"image","modelCode":"img","count":3,"title":"海报","credits":1}]}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(e.store.cards) != 1 {
		t.Fatalf("应写一张卡，实际 %d", len(e.store.cards))
	}
	c := e.store.cards[0]
	if c.Kind != domain.CardPlanConfirm || c.StepID != 30 || c.SessionID != 10 {
		t.Fatalf("卡片字段不对: %+v", c)
	}
	p := planPayload(t, c)
	// 与「对同参数逐项报价之和」一致：15 秒 × 4 + 3 张 × 2 = 66。
	want := 0
	for _, it := range e.pricer.calls {
		q, _ := e.pricer.PriceItem(context.Background(), it, 0)
		want += q.Item.Credits
	}
	if p.Total != 66 || want != 66 || p.Items[0].Credits != 60 || p.Items[1].Credits != 6 {
		t.Fatalf("金额应由服务端报价: total=%d want=%d items=%+v", p.Total, want, p.Items)
	}
	if strings.Contains(string(c.Payload), `"price"`) {
		t.Fatalf("模型给的金额字段不该进卡片: %s", c.Payload)
	}
	if p.Items[0].Unit != "second" || p.Items[0].Quantity != 15 || p.Items[1].Quantity != 3 {
		t.Fatalf("计划项数量不对: %+v", p.Items)
	}
	if !p.CanApprove || p.Shortfall != 0 || p.Balance != 1000 {
		t.Fatalf("余额充足应可批准: %+v", p)
	}
	if res.Suspend == nil || res.Suspend.Event != domain.EvSubmitPlan || res.Suspend.CardID != c.ID {
		t.Fatalf("应挂起等批准: %+v", res.Suspend)
	}
	ev, ok := e.events.evs[len(e.events.evs)-1].(*app.AgentCardEvent)
	if !ok || ev.Status != "pending" || ev.OccurredAt == "" || ev.Kind != "plan_confirm" {
		t.Fatalf("应推 agent.card(pending) 且带 occurredAt: %+v", e.events.evs)
	}
}

func TestSubmitPlan_余额不足卡片带差额(t *testing.T) {
	e := newPtEnv()
	e.deps.Balances = ptBalances(50)
	res, err := run(t, e.tool(domain.ToolSubmitPlan), ptRC(domain.StatePlanning),
		`{"items":[{"type":"video","modelCode":"seedance","durationSec":15}]}`)
	if err != nil {
		t.Fatal(err)
	}
	p := planPayload(t, e.store.cards[0])
	if p.Total != 60 || p.Shortfall != 10 || p.CanApprove {
		t.Fatalf("应带差额 10 且不可批准: %+v", p)
	}
	var raw map[string]any
	_ = json.Unmarshal(e.store.cards[0].Payload, &raw)
	if raw["shortfall"] != float64(10) || raw["canApprove"] != false {
		t.Fatalf("payload 应有 shortfall / canApprove: %s", e.store.cards[0].Payload)
	}
	if !strings.Contains(string(res.Content), `"shortfall":10`) {
		t.Fatalf("回给模型的结果也要说清差额: %s", res.Content)
	}
}

func TestSubmitPlan_作废上一张规划卡(t *testing.T) {
	e := newPtEnv()
	// 一张别的类型的待处理卡不受影响。
	e.store.cards = append(e.store.cards, repo.Card{ID: 1, SessionID: 10, Kind: domain.CardChoice,
		Status: domain.CardPending})
	tool := e.tool(domain.ToolSubmitPlan)
	for i := 0; i < 2; i++ {
		if _, err := run(t, tool, ptRC(domain.StatePlanning),
			`{"items":[{"type":"image","modelCode":"img"}]}`); err != nil {
			t.Fatal(err)
		}
	}
	if len(e.store.cards) != 3 {
		t.Fatalf("应共 3 张卡: %+v", e.store.cards)
	}
	if e.store.cards[0].Status != domain.CardPending {
		t.Fatal("选择卡不该被作废")
	}
	if e.store.cards[1].Status != domain.CardVoided || e.store.cards[2].Status != domain.CardPending {
		t.Fatalf("旧规划卡应 voided、新卡 pending: %+v", e.store.cards)
	}
	voided := false
	for _, ev := range e.events.evs {
		if c, ok := ev.(*app.AgentCardEvent); ok && c.Status == "voided" && c.CardID == fmt.Sprint(e.store.cards[1].ID) {
			voided = true
		}
	}
	if !voided {
		t.Fatal("作废要推 agent.card(voided)")
	}
}

func TestSubmitPlan_不可用模型带原因不阻断其余项(t *testing.T) {
	e := newPtEnv()
	e.pricer.blocked["gone"] = "模型当前不可用"
	res, err := run(t, e.tool(domain.ToolSubmitPlan), ptRC(domain.StatePlanning), `{"items":[
		{"type":"video","modelCode":"gone"},{"type":"image","modelCode":"img","count":2}]}`)
	if err != nil {
		t.Fatal(err)
	}
	p := planPayload(t, e.store.cards[0])
	if len(p.Items) != 2 || p.Items[0].BlockedReason != "模型当前不可用" || p.Items[0].Credits != 0 {
		t.Fatalf("不可用项应带原因: %+v", p.Items)
	}
	if p.Items[1].Credits != 4 || p.Total != 4 || p.CanApprove {
		t.Fatalf("其余项照常报价、整卡不可批准: %+v", p)
	}
	if res.Suspend == nil {
		t.Fatal("卡片照发、照样挂起")
	}
}

func TestSubmitPlan_参数错误不发卡逐项退回给模型(t *testing.T) {
	e := newPtEnv()
	e.pricer.faults = map[string]app.PricedItem{
		"grok": {BlockedCode: "CAPABILITY_UNSUPPORTED", BlockedReason: "该模型此操作不接受 image 输入",
			ArgFault: app.PlanFaultInputs, Operation: "text2video"},
		"nope": {BlockedCode: "CAPABILITY_UNSUPPORTED", BlockedReason: "该模型不支持操作 img2video",
			ArgFault: app.PlanFaultOperation, Operation: "img2video"},
	}
	e.pricer.blocked["gone"] = "模型当前不可用"
	_, err := run(t, e.tool(domain.ToolSubmitPlan), ptRC(domain.StatePlanning), `{"items":[
		{"type":"video","modelCode":"grok","operationCode":"text2video","durationSec":15,"inputs":{"image":1},"title":"成片"},
		{"type":"image","modelCode":"img","count":2},
		{"type":"video","modelCode":"nope","operationCode":"img2video"},
		{"type":"video","modelCode":"gone"}]}`)
	if err == nil {
		t.Fatal("有参数错误时应作为工具错误退回")
	}
	msg := err.Error()
	for _, want := range []string{
		"没有发卡", "没有扣费", "重新调用 submit_plan",
		"第 1 项（成片 · grok · text2video）：该模型此操作不接受 image 输入。",
		"text2video 是纯文生操作，不接受 image 输入", "去掉 inputs", "改用 img2video",
		"第 3 项（视频 · nope · img2video）：该模型不支持操作 img2video。", "operationCode",
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("退回消息应包含 %q：\n%s", want, msg)
		}
	}
	// 只列参数错误：可用项与「模型下线」这类非参数错误不在里面。
	if strings.Contains(msg, "第 2 项") || strings.Contains(msg, "第 4 项") {
		t.Fatalf("非参数错误不该列为要模型改的项：\n%s", msg)
	}
	if len(e.store.cards) != 0 || len(e.events.evs) != 0 {
		t.Fatalf("参数错误不该发卡也不该推事件: cards=%+v evs=%+v", e.store.cards, e.events.evs)
	}
}

func TestSubmitPlan_非参数错误照旧发卡(t *testing.T) {
	// 会员等级不够（用户可解决）与余额不足都不是模型写错的参数：照常发卡、卡上说明原因。
	e := newPtEnv()
	e.deps.Balances = ptBalances(10)
	e.pricer.blocked["vip"] = "该模型需要更高的会员等级"
	res, err := run(t, e.tool(domain.ToolSubmitPlan), ptRC(domain.StatePlanning), `{"items":[
		{"type":"video","modelCode":"vip"},{"type":"video","modelCode":"seedance","durationSec":5}]}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(e.store.cards) != 1 || res.Suspend == nil {
		t.Fatalf("应照常发卡并挂起: %+v", e.store.cards)
	}
	p := planPayload(t, e.store.cards[0])
	if p.CanApprove || p.Items[0].BlockedReason == "" || p.Shortfall != 10 {
		t.Fatalf("卡上应带不可用原因与差额: %+v", p)
	}
}

func TestSubmitPlan_记规划文档版本且只在规划态(t *testing.T) {
	e := newPtEnv()
	e.docs.docs[PlanDocPath] = app.Document{ID: 77, Path: PlanDocPath, Version: 3}
	if _, err := run(t, e.tool(domain.ToolSubmitPlan), ptRC(domain.StatePlanning),
		`{"items":[{"type":"image","modelCode":"img"}]}`); err != nil {
		t.Fatal(err)
	}
	if p := planPayload(t, e.store.cards[0]); p.DocPath != PlanDocPath || p.PlanDocVersion != 3 {
		t.Fatalf("应记 plan.md 版本: %+v", p)
	}
	if _, err := run(t, e.tool(domain.ToolSubmitPlan), ptRC(domain.StateExecuting),
		`{"items":[{"type":"image","modelCode":"img"}]}`); err == nil {
		t.Fatal("执行态不能提交规划")
	}
	if _, err := run(t, e.tool(domain.ToolSubmitPlan), ptRC(domain.StatePlanning),
		`{"items":[{"type":"video","modelCode":"seedance","count":2}]}`); err == nil {
		t.Fatal("视频 count>1 应要求逐条列出")
	}
}

// ---------------------------------------------------------------- ask_user

func TestAskUser_写卡并挂起(t *testing.T) {
	e := newPtEnv()
	res, err := run(t, e.tool(domain.ToolAskUser), ptRC(domain.StatePlanning), `{"kind":"choice","title":"选方向",
		"allowOther":true,"options":[{"label":"悟空孤影","detail":"忠义"},{"label":"三打燃战"}]}`)
	if err != nil {
		t.Fatal(err)
	}
	c := e.store.cards[0]
	if c.Kind != domain.CardChoice || c.StepID != 30 {
		t.Fatalf("卡片不对: %+v", c)
	}
	var p domain.ChoicePayload
	_ = json.Unmarshal(c.Payload, &p)
	if len(p.Options) != 2 || p.Options[0].ID != "1" || p.Options[1].ID != "2" || !p.AllowOther {
		t.Fatalf("选项应补 ID: %+v", p)
	}
	if res.Suspend == nil || res.Suspend.Event != domain.EvAskUser || res.Suspend.CardID != c.ID {
		t.Fatalf("应挂起等输入: %+v", res.Suspend)
	}
	// 写出来的卡用户能正常提交（选一项）。
	if err := domain.ValidateSubmission(c.Domain(), json.RawMessage(`{"selected":["2"]}`)); err != nil {
		t.Fatalf("卡片应可提交: %v", err)
	}
	// 问答卡。
	if _, err := run(t, e.tool(domain.ToolAskUser), ptRC(domain.StatePlanning), `{"kind":"questions","title":"细节",
		"questions":[{"text":"结尾情绪？","type":"single","options":[{"label":"悲壮"},{"label":"留白"}]},
		{"text":"补充","type":"text"}]}`); err != nil {
		t.Fatal(err)
	}
	if e.store.cards[1].Kind != domain.CardQuestions {
		t.Fatal("应写问答卡")
	}
}

func TestAskUser_选项为空拒绝(t *testing.T) {
	e := newPtEnv()
	for _, args := range []string{
		`{"kind":"choice","title":"x","options":[]}`,
		`{"kind":"choice","title":"x"}`,
		`{"kind":"choice","title":"","options":[{"label":"a"}]}`,
		`{"kind":"choice","title":"x","options":[{"id":"a","label":"1"},{"id":"a","label":"2"}]}`,
		`{"kind":"questions","title":"x","questions":[{"text":"q","type":"single","options":[]}]}`,
		`{"kind":"questions","title":"x","questions":[]}`,
		`{"kind":"other","title":"x","options":[{"label":"a"}]}`,
		`{"kind":"choice","title":"x","options":[{"label":"a"}],"unknown":1}`,
	} {
		if _, err := run(t, e.tool(domain.ToolAskUser), ptRC(domain.StatePlanning), args); err == nil {
			t.Fatalf("应拒绝: %s", args)
		}
	}
	if len(e.store.cards) != 0 {
		t.Fatal("参数不合法时不能写卡")
	}
}

func TestAskUser_记录恢复状态(t *testing.T) {
	e := newPtEnv()
	for _, st := range []domain.State{domain.StatePlanning, domain.StateExecuting} {
		if _, err := run(t, e.tool(domain.ToolAskUser), ptRC(st),
			`{"kind":"choice","title":"x","options":[{"label":"a"}]}`); err != nil {
			t.Fatal(err)
		}
		c := e.store.cards[len(e.store.cards)-1]
		got, err := domain.ResumeStateOf(c.Domain())
		if err != nil || got != st {
			t.Fatalf("resumeState 应为 %s，实际 %s (%v)", st, got, err)
		}
	}
	if _, err := run(t, e.tool(domain.ToolAskUser), ptRC(domain.StateIdle),
		`{"kind":"choice","title":"x","options":[{"label":"a"}]}`); err == nil {
		t.Fatal("非规划 / 执行态不能发卡")
	}
}

// ---------------------------------------------------------------- 文档

func TestWriteDoc_路径白名单(t *testing.T) {
	e := newPtEnv()
	for _, p := range []string{"/../x", "/a.txt", "/notes/../x.md", "/notes/a/b.md", "/notes/.md",
		"/notes/" + strings.Repeat("长", 65) + ".md", "plan.md", "/notes/a b.md", "/PLAN.md"} {
		if _, err := run(t, e.tool(domain.ToolWriteDoc), ptRC(domain.StatePlanning),
			fmt.Sprintf(`{"path":%q,"content":"x"}`, p)); err == nil {
			t.Fatalf("应拒绝路径 %q", p)
		}
		if _, err := run(t, e.tool(domain.ToolReadDoc), ptRC(domain.StatePlanning),
			fmt.Sprintf(`{"path":%q}`, p)); err == nil {
			t.Fatalf("read_doc 也应拒绝路径 %q", p)
		}
	}
	if len(e.docs.puts) != 0 {
		t.Fatal("白名单外不能写")
	}
	for _, p := range []string{"/plan.md", "/shots.md", "/.memory.md", "/notes/角色-设定_1.md",
		"/notes/" + strings.Repeat("长", 64) + ".md"} {
		if err := ValidAgentDocPath(p); err != nil {
			t.Fatalf("应放行 %q: %v", p, err)
		}
	}
}

func TestWriteDoc_首次写plan建文档节点(t *testing.T) {
	e := newPtEnv()
	e.canvas.data = json.RawMessage(`{"nodes":[{"id":"n1","type":"image","position":{"x":100,"y":0},"size":{"w":330}}],"edges":[]}`)
	tool := e.tool(domain.ToolWriteDoc)
	res, err := run(t, tool, ptRC(domain.StatePlanning), `{"path":"/plan.md","content":"# 规划"}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(e.canvas.applied) != 1 {
		t.Fatalf("首次写 plan.md 应建一个文档节点: %+v", e.canvas.applied)
	}
	in := e.canvas.applied[0]
	var ops []map[string]any
	_ = json.Unmarshal(in.Ops, &ops)
	node := ops[0]["node"].(map[string]any)
	agent := node["agent"].(map[string]any)
	params := node["params"].(map[string]any)
	if ops[0]["kind"] != "add_node" || node["type"] != "document" || agent["sessionId"] != "10" ||
		agent["stepId"] != "30" || params["docId"] != "900" || len(params) != 1 {
		t.Fatalf("文档节点形状不对（只存 docId）: %s", in.Ops)
	}
	if in.AgentSessionID != 10 || in.BatchID == "" || ops[0]["position"].(map[string]any)["x"] != float64(510) {
		t.Fatalf("Apply 入参不对: %+v / %s", in, in.Ops)
	}
	if !strings.Contains(string(res.Display), `"docId":"900"`) || !strings.Contains(string(res.Display), `"fileName":"plan.md"`) {
		t.Fatalf("Display 应带文件名与 docId: %s", res.Display)
	}
	// 再写（带版本）不再建节点；不带版本拒绝（防止覆盖用户改动）。
	if _, err := run(t, tool, ptRC(domain.StatePlanning), `{"path":"/plan.md","content":"v2"}`); err == nil {
		t.Fatal("已存在的文档不带 expectedVersion 应拒绝")
	}
	if _, err := run(t, tool, ptRC(domain.StatePlanning), `{"path":"/plan.md","content":"v2","expectedVersion":1}`); err != nil {
		t.Fatal(err)
	}
	if len(e.canvas.applied) != 1 {
		t.Fatal("更新不该再建节点")
	}
	// 其他文档首次写不建节点。
	if _, err := run(t, tool, ptRC(domain.StatePlanning), `{"path":"/notes/x.md","content":"n"}`); err != nil {
		t.Fatal(err)
	}
	if len(e.canvas.applied) != 1 {
		t.Fatal("只有 plan.md 建节点")
	}
}

func TestReadDoc_不存在与截断(t *testing.T) {
	e := newPtEnv()
	res, err := run(t, e.tool(domain.ToolReadDoc), ptRC(domain.StatePlanning), `{"path":"/shots.md"}`)
	if err != nil || !strings.Contains(string(res.Content), `"exists":false`) {
		t.Fatalf("不存在应回 exists=false: %s %v", res.Content, err)
	}
	e.docs.docs["/shots.md"] = app.Document{ID: 5, Path: "/shots.md", Version: 2, Content: strings.Repeat("字", 20000)}
	res, err = run(t, e.tool(domain.ToolReadDoc), ptRC(domain.StatePlanning), `{"path":"/shots.md"}`)
	if err != nil {
		t.Fatal(err)
	}
	if ptRunes(res.Content) > MaxContentRunes || !strings.Contains(string(res.Content), `"truncated":true`) {
		t.Fatalf("超长正文应截断到上限内: %d", ptRunes(res.Content))
	}
}

// ---------------------------------------------------------------- read_canvas

func bigCanvas(n int, prompt string) json.RawMessage {
	var nodes []string
	for i := 0; i < n; i++ {
		nodes = append(nodes, fmt.Sprintf(`{"id":"n%d","type":"image","title":"节点%d","params":{"prompt":%q}}`, i, i, prompt))
	}
	return json.RawMessage(`{"nodes":[` + strings.Join(nodes, ",") + `],"edges":[{"source":"n0","target":"n1","order":1}]}`)
}

func TestReadCanvas_不含params全文(t *testing.T) {
	e := newPtEnv()
	e.canvas.data = bigCanvas(3, "秘密提示词全文")
	e.deps.Generations = ptGens{"n1": {GenerationID: 5, Status: "failed", ErrorCode: "x"},
		"n2": {GenerationID: 6, Status: "success", OutputMediaID: 99, OutputType: "image"}}
	res, err := run(t, e.tool(domain.ToolReadCanvas), ptRC(domain.StatePlanning), `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(res.Content), "秘密提示词全文") || strings.Contains(string(res.Content), `"params"`) {
		t.Fatalf("未指定 nodeIds 时不能含 params: %s", res.Content)
	}
	for _, want := range []string{`"status":"failed"`, `"mediaId":"99"`, `"nodeCount":3`, `"source":"n0"`} {
		if !strings.Contains(string(res.Content), want) {
			t.Fatalf("结果缺 %s: %s", want, res.Content)
		}
	}
	if string(res.Display) != `{"nodeCount":3}` {
		t.Fatalf("Display 应给节点数: %s", res.Display)
	}
	// 指定 nodeIds 时返回 params，并报告不存在的 ID。
	res, err = run(t, e.tool(domain.ToolReadCanvas), ptRC(domain.StatePlanning), `{"nodeIds":["n1","nx"]}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(res.Content), "秘密提示词全文") || !strings.Contains(string(res.Content), `"missing":["nx"]`) ||
		strings.Contains(string(res.Content), `"id":"n2"`) {
		t.Fatalf("精确读取应只回指定节点且带 params: %s", res.Content)
	}
}

func TestReadCanvas_结果超长截断(t *testing.T) {
	e := newPtEnv()
	e.canvas.data = bigCanvas(400, "x")
	res, err := run(t, e.tool(domain.ToolReadCanvas), ptRC(domain.StatePlanning), `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if ptRunes(res.Content) > MaxContentRunes {
		t.Fatalf("超过上限: %d", ptRunes(res.Content))
	}
	var out struct {
		NodeCount int               `json:"nodeCount"`
		Nodes     []json.RawMessage `json:"nodes"`
		Truncated bool              `json:"truncated"`
		Note      string            `json:"note"`
	}
	if err := json.Unmarshal(res.Content, &out); err != nil {
		t.Fatalf("截断后仍须是合法 JSON: %v", err)
	}
	if !out.Truncated || out.Note != TruncatedNote || out.NodeCount != 400 || len(out.Nodes) == 0 || len(out.Nodes) >= 400 {
		t.Fatalf("截断标记不对: truncated=%v note=%q count=%d nodes=%d", out.Truncated, out.Note, out.NodeCount, len(out.Nodes))
	}
}

// ---------------------------------------------------------------- list_assets / quote

func TestListAssets_只回ID与名称(t *testing.T) {
	e := newPtEnv()
	e.assets.page = app.AssetPage{Items: []app.AssetItem{{ID: "1", Name: strings.Repeat("名", 60), Type: "character",
		ThumbMediaID: "9"}}, NextCursor: "20"}
	res, err := run(t, e.tool(domain.ToolListAssets), ptRC(domain.StatePlanning), `{"source":"character","limit":99}`)
	if err != nil {
		t.Fatal(err)
	}
	if e.assets.got.Limit != 20 || e.assets.got.Source != "character" {
		t.Fatalf("limit 应夹到 20: %+v", e.assets.got)
	}
	var page struct {
		Items      []map[string]any `json:"items"`
		NextCursor string           `json:"nextCursor"`
	}
	_ = json.Unmarshal(res.Content, &page)
	for k := range page.Items[0] {
		if k != "id" && k != "name" && k != "type" && k != "thumbMediaId" {
			t.Fatalf("多出字段 %s", k)
		}
	}
	if len([]rune(page.Items[0]["name"].(string))) != 40 || page.NextCursor != "20" {
		t.Fatalf("名称截断 / 游标不对: %+v", page)
	}
	for _, bad := range []string{`{"source":"x"}`, `{"source":"character","kind":"image"}`, `{"kind":"doc"}`} {
		if _, err := run(t, e.tool(domain.ToolListAssets), ptRC(domain.StatePlanning), bad); err == nil {
			t.Fatalf("应拒绝 %s", bad)
		}
	}
}

func TestQuote_返回金额与不可用原因(t *testing.T) {
	e := newPtEnv()
	res, err := run(t, e.tool(domain.ToolQuote), ptRC(domain.StatePlanning),
		`{"type":"video","modelCode":"seedance","durationSec":10}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(res.Content), `"credits":40`) || !strings.Contains(string(res.Display), `"credits":40`) {
		t.Fatalf("应报 40: %s / %s", res.Content, res.Display)
	}
	e.pricer.blocked["gone"] = "模型当前不可用"
	res, err = run(t, e.tool(domain.ToolQuote), ptRC(domain.StatePlanning), `{"type":"image","modelCode":"gone"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(res.Content), `"available":false`) || !strings.Contains(string(res.Content), `"userFixable":false`) {
		t.Fatalf("不可用应带原因与 userFixable: %s", res.Content)
	}
	if strings.Contains(string(res.Content), "argumentError") {
		t.Fatalf("非参数错误不带 argumentError: %s", res.Content)
	}
	e.pricer.faults = map[string]app.PricedItem{"grok": {BlockedReason: "该模型此操作不接受 image 输入",
		ArgFault: app.PlanFaultInputs, Operation: "text2video"}}
	res, err = run(t, e.tool(domain.ToolQuote), ptRC(domain.StatePlanning),
		`{"type":"video","modelCode":"grok","inputs":{"image":1}}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(res.Content), `"argumentError":true`) || !strings.Contains(string(res.Content), "去掉 inputs") {
		t.Fatalf("参数错误应带 argumentError 与改法: %s", res.Content)
	}
}

// ---------------------------------------------------------------- 技能

func TestLoadSkill_白名单只收窄(t *testing.T) {
	e := newPtEnv()
	e.store.skills["story"] = repo.Skill{ID: 1, Code: "story", Name: "故事选题", IsEnabled: true,
		Body: "当前阶段 {{state}}", Tools: json.RawMessage(`["read_canvas","ask_user","generate"]`)}
	res, err := run(t, e.tool(domain.ToolLoadSkill), ptRC(domain.StatePlanning), `{"code":"story"}`)
	if err != nil {
		t.Fatal(err)
	}
	if string(e.store.session.SkillCodes) != `["story"]` {
		t.Fatalf("会话 skill_codes 应含该技能: %s", e.store.session.SkillCodes)
	}
	// 重复加载不重复追加。
	if _, err := run(t, e.tool(domain.ToolLoadSkill), ptRC(domain.StatePlanning), `{"code":"story"}`); err != nil {
		t.Fatal(err)
	}
	if string(e.store.session.SkillCodes) != `["story"]` {
		t.Fatalf("重复加载不该重复追加: %s", e.store.session.SkillCodes)
	}
	if !strings.Contains(string(res.Display), "故事选题") {
		t.Fatalf("Display 应带技能名: %s", res.Display)
	}
	// 白名单只收窄：规划态即使技能写了 generate 也不下发。
	var wl []domain.ToolName
	_ = json.Unmarshal(e.store.skills["story"].Tools, &wl)
	got := domain.AllowedTools(domain.StatePlanning, wl)
	if len(got) != 2 || got[0] != domain.ToolReadCanvas || got[1] != domain.ToolAskUser {
		t.Fatalf("规划态 + 技能白名单应只剩 read_canvas / ask_user: %v", got)
	}
	// 不存在 / 已停用。
	e.store.skills["off"] = repo.Skill{ID: 2, Code: "off", IsEnabled: false}
	for _, c := range []string{"nope", "off"} {
		if _, err := run(t, e.tool(domain.ToolLoadSkill), ptRC(domain.StatePlanning), `{"code":"`+c+`"}`); err == nil {
			t.Fatalf("%s 应报错", c)
		}
	}
}

func TestLoadSkill_渲染失败报错不静默(t *testing.T) {
	e := newPtEnv()
	e.store.skills["bad"] = repo.Skill{ID: 3, Code: "bad", Name: "坏技能", IsEnabled: true, Body: "写给 {{未知变量}}"}
	_, err := run(t, e.tool(domain.ToolLoadSkill), ptRC(domain.StatePlanning), `{"code":"bad"}`)
	if err == nil || !strings.Contains(err.Error(), "渲染失败") {
		t.Fatalf("渲染失败应报错: %v", err)
	}
	if string(e.store.session.SkillCodes) != `[]` {
		t.Fatalf("渲染失败不能加载: %s", e.store.session.SkillCodes)
	}
}

func TestReadSkillRef_渲染并读取(t *testing.T) {
	e := newPtEnv()
	e.store.skills["story"] = repo.Skill{ID: 1, Code: "story", Name: "故事选题", IsEnabled: true}
	e.store.refs["1/写法"] = repo.SkillReference{SkillID: 1, Name: "写法", Body: "模型 {{reasoningModel}}"}
	e.store.refs["1/坏"] = repo.SkillReference{SkillID: 1, Name: "坏", Body: "{{nope}}"}
	res, err := run(t, e.tool(domain.ToolReadSkillRef), ptRC(domain.StatePlanning), `{"code":"story","name":"写法"}`)
	if err != nil || !strings.Contains(string(res.Content), "模型 claude") {
		t.Fatalf("应渲染变量: %s %v", res.Content, err)
	}
	if _, err := run(t, e.tool(domain.ToolReadSkillRef), ptRC(domain.StatePlanning), `{"code":"story","name":"坏"}`); err == nil {
		t.Fatal("渲染失败应报错")
	}
	if _, err := run(t, e.tool(domain.ToolReadSkillRef), ptRC(domain.StatePlanning), `{"code":"story","name":"无"}`); err == nil {
		t.Fatal("不存在应报错")
	}
}

// 04 文档：实测模型常给文本题带几个参考答案、把唯一一道题的选项写在卡片顶层，
// 原先一律退回，每次都多花一次模型调用；意思明确的照意思出题。
func TestAskUser_常见写法按意思出题不退回(t *testing.T) {
	e := newPtEnv()
	_, err := run(t, e.tool(domain.ToolAskUser), ptRC(domain.StatePlanning), `{"kind":"questions","title":"确认",`+
		`"questions":[{"id":"d","text":"时长偏好？","type":"text","options":[{"label":"15 秒"},{"label":"30 秒"}]}]}`)
	if err != nil {
		t.Fatalf("文本题带选项应按单选 + 可自填出题：%v", err)
	}
	var p domain.QuestionsPayload
	_ = json.Unmarshal(e.store.cards[len(e.store.cards)-1].Payload, &p)
	if q := p.Questions[0]; q.Type != domain.QuestionSingle || !q.AllowOther || len(q.Options) != 2 {
		t.Fatalf("应改成单选并允许自己写：%+v", q)
	}

	_, err = run(t, e.tool(domain.ToolAskUser), ptRC(domain.StatePlanning), `{"kind":"questions","title":"风格",`+
		`"options":[{"label":"国风"},{"label":"赛博"}],"multi":true,"questions":[{"id":"s","text":"想要什么风格？"}]}`)
	if err != nil {
		t.Fatalf("唯一一道题的选项写在顶层应挪进去：%v", err)
	}
	_ = json.Unmarshal(e.store.cards[len(e.store.cards)-1].Payload, &p)
	if q := p.Questions[0]; q.Type != domain.QuestionMulti || len(q.Options) != 2 {
		t.Fatalf("顶层的选项挪进那道题（多选）：%+v", q)
	}

	// 两道题而选项在顶层：说不清给哪道，照旧退回。
	_, err = run(t, e.tool(domain.ToolAskUser), ptRC(domain.StatePlanning), `{"kind":"questions","title":"x",`+
		`"options":[{"label":"a"}],"questions":[{"text":"一"},{"text":"二"}]}`)
	if err == nil {
		t.Fatal("多道题时顶层选项说不清归属，应退回")
	}
}

// 04 文档 P-22②：营销技能的正文假定了营销会话才有的输入与工具，通用会话里加载它，第一步就调不到工具。
func TestLoadSkill_只加载适用于会话场景的技能(t *testing.T) {
	e := newPtEnv() // 通用会话
	e.store.skills["mk"] = repo.Skill{ID: 3, Code: "mk", Name: "营销技能", IsEnabled: true, Body: "x",
		Scenes: json.RawMessage(`["marketing"]`)}
	e.store.skills["both"] = repo.Skill{ID: 4, Code: "both", Name: "通用", IsEnabled: true, Body: "x",
		Scenes: json.RawMessage(`["general","marketing"]`)}
	if _, err := run(t, e.tool(domain.ToolLoadSkill), ptRC(domain.StatePlanning), `{"code":"mk"}`); err == nil ||
		!strings.Contains(err.Error(), "不适用于当前会话的场景") {
		t.Fatalf("通用会话不能加载仅营销场景的技能：%v", err)
	}
	if _, err := run(t, e.tool(domain.ToolLoadSkill), ptRC(domain.StatePlanning), `{"code":"both"}`); err != nil {
		t.Fatalf("两个场景都适用的技能可以加载：%v", err)
	}
}
