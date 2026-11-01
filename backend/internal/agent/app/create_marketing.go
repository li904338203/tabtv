package app

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"strings"

	"github.com/li904338203/tabtv/backend/internal/agent/domain"
	"github.com/li904338203/tabtv/backend/internal/agent/repo"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// 建营销会话（MKT-T-17，dev/01 §3.2）：一次请求建项目（origin=marketing）+ 会话（scene=marketing，
// scene_input 钉住选择项版本）+ 首条消息。
//
// ⚠️ 全部校验在建项目之前、更在扣第一轮费用之前（第一轮的费用在 Worker 里扣）：
// ⓪ 旧请求形状拒绝（MKD-27）→ ① 必须连同新项目一起建 → ② 槽位条数预检 + 引用按工作区解析归属 →
// ③ 技能属营销场景且启用、按其输入槽声明（解码处规范化成新形状，MKD-26）校验必填 / 条数 / 类型 →
// ④ 商品 / 角色按工作区取快照（MKT-T-37，MKD-25）→ ⑤ 选项钉版本（42215）。
// 失败一律点名（槽位错误带 details.slot）。
//
// 商品槽引用商品库里的**一个商品**（kind=marketing_product），角色槽引用角色库角色
// （kind=marketing_character）或脚本角色资产（kind=asset）；服务端把它们展开成快照写进 scene_input ——
// 下游（选图、换图、view_media、generate、引用保护）全部读 scene_input，形状不变，一行不用改。

// ProjectOriginMarketing 是营销入口建的项目来源（与 project.origin 的 CHECK 逐字一致，MKD-3）。
const ProjectOriginMarketing = "marketing"

// 槽位 key（格式契约在 marketing/domain，这里只用来把引用归位；合法性由 SceneResolver 判）。
const (
	slotProduct     = "product"
	slotCharacter   = "character"
	slotSourceVideo = "source_video"
)

// 营销库引用（MKT-T-37；dev/05 MKD-26）：只用于建营销会话的槽位，与槽位声明的 accept 取值同名。
const (
	RefMarketingProduct   = "marketing_product"
	RefMarketingCharacter = "marketing_character"
)

// codeLegacyShape 是旧请求形状被拒的 details.code（MKD-27）：前端据此提示「页面已更新，请刷新」。
const codeLegacyShape = "legacy_shape"

// MarketingInput 是建营销会话的营销输入（MKT-F-40~42）。
type MarketingInput struct {
	// LegacyProduct：请求体里出现了已废弃的 `marketing.product`（名称 / 卖点改由商品库提供，MKD-27）。
	// 只为认出来并拒绝 —— 静默忽略的话前端漏改一处就建出一个没有描述、没有 productId 的会话。
	LegacyProduct bool
	Picks         MarketingPicks
}

// MarketingPicks 是三个维度各选的选项 code（空 = 未选）。
type MarketingPicks struct {
	Creative string
	Hook     string
	Style    string
}

// SceneInput 是 agent_session.scene_input 的形状（dev/02 §2.3 的格式契约）。
//
// ⚠️ 只存 ID、名称、卖点与选项的 (code, version)（+ 该版本的名称，只供展示）：
// 不存 URL（签名地址几小时就失效）、不存片段原文（片段按 (code,version) 从版本历史取，MKD-5）。
// ID 一律字符串（对外直接下发，防 JS 精度丢失）。
type SceneInput struct {
	// ProductID / ProductVersion 来源商品与快照时的版本（MKT-T-37）；旧会话没有（空串 / 0）。
	// ⚠️ 只是溯源线索：下游一律读下面的快照，不回查商品库（MKD-25）。
	ProductID      string `json:"productId,omitempty"`
	ProductVersion int    `json:"productVersion,omitempty"`
	// Products 有序 mediaId，[0] 为主图（商品库会话由服务端从商品展开）。
	Products    []string        `json:"products"`
	Product     SceneProduct    `json:"product"`
	Characters  []SceneRef      `json:"characters"`
	SourceVideo *SceneRef       `json:"sourceVideo"`
	Picks       ScenePicks      `json:"picks"`
	Pins        []ScenePinEntry `json:"pins"`
}

// SceneProduct 是商品名称、卖点与描述（描述是商品库会话才有的，旧会话缺省）。
type SceneProduct struct {
	Name          string `json:"name"`
	SellingPoints string `json:"sellingPoints"`
	Description   string `json:"description,omitempty"`
}

