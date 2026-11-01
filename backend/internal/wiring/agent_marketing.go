package wiring

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"time"

	agentapp "github.com/li904338203/tabtv/backend/internal/agent/app"
	agentdomain "github.com/li904338203/tabtv/backend/internal/agent/domain"
	catalogapp "github.com/li904338203/tabtv/backend/internal/catalog/app"
	catalogdomain "github.com/li904338203/tabtv/backend/internal/catalog/domain"
	marketingapp "github.com/li904338203/tabtv/backend/internal/marketing/app"
	mktdomain "github.com/li904338203/tabtv/backend/internal/marketing/domain"
)

// 技能场景扩展（MKT-T-05）的端口适配：agent 定义 CopyChecker / SlotDeclValidator，
// 这里接到 marketing/domain 的唯一实现（MKD-12）。agent 与 marketing 互不 import。

// AgentSkillCopy 把 marketing/domain.CheckTalkCopy 接成 agentapp.CopyChecker。
type AgentSkillCopy struct{}

func (AgentSkillCopy) CheckCopy(fields map[string]string, tags []string) (errs, warns []agentapp.CopyIssue) {
	e, w := mktdomain.CheckTalkCopy(fields, tags)
	return toCopyIssues(e), toCopyIssues(w)
}

// DefaultMarketingMaxProductImages 是每个商品图片上限的兜底值（配置项 SEEAITV_MARKETING_MAX_PRODUCT_IMAGES
// 未装配时用）。MKT-T-33 起与配置默认值一起 20 → 10（MK-22：每个商品最多 10 张），两处同一个数。
const DefaultMarketingMaxProductImages = mktdomain.DefaultMaxProductImages

// AgentSkillSlots 把 marketing/domain 的槽位声明规则接成 agentapp.SlotDeclValidator 与 SlotDeclNormalizer。
//
// ⚠️ 规范化（旧形状 → 新形状，MKD-26）只有 marketing/domain.NormalizeSlotDecls 一处：保存（这里的
// ValidateSlotDecl）、建会话读技能（AgentScene.ValidateSlotFill）、技能下发（NormalizeSlotDecl）三处都调它。
// MKT-T-37 起商品槽数的是商品（1 个）不是商品图，声明校验不再依赖商品图上限配置。
type AgentSkillSlots struct{}

// decodeSlotDecls 严格解码（未知字段直接拒：槽位上拼错一个键如 max_count 不会报错，只会让那一项按零值存下去）。
func decodeSlotDecls(raw json.RawMessage) ([]mktdomain.SlotDecl, error) {
	var decls []mktdomain.SlotDecl
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&decls); err != nil {
		return nil, err
	}
	if decls == nil {
		decls = []mktdomain.SlotDecl{}
	}
	return decls, nil
}

// ValidateSlotDecl 实现 SlotDeclValidator：解码 → 规范化 → 校验。规范化的提醒以
// code=normalized（agentapp.SlotDeclNormalizedCode）混在问题里返回，由调用方分流成 warnings。
func (AgentSkillSlots) ValidateSlotDecl(raw json.RawMessage) (json.RawMessage, []agentapp.CopyIssue) {
	decls, err := decodeSlotDecls(raw)
	if err != nil {
		return nil, []agentapp.CopyIssue{{Field: "inputSlots", Code: string(mktdomain.IssueBadFormat),
			Message: "输入槽声明不是合法的数组或含未知字段：" + err.Error()}}
	}
	decls, warns := mktdomain.NormalizeSlotDecls(decls)
	norm, err := json.Marshal(decls)
	if err != nil {
		return nil, []agentapp.CopyIssue{{Field: "inputSlots", Code: string(mktdomain.IssueBadFormat),
			Message: "输入槽声明无法序列化：" + err.Error()}}
	}
	return norm, append(toCopyIssues(mktdomain.ValidateSlotDecl(decls)), toCopyIssues(warns)...)
}

// NormalizeSlotDecl 实现 SlotDeclNormalizer（读路径）：解不开的原样返回。
func (AgentSkillSlots) NormalizeSlotDecl(raw json.RawMessage) json.RawMessage {
	var decls []mktdomain.SlotDecl
	if err := json.Unmarshal(raw, &decls); err != nil || decls == nil {
		return raw
	}
	decls, _ = mktdomain.NormalizeSlotDecls(decls)
	out, err := json.Marshal(decls)
	if err != nil {
		return raw
	}
	return out
}

var (
	_ agentapp.SlotDeclValidator  = AgentSkillSlots{}
	_ agentapp.SlotDeclNormalizer = AgentSkillSlots{}
)

