//go:build integration

// 对外 MCP quote（MCPT-05）的真库测试。
//
// ⚠️ 要证明的不是「MCP 的报价函数算得对」，而是「MCP 与 Web 端 / Agent 用的是**同一条**报价路径」：
// 所以对照的另一边直接调 estimate 的 HTTP handler（catalog/transport）与 Agent 的 PriceItem，
// 而不是在测试里再写一份期望金额 —— 那样两边各改一处的话这里照样绿。
package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	agentapp "github.com/li904338203/tabtv/backend/internal/agent/app"
	agentdomain "github.com/li904338203/tabtv/backend/internal/agent/domain"
	catalogtransport "github.com/li904338203/tabtv/backend/internal/catalog/transport"
	collab "github.com/li904338203/tabtv/backend/internal/collaboration/domain"
	mcpapp "github.com/li904338203/tabtv/backend/internal/mcp/app"
	"github.com/li904338203/tabtv/backend/internal/mcp/domain"
	mcprepo "github.com/li904338203/tabtv/backend/internal/mcp/repo"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/wiring"
)

// seedPriced 建一个可用的模型 × 操作并配精确价（无会员门槛）。role 是能力矩阵里放行的输入角色。
func (e *env) seedPriced(model, op, domainName, unit string, credits int, roles ...string) {
	e.t.Helper()
	exec(e.t, e.pool, `insert into api_protocol (code, name, is_async) values ('openai_images','OpenAI 图片',false)
		on conflict (code) do nothing`)
	pid := e.next()
	exec(e.t, e.pool, `insert into provider_config (id, code, name, adapter_type, base_url, auth_type, status)
		values ($1,$2,$2,'http','http://127.0.0.1:1/v1','none','enabled')`, pid, "p-"+model)
	mid := e.next()
	exec(e.t, e.pool, `insert into model (id, code, name, provider_id, domain, upstream_model_code, api_protocol,
		submit_path, est_seconds, status, sort) values ($1,$2,$2,$3,$4,'up','openai_images','/x',20,'enabled',0)`,
		mid, model, pid, domainName)
	exec(e.t, e.pool, `insert into operation (id, code, name, domain, in_node_types, out_node_type, status)
		values ($1,$2,$2,$3,'text,image','image','enabled') on conflict (code) do nothing`, e.next(), op, domainName)
	exec(e.t, e.pool, `insert into model_operation (id, model_id, operation_id, is_enabled)
		select $1, $2, o.id, true from operation o where o.code = $3`, e.next(), mid, op)
	for _, role := range roles {
		exec(e.t, e.pool, `insert into model_input_capability (id, model_id, operation_id, input_role, min_count, max_count)
			select $1, $2, o.id, $3, 0, 4 from operation o where o.code = $4`, e.next(), mid, role, op)
	}
	exec(e.t, e.pool, `insert into pricing (id, operation_id, model_id, credits, unit, status)
		select $1, o.id, $2, $3, $4, 'enabled' from operation o where o.code = $5`, e.next(), mid, credits, unit, op)
}

// fallbackPrice 给不绑模型的操作配兜底价（render 用）。
func (e *env) fallbackPrice(op, unit string, credits int) {
	e.t.Helper()
	exec(e.t, e.pool, `insert into operation (id, code, name, domain, in_node_types, out_node_type, status)
		values ($1,$2,$2,'video','video','video','enabled') on conflict (code) do nothing`, e.next(), op)
	exec(e.t, e.pool, `insert into pricing (id, operation_id, model_id, credits, unit, status)
		select $1, o.id, null, $2, $3, 'enabled' from operation o where o.code = $4`, e.next(), credits, unit, op)
}

func (e *env) credit(n int) {
	exec(e.t, e.pool, `insert into credit_grant (id, payer_account_id, source, amount, remaining, status)
		values ($1,$2,'admin_adjust',$3,$3,'active')`, e.next(), e.payer, n)
}

// estimate 走 Web 端真实的 handler（与 /api/v1/generations/estimate 同一个函数），返回原始响应体。
func (e *env) estimate(body string) map[string]any {
	e.t.Helper()
	h := catalogtransport.NewHandler(e.catalog, func(*http.Request) int64 { return 0 })
	pass := func(next http.Handler) http.Handler { return next }
	r := chi.NewRouter()
	h.MountPricing(r, pass, pass, func(string) func(http.Handler) http.Handler { return pass })
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/generations/estimate", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		e.t.Fatalf("estimate %s → %d %s", body, rec.Code, rec.Body)
	}
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		e.t.Fatal(err)
	}
	return m
}

