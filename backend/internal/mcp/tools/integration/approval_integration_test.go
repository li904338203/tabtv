//go:build integration

// 页面审批（MCPT-13，MCP-F-44，MCP-AC-15）的真库测试：确认服务 + 审批服务 + 真实受理与账本。
//
// ⚠️ 「批准之后恰好扣一次」靠的是 mcp_quote 的 CAS（approved_at / rejected_at / used_at）与受理侧的
// uk_generation_idem —— 用假仓储的测试把这些条件整条删掉照样绿，所以这里走真库。
// 只受理、不跑 Worker：任务停在 queued，不触发任何上游调用。
package integration

import (
	"context"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	billingapp "github.com/li904338203/tabtv/backend/internal/billing/app"
	billingrepo "github.com/li904338203/tabtv/backend/internal/billing/repo"
	collabapp "github.com/li904338203/tabtv/backend/internal/collaboration/app"
	collabrepo "github.com/li904338203/tabtv/backend/internal/collaboration/repo"
	mcpapp "github.com/li904338203/tabtv/backend/internal/mcp/app"
	mcprepo "github.com/li904338203/tabtv/backend/internal/mcp/repo"
	"github.com/li904338203/tabtv/backend/internal/platform/audit"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
	"github.com/li904338203/tabtv/backend/internal/wiring"
)

// newApprovalEnv：generate 环境 + 本授权打开「花钱需在页面确认」+ 真实的审批服务。
func newApprovalEnv(t *testing.T) (*env, *mcpapp.Approvals) {
	t.Helper()
	e := newGenEnv(t)
	if _, err := e.keys.SetRequirePageConfirm(context.Background(), e.owner, e.p.GrantID, true); err != nil {
		t.Fatal(err)
	}
	// Resolver 每次调用实时取授权（不缓存）：这里直接改主体上的字段，等价于下一次调用重新解析。
	e.p.RequirePageConfirm = true
	ap := mcpapp.NewApprovals(mcprepo.New(e.pool),
		wiring.MCPApprovalWorkspaces{Svc: collabapp.New(collabrepo.New(e.pool), e.next)},
		wiring.MCPPayerBalances{Svc: billingapp.New(billingrepo.New(e.pool, e.next))}, audit.New(e.pool, e.next, slog.New(slog.NewTextHandler(io.Discard, nil))))
	return e, ap
}

func (e *env) userByPhone(phone string) int64 {
	e.t.Helper()
	var id int64
	if err := e.pool.QueryRow(context.Background(), `select id from app_user where phone=$1`, phone).Scan(&id); err != nil {
		e.t.Fatal(err)
	}
	return id
}

