package domain

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// 前端 frontend/utils/agentMessages.ts 从助手末尾的「接下来你可以：」段落里取建议胶囊。
// 这是一条跨端的格式契约：骨架里改了措辞，胶囊就静默消失（正文照常显示，没有报错）。
func TestSystemPromptSkeleton_下一步建议格式与前端解析一致(t *testing.T) {
	if !strings.Contains(SystemPromptSkeleton, "「接下来你可以：」") {
		t.Fatal("骨架必须约定「接下来你可以：」+ 列表的下一步建议格式（前端按它解析建议胶囊）")
	}
}

func TestSceneHeading_未知场景为空(t *testing.T) {
	if got := SceneHeading(SceneMarketing); got != "## 场景：营销" {
		t.Errorf("营销场景标题 = %q", got)
	}
	for _, s := range []Scene{SceneGeneral, "", "writing", "MARKETING"} {
		if got := SceneHeading(s); got != "" {
			t.Errorf("场景 %q 不应有场景段标题，得到 %q", s, got)
		}
	}
	if MarketingInputHeading != "## 营销输入" {
		t.Errorf("营销输入标题 = %q", MarketingInputHeading)
	}
}

// fakeUpstreamConst 从联调假上游的源文件里取一个字符串常量（它是独立 module，不能 import）。
func fakeUpstreamConst(t *testing.T, name string) string {
	t.Helper()
	src, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "scripts", "fake-upstream", "agent_script.go"))
	if err != nil {
		t.Fatalf("读取假上游源文件失败：%v", err)
	}
	m := regexp.MustCompile(`(?m)^\s*` + name + `\s*=\s*"([^"]*)"`).FindSubmatch(src)
	if m == nil {
		t.Fatalf("假上游里找不到常量 %s", name)
	}
	return string(m[1])
}

// ⚠️ 骨架首句与阶段标题被假上游逐字匹配（MKD-14）：改了这里而不改那边，E2E 的 Agent 段全部拿不到
// 脚本化回复。这条测试读假上游的源文件核对 —— 改任何一边，两边同时红。
func TestSystemPromptSkeleton_首句未变(t *testing.T) {
	const first = "你是 seeAitv 画布里的创作助手"
	if !strings.HasPrefix(SystemPromptSkeleton, first) {
		t.Fatalf("骨架首句被改动：%q", strings.SplitN(SystemPromptSkeleton, "\n", 2)[0])
	}
	if got := fakeUpstreamConst(t, "agentSkeletonMarker"); !strings.HasPrefix(SystemPromptSkeleton, got) {
		t.Errorf("假上游的骨架标记 %q 不是骨架的开头", got)
	}
	for name, note := range map[string]string{
		"agentPlanningMarker": PlanningModeNote, "agentExecutingMarker": ExecutingModeNote,
	} {
		if got := fakeUpstreamConst(t, name); strings.SplitN(note, "\n", 2)[0] != got {
			t.Errorf("假上游 %s = %q，与阶段标题 %q 不一致", name, got, strings.SplitN(note, "\n", 2)[0])
		}
	}
}

func TestSceneHeading_与假上游标记同源(t *testing.T) {
	if got := fakeUpstreamConst(t, "agentMarketingMarker"); got != SceneHeading(SceneMarketing) {
		t.Errorf("假上游 agentMarketingMarker = %q，与 SceneHeading(marketing) = %q 不一致",
			got, SceneHeading(SceneMarketing))
	}
}
