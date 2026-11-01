package tools

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

// 不做项绊线（MCPT-07，MCP-F-90 / MCP-F-91 / MCP-AC-21）。
//
// MCP 明确不提供：充值、购买会员、退款、删除项目、修改成员权限、修改模型 / 价格配置，
// 也不做动态工具发现。这些事要回 seeAitv 页面做（将来走 URL 模式 elicitation 带用户过去）。
// 今后有人加了这类工具，这里当场红 —— 而不是等到一个外部客户端把团队的钱退走了才发现。
//
// 检查对象是**运行时注册表**（NewRegistry，与 cmd/api 同一个构造函数）与**对外快照**
// api/mcp/tools.json（「对外暴露了什么」的单一事实来源）两者。
//
// ⚠️ 黑名单是语义近似，会误伤（例如将来的 tasks.cancel 不是删除）。误伤时在 allowNames /
// allowDescriptions 里**显式加白名单并写理由**，不要放宽黑名单。

// forbiddenTokens 是名字按 `_` / `.` 切开后**整词**命中即拒的词（含复数）。
// 整词而不是子串：子串的话 border / reorder 都会撞上 order。
var forbiddenTokens = []string{
	// 充值 / 购买 / 支付
	"recharge", "topup", "purchase", "buy", "order", "checkout", "pay", "payment", "invoice",
	"subscribe", "subscription", "membership",
	// 退款 / 调账
	"refund", "adjust", "withdraw",
	// 删除项目
	"delete", "purge", "destroy", "trash",
	// 修改成员权限
	"role", "member", "permission", "invite", "kick", "transfer",
	// 修改模型 / 价格配置
	"pricing", "price",
}

// forbiddenPhrases 是名字里出现即拒的组合（跨词）。
var forbiddenPhrases = []string{"top_up", "price_config", "model_config", "set_price", "set_role"}

// discoveryMarkers 是动态工具发现类工具的名字特征（MCP-F-91：工具清单是静态的、按凭据过滤，
// 不提供「让模型自己开工具集 / 搜工具」的元工具）。
var discoveryMarkers = []string{"toolset", "discover", "enable_", "disable_", "list_tools", "search_tool",
	"load_tool", "tool_search", "find_tool"}

// forbiddenDescPhrases 是描述（工具、method 枚举与 schema 里全部 description）里不得出现的说法。
var forbiddenDescPhrases = []string{
	"充值", "购买", "下单", "订单", "退款", "调账", "删除项目", "清空回收站",
	"成员权限", "修改成员", "移除成员", "修改角色", "价格配置", "修改价格", "修改模型配置",
	"refund", "recharge", "purchase",
}

// allowNames / allowDescriptions：显式白名单（名字或短语 → 理由）。目前为空。
var (
	allowNames        = map[string]string{}
	allowDescriptions = map[string]string{}
)

func nameTokens(name string) []string {
	return strings.FieldsFunc(strings.ToLower(name), func(r rune) bool { return r == '_' || r == '.' })
}

// forbiddenName 返回名字命中的黑名单词（不命中返回空串）。
func forbiddenName(name string) string {
	if _, ok := allowNames[name]; ok {
		return ""
	}
	lower := strings.ToLower(name)
	for _, p := range forbiddenPhrases {
		if strings.Contains(lower, p) {
			return p
		}
	}
	for _, tok := range nameTokens(lower) {
		for _, w := range forbiddenTokens {
			if tok == w || tok == w+"s" || tok == w+"es" {
				return w
			}
		}
	}
	return ""
}

func discoveryName(name string) string {
	lower := strings.ToLower(name)
	for _, m := range discoveryMarkers {
		if strings.Contains(lower, m) {
			return m
		}
	}
	return ""
}

// snapshotFile 读对外快照 api/mcp/tools.json。
func snapshotFile(t *testing.T) []app.SnapshotTool {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "api", "mcp", "tools.json"))
	if err != nil {
		t.Fatal(err)
	}
	var out []app.SnapshotTool
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// exposedNames 是运行时注册表与快照里的全部工具名与 `工具.method`。
// 两边都查：快照落后（没跑 make gen）时运行时那一侧仍然拦得住，反之亦然。
func exposedNames(t *testing.T) []string {
	t.Helper()
	var names []string
	for _, tl := range NewRegistry(Deps{}).List() {
		names = append(names, tl.Name)
		for _, m := range tl.Methods {
			if m.Name != "" {
				names = append(names, m.Name, tl.Name+"."+m.Name)
			}
		}
	}
	snap := snapshotFile(t)
	if len(snap) == 0 {
		t.Fatal("快照为空：绊线形同虚设")
	}
	for _, st := range snap {
		names = append(names, st.Name)
		for _, m := range st.Methods {
			if m.Name != "" {
				names = append(names, m.Name, st.Name+"."+m.Name)
			}
		}
	}
	return names
}

func TestForbidden_工具与method不含不做项(t *testing.T) {
	names := exposedNames(t)
	for _, n := range names {
		if w := forbiddenName(n); w != "" {
			t.Errorf("对外暴露了不做项 %q（命中 %q，MCP-F-90）：充值 / 购买会员 / 退款 / 删除项目 / 改成员权限 / "+
				"改模型与价格配置只在 seeAitv 页面做；确属误伤请在 allowNames 显式加白名单并写理由", n, w)
		}
	}
	// 自检：判定本身拦得下典型名字、放得过现有的与近似但无害的名字。
	for _, bad := range []string{"refund", "orders", "billing_refund", "projects.delete", "set_member_role",
		"price_config", "topup", "top_up", "members", "purchase_membership", "model_config", "canvas_purge"} {
		if forbiddenName(bad) == "" {
			t.Errorf("判定应拦下 %q", bad)
		}
	}
	for _, ok := range []string{"quote", "models", "image_tools", "tasks", "tasks.cancel", "canvas_read",
		"border", "reorder", "whoami", "media.get", "projects.create"} {
		if w := forbiddenName(ok); w != "" {
			t.Errorf("判定不该拦 %q（命中 %q）", ok, w)
		}
	}
}

