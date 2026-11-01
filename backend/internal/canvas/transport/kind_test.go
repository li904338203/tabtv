package transport

import (
	"testing"

	"github.com/li904338203/tabtv/backend/internal/canvas/repo"
)

// 类型筛选白名单（AGT-45）：缺省与 all 都是全部；写错要 400 而不是静默给全部 ——
// 否则用户点了「Agent」看到的是全部项目，而他会以为那些都是 Agent 项目。
func TestParseKind(t *testing.T) {
	cases := []struct {
		raw  string
		want repo.ProjectKind
		ok   bool
	}{
		{"", repo.KindAll, true},
		{"all", repo.KindAll, true},
		{"canvas", repo.KindCanvas, true},
		{"agent", repo.KindAgent, true},
		{"marketing", repo.KindMarketing, true},
		{"Agent", "", false},
		{"chat", "", false},
		{" agent", "", false},
	}
	for _, c := range cases {
		got, ok := parseKind(c.raw)
		if ok != c.ok || got != c.want {
			t.Errorf("parseKind(%q) = (%q,%v)，期望 (%q,%v)", c.raw, got, ok, c.want, c.ok)
		}
	}
}
