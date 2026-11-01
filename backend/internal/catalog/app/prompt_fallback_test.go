package app

import (
	"strings"
	"testing"

	"github.com/li904338203/tabtv/backend/internal/catalog/domain"
)

// 兜底是「库里一条都没有」时真正发给模型的内容：它本身也必须过用途契约，
// 否则运营把它原样存成 DB 行时会被拒，而线上一直在发一段"不合规"的文本。
func TestPromptFallback_都登记了用途且通过契约(t *testing.T) {
	for code, content := range promptFallback {
		if _, ok := domain.LookupPromptSpec(code); !ok {
			t.Errorf("兜底 %s 没有登记用途（管理端无法编辑它）", code)
			continue
		}
		if err := domain.ValidatePromptContent(code, content); err != nil {
			t.Errorf("兜底 %s 不符合用途契约：%v", code, err)
		}
	}
}

func TestPromptSpecs_营销两段可在管理端编辑(t *testing.T) {
	for _, code := range []string{"agent.scene.marketing", "agent.scene.marketing.constraint"} {
		if _, ok := domain.LookupPromptSpec(code); !ok {
			t.Errorf("%s 未登记进用途表", code)
		}
	}
	// 约束段渲染时不给变量（marketing/app.Appendix）：写进占位符会让每一条营销生成在受理时失败。
	if err := domain.ValidatePromptContent("agent.scene.marketing.constraint", "保持 {{x}}"); err == nil {
		t.Error("约束段不应接受占位符")
	}
	if err := domain.ValidatePromptContent("agent.scene.marketing",
		"阶段 {{state}}，模型 {{reasoningModel}}，最多 {{maxVariants}} 个变体，{{productCount}} 张图"); err != nil {
		t.Errorf("场景段应接受四个变量：%v", err)
	}
}

// MKT-F-43 / MKD-15 / PARAM-12：场景段兜底只写已定稿的规则，不含约束段、不含待审的两句。
func TestPromptFallback_营销场景段只含已定稿规则(t *testing.T) {
	fb := promptFallback["agent.scene.marketing"]
	for _, must := range []string{"先识别商品", "目标人群（用户给了才写，不编造）", "Hook 出现的时间点", "镜头节奏",
		"画幅与时长", "所用模型", "商品图", "ROI", "不保证口型与语音逐字同步", "list_marketing_options"} {
		if !strings.Contains(fb, must) {
			t.Errorf("场景段兜底缺少「%s」", must)
		}
	}
	for _, mustNot := range []string{"画面中不出现任何文字", "人物不重复", "开头以 Hook 为准", "{{", "\n#"} {
		if strings.Contains(fb, mustNot) {
			t.Errorf("场景段兜底不应含「%s」", mustNot)
		}
	}
}

func TestRenderFallbackPrompt(t *testing.T) {
	s := New(nil, nil)
	vars := map[string]string{"state": "planning", "reasoningModel": "m", "maxVariants": "4", "productCount": "3"}
	got, err := s.RenderFallbackPrompt("agent.scene.marketing", vars)
	if err != nil || got != promptFallback["agent.scene.marketing"] {
		t.Fatalf("兜底渲染：%q %v", got, err)
	}
	if _, err := s.RenderFallbackPrompt("no.such", nil); err == nil {
		t.Error("没有兜底的 code 应报错")
	}
}

// MKT-F-83（生意增长顾问，MKT-T-26）：顾问没有外部数据源，「不编造数据 / 行业竞品平台数据写明需你核实 /
// 不承诺 ROI」必须在场景段兜底里逐字存在 —— 顾问不另写提示词，营销会话的这一段就是它唯一的底线。
func TestPromptFallback_营销场景段含不编造数据规则(t *testing.T) {
	fb := promptFallback["agent.scene.marketing"]
	for _, must := range []string{"不编造数据", "行业、竞品、平台数据", "需你核实", "不给虚构数字", "不承诺 ROI 或销量"} {
		if !strings.Contains(fb, must) {
			t.Errorf("场景段兜底缺少「%s」", must)
		}
	}
}

// 智能剪辑的六段创作要求都有兜底（库里一条都没有时也能剪），且不出现「分镜」二字（假上游见到它就回分镜 JSON）。
func TestPromptFallback_智能剪辑六段(t *testing.T) {
	for _, code := range []string{"smartcut.mix.system", "smartcut.board.system", "smartcut.talk.system",
		"smartcut.narr.system", "smartcut.edit.system", "smartcut.translate.system"} {
		fb, ok := promptFallback[code]
		if !ok || strings.TrimSpace(fb) == "" {
			t.Errorf("%s 没有兜底", code)
			continue
		}
		if strings.Contains(fb, "分镜") || strings.Contains(fb, "{{") {
			t.Errorf("%s 的兜底不能有「分镜」与占位符：%s", code, fb)
		}
	}
}
