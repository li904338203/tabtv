package mcp

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestMCP_不导入其他域repo 读 internal/mcp/** 全部非测试文件的 import（开发 01 §2、MT-1）：
// MCP 只经端口调各域 app 服务。直接 import 某个域的 repo 等于绕过那个域的业务判断
// （权限、计价、状态机），而那正是「不复制业务判断」（C-4）要守的线。
func TestMCP_不导入其他域repo(t *testing.T) {
	const prefix = "github.com/li904338203/tabtv/backend/internal/"
	forbidden := func(path string) bool {
		if !strings.HasPrefix(path, prefix) {
			return false
		}
		rest := strings.TrimPrefix(path, prefix)
		if rest == "mcp" || strings.HasPrefix(rest, "mcp/") {
			return false // 自己的 repo 可以用
		}
		return strings.HasSuffix(rest, "/repo") || strings.Contains(rest, "/repo/")
	}
	fset := token.NewFileSet()
	checked := 0
	err := filepath.WalkDir(".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return err
		}
		af, err := parser.ParseFile(fset, p, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		checked++
		for _, im := range af.Imports {
			ip, _ := strconv.Unquote(im.Path.Value)
			if forbidden(ip) {
				t.Errorf("%s import 了 %s：MCP 只能经 app 端口调用其他域", p, ip)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if checked < 5 {
		t.Fatalf("只读到 %d 个源文件：绊线形同虚设", checked)
	}
	// 自检：判定本身要拦得下典型路径、放得过自己的 repo。
	for _, p := range []string{prefix + "canvas/repo", prefix + "agent/repo/integration"} {
		if !forbidden(p) {
			t.Errorf("判定应拦下 %s", p)
		}
	}
	for _, p := range []string{prefix + "mcp/repo", prefix + "canvas/app", "github.com/x/repo"} {
		if forbidden(p) {
			t.Errorf("判定不该拦 %s", p)
		}
	}
}
