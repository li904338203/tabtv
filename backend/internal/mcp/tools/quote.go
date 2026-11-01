package tools

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	collab "github.com/li904338203/tabtv/backend/internal/collaboration/domain"
	"github.com/li904338203/tabtv/backend/internal/mcp/app"
	"github.com/li904338203/tabtv/backend/internal/mcp/domain"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// quote（MCPT-05，MCP-F-29）：**MCP 内唯一的价格来源**。
//
// ⚠️ 单项（item）的字段名与 Web 端 `POST /generations/estimate` 请求体完全相同、原样透传，
// **不填任何默认值**（MCP-AC-05：同一组参数两边逐字一致）；计划项（items）与站内 Agent 规划卡同价。
// ⚠️ 只读：不扣费、不改用户可见状态（签发的 quoteId 只落 MCP 自己的表）。
// 有任一项不可用时**不签发 quoteId**（不能让人确认一个跑不了的计划），其余项照常报价。

// planKinds 是计划项的类型（与 Agent 计划项 Kind 一致）；只做形状校验，价格与可用性仍由报价路径判定。
var planKinds = []string{"video", "image", "audio", "storyboard", "compose", "asset", "render"}

// planUnits 是计划项数量的单位（agent/domain.PlanItem.Unit）。
var planUnits = []string{"", "second", "image", "time"}

const (
	maxPlanQuantity = 600 // 与 Agent 计划项的时长 / 数量上限同值
	maxInputCount   = 20
)

type quoteItemIn struct {
	ModelCode   string         `json:"modelCode,omitempty" jsonschema:"模型代码（见 models）"`
	Operation   string         `json:"operation,omitempty" jsonschema:"操作码，如 text2img、img2video、text2video"`
	Count       int            `json:"count,omitempty" jsonschema:"张数 / 次数"`
	DurationSec int            `json:"durationSec,omitempty" jsonschema:"时长（秒），按秒计价的视频 / 音频用"`
	Text        string         `json:"text,omitempty" jsonschema:"文本（按千字计价的操作按 Unicode 码点计），原样计价、不截断"`
	Inputs      map[string]int `json:"inputs,omitempty" jsonschema:"按输入角色计数，如 {\"image\":1}"`
}

type quotePlanItemIn struct {
	Kind          string         `json:"kind" jsonschema:"video / image / audio / storyboard / compose / asset / render"`
	ModelCode     string         `json:"modelCode,omitempty" jsonschema:"render 以外必填"`
	OperationCode string         `json:"operationCode,omitempty" jsonschema:"不给则按 kind 取默认操作"`
	Quantity      int            `json:"quantity" jsonschema:"数量：秒数（unit 为 second 或 kind 为 render 时）或张数 / 条数，1~600"`
	Unit          string         `json:"unit,omitempty" jsonschema:"second / image / time；second 表示 quantity 是时长"`
	Inputs        map[string]int `json:"inputs,omitempty" jsonschema:"按输入角色计数，如 {\"image\":1}"`
}

type quoteIn struct {
	Item  *quoteItemIn      `json:"item,omitempty" jsonschema:"单项报价（与 items 二选一）"`
	Items []quotePlanItemIn `json:"items,omitempty" jsonschema:"计划项报价（与 item 二选一），最多 20 项"`
}

type quoteLineOut struct {
	Index            int    `json:"index" jsonschema:"项的序号（从 0 起）"`
	Credits          int    `json:"credits" jsonschema:"这一项的积分（整数）；不可用时为 0"`
	Unit             string `json:"unit,omitempty" jsonschema:"计价单位：per_time / per_image / per_second / per_kchar"`
	EstimatedSeconds int    `json:"estimatedSeconds,omitempty"`
	Available        bool   `json:"available"`
	BlockedCode      string `json:"blockedCode,omitempty" jsonschema:"不可用的原因码（业务码名）"`
	BlockedReason    string `json:"blockedReason,omitempty"`
	UserFixable      bool   `json:"userFixable" jsonschema:"用户自己能解决（升级会员 / 调整参数）；否则需要运营处理"`
}

type quoteOut struct {
	Items          []quoteLineOut `json:"items"`
	TotalCredits   int            `json:"totalCredits" jsonschema:"合计积分（整数）"`
	Balance        int            `json:"balance" jsonschema:"付费主体当前可用积分"`
	Shortfall      int            `json:"shortfall" jsonschema:"差额 = max(0, totalCredits - balance)"`
	PayerAccountID string         `json:"payerAccountId" jsonschema:"费用承担账户"`
	QuoteID        string         `json:"quoteId,omitempty" jsonschema:"报价凭证；有不可用项时不签发"`
	ExpiresAt      string         `json:"expiresAt,omitempty" jsonschema:"quoteId 过期时间（UTC）"`
	NotIssued      string         `json:"notIssuedReason,omitempty" jsonschema:"未签发 quoteId 的原因"`
}

