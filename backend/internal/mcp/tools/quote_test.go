package tools

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/li904338203/tabtv/backend/internal/mcp/app"
	"github.com/li904338203/tabtv/backend/internal/mcp/domain"
	"github.com/li904338203/tabtv/backend/internal/mcp/repo"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// 假报价端口：单项按 count×10；计划项 render 按秒 ×1、其余 quantity×5；模型 "vip" 不可用。
type fakeQuoter struct {
	mu    sync.Mutex
	items []app.QuoteItem
}

func (f *fakeQuoter) QuoteItem(_ context.Context, _ app.Principal, in app.QuoteItem) (domain.QuoteLine, error) {
	f.mu.Lock()
	f.items = append(f.items, in)
	f.mu.Unlock()
	if in.ModelCode == "vip" {
		return domain.QuoteLine{BlockedCode: "MEMBER_LEVEL_REQUIRED", BlockedReason: "需要专业版", UserFixable: true}, nil
	}
	return domain.QuoteLine{Credits: in.Count * 10, Unit: "per_image", EstimatedSeconds: 12, Available: true}, nil
}

func (*fakeQuoter) QuotePlanItem(_ context.Context, _ app.Principal, in app.PlanQuoteItem) (domain.QuoteLine, error) {
	if in.ModelCode == "vip" {
		return domain.QuoteLine{BlockedCode: "MEMBER_LEVEL_REQUIRED", BlockedReason: "需要专业版", UserFixable: true}, nil
	}
	if in.Kind == "render" {
		return domain.QuoteLine{Credits: in.Quantity, Unit: "per_second", Available: true}, nil
	}
	return domain.QuoteLine{Credits: in.Quantity * 5, Unit: "per_image", Available: true}, nil
}

func (*fakeQuoter) Balance(context.Context, app.Principal) (int, error) { return 25, nil }

type fakeQuoteStore struct {
	mu   sync.Mutex
	rows []repo.Quote
}

func (s *fakeQuoteStore) InsertQuote(_ context.Context, q repo.Quote) (repo.Quote, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	q.CreatedAt = t0
	s.rows = append(s.rows, q)
	return q, nil
}

func (s *fakeQuoteStore) GetQuote(_ context.Context, grantID, id int64) (repo.Quote, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, q := range s.rows {
		if q.ID == id && q.GrantID == grantID {
			return q, nil
		}
	}
	return repo.Quote{}, repo.ErrNotFound
}

// cas 与 repo 的 SQL 同语义：used_at is null 才改。
func (s *fakeQuoteStore) cas(grantID, id int64, voided string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.rows {
		q := &s.rows[i]
		if q.ID == id && q.GrantID == grantID && q.UsedAt == nil {
			now := t0
			q.UsedAt, q.VoidedReason = &now, voided
			return true
		}
	}
	return false
}

func (s *fakeQuoteStore) MarkQuoteUsed(_ context.Context, grantID, id int64) (bool, error) {
	return s.cas(grantID, id, ""), nil
}

func (s *fakeQuoteStore) VoidQuote(_ context.Context, grantID, id int64, reason string) (bool, error) {
	return s.cas(grantID, id, reason), nil
}

func newFakeQuotes(store *fakeQuoteStore) *app.Quotes {
	n := int64(9000)
	q := app.NewQuotes(&fakeQuoter{}, store, func() int64 { n++; return n }, 0)
	q.SetClock(func() time.Time { return t0 })
	return q
}

func TestMCPQuote_单项余额差额与签发(t *testing.T) {
	store := &fakeQuoteStore{}
	d := fakeDeps()
	d.Quotes = newFakeQuotes(store)
	m, out := call(t, NewRegistry(d), "quote", `{"item":{"modelCode":"m","operation":"text2img","count":3}}`, 6000)
	if m["totalCredits"].(float64) != 30 || m["balance"].(float64) != 25 || m["shortfall"].(float64) != 5 {
		t.Fatalf("合计 / 余额 / 差额：%v", m)
	}
	if m["quoteId"] != "9001" || m["payerAccountId"] != "3" || m["expiresAt"] != "2026-10-17T08:10:00Z" {
		t.Fatalf("quoteId / 付费主体 / 过期：%v", m)
	}
	if len(store.rows) != 1 {
		t.Fatalf("应落一行：%d", len(store.rows))
	}
	r := store.rows[0]
	if r.GrantID != 5 || r.Tool != domain.QuoteToolGenerate || r.Credits != 30 || r.PayerAccountID != 3 ||
		len(r.Fingerprint) != 32 {
		t.Fatalf("落库字段：%+v", r)
	}
	if !strings.Contains(out.Summary, "还差 5") {
		t.Errorf("摘要应说出差额：%s", out.Summary)
	}
}

func TestMCPQuote_不填默认值且文本不入库(t *testing.T) {
	store := &fakeQuoteStore{}
	fq := &fakeQuoter{}
	d := fakeDeps()
	qs := app.NewQuotes(fq, store, func() int64 { return 1 }, 0)
	d.Quotes = qs
	call(t, NewRegistry(d), "quote", `{"item":{"modelCode":" m ","text":"  机密提示词  "}}`, 6000)
	got := fq.items[0]
	if got.ModelCode != " m " || got.Text != "  机密提示词  " || got.Count != 0 || got.DurationSec != 0 || got.Inputs != nil {
		t.Fatalf("单项应原样透传、不补默认值：%+v", got)
	}
	if strings.Contains(string(store.rows[0].Detail), "机密") {
		t.Fatalf("detail 不应含提示词原文：%s", store.rows[0].Detail)
	}
	var det struct {
		Input struct {
			TextChars int `json:"textChars"`
		} `json:"input"`
	}
	_ = json.Unmarshal(store.rows[0].Detail, &det)
	if det.Input.TextChars != 9 {
		t.Fatalf("文本长度按码点记：%s", store.rows[0].Detail)
	}
}

