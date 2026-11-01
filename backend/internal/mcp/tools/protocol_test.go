package tools

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	collab "github.com/li904338203/tabtv/backend/internal/collaboration/domain"
	"github.com/li904338203/tabtv/backend/internal/mcp/app"
	"github.com/li904338203/tabtv/backend/internal/mcp/domain"
	mcphttp "github.com/li904338203/tabtv/backend/internal/mcp/transport"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// 协议层：用官方 SDK 客户端对真实的 /mcp 路由跑一遍 M1 只读工具（注册表 = 快照同源的 NewRegistry），
// 证明 outputSchema 校验、文本信封与图片内联在 SDK 那一侧也成立 —— 只测 handler 的话，
// 「结构化内容过不了 SDK 的 outputSchema 校验」这类问题只有接上真客户端才暴露。

// bearerRT 带上 Authorization：transport 在凭据中间件之后还要求 Bearer 头（把 Principal 透传给 SDK）。
type bearerRT struct{}

func (bearerRT) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer test")
	return http.DefaultTransport.RoundTrip(r)
}

// allToolsetsRT 在 bearerRT 之上用请求头开启**全部**工具集（MCPT-16 起有默认关闭的 script / workflow / timeline）：
// 凭据授予了全部工具集，但不带请求头时只开默认集 —— 不开的话这些工具在协议用例里根本不可见、绕开了检查。
type allToolsetsRT struct{}

func (allToolsetsRT) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set(domain.HeaderToolsets, strings.Join(domain.Toolsets(), ","))
	return bearerRT{}.RoundTrip(r)
}

type oneAuth struct{ p app.Principal }

func (a oneAuth) Authenticate(*http.Request) (app.Principal, error) { return a.p, nil }

func connectTools(t *testing.T, d Deps, role collab.Role) *mcp.ClientSession {
	t.Helper()
	p := app.Principal{UserID: 1, WorkspaceID: 2, GrantID: 10, ClientName: "t", Toolsets: domain.Toolsets(),
		Scopes: []string{domain.ScopeView}, Role: role}
	r := chi.NewRouter()
	r.Use(httpx.TraceID)
	mcphttp.NewHandler(mcphttp.Options{Registry: NewRegistry(d), Auth: oneAuth{p},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}).Mount(r)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "mcpt04", Version: "0"}, nil).Connect(ctx,
		&mcp.StreamableClientTransport{Endpoint: srv.URL + "/mcp", MaxRetries: -1,
			HTTPClient: &http.Client{Transport: allToolsetsRT{}}},
		&mcp.ClientSessionOptions{ProtocolVersion: "2025-11-25"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func TestMCPTools_协议端到端(t *testing.T) {
	cs := connectTools(t, fakeDeps(), collab.RoleViewer)
	lr, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tl := range lr.Tools {
		names = append(names, tl.Name)
	}
	// 只读的 viewer + 只有 view scope：M1 只读工具全部可见（默认工具集）。
	for _, want := range []string{"whoami", "projects", "canvas_read", "documents", "models", "image_tools",
		"tasks", "media"} {
		if !slices.Contains(names, want) {
			t.Errorf("tools/list 缺 %s：%v", want, names)
		}
	}
	for _, c := range allCalls {
		var args map[string]any
		_ = json.Unmarshal([]byte(c.args), &args)
		res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: c.tool, Arguments: args})
		if err != nil {
			t.Fatalf("%s %s：%v", c.tool, c.args, err)
		}
		if res.IsError {
			b, _ := json.Marshal(res.Content)
			t.Fatalf("%s %s 返回错误：%s", c.tool, c.args, b)
		}
		if res.StructuredContent == nil {
			t.Fatalf("%s 缺 structuredContent", c.tool)
		}
	}
	// media.get：PNG 附 image content；文本里仍有完整 JSON。
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "media",
		Arguments: map[string]any{"method": "get", "mediaId": "77"}})
	if err != nil {
		t.Fatal(err)
	}
	var img *mcp.ImageContent
	for _, c := range res.Content {
		if ic, ok := c.(*mcp.ImageContent); ok {
			img = ic
		}
	}
	if img == nil || img.MIMEType != "image/png" || !strings.HasPrefix(string(img.Data), "\x89PNG") {
		t.Fatalf("应内联 PNG 缩略图：%+v", img)
	}
}

// 端口报的业务错误（如别的工作区的媒体 → NOT_FOUND）原样交给客户端，不被吞成内部错误。
func TestMCPTools_业务错误原样返回(t *testing.T) {
	d := fakeDeps()
	d.Media = notFoundMedia{}
	cs := connectTools(t, d, collab.RoleOwner)
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "media",
		Arguments: map[string]any{"method": "get", "mediaId": "77"}})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatal("应是错误结果")
	}
	text := res.Content[0].(*mcp.TextContent).Text
	if !strings.Contains(text, "40401") {
		t.Fatalf("应带 NOT_FOUND 业务码：%s", text)
	}
}

