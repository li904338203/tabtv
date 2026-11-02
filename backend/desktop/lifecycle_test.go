package desktop

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/li904338203/tabtv/backend/internal/platform/resources"
)

// 关窗口前的「还有多少在跑」：只数排队 / 运行中的，落定的不算（TT-T-30）。
func TestActiveWork_只数未结束的(t *testing.T) {
	if _, err := resources.Postgres(); err != nil {
		t.Skipf("没有 PG 二进制：%v", err)
	}
	t.Setenv("TABTV_DATA_DIR", t.TempDir())
	ctx := context.Background()
	rt, err := Boot(ctx, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("启动失败：%v", err)
	}
	defer rt.Close(context.Background())

	w, err := rt.ActiveWork(ctx)
	if err != nil || w.Total() != 0 {
		t.Fatalf("空库应为 0：%+v %v", w, err)
	}
	// running 计入，success / failed 不计。⚠️ 用 running 不用 queued：排队的导出会被后台立即领走、状态随之变化。
	for i, st := range []string{"running", "success", "failed"} {
		if _, err := rt.pool.Exec(ctx, `insert into export_task (id, workspace_id, user_id, status, items, expires_at)
			values ($1, 1, 1, $2, '[]', now() + interval '1 day')`, int64(9000+i), st); err != nil {
			t.Fatal(err)
		}
	}
	if w, err = rt.ActiveWork(ctx); err != nil || w.Exports != 1 || w.Total() != 1 {
		t.Fatalf("应只数到 1 个运行中的导出：%+v %v", w, err)
	}

	if rt.CloseAlwaysMinimize() {
		t.Fatal("缺省应询问")
	}
	if err := rt.SetCloseAlwaysMinimize(ctx, true); err != nil || !rt.CloseAlwaysMinimize() {
		t.Fatalf("「不再询问」应生效：%v", err)
	}
}
