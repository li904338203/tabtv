package app

import (
	"strings"
	"testing"

	"github.com/li904338203/tabtv/backend/internal/canvas/repo"
)

// MCPT-09：MCP 写入时最后编辑者是授权所属的用户本人。按 ID 比的话他会被告知
// 「你在另一个窗口改过」，进而点「以我为准」把 MCP 刚建的节点整份覆盖（违反 MCP-F-60）。
func TestConflictMessage_MCP写入不算本人(t *testing.T) {
	const me = int64(1001)
	cases := []struct {
		name     string
		st       repo.ConflictState
		bySelf   bool
		contains string
	}{
		{"本人真人保存", repo.ConflictState{EditorKind: repo.EditorUser, EditorID: me, EditorName: "林澈"}, true, "另一个窗口"},
		{"同事真人保存", repo.ConflictState{EditorKind: repo.EditorUser, EditorID: 2, EditorName: "苏夏"}, false, "苏夏"},
		{"Agent 代本人", repo.ConflictState{EditorKind: repo.EditorAgent, EditorID: me, EditorName: "林澈", AgentSessionID: 7},
			false, "AI 助手（代 林澈）"},
		{"MCP 代本人", repo.ConflictState{EditorKind: repo.EditorMCP, EditorID: me, EditorName: "林澈", McpGrantID: 5001},
			false, "外部 AI 客户端（代 林澈）"},
		{"MCP 无昵称", repo.ConflictState{EditorKind: repo.EditorMCP, EditorID: me, McpGrantID: 5001},
			false, "外部 AI 客户端修改了"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := BySelf(c.st, me); got != c.bySelf {
				t.Errorf("BySelf 得到 %v，期望 %v", got, c.bySelf)
			}
			msg := conflictMessage(c.st, me)
			if !strings.Contains(msg, c.contains) {
				t.Errorf("文案 %q 应包含 %q", msg, c.contains)
			}
			if c.st.EditorKind == repo.EditorMCP && strings.Contains(msg, "另一个窗口") {
				t.Errorf("MCP 写入不能说成本人另一个窗口：%q", msg)
			}
		})
	}
	if idOrEmpty(0) != "" || idOrEmpty(5001) != "5001" {
		t.Error("idOrEmpty 形态不对")
	}
}