type notFoundMedia struct{ fakeMedia }

func (notFoundMedia) Info(context.Context, app.Principal, int64) (app.MediaInfo, error) {
	return app.MediaInfo{}, httpx.Err(errcode.NotFound)
}

// ---------------------------------------------------------------- generate（MCPT-10）

// connectGen 以 owner + 全部 scope 连 /mcp；elicit 非空时客户端声明 elicitation（SDK 按是否给 handler 声明）。
func connectGen(t *testing.T, d Deps, version string,
	elicit func(context.Context, *mcp.ElicitRequest) (*mcp.ElicitResult, error)) *mcp.ClientSession {
	t.Helper()
	p := app.Principal{UserID: 1, WorkspaceID: 2, GrantID: 5, PayerAccountID: 3, ClientName: "t",
		Toolsets: domain.Toolsets(), Scopes: domain.Scopes(), Role: collab.RoleOwner}
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
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "mcpt10", Version: "0"}, opts).Connect(ctx,
		&mcp.StreamableClientTransport{Endpoint: srv.URL + "/mcp", MaxRetries: -1,
			HTTPClient: &http.Client{Transport: bearerRT{}}},
		&mcp.ClientSessionOptions{ProtocolVersion: version})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func structured(t *testing.T, res *mcp.CallToolResult) map[string]any {
	t.Helper()
	if res.IsError {
		b, _ := json.Marshal(res.Content)
		t.Fatalf("返回错误：%s", b)
	}
	b, _ := json.Marshal(res.StructuredContent)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	return m
}

var genCallArgs = map[string]any{"projectId": "11", "nodeType": "image", "operationCode": "text2img",
	"modelCode": "m", "prompt": "一只猫", "count": 2}

// 基线：不支持 elicitation 的客户端 → needs_confirmation → 带 confirmQuoteId + confirmCredits 重调 → queued；
// 再调一次 → replayed，只有一个任务。两个协议版本各跑一遍。
func TestMCP_基线确认会话(t *testing.T) {
	for _, v := range []string{"2025-11-25", "2026-07-28"} {
		t.Run(v, func(t *testing.T) {
			d, cg, _ := genDeps(100)
			cs := connectGen(t, d, v, nil)
			lr, err := cs.ListTools(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			var gt *mcp.Tool
			for _, tl := range lr.Tools {
				if tl.Name == "generate" {
					gt = tl
				}
			}
			if gt == nil || gt.Annotations.ReadOnlyHint || !gt.Annotations.IdempotentHint {
				t.Fatalf("generate 应可见且注解为花钱 + 幂等：%+v", gt)
			}
			res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "generate", Arguments: genCallArgs})
			if err != nil {
				t.Fatal(err)
			}
			m := structured(t, res)
			nc, _ := m["needsConfirmation"].(map[string]any)
			if m["status"] != "needs_confirmation" || nc == nil || cg.calls != 0 {
				t.Fatalf("应需确认、未受理：%v", m)
			}
			args := map[string]any{"confirmQuoteId": nc["quoteId"], "confirmCredits": nc["credits"]}
			for k, val := range genCallArgs {
				args[k] = val
			}
			for i := 0; i < 2; i++ {
				res, err = cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "generate", Arguments: args})
				if err != nil {
					t.Fatal(err)
				}
				m = structured(t, res)
				if m["generationId"] != "7001" || (m["replayed"] == true) != (i == 1) {
					t.Fatalf("第 %d 次：%v", i+1, m)
				}
			}
			if !strings.Contains(text(res), "不要重新提交") {
				t.Errorf("文本要说明后台任务语义：%s", text(res))
			}
			if cg.acceptedCount() != 1 {
				t.Fatalf("只应一个任务：%d", cg.acceptedCount())
			}
		})
	}
}