func quoteTool(d Deps) app.Tool {
	return app.Define(app.Tool{
		Name:    "quote",
		Toolset: domain.ToolsetGeneration,
		Description: "报价：这是 MCP 内唯一的价格来源，不要自己估算费用。item 报单项（字段与站内报价相同：" +
			"modelCode / operation / count / durationSec / text / inputs）；items 报一组计划项" +
			"（kind / modelCode / operationCode / quantity / unit / inputs，与站内 Agent 规划同价）。" +
			"返回逐项金额、合计、余额、差额与 quoteId（有效期内供确认花钱使用；有不可用项时不签发并说明原因）。" +
			"不扣费、不改任何内容。",
		Methods: []app.Method{{Action: collab.ActionView}},
	}, func(ctx context.Context, _ string, in quoteIn) (quoteOut, string, error) {
		p, err := principal(ctx)
		if err != nil {
			return quoteOut{}, "", err
		}
		if d.Quotes == nil {
			return quoteOut{}, "", unwired("Quotes")
		}
		req, err := quoteRequestFrom(in)
		if err != nil {
			return quoteOut{}, "", err
		}
		r, err := d.Quotes.Quote(ctx, p, req)
		if err != nil {
			return quoteOut{}, "", err
		}
		out := quoteOut{Items: make([]quoteLineOut, len(r.Lines)), TotalCredits: r.Total, Balance: r.Balance,
			Shortfall: r.Shortfall, PayerAccountID: idStr(r.PayerAccountID), QuoteID: idStr(r.QuoteID),
			NotIssued: r.NotIssuedReason}
		if r.QuoteID != 0 {
			out.ExpiresAt = ts(r.ExpiresAt)
		}
		for i, l := range r.Lines {
			out.Items[i] = quoteLineOut{Index: i, Credits: l.Credits, Unit: l.Unit, EstimatedSeconds: l.EstimatedSeconds,
				Available: l.Available, BlockedCode: l.BlockedCode, BlockedReason: l.BlockedReason,
				UserFixable: l.UserFixable}
		}
		return out, quoteSummary(out), nil
	})
}

func quoteRequestFrom(in quoteIn) (app.QuoteRequest, error) {
	if (in.Item == nil) == (len(in.Items) == 0) {
		return app.QuoteRequest{}, httpx.Errf(errcode.InvalidArgument, "item 与 items 必须且只能给一个")
	}
	if it := in.Item; it != nil {
		// ⚠️ 原样透传，不 trim、不补默认值（与 estimate 逐字一致）。
		return app.QuoteRequest{Item: &app.QuoteItem{ModelCode: it.ModelCode, Operation: it.Operation,
			Count: it.Count, DurationSec: it.DurationSec, Text: it.Text, Inputs: it.Inputs}}, nil
	}
	if len(in.Items) > app.MaxQuotePlanItems {
		return app.QuoteRequest{}, httpx.Errf(errcode.InvalidArgument,
			fmt.Sprintf("items 一次最多 %d 项", app.MaxQuotePlanItems))
	}
	items := make([]app.PlanQuoteItem, len(in.Items))
	for i, it := range in.Items {
		where := "items[" + strconv.Itoa(i) + "]"
		kind := strings.TrimSpace(it.Kind)
		if !slices.Contains(planKinds, kind) {
			return app.QuoteRequest{}, httpx.Errf(errcode.InvalidArgument,
				where+".kind 只能是 "+strings.Join(planKinds, " / "))
		}
		if kind != "render" && strings.TrimSpace(it.ModelCode) == "" {
			return app.QuoteRequest{}, httpx.Errf(errcode.InvalidArgument, where+" 缺少 modelCode")
		}
		if it.Quantity < 1 || it.Quantity > maxPlanQuantity {
			return app.QuoteRequest{}, httpx.Errf(errcode.InvalidArgument,
				fmt.Sprintf("%s.quantity 只能是 1~%d", where, maxPlanQuantity))
		}
		if !slices.Contains(planUnits, it.Unit) {
			return app.QuoteRequest{}, httpx.Errf(errcode.InvalidArgument, where+".unit 只能是 second / image / time")
		}
		for role, n := range it.Inputs {
			if n < 0 || n > maxInputCount {
				return app.QuoteRequest{}, httpx.Errf(errcode.InvalidArgument,
					fmt.Sprintf("%s.inputs.%s 只能是 0~%d", where, role, maxInputCount))
			}
		}
		model := strings.TrimSpace(it.ModelCode)
		if kind == "render" {
			model = "" // 渲染不绑模型：按 timeline_render 的兜底价报（与 Agent、真实渲染同一个口径）
		}
		items[i] = app.PlanQuoteItem{Kind: kind, ModelCode: model, OperationCode: strings.TrimSpace(it.OperationCode),
			Quantity: it.Quantity, Unit: it.Unit, Inputs: it.Inputs}
	}
	return app.QuoteRequest{Items: items}, nil
}

func quoteSummary(o quoteOut) string {
	s := fmt.Sprintf("%d 项合计 %d 积分，余额 %d", len(o.Items), o.TotalCredits, o.Balance)
	if o.Shortfall > 0 {
		s += fmt.Sprintf("，还差 %d", o.Shortfall)
	}
	if o.QuoteID != "" {
		return s + "；quoteId " + o.QuoteID + "（" + o.ExpiresAt + " 前有效）"
	}
	return s + "；" + o.NotIssued
}
