package tools

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/li904338203/tabtv/backend/internal/agent/domain"
	collab "github.com/li904338203/tabtv/backend/internal/collaboration/domain"
)

type stubTool struct{ name domain.ToolName }

func (s stubTool) Name() domain.ToolName   { return s.name }
func (s stubTool) Description() string     { return "d-" + string(s.name) }
func (s stubTool) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (s stubTool) Paid() bool              { return false }
func (s stubTool) Action() collab.Action   { return collab.ActionView }
func (s stubTool) Run(context.Context, RunContext, json.RawMessage) (ToolResult, error) {
	return ToolResult{}, nil
}

func TestRegistry_按白名单顺序输出且跳过未实现(t *testing.T) {
	r, err := NewRegistry(stubTool{domain.ToolListAssets}, stubTool{domain.ToolReadCanvas})
	if err != nil {
		t.Fatal(err)
	}
	defs := r.Schemas([]domain.ToolName{domain.ToolReadCanvas, domain.ToolQuote, domain.ToolListAssets})
	if len(defs) != 2 || defs[0].Name != "read_canvas" || defs[1].Name != "list_assets" {
		t.Fatalf("应按传入顺序、跳过未注册的 quote：%+v", defs)
	}
	if _, ok := r.Lookup(domain.ToolQuote); ok {
		t.Error("未注册的工具不应查到")
	}
}

func TestRegistry_拒绝规格表外与重复注册(t *testing.T) {
	if _, err := NewRegistry(stubTool{"rm_rf"}); err == nil {
		t.Error("规格表外的工具应拒绝")
	}
	if _, err := NewRegistry(stubTool{domain.ToolQuote}, stubTool{domain.ToolQuote}); err == nil {
		t.Error("重复注册应拒绝")
	}
}
