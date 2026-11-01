package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/li904338203/tabtv/backend/internal/agent/app"
	"github.com/li904338203/tabtv/backend/internal/agent/domain"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// submit_plan 的营销分支（MKT-T-20、dev/01 §3.4、MKD-9）：每个计划项带条目键与选图。
//
//   - 条目键：模型给（v1 / hero），没给按位置补 item{n}；执行时 generate 凭它取已批准的条目（MKT-T-22）。
//   - 选图：productIndexes 落在 1..商品图总数；没给按商品槽顺序取前 Y 张（Y = 模型 × 操作的 image 上限
//     减去角色图占用，取自**报价用的那份能力矩阵**）。给多了**不出卡**，回 too_many_images + max 让 Agent 重选。
//   - 输入计数：由选中的商品图 + 角色 + 源视频算出，不信模型写的 inputs（与执行时发出的输入同源，MKT-D-45）。
//   - 选项：picks 缺省继承会话；新出现的 code 钉当前版本并并进 scene_input.pins（同会话同 code 只认第一次）。
//     依赖出声的选项配了不出声的模型 → 该条不可用（userFixable），卡片照发、不能批准。
//   - 口播：任一条的选项或当前技能带「口播」标签 → 卡片 notices 追加如实提示句（服务端写，不靠 Agent）。
//
// ⚠️ 只有 video / image 条目选商品图与选项；其余类型（配音、渲染…）只带条目键。

// marketingSpec 是营销计划项比通用多出的字段（同一段 raw 再解一次；通用会话不解，形状逐字不变）。
type marketingSpec struct {
	Key            string             `json:"key"`
	Variant        int                `json:"variant"`
	VariantLabel   string             `json:"variantLabel"`
	ProductIndexes []int              `json:"productIndexes"`
	Picks          map[string]*string `json:"picks"`
	// SourceVideo 本项是否以会话的源视频为源（素材裂变，MKT-T-28）。不填：会话有源视频时
	// 视频条目默认是裂变条目；false 显式不用；true 而会话没有源视频 / 不是视频条目 → 退回模型。
	SourceVideo *bool `json:"sourceVideo"`
}

const maxVariantLabel = 30

// planPickKinds 是选项维度（拼接顺序：创意 → Hook → 风格）。
var planPickKinds = []string{"creative", "hook", "style"}

// marketingPlan 是一次 submit_plan 的营销上下文。
type marketingPlan struct {
	si      app.SceneInput
	in      domain.MarketingInputs
	pinned  []app.ScenePin
	newPins []app.ScenePinEntry
	keys    map[string]bool
	notice  string
	// blocks 按计划项序号记选项带来的不可用原因（报价可用时才落到条目上）。
	blocks map[int]app.PlanPickFact
	// memberLevel 裂变条目判路线时报价用（与发卡报价同一个会员等级）。
	memberLevel int64
	// spec 视频规格的生效值（偏好 + 确认过的回答）：视频条目的时长按它核对，一支视频不拆段。
	spec app.VideoSpecState
	// multiVideo 计划里有两条及以上视频：每一条都必须是完整的选定时长（specDuration）。
	multiVideo bool
	// ops 挑生成方式（要用商品图 / 角色图而所选方式不收图时换掉）；opNotes 是换了之后给卡片的说明。
	ops     *opMatcher
	opNotes []string
}

// loadMarketing 读会话场景；通用会话返回 nil（走原路径，一个字都不改）。
func (t SubmitPlan) loadMarketing(ctx context.Context, rc RunContext) (*marketingPlan, error) {
	sess, err := t.d.Store.GetByID(ctx, rc.SessionID)
	if err != nil {
		return nil, fmt.Errorf("读取会话失败：%w", err)
	}
	if sess.Scene != domain.SceneMarketing {
		return nil, nil
	}
	if t.d.Scene == nil {
		return nil, errors.New("营销规划未接入，暂时无法提交规划")
	}
	si, err := app.ParseSceneInput(sess.SceneInput)
	if err != nil {
		return nil, err
	}
	mk := &marketingPlan{si: si, in: si.PlanInputs(), pinned: si.PinnedList(), keys: map[string]bool{},
		blocks: map[int]app.PlanPickFact{}, memberLevel: rc.MemberLevelID, spec: rc.Spec}
	// 当前技能带「口播」标签 → 提示句（技能读不到不阻断规划：提示是附加的，少一个技能不影响正确性）。
	var codes []string
	_ = json.Unmarshal(sess.SkillCodes, &codes)
	for _, c := range codes {
		sk, err := t.d.Store.GetSkillByCode(ctx, c)
		if err != nil {
			continue
		}
		var tags []string
		_ = json.Unmarshal(sk.Tags, &tags)
		if n := t.d.Scene.TalkNotice(tags); n != "" {
			mk.notice = n
		}
	}
	return mk, nil
}

