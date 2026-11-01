package app

import (
	"context"
	"strconv"

	"github.com/li904338203/tabtv/backend/internal/agent/domain"
)

// 营销会话组装上下文与只读工具要问营销域的事（MKT-T-18，dev/01 §3.3、§4.2、§5.3）。
//
// ⚠️ 端口定义在 agent、实现在 wiring（适配 marketing/app 与 catalog/app）：agent 不 import marketing。
// 「## 营销输入」的正文格式全仓只有一份实现（marketing/domain.SlotSummary）；选项可见性判定也只有一份
// （marketing/app.ListOptions）—— 在这里再拼一遍迟早两边不一致。

// MarketingSceneTemplate 是营销场景段的提示词模板 code（prompt_template，运营可在管理端编辑）。
const MarketingSceneTemplate = "agent.scene.marketing"

// SceneContext 给出营销会话系统提示词的③④两段素材。
type SceneContext interface {
	// RenderScenePrompt 渲染场景段模板（DB 行 > 代码兜底）。
	RenderScenePrompt(ctx context.Context, code, modelCode string, vars map[string]string) (string, error)
	// FallbackScenePrompt 只用代码兜底渲染：DB 行渲染失败时回落用（不让整段要求凭空消失）。
	FallbackScenePrompt(code string, vars map[string]string) (string, error)
	// MaxVariants 单次最多几个变体（SEEAITV_MARKETING_MAX_VARIANTS）。
	MaxVariants() int
	// MarketingSummary 拼「## 营销输入」的**正文**（不含标题，标题是 domain.MarketingInputHeading）。
	// ws 用来按工作区取角色资产名；visibleImages 是本次附给推理模型看的商品图张数（#1 起连续）。
	//
	// ⚠️ 结果不含 URL、mediaId、片段原文（MKT-AC-05、06）：只有名称、说明、推荐模型的**显示名**。
	MarketingSummary(ctx context.Context, ws int64, in SceneInput, visibleImages int) (string, error)
}

// MarketingSceneVars 是渲染场景段模板的变量 —— **给全**（含可选的）：少给一个，运营在模板里一引用
// 就整条渲染失败、回落兜底（CLAUDE.md §5.7）。与 catalog 用途契约的一致性由 wiring 的测试钉住。
func MarketingSceneVars(state domain.State, reasoningModel string, maxVariants, productCount int) map[string]string {
	return map[string]string{
		"state":          string(state),
		"reasoningModel": reasoningModel,
		"maxVariants":    strconv.Itoa(maxVariants),
		"productCount":   strconv.Itoa(productCount),
	}
}

// ---------------------------------------------------------------- list_marketing_options

// MarketingOptionsQuery 是 list_marketing_options 的查询。Kind 为空 = 三类各取一页。
type MarketingOptionsQuery struct {
	Kind     string
	Category string
	Q        string
	Limit    int
}

// MarketingOption 是工具回给模型的一条选项。
//
// ⚠️ 结构体上**没有**片段原文、封面媒体 ID、URL（MKT-AC-06）：不是「拼的时候记得别放」，而是放不进来。
type MarketingOption struct {
	Kind            string   `json:"kind"`
	Code            string   `json:"code"`
	Name            string   `json:"name"`
	Description     string   `json:"description,omitempty"`
	CategoryCode    string   `json:"categoryCode,omitempty"`
	Tags            []string `json:"tags,omitempty"`
	IsAudioRequired bool     `json:"isAudioRequired"`
}

// MarketingOptionsResult 是一次查询的结果。More = 还有更多（让模型缩小条件，而不是翻页）。
type MarketingOptionsResult struct {
	Items []MarketingOption `json:"items"`
	More  bool              `json:"more"`
}

// MarketingOptionLister 列此刻**在投**的营销选项（可见性判定与模板墙同一个，MKD-13）。
type MarketingOptionLister interface {
	ListMarketingOptions(ctx context.Context, q MarketingOptionsQuery) (MarketingOptionsResult, error)
}
