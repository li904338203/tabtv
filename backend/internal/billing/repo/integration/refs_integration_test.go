//go:build integration

package integration

import (
	"context"
	"testing"

	"github.com/li904338203/tabtv/backend/internal/billing/repo"
)

// TransactionsByRefs（AGT-26：Agent 每轮对账）只读、按引用精确命中、且只认本付费主体。
//
// ⚠️ 引用来自调用方：同一个 refID 在别的付费主体下也可能有流水（refType 是自由字符串），
// 少了 payer 条件就能借一个 ID 读到别人的账。
func TestTransactionsByRefs_按引用取且只认本付费主体(t *testing.T) {
	r, pool, next := usageSetup(t)
	ctx := context.Background()
	mine := mkPayer(t, pool, next)
	other := mkPayer(t, pool, next)
	for _, p := range []int64{mine, other} {
		if _, err := r.Grant(ctx, repo.GrantInput{PayerID: p, Source: "admin_adjust", Amount: 500, Reason: "测试"}); err != nil {
			t.Fatal(err)
		}
	}
	consume := func(payer int64, refType, refID string, amount int) {
		t.Helper()
		if _, err := r.Consume(ctx, repo.ConsumeInput{PayerID: payer, Amount: amount, RefType: refType,
			RefID: refID, Reason: "测试"}); err != nil {
			t.Fatal(err)
		}
	}
	consume(mine, "agent_turn", "11", 4)
	consume(mine, "generation", "21", 30)
	consume(mine, "generation", "22", 20)
	consume(mine, "generation", "23", 10) // 不在引用里
	consume(other, "generation", "21x", 99)
	consume(other, "agent_turn", "12", 7) // 别人的账（refID 不同）
	if _, err := r.Refund(ctx, repo.RefundInput{PayerID: mine, RefType: "generation", RefID: "22", Reason: "失败"}); err != nil {
		t.Fatal(err)
	}

	got, err := r.TransactionsByRefs(ctx, mine, []repo.TxRef{
		{Type: "agent_turn", ID: "11"}, {Type: "generation", ID: "21"}, {Type: "generation", ID: "22"},
		{Type: "agent_turn", ID: "12"}, // 属于另一个付费主体：不得返回
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 {
		t.Fatalf("应返回 3 条消费 + 1 条退款，得到 %d：%+v", len(got), got)
	}
	var consumed, refunded int
	for i, tx := range got {
		if i > 0 && got[i-1].ID >= tx.ID {
			t.Errorf("应按 id 升序")
		}
		switch tx.Type {
		case "consume":
			consumed += -tx.Change
		case "refund":
			refunded += tx.Change
		}
	}
	if consumed != 54 || refunded != 20 {
		t.Errorf("消费 54、退款 20，得到 %d / %d", consumed, refunded)
	}
	if none, err := r.TransactionsByRefs(ctx, mine, nil); err != nil || len(none) != 0 {
		t.Errorf("空引用应返回空：%v %v", none, err)
	}
}
