package transport

import (
	"testing"

	"github.com/li904338203/tabtv/backend/internal/billing/repo"
)

// 后台订单检索此前收 cursor 却从不回下一页的游标：更早的订单在后台根本翻不到。
func TestOrderNextCursor_满页才有下一页(t *testing.T) {
	page := []repo.Order{{ID: 30}, {ID: 20}, {ID: 10}}
	if got := orderNextCursor(page, 3); got != "10" {
		t.Fatalf("满页时应以最后一条的 id 作游标，得到 %q", got)
	}
	if got := orderNextCursor(page, 5); got != "" {
		t.Fatalf("不满一页说明没有下一页，得到 %q", got)
	}
	if got := orderNextCursor(nil, 50); got != "" {
		t.Fatalf("空页没有下一页，得到 %q", got)
	}
	// 处理器与仓储用同一个每页条数：各算各的话，传 500 时仓储按 50 取，
	// 处理器拿 50 条去比 500，永远判成「没有下一页」。
	if repo.OrderPageSize(500) != 50 || repo.OrderPageSize(0) != 50 || repo.OrderPageSize(30) != 30 {
		t.Fatal("每页条数的归一规则变了")
	}
}
