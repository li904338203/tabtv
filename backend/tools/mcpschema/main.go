// Command mcpschema 从 MCP 工具注册表导出快照 api/mcp/tools.json（MT-8、开发 02 §4.2）。
//
// ⚠️ 用的是与 cmd/api **同一个**构造函数 internal/mcp/tools.NewRegistry：快照与运行时同源，
// 新增或改动工具时差异会出现在评审里；`make gen-check` 拿它比对漂移（MCP-N-10）。
// 生成物禁止手改。
//
//	cd backend && go run ./tools/mcpschema            # 写 ../api/mcp/tools.json
//	cd backend && go run ./tools/mcpschema -out -     # 打到标准输出
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/li904338203/tabtv/backend/internal/mcp/app"
	"github.com/li904338203/tabtv/backend/internal/mcp/tools"
)

func main() {
	out := flag.String("out", "../api/mcp/tools.json", "输出路径（相对 backend）；- 表示标准输出")
	flag.Parse()
	if err := run(*out); err != nil {
		fmt.Fprintln(os.Stderr, "mcpschema:", err)
		os.Exit(1)
	}
}

func run(out string) error {
	// 零值 Deps：注册（名字、method、schema）不依赖端口，只有 handler 执行时才用到。
	b, err := app.Snapshot(tools.NewRegistry(tools.Deps{}))
	if err != nil {
		return err
	}
	if out == "-" {
		_, err = os.Stdout.Write(b)
		return err
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return err
	}
	return os.WriteFile(out, b, 0o644)
}
