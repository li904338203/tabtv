package tools

import (
	"testing"

	"github.com/li904338203/tabtv/backend/internal/mcp/app"
)

// 快照生成器传的是零值 Deps：注册阶段不能依赖端口非 nil，否则 `make gen` 直接 panic。
func TestNewRegistry_零值依赖可构造并导出快照(t *testing.T) {
	if _, err := app.Snapshot(NewRegistry(Deps{})); err != nil {
		t.Fatal(err)
	}
}
