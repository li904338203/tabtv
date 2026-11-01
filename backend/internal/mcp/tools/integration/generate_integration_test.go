//go:build integration

// 对外 MCP generate（MCPT-10）的真库测试：确认服务 + 共享画布生成编排 + 真实受理与账本。
//
// ⚠️ 走真实的装配（wiring.BuildCore 的画布 / 生成服务、wiring.NewMCPToolDeps 的确认服务与端口适配器）：
// 「只扣一次」靠的是受理侧的 uk_generation_idem 与 mcp_quote 的 CAS，用假端口的测试把这两条整条删掉照样绿。
// 只受理、不跑 Worker：任务停在 queued，**不触发任何上游调用**。
package integration

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	billingapp "github.com/li904338203/tabtv/backend/internal/billing/app"
	billingrepo "github.com/li904338203/tabtv/backend/internal/billing/repo"
	collabapp "github.com/li904338203/tabtv/backend/internal/collaboration/app"
	collabrepo "github.com/li904338203/tabtv/backend/internal/collaboration/repo"
	contentapp "github.com/li904338203/tabtv/backend/internal/content/app"
	contentrepo "github.com/li904338203/tabtv/backend/internal/content/repo"
	identityapp "github.com/li904338203/tabtv/backend/internal/identity/app"
	identityrepo "github.com/li904338203/tabtv/backend/internal/identity/repo"
	mcpapp "github.com/li904338203/tabtv/backend/internal/mcp/app"
	"github.com/li904338203/tabtv/backend/internal/mcp/domain"
	mcprepo "github.com/li904338203/tabtv/backend/internal/mcp/repo"
	"github.com/li904338203/tabtv/backend/internal/mcp/tools"
	"github.com/li904338203/tabtv/backend/internal/platform/audit"
	"github.com/li904338203/tabtv/backend/internal/platform/bootstrap"
	"github.com/li904338203/tabtv/backend/internal/platform/config"
	"github.com/li904338203/tabtv/backend/internal/platform/db"
	timelineapp "github.com/li904338203/tabtv/backend/internal/timeline/app"
	timelinerepo "github.com/li904338203/tabtv/backend/internal/timeline/repo"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
	"github.com/li904338203/tabtv/backend/internal/wiring"
	workflowapp "github.com/li904338203/tabtv/backend/internal/workflow/app"
	workflowrepo "github.com/li904338203/tabtv/backend/internal/workflow/repo"
)