// collectDescriptions 递归取出 JSON 里全部 "description" 字符串。
func collectDescriptions(v any, out *[]string) {
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			if s, ok := val.(string); ok && k == "description" {
				*out = append(*out, s)
				continue
			}
			collectDescriptions(val, out)
		}
	case []any:
		for _, val := range x {
			collectDescriptions(val, out)
		}
	}
}

func TestForbidden_描述不含不做项关键词(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "api", "mcp", "tools.json"))
	if err != nil {
		t.Fatal(err)
	}
	var raw any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	var descs []string
	collectDescriptions(raw, &descs)
	// 运行时那一侧：工具描述 + 带 method 枚举的入参 schema。
	snap, err := app.Snapshot(NewRegistry(Deps{}))
	if err != nil {
		t.Fatal(err)
	}
	var rt any
	if err := json.Unmarshal(snap, &rt); err != nil {
		t.Fatal(err)
	}
	collectDescriptions(rt, &descs)
	if len(descs) < 20 {
		t.Fatalf("只取到 %d 条描述：绊线形同虚设", len(descs))
	}
	for _, d := range descs {
		lower := strings.ToLower(d)
		for _, p := range forbiddenDescPhrases {
			if _, ok := allowDescriptions[p]; ok {
				continue
			}
			if strings.Contains(lower, p) {
				t.Errorf("描述里出现不做项 %q（MCP-F-90）：%s", p, d)
			}
		}
	}
}

func TestForbidden_无动态工具发现(t *testing.T) {
	for _, n := range exposedNames(t) {
		if m := discoveryName(n); m != "" {
			t.Errorf("出现动态工具发现类工具 %q（命中 %q，MCP-F-91）：工具清单按凭据静态过滤，不提供元工具", n, m)
		}
	}
	for _, bad := range []string{"enable_toolset", "discover_tools", "list_tools", "toolsets.enable", "search_tools"} {
		if discoveryName(bad) == "" {
			t.Errorf("判定应拦下 %q", bad)
		}
	}
	// 服务端（真实注册表）不声明 tools.listChanged：工具列表在会话中途不会变。
	cs := connectTools(t, Deps{}, collab.RoleOwner)
	caps := cs.InitializeResult().Capabilities
	if caps == nil || caps.Tools == nil || caps.Tools.ListChanged {
		t.Fatalf("tools 能力应声明且 listChanged=false：%+v", caps)
	}
	// 快照与运行时同名同序（快照落后时先 make gen）。
	var a, b []string
	for _, st := range snapshotFile(t) {
		a = append(a, st.Name)
	}
	for _, tl := range NewRegistry(Deps{}).List() {
		b = append(b, tl.Name)
	}
	if !slices.Equal(a, b) {
		t.Fatalf("api/mcp/tools.json 与运行时注册表不一致（先 make gen）：%v vs %v", a, b)
	}
}

// 真实注册表：持有全部 scope 的 owner 连 /mcp/readonly，列出来的每个 method 都是只读的（MCP-AC-02）；
// 快照里标了 readOnly 的 method 都看得到，没标的一个都看不到。
func TestMCPReadonly_真实注册表只暴露只读method(t *testing.T) {
	p := app.Principal{UserID: 1, WorkspaceID: 2, GrantID: 10, ClientName: "t", Toolsets: domain.Toolsets(),
		Scopes: domain.Scopes(), Role: collab.RoleOwner}
	r := chi.NewRouter()
	r.Use(httpx.TraceID)
	mcphttp.NewHandler(mcphttp.Options{Registry: NewRegistry(Deps{}), Auth: oneAuth{p},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}).Mount(r)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "mcpt07", Version: "0"}, nil).Connect(ctx,
		&mcp.StreamableClientTransport{Endpoint: srv.URL + "/mcp/readonly", MaxRetries: -1,
			// 请求头开启全部工具集：默认关闭的工具集（MCPT-16 的 script / workflow / timeline）也要逐 method 核对。
			HTTPClient: &http.Client{Transport: allToolsetsRT{}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	res, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	listed := map[string]*mcp.Tool{}
	for _, tl := range res.Tools {
		listed[tl.Name] = tl
	}
	for _, st := range snapshotFile(t) {
		tl, ok := listed[st.Name]
		var enum []string
		if ok {
			b, _ := json.Marshal(tl.InputSchema)
			var s struct {
				Properties map[string]struct {
					Enum []string `json:"enum"`
				} `json:"properties"`
			}
			_ = json.Unmarshal(b, &s)
			enum = s.Properties["method"].Enum
		}
		for _, m := range st.Methods {
			visible := ok && (m.Name == "" || slices.Contains(enum, m.Name))
			if visible != m.ReadOnly {
				t.Errorf("/mcp/readonly 上 %s.%s：可见=%v，快照 readOnly=%v", st.Name, m.Name, visible, m.ReadOnly)
			}
			if m.ReadOnly && (m.Paid || m.Write) {
				t.Errorf("快照里 %s.%s 标了 readOnly 却会写 / 花钱", st.Name, m.Name)
			}
		}
	}
}
