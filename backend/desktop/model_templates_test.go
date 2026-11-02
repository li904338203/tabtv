package desktop

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/li904338203/tabtv/backend/internal/platform/resources"
)

// 模板模式（TT-T-18）：列出模板 → 不填 Key 启用被拒 → 填 Key 启用后模板模型逐个过发布检查并启用。
func TestModelTemplates_填Key启用(t *testing.T) {
	if _, err := resources.Postgres(); err != nil {
		t.Skipf("没有 PG 二进制：%v", err)
	}
	t.Setenv("TABTV_DATA_DIR", t.TempDir())
	rt, err := Boot(context.Background(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("启动失败：%v", err)
	}
	defer rt.Close(context.Background())
	call := func(method, path string, body any) (int, map[string]any) {
		t.Helper()
		var rd io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rd = bytes.NewReader(b)
		}
		req := httptest.NewRequest(method, path, rd)
		rec := httptest.NewRecorder()
		rt.Handler.ServeHTTP(rec, req)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}

	code, out := call(http.MethodGet, "/api/v1/admin/model-templates", nil)
	items, _ := out["items"].([]any)
	if code != http.StatusOK || len(items) == 0 {
		t.Fatalf("模板列表：%d %v", code, out)
	}
	var openai map[string]any
	for _, it := range items {
		if m := it.(map[string]any); m["code"] == "openai" {
			openai = m
		}
	}
	if openai == nil || openai["installed"] != true || openai["status"] != "disabled" || openai["hasApiKey"] != false {
		t.Fatalf("openai 模板应已写入、停用、无 Key：%v", openai)
	}

	// 没有 Key：拒绝启用（需要认证的厂商不能空着 Key 上线）。
	if code, out := call(http.MethodPost, "/api/v1/admin/model-templates/openai/enable", nil); code < 400 && out["code"] == nil {
		t.Fatalf("无 Key 启用应失败：%d %v", code, out)
	}
	code, out = call(http.MethodPost, "/api/v1/admin/model-templates/openai/enable",
		map[string]any{"apiKey": map[string]string{"action": "replace", "value": "sk-test-1234567890"}})
	if code != http.StatusOK {
		t.Fatalf("启用失败：%d %v", code, out)
	}
	models, _ := out["models"].([]any)
	if len(models) == 0 || out["enabledCount"].(float64) == 0 {
		t.Fatalf("应至少启用一个模型：%v", out)
	}
	for _, m := range models {
		mm := m.(map[string]any)
		if mm["enabled"] != true {
			t.Errorf("模板模型 %v 没启用：%v", mm["code"], mm["reason"])
		}
	}
	// 再点一次：已启用的跳过，不报错。
	if code, out := call(http.MethodPost, "/api/v1/admin/model-templates/openai/enable", nil); code != http.StatusOK {
		t.Fatalf("重复启用应成功：%d %v", code, out)
	}
	if code, _ := call(http.MethodPost, "/api/v1/admin/model-templates/nope/enable", nil); code == http.StatusOK {
		t.Fatal("不存在的模板应报错")
	}
}
