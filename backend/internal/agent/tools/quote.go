package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/li904338203/tabtv/backend/internal/agent/app"
	"github.com/li904338203/tabtv/backend/internal/agent/domain"
	collab "github.com/li904338203/tabtv/backend/internal/collaboration/domain"
)

// quote（dev/01 §5.3）：包 QuoteWithFacts，返回金额、单位与不可用原因。
//
// ⚠️ 与 submit_plan、批准时重新报价**同一个映射**（planItemFrom → PlanPricer →
// wiring.PlanQuoteRequest）：模型用 quote 问到的价、卡片上的价、批准时的价三者一致。

// 计划项的产出类型（与 PlanItem.Kind 一致）。
//
// 多段（AGT-50）多出四类：storyboard（分镜，一次同步调用）、compose（提示词合成，按条）、
// asset（资产设定图，按张）、render（成片渲染，按秒、不绑模型）。分镜图组的图仍是 image、
// 视频组每段仍是一条 video（逐段列出，每段一个计划项）。
var planKinds = map[string]bool{"video": true, "image": true, "audio": true,
	"storyboard": true, "compose": true, "asset": true, "render": true}

// planKindLabels 是计划项没写标题时的默认标题。
var planKindLabels = map[string]string{"video": "视频", "image": "图片", "audio": "音频",
	"storyboard": "分镜", "compose": "提示词合成", "asset": "资产设定图", "render": "成片渲染"}

const (
	// defaultVideoSeconds：视频没给时长时按 5 秒报（与计价 per_second 的缺省一致，§5.2）。
	defaultVideoSeconds = 5
	maxPlanQuantity     = 600
	maxPlanCount        = 20
	maxPlanBatch        = 200
)

// planSpec 是模型给的一项「要生成什么」（quote 与 submit_plan 共用）。
//
// ⚠️ 没有任何金额字段：金额只能由服务端报价得出（AG-07）。
type planSpec struct {
	Type          string         `json:"type"`
	ModelCode     string         `json:"modelCode"`
	OperationCode string         `json:"operationCode"`
	DurationSec   int            `json:"durationSec"`
	Count         int            `json:"count"`
	Title         string         `json:"title"`
	Inputs        map[string]int `json:"inputs"`
}

const planSpecSchema = `{"type":"object","required":["type","modelCode"],"properties":{` +
	`"type":{"type":"string","enum":["video","image","audio","storyboard","compose","asset","render"],` +
	`"description":"storyboard=分镜一次；compose=提示词按条；asset=资产设定图按张；image=分镜图按张；video=每段一条；render=成片按秒（不需要 modelCode）"},` +
	`"modelCode":{"type":"string","description":"render 以外必填"},` +
	`"operationCode":{"type":"string","description":"不给则按类型取默认：text2video / text2img / text2audio / storyboard_gen / prompt_compose / asset_gen"},` +
	`"durationSec":{"type":"integer","minimum":1,"description":"视频每段时长或成片时长（秒）；视频默认 5"},` +
	`"count":{"type":"integer","minimum":1,"maximum":200,"description":"张数 / 条数，默认 1；视频请逐段列出"},` +
	`"title":{"type":"string"},` +
	`"inputs":{"type":"object","additionalProperties":{"type":"integer"},"description":"按输入角色计数，如 {\"image\":1}。` +
	`只能填用户真实提供（附件 / 引用）或画布上已有的媒体数量；纯文生（text2video / text2img 等）不要填。` +
	`有参考图时改用接收图片的操作（如 img2video / img2img）"}}}`

// planInputsRule 是 quote / submit_plan 工具描述里关于 inputs 的同一句规则。
const planInputsRule = "inputs 只能填用户真实提供（附件 / 引用）或画布上已有的媒体数量，不要凭空声明；" +
	"纯文生（text2video / text2img 等）不要填 inputs，有参考图时改用接收图片的操作（如 img2video / img2img）。"

// imageOpFor 是「纯文生写了参考图」时建议改用的操作（只用于错误提示，不参与报价）。
var imageOpFor = map[string]string{"video": "img2video", "image": "img2img"}

