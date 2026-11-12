package desktop

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/li904338203/tabtv/backend/internal/platform/resources"
)

type testCaller func(method, path string, body any) (int, []byte)

func bootForTest(t *testing.T) (*Runtime, testCaller) {
	t.Helper()
	t.Setenv("TABTV_DATA_DIR", t.TempDir())
	rt, err := Boot(context.Background(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("启动失败：%v", err)
	}
	call := func(method, path string, body any) (int, []byte) {
		t.Helper()
		var rd io.Reader
		if body != nil {
			if raw, ok := body.([]byte); ok {
				rd = bytes.NewReader(raw)
			} else {
				b, _ := json.Marshal(body)
				rd = bytes.NewReader(b)
			}
		}
		rec := httptest.NewRecorder()
		rt.Handler.ServeHTTP(rec, httptest.NewRequest(method, path, rd))
		return rec.Code, rec.Body.Bytes()
	}
	return rt, call
}

func mustOK(t *testing.T, what string, do func() (int, []byte)) map[string]any {
	t.Helper()
	code, body := do()
	if code/100 != 2 {
		t.Fatalf("%s：HTTP %d %s", what, code, body)
	}
	var out map[string]any
	_ = json.Unmarshal(body, &out)
	return out
}

// 换电脑同步：A 机配好模板厂商 + 自建中转 + 公网中转存储 → 导出（带明文 Key）→ 空的 B 机预览、导入 →
// B 机再导出一份，厂商 / 模型 / 绑定 / 能力 / 参数 / 存储与 A 机逐项一致。
func TestModelConfig_导出导入往返一致(t *testing.T) {
	if _, err := resources.Postgres(); err != nil {
		t.Skipf("没有 PG 二进制：%v", err)
	}
	rtA, a := bootForTest(t)
	mustOK(t, "启用 openai 模板", func() (int, []byte) {
		return a(http.MethodPost, "/api/v1/admin/model-templates/openai/enable",
			map[string]any{"apiKey": map[string]string{"action": "replace", "value": "sk-openai-1234567890"}})
	})
	mustOK(t, "建中转厂商", func() (int, []byte) {
		return a(http.MethodPut, "/api/v1/admin/models/providers", map[string]any{
			"code": "myrelay", "name": "我的中转", "adapterType": "configurable_http", "baseUrl": "https://relay.example.com",
			"authType": "bearer", "apiKey": map[string]string{"action": "replace", "value": "sk-relay-abcdef123456"},
			"connectTimeoutMs": 5000, "requestTimeoutMs": 600000, "idleTimeoutMs": 120000, "maxRetries": 1,
			"maxConcurrent": 3, "status": "enabled",
		})
	})
	mustOK(t, "建中转模型", func() (int, []byte) {
		return a(http.MethodPut, "/api/v1/admin/models/", map[string]any{
			"code": "relay_video", "name": "中转视频", "providerCode": "myrelay", "domain": "video",
			"upstreamModelCode": "seedance-x", "apiProtocol": "openai_video", "submitPath": "/v1/videos",
			"queryPath": "/v1/videos/{taskId}", "estSeconds": 240, "status": "disabled", "sort": 7,
			"maxDurationSec": 12, "hasNativeAudio": true,
		})
	})
	mustOK(t, "推荐绑定", func() (int, []byte) {
		return a(http.MethodPost, "/api/v1/admin/model-templates/recommend-bindings/relay_video", nil)
	})
	mustOK(t, "参数覆盖", func() (int, []byte) {
		return a(http.MethodPut, "/api/v1/admin/models/relay_video/param-supports", map[string]any{
			"paramCode": "duration", "allowedValues": []string{"5", "10"}, "defaultValue": "5",
		})
	})
	mustOK(t, "启用中转模型", func() (int, []byte) {
		return a(http.MethodPut, "/api/v1/admin/models/", map[string]any{
			"code": "relay_video", "name": "中转视频", "providerCode": "myrelay", "domain": "video",
			"upstreamModelCode": "seedance-x", "apiProtocol": "openai_video", "submitPath": "/v1/videos",
			"queryPath": "/v1/videos/{taskId}", "estSeconds": 240, "status": "enabled", "sort": 7,
		})
	})
	mustOK(t, "S3 设置", func() (int, []byte) {
		return a(http.MethodPut, "/api/v1/settings", map[string]any{
			"storage.s3.endpoint": "https://s3.example.com", "storage.s3.bucket": "b1",
			"storage.s3.access_key": "AK1", "storage.s3.secret_key": "SK-secret-1", "storage.s3.prefix": "p/",
		})
	})

	code, exported := a(http.MethodGet, "/api/v1/admin/model-config/export", nil)
	if code != http.StatusOK {
		t.Fatalf("导出：HTTP %d %s", code, exported)
	}
	for _, want := range []string{"sk-openai-1234567890", "sk-relay-abcdef123456", "SK-secret-1", `"kind":"model-config"`} {
		if !bytes.Contains(exported, []byte(want)) {
			t.Fatalf("导出文件里应有 %s", want)
		}
	}
	rtA.Close(context.Background())

	rtB, b := bootForTest(t)
	defer rtB.Close(context.Background())

	// 形状不对：整次拒绝，什么都不写。
	bad := mustOK(t, "错误文件", func() (int, []byte) {
		return b(http.MethodPost, "/api/v1/admin/model-config/import", map[string]any{"format": "tabtv.settings", "kind": "skills", "version": 1})
	})
	if p, _ := bad["problems"].([]any); len(p) == 0 {
		t.Fatalf("错误的 kind 应报问题：%v", bad)
	}

	preview := mustOK(t, "预览", func() (int, []byte) {
		return b(http.MethodPost, "/api/v1/admin/model-config/import?dryRun=true", exported)
	})
	if p, _ := preview["problems"].([]any); len(p) > 0 {
		t.Fatalf("预览不应有问题：%v", p)
	}
	actions := map[string]string{}
	for _, it := range preview["providers"].([]any) {
		m := it.(map[string]any)
		actions[m["code"].(string)] = m["action"].(string)
	}
	if actions["openai"] != "update" || actions["myrelay"] != "create" {
		t.Fatalf("预览动作不对：%v", actions)
	}
	// 预览不写库。
	_, again := b(http.MethodGet, "/api/v1/admin/model-config/export", nil)
	if bytes.Contains(again, []byte("myrelay")) {
		t.Fatal("预览不应写入任何东西")
	}

	rep := mustOK(t, "导入", func() (int, []byte) {
		return b(http.MethodPost, "/api/v1/admin/model-config/import", exported)
	})
	for _, group := range []string{"providers", "models"} {
		for _, it := range rep[group].([]any) {
			m := it.(map[string]any)
			if m["ok"] != true || m["warnings"] != nil {
				t.Errorf("%s %v：%v %v", group, m["code"], m["error"], m["warnings"])
			}
		}
	}
	if rep["restartRequired"] != true {
		t.Errorf("写了 S3 设置应提示重启：%v", rep)
	}

	_, exportedB := b(http.MethodGet, "/api/v1/admin/model-config/export", nil)
	if got, want := normalizeExport(t, exportedB), normalizeExport(t, exported); got != want {
		t.Fatalf("B 机导出与 A 机不一致：\nA=%s\nB=%s", want, got)
	}

	// 再导一次：幂等，全部成功。
	rep2 := mustOK(t, "重复导入", func() (int, []byte) {
		return b(http.MethodPost, "/api/v1/admin/model-config/import", exported)
	})
	for _, it := range rep2["models"].([]any) {
		if m := it.(map[string]any); m["ok"] != true || m["action"] != "update" {
			t.Errorf("重复导入 %v：%v", m["code"], m)
		}
	}
}

// normalizeExport 去掉导出时间，按 JSON 重排后比较。
func normalizeExport(t *testing.T, raw []byte) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("导出不是 JSON：%v", err)
	}
	delete(m, "exportedAt")
	b, _ := json.MarshalIndent(m, "", " ")
	return strings.TrimSpace(string(b))
}
