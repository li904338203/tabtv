package app

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/li904338203/tabtv/backend/internal/mcp/domain"
	"github.com/li904338203/tabtv/backend/internal/mcp/repo"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// memQuotes 是内存版 mcp_quote：CAS 语义与 repo 的 SQL 一致（used_at is null 才改）。
type memQuotes struct {
	mu   sync.Mutex
	rows map[int64]*repo.Quote
}

func newMemQuotes() *memQuotes { return &memQuotes{rows: map[int64]*repo.Quote{}} }

func (m *memQuotes) InsertQuote(_ context.Context, q repo.Quote) (repo.Quote, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := q
	m.rows[q.ID] = &cp
	return q, nil
}

func (m *memQuotes) GetQuote(_ context.Context, grantID, id int64) (repo.Quote, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	q, ok := m.rows[id]
	if !ok || q.GrantID != grantID {
		return repo.Quote{}, repo.ErrNotFound
	}
	return *q, nil
}

func (m *memQuotes) cas(grantID, id int64, voided string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	q, ok := m.rows[id]
	if !ok || q.GrantID != grantID || q.UsedAt != nil || q.RejectedAt != nil {
		return false
	}
	now := time.Now()
	q.UsedAt, q.VoidedReason = &now, voided
	return true
}

func (m *memQuotes) MarkQuoteUsed(_ context.Context, grantID, id int64) (bool, error) {
	return m.cas(grantID, id, ""), nil
}

func (m *memQuotes) VoidQuote(_ context.Context, grantID, id int64, reason string) (bool, error) {
	return m.cas(grantID, id, reason), nil
}

type fixedBalance int

func (b fixedBalance) Balance(context.Context, Principal) (int, error) { return int(b), nil }

var ct0 = time.Date(2026, 10, 17, 8, 0, 0, 0, time.UTC)

func newTestConfirm(store ConfirmStore, bal int) *Confirm {
	n := int64(100)
	c := NewConfirm(store, fixedBalance(bal), func() int64 { n++; return n }, 0, DeriveStateKey("k"))
	c.SetClock(func() time.Time { return ct0 })
	return c
}

func priced(credits int) PriceFn {
	return func(context.Context) (Priced, error) { return Priced{Credits: credits, Summary: "出一张图"}, nil }
}

var testPrincipal = Principal{UserID: 1, WorkspaceID: 2, GrantID: 5, PayerAccountID: 3}

func bizCodeOf(t *testing.T, err error) errcode.Code {
	t.Helper()
	var be *httpx.BizError
	if !errors.As(err, &be) {
		t.Fatalf("期望业务错误，得到 %v", err)
	}
	return be.Code
}

func TestConfirm_余额不足在Begin拒绝且不签发(t *testing.T) {
	store := newMemQuotes()
	c := newTestConfirm(store, 20)
	_, err := c.Begin(context.Background(), testPrincipal, ConfirmAsk{Tool: "generate", Fingerprint: fp32(1),
		Price: priced(30)}, false)
	if bizCodeOf(t, err) != errcode.InsufficientCredits {
		t.Fatalf("应是积分不足：%v", err)
	}
	var be *httpx.BizError
	errors.As(err, &be)
	if be.Details["shortfall"] != 10 || !strings.Contains(be.Error(), "还差 10") {
		t.Fatalf("要说出差额：%v %v", be, be.Details)
	}
	if len(store.rows) != 0 {
		t.Fatal("余额不足不应签发 quote")
	}
}

