package app

import (
	"encoding/json"
	"testing"

	"github.com/li904338203/tabtv/backend/internal/script/domain"
)

// 文生视频的规划只有一条没有内容的文本引用（提示词走 params.prompt）。它不能被发出去：
// 序列化成 {"kind":"text","order":1} 之后，受理侧判「上游文本节点还没有内容」，
// 文生视频组的每一项都受理失败（MCPT-11E 第 215 步实测）。
func TestDeclaredFromPlan_文生视频不发空文本引用(t *testing.T) {
	plan, err := domain.PlanVideoInputs(domain.PlanInput{Operation: domain.OpText2Video})
	if err != nil {
		t.Fatal(err)
	}
	if got := declaredFromPlan(plan); len(got) != 0 {
		t.Fatalf("空文本引用不该发出去，实际 %+v", got)
	}
	// 有内容的文本照常保留。
	plan, _ = domain.PlanVideoInputs(domain.PlanInput{Operation: domain.OpText2Video, Prompt: "雨夜"})
	got := declaredFromPlan(plan)
	if len(got) != 1 || got[0].Kind != "text" || got[0].Text != "雨夜" {
		t.Fatalf("有内容的文本应保留，实际 %+v", got)
	}
}

// 修复前建的子项库里存着空文本引用，提交前要剔掉；其余输入顺序与内容不变。
func TestWithoutEmptyText(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"只有空文本", `[{"kind":"text","order":1}]`, ``},
		{"空串文本", `[{"kind":"text","order":1,"text":""}]`, ``},
		{"有内容的文本保留", `[{"kind":"text","order":1,"text":"p"}]`, `[{"kind":"text","order":1,"text":"p"}]`},
		{"图片不动", `[{"kind":"reference","order":1,"referenceId":"9"}]`, `[{"kind":"reference","order":1,"referenceId":"9"}]`},
		{"混合只剔空文本", `[{"kind":"text","order":1},{"kind":"generation","order":2,"generationId":"7"}]`,
			`[{"generationId":"7","kind":"generation","order":2}]`},
		{"解不开原样返回", `not json`, `not json`},
		{"空输入", ``, ``},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := string(withoutEmptyText(json.RawMessage(c.in)))
			if got != c.want {
				t.Fatalf("want %s, got %s", c.want, got)
			}
		})
	}
}