func quoteIDOf(t *testing.T, nc map[string]any) int64 {
	t.Helper()
	id, err := strconv.ParseInt(nc["quoteId"].(string), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func bizOf(err error) *httpx.BizError {
	var be *httpx.BizError
	if asBiz(err, &be) {
		return be
	}
	return nil
}

func TestConfirm_PageMode_EchoIgnored(t *testing.T) {
	e, _ := newApprovalEnv(t)
	e.seedPriced("img-m", "text2img", "image", "per_image", 7, "text")
	e.credit(100)
	pid := e.project(e.ws, "片子", genCanvas, time.Now())
	nc := e.begin(genArgsFor(pid, ""))
	url, _ := nc["approvalUrl"].(string)
	if !strings.HasSuffix(url, "/mcp/approve/"+nc["quoteId"].(string)) {
		t.Fatalf("应给审批链接：%v", nc)
	}
	// 客户端带正确的 confirmQuoteId + confirmCredits 也不受理（MCP-AC-15）。
	for i := 0; i < 2; i++ {
		_, err := e.genCall(genArgsFor(pid, confirmArgs(nc)))
		be := bizOf(err)
		if be == nil || be.Code != errcode.McpApprovalPending || be.Details["approvalUrl"] != url {
			t.Fatalf("未批准应等待页面确认：%v", err)
		}
	}
	if n := e.count(`select count(*) from generation`); n != 0 {
		t.Fatalf("未批准不应受理：%d", n)
	}
	if n := e.count(`select count(*) from credit_transaction`); n != 0 {
		t.Fatalf("未批准不应有流水：%d", n)
	}
	if n := e.count(`select count(*) from mcp_quote where id=$1 and used_at is null`, quoteIDOf(t, nc)); n != 1 {
		t.Fatal("未批准不得占用 quote")
	}
}

func TestConfirm_PageMode_ApprovedThenChargedOnce(t *testing.T) {
	e, ap := newApprovalEnv(t)
	e.seedPriced("img-m", "text2img", "image", "per_image", 7, "text")
	e.credit(100)
	pid := e.project(e.ws, "片子", genCanvas, time.Now())
	nc := e.begin(genArgsFor(pid, ""))
	v, err := ap.Approve(context.Background(), e.owner, quoteIDOf(t, nc))
	if err != nil || v.State != mcpapp.ApprovalApproved || v.Credits != 14 || v.WorkspaceName != "制作组" ||
		v.WorkspaceKind != "team" || v.Balance != 100 || v.After != 86 || v.ClientName != "Claude Code" {
		t.Fatalf("批准：%+v %v", v, err)
	}
	// 批准之后：顺序 2 次 + 并发 3 次，恰好一个任务、一笔扣费。
	var genID string
	for i := 0; i < 2; i++ {
		m, err := e.genCall(genArgsFor(pid, confirmArgs(nc)))
		if err != nil {
			t.Fatalf("第 %d 次：%v", i+1, err)
		}
		if i == 0 {
			genID = m["generationId"].(string)
		}
		if m["generationId"] != genID || (m["replayed"] == true) != (i > 0) {
			t.Fatalf("第 %d 次：%v", i+1, m)
		}
	}
	var wg sync.WaitGroup
	var bad atomic.Int32
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m, err := e.genCall(genArgsFor(pid, confirmArgs(nc)))
			if err != nil || m["generationId"] != genID {
				bad.Add(1)
			}
		}()
	}
	wg.Wait()
	if bad.Load() != 0 {
		t.Fatal("并发重放应都拿回原任务")
	}
	if n := e.count(`select count(*) from generation where payer_account_id=$1`, e.payer); n != 1 {
		t.Fatalf("应恰好 1 个任务：%d", n)
	}
	if n := e.consumes(e.payer); n != 1 {
		t.Fatalf("应恰好 1 笔扣费：%d", n)
	}
	// 审批页此后显示「已执行」。
	if v, _ := ap.Get(context.Background(), e.owner, quoteIDOf(t, nc)); v.State != mcpapp.ApprovalUsed {
		t.Fatalf("执行后应是 used：%s", v.State)
	}
	if n := e.count(`select count(*) from audit_log where action=$1`, mcpapp.AuditQuoteApprove); n != 1 {
		t.Fatalf("批准应写一条审计：%d", n)
	}
}

func TestConfirm_PageMode_Rejected_NoCharge(t *testing.T) {
	e, ap := newApprovalEnv(t)
	e.seedPriced("img-m", "text2img", "image", "per_image", 7, "text")
	e.credit(100)
	pid := e.project(e.ws, "片子", genCanvas, time.Now())
	nc := e.begin(genArgsFor(pid, ""))
	if v, err := ap.Reject(context.Background(), e.owner, quoteIDOf(t, nc)); err != nil || v.State != mcpapp.ApprovalRejected {
		t.Fatalf("拒绝：%+v %v", v, err)
	}
	_, err := e.genCall(genArgsFor(pid, confirmArgs(nc)))
	if be := bizOf(err); be == nil || be.Code != errcode.McpQuoteInvalid || be.Details["reason"] != "rejected" {
		t.Fatalf("已拒绝应无效：%v", err)
	}
	// 拒绝之后用户关掉了页面确认：同一个 quoteId 回显金额也不能复活它。
	if _, err := e.keys.SetRequirePageConfirm(context.Background(), e.owner, e.p.GrantID, false); err != nil {
		t.Fatal(err)
	}
	e.p.RequirePageConfirm = false
	if _, err := e.genCall(genArgsFor(pid, confirmArgs(nc))); bizCode(err) != errcode.McpQuoteInvalid {
		t.Fatalf("关掉开关也不能复活已拒绝的 quote：%v", err)
	}
	if n := e.count(`select count(*) from generation`) + e.count(`select count(*) from credit_transaction`); n != 0 {
		t.Fatalf("拒绝后不应受理或扣费：%d", n)
	}
}

func TestApproval_NotOwner_403NoDetail(t *testing.T) {
	e, ap := newApprovalEnv(t)
	e.seedPriced("img-m", "text2img", "image", "per_image", 7, "text")
	e.credit(100)
	pid := e.project(e.ws, "片子", genCanvas, time.Now())
	nc := e.begin(genArgsFor(pid, ""))
	qid := quoteIDOf(t, nc)
	other := e.userByPhone("13900000022")
	ctx := context.Background()
	for name, fn := range map[string]func(context.Context, int64, int64) (mcpapp.ApprovalView, error){
		"get": ap.Get, "approve": ap.Approve, "reject": ap.Reject} {
		v, err := fn(ctx, other, qid)
		be := bizOf(err)
		if be == nil || be.Code != errcode.McpApprovalNotOwner || len(be.Details) != 0 || v.QuoteID != 0 {
			t.Fatalf("%s：非本人应 403 且不带内容：%+v %v", name, v, err)
		}
		// 错误文案里不能出现客户端名与金额。
		if strings.Contains(be.Message, "Claude") || strings.Contains(be.Message, "14") {
			t.Fatalf("%s：文案泄露了报价内容：%s", name, be.Message)
		}
	}
	if n := e.count(`select count(*) from mcp_quote where id=$1 and approved_at is null and rejected_at is null`,
		qid); n != 1 {
		t.Fatal("非本人不得改动审批状态")
	}
	// 不存在 → 404（与非本人是两个码：审批页要告诉本人「换账号」，而不存在的就是不存在）。
	if _, err := ap.Get(ctx, e.owner, qid+1); bizCode(err) != errcode.NotFound {
		t.Fatalf("不存在应 404：%v", err)
	}
}