// planFaultLine 把一项参数错误写成给模型看的一行：第几项、哪个模型 / 操作、原因、怎么改。
func planFaultLine(n int, it domain.PlanItem, p app.PricedItem) string {
	where := []string{it.Label}
	if it.ModelCode != "" {
		where = append(where, it.ModelCode)
	}
	if p.Operation != "" {
		where = append(where, p.Operation)
	}
	line := fmt.Sprintf("第 %d 项（%s）：%s。", n, strings.Join(where, " · "), strings.TrimRight(p.BlockedReason, "。"))
	return line + "改法：" + planFaultFix(it, p)
}

// planFaultFix 按参数错误类别给出改法（quote 的 fix 字段与 submit_plan 的退回消息共用）。
func planFaultFix(it domain.PlanItem, p app.PricedItem) string {
	switch p.ArgFault {
	case app.PlanFaultOperation:
		return "确认 modelCode 与 operationCode 写对了；该模型不支持这个操作时，换用它支持的操作，或换一个支持此操作的模型（可先用 quote 核对）。"
	case app.PlanFaultInputs:
		var media []string
		for role, c := range it.Inputs {
			if c > 0 && role != "text" {
				media = append(media, role)
			}
		}
		sort.Strings(media)
		if strings.HasPrefix(p.Operation, "text2") && len(media) > 0 {
			alt := imageOpFor[it.Kind]
			if alt == "" {
				alt = "接收该媒体的操作"
			}
			return fmt.Sprintf("%s 是纯文生操作，不接受 %s 输入。用户没有提供参考媒体、画布上也没有可用的媒体时，去掉 inputs；"+
				"确有参考图时改用 %s。", p.Operation, strings.Join(media, " / "), alt)
		}
		return "按上面的要求调整 inputs 的角色与数量。inputs 只能填用户真实提供或画布上已有的媒体数量；没有参考媒体时去掉 inputs。"
	case app.PlanFaultQuantity:
		return "按上面的范围调整 durationSec / count（视频单次最长时长以模型目录为准，超出时缩短或拆成多段）。"
	}
	return "按上面的原因调整这一项后重新提交。"
}

// fitsHint 是「操作不对 / 输入不符」时附上的该模型可用生成方式（让模型照着改，而不是把参考图去掉）。
func fitsHint(ctx context.Context, ops *opMatcher, it domain.PlanItem, p app.PricedItem) string {
	switch p.ArgFault {
	case app.PlanFaultOperation, app.PlanFaultInputs:
		return ops.hint(ctx, it)
	}
	return ""
}

