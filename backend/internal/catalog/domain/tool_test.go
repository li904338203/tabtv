package domain

import (
	"strings"
	"testing"
)

func TestCheckToolInput_缺图与缺词分别说清楚(t *testing.T) {
	cases := []struct {
		name  string
		probe ToolProbe
		want  string
	}{
		{"需要图但没图", ToolProbe{InputMode: InputModeImageRequired}, ToolNeedsImage},
		{"需要词但没词", ToolProbe{InputMode: InputModeTextRequired}, ToolNeedsText},
		// ⚠️ either 要求**至少有一样**：两样都没有时模型收到一句空提示词，
		// 出来的图和用户想要的毫无关系，而任务是"成功"的、钱照扣。
		{"两者皆可但两样都没有", ToolProbe{InputMode: InputModeEither}, ToolNeedsText},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := CheckToolInput(c.probe, "高清放大")
			if b == nil {
				t.Fatal("应被拦下")
			}
			if b.Code != c.want {
				t.Fatalf("分类应为 %s，实际 %s", c.want, b.Code)
			}
			// 提示里要带上工具名：一排灰按钮配同一句"缺少输入"，
			// 用户不知道是哪个工具在抱怨。
			if !strings.Contains(b.Detail, "高清放大") {
				t.Fatalf("提示里应带上工具名：%s", b.Detail)
			}
		})
	}
}

func TestCheckToolInput_输入够了就放行(t *testing.T) {
	ok := []ToolProbe{
		{InputMode: InputModeImageRequired, ImageCount: 1},
		{InputMode: InputModeTextRequired, HasText: true},
		{InputMode: InputModeEither, ImageCount: 1},
		{InputMode: InputModeEither, HasText: true},
		// 认不出的 input_mode 不拦：配置可以比代码新（与配置驱动画布同一条）。
		{InputMode: "future_mode"},
	}
	for i, p := range ok {
		if b := CheckToolInput(p, "工具"); b != nil {
			t.Fatalf("第 %d 项不该被拦：%+v", i, b)
		}
	}
}

// ⚠️ 前端按**分类**决定提示语气，不按文案匹配：文案改一个字匹配就失效，
// 而失效的表现是所有提示都退回默认那句。
func TestUserFixable_区分谁能解决(t *testing.T) {
	if !UserFixable(ToolNeedsImage) || !UserFixable(ToolNeedsText) {
		t.Fatal("缺输入是用户自己能解决的")
	}
	// 这些是运营要解决的：用户盯着灰按钮等也没用。
	for _, c := range []string{"capability_unsupported", "pricing_missing", "model_unavailable"} {
		if UserFixable(c) {
			t.Fatalf("%s 不是用户能解决的", c)
		}
	}
}

// ⚠️ 这条是 P09-04 的逐工具验收矩阵抓出来的，单测抓不到它 ——
// 它只在"真的连了一张图"时才出现：工具栏说 `character_sheet` 能用，
// 点下去受理报「文生图 不接受媒体输入，当前连了 1 个」。
func TestCheckOperationAcceptsMedia_收不收图与能力矩阵是两回事(t *testing.T) {
	// text2img 的 in_node_types 是 text,style —— 不收图。
	b := CheckOperationAcceptsMedia([]string{"text", "style"}, 1, "角色三视图")
	if b == nil {
		t.Fatal("连了图给不收图的操作，应在工具栏就拦下")
	}
	if b.Code != ToolMediaRejected {
		t.Fatalf("分类应为 %s，实际 %s", ToolMediaRejected, b.Code)
	}
	// 断开连线就能用，所以算用户可解决。
	if !UserFixable(b.Code) {
		t.Fatal("断开图片连线是用户自己能做的")
	}
	if !strings.Contains(b.Detail, "角色三视图") {
		t.Fatalf("提示要带上工具名：%s", b.Detail)
	}
}

func TestCheckOperationAcceptsMedia_该放行的放行(t *testing.T) {
	cases := []struct {
		name    string
		accepts []string
		images  int
	}{
		{"没连图", []string{"text", "style"}, 0},
		{"操作本来就收图", []string{"image", "style"}, 1},
		// ⚠️ 没声明输入类型时**放行**：配置可以比代码新，
		// 不认识的东西不该在这里被拦下，受理链路上还有一道真校验。
		{"配置里没填", nil, 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if b := CheckOperationAcceptsMedia(c.accepts, c.images, "工具"); b != nil {
				t.Fatalf("不该拦：%+v", b)
			}
		})
	}
}