func TestApproval_Expired(t *testing.T) {
	e, ap := newApprovalEnv(t)
	e.seedPriced("img-m", "text2img", "image", "per_image", 7, "text")
	e.credit(100)
	pid := e.project(e.ws, "片子", genCanvas, time.Now())
	nc := e.begin(genArgsFor(pid, ""))
	qid := quoteIDOf(t, nc)
	ap.SetClock(func() time.Time { return time.Now().Add(11 * time.Minute) })
	v, err := ap.Get(context.Background(), e.owner, qid)
	if err != nil || v.State != mcpapp.ApprovalExpired {
		t.Fatalf("应已过期：%+v %v", v, err)
	}
	// 过期后批准不报错、不写入，回当前真实状态（CAS 条件里的 expires_at）。
	if v, err := ap.Approve(context.Background(), e.owner, qid); err != nil || v.State != mcpapp.ApprovalExpired {
		t.Fatalf("过期批准：%+v %v", v, err)
	}
	if ok, err := mcprepo.New(e.pool).ApproveQuote(context.Background(), e.owner, qid,
		time.Now().Add(11*time.Minute)); err != nil || ok {
		t.Fatalf("CAS 不得批准过期的报价：%v %v", ok, err)
	}
}

func TestApproval_ApproveTwice_SecondNoop(t *testing.T) {
	e, ap := newApprovalEnv(t)
	e.seedPriced("img-m", "text2img", "image", "per_image", 7, "text")
	e.credit(100)
	pid := e.project(e.ws, "片子", genCanvas, time.Now())
	nc := e.begin(genArgsFor(pid, ""))
	qid := quoteIDOf(t, nc)
	ctx := context.Background()
	v1, err := ap.Approve(ctx, e.owner, qid)
	if err != nil || v1.State != mcpapp.ApprovalApproved || v1.ApprovedAt == nil {
		t.Fatalf("第一次：%+v %v", v1, err)
	}
	v2, err := ap.Approve(ctx, e.owner, qid)
	if err != nil || v2.State != mcpapp.ApprovalApproved || !v2.ApprovedAt.Equal(*v1.ApprovedAt) {
		t.Fatalf("第二次应是 no-op、回同一个批准时间：%+v %v", v2, err)
	}
	if n := e.count(`select count(*) from audit_log where action=$1`, mcpapp.AuditQuoteApprove); n != 1 {
		t.Fatalf("重复批准只写一条审计：%d", n)
	}
	// 并发批准与拒绝：只有一个生效，且结局互斥。
	nc2 := e.begin(genArgsFor(pid, `,"title":"第二张"`))
	q2 := quoteIDOf(t, nc2)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				_, _ = ap.Approve(ctx, e.owner, q2)
			} else {
				_, _ = ap.Reject(ctx, e.owner, q2)
			}
		}(i)
	}
	wg.Wait()
	if n := e.count(`select count(*) from mcp_quote where id=$1 and (approved_at is null) <> (rejected_at is null)`,
		q2); n != 1 {
		t.Fatal("并发批准 / 拒绝之后应恰好有一种结局")
	}
	if n := e.count(`select count(*) from audit_log where action in ($1,$2) and object_id=$3`,
		mcpapp.AuditQuoteApprove, mcpapp.AuditQuoteReject, q2); n != 1 {
		t.Fatalf("并发下只有生效的那一次写审计：%d", n)
	}
}

