package wiring

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const modulePrefix = "github.com/li904338203/tabtv/backend/internal/"

// allowedTransport 是唯一放行的 `*/transport` 包：Provider 的**出站** HTTP 客户端
// （URL 策略、超时、脱敏日志），不是 HTTP handler 层 —— Worker 本来就依赖它，
// 而文本 Provider 的装配离不开它。
const allowedTransport = modulePrefix + "provider/transport"

// forbiddenImport 判断一个 import 是否把装配拖进 HTTP/WS 层。
//
// 禁止：各领域的 HTTP 层 `internal/<域>/transport`（provider 的出站客户端除外）
// 与 `internal/transport/ws`。
// 放行：`internal/transport/httpx`、`internal/transport/errcode` —— 它们是业务错误码
// 与错误信封，领域 app 包（billing/canvas/catalog…）本来就 import 它们，
// 禁掉它们在传递依赖上没有意义。
func forbiddenImport(path string) bool {
	if !strings.HasPrefix(path, modulePrefix) {
		return false
	}
	if path == modulePrefix+"transport/ws" || strings.HasPrefix(path, modulePrefix+"transport/ws/") {
		return true
	}
	if strings.HasSuffix(path, "/transport") && path != allowedTransport {
		return true
	}
	return false
}

// Worker 将来要调 BuildCore：装配包一旦 import 了某个域的 handler 层或 WS hub，
// Worker 就会被连带拖进 HTTP 层（AGT-01）。这条绊线读本包全部非测试文件的 import。
func TestWiring_不依赖HTTP与WS层(t *testing.T) {
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
		for _, imp := range af.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				t.Fatal(err)
			}
			if forbiddenImport(path) {
				t.Errorf("%s import 了 %s：internal/wiring 不得依赖 HTTP handler 层与 WS", f, path)
			}
		}
	}
	if checked == 0 {
		t.Fatal("没有读到任何非测试文件，绊线形同虚设")
	}
}

// 判定函数本身也要钉住：否则规则写反了，上面那条测试会永远绿。
func TestForbiddenImport(t *testing.T) {
	cases := map[string]bool{
		modulePrefix + "catalog/transport":    true,
		modulePrefix + "generation/transport": true,
		modulePrefix + "transport/ws":         true,
		modulePrefix + "provider/transport":   false,
		modulePrefix + "transport/httpx":      false,
		modulePrefix + "transport/errcode":    false,
		modulePrefix + "catalog/app":          false,
		"net/http":                            false,
	}
	for path, want := range cases {
		if got := forbiddenImport(path); got != want {
			t.Errorf("forbiddenImport(%q) = %v，期望 %v", path, got, want)
		}
	}
}