func TestConfirm_页面确认签发审批链接不发表单(t *testing.T) {
	store := newMemQuotes()
	c := newTestConfirm(store, 100)
	c.SetApprovalBase("https://app.example.com/")
	p := testPrincipal
	p.RequirePageConfirm = true
	ask := ConfirmAsk{Tool: "generate", Fingerprint: fp32(1), Price: priced(30)}
	// 客户端声明了 form 表单也不发：表单里的「确认」不算同意。
	pd, err := c.Begin(context.Background(), p, ask, true)
	if err != nil || pd.Elicit != nil {
		t.Fatalf("页面确认不应发 form 表单：%+v %v", pd, err)
	}
	if want := "https://app.example.com/mcp/approve/" + itoa(pd.QuoteID); pd.ApprovalURL != want {
		t.Fatalf("审批链接 %q，期望 %q", pd.ApprovalURL, want)
	}
	if PendingDetails(pd)["approvalUrl"] != pd.ApprovalURL {
		t.Fatal("对外形态要带 approvalUrl")
	}
	// 声明了 url 模式：以 url 模式交给客户端，elicitationId = quoteId。
	ctx := WithElicitation(context.Background(), ElicitContext{Supported: true, URLSupported: true})
	pd2, err := c.Begin(ctx, p, ask, true)
	if err != nil || pd2.Elicit == nil || pd2.Elicit.URL != pd2.ApprovalURL || pd2.Elicit.Schema != nil ||
		pd2.Elicit.ElicitationID != itoa(pd2.QuoteID) || pd2.Elicit.State == "" {
		t.Fatalf("url 模式：%+v %v", pd2.Elicit, err)
	}
	// 没开页面确认的凭据不给审批链接。
	pd3, _ := c.Begin(context.Background(), testPrincipal, ask, false)
	if pd3.ApprovalURL != "" {
		t.Fatalf("基线不应有审批链接：%q", pd3.ApprovalURL)
	}
}

func TestConfirm_PageMode_EchoIgnored(t *testing.T) {
	store := newMemQuotes()
	c := newTestConfirm(store, 100)
	ctx := context.Background()
	p := testPrincipal
	p.RequirePageConfirm = true
	ask := ConfirmAsk{Tool: "generate", Fingerprint: fp32(1), Price: priced(30)}
	pd, err := c.Begin(ctx, p, ask, false)
	if err != nil {
		t.Fatal(err)
	}
	// 回显正确的金额、elicitation 的回答：都不算同意。
	for _, via := range []domain.ProofVia{domain.ProofBaseline, domain.ProofElicitation} {
		_, err := c.Verify(ctx, p, ask, domain.Proof{QuoteID: pd.QuoteID, Credits: 30, Via: via})
		if bizCodeOf(t, err) != errcode.McpApprovalPending {
			t.Fatalf("%s：应等待页面批准，得到 %v", via, err)
		}
		var be *httpx.BizError
		errors.As(err, &be)
		if be.Details["approvalUrl"] != c.ApprovalURL(pd.QuoteID) {
			t.Fatalf("错误要带审批链接：%v", be.Details)
		}
	}
	if store.rows[pd.QuoteID].UsedAt != nil {
		t.Fatal("未批准不得占用 quote")
	}
	// 别处签发的 quote（如 quote 工具、开关打开之前签的）也一样要页面批准。
	q, _ := newTestConfirm(store, 100).Begin(ctx, testPrincipal, ask, false)
	if _, err := c.Verify(ctx, p, ask, domain.Proof{QuoteID: q.QuoteID, Credits: 30}); bizCodeOf(t, err) !=
		errcode.McpApprovalPending {
		t.Fatalf("开关打开前签发的 quote 也要批准：%v", err)
	}
}

