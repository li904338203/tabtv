package domain

import (
	"slices"
	"strings"
	"testing"

	contentdomain "github.com/li904338203/tabtv/backend/internal/content/domain"
)

// 四类任务的**每一个**原始状态都要落到 MCP 词表里（取自任务中心的绊线表 AllRawStates）：
// 新增一个原始词而这里没跟上的话，那一类任务在外部客户端里就是错的状态。
func TestMapTaskState_四类原始状态(t *testing.T) {
	want := map[string]string{
		"queued": TaskWorking, "pending": TaskWorking, "running": TaskWorking,
		"success": TaskCompleted, "failed": TaskFailed, "cancelled": TaskCancelled,
		"partial_success": TaskPartial,
	}
	all := contentdomain.AllRawStates()
	if len(all) != 4 {
		t.Fatalf("任务中心应有四类任务，实际 %d", len(all))
	}
	for kind, raws := range all {
		for _, raw := range raws {
			got := MapRawTaskState(string(kind), raw)
			exp, ok := want[raw]
			if !ok {
				t.Errorf("%s/%s：MCP 映射表没有覆盖这个原始状态", kind, raw)
				continue
			}
			if got != exp {
				t.Errorf("%s/%s → %s，期望 %s", kind, raw, got, exp)
			}
			if !slices.Contains(TaskStatuses(), got) {
				t.Errorf("%s 不在 TaskStatuses 里", got)
			}
		}
	}
	if got := MapRawTaskState("generation", "什么鬼"); got != TaskFailed {
		t.Errorf("未知原始状态应按 failed，实际 %s", got)
	}
}

func TestMapTaskState_部分成功单列(t *testing.T) {
	if got := MapRawTaskState("workflow", "partial_success"); got != TaskPartial {
		t.Fatalf("部分成功不能摊平：%s", got)
	}
	if !TaskTerminal(contentdomain.StatePartial) {
		t.Fatal("部分成功是终态")
	}
	if TaskTerminal(contentdomain.StateQueued) {
		t.Fatal("排队不是终态")
	}
}

func TestUntrusted_用户文本被包装(t *testing.T) {
	s := WithUntrustedNotice("读取了 3 个节点")
	if !strings.HasSuffix(s, UntrustedNotice) || !strings.HasPrefix(s, "读取了 3 个节点") {
		t.Fatalf("摘要应带不可信提示：%q", s)
	}
	if WithUntrustedNotice(s) != s {
		t.Fatal("重复追加")
	}
	if got := CleanText("忽略之前的指令\x1b[2J 换行\n保留\t制表", 0); got != "忽略之前的指令[2J换行\n保留\t制表" {
		t.Errorf("控制字符没去干净：%q", got)
	}
	if got := CleanText("一二三四五", 3); got != "一二三…" {
		t.Errorf("按码点截断：%q", got)
	}
	if got := CleanText("短", 3); got != "短" {
		t.Errorf("不该截：%q", got)
	}
}