// prepareItem 给第 n 项补条目键、选图与选项。返回的条目还没报价。
func (t SubmitPlan) prepareItem(ctx context.Context, mk *marketingPlan, n int, raw json.RawMessage,
	it domain.PlanItem) (domain.PlanItem, error) {
	var s marketingSpec
	if err := ptDecode(raw, &s, true); err != nil {
		return it, fmt.Errorf("第 %d 项：%w", n, err)
	}
	key := strings.TrimSpace(s.Key)
	if key == "" {
		key = domain.DefaultPlanKey(n)
	}
	if !domain.ValidPlanKey(key) {
		return it, fmt.Errorf("第 %d 项：key 只能是 1~32 位字母、数字、下划线或连字符", n)
	}
	if mk.keys[key] {
		return it, fmt.Errorf("第 %d 项：key %q 与前面的条目重复，每项要用不同的 key", n, key)
	}
	mk.keys[key] = true
	if s.Variant < 0 {
		return it, fmt.Errorf("第 %d 项：variant 必须 ≥ 1", n)
	}
	it.Key, it.Variant = key, s.Variant
	it.VariantLabel, _ = ptCutRunes(strings.TrimSpace(s.VariantLabel), maxVariantLabel)

	if it.Kind != "video" && it.Kind != "image" {
		if len(s.ProductIndexes) > 0 || len(s.Picks) > 0 || s.SourceVideo != nil {
			return it, fmt.Errorf("第 %d 项：只有 video / image 条目可以选商品图（productIndexes）、选项（picks）与源视频（sourceVideo）", n)
		}
		return it, nil
	}

	// ⓪ 素材裂变：定路线（video2video / 重新生成）。
	it, err := t.fissionRoute(ctx, mk, n, s, it)
	if err != nil {
		return it, err
	}
	if it, err = specDuration(mk.spec, n, raw, it, mk.multiVideo); err != nil {
		return it, err
	}
	if it.FissionRoute == domain.FissionVideo2Video {
		// 视频生视频只收 1 个视频：不选商品图（模型给了也不发，卡片说明写清「不使用商品图与角色」）。
		return t.applyPicks(ctx, mk, n, s, it)
	}
	return t.selectImages(ctx, mk, n, s, it)
}

// fissionRoute 给裂变条目定路线（MKT-T-28，见 domain/plan_fission.go）。
//
//   - 是不是裂变条目：模型给了 sourceVideo 就按它；没给时会话有源视频的视频条目默认是。
//     给了 true 而会话没有源视频、或不是视频条目 → 退回模型（参数写错了，不发卡）。
//   - 走不走 video2video：把条目改成 video2video 形态**报一次价**。报得出价 = 绑定 ∧ 启用 ∧
//     能力矩阵允许 1 个视频 ∧ 有价 ∧ 会员等级够 —— 与受理同一份判断（GAP-22），不另写模型名单。
//     报不出价（没绑、能力不收视频、缺价…）→ 重新生成，原因写进卡片说明。
func (t SubmitPlan) fissionRoute(ctx context.Context, mk *marketingPlan, n int, s marketingSpec,
	it domain.PlanItem) (domain.PlanItem, error) {
	src := mk.si.SourceVideo
	want := src != nil && it.Kind == "video"
	if s.SourceVideo != nil {
		want = *s.SourceVideo
		if want && it.Kind != "video" {
			return it, fmt.Errorf("第 %d 项：只有视频条目可以以源视频裂变（sourceVideo），图片条目请去掉 sourceVideo", n)
		}
		if want && src == nil {
			return it, fmt.Errorf("第 %d 项：本会话没有源视频，不能做裂变（sourceVideo）；请去掉 sourceVideo，或让用户在源视频槽里放一条视频后重新开始", n)
		}
	}
	if !want {
		return it, nil
	}
	if src.Kind != app.RefMedia {
		return it, fmt.Errorf("第 %d 项：源视频引用类型 %q 不受支持", n, src.Kind)
	}
	cand := domain.Video2VideoItem(it, src.ID)
	p, err := t.d.Pricer.PriceItem(ctx, cand, mk.memberLevel)
	if err != nil {
		return it, fmt.Errorf("第 %d 项：查询视频生视频是否可用失败：%w", n, err)
	}
	if p.Available {
		return cand, nil
	}
	return domain.RegenerateItem(it, src.ID, fissionFallbackReason(p)), nil
}