func TestConfirm_PageMode_ApprovedThenOnce(t *testing.T) {
	store := newMemQuotes()
	c := newTestConfirm(store, 100)
	ctx := context.Background()
	p := testPrincipal
	p.RequirePageConfirm = true
	ask := ConfirmAsk{Tool: "generate", Fingerprint: fp32(1), Price: priced(30)}
	pd, _ := c.Begin(ctx, p, ask, false)
	now := ct0
	store.rows[pd.QuoteID].ApprovedAt = &now
	proof := domain.Proof{QuoteID: pd.QuoteID, Credits: 30, Via: domain.ProofBaseline}
	v, err := c.Verify(ctx, p, ask, proof)
	if err != nil || v.Replay || v.IdemKey != domain.IdemKey(5, pd.QuoteID) {
		t.Fatalf("批准后应放行：%+v %v", v, err)
	}
	if v2, err := c.Verify(ctx, p, ask, proof); err != nil || !v2.Replay || v2.IdemKey != v.IdemKey {
		t.Fatalf("再交一次应是重放（同一幂等键）：%+v %v", v2, err)
	}
	// 批准后金额回显仍要对：防止模型拿批准过的 quote 换个金额。
	pd2, _ := c.Begin(ctx, p, ask, false)
	store.rows[pd2.QuoteID].ApprovedAt = &now
	if _, err := c.Verify(ctx, p, ask, domain.Proof{QuoteID: pd2.QuoteID, Credits: 1}); bizCodeOf(t, err) !=
		errcode.McpQuoteInvalid {
		t.Fatalf("金额不符：%v", err)
	}
}

func TestConfirm_PageMode_Rejected(t *testing.T) {
	store := newMemQuotes()
	c := newTestConfirm(store, 100)
	ctx := context.Background()
	p := testPrincipal
	p.RequirePageConfirm = true
	ask := ConfirmAsk{Tool: "generate", Fingerprint: fp32(1), Price: priced(30)}
	pd, _ := c.Begin(ctx, p, ask, false)
	now := ct0
	store.rows[pd.QuoteID].RejectedAt = &now
	proof := domain.Proof{QuoteID: pd.QuoteID, Credits: 30}
	// 拒绝之后用户关掉了页面确认，同一个 quoteId 回显金额也不能复活它。
	for _, pr := range []Principal{p, testPrincipal} {
		_, err := c.Verify(ctx, pr, ask, proof)
		var be *httpx.BizError
		if bizCodeOf(t, err) != errcode.McpQuoteInvalid || !errors.As(err, &be) || be.Details["reason"] != "rejected" {
			t.Fatalf("已拒绝应无效：%v", err)
		}
	}
	if store.rows[pd.QuoteID].UsedAt != nil {
		t.Fatal("拒绝的 quote 不应被用掉")
	}
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }

func TestConfirm_基线流程与重放(t *testing.T) {
	store := newMemQuotes()
	c := newTestConfirm(store, 100)
	ctx := context.Background()
	ask := ConfirmAsk{Tool: "generate", Fingerprint: fp32(1), Price: priced(30)}
	pd, err := c.Begin(ctx, testPrincipal, ask, false)
	if err != nil || pd.QuoteID == 0 || pd.Credits != 30 || pd.After != 70 || pd.Elicit != nil {
		t.Fatalf("Begin：%+v %v", pd, err)
	}
	proof := domain.Proof{QuoteID: pd.QuoteID, Credits: 30, Via: domain.ProofBaseline}
	v, err := c.Verify(ctx, testPrincipal, ask, proof)
	if err != nil || v.Replay || v.IdemKey != domain.IdemKey(5, pd.QuoteID) {
		t.Fatalf("第一次应放行：%+v %v", v, err)
	}
	v2, err := c.Verify(ctx, testPrincipal, ask, proof)
	if err != nil || !v2.Replay || v2.IdemKey != v.IdemKey {
		t.Fatalf("第二次应重放且同一幂等键：%+v %v", v2, err)
	}
	// 金额不符 / 指纹不符 / 他人授权。
	for name, tc := range map[string]struct {
		p     Principal
		proof domain.Proof
		ask   ConfirmAsk
		want  domain.InvalidReason
	}{
		"金额不符": {testPrincipal, domain.Proof{QuoteID: pd.QuoteID, Credits: 29}, ask, domain.ReasonAmountMismatch},
		"指纹不符": {testPrincipal, proof, ConfirmAsk{Fingerprint: fp32(2), Price: priced(30)}, domain.ReasonFingerprintMismatch},
		"他人授权": {Principal{GrantID: 6, PayerAccountID: 3}, proof, ask, domain.ReasonNotFound},
	} {
		_, err := c.Verify(ctx, tc.p, tc.ask, tc.proof)
		var be *httpx.BizError
		if !errors.As(err, &be) || be.Code != errcode.McpQuoteInvalid || be.Details["reason"] != string(tc.want) {
			t.Errorf("%s：%v", name, err)
		}
	}
}