func toCopyIssues(in []mktdomain.Issue) []agentapp.CopyIssue {
	out := make([]agentapp.CopyIssue, 0, len(in))
	for _, i := range in {
		out = append(out, agentapp.CopyIssue{Field: i.Field, Code: string(i.Code), Message: i.Message})
	}
	return out
}

// AgentSkillModels 把模型目录接成 agentapp.SkillModels（用户端技能列表的模型显示名，MKT-D-13）。
type AgentSkillModels struct{ Catalog *catalogapp.Service }

func (a AgentSkillModels) SkillModels(ctx context.Context) ([]agentapp.SkillModelInfo, error) {
	list, err := a.Catalog.ListModels(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]agentapp.SkillModelInfo, 0, len(list))
	for _, x := range list {
		out = append(out, agentapp.SkillModelInfo{Code: x.Code, Name: x.Name, Domain: x.Domain,
			Status: x.Status, AgentCapable: x.IsAgentCapable})
	}
	return out, nil
}

// AgentScene 把营销域接成 agentapp.SceneResolver（MKT-T-17）：选项钉版本走 marketing/app.Resolve
// （可见性判定只有一处，MKD-13），槽位校验走 marketing/domain.ValidateSlotFill（槽位规则只有一份）。
//
// ⚠️ Resolve 的 outputType 传空：建会话时模型可能还是「智能匹配」，适用产出与出声判在 submit_plan。
type AgentScene struct {
	Svc *marketingapp.Service
}

// Resolve 实现 SceneResolver：只把 (code, version) 与该版本名称交给 agent，片段原文不出营销域。
func (a AgentScene) Resolve(ctx context.Context, picks []agentapp.ScenePickRef, now time.Time) (
	[]agentapp.ScenePin, error) {
	refs := make([]marketingapp.PickRef, 0, len(picks))
	for _, p := range picks {
		refs = append(refs, marketingapp.PickRef{Kind: mktdomain.Kind(p.Kind), Code: p.Code})
	}
	snaps, err := a.Svc.Resolve(ctx, refs, "", now)
	if err != nil {
		return nil, err
	}
	out := make([]agentapp.ScenePin, 0, len(snaps))
	for _, sn := range snaps {
		out = append(out, agentapp.ScenePin{Kind: string(sn.Kind), Code: sn.Code, Version: sn.Version, Name: sn.Name})
	}
	return out, nil
}

// ValidateSlotFill 实现 SceneResolver。decl 为空（未选技能 / 技能未声明槽位）时用场景默认声明。
func (a AgentScene) ValidateSlotFill(decl json.RawMessage, fill agentapp.SceneSlotFill) []agentapp.SlotIssue {
	var decls []mktdomain.SlotDecl
	if t := bytes.TrimSpace(decl); len(t) > 0 && !bytes.Equal(t, []byte("null")) {
		if err := json.Unmarshal(t, &decls); err != nil {
			// 存进去的是规范化结果（MKT-T-05），解不开说明数据坏了：拒绝而不是退回默认声明 ——
			// 退回的话技能声明的「源视频必填」会悄悄消失。
			return []agentapp.SlotIssue{{Field: "skillCode", Code: string(mktdomain.IssueBadFormat),
				Message: "技能的输入槽配置损坏，请联系运营"}}
		}
	}
	if len(decls) == 0 {
		decls = mktdomain.DefaultSlots()
	}
	// 库里的技能可能还是旧形状（MKT-T-09 录入）：解码处规范化（MKD-26），否则商品槽按 image 判，
	// 新请求的 marketing_product 一律被当成「该槽只收 image」拒掉。
	decls, _ = mktdomain.NormalizeSlotDecls(decls)
	issues := mktdomain.ValidateSlotFill(decls, mktdomain.SlotFill{Refs: fill.Refs, Fields: fill.Fields})
	out := make([]agentapp.SlotIssue, 0, len(issues))
	for _, i := range issues {
		out = append(out, agentapp.SlotIssue{Slot: i.Slot, Field: i.Field, Code: string(i.Code), Message: i.Message})
	}
	return out
}

// ---------------------------------------------------------------- 营销规划（MKT-T-20）

// AgentPlanScene 把目录与营销域接成 agentapp.PlanScene（submit_plan 的营销分支）。
//
//   - 选图上限：catalog.InputLimit（与报价同一份能力矩阵，不另写判断，MKR-2）；
//   - 选项：已钉住的按版本历史取（Snapshot，不看是否在投），新的按 Resolve(outputType) 钉当前版本；
//   - 不可用判定：marketing/domain.BlockReason（产出不符 → 不出声 → 缺输入），出声取模型目录的声明；
//   - 口播：marketing/domain.HasTag(TagTalk) → TalkDisclaimer（提示句只有营销域一份）。
type AgentPlanScene struct {
	Catalog   *catalogapp.Service
	Marketing *marketingapp.Service
}