// SceneRef 是槽位里的一个引用（kind = asset / media / node）。
//
// 角色库角色快照成 kind=media、id=主图（执行路径对 media 原样使用，不需要新分支），并带上
// characterId 与名称 / 形象 / 音色 / 参考音频（MKT-T-37）；这些字段旧会话与源视频都没有。
// ⚠️ VoiceMediaID 只存不发（MKD-33）：任何 Provider 请求都不带它，上下文只写「有参考音频」。
type SceneRef struct {
	Kind         string `json:"kind"`
	ID           string `json:"id"`
	CharacterID  string `json:"characterId,omitempty"`
	Name         string `json:"name,omitempty"`
	Appearance   string `json:"appearance,omitempty"`
	VoiceText    string `json:"voiceText,omitempty"`
	VoiceMediaID string `json:"voiceMediaId,omitempty"`
}

// ScenePicks 是建会话时钉住的三个维度（未选为 null）。
type ScenePicks struct {
	Creative *ScenePick `json:"creative"`
	Hook     *ScenePick `json:"hook"`
	Style    *ScenePick `json:"style"`
}

// ScenePick 是钉住的一个选项：(code, version) 永远指向同一段原文；name 是该版本的名称。
type ScenePick struct {
	Code    string `json:"code"`
	Version int    `json:"version"`
	Name    string `json:"name,omitempty"`
}

// ScenePinEntry 是规划时新钉的选项（MKT-T-20 写入；建会话时为空数组）。
type ScenePinEntry struct {
	Kind    string `json:"kind"`
	Code    string `json:"code"`
	Version int    `json:"version"`
	Source  string `json:"source,omitempty"`
}

// marketingPrep 是营销分支校验通过后的产物（落库用）。
type marketingPrep struct {
	input      SceneInput
	sceneInput json.RawMessage
	refs       json.RawMessage
	skills     json.RawMessage
}

// rejectMarketingFields：通用会话带了营销字段一律 400 —— 静默忽略的话，前端漏传 scene 时
// 会建出一个「看起来成功」却没有任何营销输入的通用会话。
func rejectMarketingFields(in CreateSessionInput) error {
	if in.Marketing != nil {
		return httpx.Errf(errcode.InvalidArgument, "marketing 只用于营销会话（scene=marketing）").
			WithDetails(map[string]any{"field": "marketing"})
	}
	for i, r := range in.Refs {
		if r.Slot != "" || r.Order != 0 {
			return httpx.Errf(errcode.InvalidArgument, "第 "+strconv.Itoa(i+1)+" 个引用：slot / order 只用于营销会话").
				WithDetails(map[string]any{"field": "message.refs"})
		}
		if isLibraryRef(r.Kind) {
			return httpx.Errf(errcode.InvalidArgument, "第 "+strconv.Itoa(i+1)+" 个引用："+r.Kind+" 只用于营销会话").
				WithDetails(map[string]any{"field": "message.refs"})
		}
	}
	return nil
}

func isLibraryRef(kind string) bool {
	return kind == RefMarketingProduct || kind == RefMarketingCharacter
}

// rejectLegacyShape 拒绝旧请求形状（MKD-27）：`marketing.product`、商品槽 / 角色槽给 kind=media。
// 点名字段或槽位，code=legacy_shape。
func rejectLegacyShape(in CreateSessionInput) error {
	if in.Marketing != nil && in.Marketing.LegacyProduct {
		return httpx.Errf(errcode.InvalidArgument, "marketing.product 已废弃：商品名称与卖点改由商品库提供，请刷新页面后重新选择商品").
			WithDetails(map[string]any{"field": "marketing.product", "code": codeLegacyShape})
	}
	for _, r := range in.Refs {
		if r.Kind != RefMedia {
			continue
		}
		switch r.Slot {
		case slotProduct:
			return slotErr(slotProduct, slotProduct, codeLegacyShape,
				"商品槽不再收直接上传的图片：请刷新页面后从商品库选择商品", nil)
		case slotCharacter:
			return slotErr(slotCharacter, slotCharacter, codeLegacyShape,
				"角色槽不再收直接上传的人物图：请刷新页面后从角色库选择角色", nil)
		}
	}
	return nil
}

func slotErr(slot, field, code, msg string, issues []SlotIssue) error {
	d := map[string]any{"slot": slot, "field": field, "code": code}
	if len(issues) > 0 {
		list := make([]map[string]any, 0, len(issues))
		for _, x := range issues {
			list = append(list, map[string]any{"slot": x.Slot, "field": x.Field, "code": x.Code, "message": x.Message})
		}
		d["issues"] = list
	}
	return httpx.Errf(errcode.InvalidArgument, msg).WithDetails(d)
}

