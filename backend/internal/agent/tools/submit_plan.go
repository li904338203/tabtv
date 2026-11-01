package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/li904338203/tabtv/backend/internal/agent/app"
	"github.com/li904338203/tabtv/backend/internal/agent/domain"
	"github.com/li904338203/tabtv/backend/internal/agent/repo"
	collab "github.com/li904338203/tabtv/backend/internal/collaboration/domain"
)

// submit_plan（dev/01 §5.3、design/01 §6.2、design/03 §3.4 C）：发规划确认卡并挂起等批准。
//
// ⚠️ **金额由服务端逐项报价**（AG-07）：计划项里模型给的任何金额字段一律忽略 —— 解码时
// 允许未知字段正是为了「忽略」而不是「拒绝」（拒绝的话模型会反复重试同一个带金额的调用）。
// 报价走 PlanPricer，与批准时重新报价（AGT-26 PlanQuoter）是同一个映射；两边不一致的话
// 用户每点一次确认都会收到「价格已更新」。
// ⚠️ 一项不可用（模型下线、会员等级不够）不阻断其余项：卡片照发、该项带原因、不能批准。
// ⚠️ 但**模型自己写错的参数**（PricedItem.ArgFault：模型没有这个操作、inputs 不符合输入要求、
// 时长 / 张数非法）**不发卡**：整次调用作为工具错误退回，逐项写清第几项、原因、怎么改，
// 让模型在同一轮里改正后重新提交。起因（会话 105692702233853952）：模型给 text2video 凭空写了
// inputs:{image:1}，卡片合计 ✦0、「确认并开始」禁用，用户只能点「需要调整」再花一轮钱 ——
// 而这件事用户既看不懂也改不了。退回不扣费（本工具不是付费工具，不写额度），反复写错由
// Runner 的单轮调用上限（AGENT_TURN_MAX_CALLS）兜底。
// ⚠️ 再次 submit_plan 时作废本会话上一张待处理的规划卡：两张同时待批准的话，用户批准旧的那张，
// 额度就是按一份已经被改掉的规划算的。

const maxPlanItems = 20

// SubmitPlan 是 submit_plan 工具。
type SubmitPlan struct{ d PlanningDeps }

func (SubmitPlan) Name() domain.ToolName { return domain.ToolSubmitPlan }
func (SubmitPlan) Paid() bool            { return false }
func (SubmitPlan) Action() collab.Action { return collab.ActionEdit }

func (SubmitPlan) Description() string {
	return "提交创作规划，请用户确认后才能开始生成。列出将要生成的每一项（类型、模型、视频时长或图片张数、标题）；" +
		"费用由服务端逐项报价，不要自己填金额。提交后本轮暂停，用户确认后进入执行阶段，需要调整时会带着意见回到规划。" +
		"建议先把规划写进 /plan.md。" + planInputsRule +
		"计划项参数有误（模型不支持该操作、inputs 不符合输入要求、时长或张数非法）时不会发卡，会返回逐项的错误，按提示改正后重新提交。" +
		"营销会话里每项另带 key 与选图（productIndexes / picks），inputs 由服务端按选中的商品图、角色、源视频计算，不用填。"
}

func (SubmitPlan) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","required":["items"],"properties":{` +
		`"title":{"type":"string","description":"规划标题"},` +
		`"items":{"type":"array","minItems":1,"maxItems":20,"items":` + submitPlanItemSchema + `}` +
		`},"additionalProperties":false}`)
}