// fissionFallbackReason 是不走视频生视频的原因（卡片上给用户看）。
// 按 ArgFault 分类（不匹配文案）：没绑这个操作 / 输入不符 = 该模型不支持以视频为输入；其余照报价原因说。
func fissionFallbackReason(p app.PricedItem) string {
	switch p.ArgFault {
	case app.PlanFaultOperation, app.PlanFaultInputs:
		return "所选模型不支持以视频为输入（视频生视频）"
	}
	if r := strings.TrimSpace(p.BlockedReason); r != "" {
		return "所选模型的视频生视频暂不可用（" + strings.TrimRight(r, "。") + "）"
	}
	return "所选模型的视频生视频暂不可用"
}

// selectImages 是 ① 选图 + ② 选项（非 video2video 的 video / image 条目）。
func (t SubmitPlan) selectImages(ctx context.Context, mk *marketingPlan, n int, s marketingSpec,
	it domain.PlanItem) (domain.PlanItem, error) {
	key := it.Key
	// ⓪' 生成方式（04 文档「技能和模型参数」）：要用商品图 / 角色图，而模型写的方式不收图（文生视频 /
	// 文生图，或者没写 —— 原先按产出类型取默认的文生）时，换成该模型收得下的那一种。
	// 不换的话上限是 0 张：提示「减到 0 张」后模型会把商品图、再把要求商品图的营销选项一并去掉，
	// 产出的视频里没有商品（实测会话 106895426245885952，所选 seedance_2_5 明明支持图生视频）。
	if want := wantImages(mk, s); want > 0 && (it.OperationCode == "" || domain.IsTextOnlyOperation(it.OperationCode)) {
		var note string
		if it, note = mk.ops.match(ctx, n, it, map[string]int{"image": want}); note != "" {
			mk.opNotes = append(mk.opNotes, note)
		}
	}
	// ① 选图：上限取自报价用的能力矩阵（模型 × 操作的 image 上限 − 角色图）。
	capMax, found, err := t.d.Scene.ImageLimit(ctx, it)
	if err != nil {
		return it, fmt.Errorf("第 %d 项：查询模型输入上限失败：%w", n, err)
	}
	var selected []int
	if found {
		limit := domain.ProductImageLimit(capMax, mk.in.Characters)
		sel, err := domain.SelectProductIndexes(len(mk.in.Products), s.ProductIndexes, limit)
		var pe *domain.ProductIndexError
		switch {
		case errors.As(err, &pe) && pe.Code == domain.ProductIndexTooMany:
			msg := fmt.Sprintf("规划未提交（没有发卡、没有扣费）：第 %d 项「%s」选了 %d 张商品图，该模型此操作最多用 %d 张"+
				"（共 %d 张商品图）。请减到 %d 张以内后重新调用 submit_plan，或不填 productIndexes（按 #1 起取前 %d 张）。",
				n, key, len(s.ProductIndexes), limit, len(mk.in.Products), limit, limit)
			if limit <= 0 {
				// ⚠️ 不说「减到 0 张」：那等于让模型把商品图去掉。说清是生成方式不收图，该换方式。
				msg = fmt.Sprintf("规划未提交（没有发卡、没有扣费）：第 %d 项「%s」的生成方式 %s 不收图片，用不了商品图。"+
					"要用商品图请改用收图片的生成方式（见下），并在 operationCode 写明；确实不需要商品图时把 productIndexes 写成 []。%s",
					n, key, opLabel(it.OperationCode), mk.ops.hint(ctx, it))
			}
			return it, &domain.ToolRejection{Code: domain.ToolErrTooManyImages, Message: msg,
				Details: map[string]any{"item": key, "max": limit, "selected": len(s.ProductIndexes),
					"total": len(mk.in.Products)}}
		case err != nil:
			return it, fmt.Errorf("第 %d 项：%w", n, err)
		}
		selected = sel
		it.ImageLimit = &limit
	}
	// 模型 × 操作查不到（found=false）时不选图、不猜上限：报价会说明是哪里写错了（退回模型）
	// 或运营没配好（照常发卡说明原因）。
	if it, err = domain.ApplyProductSelection(it, selected, mk.in); err != nil {
		return it, fmt.Errorf("第 %d 项：%w", n, err)
	}
	return t.applyPicks(ctx, mk, n, s, it)
}