// newGenEnv 在只读环境上换成真实的画布 / 生成服务（BuildCore），注册表接上 generate 的端口。
// 主体带 view + generate scope（generate 要 generate 动作）。
func newGenEnv(t *testing.T) *env {
	t.Helper()
	e := newEnv(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	core, err := wiring.BuildCore(wiring.CoreInput{Cfg: config.Config{},
		Deps: &bootstrap.Deps{DB: &db.Pool{Pool: e.pool}}, NextID: e.next, Logger: logger,
		Audit: audit.New(e.pool, e.next, logger), Media: e.media, Catalog: e.catalog})
	if err != nil {
		t.Fatal(err)
	}
	cr := collabrepo.New(e.pool)
	wf := workflowapp.New()
	wf.SetRuns(workflowrepo.New(e.pool))
	deps := wiring.NewMCPToolDeps(wiring.MCPReadInput{
		Identity: identityapp.New(identityrepo.New(e.pool), nil, nil, nil, e.next), Collab: collabapp.New(cr, e.next),
		Billing: billingapp.New(billingrepo.New(e.pool, e.next)), Canvas: core.Canvas, GenRepo: core.GenRepo,
		Gen: core.Gen, Catalog: e.catalog, Content: contentapp.New(contentrepo.New(e.pool, e.next)), Workflow: wf,
		Timeline: timelineapp.New(timelinerepo.New(e.pool, e.next)), Media: e.media, MediaURLTTL: time.Hour,
		Store: mcprepo.New(e.pool), NextID: e.next, QuoteTTL: 10 * time.Minute,
		Ledger: core.Billing, StateKey: mcpapp.DeriveStateKey("integration"),
	})
	if deps.CanvasGen == nil || deps.Confirm == nil {
		t.Fatal("generate 端口没有接上")
	}
	e.reg = tools.NewRegistry(deps)
	k, err := e.keys.CreateKey(context.Background(), e.owner, mcpapp.CreateKeyInput{WorkspaceID: e.ws,
		ClientName: "Claude Code", Scopes: []string{domain.ScopeView, domain.ScopeGenerate}})
	if err != nil {
		t.Fatal(err)
	}
	if e.p, err = e.resolver.Resolve(context.Background(), k.Plaintext); err != nil {
		t.Fatal(err)
	}
	return e
}

const genCanvas = `{"schemaVersion":1,"viewport":{"x":0,"y":0,"zoom":1},"nodes":[` +
	`{"id":"n_user1","type":"image","title":"用户的图","position":{"x":0,"y":0},"size":{"w":330,"h":180},"params":{}}` +
	`],"edges":[]}`

func (e *env) count(sql string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

// genCall 调 generate，返回结果或业务错误（不 Fatal）。
func (e *env) genCall(args string) (map[string]any, error) {
	e.t.Helper()
	m, _, err := e.call(e.p, "generate", args)
	return m, err
}

func genArgsFor(pid int64, extra string) string {
	return `{"projectId":"` + itoa(pid) + `","nodeType":"image","operationCode":"text2img","modelCode":"img-m",` +
		`"prompt":"一只猫","count":2` + extra + `}`
}

func confirmArgs(nc map[string]any) string {
	return `,"confirmQuoteId":"` + nc["quoteId"].(string) + `","confirmCredits":` +
		strconv.Itoa(int(nc["credits"].(float64)))
}

// begin 第一次调用（无确认）：必须停在 needs_confirmation，返回 needsConfirmation。
func (e *env) begin(args string) map[string]any {
	e.t.Helper()
	m, err := e.genCall(args)
	if err != nil {
		e.t.Fatalf("首次调用：%v", err)
	}
	nc, _ := m["needsConfirmation"].(map[string]any)
	if m["status"] != "needs_confirmation" || nc == nil {
		e.t.Fatalf("应需确认：%v", m)
	}
	return nc
}

func (e *env) consumes(payer int64) int {
	return e.count(`select count(*) from credit_transaction where payer_account_id=$1 and type='consume'`, payer)
}

func TestGenerate_无确认不扣费(t *testing.T) {
	e := newGenEnv(t)
	e.seedPriced("img-m", "text2img", "image", "per_image", 7, "text")
	e.credit(100)
	pid := e.project(e.ws, "片子", genCanvas, time.Now())
	nc := e.begin(genArgsFor(pid, ""))
	if nc["credits"].(float64) != 14 || nc["balance"].(float64) != 100 || nc["after"].(float64) != 86 {
		t.Fatalf("报价：%v", nc)
	}
	if n := e.count(`select count(*) from generation`); n != 0 {
		t.Fatalf("不应受理：%d", n)
	}
	if n := e.count(`select count(*) from credit_transaction`); n != 0 {
		t.Fatalf("不应有任何流水：%d", n)
	}
	var nodes int
	if err := e.pool.QueryRow(context.Background(),
		`select jsonb_array_length(canvas_data->'nodes') from project where id=$1`, pid).Scan(&nodes); err != nil ||
		nodes != 1 {
		t.Fatalf("确认之前不建节点：%d %v", nodes, err)
	}
	if n := e.count(`select count(*) from mcp_quote where grant_id=$1 and used_at is null and credits=14`,
		e.p.GrantID); n != 1 {
		t.Fatalf("应签发一张未用的 quote：%d", n)
	}
	// 金额不符 / 不认识的 quote → MCP_QUOTE_INVALID，仍不扣费。
	for _, bad := range []string{`,"confirmQuoteId":"` + nc["quoteId"].(string) + `","confirmCredits":13`,
		`,"confirmQuoteId":"123","confirmCredits":14`} {
		_, err := e.genCall(genArgsFor(pid, bad))
		if bizCode(err) != errcode.McpQuoteInvalid {
			t.Fatalf("%s 应无效：%v", bad, err)
		}
	}
	if n := e.count(`select count(*) from generation`); n != 0 {
		t.Fatal("无效确认不应受理")
	}
}

func TestGenerate_同一确认重试3次一个任务一笔扣费(t *testing.T) {
	e := newGenEnv(t)
	e.seedPriced("img-m", "text2img", "image", "per_image", 7, "text")
	e.credit(100)
	pid := e.project(e.ws, "片子", genCanvas, time.Now())

	// 顺序重试 3 次。
	nc := e.begin(genArgsFor(pid, ""))
	var genID, nodeID string
	for i := 0; i < 3; i++ {
		m, err := e.genCall(genArgsFor(pid, confirmArgs(nc)))
		if err != nil {
			t.Fatalf("第 %d 次：%v", i+1, err)
		}
		if i == 0 {
			genID, nodeID = m["generationId"].(string), m["nodeId"].(string)
		}
		if m["generationId"] != genID || m["nodeId"] != nodeID || (m["replayed"] == true) != (i > 0) {
			t.Fatalf("第 %d 次：%v", i+1, m)
		}
	}
	if n := e.count(`select count(*) from generation where payer_account_id=$1`, e.payer); n != 1 {
		t.Fatalf("应恰好 1 个任务：%d", n)
	}
	if n := e.consumes(e.payer); n != 1 {
		t.Fatalf("应恰好 1 笔扣费：%d", n)
	}
	var key string
	_ = e.pool.QueryRow(context.Background(), `select idempotency_key from generation where id=$1`, genID).Scan(&key)
	if key != "mcp:"+itoa(e.p.GrantID)+":"+nc["quoteId"].(string) {
		t.Fatalf("幂等键：%s", key)
	}
	// 受理后节点：result 挂上任务、参数是画布编码、来源是 mcp。
	var node string
	if err := e.pool.QueryRow(context.Background(), `select n::text from project p,
		jsonb_array_elements(p.canvas_data->'nodes') n where p.id=$1 and n->>'id'=$2`, pid, nodeID).Scan(&node); err != nil {
		t.Fatal(err)
	}
	var n map[string]any
	_ = json.Unmarshal([]byte(node), &n)
	res, _ := n["result"].(map[string]any)
	origin, _ := n["origin"].(map[string]any)
	params, _ := n["params"].(map[string]any)
	if res == nil || res["id"] != genID {
		t.Errorf("节点 result.id 应为任务 ID：%v", n["result"])
	}
	if origin == nil || origin["kind"] != "mcp" || origin["grantId"] != itoa(e.p.GrantID) ||
		origin["clientName"] != "Claude Code" {
		t.Errorf("来源：%v", n["origin"])
	}
	if params["model"] != "img-m" || params["prompt"] != "一只猫" || params["count"] != "2" {
		t.Errorf("参数应为画布编码：%v", params)
	}
	if e.count(`select count(*) from mcp_quote where id=$1 and used_at is not null`, mustInt(t, nc["quoteId"])) != 1 {
		t.Error("quote 应已用")
	}

	// 并发重试 3 次（另一张确认）。
	nc2 := e.begin(genArgsFor(pid, `,"title":"第二张"`))
	var wg sync.WaitGroup
	ids := make([]string, 3)
	errs := make([]error, 3)
	for i := range ids {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			m, err := e.genCall(genArgsFor(pid, `,"title":"第二张"`+confirmArgs(nc2)))
			errs[i] = err
			if err == nil {
				ids[i], _ = m["generationId"].(string)
			}
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("并发第 %d 个：%v", i+1, err)
		}
		if ids[i] != ids[0] || ids[i] == genID {
			t.Fatalf("并发应拿到同一个新任务：%v", ids)
		}
	}
	if n := e.count(`select count(*) from generation where payer_account_id=$1`, e.payer); n != 2 {
		t.Fatalf("两次确认应恰好 2 个任务：%d", n)
	}
	if n := e.consumes(e.payer); n != 2 {
		t.Fatalf("两次确认应恰好 2 笔扣费：%d", n)
	}
}

func TestGenerate_价格变化返回新报价不扣费(t *testing.T) {
	e := newGenEnv(t)
	e.seedPriced("img-m", "text2img", "image", "per_image", 7, "text")
	e.credit(100)
	pid := e.project(e.ws, "片子", genCanvas, time.Now())
	nc := e.begin(genArgsFor(pid, ""))
	exec(t, e.pool, `update pricing set credits = 9`)
	_, err := e.genCall(genArgsFor(pid, confirmArgs(nc)))
	var be *httpx.BizError
	if !asBiz(err, &be) || be.Code != errcode.McpQuotePriceChanged {
		t.Fatalf("应返回价格变化：%v", err)
	}
	nq, _ := be.Details["quote"].(map[string]any)
	if nq == nil || nq["credits"] != 18 || be.Details["oldCredits"] != 14 {
		t.Fatalf("details 要带新报价：%v", be.Details)
	}
	if n := e.count(`select count(*) from credit_transaction`); n != 0 {
		t.Fatalf("不按新价扣费：%d", n)
	}
	if e.count(`select count(*) from mcp_quote where id=$1 and used_at is not null and detail->>'voided'='price_changed'`,
		mustInt(t, nc["quoteId"])) != 1 {
		t.Fatal("旧 quote 应作废（审计分得出作废与用过）")
	}
	// 旧 quote 再提交：无效（不是重放）；新 quote 照常受理，按新价扣一次。
	if _, err := e.genCall(genArgsFor(pid, confirmArgs(nc))); bizCode(err) != errcode.McpQuoteInvalid {
		t.Fatalf("作废的 quote 应无效：%v", err)
	}
	m, err := e.genCall(genArgsFor(pid, `,"confirmQuoteId":"`+nq["quoteId"].(string)+`","confirmCredits":18`))
	if err != nil || m["credits"].(float64) != 18 {
		t.Fatalf("新报价应可受理：%v %v", m, err)
	}
	if n := e.consumes(e.payer); n != 1 {
		t.Fatalf("恰好一笔扣费：%d", n)
	}
}

func TestGenerate_余额不足不签发quote(t *testing.T) {
	e := newGenEnv(t)
	e.seedPriced("img-m", "text2img", "image", "per_image", 7, "text")
	e.credit(5)
	pid := e.project(e.ws, "片子", genCanvas, time.Now())
	_, err := e.genCall(genArgsFor(pid, ""))
	var be *httpx.BizError
	if !asBiz(err, &be) || be.Code != errcode.InsufficientCredits || be.Details["shortfall"] != 9 ||
		!strings.Contains(be.Error(), "还差 9") {
		t.Fatalf("应直接拒绝并给差额：%v", err)
	}
	if n := e.count(`select count(*) from mcp_quote`); n != 0 {
		t.Fatalf("余额不足不签发 quote：%d", n)
	}
}

func TestGenerate_改用户节点返回NodeNotOwned(t *testing.T) {
	e := newGenEnv(t)
	e.seedPriced("img-m", "text2img", "image", "per_image", 7, "text")
	e.credit(100)
	pid := e.project(e.ws, "片子", genCanvas, time.Now())
	_, err := e.genCall(`{"projectId":"` + itoa(pid) + `","nodeId":"n_user1","operationCode":"text2img",` +
		`"modelCode":"img-m","prompt":"改成狗","count":1}`)
	if bizCode(err) != errcode.McpNodeNotOwned {
		t.Fatalf("改用户的节点应返回 MCP_NODE_NOT_OWNED：%v", err)
	}
	if n := e.count(`select count(*) from mcp_quote`); n != 0 {
		t.Fatalf("报价之前就该拦下：%d", n)
	}
	// 自己建的节点可以复用：先建一个（受理一次），再在它上面改参数生成。
	nc := e.begin(genArgsFor(pid, ""))
	m, err := e.genCall(genArgsFor(pid, confirmArgs(nc)))
	if err != nil {
		t.Fatal(err)
	}
	own := m["nodeId"].(string)
	args := `{"projectId":"` + itoa(pid) + `","nodeId":"` + own + `","operationCode":"text2img","modelCode":"img-m",` +
		`"prompt":"再来一张","count":1`
	nc2 := e.begin(args + `}`)
	m2, err := e.genCall(args + confirmArgs(nc2) + `}`)
	if err != nil || m2["nodeId"] != own {
		t.Fatalf("本授权的节点应可复用：%v %v", m2, err)
	}
	// 用户改过之后（userTouched）就不归 MCP 了。
	exec(t, e.pool, `update project set canvas_data = jsonb_set(canvas_data, '{nodes}',
		(select jsonb_agg(case when n->>'id'=$2 then jsonb_set(n, '{origin,userTouched}', 'true') else n end)
		 from jsonb_array_elements(canvas_data->'nodes') n)) where id=$1`, pid, own)
	if _, err := e.genCall(args + `}`); bizCode(err) != errcode.McpNodeNotOwned {
		t.Fatalf("用户改过的节点应拒绝：%v", err)
	}
}

func TestGenerate_团队消费记在团队账户(t *testing.T) {
	e := newGenEnv(t)
	e.seedPriced("img-m", "text2img", "image", "per_image", 7, "text")
	e.credit(100)
	var personal int64
	if err := e.pool.QueryRow(context.Background(), `select payer_account_id from workspace
		where kind='personal' and owner_user_id=$1`, e.owner).Scan(&personal); err != nil {
		t.Fatal(err)
	}
	exec(t, e.pool, `insert into credit_grant (id, payer_account_id, source, amount, remaining, status)
		values ($1,$2,'admin_adjust',100,100,'active')`, e.next(), personal)
	pid := e.project(e.ws, "团队片子", genCanvas, time.Now())
	nc := e.begin(genArgsFor(pid, ""))
	if nc["payerAccountId"] != itoa(e.payer) {
		t.Fatalf("报价的付费账户应是团队：%v", nc)
	}
	m, err := e.genCall(genArgsFor(pid, confirmArgs(nc)))
	if err != nil {
		t.Fatal(err)
	}
	if n := e.count(`select count(*) from credit_transaction where payer_account_id=$1`, personal); n != 0 {
		t.Errorf("个人账户一条流水都不该有：%d", n)
	}
	if n := e.count(`select count(*) from credit_transaction where type='consume' and payer_account_id=$1
		and ref_id=$2`, e.payer, m["generationId"]); n != 1 {
		t.Errorf("团队账户应恰好 1 笔消费：%d", n)
	}
	if n := e.count(`select count(*) from generation where payer_account_id=$1 and created_by_user_id=$2
		and workspace_id=$3`, e.payer, e.owner, e.ws); n != 1 {
		t.Errorf("任务归属三要素（工作区 + 操作者 + 费用账户）：%d", n)
	}
}

// quote 工具签发的单项 quoteId 可直接当 generate 的 confirmQuoteId（MCPT-10 唯一规范化映射）：
// quote 走 estimate 那条报价、generate 走受理那条，两边落到同一个指纹、同一个价。
func TestQuoteTool_签发的quote可用于generate(t *testing.T) {
	e := newGenEnv(t)
	e.seedPriced("img-m", "text2img", "image", "per_image", 7, "text")
	e.credit(100)
	pid := e.project(e.ws, "片子", genCanvas, time.Now())
	cases := []struct{ name, item, extra string }{
		{"纯文生", `{"modelCode":"img-m","operation":"text2img","count":2,"text":"一只猫"}`, ``},
		// 声明了上游文本输入：generate 的角色数由服务端解析得出（text:1），与 quote 里报的一致。
		{"带文本输入", `{"modelCode":"img-m","operation":"text2img","count":2,"text":"一只猫","inputs":{"text":1,"image":0}}`,
			`,"inputs":[{"kind":"text","order":1,"text":"上游的文字"}]`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			q := e.mustCall("quote", `{"item":`+c.item+`}`)
			qid, _ := q["quoteId"].(string)
			if qid == "" || q["totalCredits"].(float64) != 14 {
				t.Fatalf("quote：%v", q)
			}
			before := e.consumes(e.payer)
			m, err := e.genCall(genArgsFor(pid, c.extra+`,"confirmQuoteId":"`+qid+`","confirmCredits":14`))
			if err != nil {
				t.Fatalf("quote 签发的 quoteId 应可直接确认：%v", err)
			}
			if m["status"] != "queued" || m["replayed"] == true || e.consumes(e.payer) != before+1 {
				t.Fatalf("应受理一次：%v", m)
			}
		})
	}
	// 改了参数（多要一张）就对不上。
	q := e.mustCall("quote", `{"item":{"modelCode":"img-m","operation":"text2img","count":2,"text":"一只猫"}}`)
	_, err := e.genCall(strings.Replace(genArgsFor(pid, `,"confirmQuoteId":"`+q["quoteId"].(string)+`","confirmCredits":14`),
		`"count":2`, `"count":3`, 1))
	var be *httpx.BizError
	if !asBiz(err, &be) || be.Code != errcode.McpQuoteInvalid || be.Details["reason"] != "fingerprint_mismatch" {
		t.Fatalf("参数不同应指纹不符：%v", err)
	}
}

func mustInt(t *testing.T, v any) int64 {
	t.Helper()
	n, err := strconv.ParseInt(v.(string), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	return n
}
