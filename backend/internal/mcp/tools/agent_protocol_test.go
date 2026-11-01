package tools

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	mcphttp "github.com/li904338203/tabtv/backend/internal/mcp/transport"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// 委托站内 Agent（MCPT-17）的协议层：官方 SDK 客户端对真实 /mcp 路由。

// connectAgent 以发起人本人（UserID 1）+ 全部 scope 连 /mcp/x/agent；elicit 非空时客户端声明 elicitation。
func connectAgent(t *testing.T, d Deps, version, path string,
	elicit func(context.Context, *mcp.ElicitRequest) (*mcp.ElicitResult, error)) *mcp.ClientSession {
	t.Helper()
	p := ownerAll()
	p.UserID, p.PayerAccountID, p.GrantID = 1, 3, 5
	r := chi.NewRouter()
	r.Use(httpx.TraceID)
	mcphttp.NewHandler(mcphttp.Options{Registry: NewRegistry(d), Auth: oneAuth{p},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}).Mount(r)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var opts *mcp.ClientOptions
	if elicit != nil {
		opts = &mcp.ClientOptions{ElicitationHandler: elicit}
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "mcpt17", Version: "0"}, opts).Connect(ctx,
		&mcp.StreamableClientTransport{Endpoint: srv.URL + path, MaxRetries: -1,
			HTTPClient: &http.Client{Transport: bearerRT{}}},
		&mcp.ClientSessionOptions{ProtocolVersion: version})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

// elicitation 客户端批准 / 拒绝规划确认（两个协议版本）：拒绝不提交、quote 未用；接受提交一次 approve。
func TestMCPAgent_elicitation批准与拒绝规划确认(t *testing.T) {
	for _, v := range []string{"2025-11-25", "2026-07-28"} {
		for _, action := range []string{"decline", "accept"} {
			t.Run(v+"/"+action, func(t *testing.T) {
				d, ag, store := agentDeps(100)
				ag.addCard(planCard(31, 30, false))
				var asked *mcp.ElicitParams
				cs := connectAgent(t, d, v, "/mcp/x/agent", func(_ context.Context, r *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
					asked = r.Params
					if action == "accept" {
						return &mcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}, nil
					}
					return &mcp.ElicitResult{Action: "decline"}, nil
				})
				res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "agent", Arguments: map[string]any{
					"method": "submit_card", "sessionId": "21", "cardId": "31", "decision": "approve"}})
				if err != nil {
					t.Fatal(err)
				}
				if asked == nil {
					t.Fatal("客户端应收到确认表单")
				}
				for _, s := range []string{"批准站内 Agent 的规划", "30 积分", "当前余额 100", "确认后剩余 70"} {
					if !strings.Contains(asked.Message, s) {
						t.Errorf("表单缺「%s」：%s", s, asked.Message)
					}
				}
				m := structured(t, res)
				if action == "decline" {
					if m["status"] != "declined" || ag.submitCount() != 0 || store.rows[0].UsedAt != nil {
						t.Fatalf("拒绝后不应提交、quote 未用：%v", m)
					}
					return
				}
				if m["credits"] != float64(30) || ag.submitCount() != 1 || store.rows[0].UsedAt == nil {
					t.Fatalf("接受后应提交一次 approve：%v", m)
				}
			})
		}
	}
}

// 工具集默认关闭：/mcp 上看不到 agent；/mcp/x/agent 上看得到；站内 Agent 关闭时连 /mcp/x/agent 也没有。
func TestMCPAgent_默认关闭且站内关闭时不可见(t *testing.T) {
	d, _, _ := agentDeps(100)
	if names := listNames(t, connectAgent(t, d, "2025-11-25", "/mcp", nil)); slices.Contains(names, "agent") {
		t.Fatalf("agent 工具集默认关闭：%v", names)
	}
	if names := listNames(t, connectAgent(t, d, "2025-11-25", "/mcp/x/agent", nil)); !slices.Equal(names, []string{"agent"}) {
		t.Fatalf("/mcp/x/agent 应只见 agent：%v", names)
	}
	d.AgentDisabled = true
	if names := listNames(t, connectAgent(t, d, "2025-11-25", "/mcp/x/agent", nil)); len(names) != 0 {
		t.Fatalf("站内 Agent 关闭时不应有 agent：%v", names)
	}
	// 只读端点上只剩 get：由 TestMCPReadonly_真实注册表只暴露只读method 逐 method 覆盖（请求头开全部工具集）。
}