// wantImages 是本项要发出的图片数（商品图 + 角色图），用来挑生成方式。
//
// 商品图：productIndexes 显式写成 [] 表示不用；写了几张算几张；没写时至少用主图 #1（随后按上限取前若干张）。
func wantImages(mk *marketingPlan, s marketingSpec) int {
	products := 0
	if len(mk.in.Products) > 0 && (s.ProductIndexes == nil || len(s.ProductIndexes) > 0) {
		products = len(s.ProductIndexes)
		if products == 0 {
			products = 1
		}
	}
	return products + mk.in.Characters
}

// applyPicks 是 ② 选项：缺省继承会话；新 code 钉当前版本。
func (t SubmitPlan) applyPicks(ctx context.Context, mk *marketingPlan, n int, s marketingSpec,
	it domain.PlanItem) (domain.PlanItem, error) {
	var refs []app.ScenePickRef
	for k := range s.Picks {
		if !contains(planPickKinds, k) {
			return it, fmt.Errorf("第 %d 项：picks 只能有 creative / hook / style", n)
		}
	}
	for _, k := range planPickKinds {
		code := mk.si.SessionPickCode(k)
		if v, ok := s.Picks[k]; ok && v != nil {
			code = mk.pickCode(k, strings.TrimSpace(*v))
		}
		if code != "" {
			refs = append(refs, app.ScenePickRef{Kind: k, Code: code})
		}
	}
	if len(refs) == 0 {
		return it, nil
	}
	// 槽位计数 = 本项实际发出的输入：视频生视频只发源视频；其余条目不发源视频（MKT-T-28）。
	slots := map[string]int{"product": len(it.ProductMediaIDs), "character": mk.in.Characters}
	if it.FissionRoute == domain.FissionVideo2Video {
		// 素材裂变的源视频本身就是商品视频，商品在画面里：要求商品图的选项照常可用（04 文档 P-47）。
		// 原先按「商品 0 张」判，部署数据里 54 条选项全部要求商品，裂变一个选项都用不了 ——
		// 与 MKT-F-53「裂变可换 Hook、换风格」冲突。
		slots = map[string]int{"product": 1, "character": 0, "source_video": 1}
	}
	facts, err := t.d.Scene.PlanPicks(ctx, app.PlanPickQuery{Refs: refs, Pinned: mk.pinned,
		OutputType: it.Kind, ModelCode: it.ModelCode, Slots: slots}, t.d.Now())
	if err != nil {
		var be *httpx.BizError
		if errors.As(err, &be) && be.Code != errcode.Internal {
			return it, fmt.Errorf("第 %d 项：%s。改法：换一个在投且适用于%s的选项，或去掉 picks 里的这一项（沿用会话的选择）",
				n, strings.TrimRight(be.Message, "。"), planKindLabels[it.Kind])
		}
		return it, fmt.Errorf("第 %d 项：查询营销选项失败：%w", n, err)
	}
	it.Picks = make([]domain.PlanPick, 0, len(facts))
	for _, f := range facts {
		it.Picks = append(it.Picks, domain.PlanPick{Kind: f.Pin.Kind, Code: f.Pin.Code, Version: f.Pin.Version,
			Name: f.Pin.Name})
		if f.New {
			// 后面的条目再用到同一个 code 时按这一版（同会话同 code 只认第一次）。
			mk.pinned = append(mk.pinned, f.Pin)
			mk.newPins = append(mk.newPins, app.ScenePinEntry{Kind: f.Pin.Kind, Code: f.Pin.Code,
				Version: f.Pin.Version, Source: app.PinSourcePlan})
		}
		if note := t.d.Scene.TalkNotice(f.Tags); note != "" {
			mk.notice = note
		}
		if _, blocked := mk.blocks[n]; !blocked && f.BlockCode != "" {
			mk.blocks[n] = f
		}
	}
	return it, nil
}