// submitPlanItemSchema 是 submit_plan 的计划项：在 quote 的形状上多了营销会话用的字段
// （key / variant / variantLabel / productIndexes / picks，MKT-T-20）。通用会话里这些字段被忽略。
var submitPlanItemSchema = strings.TrimSuffix(planSpecSchema, "}}") + `,` +
	`"key":{"type":"string","description":"仅营销会话：条目键（1~32 位字母数字下划线连字符，各项不同；执行 generate 时要带它）。不填按位置补 item1、item2…"},` +
	`"variant":{"type":"integer","minimum":1,"description":"仅营销会话：批量变体序号"},` +
	`"variantLabel":{"type":"string","description":"仅营销会话：变体说明（≤30 字）"},` +
	`"productIndexes":{"type":"array","items":{"type":"integer","minimum":1},"description":"仅营销会话（video / image）：选用的商品图序号（商品图 #n，从 1 起，按发送顺序）。不填按 #1 起取前若干张（以模型可收张数为上限）；写成 [] 表示本项不用商品图（如空镜头）；超过上限会被退回并告知最多几张。所写的生成方式不收图片时，服务端会换成该模型收图片的方式并在卡片上说明"},` +
	`"picks":{"type":"object","properties":{"creative":{"type":"string"},"hook":{"type":"string"},"style":{"type":"string"}},"additionalProperties":false,"description":"仅营销会话（video / image）：本项用的创意 / Hook / 风格的 code（如 cr_pain_point_fix，见「营销输入」或 list_marketing_options），不是名称。与会话里用户的选择一样时不用填（不填即沿用）；填空串表示本项不用该维度"},` +
	`"sourceVideo":{"type":"boolean","description":"仅营销会话（video）：本项是否以用户给的源视频为源做裂变。不填时会话有源视频的视频条目默认是。服务端判定走「视频生视频」（源视频作为输入，不用商品图）还是「重新生成」（按商品图与选项，不参考源视频），结果在 fissionRoute / fissionNote 里，照着向用户说明；operationCode 写不能走视频生视频时要用的操作"}}}`

type submitPlanArgs struct {
	Title string `json:"title"`
	// Items 用 RawMessage 逐项解码：计划项里多出的字段（credits、price…）忽略，顶层仍严格。
	Items []json.RawMessage `json:"items"`
}

// PlanCardContent 是回给模型的摘要（不含余额以外的账户信息）。
type planItemBrief struct {
	Label     string `json:"label"`
	Kind      string `json:"type"`
	ModelCode string `json:"modelCode"`
	// OperationCode 实际用的生成方式（服务端可能按所带的输入改过）：执行时 generate 照着用。
	OperationCode string `json:"operationCode,omitempty"`
	Credits       int    `json:"credits"`
	BlockedReason string `json:"blockedReason,omitempty"`
	// 营销条目（MKT-T-20）：执行时 generate 要带 key；选图与上限回给模型，便于它在说明里写清用了哪几张。
	Key            string `json:"key,omitempty"`
	ProductIndexes []int  `json:"productIndexes,omitempty"`
	ImageLimit     *int   `json:"imageLimit,omitempty"`
	// 素材裂变（MKT-T-28）：服务端定的路线与说明，Agent 向用户解释时照着说。
	FissionRoute string `json:"fissionRoute,omitempty"`
	FissionNote  string `json:"fissionNote,omitempty"`
}

