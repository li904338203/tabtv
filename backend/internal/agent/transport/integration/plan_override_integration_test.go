//go:build integration

// 确认卡换图（MKT-T-20、MKD-9）：用户批准时带 itemOverrides，服务端按会话的商品图重算条目、
// 在事务内重新报价；合计变了按现有规则 409 + 新报价（不静默批准），没变则批准且卡片 payload
// 就是「已批准计划」（换后的那张）。走真实路由 + 真库；报价用按输入计数计价的假实现。
package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"testing"

	agentapp "github.com/li904338203/tabtv/backend/internal/agent/app"
	"github.com/li904338203/tabtv/backend/internal/agent/domain"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
)

// imagePlans 按输入计数报价：每段 base + 每张图 perImage（perImage=0 = 换图不影响价格）。
type imagePlans struct {
	base, perImage int
}

func (p *imagePlans) QuotePlan(_ context.Context, items []domain.PlanItem, _ int64) ([]domain.PlanItem, error) {
	out := make([]domain.PlanItem, len(items))
	for i, it := range items {
		it.Credits = p.base + p.perImage*it.Inputs["image"]
		it.BlockedCode, it.BlockedReason, it.UserFixable = "", "", false
		out[i] = it
	}
	return out, nil
}

// marketingPlanCard 把会话改成营销会话（5 张商品图），并挂起在一张营销规划卡上：
// v1 选了 #1 #2（上限 3 张），v2 是配音（没有选图）。
func marketingPlanCard(t *testing.T, e *env, plans *imagePlans) (sid, cid int64) {
	t.Helper()
	sid = e.createSession("k-mkt-plan-" + strconv.FormatInt(e.next(), 10))
	si := agentapp.SceneInput{Products: []string{"501", "502", "503", "504", "505"},
		Characters: []agentapp.SceneRef{}, Pins: []agentapp.ScenePinEntry{}}
	raw, _ := json.Marshal(si)
	if _, err := e.pool.Exec(context.Background(),
		`update agent_session set scene = 'marketing', scene_input = $2 where id = $1`, sid, raw); err != nil {
		t.Fatal(err)
	}
	limit := 3
	v1 := domain.PlanItem{Kind: "video", ModelCode: "seedance", OperationCode: "ref2video", Label: "变体 1",
		Quantity: 10, Unit: "second", Key: "v1", ProductIndexes: []int{1, 2}, ProductMediaIDs: []string{"501", "502"},
		ProductTotal: 5, ImageLimit: &limit, Inputs: map[string]int{"image": 2}}
	v2 := domain.PlanItem{Kind: "audio", ModelCode: "tts", Label: "配音", Quantity: 1, Unit: "time", Key: "a1"}
	items, _ := plans.QuotePlan(context.Background(), []domain.PlanItem{v1, v2}, 0)
	total := items[0].Credits + items[1].Credits
	cid = e.suspendWith(sid, domain.CardPlanConfirm, domain.PlanConfirmPayload{Title: "营销", Items: items,
		Total: total, Notices: []string{"画面为人物对镜讲解，不保证口型与语音逐字同步"}})
	return sid, cid
}

func approveWith(ovs ...map[string]any) map[string]any {
	return map[string]any{"submission": map[string]any{"decision": "approve", "itemOverrides": ovs}}
}