// prepareMarketing 执行营销分支的全部校验并组装 scene_input / refs / skills（不写库）。
func (s *Service) prepareMarketing(ctx context.Context, a SessionActor, in CreateSessionInput) (*marketingPrep, error) {
	if s.p.Scene == nil {
		return nil, httpx.Errf(errcode.InvalidArgument, "营销 Agent 暂未开放").
			WithDetails(map[string]any{"field": "scene"})
	}
	// ⓪ 旧请求形状（MKD-27）：在一切查库之前拒绝并点名。
	if err := rejectLegacyShape(in); err != nil {
		return nil, err
	}
	// ① 营销只能从营销入口连同新项目一起建：挂到已有项目上的话「按入口判定」就失去了入口（MKT-D-03）。
	if in.CreateProjectName == nil {
		return nil, httpx.Errf(errcode.InvalidArgument, "营销会话必须连同新项目一起创建（createProject）").
			WithDetails(map[string]any{"field": "createProject"})
	}
	if in.ProjectID > 0 {
		return nil, httpx.Errf(errcode.InvalidArgument, "营销会话不能挂到已有项目上").
			WithDetails(map[string]any{"field": "projectId"})
	}

	// ② 条数预检（不查库）：先挡住超量，免得逐项查归属后才报错；错误点名槽位。
	// 每个槽的精确上限（商品 1、角色 2、源视频 1）由槽位声明在 ③ 判，这里只挡明显的滥用。
	counts := map[string]int{}
	unslotted := 0
	for i, r := range in.Refs {
		if r.Order < 0 {
			return nil, httpx.Errf(errcode.InvalidArgument, "第 "+strconv.Itoa(i+1)+" 个引用的 order 必须 ≥ 1").
				WithDetails(map[string]any{"field": "message.refs"})
		}
		if r.Slot == "" {
			if isLibraryRef(r.Kind) {
				return nil, httpx.Errf(errcode.InvalidArgument, "第 "+strconv.Itoa(i+1)+" 个引用："+r.Kind+" 必须填在槽位里").
					WithDetails(map[string]any{"field": "message.refs"})
			}
			unslotted++
			continue
		}
		counts[r.Slot]++
	}
	for slot, n := range counts {
		if n > MaxRefs {
			return nil, slotErr(slot, slot, "too_many", "「"+slot+"」引用过多", nil)
		}
	}
	if unslotted > MaxRefs {
		return nil, httpx.Errf(errcode.InvalidArgument, "一条消息最多引用 "+strconv.Itoa(MaxRefs)+" 项")
	}
	// 引用按工作区解析归属（与通用会话同一段代码）。新项目的画布是空的：节点引用在这里就被拒。
	// 商品库 / 角色库引用不走 RefLookup：它们的归属在 ④ 取快照时按工作区判。
	var plain []RefInput
	var library []ResolvedRef
	seenLib := map[string]bool{}
	for i, r := range in.Refs {
		if !isLibraryRef(r.Kind) {
			plain = append(plain, r)
			continue
		}
		id := strings.TrimSpace(r.ID)
		if n, err := strconv.ParseInt(id, 10, 64); err != nil || n <= 0 {
			return nil, slotErr(r.Slot, r.Slot, "bad_value", "第 "+strconv.Itoa(i+1)+" 个引用的 id 非法", nil)
		}
		if key := r.Slot + "|" + r.Kind + ":" + id; !seenLib[key] {
			seenLib[key] = true
			library = append(library, ResolvedRef{Kind: r.Kind, ID: id, Slot: r.Slot, Order: r.Order})
		}
	}
	resolved, err := s.resolveRefList(ctx, a, map[string]ProjectNode{}, plain)
	if err != nil {
		return nil, err
	}
	resolved, err = normalizeSlotOrder(mergeInRequestOrder(in.Refs, resolved, library))
	if err != nil {
		return nil, err
	}

	// ③ 技能：属营销场景且启用；按其输入槽声明校验（无技能 / 未声明 = 场景默认声明）。
	var decl json.RawMessage
	var skills json.RawMessage
	if code := strings.TrimSpace(in.SkillCode); code != "" {
		sk, err := s.repo.GetSkillByCode(ctx, code)
		if errors.Is(err, repo.ErrNotFound) || (err == nil && !sk.IsEnabled) {
			return nil, httpx.Errf(errcode.InvalidArgument, "技能不存在或已停用："+code).
				WithDetails(map[string]any{"field": "skillCode"})
		}
		if err != nil {
			return nil, httpx.Wrap(errcode.Internal, err)
		}
		if !domain.SkillInScene(sk.SceneList(), domain.SkillSceneMarketing) {
			return nil, httpx.Errf(errcode.InvalidArgument, "技能「"+sk.Name+"」不属于营销场景").
				WithDetails(map[string]any{"field": "skillCode"})
		}
		decl = sk.InputSlots
		skills, _ = json.Marshal([]string{sk.Code})
	}
	var picks MarketingPicks
	if in.Marketing != nil {
		picks = in.Marketing.Picks
	}
	fill := SceneSlotFill{Refs: map[string][]string{}, Fields: map[string]map[string]string{}}
	for _, r := range resolved {
		if r.Slot != "" {
			fill.Refs[r.Slot] = append(fill.Refs[r.Slot], slotInputKind(r))
		}
	}
	if issues := s.p.Scene.ValidateSlotFill(decl, fill); len(issues) > 0 {
		first := issues[0]
		return nil, slotErr(first.Slot, first.Field, first.Code, first.Message, issues)
	}

	// ④ 商品 / 角色快照（MKD-25）：按工作区取当前内容冻结进 scene_input。
	si := SceneInput{Products: []string{}, Characters: []SceneRef{}, Pins: []ScenePinEntry{}}
	if err := s.expandLibrary(ctx, a, resolved, &si); err != nil {
		return nil, err
	}

	// ⑤ 选项钉版本：存在 ∧ 在投 ∧ kind 对得上。适用产出此刻不判（模型可能是智能匹配，判在 submit_plan）。
	var refs []ScenePickRef
	for _, p := range []struct{ kind, code string }{
		{"creative", picks.Creative}, {"hook", picks.Hook}, {"style", picks.Style},
	} {
		if c := strings.TrimSpace(p.code); c != "" {
			refs = append(refs, ScenePickRef{Kind: p.kind, Code: c})
		}
	}
	var pins []ScenePin
	if len(refs) > 0 {
		if pins, err = s.p.Scene.Resolve(ctx, refs, s.now()); err != nil {
			return nil, err
		}
	}
	for _, r := range resolved {
		if r.Slot == slotSourceVideo && si.SourceVideo == nil {
			si.SourceVideo = &SceneRef{Kind: r.Kind, ID: r.ID}
		}
	}
	for _, p := range pins {
		pk := &ScenePick{Code: p.Code, Version: p.Version, Name: p.Name}
		switch p.Kind {
		case "creative":
			si.Picks.Creative = pk
		case "hook":
			si.Picks.Hook = pk
		case "style":
			si.Picks.Style = pk
		}
	}
	siJSON, err := json.Marshal(si)
	if err != nil {
		return nil, httpx.Wrap(errcode.Internal, err)
	}
	refsJSON, err := json.Marshal(resolved)
	if err != nil {
		return nil, httpx.Wrap(errcode.Internal, err)
	}
	return &marketingPrep{input: si, sceneInput: siJSON, refs: refsJSON, skills: skills}, nil
}