func TestApproval_RejectThenApprove_Fails(t *testing.T) {
	e, ap := newApprovalEnv(t)
	e.seedPriced("img-m", "text2img", "image", "per_image", 7, "text")
	e.credit(100)
	pid := e.project(e.ws, "片子", genCanvas, time.Now())
	qid := quoteIDOf(t, e.begin(genArgsFor(pid, "")))
	ctx := context.Background()
	if v, err := ap.Reject(ctx, e.owner, qid); err != nil || v.State != mcpapp.ApprovalRejected {
		t.Fatalf("拒绝：%+v %v", v, err)
	}
	v, err := ap.Approve(ctx, e.owner, qid)
	if err != nil || v.State != mcpapp.ApprovalRejected || v.ApprovedAt != nil {
		t.Fatalf("拒绝之后不能再批准：%+v %v", v, err)
	}
	// 库级兜底：两条 CHECK 让「批准 + 拒绝」「拒绝 + 已用」写不进去。
	if _, err := e.pool.Exec(ctx, `update mcp_quote set approved_at = now() where id=$1`, qid); err == nil {
		t.Fatal("ck_mcp_quote_decided 应拒绝同时批准与拒绝")
	}
	if ok, _ := mcprepo.New(e.pool).MarkQuoteUsed(ctx, e.p.GrantID, qid); ok {
		t.Fatal("拒绝的 quote 不能被用掉")
	}
}

func TestApproval_RevokedGrant_409(t *testing.T) {
	e, ap := newApprovalEnv(t)
	e.seedPriced("img-m", "text2img", "image", "per_image", 7, "text")
	e.credit(100)
	pid := e.project(e.ws, "片子", genCanvas, time.Now())
	qid := quoteIDOf(t, e.begin(genArgsFor(pid, "")))
	ctx := context.Background()
	if err := e.keys.RevokeGrant(ctx, e.owner, e.p.GrantID); err != nil {
		t.Fatal(err)
	}
	for name, fn := range map[string]func(context.Context, int64, int64) (mcpapp.ApprovalView, error){
		"get": ap.Get, "approve": ap.Approve} {
		_, err := fn(ctx, e.owner, qid)
		if be := bizOf(err); be == nil || be.Code != errcode.InvalidStateTransition || be.Details["reason"] != "grant_revoked" {
			t.Fatalf("%s：授权撤销后应 409：%v", name, err)
		}
	}
	// CAS 条件本身也带 g.status='active'：绕过 app 层直接写也写不进去。
	if ok, _ := mcprepo.New(e.pool).ApproveQuote(ctx, e.owner, qid, time.Now()); ok {
		t.Fatal("撤销的授权下不能批准")
	}
}

func TestApproval_余额不足不能批准(t *testing.T) {
	e, ap := newApprovalEnv(t)
	e.seedPriced("img-m", "text2img", "image", "per_image", 7, "text")
	e.credit(20)
	pid := e.project(e.ws, "片子", genCanvas, time.Now())
	qid := quoteIDOf(t, e.begin(genArgsFor(pid, "")))
	// 签发之后余额被别的任务花掉。
	exec(t, e.pool, `update credit_grant set remaining = 5 where payer_account_id=$1`, e.payer)
	ctx := context.Background()
	v, err := ap.Get(ctx, e.owner, qid)
	if err != nil || v.State != mcpapp.ApprovalPending || v.Shortfall != 9 || v.After != -9 {
		t.Fatalf("应显示差额：%+v %v", v, err)
	}
	if _, err := ap.Approve(ctx, e.owner, qid); bizCode(err) != errcode.InsufficientCredits {
		t.Fatalf("余额不足不能批准：%v", err)
	}
}

func TestGrant_PageConfirm切换(t *testing.T) {
	e := newGenEnv(t)
	ctx := context.Background()
	g, err := e.keys.SetRequirePageConfirm(ctx, e.owner, e.p.GrantID, true)
	if err != nil || !g.RequirePageConfirm || g.KeyPrefix == "" {
		t.Fatalf("打开：%+v %v", g, err)
	}
	// 重复设同一个值不报错、不再写审计。
	if _, err := e.keys.SetRequirePageConfirm(ctx, e.owner, e.p.GrantID, true); err != nil {
		t.Fatal(err)
	}
	if n := e.count(`select count(*) from audit_log where action=$1`, mcpapp.AuditGrantPageConfirm); n != 1 {
		t.Fatalf("值没变不写审计：%d", n)
	}
	// Resolver 下一次解析就拿到新值（不缓存）。
	var on bool
	_ = e.pool.QueryRow(ctx, `select require_page_confirm from mcp_grant where id=$1`, e.p.GrantID).Scan(&on)
	if !on {
		t.Fatal("库里应已打开")
	}
	// 别人的授权 → 404；撤销后 → 409。
	if _, err := e.keys.SetRequirePageConfirm(ctx, e.userByPhone("13900000022"), e.p.GrantID, false); bizCode(err) !=
		errcode.NotFound {
		t.Fatalf("别人的授权应 404：%v", err)
	}
	if err := e.keys.RevokeGrant(ctx, e.owner, e.p.GrantID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.keys.SetRequirePageConfirm(ctx, e.owner, e.p.GrantID, false); bizCode(err) !=
		errcode.InvalidStateTransition {
		t.Fatalf("撤销后应 409：%v", err)
	}
}
