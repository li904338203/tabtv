package tools

import (
	"context"
	"encoding/json"
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

	collab "github.com/li904338203/tabtv/backend/internal/collaboration/domain"
	"github.com/li904338203/tabtv/backend/internal/mcp/app"
	"github.com/li904338203/tabtv/backend/internal/mcp/domain"
	mcphttp "github.com/li904338203/tabtv/backend/internal/mcp/transport"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// 多段工具集、资源与提示词（MCPT-16）的协议层：官方 SDK 客户端对真实 /mcp 路由。

type recCalls struct{ entries []app.CallEntry }

func (r *recCalls) Record(e app.CallEntry) { r.entries = append(r.entries, e) }

func connectAs(t *testing.T, d Deps, p app.Principal, path string, rt http.RoundTripper, calls mcphttp.CallRecorder) *mcp.ClientSession {
	t.Helper()
	r := chi.NewRouter()
	r.Use(httpx.TraceID)
	mcphttp.NewHandler(mcphttp.Options{Registry: NewRegistry(d), Auth: oneAuth{p}, Calls: calls,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}).Mount(r)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "mcpt16", Version: "0"}, nil).Connect(ctx,
		&mcp.StreamableClientTransport{Endpoint: srv.URL + path, MaxRetries: -1, HTTPClient: &http.Client{Transport: rt}},
		&mcp.ClientSessionOptions{ProtocolVersion: "2025-11-25"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func ownerAll() app.Principal {
	return app.Principal{UserID: 1, WorkspaceID: 2, GrantID: 10, ClientName: "t", Toolsets: domain.Toolsets(),
		Scopes: domain.Scopes(), Role: collab.RoleOwner}
}

func listNames(t *testing.T, cs *mcp.ClientSession) []string {
	t.Helper()
	lr, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, tl := range lr.Tools {
		out = append(out, tl.Name)
	}
	return out
}

// 三个工具集默认不可见；开了工具集后在只读端点上只剩只读 method（付费与写方法不可见）。
func TestMultiToolsets_默认不可见且只读端点只剩只读method(t *testing.T) {
	cs := connectAs(t, fakeDeps(), ownerAll(), "/mcp", bearerRT{}, nil)
	names := listNames(t, cs)
	for _, n := range []string{"script", "workflow", "timeline"} {
		if slices.Contains(names, n) {
			t.Errorf("%s 默认不应可见：%v", n, names)
		}
	}
	cs = connectAs(t, fakeDeps(), ownerAll(), "/mcp/x/workflow", bearerRT{}, nil)
	if names := listNames(t, cs); !slices.Contains(names, "workflow") {
		t.Fatalf("/mcp/x/workflow 应可见 workflow：%v", names)
	}
	cs = connectAs(t, fakeDeps(), ownerAll(), "/mcp/readonly", allToolsetsRT{}, nil)
	lr, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tl := range lr.Tools {
		if tl.Name != "workflow" && tl.Name != "script" && tl.Name != "timeline" {
			continue
		}
		raw := tl.InputSchema
		b, _ := jsonMarshal(raw)
		for _, bad := range []string{`"run"`, `"cancel"`, `"storyboard"`, `"render"`, `"assemble"`, `"create"`} {
			if strings.Contains(b, bad) {
				t.Errorf("/mcp/readonly 上 %s 不应暴露 %s：%s", tl.Name, bad, b)
			}
		}
		if tl.Annotations == nil || !tl.Annotations.ReadOnlyHint {
			t.Errorf("/mcp/readonly 上 %s 应是只读注解", tl.Name)
		}
	}
}

// 资源：模板按工具集过滤；读画布 / 文档 / 媒体走同一份投影，读资源进调用审计。
func TestResources_模板过滤与读取并进审计(t *testing.T) {
	rec := &recCalls{}
	cs := connectAs(t, fakeDeps(), ownerAll(), "/mcp", allToolsetsRT{}, rec)
	ctx := context.Background()
	tr, err := cs.ListResourceTemplates(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var uris []string
	for _, x := range tr.ResourceTemplates {
		uris = append(uris, x.URITemplate)
	}
	for _, want := range []string{resourceCanvas, resourceDocument, resourceMedia} {
		if !slices.Contains(uris, want) {
			t.Errorf("缺模板 %s：%v", want, uris)
		}
	}
	for _, uri := range []string{"seeaitv://projects/11/canvas", "seeaitv://projects/11/documents/5", "seeaitv://media/77"} {
		rr, err := cs.ReadResource(ctx, &mcp.ReadResourceParams{URI: uri})
		if err != nil {
			t.Fatalf("%s：%v", uri, err)
		}
		if len(rr.Contents) != 1 || rr.Contents[0].URI != uri || rr.Contents[0].MIMEType != "application/json" ||
			!strings.HasPrefix(rr.Contents[0].Text, "{") {
			t.Fatalf("%s：%+v", uri, rr.Contents)
		}
	}
	if _, err := cs.ReadResource(ctx, &mcp.ReadResourceParams{URI: "seeaitv://nope/1"}); err == nil {
		t.Fatal("不认识的 URI 应报错")
	}
	n := 0
	for _, e := range rec.entries {
		if e.Tool == "resources/read" {
			n++
		}
	}
	if n != 4 {
		t.Fatalf("读资源应进审计（4 次，含失败的那次）：%d %+v", n, rec.entries)
	}

	// 只开 generation 工具集：画布 / 文档模板不可见、读画布被拒（与工具同一个判定）。
	rec2 := &recCalls{}
	cs = connectAs(t, fakeDeps(), ownerAll(), "/mcp/x/generation", bearerRT{}, rec2)
	tr, err = cs.ListResourceTemplates(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(tr.ResourceTemplates) != 1 || tr.ResourceTemplates[0].URITemplate != resourceMedia {
		t.Fatalf("只应看到媒体模板：%+v", tr.ResourceTemplates)
	}
	if _, err := cs.ReadResource(ctx, &mcp.ReadResourceParams{URI: "seeaitv://projects/11/canvas"}); err == nil ||
		!strings.Contains(err.Error(), "canvas") {
		t.Fatalf("应拒绝读画布：%v", err)
	}
	if len(rec2.entries) != 1 || rec2.entries[0].Result != domain.CallDenied || rec2.entries[0].Method != "canvas" {
		t.Fatalf("被拒的读取应记 denied：%+v", rec2.entries)
	}
}

func TestPrompts_协议列出与取回(t *testing.T) {
	cs := connectAs(t, fakeDeps(), ownerAll(), "/mcp", bearerRT{}, nil)
	ctx := context.Background()
	lr, err := cs.ListPrompts(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(lr.Prompts) != 1 || lr.Prompts[0].Name != "video_single" {
		t.Fatalf("%+v", lr.Prompts)
	}
	gr, err := cs.GetPrompt(ctx, &mcp.GetPromptParams{Name: "video_single"})
	if err != nil {
		t.Fatal(err)
	}
	tc, ok := gr.Messages[0].Content.(*mcp.TextContent)
	if !ok || !strings.Contains(tc.Text, "canvas_read") || strings.Contains(tc.Text, "{{") {
		t.Fatalf("%+v", gr.Messages[0].Content)
	}
	if _, err := cs.GetPrompt(ctx, &mcp.GetPromptParams{Name: "nope"}); err == nil {
		t.Fatal("不存在的提示词应报错")
	}
}

func jsonMarshal(v any) (string, error) {
	b, err := json.Marshal(v)
	return string(b), err
}