// pickCode 把模型写进 picks 的值换成 code：写的是本会话已确认选项的**名称**时换成它的 code，其余原样返回。
//
// 起因（会话 107398945801502720）：「营销输入」里原先只写了名称，模型照着把「痛点立解」当 code 传，
// 报「创意不存在」，又调了三次 list_marketing_options 才查到 code。现在提示词里已写出 code，这里再兜一次底 ——
// 只认本会话确认过的选项（名称来自我们自己写给模型的那一段），不按名称去全库模糊匹配。
// 本身就是已钉住的 code 时原样返回；同名对应到两个不同 code 时也原样返回（不替模型猜，交给 Resolve 报错）。
func (mk *marketingPlan) pickCode(kind, v string) string {
	if v == "" {
		return v
	}
	match := ""
	for _, p := range mk.pinned {
		if p.Kind != kind {
			continue
		}
		if p.Code == v {
			return v
		}
		if p.Name == "" || p.Name != v || p.Code == match {
			continue
		}
		if match != "" {
			return v
		}
		match = p.Code
	}
	if match != "" {
		return match
	}
	return v
}

// applyBlock 把选项带来的不可用原因落到**报价可用**的条目上（报价本身不可用时保留报价的原因：
// 那条更根本 —— 模型都用不了，就谈不上它出不出声）。
//
// ⚠️ 码用 MARKETING_OPTION_UNAVAILABLE（与报价阻塞同为错误码名）：批准时据此拒绝，
// 而不是让重新报价把它当成「这次报得出价」悄悄解除（见 Service.submitPlan）。
func (mk *marketingPlan) applyBlock(n int, it domain.PlanItem) domain.PlanItem {
	f, ok := mk.blocks[n]
	if !ok || !it.Available() {
		return it
	}
	it.Credits = 0
	it.BlockedCode = errcode.MarketingOptionUnavailable.Name()
	it.BlockedReason = f.BlockReason
	it.UserFixable = f.UserFixable
	return it
}

// persistPins 把本次新钉的选项并进会话（发卡之前；失败不发卡，免得卡片上的版本在会话里查不到）。
func (t SubmitPlan) persistPins(ctx context.Context, rc RunContext, mk *marketingPlan) error {
	if len(mk.newPins) == 0 {
		return nil
	}
	b, err := json.Marshal(mk.newPins)
	if err != nil {
		return err
	}
	if err := t.d.Store.AppendScenePins(ctx, rc.SessionID, b); err != nil {
		return fmt.Errorf("记录选项版本失败：%w", err)
	}
	return nil
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// checkVariants 按不同 variant 计数（domain.ValidateVariants），超过配置上限回 too_many_variants：
// 不出卡、不报价、不钉版本。消息里的数字取配置（SEEAITV_MARKETING_MAX_VARIANTS）。
func (t SubmitPlan) checkVariants(raws []json.RawMessage) error {
	items := make([]domain.PlanItem, 0, len(raws))
	for _, raw := range raws {
		var s struct {
			Variant int `json:"variant"`
		}
		_ = json.Unmarshal(raw, &s) // 形状错误由逐项解析报
		items = append(items, domain.PlanItem{Variant: s.Variant})
	}
	err := domain.ValidateVariants(items, t.d.MaxVariants)
	var rej *domain.ToolRejection
	if errors.As(err, &rej) {
		rej.Message = "规划未提交（没有发卡、没有扣费）：" + rej.Message +
			"。请把变体合并或减到上限以内后重新调用 submit_plan，超出的部分可以下一批再做。"
	}
	return err
}