// mergeInRequestOrder 把解析过的普通引用与营销库引用按请求里的出现顺序合回一个列表
// （normalizeSlotOrder 按「槽位首次出现的位置」排槽，顺序要与请求一致）。
// resolveRefList 会按 (slot, kind, id) 去重，这里同样按出现顺序逐个取。
func mergeInRequestOrder(req []RefInput, plain, library []ResolvedRef) []ResolvedRef {
	out := make([]ResolvedRef, 0, len(plain)+len(library))
	pi, li := 0, 0
	seen := map[string]bool{}
	for _, r := range req {
		key := r.Slot + "|" + r.Kind + ":" + strings.TrimSpace(r.ID)
		if seen[key] {
			continue
		}
		seen[key] = true
		if isLibraryRef(r.Kind) {
			if li < len(library) {
				out = append(out, library[li])
				li++
			}
		} else if pi < len(plain) {
			out = append(out, plain[pi])
			pi++
		}
	}
	return out
}

// expandLibrary 取商品 / 角色快照写进 si（商品图、名称卖点描述、角色主图与文字），并把名称回填到
// 首条消息的 refs 上（label）。角色槽按槽内顺序：角色库角色快照成 kind=media，脚本角色资产原样。
func (s *Service) expandLibrary(ctx context.Context, a SessionActor, resolved []ResolvedRef, si *SceneInput) error {
	var productID int64
	var charIDs []int64
	for _, r := range resolved {
		id, _ := strconv.ParseInt(r.ID, 10, 64)
		switch {
		case r.Slot == slotProduct && r.Kind == RefMarketingProduct:
			productID = id
		case r.Slot == slotCharacter && r.Kind == RefMarketingCharacter:
			charIDs = append(charIDs, id)
		}
	}
	var prod *SceneProductSnap
	var chars []SceneCharacterSnap
	if productID > 0 || len(charIDs) > 0 {
		if s.p.Library == nil {
			return httpx.Errf(errcode.Internal, "营销库未接入")
		}
		var err error
		prod, chars, err = s.p.Library.Snapshot(ctx, a.WorkspaceID, productID, charIDs)
		var miss *SceneLibraryMissing
		if errors.As(err, &miss) {
			what := map[string]string{slotProduct: "商品", slotCharacter: "角色"}[miss.Slot]
			return slotErr(miss.Slot, miss.Slot, "not_found", what+"不存在或已删除，请重新选择", nil)
		}
		if err != nil {
			return httpx.Wrap(errcode.Internal, err)
		}
		if (productID > 0 && prod == nil) || len(chars) != len(charIDs) {
			return httpx.Errf(errcode.Internal, "营销库快照与请求对不上")
		}
	}
	ci := 0
	for i := range resolved {
		r := &resolved[i]
		switch {
		case r.Slot == slotProduct && r.Kind == RefMarketingProduct:
			si.ProductID, si.ProductVersion = r.ID, prod.Version
			si.Product = SceneProduct{Name: prod.Name, SellingPoints: prod.SellingPoints, Description: prod.Description}
			for _, mid := range prod.ImageMediaIDs {
				si.Products = append(si.Products, strconv.FormatInt(mid, 10))
			}
			r.Label = prod.Name
		case r.Slot == slotCharacter && r.Kind == RefMarketingCharacter:
			c := chars[ci]
			ci++
			ref := SceneRef{Kind: RefMedia, ID: strconv.FormatInt(c.MainMediaID, 10), CharacterID: r.ID,
				Name: c.Name, Appearance: c.Appearance, VoiceText: c.VoiceText}
			if c.VoiceMediaID > 0 {
				ref.VoiceMediaID = strconv.FormatInt(c.VoiceMediaID, 10)
			}
			si.Characters = append(si.Characters, ref)
			r.Label = c.Name
		case r.Slot == slotCharacter:
			si.Characters = append(si.Characters, SceneRef{Kind: r.Kind, ID: r.ID})
		}
	}
	return nil
}