func cardPayload(t *testing.T, e *env, sid, cid int64) domain.PlanConfirmPayload {
	t.Helper()
	var p domain.PlanConfirmPayload
	if err := json.Unmarshal(e.card(sid, cid).Payload, &p); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestApprove_换图后重报价(t *testing.T) {
	plans := &imagePlans{base: 40} // 按秒计价：换图不改价
	e := newEnvWith(t, true, func(_ *env, p *agentapp.ServicePorts) { p.Plans = plans })
	e.grant(1000)
	sid, cid := marketingPlanCard(t, e, plans)

	r := e.do("POST", path("/agent/sessions/%d/cards/%d/submit", sid, cid), e.owner, "",
		approveWith(map[string]any{"key": "v1", "productIndexes": []int{4, 1, 5}}))
	if r.status != http.StatusOK || r.body["state"] != "executing" {
		t.Fatalf("换图后价格不变应直接批准：%d %v", r.status, r.body)
	}
	p := cardPayload(t, e, sid, cid)
	it := p.Items[0]
	if e.card(sid, cid).Status != domain.CardSubmitted {
		t.Fatalf("卡片应已提交：%s", e.card(sid, cid).Status)
	}
	// 批准后的卡片 payload 即「已批准计划」：换后的图、按新张数重算的输入计数。
	if got := it.ProductMediaIDs; len(got) != 3 || got[0] != "504" || got[1] != "501" || got[2] != "505" {
		t.Fatalf("卡片上应是换后的图（按选择顺序）：%v", got)
	}
	if it.Inputs["image"] != 3 || len(it.ProductIndexes) != 3 || it.ProductTotal != 5 || *it.ImageLimit != 3 {
		t.Fatalf("条目应按换图重算：%+v", it)
	}
	// 其余键原样保留（notices、未换图的条目）。
	if len(p.Notices) != 1 || p.Items[1].Key != "a1" || p.Items[1].ProductMediaIDs != nil {
		t.Fatalf("其余内容不应被改动：%+v", p)
	}
	if s := e.session(sid); s.ApprovedBudget != 80 {
		t.Fatalf("额度 = 合计 80：%d", s.ApprovedBudget)
	}
}

func TestApprove_价格变化409(t *testing.T) {
	plans := &imagePlans{base: 30, perImage: 5} // 每张图 5：换成 3 张合计 +5
	e := newEnvWith(t, true, func(_ *env, p *agentapp.ServicePorts) { p.Plans = plans })
	e.grant(1000)
	sid, cid := marketingPlanCard(t, e, plans) // v1=30+10, a1=30 → 70
	if before := cardPayload(t, e, sid, cid).Total; before != 70 {
		t.Fatalf("初始合计应为 70：%d", before)
	}

	over := map[string]any{"key": "v1", "productIndexes": []int{3, 4, 5}}
	r := e.do("POST", path("/agent/sessions/%d/cards/%d/submit", sid, cid), e.owner, "", approveWith(over))
	if r.status != http.StatusConflict || r.code() != int(errcode.Conflict) {
		t.Fatalf("换图后价格变了应 409/40901：%d %v", r.status, r.body)
	}
	d := r.body["details"].(map[string]any)
	if d["reason"] != "price_changed" || d["total"].(float64) != 75 || d["previousTotal"].(float64) != 70 {
		t.Fatalf("details 应带新报价：%v", d)
	}
	// 不静默批准：卡片仍待处理、没有额度；新报价连同换后的图写回卡片。
	if c := e.card(sid, cid); c.Status != domain.CardPending || e.session(sid).ApprovedBudget != 0 {
		t.Fatalf("价格变化不应批准：%s %d", c.Status, e.session(sid).ApprovedBudget)
	}
	p := cardPayload(t, e, sid, cid)
	if p.Total != 75 || len(p.Items[0].ProductMediaIDs) != 3 || p.Items[0].ProductMediaIDs[0] != "503" {
		t.Fatalf("卡片应写回新报价与换后的图：%+v", p)
	}
	// 用户看过新价后再确认（前端可再带同样的覆盖，幂等）：按 75 批准。
	r = e.do("POST", path("/agent/sessions/%d/cards/%d/submit", sid, cid), e.owner, "", approveWith(over))
	if r.status != http.StatusOK || e.session(sid).ApprovedBudget != 75 {
		t.Fatalf("再次确认应按新报价批准：%d %v", r.status, r.body)
	}
}

func TestApprove_换图非法与营销阻塞(t *testing.T) {
	plans := &imagePlans{base: 40}
	e := newEnvWith(t, true, func(_ *env, p *agentapp.ServicePorts) { p.Plans = plans })
	e.grant(1000)
	sid, cid := marketingPlanCard(t, e, plans)
	for _, ov := range []map[string]any{
		{"key": "v1", "productIndexes": []int{6}},          // 越界
		{"key": "v1", "productIndexes": []int{1, 2, 3, 4}}, // 超上限
		{"key": "a1", "productIndexes": []int{1}},          // 不选图的条目
		{"key": "nope", "productIndexes": []int{1}},        // 键不存在
	} {
		r := e.do("POST", path("/agent/sessions/%d/cards/%d/submit", sid, cid), e.owner, "", approveWith(ov))
		if r.status != http.StatusBadRequest {
			t.Errorf("%v 应 400：%d %v", ov, r.status, r.body)
		}
	}
	if c := e.card(sid, cid); c.Status != domain.CardPending {
		t.Fatalf("非法换图不改变卡片：%s", c.Status)
	}

	// 因营销选项不可用而阻塞的条目：批准 422/42215，不能被重新报价悄悄解除。
	p := cardPayload(t, e, sid, cid)
	p.Items[0].Credits, p.Items[0].BlockedCode = 0, errcode.MarketingOptionUnavailable.Name()
	p.Items[0].BlockedReason, p.Items[0].UserFixable = "Hook「抓耳音效」需要能出声的视频模型", true
	p.Total = p.Items[1].Credits
	raw, _ := json.Marshal(p)
	if _, err := e.pool.Exec(context.Background(), `update agent_card set payload = $2 where id = $1`, cid, raw); err != nil {
		t.Fatal(err)
	}
	r := e.do("POST", path("/agent/sessions/%d/cards/%d/submit", sid, cid), e.owner, "", approve())
	if r.status != http.StatusUnprocessableEntity || r.code() != int(errcode.MarketingOptionUnavailable) {
		t.Fatalf("营销阻塞条目应 422/42215：%d %v", r.status, r.body)
	}
	if d := r.body["details"].(map[string]any); d["key"] != "v1" || d["userFixable"] != true {
		t.Fatalf("details 应点名条目且 userFixable：%v", d)
	}
	if e.session(sid).ApprovedBudget != 0 || e.card(sid, cid).Status != domain.CardPending {
		t.Fatal("不应批准")
	}
}
