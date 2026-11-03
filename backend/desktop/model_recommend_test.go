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

// 自建模型「推荐配置」（2026-11-03 用户反馈：新建模型直接打开启用被拒——至少需要绑定并启用一个操作）：
// 建一个停用的自建文本模型 → 推荐配置 → 能直接启用；再点一次不重复绑；没勾 Agent 时不绑 agent_chat。
func TestRecommendBindings_自建文本模型推荐后可启用(t *testing.T) {
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
		rec := httptest.NewRecorder()
		rt.Handler.ServeHTTP(rec, httptest.NewRequest(method, path, rd))
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	// 厂商要先就绪（有 Key）：用 openai 模板厂商。
	if code, out := call(http.MethodPost, "/api/v1/admin/model-templates/openai/enable",
		map[string]any{"apiKey": map[string]string{"action": "replace", "value": "sk-test-1234567890"}}); code != http.StatusOK {
		t.Fatalf("厂商启用失败：%d %v", code, out)
	}
	model := map[string]any{
		"code": "my_chat", "name": "My Chat", "providerCode": "openai", "domain": "text",
		"upstreamModelCode": "gpt-x", "apiProtocol": "openai_chat", "submitPath": "/chat/completions",
		"estSeconds": 30, "sort": 100, "status": "disabled", "agentCapable": false,
	}
	if code, out := call(http.MethodPut, "/api/v1/admin/models/", model); code >= 300 {
		t.Fatalf("建模型失败：%d %v", code, out)
	}
	// 直接启用：被拒（这正是用户撞上的那一下）。
	model["status"] = "enabled"
	if code, _ := call(http.MethodPut, "/api/v1/admin/models/", model); code < 300 {
		t.Fatal("没绑操作时启用应被拒")
	}

	code, out := call(http.MethodPost, "/api/v1/admin/model-templates/recommend-bindings/my_chat", nil)
	bound, _ := out["bound"].([]any)
	if code != http.StatusOK || len(bound) == 0 {
		t.Fatalf("推荐配置失败：%d %v", code, out)
	}
	has := map[string]bool{}
	for _, b := range bound {
		has[b.(string)] = true
	}
	if !has["text2text"] || has["agent_chat"] {
		t.Fatalf("应绑 text2text、没勾 Agent 时不绑 agent_chat：%v", bound)
	}
	if code, out := call(http.MethodPut, "/api/v1/admin/models/", model); code >= 300 {
		t.Fatalf("推荐配置后应能启用：%d %v", code, out)
	}
	// 再点一次：全部跳过，不重复绑。
	_, out = call(http.MethodPost, "/api/v1/admin/model-templates/recommend-bindings/my_chat", nil)
	if b, _ := out["bound"].([]any); len(b) != 0 {
		t.Fatalf("已绑过的应跳过：%v", out)
	}
	if code, _ := call(http.MethodPost, "/api/v1/admin/model-templates/recommend-bindings/nope", nil); code < 300 && code != 0 {
		t.Fatal("不存在的模型应报错")
	}
}