// planItemFrom 把模型给的一项换成计划项（还没报价，Credits=0）。
func planItemFrom(s planSpec) (domain.PlanItem, error) {
	kind := strings.TrimSpace(s.Type)
	if !planKinds[kind] {
		return domain.PlanItem{}, fmt.Errorf("type 只能是 video / image / audio / storyboard / compose / asset / render")
	}
	model := strings.TrimSpace(s.ModelCode)
	if model == "" && kind != "render" {
		return domain.PlanItem{}, fmt.Errorf("缺少 modelCode")
	}
	if kind == "render" {
		model = "" // 渲染不绑模型：按 timeline_render 的兜底价报（与真实渲染同一个口径）
	}
	for role, n := range s.Inputs {
		if n < 0 || n > maxPlanCount {
			return domain.PlanItem{}, fmt.Errorf("inputs.%s 超出范围", role)
		}
	}
	it := domain.PlanItem{Kind: kind, ModelCode: model, OperationCode: strings.TrimSpace(s.OperationCode),
		Inputs: s.Inputs}
	it.Label, _ = ptCutRunes(strings.TrimSpace(s.Title), 60)
	switch kind {
	case "video":
		if s.Count > 1 {
			return domain.PlanItem{}, fmt.Errorf("视频请逐条列出（每条一个计划项），不要用 count")
		}
		d := s.DurationSec
		if d == 0 {
			d = defaultVideoSeconds
		}
		if d < 1 || d > maxPlanQuantity {
			return domain.PlanItem{}, fmt.Errorf("durationSec 超出范围")
		}
		it.Unit, it.Quantity = "second", d
	case "render":
		if s.DurationSec < 1 || s.DurationSec > maxPlanQuantity {
			return domain.PlanItem{}, fmt.Errorf("render 要给成片时长 durationSec（1~%d 秒）", maxPlanQuantity)
		}
		if s.Count > 1 {
			return domain.PlanItem{}, fmt.Errorf("render 不用 count")
		}
		it.Unit, it.Quantity = "second", s.DurationSec
	default: // image / audio / storyboard / compose / asset：按张 / 条
		if s.DurationSec != 0 && kind != "audio" {
			return domain.PlanItem{}, fmt.Errorf("%s 没有 durationSec", kind)
		}
		c := s.Count
		if c == 0 {
			c = 1
		}
		limit := maxPlanCount
		if kind == "compose" || kind == "asset" || kind == "image" {
			// 多段的分镜图 / 资产图 / 提示词按镜头数计，几十条是常态。
			limit = maxPlanBatch
		}
		if c < 1 || c > limit {
			return domain.PlanItem{}, fmt.Errorf("count 只能是 1~%d", limit)
		}
		it.Unit, it.Quantity = "image", c
		if kind == "audio" || kind == "storyboard" || kind == "compose" {
			it.Unit = "time"
		}
	}
	if it.Label == "" {
		it.Label = planKindLabels[kind]
	}
	return it, nil
}

// Quote 是 quote 工具。Ops 为 nil 时不调整生成方式。
type Quote struct {
	Pricer app.PlanPricer
	Ops    app.OperationCatalog
}

func (Quote) Name() domain.ToolName { return domain.ToolQuote }
func (Quote) Paid() bool            { return false }
func (Quote) Action() collab.Action { return collab.ActionView }

func (Quote) Description() string {
	return "估算一次生成的费用（与用户在节点上看到的价同一套报价）。不扣费。" +
		"返回 credits、计价单位；不可用时返回原因与 userFixable（用户自己能否解决），" +
		"参数写错时另带 argumentError 与 fix（怎么改）。" + planInputsRule
}

func (Quote) Schema() json.RawMessage { return json.RawMessage(planSpecSchema) }

func (t Quote) Run(ctx context.Context, rc RunContext, args json.RawMessage) (ToolResult, error) {
	var s planSpec
	if err := ptDecode(args, &s, false); err != nil {
		return ToolResult{}, err
	}
	it, err := planItemFrom(s)
	if err != nil {
		return ToolResult{}, err
	}
	// 生成方式与输入对齐（带了图却写成 / 默认成文生视频时换成收得下的那一种）。
	ops := newOpMatcher(t.Ops)
	it, switchNote := ops.match(ctx, 1, it, it.Inputs)
	p, err := t.Pricer.PriceItem(ctx, it, rc.MemberLevelID)
	if err != nil {
		return ToolResult{}, fmt.Errorf("报价失败：%w", err)
	}
	out := map[string]any{"available": p.Available, "modelCode": it.ModelCode, "type": it.Kind,
		"quantity": it.Quantity, "quantityUnit": it.Unit}
	if p.Operation != "" {
		out["operationCode"] = p.Operation
	}
	if switchNote != "" {
		out["note"] = switchNote
	}
	if p.Available {
		out["credits"], out["unit"] = p.Item.Credits, p.Unit
	} else {
		out["blockedCode"], out["blockedReason"], out["userFixable"] = p.BlockedCode, p.BlockedReason, p.UserFixable
		if p.ArgFault != "" {
			out["argumentError"], out["fix"] = true, planFaultFix(it, p)+fitsHint(ctx, ops, it, p)
		}
	}
	return ToolResult{
		Content: ptJSON(out),
		Display: ptJSON(map[string]any{"credits": p.Item.Credits, "available": p.Available,
			"modelCode": it.ModelCode, "label": it.Label, "blockedReason": p.BlockedReason}),
	}, nil
}

var _ app.Tool = Quote{}
