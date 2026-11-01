package transport

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/li904338203/tabtv/backend/internal/mcp/app"
	"github.com/li904338203/tabtv/backend/internal/mcp/domain"
)

// TestClientSnippets_端点只读且不含密钥：GET /me/mcp/client-snippets 的形状与契约一致，
// 地址来自公网配置、工具集可用性来自注册表，响应里没有任何密钥形态。
func TestClientSnippets_端点只读且不含密钥(t *testing.T) {
	h := NewKeysHandler(nil, func(context.Context) (int64, bool) { return 1, true })
	h.SetClientSnippets("https://mcp.example.com/mcp",
		app.ToolsetAvailability([]*app.Tool{{Name: "whoami", Toolset: domain.ToolsetContext}}))
	r := chi.NewRouter()
	h.Mount(r, func(next http.Handler) http.Handler { return next })

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/me/mcp/client-snippets", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "sk_mcp_") {
		t.Fatalf("响应里出现了密钥形态：%s", rec.Body.String())
	}
	var body struct {
		MCPURL         string `json:"mcpUrl"`
		URLConfigured  bool   `json:"mcpUrlConfigured"`
		KeyPlaceholder string `json:"keyPlaceholder"`
		Toolsets       []struct {
			Name      string `json:"name"`
			Available bool   `json:"available"`
		} `json:"toolsets"`
		Items []struct {
			Client  string `json:"client"`
			Content string `json:"content"`
			Modes   []struct {
				Mode        string  `json:"mode"`
				Supported   bool    `json:"supported"`
				Format      *string `json:"format"`
				Content     string  `json:"content"`
				AgentPrompt string  `json:"agentPrompt"`
				Note        string  `json:"note"`
			} `json:"modes"`
		} `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.MCPURL != "https://mcp.example.com/mcp" || !body.URLConfigured || body.KeyPlaceholder != app.KeyPlaceholder {
		t.Fatalf("头部字段不对：%+v", body)
	}
	if len(body.Items) != len(app.SnippetClients()) {
		t.Fatalf("片段数 = %d", len(body.Items))
	}
	for _, ts := range body.Toolsets {
		if ts.Available != (ts.Name == domain.ToolsetContext) {
			t.Errorf("%s available = %v", ts.Name, ts.Available)
		}
	}
	// 改版（`/mcp` 接入指南）：每个客户端都有 modes，必有可用的 key；不支持的那一项不带 format、只有说明。
	for _, it := range body.Items {
		hasKey := false
		for _, m := range it.Modes {
			if m.Mode == app.ModeKey && m.Supported && m.AgentPrompt != "" {
				hasKey = true
			}
			if !m.Supported && (m.Format != nil || m.Content != "" || m.AgentPrompt != "" || m.Note == "") {
				t.Errorf("%s/%s 不支持时应只有 note：%+v", it.Client, m.Mode, m)
			}
		}
		if !hasKey {
			t.Errorf("%s 缺少可用的 key 方式", it.Client)
		}
	}
}