// ImageLimit 实现 PlanScene：操作码没写时按类型取默认（PlanQuoteRequest，与报价同一个映射）。
func (a AgentPlanScene) ImageLimit(ctx context.Context, it agentdomain.PlanItem) (int, bool, error) {
	op := PlanQuoteRequest(it, 0).OperationCode
	return a.Catalog.InputLimit(ctx, it.ModelCode, op, catalogdomain.RoleImage)
}

// PlanPicks 实现 PlanScene。
func (a AgentPlanScene) PlanPicks(ctx context.Context, q agentapp.PlanPickQuery, now time.Time) (
	[]agentapp.PlanPickFact, error) {
	pinned := make(map[string]agentapp.ScenePin, len(q.Pinned))
	for _, p := range q.Pinned {
		k := p.Kind + ":" + p.Code
		if _, ok := pinned[k]; !ok { // 同 code 只认第一次的版本
			pinned[k] = p
		}
	}
	hasAudio := a.modelHasAudio(ctx, q.OutputType, q.ModelCode)
	out := make([]agentapp.PlanPickFact, 0, len(q.Refs))
	for _, r := range q.Refs {
		var snap mktdomain.Snapshot
		isNew := false
		if p, ok := pinned[r.Kind+":"+r.Code]; ok {
			snaps, err := a.Marketing.Snapshot(ctx, []mktdomain.Pick{{Kind: mktdomain.Kind(p.Kind), Code: p.Code, Version: p.Version}})
			if err != nil {
				return nil, err
			}
			snap = snaps[0]
		} else {
			snaps, err := a.Marketing.Resolve(ctx, []marketingapp.PickRef{{Kind: mktdomain.Kind(r.Kind), Code: r.Code}},
				q.OutputType, now)
			if err != nil {
				return nil, err
			}
			snap, isNew = snaps[0], true
		}
		f := agentapp.PlanPickFact{New: isNew, Tags: snap.Tags,
			Pin: agentapp.ScenePin{Kind: string(snap.Kind), Code: snap.Code, Version: snap.Version, Name: snap.Name}}
		opt := mktdomain.Option{Kind: snap.Kind, Code: snap.Code, Name: snap.Name, OutputTypes: snap.OutputTypes,
			InputRequirements: snap.InputRequirements, IsAudioRequired: snap.IsAudioRequired}
		if code, fixable := mktdomain.BlockReason(opt, mktdomain.UsageContext{OutputType: q.OutputType,
			ModelHasAudio: hasAudio, Slots: q.Slots}); code != "" {
			f.BlockCode, f.UserFixable, f.BlockReason = code, fixable, planBlockText(opt, code)
		}
		out = append(out, f)
	}
	return out, nil
}

// modelHasAudio 只对视频产出判出声；模型不是视频模型或查不到时返回 nil（不判 —— 判错的话
// 所有依赖出声的 Hook 一律灰掉，而原因与用户无关）。
func (a AgentPlanScene) modelHasAudio(ctx context.Context, outputType, modelCode string) *bool {
	if outputType != mktdomain.OutputVideo || modelCode == "" {
		return nil
	}
	t, err := a.Catalog.VideoTraits(ctx, modelCode)
	if err != nil || !t.OK {
		return nil
	}
	v := t.HasNativeAudio
	return &v
}

// planBlockText 是卡片上给用户看的一句话（前端按 userFixable 定语气，不匹配文案）。
func planBlockText(o mktdomain.Option, code string) string {
	name := o.Kind.Label() + "「" + o.Name + "」"
	switch code {
	case mktdomain.BlockOutputMismatch:
		return name + "不适用于当前产出"
	case mktdomain.BlockNeedsAudio:
		return name + "需要能出声的视频模型，当前所选模型不出声"
	}
	slot := strings.TrimPrefix(code, "needs_")
	label := map[string]string{mktdomain.SlotProduct: "商品图", mktdomain.SlotCharacter: "角色",
		mktdomain.SlotSourceVideo: "源视频"}[slot]
	if label == "" {
		label = slot
	}
	return name + "需要" + label
}

// TalkNotice 实现 PlanScene。
func (AgentPlanScene) TalkNotice(tags []string) string {
	if mktdomain.HasTag(tags, mktdomain.TagTalk) {
		return mktdomain.TalkDisclaimer
	}
	return ""
}

var _ agentapp.PlanScene = AgentPlanScene{}