// slotInputKind 把解析后的引用翻成槽位的输入类型（image / video / character_asset …）。
// 认不出的类型原样带出，由槽位校验点名「该槽只收 …」。
func slotInputKind(r ResolvedRef) string {
	switch r.Kind {
	case RefMedia:
		return r.MediaKind
	case RefAsset:
		if r.AssetType == "character" {
			return "character_asset"
		}
		return r.AssetType + "_asset"
	}
	return r.Kind
}

// normalizeSlotOrder 把每个槽内的顺序规范成 1..N（商品槽 #1 为主图）。
//
// 一个槽里要么都不带 order（按出现顺序），要么都带且互不相同 —— 混着来的话「谁是主图」没有唯一答案。
// 结果按「先无槽位、再按槽位首次出现的位置、槽内按 order」排列。
func normalizeSlotOrder(in []ResolvedRef) ([]ResolvedRef, error) {
	type group struct {
		first int
		items []int
	}
	groups := map[string]*group{}
	var keys []string
	for i, r := range in {
		g, ok := groups[r.Slot]
		if !ok {
			g = &group{first: i}
			groups[r.Slot] = g
			keys = append(keys, r.Slot)
		}
		g.items = append(g.items, i)
	}
	out := make([]ResolvedRef, 0, len(in))
	if g, ok := groups[""]; ok {
		for _, i := range g.items {
			out = append(out, in[i])
		}
	}
	for _, k := range keys {
		if k == "" {
			continue
		}
		g := groups[k]
		withOrder := 0
		seen := map[int]bool{}
		for _, i := range g.items {
			if o := in[i].Order; o > 0 {
				withOrder++
				if seen[o] {
					return nil, slotErr(k, k, "bad_value", "「"+k+"」槽的 order 重复："+strconv.Itoa(o), nil)
				}
				seen[o] = true
			}
		}
		if withOrder != 0 && withOrder != len(g.items) {
			return nil, slotErr(k, k, "bad_value", "「"+k+"」槽的 order 要么都填、要么都不填", nil)
		}
		items := append([]int(nil), g.items...)
		if withOrder > 0 {
			sort.SliceStable(items, func(a, b int) bool { return in[items[a]].Order < in[items[b]].Order })
		}
		for n, i := range items {
			r := in[i]
			r.Order = n + 1
			out = append(out, r)
		}
	}
	return out, nil
}
