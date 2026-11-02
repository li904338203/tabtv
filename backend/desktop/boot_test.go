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
	// 本机身份的三个接口。
	if code, out := call(http.MethodGet, "/api/v1/workspaces", nil); !ok(code) {
		t.Fatalf("工作区 %d %v", code, out)
	}
	if code, out := call(http.MethodGet, "/api/v1/auth/me", nil); !ok(code) || out["nickname"] != "本机用户" {
		t.Fatalf("当前用户 %d %v", code, out)
	}
	if code, _ := call(http.MethodPut, "/api/v1/auth/profile", map[string]any{"isAiWatermarkOff": true}); code != 204 {
		t.Fatalf("水印开关 %d", code)
	}
	// 本机设置与上游调用日志。
	if code, out := call(http.MethodGet, "/api/v1/settings", nil); !ok(code) || out["items"] == nil {
		t.Fatalf("设置 %d %v", code, out)
	}
	if code, out := call(http.MethodPut, "/api/v1/settings", map[string]any{"generation.max_running": 6}); !ok(code) {
		t.Fatalf("改设置 %d %v", code, out)
	}
	if code, _ := call(http.MethodPut, "/api/v1/settings", map[string]any{"no.such.key": 1}); code != 400 {
		t.Fatalf("未知设置键应 400，得到 %d", code)
	}
	if code, out := call(http.MethodGet, "/api/v1/settings/storage/usage", nil); !ok(code) || out["database"] == nil {
		t.Fatalf("存储占用 %d %v", code, out)
	}
	if code, out := call(http.MethodGet, "/api/v1/admin/request-logs/provider", nil); !ok(code) {
		t.Fatalf("上游调用日志 %d %v", code, out)
	}
	if code, out := call(http.MethodGet, "/api/v1/settings/diagnostics", nil); !ok(code) || out["checks"] == nil {
		t.Fatalf("环境自检 %d %v", code, out)
	}
	// 设置页的技能 / 提示词 / 预设 / 营销（TT-T-19~21）：沿用来源系统的管理端点，预设写接口与「恢复内置版本」是新增的。
	if code, out := call(http.MethodGet, "/api/v1/admin/agent/skills", nil); !ok(code) {
		t.Fatalf("技能列表 %d %v", code, out)
	}
	if code, out := call(http.MethodGet, "/api/v1/admin/prompts/specs", nil); !ok(code) {
		t.Fatalf("提示词规格 %d %v", code, out)
	}
	if code, out := call(http.MethodGet, "/api/v1/admin/marketing/options", nil); !ok(code) {
		t.Fatalf("营销选项 %d %v", code, out)
	}
	code, out = call(http.MethodGet, "/api/v1/admin/image-presets", nil)
	items, _ := out["items"].([]any)
	if !ok(code) || len(items) == 0 {
		t.Fatalf("预设列表 %d %v", code, out)
	}
	first, _ := items[0].(map[string]any)
	pcode, _ := first["code"].(string)
	if first["isBuiltin"] != true {
		t.Fatalf("seed 装的预设应标内置：%v", first)
	}
	first["prompt"] = "冒烟改过的提示词"
	if code, out := call(http.MethodPut, "/api/v1/admin/image-presets/"+pcode, first); !ok(code) {
		t.Fatalf("改预设 %d %v", code, out)
	}
	if code, out := call(http.MethodPost, "/api/v1/admin/image-presets/"+pcode+"/restore-builtin", nil); !ok(code) {
		t.Fatalf("恢复内置预设 %d %v", code, out)
	}
	code, out = call(http.MethodGet, "/api/v1/admin/agent/skills", nil)
	skills, _ := out["items"].([]any)
	if !ok(code) || len(skills) == 0 {
		t.Fatalf("技能列表为空 %d %v", code, out)
	}
	restored := false
	for _, it := range skills {
		s, _ := it.(map[string]any)
		if s["isBuiltin"] == true {
			id, _ := s["id"].(string)
			if code, out := call(http.MethodPost, "/api/v1/admin/agent/skills/"+id+"/restore-builtin", nil); !ok(code) {
				t.Fatalf("恢复内置技能 %d %v", code, out)
			}
			restored = true
			break
		}
	}
	if !restored {
		t.Fatal("没有标内置的技能")
	}
	// 被裁剪的域不挂路由。
	for _, p := range []string{"/api/v1/credits/balance", "/api/v1/auth/login", "/api/v1/orders"} {
		if code, _ := call(http.MethodGet, p, nil); code != 404 && code != 405 {
			t.Errorf("%s 应不存在，得到 %d", p, code)
		}
	}
}