func TestMCPQuote_与estimate逐字一致(t *testing.T) {
	e := newEnv(t)
	e.seedPriced("img-m", "text2img", "image", "per_image", 7, "text")
	e.seedPriced("vid-m", "text2video", "video", "per_second", 9, "text")
	e.seedPriced("txt-m", "text2text", "text", "per_time", 3, "text")
	e.seedPriced("tts-m", "text2audio", "audio", "per_kchar", 4, "text")
	zh := strings.Repeat("中文码点", 400) // 1600 个码点 → ceil(1.6) = 2 千字；按字节算会是 5
	cases := []struct{ name, item string }{
		{"per_image", `{"modelCode":"img-m","operation":"text2img","count":3,"inputs":{"text":1}}`},
		{"per_second", `{"modelCode":"vid-m","operation":"text2video","durationSec":6,"inputs":{"text":1}}`},
		{"per_second 不给时长", `{"modelCode":"vid-m","operation":"text2video"}`},
		{"per_time", `{"modelCode":"txt-m","operation":"text2text","count":2}`},
		{"per_kchar 中文", `{"modelCode":"tts-m","operation":"text2audio","text":"` + zh + `"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			want := e.estimate(c.item)
			m := e.mustCall("quote", `{"item":`+c.item+`}`)
			got := m["items"].([]any)[0].(map[string]any)
			for _, k := range []string{"credits", "unit", "estimatedSeconds"} {
				if got[k] != want[k] {
					t.Errorf("%s：MCP %v ≠ estimate %v", k, got[k], want[k])
				}
			}
			if got["available"] != true || m["totalCredits"] != want["credits"] {
				t.Errorf("可用 / 合计：%v（estimate %v）", m, want)
			}
		})
	}
	// 中文码点那一条的绝对值也钉一下：两边都按字节算的话上面那条对照照样相等。
	m := e.mustCall("quote", `{"item":{"modelCode":"tts-m","operation":"text2audio","text":"`+zh+`"}}`)
	if m["totalCredits"].(float64) != 8 {
		t.Fatalf("per_kchar 应按码点计（2 × 4）：%v", m["totalCredits"])
	}
}

func TestMCPQuote_计划项与Agent规划同价(t *testing.T) {
	e := newEnv(t)
	e.seedPriced("img-m", "text2img", "image", "per_image", 7, "text")
	e.seedPriced("vid-m", "text2video", "video", "per_second", 9, "text")
	e.seedPriced("llm-m", "storyboard_gen", "text", "per_kchar", 5, "text")
	e.fallbackPrice("timeline_render", "per_second", 1)
	items := []agentdomain.PlanItem{
		{Kind: "image", ModelCode: "img-m", Quantity: 2, Unit: "image"},
		{Kind: "video", ModelCode: "vid-m", Quantity: 6, Unit: "second"},
		// storyboard × 3：Agent 按「单次 × 数量」报（per_kchar 不乘 count，直接当 count 传会报成一次的钱）。
		{Kind: "storyboard", ModelCode: "llm-m", Quantity: 3, Unit: "time"},
		// render：不绑模型，走 timeline_render 兜底价。
		{Kind: "render", Quantity: 30, Unit: "second"},
	}
	// MCP 的入参只有报价字段（label / credits 不收）。
	in := make([]map[string]any, len(items))
	for i, it := range items {
		in[i] = map[string]any{"kind": it.Kind, "modelCode": it.ModelCode, "quantity": it.Quantity, "unit": it.Unit}
	}
	b, _ := json.Marshal(in)
	m := e.mustCall("quote", `{"items":`+string(b)+`}`)
	got := m["items"].([]any)
	agent := wiring.AgentPlanQuoter{Catalog: e.catalog}
	total := 0
	for i, it := range items {
		pi, err := agent.PriceItem(context.Background(), it, e.p.MemberLevelID)
		if err != nil || !pi.Available {
			t.Fatalf("Agent 报价 %d：%v %+v", i, err, pi)
		}
		g := got[i].(map[string]any)
		if int(g["credits"].(float64)) != pi.Item.Credits || g["unit"] != pi.Unit {
			t.Errorf("第 %d 项（%s）：MCP %v/%v ≠ Agent %d/%s", i, it.Kind, g["credits"], g["unit"], pi.Item.Credits, pi.Unit)
		}
		total += pi.Item.Credits
	}
	if int(m["totalCredits"].(float64)) != total || total != 14+54+15+30 {
		t.Fatalf("合计 %v，Agent 合计 %d", m["totalCredits"], total)
	}
	if m["quoteId"] == nil {
		t.Fatal("全部可用应签发 quoteId")
	}
}

// Agent 的 submit_plan 靠 PricedItem.ArgFault 决定「退回给模型改」还是「照常发卡」。
// 真库验：细分原因确实从目录域的报价路径带出来了（只在单测里构造 BizError 的话，
// 目录域哪天把 Details 丢了这里照样绿，而 submit_plan 会退回到发一张不可批准的卡）。
func TestAgentPriceItem_参数错误分类(t *testing.T) {
	e := newEnv(t)
	e.seedPriced("vid-m", "text2video", "video", "per_second", 9, "text")
	e.seedPriced("off-m", "text2video", "video", "per_second", 9, "text")
	exec(t, e.pool, `update model set status = 'disabled' where code = 'off-m'`)
	levelID := e.next()
	exec(t, e.pool, `insert into member_level (id, code, name, sort, is_enabled) values ($1,'pro','专业版',20,true)`, levelID)
	e.seedPriced("vip-m", "text2video", "video", "per_second", 9, "text")
	exec(t, e.pool, `update model set min_member_level_id = $1 where code = 'vip-m'`, levelID)

	agent := wiring.AgentPlanQuoter{Catalog: e.catalog}
	video := func(model, op string, inputs map[string]int) agentdomain.PlanItem {
		return agentdomain.PlanItem{Kind: "video", ModelCode: model, OperationCode: op, Quantity: 15, Unit: "second",
			Inputs: inputs}
	}
	cases := []struct {
		name  string
		item  agentdomain.PlanItem
		fault string
		code  errcode.Code
	}{
		// 会话 105692702233853952 的原样：text2video 凭空写了一张图。
		{"纯文生写了图", video("vid-m", "text2video", map[string]int{"image": 1}), agentapp.PlanFaultInputs, errcode.CapabilityUnsupported},
		{"模型没绑这个操作", video("vid-m", "img2video", nil), agentapp.PlanFaultOperation, errcode.CapabilityUnsupported},
		{"未知模型", video("nope", "", nil), agentapp.PlanFaultOperation, errcode.CapabilityUnsupported},
		{"模型下线", video("off-m", "", nil), "", errcode.ModelUnavailable},
		{"会员等级不够", video("vip-m", "", nil), "", errcode.MemberLevelRequired},
	}
	for _, c := range cases {
		pi, err := agent.PriceItem(context.Background(), c.item, 0)
		if err != nil {
			t.Fatalf("%s：%v", c.name, err)
		}
		if pi.Available || pi.ArgFault != c.fault || pi.BlockedCode != c.code.Name() {
			t.Fatalf("%s：%+v", c.name, pi)
		}
	}
	pi, err := agent.PriceItem(context.Background(), video("vid-m", "", nil), 0)
	if err != nil || !pi.Available || pi.ArgFault != "" || pi.Operation != "text2video" || pi.Item.Credits != 135 {
		t.Fatalf("不带 inputs 的纯文生应可用（15 秒 × 9）：%+v %v", pi, err)
	}
}

func TestMCPQuote_余额与差额(t *testing.T) {
	e := newEnv(t)
	e.seedPriced("img-m", "text2img", "image", "per_image", 10, "text")
	e.credit(25)
	m := e.mustCall("quote", `{"item":{"modelCode":"img-m","operation":"text2img","count":3}}`)
	if m["totalCredits"].(float64) != 30 || m["balance"].(float64) != 25 || m["shortfall"].(float64) != 5 {
		t.Fatalf("合计 / 余额 / 差额：%v", m)
	}
	m = e.mustCall("quote", `{"item":{"modelCode":"img-m","operation":"text2img","count":2}}`)
	if m["shortfall"].(float64) != 0 {
		t.Fatalf("够用时差额为 0：%v", m)
	}

	// 有不可用项：其余照常报价、不签发、说明原因、不落库。
	levelID := e.next()
	exec(t, e.pool, `insert into member_level (id, code, name, sort, is_enabled) values ($1,'pro','专业版',20,true)`, levelID)
	e.seedPriced("vip-m", "text2video", "video", "per_second", 9, "text")
	exec(t, e.pool, `update model set min_member_level_id = $1 where code = 'vip-m'`, levelID)
	m = e.mustCall("quote", `{"items":[{"kind":"image","modelCode":"img-m","quantity":2},`+
		`{"kind":"video","modelCode":"vip-m","quantity":5,"unit":"second"}]}`)
	if _, ok := m["quoteId"]; ok || m["totalCredits"].(float64) != 20 {
		t.Fatalf("有不可用项不签发、其余照常：%v", m)
	}
	bad := m["items"].([]any)[1].(map[string]any)
	if bad["blockedCode"] != errcode.MemberLevelRequired.Name() || bad["userFixable"] != true ||
		!strings.Contains(m["notIssuedReason"].(string), "第 2 项") {
		t.Fatalf("不可用原因：%v", m)
	}
	var n int
	if err := e.pool.QueryRow(context.Background(), `select count(*) from mcp_quote`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("只有前两次签发了：%d %v", n, err)
	}
}

func TestMCPQuote_落库并带过期时间(t *testing.T) {
	e := newEnv(t)
	e.seedPriced("img-m", "text2img", "image", "per_image", 7, "text")
	before := time.Now()
	m := e.mustCall("quote", `{"item":{"modelCode":"img-m","operation":"text2img","count":2,"text":"一只猫"}}`)
	qid, ok := m["quoteId"].(string)
	if !ok || qid == "" || m["payerAccountId"] != itoa(e.payer) {
		t.Fatalf("quoteId / payerAccountId 应为字符串：%v", m)
	}
	store := mcprepo.New(e.pool)
	var id int64
	_ = json.Unmarshal([]byte(qid), &id)
	q, err := store.GetQuote(context.Background(), e.p.GrantID, id)
	if err != nil {
		t.Fatal(err)
	}
	if q.GrantID != e.p.GrantID || q.PayerAccountID != e.payer || q.Credits != 14 || q.Tool != domain.QuoteToolGenerate ||
		q.UsedAt != nil || q.ApprovedAt != nil {
		t.Fatalf("落库字段：%+v", q)
	}
	_, fp, _ := mcpapp.FingerprintFor(mcpapp.QuoteRequest{Item: &mcpapp.QuoteItem{ModelCode: "img-m",
		Operation: "text2img", Count: 2, Text: "一只猫"}})
	if !bytes.Equal(q.Fingerprint, fp) {
		t.Fatal("指纹应与 FingerprintFor 一致（M2 的 generate 要能复现）")
	}
	ttl := q.ExpiresAt.Sub(before)
	if ttl < 10*time.Minute-time.Second || ttl > 10*time.Minute+5*time.Second {
		t.Fatalf("过期时间应为签发时 + 10 分钟：%s", ttl)
	}
	if exp, _ := time.Parse(time.RFC3339, m["expiresAt"].(string)); !exp.Equal(q.ExpiresAt.Truncate(time.Second)) {
		t.Fatalf("对外 expiresAt %v 与库里 %v 不一致", m["expiresAt"], q.ExpiresAt)
	}
	if strings.Contains(string(q.Detail), "一只猫") || !strings.Contains(strings.ReplaceAll(string(q.Detail), " ", ""), `"textChars":3`) {
		t.Fatalf("detail 只记文本长度不记原文：%s", q.Detail)
	}
	// 别的授权取不到（不区分不存在与不是你的）。
	if _, err := store.GetQuote(context.Background(), e.p.GrantID+1, id); !errors.Is(err, mcprepo.ErrNotFound) {
		t.Fatalf("别的授权应取不到：%v", err)
	}
}

func TestMCPQuote_非成员拿不到报价(t *testing.T) {
	e := newEnv(t)
	e.seedPriced("img-m", "text2img", "image", "per_image", 7, "text")
	ctx := context.Background()
	mate := mkUser(t, e.pool, e.next, "同事", "13900000031")
	exec(t, e.pool, `insert into workspace_member (id, workspace_id, user_id, role) values ($1,$2,$3,'editor')`,
		e.next(), e.ws, mate)
	k, err := e.keys.CreateKey(ctx, mate, mcpapp.CreateKeyInput{WorkspaceID: e.ws, ClientName: "Cursor",
		Scopes: []string{domain.ScopeView}})
	if err != nil {
		t.Fatal(err)
	}
	p, err := e.resolver.Resolve(ctx, k.Plaintext)
	if err != nil || p.Role != collab.RoleEditor {
		t.Fatalf("成员应能解析：%v %+v", err, p)
	}
	if _, _, err := e.call(p, "quote", `{"item":{"modelCode":"img-m","operation":"text2img","count":1}}`); err != nil {
		t.Fatalf("成员应能报价：%v", err)
	}
	// 移出工作区：下一次调用在凭据解析处就被拒（Principal 每个请求实时解析，不缓存）。
	exec(t, e.pool, `update workspace_member set status = 'removed', removed_at = now()
		where workspace_id = $1 and user_id = $2`, e.ws, mate)
	if _, err := e.resolver.Resolve(ctx, k.Plaintext); bizCode(err) != errcode.NotWorkspaceMember {
		t.Fatalf("移出后应为 NOT_WORKSPACE_MEMBER：%v", err)
	}
	// 没有主体（凭据中间件没挂）时工具本身也拒绝，不按空工作区报价。
	tool, _ := e.reg.Lookup("quote")
	c, _ := tool.Decode(json.RawMessage(`{"item":{"modelCode":"img-m","operation":"text2img"}}`))
	if _, err := tool.Handler(ctx, c); bizCode(err) != errcode.Unauthenticated {
		t.Fatalf("无主体应拒绝：%v", err)
	}
}