func TestConfirm_过期被拒(t *testing.T) {
	store := newMemQuotes()
	c := newTestConfirm(store, 100)
	ask := ConfirmAsk{Tool: "generate", Fingerprint: fp32(1), Price: priced(30)}
	pd, _ := c.Begin(context.Background(), testPrincipal, ask, false)
	c.SetClock(func() time.Time { return ct0.Add(DefaultQuoteTTL) })
	_, err := c.Verify(context.Background(), testPrincipal, ask, domain.Proof{QuoteID: pd.QuoteID, Credits: 30})
	var be *httpx.BizError
	if !errors.As(err, &be) || be.Details["reason"] != string(domain.ReasonExpired) {
		t.Fatalf("应过期：%v", err)
	}
}

func TestConfirm_价格变化作废旧quote并签发新报价(t *testing.T) {
	store := newMemQuotes()
	c := newTestConfirm(store, 100)
	ctx := context.Background()
	ask := ConfirmAsk{Tool: "generate", Fingerprint: fp32(1), Price: priced(30)}
	pd, _ := c.Begin(ctx, testPrincipal, ask, false)
	ask.Price = priced(40)
	_, err := c.Verify(ctx, testPrincipal, ask, domain.Proof{QuoteID: pd.QuoteID, Credits: 30})
	var be *httpx.BizError
	if !errors.As(err, &be) || be.Code != errcode.McpQuotePriceChanged {
		t.Fatalf("应价格变化：%v", err)
	}
	nq, _ := be.Details["quote"].(map[string]any)
	if nq == nil || nq["credits"] != 40 || nq["quoteId"] == "" {
		t.Fatalf("details 要带新报价：%v", be.Details)
	}
	old, _ := store.GetQuote(ctx, 5, pd.QuoteID)
	if old.UsedAt == nil || old.VoidedReason != "price_changed" {
		t.Fatalf("旧 quote 应作废：%+v", old)
	}
	// 作废的 quote 再提交不能当「已用」重放（同一幂等键下没有任务，会按新价受理一次）。
	_, err = c.Verify(ctx, testPrincipal, ask, domain.Proof{QuoteID: pd.QuoteID, Credits: 30})
	if !errors.As(err, &be) || be.Details["reason"] != string(domain.ReasonVoided) {
		t.Fatalf("作废的 quote 应无效：%v", err)
	}
}

func TestConfirm_确认后余额不足不占用quote(t *testing.T) {
	store := newMemQuotes()
	ctx := context.Background()
	ask := ConfirmAsk{Tool: "generate", Fingerprint: fp32(1), Price: priced(30)}
	pd, _ := newTestConfirm(store, 100).Begin(ctx, testPrincipal, ask, false)
	_, err := newTestConfirm(store, 10).Verify(ctx, testPrincipal, ask, domain.Proof{QuoteID: pd.QuoteID, Credits: 30})
	if bizCodeOf(t, err) != errcode.InsufficientCredits {
		t.Fatalf("应积分不足：%v", err)
	}
	if q, _ := store.GetQuote(ctx, 5, pd.QuoteID); q.UsedAt != nil {
		t.Fatal("余额不足时 quote 应保持未用（补足后可原样重试）")
	}
}