// elicitation：客户端声明了 elicitation → 收到表单（做什么、积分、付费账户、余额、确认后剩余）；
// 拒绝 → 不扣费、quote 保持未用；接受 → 受理。2025 由 SDK 代发 elicitation/create，2026 走 input_required。
func TestMCP_elicitation拒绝不扣费(t *testing.T) {
	for _, v := range []string{"2025-11-25", "2026-07-28"} {
		for _, action := range []string{"decline", "accept"} {
			t.Run(v+"/"+action, func(t *testing.T) {
				d, cg, store := genDeps(100)
				var asked *mcp.ElicitParams
				cs := connectGen(t, d, v, func(_ context.Context, r *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
					asked = r.Params
					if action == "accept" {
						return &mcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}, nil
					}
					return &mcp.ElicitResult{Action: "decline"}, nil
				})
				res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "generate", Arguments: genCallArgs})
				if err != nil {
					t.Fatal(err)
				}
				if asked == nil {
					t.Fatal("客户端应收到确认表单")
				}
				for _, s := range []string{"text2img", "20 积分", "付费账户", "当前余额 100", "确认后剩余 80"} {
					if !strings.Contains(asked.Message, s) {
						t.Errorf("表单缺「%s」：%s", s, asked.Message)
					}
				}
				m := structured(t, res)
				if action == "decline" {
					if m["status"] != "declined" || cg.calls != 0 || store.rows[0].UsedAt != nil {
						t.Fatalf("拒绝后不应受理、quote 未用：%v calls=%d", m, cg.calls)
					}
					if !strings.Contains(text(res), "未扣费") {
						t.Errorf("要说明未扣费：%s", text(res))
					}
					return
				}
				if m["status"] != "queued" || cg.acceptedCount() != 1 || store.rows[0].UsedAt == nil {
					t.Fatalf("接受后应受理一次：%v", m)
				}
			})
		}
	}
}

func text(r *mcp.CallToolResult) string {
	var parts []string
	for _, c := range r.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			parts = append(parts, tc.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// ---------------------------------------------------------------- 页面审批（MCPT-13）

// connectPage 以「花钱需在 seeAitv 页面确认」的凭据连 /mcp；url 为真时客户端声明 form + url 两种 elicitation。
func connectPage(t *testing.T, d Deps, version string, url bool,
	elicit func(context.Context, *mcp.ElicitRequest) (*mcp.ElicitResult, error)) *mcp.ClientSession {
	t.Helper()
	p := app.Principal{UserID: 1, WorkspaceID: 2, GrantID: 5, PayerAccountID: 3, ClientName: "t",
		Toolsets: domain.Toolsets(), Scopes: domain.Scopes(), Role: collab.RoleOwner, RequirePageConfirm: true}
	r := chi.NewRouter()
	r.Use(httpx.TraceID)
	mcphttp.NewHandler(mcphttp.Options{Registry: NewRegistry(d), Auth: oneAuth{p},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}).Mount(r)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	opts := &mcp.ClientOptions{ElicitationHandler: elicit}
	if url {
		opts.Capabilities = &mcp.ClientCapabilities{Elicitation: &mcp.ElicitationCapabilities{
			Form: &mcp.FormElicitationCapabilities{}, URL: &mcp.URLElicitationCapabilities{}}}
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "mcpt13", Version: "0"}, opts).Connect(ctx,
		&mcp.StreamableClientTransport{Endpoint: srv.URL + "/mcp", MaxRetries: -1,
			HTTPClient: &http.Client{Transport: bearerRT{}}},
		&mcp.ClientSessionOptions{ProtocolVersion: version})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func withArgs(extra map[string]any) map[string]any {
	args := map[string]any{}
	for k, v := range genCallArgs {
		args[k] = v
	}
	for k, v := range extra {
		args[k] = v
	}
	return args
}

// 页面确认 + url 模式：客户端收到的是审批页（url 模式，elicitationId = quoteId），**不是 form 表单**；
// 用户在客户端里点「接受」（打开了页面）不算同意 → 40913 带审批链接、不受理；页面批准之后带同一个
// quoteId 重调才受理，且只一次。两个协议版本各跑一遍（2025 由 SDK 代发 elicitation/create，2026 走 input_required）。
func TestMCP_页面确认url模式(t *testing.T) {
	for _, v := range []string{"2025-11-25", "2026-07-28"} {
		t.Run(v, func(t *testing.T) {
			d, cg, store := genDeps(100)
			d.Confirm.SetApprovalBase("https://app.example.com")
			var asked *mcp.ElicitParams
			cs := connectPage(t, d, v, true, func(_ context.Context, r *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
				asked = r.Params
				return &mcp.ElicitResult{Action: "accept"}, nil
			})
			res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "generate", Arguments: genCallArgs})
			if err != nil {
				t.Fatal(err)
			}
			if asked == nil || asked.Mode != "url" || asked.RequestedSchema != nil {
				t.Fatalf("应收到 url 模式的审批页：%+v", asked)
			}
			qid := strconv.FormatInt(store.rows[0].ID, 10)
			if asked.URL != "https://app.example.com/mcp/approve/"+qid || asked.ElicitationID != qid ||
				!strings.Contains(asked.Message, "20 积分") {
				t.Fatalf("审批页参数：%+v", asked)
			}
			if !res.IsError || !strings.Contains(text(res), "40913") || !strings.Contains(text(res), asked.URL) {
				t.Fatalf("未批准应 40913 并带审批链接：%s", text(res))
			}
			if cg.calls != 0 || store.rows[0].UsedAt != nil {
				t.Fatal("未批准不应受理、quote 未用")
			}
			now := t0
			store.rows[0].ApprovedAt = &now
			args := withArgs(map[string]any{"confirmQuoteId": qid, "confirmCredits": 20})
			for i := 0; i < 2; i++ {
				res, err = cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "generate", Arguments: args})
				if err != nil {
					t.Fatal(err)
				}
				if m := structured(t, res); m["generationId"] != "7001" || (m["replayed"] == true) != (i == 1) {
					t.Fatalf("第 %d 次：%v", i+1, m)
				}
			}
			if cg.acceptedCount() != 1 {
				t.Fatalf("只应一个任务：%d", cg.acceptedCount())
			}
		})
	}
}

