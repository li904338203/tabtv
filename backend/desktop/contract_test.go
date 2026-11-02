package desktop

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"gopkg.in/yaml.v3"

	"github.com/li904338203/tabtv/backend/internal/platform/resources"
)

// 契约与真实路由逐条对照（TT-T-10）。
//
// ⚠️ `make gen-check` 只比「契约与生成物一致」，抓不到两类缺口：
//   - 端点挂上了却不在契约里（前端拿不到生成类型，只能手写 DTO —— 来源系统有过 12 组）；
//   - 契约里的端点已经没有路由（被裁剪的域：前端照着生成类型调过去，拿到的是 404）。
//
// 这条测试把两边都列出来比。新增端点时同步登记契约；确实不该进契约的写进下面的白名单并说明原因。

// notInContract 是有意不进契约的路由。
var notInContract = map[string]string{
	"GET /health": "探活，不是业务接口",
	// 宿主专用：由 app.SaveMiddleware 在 Wails 资源服务里截获，不经过后端路由；这里列出只为说明。
}

var paramRE = regexp.MustCompile(`\{[^}]*\}`)

func normRoute(method, path string) string {
	path = strings.TrimSuffix(paramRE.ReplaceAllString(path, "{}"), "/")
	return method + " " + path
}

func contractRoutes(t *testing.T) map[string]bool {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "api", "openapi.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Paths map[string]map[string]any `yaml:"paths"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for p, ops := range doc.Paths {
		for m := range ops {
			switch m {
			case "get", "post", "put", "patch", "delete":
				out[normRoute(strings.ToUpper(m), p)] = true
			}
		}
	}
	return out
}

func TestContract_与真实路由一致(t *testing.T) {
	if _, err := resources.Postgres(); err != nil {
		t.Skipf("没有 PG 二进制：%v", err)
	}
	t.Setenv("TABTV_DATA_DIR", t.TempDir())
	rt, err := Boot(context.Background(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("启动失败：%v", err)
	}
	defer rt.Close(context.Background())

	mounted := map[string]bool{}
	err = chi.Walk(rt.Handler.(chi.Routes), func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if !strings.HasPrefix(route, "/api/v1/") {
			return nil
		}
		mounted[normRoute(method, strings.TrimPrefix(route, "/api/v1"))] = true
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	contract := contractRoutes(t)
	if out := os.Getenv("TABTV_DUMP_ROUTES"); out != "" {
		var list []string
		for r := range mounted {
			list = append(list, r)
		}
		sort.Strings(list)
		_ = os.WriteFile(out, []byte(strings.Join(list, "\n")+"\n"), 0o644)
	}

	var missing, dead []string
	for r := range mounted {
		if !contract[r] && notInContract[r] == "" {
			missing = append(missing, r)
		}
	}
	for r := range contract {
		if !mounted[r] {
			dead = append(dead, r)
		}
	}
	sort.Strings(missing)
	sort.Strings(dead)
	if len(missing) > 0 {
		t.Errorf("挂了路由却不在契约里（%d）：\n  %s", len(missing), strings.Join(missing, "\n  "))
	}
	if len(dead) > 0 {
		t.Errorf("契约里有、路由已不存在（%d）：\n  %s", len(dead), strings.Join(dead, "\n  "))
	}
}
