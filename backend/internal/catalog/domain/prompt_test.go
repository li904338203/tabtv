package domain

import (
	"strings"
	"testing"
)

func TestExtractPlaceholders(t *testing.T) {
	got, err := ExtractPlaceholders("请按 {{story}} 生成 {{shotCount}} 个镜头，风格 {{story}}")
	if err != nil {
		t.Fatal(err)
	}
	// 去重且有序
	if len(got) != 2 || got[0] != "shotCount" || got[1] != "story" {
		t.Errorf("得到 %v", got)
	}
	if got, _ := ExtractPlaceholders("没有占位符"); len(got) != 0 {
		t.Errorf("应为空，得到 %v", got)
	}
	// 允许空格
	got, _ = ExtractPlaceholders("{{ story }}")
	if len(got) != 1 || got[0] != "story" {
		t.Errorf("应去除空格，得到 %v", got)
	}
}

// 有开无闭时，运行时会把后面整段当成占位符名，或原样发给模型。
func TestExtractPlaceholders_语法错误(t *testing.T) {
	for _, bad := range []string{
		"请按 {{story 生成", // 未闭合
		"请按 {{}} 生成",    // 空占位符
		"{{a{{b}}",      // 嵌套
		"多余的 }} 闭合",     // 只有闭合
	} {
		if _, err := ExtractPlaceholders(bad); err == nil {
			t.Errorf("非法语法应被拒绝: %q", bad)
		}
	}
}

// ⚠️ 核心：写错变量名不会报错，会把字面量 {{shotCont}} 发给模型，
// 模型照着那段乱码生成，用户看到莫名其妙的结果。必须保存时就拦。
func TestValidatePromptContent_未知占位符(t *testing.T) {
	err := ValidatePromptContent("storyboard_gen.system",
		"按 {{story}} 生成 {{shotCount}} 个镜头，参考 {{shotCont}}")
	if err == nil {
		t.Fatal("未知占位符应被拒绝")
	}
	if !strings.Contains(err.Error(), "shotCont") {
		t.Errorf("错误应指出是哪个占位符，实际 %v", err)
	}
	// 还要列出可用的，否则运营只能猜。
	if !strings.Contains(err.Error(), "可用") {
		t.Errorf("错误应列出可用占位符，实际 %v", err)
	}
}

// 缺必需占位符 = 丢掉一段上下文，模型会在信息不全的情况下照样生成。
//
// ⚠️ 脚本域那三个用途的内容占位符现在都是**可选**的（见 promptSpecs 的注释），
// 所以这条规则用一个临时用途来守：规则本身还在，只是不再套在
// "上下文由 user 消息保证送达"的那几个用途上。
func TestValidatePromptContent_缺必需占位符(t *testing.T) {
	const code = "test_required.system"
	promptSpecs[code] = PromptSpec{
		Code: code, Name: "测试用途",
		Required: []string{"alpha", "beta"}, MaxChars: 1000,
	}
	t.Cleanup(func() { delete(promptSpecs, code) })

	err := ValidatePromptContent(code, "只写了 {{alpha}}")
	if err == nil {
		t.Fatal("缺 beta 应被拒绝")
	}
	if !strings.Contains(err.Error(), "beta") {
		t.Errorf("错误应指出缺哪个，实际 %v", err)
	}
}

// ⚠️ 与上一条相对：脚本域的系统提示词**允许一个占位符都不写**。
// 强制内联剧情会让同一段文字在 system 和 user 里各发一遍，
// 而回落到内置兜底时模型会一个字的剧情都收不到 —— 却照样生成。
func TestValidatePromptContent_脚本域允许纯指令模板(t *testing.T) {
	for _, code := range []string{"storyboard_gen.system", "prompt_compose.system",
		"asset_profile.system"} {
		if err := ValidatePromptContent(code, "你是分镜师，按要求输出。"); err != nil {
			t.Errorf("%s 应允许纯指令模板: %v", code, err)
		}
	}
}

func TestValidatePromptContent_正常(t *testing.T) {
	ok := "按 {{story}} 生成 {{shotCount}} 个镜头，全局风格 {{globalStyle}}"
	if err := ValidatePromptContent("storyboard_gen.system", ok); err != nil {
		t.Errorf("合法模板应通过: %v", err)
	}
	// 可选占位符不给也行 —— 运营想内联上下文就写，不想写就靠 user 消息。
	if err := ValidatePromptContent("storyboard_gen.system",
		"按 {{story}} 生成 {{shotCount}} 个镜头"); err != nil {
		t.Errorf("省略可选占位符应通过: %v", err)
	}
}