// 页面确认 + 只声明了 form（或没声明 elicitation）的客户端：不弹 form 表单（表单里的「确认」不算同意），
// 回落到基线 needsConfirmation，里面带 approvalUrl；回显金额不受理。
func TestMCP_页面确认回落基线带审批链接(t *testing.T) {
	for _, v := range []string{"2025-11-25", "2026-07-28"} {
		t.Run(v, func(t *testing.T) {
			d, cg, store := genDeps(100)
			formAsked := false
			cs := connectPage(t, d, v, false, func(context.Context, *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
				formAsked = true
				return &mcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}, nil
			})
			res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "generate", Arguments: genCallArgs})
			if err != nil {
				t.Fatal(err)
			}
			m := structured(t, res)
			nc, _ := m["needsConfirmation"].(map[string]any)
			if formAsked || m["status"] != "needs_confirmation" || nc == nil ||
				!strings.HasSuffix(nc["approvalUrl"].(string), "/mcp/approve/"+nc["quoteId"].(string)) {
				t.Fatalf("应回落基线并给审批链接（不弹表单）：%v formAsked=%v", m, formAsked)
			}
			res, err = cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "generate",
				Arguments: withArgs(map[string]any{"confirmQuoteId": nc["quoteId"], "confirmCredits": nc["credits"]})})
			if err != nil {
				t.Fatal(err)
			}
			if !res.IsError || !strings.Contains(text(res), "40913") || cg.calls != 0 || store.rows[0].UsedAt != nil {
				t.Fatalf("回显金额不算同意：%s", text(res))
			}
		})
	}
}

// MCPT-15 实测：2025 会话里用户作答确认表单的时间算在单次调用上限里，超过时 SDK 回一条英文 JSON-RPC 错误。
// 现在翻成工具错误（isError + 中文原因、说明没有扣费）；且确实没有受理、quote 未用。
// ⚠️ 这条也钉住 SDK 错误文本里的 "input request"（deadlineMessage 靠它认出确认表单）。
func TestCallDeadline_等确认表单超时给可读错误(t *testing.T) {
	d, cg, store := genDeps(100)
	p := app.Principal{UserID: 1, WorkspaceID: 2, GrantID: 5, PayerAccountID: 3, ClientName: "t",
		Toolsets: domain.Toolsets(), Scopes: domain.Scopes(), Role: collab.RoleOwner}
	r := chi.NewRouter()
	r.Use(httpx.TraceID)
	mcphttp.NewHandler(mcphttp.Options{Registry: NewRegistry(d), Auth: oneAuth{p}, CallTimeout: 300 * time.Millisecond,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}).Mount(r)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	slow := func(ctx context.Context, _ *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
		select { // 用户迟迟不答
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
		return &mcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "mcpt15", Version: "0"},
		&mcp.ClientOptions{ElicitationHandler: slow}).Connect(ctx,
		&mcp.StreamableClientTransport{Endpoint: srv.URL + "/mcp", MaxRetries: -1,
			HTTPClient: &http.Client{Transport: bearerRT{}}},
		&mcp.ClientSessionOptions{ProtocolVersion: "2025-11-25"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "generate", Arguments: genCallArgs})
	if err != nil {
		t.Fatalf("应是工具错误而不是协议错误：%v", err)
	}
	if !res.IsError || !strings.Contains(text(res), "确认表单") || !strings.Contains(text(res), "没有扣费") ||
		strings.Contains(text(res), "multi-round-trip") {
		t.Fatalf("超时文案：%s", text(res))
	}
	if cg.calls != 0 || len(store.rows) != 1 || store.rows[0].UsedAt != nil {
		t.Fatalf("超时不应受理、quote 应未用：calls=%d rows=%d", cg.calls, len(store.rows))
	}
}
