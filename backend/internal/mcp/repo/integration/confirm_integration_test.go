//go:build integration

// mcp_quote 的 CAS 与清理（MCPT-10）在真库上的行为。
//
// ⚠️ 并发必须在真库多 goroutine 下测：顺序调用永远先撞上「已用」的判定，那种测试把 CAS 的
// `used_at is null` 条件整条删掉照样绿（同 CLAUDE.md §5.7「乐观锁要在真并发下测」）。
package integration

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	mcpapp "github.com/li904338203/tabtv/backend/internal/mcp/app"
	"github.com/li904338203/tabtv/backend/internal/mcp/domain"
	mcprepo "github.com/li904338203/tabtv/backend/internal/mcp/repo"
	"github.com/li904338203/tabtv/backend/internal/platform/id"
	"github.com/li904338203/tabtv/backend/internal/platform/testsupport"
)

type fixedBalance int

func (b fixedBalance) Balance(context.Context, mcpapp.Principal) (int, error) { return int(b), nil }

func fp(b byte) []byte {
	out := make([]byte, 32)
	for i := range out {
		out[i] = b
	}
	return out
}

func TestConfirm_并发同一quote只有一次进入受理(t *testing.T) {
	pool := testsupport.Fresh(t)
	gen, _ := id.New(0)
	store := mcprepo.New(pool)
	c := mcpapp.NewConfirm(store, fixedBalance(100), gen.Next, 0, mcpapp.DeriveStateKey("k"))
	ctx := context.Background()
	p := mcpapp.Principal{UserID: 1, WorkspaceID: 2, GrantID: gen.Next(), PayerAccountID: 3}
	var priced atomic.Int32
	ask := mcpapp.ConfirmAsk{Tool: domain.QuoteToolGenerate, Fingerprint: fp(7),
		Price: func(context.Context) (mcpapp.Priced, error) {
			priced.Add(1)
			return mcpapp.Priced{Credits: 30, Summary: "出一张图"}, nil
		}}
	pd, err := c.Begin(ctx, p, ask, false)
	if err != nil {
		t.Fatal(err)
	}
	const n = 16
	var wg sync.WaitGroup
	var proceed, replay atomic.Int32
	keys := make([]string, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			v, err := c.Verify(ctx, p, ask, domain.Proof{QuoteID: pd.QuoteID, Credits: 30, Via: domain.ProofBaseline})
			if err != nil {
				t.Errorf("并发确认不应报错：%v", err)
				return
			}
			keys[i] = v.IdemKey
			if v.Replay {
				replay.Add(1)
			} else {
				proceed.Add(1)
			}
		}(i)
	}
	close(start)
	wg.Wait()
	if proceed.Load() != 1 || replay.Load() != n-1 {
		t.Fatalf("进入受理 %d 次、重放 %d 次，期望 1 / %d", proceed.Load(), replay.Load(), n-1)
	}
	for _, k := range keys {
		if k != domain.IdemKey(p.GrantID, pd.QuoteID) {
			t.Fatalf("所有调用必须落到同一个幂等键：%v", keys)
		}
	}
	q, err := store.GetQuote(ctx, p.GrantID, pd.QuoteID)
	if err != nil || q.UsedAt == nil || q.VoidedReason != "" {
		t.Fatalf("quote 应已用（不是作废）：%+v %v", q, err)
	}
	// 别的授权取不到（不区分不存在与不是你的）。
	if _, err := store.GetQuote(ctx, p.GrantID+1, pd.QuoteID); err != mcprepo.ErrNotFound {
		t.Fatalf("别的授权应取不到：%v", err)
	}
}

func TestQuoteRepo_作废与用过可分辨且CAS只成功一次(t *testing.T) {
	pool := testsupport.Fresh(t)
	gen, _ := id.New(0)
	store := mcprepo.New(pool)
	ctx := context.Background()
	ins := func(grant int64) int64 {
		q, err := store.InsertQuote(ctx, mcprepo.Quote{ID: gen.Next(), GrantID: grant, Tool: "generate",
			Fingerprint: fp(1), Credits: 10, PayerAccountID: 3, Detail: []byte(`{"input":{}}`),
			ExpiresAt: time.Now().Add(time.Minute)})
		if err != nil {
			t.Fatal(err)
		}
		return q.ID
	}
	a, b := ins(5), ins(5)
	if ok, _ := store.VoidQuote(ctx, 6, a, "price_changed"); ok {
		t.Fatal("别的授权不能作废")
	}
	if ok, _ := store.VoidQuote(ctx, 5, a, "price_changed"); !ok {
		t.Fatal("第一次作废应成功")
	}
	if ok, _ := store.MarkQuoteUsed(ctx, 5, a); ok {
		t.Fatal("作废之后不能再用")
	}
	if ok, _ := store.MarkQuoteUsed(ctx, 5, b); !ok {
		t.Fatal("第一次使用应成功")
	}
	if ok, _ := store.VoidQuote(ctx, 5, b, "price_changed"); ok {
		t.Fatal("用过之后不能再作废")
	}
	qa, _ := store.GetQuote(ctx, 5, a)
	qb, _ := store.GetQuote(ctx, 5, b)
	if qa.UsedAt == nil || qa.VoidedReason != "price_changed" || qb.UsedAt == nil || qb.VoidedReason != "" {
		t.Fatalf("作废与用过要分得开：%+v / %+v", qa, qb)
	}
	if string(qa.Detail) == `{"input":{}}` {
		t.Fatal("作废原因应合并进 detail（原有内容保留）")
	}
}

func TestQuoteRepo_清理过期报价分批(t *testing.T) {
	pool := testsupport.Fresh(t)
	gen, _ := id.New(0)
	store := mcprepo.New(pool)
	ctx := context.Background()
	now := time.Now()
	for i, exp := range []time.Time{now.Add(-40 * 24 * time.Hour), now.Add(-35 * 24 * time.Hour),
		now.Add(-31 * 24 * time.Hour), now.Add(-29 * 24 * time.Hour), now.Add(time.Minute)} {
		if _, err := store.InsertQuote(ctx, mcprepo.Quote{ID: gen.Next(), GrantID: int64(i + 1), Tool: "generate",
			Fingerprint: fp(1), Credits: 1, PayerAccountID: 3, Detail: []byte(`{}`), ExpiresAt: exp}); err != nil {
			t.Fatal(err)
		}
	}
	before := now.Add(-domain.QuoteRetention)
	if n, err := store.CleanupQuotesBefore(ctx, before, 2); err != nil || n != 2 {
		t.Fatalf("单批至多 2 行：%d %v", n, err)
	}
	if n, err := store.CleanupQuotesBefore(ctx, before, 2); err != nil || n != 1 {
		t.Fatalf("第二批删剩下的 1 行：%d %v", n, err)
	}
	var left int
	if err := pool.QueryRow(ctx, `select count(*) from mcp_quote`).Scan(&left); err != nil || left != 2 {
		t.Fatalf("保留期内与未过期的留下：%d %v", left, err)
	}
}
