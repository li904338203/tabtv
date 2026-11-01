package app

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/li904338203/tabtv/backend/internal/mcp/repo"
)

func TestApprovalState_由事实算出(t *testing.T) {
	now := ct0
	at := now.Add(-time.Minute)
	base := repo.Quote{ExpiresAt: now.Add(time.Minute)}
	cases := []struct {
		name string
		mut  func(q *repo.Quote)
		want string
	}{
		{"待确认", func(*repo.Quote) {}, ApprovalPending},
		{"已批准", func(q *repo.Quote) { q.ApprovedAt = &at }, ApprovalApproved},
		{"已批准但过期未用 → 过期", func(q *repo.Quote) { q.ApprovedAt, q.ExpiresAt = &at, now }, ApprovalExpired},
		{"已拒绝（过期后仍显示已拒绝）", func(q *repo.Quote) { q.RejectedAt, q.ExpiresAt = &at, now }, ApprovalRejected},
		{"已用（过期后仍显示已执行）", func(q *repo.Quote) { q.ApprovedAt, q.UsedAt, q.ExpiresAt = &at, &at, now }, ApprovalUsed},
		{"作废优先", func(q *repo.Quote) { q.UsedAt, q.VoidedReason = &at, "price_changed" }, ApprovalVoided},
	}
	for _, c := range cases {
		q := base
		c.mut(&q)
		if got := approvalState(q, now); got != c.want {
			t.Errorf("%s：得到 %s，期望 %s", c.name, got, c.want)
		}
	}
}

func TestApprovalItems_两种detail形状(t *testing.T) {
	// generate（Confirm.issue）：一项，金额即合计。
	gen, _ := json.Marshal(map[string]any{"input": map[string]any{"modelCode": "img-m", "operation": "text2img",
		"count": 2, "textChars": 4}, "summary": "用模型 img-m 执行 text2img（新建image节点），2 张/次", "totalCredits": 14})
	s, items := approvalItems(gen, 14)
	if s == "" || len(items) != 1 || items[0].Credits != 14 || items[0].Label != "img-m · text2img · × 2 · 描述 4 字" {
		t.Fatalf("generate：%q %+v", s, items)
	}
	// quote 工具的计划项：逐项积分。
	plan, _ := json.Marshal(map[string]any{"input": []any{
		map[string]any{"kind": "video", "modelCode": "v1", "quantity": 5, "unit": "second"},
		map[string]any{"kind": "image", "modelCode": "i1", "quantity": 3},
	}, "items": []any{map[string]any{"credits": 50}, map[string]any{"credits": 9}}, "totalCredits": 59})
	s, items = approvalItems(plan, 59)
	if s != "共 2 项" || len(items) != 2 || items[0].Label != "video · v1 · 5 秒" || items[1].Label != "image · i1 · × 3" ||
		items[1].Credits != 9 {
		t.Fatalf("plan：%q %+v", s, items)
	}
	// 解不开的历史形状：退回一行合计，不让审批页打不开。
	s, items = approvalItems(json.RawMessage(`"坏的"`), 8)
	if s == "" || len(items) != 1 || items[0].Credits != 8 {
		t.Fatalf("兜底：%q %+v", s, items)
	}
}