func TestMCPQuote_有不可用项不签发(t *testing.T) {
	store := &fakeQuoteStore{}
	d := fakeDeps()
	d.Quotes = newFakeQuotes(store)
	m, _ := call(t, NewRegistry(d), "quote",
		`{"items":[{"kind":"image","modelCode":"m","quantity":2},{"kind":"video","modelCode":"vip","quantity":5,"unit":"second"}]}`, 6000)
	if _, ok := m["quoteId"]; ok {
		t.Fatalf("有不可用项不应签发：%v", m)
	}
	if len(store.rows) != 0 {
		t.Fatal("不签发就不应落库")
	}
	if !strings.Contains(m["notIssuedReason"].(string), "第 2 项：需要专业版") || m["totalCredits"].(float64) != 10 {
		t.Fatalf("其余项照常报价并说明原因：%v", m)
	}
	items := m["items"].([]any)
	if b := items[1].(map[string]any); b["available"] != false || b["userFixable"] != true || b["credits"].(float64) != 0 {
		t.Fatalf("不可用项：%v", b)
	}
}

func TestMCPQuote_入参校验(t *testing.T) {
	reg := NewRegistry(fakeDeps())
	tool, _ := reg.Lookup("quote")
	for _, args := range []string{
		`{}`,
		`{"item":{"modelCode":"m"},"items":[{"kind":"image","modelCode":"m","quantity":1}]}`,
		`{"items":[{"kind":"writing","modelCode":"m","quantity":1}]}`,
		`{"items":[{"kind":"image","quantity":1}]}`,
		`{"items":[{"kind":"image","modelCode":"m","quantity":0}]}`,
		`{"items":[{"kind":"image","modelCode":"m","quantity":601}]}`,
		`{"items":[{"kind":"image","modelCode":"m","quantity":1,"unit":"day"}]}`,
		`{"items":[{"kind":"image","modelCode":"m","quantity":1,"inputs":{"image":21}}]}`,
		`{"items":[` + strings.TrimSuffix(strings.Repeat(`{"kind":"render","quantity":1},`, 21), ",") + `]}`,
	} {
		c, err := tool.Decode(json.RawMessage(args))
		if err != nil {
			continue // schema 层已拒
		}
		_, err = tool.Handler(ctxWithPrincipal(), c)
		var be *httpx.BizError
		if err == nil || !asBizErr(err, &be) || be.Code != errcode.InvalidArgument {
			t.Errorf("%s 应报参数错误：%v", args, err)
		}
	}
}

func asBizErr(err error, be **httpx.BizError) bool {
	b, ok := err.(*httpx.BizError)
	if ok {
		*be = b
	}
	return ok
}

func TestMCPQuote_指纹与键序和confirm无关(t *testing.T) {
	a := app.QuoteRequest{Item: &app.QuoteItem{ModelCode: "m", Operation: "text2img", Count: 2,
		Inputs: map[string]int{"image": 1, "text": 1}}}
	b := app.QuoteRequest{Item: &app.QuoteItem{Inputs: map[string]int{"text": 1, "image": 1}, Count: 2,
		Operation: "text2img", ModelCode: "m"}}
	ta, fa, _ := app.FingerprintFor(a)
	tb, fb, _ := app.FingerprintFor(b)
	if ta != domain.QuoteToolGenerate || tb != ta || string(fa) != string(fb) {
		t.Fatal("同一组参数应同指纹")
	}
	b.Item.Count = 3
	if _, fc, _ := app.FingerprintFor(b); string(fc) == string(fa) {
		t.Fatal("改了数量应不同指纹")
	}
	tp, _, _ := app.FingerprintFor(app.QuoteRequest{Items: []app.PlanQuoteItem{{Kind: "render", Quantity: 3}}})
	if tp != domain.QuoteToolPlan {
		t.Fatalf("计划项工具名：%s", tp)
	}
}

// MCPT-15 实测（Cursor）：模型先 `quote {count:1}` 再 `generate` 不带 count 去确认 —— 钱与要做的事都一样
// （计价与受理都把 ≤0 当 1），却得到 fingerprint_mismatch。count 的 0 / 缺省 / 1 必须同指纹；
// 时长的 0 与 5 不归一（0 是「没传」，发给上游由模型自己的默认时长决定）。
func TestMCPQuote_指纹里count的0与1等价(t *testing.T) {
	fp := func(it app.QuoteItem) string {
		_, f, err := app.FingerprintFor(app.QuoteRequest{Item: &it})
		if err != nil {
			t.Fatal(err)
		}
		return string(f)
	}
	base := app.QuoteItem{ModelCode: "m", Operation: "text2img", Text: "红伞"}
	one := base
	one.Count = 1
	if fp(base) != fp(one) {
		t.Fatal("count 缺省与 count=1 应同指纹")
	}
	if g, _ := app.GenerateFingerprint(base); string(g) != fp(one) {
		t.Fatal("generate 不带 count 与 quote count=1 应同指纹")
	}
	two := base
	two.Count = 2
	if fp(two) == fp(one) {
		t.Fatal("count=2 与 1 应不同指纹")
	}
	five := base
	five.DurationSec = 5
	if fp(five) == fp(base) {
		t.Fatal("时长 0（没传）与 5 不归一")
	}
}