// 提示词过长会把模型输出撑短甚至撑坏 ——
// 旧系统曾因样板过长让模型在 JSON 里写出裸引号，整批响应作废。
func TestValidatePromptContent_长度上限(t *testing.T) {
	long := "按 {{story}} 生成 {{shotCount}} 个镜头。" + strings.Repeat("补", 9000)
	err := ValidatePromptContent("storyboard_gen.system", long)
	if err == nil {
		t.Fatal("超长内容应被拒绝")
	}
	if !strings.Contains(err.Error(), "超过上限") {
		t.Errorf("错误应说明超长，实际 %v", err)
	}
}

func TestValidatePromptContent_未知用途与空内容(t *testing.T) {
	if err := ValidatePromptContent("unknown.code", "{{x}}"); err == nil {
		t.Error("未知用途应被拒绝")
	}
	if err := ValidatePromptContent("storyboard_gen.system", "   "); err == nil {
		t.Error("空内容应被拒绝")
	}
}

// ⚠️ 缺变量时报错而不是静默留空：
// 留空会让模型收到缺了关键上下文的提示词并照样生成，
// 结果看起来"能跑"但质量莫名其妙地差。
func TestRenderPrompt(t *testing.T) {
	tpl := "按 {{story}} 生成 {{shotCount}} 个镜头"
	got, err := RenderPrompt(tpl, map[string]string{"story": "西游记", "shotCount": "12"})
	if err != nil {
		t.Fatal(err)
	}
	if got != "按 西游记 生成 12 个镜头" {
		t.Errorf("得到 %q", got)
	}

	_, err = RenderPrompt(tpl, map[string]string{"story": "西游记"})
	if err == nil {
		t.Fatal("缺变量应报错而不是留空")
	}
	if !strings.Contains(err.Error(), "shotCount") {
		t.Errorf("错误应指出缺哪个变量，实际 %v", err)
	}
}

func TestRenderPrompt_重复占位符全部替换(t *testing.T) {
	got, err := RenderPrompt("{{a}} 和 {{a}}", map[string]string{"a": "X"})
	if err != nil {
		t.Fatal(err)
	}
	if got != "X 和 X" {
		t.Errorf("重复占位符应全部替换，得到 %q", got)
	}
}

func TestPromptSpecs_有序且非空(t *testing.T) {
	specs := PromptSpecs()
	if len(specs) == 0 {
		t.Fatal("用途目录不应为空")
	}
	for i := 1; i < len(specs); i++ {
		if specs[i-1].Code >= specs[i].Code {
			t.Error("应按 code 排序")
		}
	}
	// ⚠️ 这里断言的是**可用占位符**非空，不是必需占位符非空。
	// 一个用途至少要让运营能引用到上下文；是否强制内联是另一回事 ——
	// 强制会让同一段剧情在 system 和 user 里各发一遍，
	// 而回落到内置兜底时模型一个字的剧情都收不到（见 promptSpecs 的注释）。
	for _, s := range specs {
		if len(s.Required)+len(s.Optional) == 0 && !s.Static {
			t.Errorf("%s 一个可用占位符都没有，模板与上下文完全无关", s.Code)
		}
	}
}

// 04 文档 P-45：`{{ state }}` 能通过保存校验，渲染时却原样留下（只替换了无空格的写法）。
func TestRenderPrompt_带空格的占位符同样替换(t *testing.T) {
	got, err := RenderPrompt("阶段：{{ state }}；模型：{{model}}；再来一次 {{  state  }}。", map[string]string{
		"state": "planning", "model": "m1"})
	if err != nil {
		t.Fatal(err)
	}
	if got != "阶段：planning；模型：m1；再来一次 planning。" {
		t.Fatalf("带空格的占位符也要替换：%q", got)
	}
	// 变量值里出现 {{x}} 不再被二次替换（逐段扫描，不是 ReplaceAll 链）。
	got, err = RenderPrompt("{{a}}{{b}}", map[string]string{"a": "{{b}}", "b": "B"})
	if err != nil || got != "{{b}}B" {
		t.Fatalf("变量值不参与二次替换：%q %v", got, err)
	}
}

// 3D 导演台的三段提示词都是平台规则、不收变量（DRT-07 / DRT-19）：上下文由调用方拼的 user 消息送达。
func TestPromptSpecs_导演台约束段为Static(t *testing.T) {
	for _, code := range []string{"director.reference_guard.capture", "director.reference_guard.preview",
		"director.scene_build.system"} {
		spec, ok := promptSpecs[code]
		if !ok || !spec.Static || len(spec.Required)+len(spec.Optional) != 0 {
			t.Errorf("%s 应登记为 Static 且不收变量，得到 %+v（登记 %v）", code, spec, ok)
		}
	}
}
