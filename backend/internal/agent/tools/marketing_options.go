package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/li904338203/tabtv/backend/internal/agent/app"
	"github.com/li904338203/tabtv/backend/internal/agent/domain"
	collab "github.com/li904338203/tabtv/backend/internal/collaboration/domain"
)

// list_marketing_options（MKT-T-18，dev/01 §5.3）：只读、免费，列此刻**在投**的创意 / Hook / 风格。
// 批量变体要给每个变体换 Hook / 风格时用（MKT-T-27）；只在营销会话下发（domain.toolSpecs 的 Scenes）。
//
// ⚠️ 只回 code、名称、说明、分类、标签、是否依赖出声 —— **没有片段原文**（MKT-AC-06）：片段由服务端在
// 生成时按钉住的版本追加，Agent 拿不到也不复述（MKT-F-43）。端口的结构体上本就没有那个字段。

const (
	maxMarketingOptionLimit     = 30
	defaultMarketingOptionLimit = 20
	// 名称 / 说明是运营录入的外部内容：截短后再进模型上下文。
	marketingNameRunes = 40
	marketingDescRunes = 120
)

var marketingKinds = map[string]bool{"": true, "creative": true, "hook": true, "style": true}

// ListMarketingOptions 是 list_marketing_options 工具。
type ListMarketingOptions struct{ Options app.MarketingOptionLister }

func (ListMarketingOptions) Name() domain.ToolName { return domain.ToolListMarketingOptions }
func (ListMarketingOptions) Paid() bool            { return false }
func (ListMarketingOptions) Action() collab.Action { return collab.ActionView }

func (ListMarketingOptions) Description() string {
	return "列出此刻可选的营销选项：kind=creative（创意）/ hook / style（风格），不填则三类都列；" +
		"可按 category（分类 code）与 q（关键词）筛选。只返回 code、名称、说明、标签与是否需要模型出声；" +
		"选项的具体写法由平台在生成时追加，你看不到也不需要复述。结果 more=true 时请缩小条件。"
}

func (ListMarketingOptions) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{` +
		`"kind":{"type":"string","enum":["creative","hook","style"],"description":"不填 = 三类都列"},` +
		`"category":{"type":"string","description":"分类 code"},` +
		`"q":{"type":"string","description":"按名称 / 说明的关键词"},` +
		`"limit":{"type":"integer","minimum":1,"maximum":30,"description":"每类最多几条，默认 20"}` +
		`},"additionalProperties":false}`)
}

type listMarketingOptionsArgs struct {
	Kind     string `json:"kind"`
	Category string `json:"category"`
	Q        string `json:"q"`
	Limit    int    `json:"limit"`
}

func (t ListMarketingOptions) Run(ctx context.Context, _ RunContext, args json.RawMessage) (ToolResult, error) {
	if t.Options == nil {
		return ToolResult{}, fmt.Errorf("营销选项暂不可用")
	}
	var a listMarketingOptionsArgs
	if err := ptDecode(args, &a, false); err != nil {
		return ToolResult{}, err
	}
	a.Kind = strings.TrimSpace(a.Kind)
	if !marketingKinds[a.Kind] {
		return ToolResult{}, fmt.Errorf("kind 只能是 creative / hook / style")
	}
	if a.Limit <= 0 {
		a.Limit = defaultMarketingOptionLimit
	}
	if a.Limit > maxMarketingOptionLimit {
		a.Limit = maxMarketingOptionLimit
	}
	res, err := t.Options.ListMarketingOptions(ctx, app.MarketingOptionsQuery{
		Kind: a.Kind, Category: strings.TrimSpace(a.Category), Q: strings.TrimSpace(a.Q), Limit: a.Limit})
	if err != nil {
		return ToolResult{}, fmt.Errorf("查询营销选项失败：%w", err)
	}
	if res.Items == nil {
		res.Items = []app.MarketingOption{}
	}
	for i := range res.Items {
		res.Items[i].Name, _ = ptCutRunes(res.Items[i].Name, marketingNameRunes)
		res.Items[i].Description, _ = ptCutRunes(res.Items[i].Description, marketingDescRunes)
	}
	return ToolResult{
		Content: ptJSON(res),
		Display: ptJSON(map[string]any{"kind": a.Kind, "count": len(res.Items)}),
	}, nil
}

var _ app.Tool = ListMarketingOptions{}
