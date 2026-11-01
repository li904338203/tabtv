package canvasgen

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// canvasgen 是 Agent 与 MCP 共用的编排（MCPT-08）：一旦 import 了 agent 的类型，MCP 用它就会把整个
// agent 域拖进来；import 了 mcp 则反过来成环。任何域的 repo / transport 也不许碰 —— 本包只编排，
// 落库与 HTTP 都在端口的另一侧。这条绊线读本包全部非测试文件的 import。
func TestCanvasgen_不依赖agent与mcp(t *testing.T) {
	const prefix = "github.com/li904338203/tabtv/backend/internal/"
	forbidden := func(path string) string {
		if !strings.HasPrefix(path, prefix) {
			return ""
		}
		rest := strings.TrimPrefix(path, prefix)
		switch {
		case rest == "agent" || strings.HasPrefix(rest, "agent/"):
			return "agent 域"
		case rest == "mcp" || strings.HasPrefix(rest, "mcp/"):
			return "mcp 域"
		case strings.HasSuffix(rest, "/repo") || strings.Contains(rest, "/repo/"):
			return "仓储层"
		case rest == "transport" || strings.HasPrefix(rest, "transport/") ||
			strings.HasSuffix(rest, "/transport") || strings.Contains(rest, "/transport/"):
			return "HTTP 层"
		}
		return ""
	}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	checked := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		af, err := parser.ParseFile(fset, f, src, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		checked++
		for _, im := range af.Imports {
			p, _ := strconv.Unquote(im.Path.Value)
			if why := forbidden(p); why != "" {
				t.Errorf("%s import 了 %s（%s）：canvasgen 只能依赖标准库与 collaboration/domain", f, p, why)
			}
		}
	}
	if checked == 0 {
		t.Fatal("一个源文件都没读到：绊线形同虚设")
	}
	// 自检：判定函数本身要能拦下典型路径（否则改坏了也一直绿）。
	for _, p := range []string{prefix + "agent/app", prefix + "mcp", prefix + "canvas/repo",
		prefix + "canvas/transport", prefix + "transport/httpx"} {
		if forbidden(p) == "" {
			t.Errorf("判定应拦下 %s", p)
		}
	}
	if forbidden(prefix+"collaboration/domain") != "" {
		t.Error("collaboration/domain 应放行")
	}
}
