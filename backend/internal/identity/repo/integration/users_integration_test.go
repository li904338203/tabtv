//go:build integration

// 后台用户列表的真库测试。
package integration

import (
	"context"
	"testing"
	"time"

	"github.com/li904338203/tabtv/backend/internal/identity/repo"
	"github.com/li904338203/tabtv/backend/internal/platform/testsupport"
)

// ⚠️ 起因（P15-05）：列表此前不带水印开关与注册时间，管理端的「AI 水印」列对所有人都显示
// 「开启」—— 一个关掉了水印的用户在后台看起来和别人一样，而那恰恰是后台要看清的一件事。
func TestSearchUsers_带水印开关与注册时间(t *testing.T) {
	pool := testsupport.Fresh(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
		insert into app_user (id, email, password_hash, nickname, is_ai_watermark_off, created_at) values
		  (1001, 'on@example.com',  'x', '水印开着', false, '2026-09-01T08:00:00Z'),
		  (1002, 'off@example.com', 'x', '水印关了', true,  '2026-09-02T08:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	list, err := repo.New(pool).SearchUsers(ctx, "", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	got := map[int64]repo.UserSummary{}
	for _, u := range list {
		got[u.ID] = u
	}
	if got[1002].WatermarkOff != true || got[1001].WatermarkOff != false {
		t.Fatalf("水印开关没带出来：%+v", got)
	}
	if want := time.Date(2026, 9, 2, 8, 0, 0, 0, time.UTC); !got[1002].CreatedAt.Equal(want) {
		t.Fatalf("注册时间 = %v，应为 %v", got[1002].CreatedAt, want)
	}
	// 新注册的排前面（按 created_at 倒序）
	if len(list) < 2 || list[0].ID != 1002 {
		t.Fatalf("排序不对：%+v", list)
	}
}
