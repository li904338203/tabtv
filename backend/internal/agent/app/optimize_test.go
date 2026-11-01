package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

func TestCleanOptimizedText(t *testing.T) {
	cases := []struct {
		name, in, want string
		bad            bool
	}{
		{name: "原样", in: "  雨夜霓虹街头，镜头缓慢推进 \n", want: "雨夜霓虹街头，镜头缓慢推进"},
		{name: "中文引号包裹", in: "“雨夜霓虹街头”", want: "雨夜霓虹街头"},
		{name: "英文引号包裹", in: "\"雨夜\"", want: "雨夜"},
		{name: "中间也有引号不剥", in: "“甲”与“乙”", want: "“甲”与“乙”"},
		{name: "代码块", in: "```\n雨夜\n```", want: "雨夜"},
		{name: "带语言标记的代码块", in: "```text\n雨夜霓虹\n```", want: "雨夜霓虹"},
		{name: "空", in: "   \n\t", bad: true},
		{name: "空代码块", in: "``` ```", bad: true},
		{name: "只有引号", in: "“”", bad: true},
		{name: "超长", in: strings.Repeat("字", MaxMessageRunes+1), bad: true},
	}
	for _, c := range cases {
		got, reason := CleanOptimizedText(c.in)
		if c.bad {
			if reason == "" {
				t.Errorf("%s：应判不可用，得到 %q", c.name, got)
			}
			continue
		}
		if reason != "" || got != c.want {
			t.Errorf("%s：得到 %q（%s），期望 %q", c.name, got, reason, c.want)
		}
	}
}

// 缺省 purpose 与 general 用的是改动前那一份指令，逐字不变（MKT-AC-40）；角色外观用另一份。
func TestOptimize_purpose缺省逐字不变(t *testing.T) {
	for _, p := range []string{"", OptimizePurposeGeneral} {
		got, ok := optimizeSystemFor(p)
		if !ok || got != optimizeSystemPrompt {
			t.Errorf("purpose=%q 应沿用通用改写指令", p)
		}
	}
	// 钉住通用指令的开头与输出契约：改它等于改创造页一键优化的行为，要单独评审。
	if !strings.HasPrefix(optimizeSystemPrompt, "你是短视频创作的提示词优化助手。") ||
		!strings.Contains(optimizeSystemPrompt, "5. 只输出改写后的正文：不要解释、不要标题、不要列表符号、不要加引号或代码块。") {
		t.Errorf("通用改写指令被改动了：%q", optimizeSystemPrompt)
	}
	got, ok := optimizeSystemFor(OptimizePurposeCharacterAppearance)
	if !ok || got != characterAppearancePrompt || got == optimizeSystemPrompt {
		t.Fatalf("character_appearance 应选角色外观指令")
	}
	// 只描述外观、不写动作剧情、不写真实人物姓名、输出形状与通用指令一致。
	for _, must := range []string{"非真人角色", "只描述角色的外观", "不写动作、情节", "不写任何真实人物的姓名",
		"只输出改写后的正文：不要解释、不要标题、不要列表符号、不要加引号或代码块。"} {
		if !strings.Contains(characterAppearancePrompt, must) {
			t.Errorf("角色外观指令缺少「%s」", must)
		}
	}
	for _, banned := range []string{"复刻", "真人认证", "声音克隆"} {
		if strings.Contains(characterAppearancePrompt, banned) {
			t.Errorf("角色外观指令不应出现「%s」（MK-23、MK-25）", banned)
		}
	}
}

// 未知 purpose 在一切副作用之前拒绝：不调模型、不扣费（MKT-AC-40）。
func TestOptimize_未知purpose拒绝(t *testing.T) {
	chat, ledger := &fakeChat{}, newFakeLedger(100)
	s := NewService(nil, func() int64 { return 1 }, ServicePorts{Optimize: &OptimizePorts{
		Chat: chat, Quoter: fakeQuoter{credits: 3}, Ledger: ledger}}, nil)
	_, err := s.OptimizePrompt(context.Background(), SessionActor{WorkspaceID: 1, PayerAccountID: 2, UserID: 3},
		OptimizeInput{IdempotencyKey: "k1", Text: "圆脸柚子", Purpose: "script"})
	var be *httpx.BizError
	if !errors.As(err, &be) || be.Code != errcode.InvalidArgument || be.Details["field"] != "purpose" {
		t.Fatalf("未知 purpose 应 40001 点名 purpose：%v", err)
	}
	if len(chat.calls()) != 0 || len(ledger.consumed) != 0 {
		t.Errorf("被拒的请求不应调模型或扣费")
	}
}
