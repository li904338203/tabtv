package desktop

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/li904338203/tabtv/backend/internal/platform/resources"
)

// 启动冒烟（TT-T-04/07）：临时数据目录里完整跑一遍 Boot，再打几条真实接口。
// 没有 PG 二进制时跳过（先运行 build/fetch-third-party.sh postgres）。
func TestBoot_空目录启动并能调用接口(t *testing.T) {
	if _, err := resources.Postgres(); err != nil {
		t.Skipf("没有 PG 二进制：%v", err)
	}
	t.Setenv("TABTV_DATA_DIR", t.TempDir())
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if testing.Verbose() {
		log = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	}
	ctx := context.Background()
	t0 := time.Now()
	rt, err := Boot(ctx, log)
	if err != nil {
		t.Fatalf("启动失败：%v", err)
	}
	t.Logf("首次启动耗时 %v（%v）", time.Since(t0), rt.Timings)
	defer rt.Close(context.Background())

	call := func(method, path string, body any) (int, map[string]any) {
		t.Helper()
		var rd io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rd = bytes.NewReader(b)
		}
		req := httptest.NewRequest(method, path, rd)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		rt.Handler.ServeHTTP(rec, req)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}

	if code, out := call(http.MethodGet, "/api/v1/health", nil); code != 200 {
		t.Fatalf("health %d %v", code, out)
	}
	// 用户侧：带工作区闸的业务接口（本机身份由 LocalAuth 自动补上）。
	ok := func(code int) bool { return code >= 200 && code < 300 }
	code, out := call(http.MethodPost, "/api/v1/projects", map[string]any{"name": "冒烟项目"})
	if !ok(code) || out["id"] == nil {
		t.Fatalf("建项目 %d %v", code, out)
	}
	if code, out := call(http.MethodGet, "/api/v1/projects", nil); !ok(code) {
		t.Fatalf("项目列表 %d %v", code, out)
	}
	if code, out := call(http.MethodGet, "/api/v1/canvas/config", nil); !ok(code) {
		t.Fatalf("画布配置 %d %v", code, out)
	}
	// 管理侧：设置页复用的管理端点（本机管理员令牌）。
	if code, out := call(http.MethodGet, "/api/v1/admin/models", nil); !ok(code) {
		t.Fatalf("模型管理 %d %v", code, out)
	}
	// 被裁剪的域不挂路由。
	for _, p := range []string{"/api/v1/credits/balance", "/api/v1/auth/login", "/api/v1/orders"} {
		if code, _ := call(http.MethodGet, p, nil); code != 404 && code != 405 {
			t.Errorf("%s 应不存在，得到 %d", p, code)
		}
	}
}