func TestConfirm_并发同一确认只有一次放行(t *testing.T) {
	store := newMemQuotes()
	c := newTestConfirm(store, 100)
	ctx := context.Background()
	ask := ConfirmAsk{Tool: "generate", Fingerprint: fp32(1), Price: priced(30)}
	pd, _ := c.Begin(ctx, testPrincipal, ask, false)
	var wg sync.WaitGroup
	var mu sync.Mutex
	proceed, replay := 0, 0
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, err := c.Verify(ctx, testPrincipal, ask, domain.Proof{QuoteID: pd.QuoteID, Credits: 30})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err != nil:
				t.Errorf("并发确认不应报错：%v", err)
			case v.Replay:
				replay++
			default:
				proceed++
			}
		}()
	}
	wg.Wait()
	if proceed != 1 || replay != 15 {
		t.Fatalf("放行 %d 次、重放 %d 次，期望 1 / 15", proceed, replay)
	}
}

func TestConfirm_elicitation与基线同一判定(t *testing.T) {
	store := newMemQuotes()
	c := newTestConfirm(store, 100)
	ctx := context.Background()
	ask := ConfirmAsk{Tool: "generate", Fingerprint: fp32(1), Price: priced(30)}
	pd, err := c.Begin(ctx, testPrincipal, ask, true)
	if err != nil || pd.Elicit == nil || pd.Elicit.State == "" {
		t.Fatalf("应给 elicitation 表单：%+v %v", pd, err)
	}
	for _, s := range []string{"出一张图", "30 积分", "付费账户", "当前余额 100", "确认后剩余 70"} {
		if !strings.Contains(pd.Elicit.Message, s) {
			t.Errorf("表单说明缺「%s」：%s", s, pd.Elicit.Message)
		}
	}
	proof, err := c.ProofFromElicitation(ctx, testPrincipal, pd.Elicit.State)
	if err != nil || proof.QuoteID != pd.QuoteID || proof.Credits != 30 || proof.Via != domain.ProofElicitation {
		t.Fatalf("回答换成同一个 Proof：%+v %v", proof, err)
	}
	v, err := c.Verify(ctx, testPrincipal, ask, proof)
	if err != nil || v.IdemKey != domain.IdemKey(5, pd.QuoteID) {
		t.Fatalf("elicitation 与基线落到同一幂等键：%+v %v", v, err)
	}
	// 同一个 quoteId 再走基线：已用 → 重放，同一幂等键。
	v2, err := c.Verify(ctx, testPrincipal, ask, domain.Proof{QuoteID: pd.QuoteID, Credits: 30})
	if err != nil || !v2.Replay || v2.IdemKey != v.IdemKey {
		t.Fatalf("两条路径共用一张 quote：%+v %v", v2, err)
	}
}

func TestRequestState_篡改与过期被拒(t *testing.T) {
	key := DeriveStateKey("secret")
	exp := ct0.Add(10 * time.Minute)
	s := SignState(key, 4242, exp)
	if id, err := VerifyState(key, s, ct0); err != nil || id != 4242 {
		t.Fatalf("原样应通过：%d %v", id, err)
	}
	body, sig, _ := strings.Cut(s, ".")
	forged := SignState(DeriveStateKey("other"), 4243, exp)
	fbody, _, _ := strings.Cut(forged, ".")
	for name, bad := range map[string]string{
		"换 quoteId": fbody + "." + sig,
		"换密钥签":      forged,
		"截断签名":      body + "." + sig[:len(sig)-2],
		"没有分隔符":     body,
		"空串":        "",
		"非 base64":  "!!!." + sig,
	} {
		if _, err := VerifyState(key, bad, ct0); !errors.Is(err, ErrBadState) {
			t.Errorf("%s 应被拒：%v", name, err)
		}
	}
	if _, err := VerifyState(key, s, exp); !errors.Is(err, ErrBadState) {
		t.Error("到期即拒")
	}
	if _, err := VerifyState(nil, s, ct0); !errors.Is(err, ErrBadState) {
		t.Error("没有密钥时一律拒")
	}
	if string(DeriveStateKey("secret")) == "secret" || len(DeriveStateKey("secret")) != 32 {
		t.Error("应是派生出的 32 字节子密钥，不是主密钥本身")
	}
}

func fp32(b byte) []byte {
	out := make([]byte, 32)
	for i := range out {
		out[i] = b
	}
	return out
}