func (t SubmitPlan) Run(ctx context.Context, rc RunContext, args json.RawMessage) (ToolResult, error) {
	if rc.State != domain.StatePlanning {
		return ToolResult{}, fmt.Errorf("只有规划阶段可以提交规划")
	}
	var a submitPlanArgs
	if err := ptDecode(args, &a, false); err != nil {
		return ToolResult{}, err
	}
	if len(a.Items) == 0 {
		return ToolResult{}, fmt.Errorf("items 不能为空")
	}
	if len(a.Items) > maxPlanItems {
		return ToolResult{}, fmt.Errorf("一次最多 %d 项", maxPlanItems)
	}
	// 规格三问（design/01 修订块 R-1，营销与通用会话都适用）：画幅 / 清晰度 / 时长里有「智能」又没确认的项时，不出卡。
	if err := checkSpecConfirmed(rc.Spec, a.Items); err != nil {
		return ToolResult{}, err
	}
	// 营销会话（MKT-T-20）：条目键、选图、选项钉版本、口播提示。通用会话 mk == nil，走原路径。
	mk, err := t.loadMarketing(ctx, rc)
	if err != nil {
		return ToolResult{}, err
	}
	// 变体上限（MKT-T-27、MKT-F-52）：在选图、钉版本、报价之前判 —— 超了不出卡，也不留任何副作用。
	if mk != nil {
		if err := t.checkVariants(a.Items); err != nil {
			return ToolResult{}, err
		}
	}
	// 通用会话的长片（有成片渲染项、且不止一个视频条目 = 脚本线）：视频条目是长片的分段，不逐条核对时长，
	// 改为核对成片时长、并确认一次生成确实放不下（checkLongFilm）。营销会话没有这条路：营销视频一律不拆。
	videoCount, _ := planShape(a.Items)
	longFilm := mk == nil && planIsLongFilm(a.Items)
	if mk != nil {
		mk.multiVideo = videoCount > 1
	}
	items := make([]domain.PlanItem, 0, len(a.Items))
	var faults, opNotes []string
	ops := newOpMatcher(t.d.Ops)
	if mk != nil {
		mk.ops = ops
	}
	for i, raw := range a.Items {
		var s planSpec
		if err := ptDecode(raw, &s, true); err != nil {
			return ToolResult{}, fmt.Errorf("第 %d 项：%w", i+1, err)
		}
		it, err := planItemFrom(s)
		if err != nil {
			return ToolResult{}, fmt.Errorf("第 %d 项：%w", i+1, err)
		}
		if mk != nil {
			if it, err = t.prepareItem(ctx, mk, i+1, raw, it); err != nil {
				return ToolResult{}, err
			}
		} else {
			// 一支视频不拆段（R-1）：每个视频条目都是完整的一支，时长就是用户选定的时长（营销条目在 prepareItem 里核对）。
			if !longFilm {
				if it, err = specDuration(rc.Spec, i+1, raw, it, videoCount > 1); err != nil {
					return ToolResult{}, err
				}
			}
			// 生成方式与输入对齐（营销条目在 prepareItem 里按选中的商品图与角色对齐）。
			var note string
			if it, note = ops.match(ctx, i+1, it, it.Inputs); note != "" {
				opNotes = append(opNotes, note)
			}
		}
		p, err := t.d.Pricer.PriceItem(ctx, it, rc.MemberLevelID)
		if err != nil {
			return ToolResult{}, fmt.Errorf("第 %d 项报价失败：%w", i+1, err)
		}
		if !p.Available && p.ArgFault != "" {
			faults = append(faults, planFaultLine(i+1, it, p)+fitsHint(ctx, ops, it, p))
		}
		item := p.Item
		if mk != nil {
			item = mk.applyBlock(i+1, item)
			// 裂变条目的说明（走哪条、花多少）按这次报价写（MKT-T-28）。
			item.FissionNote = domain.FissionNote(item)
		}
		items = append(items, item)
	}
	// 参数错误一律先退回，哪怕只有一项：发出去的卡片不可批准，而用户改不了模型写的参数。
	if len(faults) > 0 {
		return ToolResult{}, fmt.Errorf("规划未提交（没有发卡、没有扣费），以下计划项参数有误，请改正后重新调用 submit_plan：\n%s",
			strings.Join(faults, "\n"))
	}
	if longFilm {
		if err := t.checkLongFilm(ctx, rc, items); err != nil {
			return ToolResult{}, err
		}
	}
	balance, err := t.d.Balances.Balance(ctx, rc.PayerAccountID)
	if err != nil {
		return ToolResult{}, fmt.Errorf("查询余额失败：%w", err)
	}
	total, shortfall, canApprove, ok := domain.PlanTotals(items, balance)
	if !ok {
		return ToolResult{}, fmt.Errorf("报价金额超出范围")
	}

	payload := domain.PlanConfirmPayload{Items: items, Total: total, Balance: balance,
		Shortfall: shortfall, CanApprove: canApprove}
	if mk != nil {
		if mk.notice != "" {
			payload.Notices = []string{mk.notice}
		}
		opNotes = append(opNotes, mk.opNotes...)
		// 新钉的选项先落进会话再发卡：卡片上的 (code, version) 必须在会话里查得到（执行时按它拼片段）。
		if err := t.persistPins(ctx, rc, mk); err != nil {
			return ToolResult{}, err
		}
	}
	// 换了生成方式要说出来：用户在卡片上看到用的是哪种方式（价格可能随之变化）。
	payload.Notices = append(payload.Notices, opNotes...)
	payload.Title, _ = ptCutRunes(ptTrim(a.Title), maxAskTitle)
	if payload.Title == "" {
		payload.Title = "请确认创作规划"
	}
	// 记规划文档版本：批准时比对，文档在确认前被改过就作废回规划（AGT-26）。
	if t.d.Documents != nil {
		d, err := t.d.Documents.Get(ctx, rc.WorkspaceID, rc.ProjectID, rc.Role, PlanDocPath)
		switch {
		case err == nil:
			payload.DocPath, payload.PlanDocVersion = PlanDocPath, d.Version
		case !errors.Is(err, app.ErrDocumentNotFound):
			return ToolResult{}, fmt.Errorf("读取规划文档失败：%w", err)
		}
	}

	// 作废上一张待处理的规划卡（先作废再发新卡：发卡失败时模型重试会再作废一次，是空操作）。
	pending, err := t.d.Store.PendingBySession(ctx, rc.SessionID)
	if err != nil {
		return ToolResult{}, fmt.Errorf("查询待处理卡片失败：%w", err)
	}
	for _, c := range pending {
		if c.Kind != domain.CardPlanConfirm {
			continue
		}
		changed, err := t.d.Store.VoidCard(ctx, t.d.Store.Pool(), c.ID)
		if err != nil {
			return ToolResult{}, fmt.Errorf("作废旧规划卡失败：%w", err)
		}
		if changed {
			ptPublishCard(ctx, t.d, rc.ProjectID, rc.SessionID, c.ID, c.Kind, domain.CardVoided)
		}
	}
	card, err := t.d.Store.InsertCard(ctx, t.d.Store.Pool(), repo.Card{ID: t.d.NextID(),
		SessionID: rc.SessionID, StepID: rc.StepID, Kind: domain.CardPlanConfirm, Payload: ptJSON(payload)})
	if err != nil {
		return ToolResult{}, fmt.Errorf("发卡失败：%w", err)
	}
	ptPublishCard(ctx, t.d, rc.ProjectID, rc.SessionID, card.ID, domain.CardPlanConfirm, domain.CardPending)

	briefs := make([]planItemBrief, len(items))
	for i, it := range items {
		briefs[i] = planItemBrief{Label: it.Label, Kind: it.Kind, ModelCode: it.ModelCode,
			OperationCode: it.OperationCode, Credits: it.Credits,
			BlockedReason: it.BlockedReason, Key: it.Key, ProductIndexes: it.ProductIndexes, ImageLimit: it.ImageLimit,
			FissionRoute: it.FissionRoute, FissionNote: it.FissionNote}
	}
	content := map[string]any{"cardId": fmt.Sprint(card.ID), "status": "awaiting_approval",
		"items": briefs, "total": total, "balance": balance, "canApprove": canApprove}
	if shortfall > 0 {
		content["shortfall"] = shortfall
	}
	if len(payload.Notices) > 0 {
		content["notices"] = payload.Notices
	}
	content["note"] = "规划确认卡已发给用户，本轮到此暂停。"
	if !canApprove {
		content["note"] = "规划确认卡已发给用户，但当前无法批准（有不可用项或余额不足），用户可能会要求调整。本轮到此暂停。"
	}
	return ToolResult{
		Content: ptJSON(content),
		Display: ptJSON(map[string]any{"cardId": fmt.Sprint(card.ID), "total": total, "canApprove": canApprove,
			"itemCount": len(items)}),
		Suspend: &Suspension{Event: domain.EvSubmitPlan, CardID: card.ID, Reason: "awaiting_approval"},
	}, nil
}

var _ app.Tool = SubmitPlan{}
