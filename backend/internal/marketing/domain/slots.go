package domain

import (
	"fmt"
	"strings"
)

// 槽位 key（MKT-D-41）。⚠️ 槽名是**格式契约**，运营不能新造：新槽名意味着服务端
// 不知道怎么用它 —— 能存进去，然后在建会话时没有任何代码读它。
const (
	SlotProduct     = "product"
	SlotCharacter   = "character"
	SlotSourceVideo = "source_video"
)

// SlotKeys 按展示顺序列出全部槽位。
var SlotKeys = []string{SlotProduct, SlotCharacter, SlotSourceVideo}

// ValidSlotKey 判断槽位 key 是否合法。
func ValidSlotKey(k string) bool { return contains(SlotKeys, k) }

// 槽位可收的输入类型。
//
// AcceptMarketingProduct / AcceptMarketingCharacter 与建会话请求里 `refs[].kind` 的取值**同名**
// （dev/05 MKD-26，便于 grep）：商品槽引用商品库里的一个商品、角色槽引用角色库里的角色，
// 服务端展开成快照写进 scene_input（MKD-25）。
// ⚠️ AcceptImage 只剩**旧数据**会出现在商品 / 角色槽上（解码时由 NormalizeSlotDecls 换掉），
// 新声明里商品槽、角色槽都不收它。
const (
	AcceptImage              = "image"
	AcceptVideo              = "video"
	AcceptCharacterAsset     = "character_asset"
	AcceptMarketingProduct   = "marketing_product"
	AcceptMarketingCharacter = "marketing_character"
)

// slotAccepts 是每个槽位**能**收的类型：声明只能在这个范围里收窄。
// 商品槽收视频、源视频槽收图片的话，服务端不知道拿它们怎么办。
var slotAccepts = map[string][]string{
	SlotProduct:     {AcceptMarketingProduct},
	SlotCharacter:   {AcceptMarketingCharacter, AcceptCharacterAsset},
	SlotSourceVideo: {AcceptVideo},
}

// slotFieldLimits 是槽位上允许的字段与代码里的长度上限。
//
// MKT-T-37 起为空：商品名称 / 卖点 / 描述属于商品库里的商品（MKD-25），不再随消息填在槽位上；
// 旧声明里的 `fields` 由 NormalizeSlotDecls 丢弃。字段机制保留，是为了让「声明里写了字段」
// 仍然按 unknown_field 报出来，而不是被静默接受。
var slotFieldLimits = map[string]map[string]int{}

// SlotField 是槽位上的文本字段声明。
type SlotField struct {
	Key    string `json:"key"`
	Label  string `json:"label"`
	MaxLen int    `json:"maxLen,omitempty"` // 0 = 用代码上限
}

// SlotDecl 是技能的输入槽声明（`agent_skill.input_slots` 的一项，dev/01 §4.3）。
type SlotDecl struct {
	Key      string      `json:"key"`
	Label    string      `json:"label"`
	Accept   []string    `json:"accept"`
	Required bool        `json:"required"`
	MaxCount int         `json:"maxCount"`
	Fields   []SlotField `json:"fields,omitempty"`
}

// SceneSlotMax 是营销场景对槽位数量的上限。
//
// ⚠️ MKT-T-37 起商品槽数的是**商品**不是商品图（MaxProductsPerSession = 1）：每个商品的图片张数
// 是另一个配置值（SEEAITV_MARKETING_MAX_PRODUCT_IMAGES），在商品库保存时校验。
func SceneSlotMax(key string) int {
	switch key {
	case SlotProduct:
		return MaxProductsPerSession
	case SlotCharacter:
		return MaxCharacterRefs
	case SlotSourceVideo:
		return MaxSourceVideos
	}
	return 0
}

// DefaultSlots 是营销场景未选技能时的默认声明：商品（必填，商品库里的 1 个商品）+ 角色（可选，
// 角色库角色或脚本角色资产）。经 `/marketing/config` 下发，前后端用同一份。
func DefaultSlots() []SlotDecl {
	return []SlotDecl{
		{Key: SlotProduct, Label: "商品", Accept: []string{AcceptMarketingProduct}, Required: true,
			MaxCount: MaxProductsPerSession},
		{Key: SlotCharacter, Label: "角色", Accept: []string{AcceptMarketingCharacter, AcceptCharacterAsset},
			Required: false, MaxCount: MaxCharacterRefs},
	}
}

// IssueNormalized 是「旧形状已按新形状换掉」的提醒码（管理端保存回 warnings，MKD-26）。
const IssueNormalized IssueCode = "normalized"

// NormalizeSlotDecls 把技能的输入槽声明换成新形状（MKD-26），返回新声明与提醒（Level=warning）。
//
//   - 商品槽：accept 里的 image → marketing_product（去重）；maxCount 超过 MaxProductsPerSession 的夹到它；
//     fields（商品名称 / 卖点）丢弃 —— 它们现在是商品库里商品的属性；
//   - 角色槽：accept 里的 image → marketing_character（去重，保留 character_asset）；
//   - 源视频槽与认不出的槽原样不动（认不出的交给 ValidateSlotDecl 报错，不在这里悄悄删掉）。
//
// ⚠️ **只有这一处实现**，读与写都调它：建会话读技能、管理端保存、`/agent/skills` 与管理端下发。
// 各写一份的话总有一个页面看到旧形状。库里、数据文件（platform/seed/data/marketing）、导入工具因此都不用改 ——
// 只改库不改解码的话，下一次重复导入就把旧形状写回去。
// ⚠️ 幂等：新形状输入原样返回、没有提醒。不改入参（返回副本）。
func NormalizeSlotDecls(decls []SlotDecl) ([]SlotDecl, []Issue) {
	out := make([]SlotDecl, 0, len(decls))
	var changed []string
	for _, d := range decls {
		n := d
		n.Accept = append([]string(nil), d.Accept...)
		n.Fields = append([]SlotField(nil), d.Fields...)
		var what []string
		switch d.Key {
		case SlotProduct:
			if acc, ok := replaceAccept(n.Accept, AcceptImage, AcceptMarketingProduct); ok {
				n.Accept = acc
				what = append(what, "可收类型改为商品库商品")
			}
			if n.MaxCount > MaxProductsPerSession {
				n.MaxCount = MaxProductsPerSession
				what = append(what, fmt.Sprintf("数量上限改为 %d 个商品", MaxProductsPerSession))
			}
			if len(n.Fields) > 0 {
				what = append(what, "去掉商品名称 / 卖点字段（改由商品库提供）")
			}
			n.Fields = nil
		case SlotCharacter:
			if acc, ok := replaceAccept(n.Accept, AcceptImage, AcceptMarketingCharacter); ok {
				n.Accept = acc
				what = append(what, "可收类型的人物图改为角色库角色")
			}
		}
		if len(n.Fields) == 0 {
			n.Fields = nil
		}
		if len(what) > 0 {
			changed = append(changed, fmt.Sprintf("「%s」槽%s", labelOr(d), strings.Join(what, "、")))
		}
		out = append(out, n)
	}
	if len(changed) == 0 {
		return out, nil
	}
	return out, []Issue{warnIssue("inputSlots", IssueNormalized,
		"输入槽声明是旧形状，已按新形状保存："+strings.Join(changed, "；"))}
}

// replaceAccept 把 accept 里的 from 换成 to 并去重（保持首次出现的顺序）；没有 from 时 ok=false。
func replaceAccept(acc []string, from, to string) ([]string, bool) {
	if !contains(acc, from) {
		return acc, false
	}
	out := make([]string, 0, len(acc))
	for _, a := range acc {
		if a == from {
			a = to
		}
		if !contains(out, a) {
			out = append(out, a)
		}
	}
	return out, true
}

// ValidateSlotDecl 校验技能的输入槽声明（技能保存时调，MKT-D-11）。一次报完。
// 调用方先 NormalizeSlotDecls（旧形状在这里会按「商品槽不能收 image」报错）。
func ValidateSlotDecl(decls []SlotDecl) []Issue {
	var out []Issue
	seen := map[string]bool{}
	for i, d := range decls {
		p := fmt.Sprintf("inputSlots[%d]", i)
		if !ValidSlotKey(d.Key) {
			out = append(out, errIssue(p+".key", IssueBadValue, fmt.Sprintf("槽位只能是 product / character / source_video，不认识「%s」", d.Key)))
			continue
		}
		if seen[d.Key] {
			out = append(out, errIssue(p+".key", IssueDuplicate, fmt.Sprintf("槽位「%s」重复声明", d.Key)))
			continue
		}
		seen[d.Key] = true
		if strings.TrimSpace(d.Label) == "" {
			out = append(out, errIssue(p+".label", IssueRequired, "槽位名称不能为空"))
		}
		if len(d.Accept) == 0 {
			out = append(out, errIssue(p+".accept", IssueRequired, "至少声明一种可收的输入类型"))
		}
		for j, a := range d.Accept {
			if !contains(slotAccepts[d.Key], a) {
				out = append(out, errIssue(fmt.Sprintf("%s.accept[%d]", p, j), IssueBadValue,
					fmt.Sprintf("「%s」槽只能收 %s，不能收「%s」", d.Key, strings.Join(slotAccepts[d.Key], " / "), a)))
			}
		}
		if limit := SceneSlotMax(d.Key); d.MaxCount < 1 || d.MaxCount > limit {
			out = append(out, errIssue(p+".maxCount", IssueBadValue, fmt.Sprintf("「%s」槽数量上限须在 1~%d 之间", d.Key, limit)))
		}
		for j, f := range d.Fields {
			fp := fmt.Sprintf("%s.fields[%d]", p, j)
			limit, ok := slotFieldLimits[d.Key][f.Key]
			switch {
			case !ok:
				out = append(out, errIssue(fp+".key", IssueUnknownField, fmt.Sprintf("「%s」槽没有字段「%s」", d.Key, f.Key)))
			case f.MaxLen < 0 || f.MaxLen > limit:
				out = append(out, errIssue(fp+".maxLen", IssueBadValue, fmt.Sprintf("字段「%s」长度上限须在 1~%d 之间", f.Key, limit)))
			}
		}
	}
	return out
}

// SlotFill 是一次发送里各槽位的实际填充。
type SlotFill struct {
	// Refs：槽位 → 每一项的输入类型（marketing_product / marketing_character / character_asset / video …），按槽内顺序。
	// 归属校验不在这里（要查库，属 app 层）；这里只管数量与类型。
	Refs map[string][]string
	// Fields：槽位 → 字段 → 值。MKT-T-37 起没有任何槽位声明字段（见 slotFieldLimits），带了即 unknown_field。
	Fields map[string]map[string]string
}

// ValidateSlotFill 按声明校验一次填充（MKT-F-17、41）。一次报完，每条都**点名槽位**
// （Issue.Slot），前端据此把错误挂到对应的槽上，接口据此给出 `details.slot`。
func ValidateSlotFill(decls []SlotDecl, fill SlotFill) []Issue {
	var out []Issue
	declared := map[string]SlotDecl{}
	for _, d := range decls {
		declared[d.Key] = d
		refs := fill.Refs[d.Key]
		switch {
		case d.Required && len(refs) == 0:
			out = append(out, slotIssue(d.Key, "", IssueRequired, fmt.Sprintf("请填写「%s」", labelOr(d))))
		case d.MaxCount > 0 && len(refs) > d.MaxCount:
			out = append(out, slotIssue(d.Key, "", IssueTooMany, fmt.Sprintf("「%s」最多 %d 项，当前 %d 项", labelOr(d), d.MaxCount, len(refs))))
		}
		for i, kind := range refs {
			if !contains(d.Accept, kind) {
				out = append(out, slotIssue(d.Key, "", IssueBadValue, fmt.Sprintf("「%s」第 %d 项是 %s，该槽只收 %s", labelOr(d), i+1, kind, strings.Join(d.Accept, " / "))))
			}
		}
		fields := fill.Fields[d.Key]
		for _, fk := range sortedKeys(fields) {
			limit, ok := fieldLimit(d, fk)
			if !ok {
				out = append(out, slotIssue(d.Key, fk, IssueUnknownField, fmt.Sprintf("「%s」没有字段「%s」", labelOr(d), fk)))
				continue
			}
			if n := RuneLen(fields[fk]); n > limit {
				out = append(out, slotIssue(d.Key, fk, IssueTooLong, fmt.Sprintf("「%s」最多 %d 字，当前 %d 字", fieldLabel(d, fk), limit, n)))
			}
		}
	}
	for _, k := range sortedKeys(fill.Refs) {
		if _, ok := declared[k]; !ok && len(fill.Refs[k]) > 0 {
			out = append(out, slotIssue(k, "", IssueBadValue, fmt.Sprintf("当前技能没有「%s」槽位", k)))
		}
	}
	for _, k := range sortedKeys(fill.Fields) {
		if _, ok := declared[k]; !ok && len(fill.Fields[k]) > 0 {
			out = append(out, slotIssue(k, "", IssueBadValue, fmt.Sprintf("当前技能没有「%s」槽位", k)))
		}
	}
	return out
}

// fieldLimit 取字段的有效上限：声明的 maxLen 与代码上限取小（声明只能收紧）。
func fieldLimit(d SlotDecl, key string) (int, bool) {
	codeLimit, ok := slotFieldLimits[d.Key][key]
	if !ok {
		return 0, false
	}
	for _, f := range d.Fields {
		if f.Key == key {
			if f.MaxLen > 0 && f.MaxLen < codeLimit {
				return f.MaxLen, true
			}
			return codeLimit, true
		}
	}
	return 0, false
}

func fieldLabel(d SlotDecl, key string) string {
	for _, f := range d.Fields {
		if f.Key == key && strings.TrimSpace(f.Label) != "" {
			return f.Label
		}
	}
	return key
}

func labelOr(d SlotDecl) string {
	if strings.TrimSpace(d.Label) != "" {
		return d.Label
	}
	return d.Key
}

func slotIssue(slot, field string, code IssueCode, msg string) Issue {
	f := slot
	if field != "" {
		f = slot + "." + field
	}
	return Issue{Slot: slot, Field: f, Code: code, Message: msg, Level: LevelError}
}

// SummaryCharacter 是角色槽的一项，三种来源：
//   - 角色库角色（Library=true，MKT-T-37）：名称 + 形象 / 音色摘要 + 是否有参考音频；
//   - 脚本角色资产：AssetName；
//   - 都没有 = 旧会话里用户上传的人物图。
//
// ⚠️ 只有「有没有参考音频」这一位，没有音频的 ID —— 参考音频只存不发（MKD-33），
// 结构体上没有它，就不可能被拼进上下文。
type SummaryCharacter struct {
	AssetName  string `json:"assetName,omitempty"`
	Library    bool   `json:"library,omitempty"`
	Name       string `json:"name,omitempty"`
	Appearance string `json:"appearance,omitempty"`
	VoiceText  string `json:"voiceText,omitempty"`
	HasVoice   bool   `json:"hasVoice,omitempty"`
}

// SummaryTextMax 是角色形象 / 音色描述在上下文里的截断长度（码点，dev/05 §4.2、MKR-21）：
// 快照存原文（≤ 500），渲染进上下文时只给摘要。
const SummaryTextMax = 80

// SummaryOption 是摘要里一个所选选项：只有**名称与说明**（MKT-F-43），没有片段原文。
// 推荐模型是**展示名**（由 app 层经目录端口解析），推荐参数来自 ParseRecommendedParams。
type SummaryOption struct {
	Kind Kind `json:"kind"`
	// FromPlan 是规划时新钉的（不是建会话时选的）：标题写「规划中用到的…」（04 文档 P-48）。
	FromPlan bool `json:"fromPlan,omitempty"`
	// Code 选项 code（submit_plan 的 picks 要填它）。为空时不写（旧 fixture 逐字不变）。
	//
	// ⚠️ 起因（会话 107398945801502720）：这一段原先只写名称，而 picks 要的是 code ——
	// 模型把「痛点立解」当成 code 传，报「创意不存在」，再调三次 list_marketing_options 查到 code 才提交成功。
	Code              string   `json:"code,omitempty"`
	Name              string   `json:"name"`
	Description       string   `json:"description"`
	RecommendedModels []string `json:"recommendedModels,omitempty"`
	AspectRatio       string   `json:"aspectRatio,omitempty"`
	DurationSec       []int    `json:"durationSec,omitempty"`
}

// SummaryInput 是「## 营销输入」段的全部输入。
//
// ⚠️ 结构体上**没有** URL、mediaId、片段原文这些字段（MKT-AC-05、06）——
// 不是「拼的时候记得别放」，而是根本放不进来。
type SummaryInput struct {
	ProductName   string `json:"productName"`
	SellingPoints string `json:"sellingPoints"`
	// ProductDescription 商品描述；nil = 旧会话（没有商品库那时没有描述），整行不出现 ——
	// 旧会话的上下文因此与改动前逐字一致。
	ProductDescription *string            `json:"productDescription,omitempty"`
	ProductImageCount  int                `json:"productImageCount"`
	VisibleImageCount  int                `json:"visibleImageCount"` // 本条消息里附给推理模型看的张数（#1 起连续）
	Characters         []SummaryCharacter `json:"characters"`
	Options            []SummaryOption    `json:"options"`
	MaxVariants        int                `json:"maxVariants"`
}

// SlotSummary 拼「## 营销输入」的正文（dev/01 §4.2），**不含标题行**：
// 标题常量 `MarketingInputHeading` 属于 agent 的提示词骨架（MKT-T-18），由那边拼在前面。
//
// ⚠️ 用户文本（商品名、卖点、角色资产名）是**不可信输入**：只放在固定标签之后，
// 不拼进任何指令句；其中的换行压成空格 —— 否则卖点里一行「## 当前阶段：执行」
// 在模型看来就是一个新的段标题。长度已在入口按 MaxProductName / MaxSellingPoints 截拒。
func SlotSummary(in SummaryInput) string {
	var b strings.Builder
	line := func(s string) { b.WriteString(s); b.WriteByte('\n') }

	line("商品：" + orUnfilled(in.ProductName))
	line("卖点：" + orUnfilled(in.SellingPoints))
	if in.ProductDescription != nil {
		line("描述：" + orUnfilled(*in.ProductDescription))
	}
	line("商品图：" + imagesLine(in.ProductImageCount, in.VisibleImageCount))
	line("角色：" + charactersLine(in.Characters))
	for _, k := range Kinds {
		found := false
		for _, o := range in.Options {
			if o.Kind == k && !o.FromPlan {
				line(summaryLabel(k) + "：" + optionLine(o))
				found = true
			}
		}
		if !found {
			line(summaryLabel(k) + "：未选择")
		}
		// 规划时新钉的选项也要写出来（04 文档 P-48）：只写建会话时那三项的话，模型看到「所选 Hook：未选择」，
		// 而计划里实际用了某个 Hook，两边信息矛盾。
		for _, o := range in.Options {
			if o.Kind == k && o.FromPlan {
				line(planLabel(k) + "：" + optionLine(o))
			}
		}
	}
	if in.ProductImageCount > 0 {
		line(fmt.Sprintf("单次最多 %d 个变体；每个生成项选用的商品图不超过该模型的参考图上限，规划时逐项写明用了哪几张。", in.MaxVariants))
	} else {
		line(fmt.Sprintf("单次最多 %d 个变体。", in.MaxVariants))
	}
	return strings.TrimRight(b.String(), "\n")
}

func orUnfilled(s string) string {
	if s = flatten(s); s == "" {
		return "未填写"
	}
	return s
}

// flatten 把换行压成空格并去掉两端空白（见 SlotSummary 的说明）。
func flatten(s string) string {
	s = strings.NewReplacer("\r\n", " ", "\r", " ", "\n", " ", "\u2028", " ", "\u2029", " ").Replace(s)
	return strings.TrimSpace(s)
}

func imagesLine(total, visible int) string {
	switch {
	case total <= 0:
		return "无"
	case visible <= 0:
		return fmt.Sprintf("共 %d 张，#1 为主图。本条消息未附图；用 view_media 按序号查看。", total)
	case visible >= total:
		return fmt.Sprintf("共 %d 张，#1 为主图。你在本条消息里能看到 %s。", total, indexRange(total))
	}
	return fmt.Sprintf("共 %d 张，#1 为主图。你在本条消息里能看到 %s；其余用 view_media 按序号查看。", total, indexRange(visible))
}

func indexRange(n int) string {
	if n == 1 {
		return "#1"
	}
	return fmt.Sprintf("#1~#%d", n)
}

func charactersLine(cs []SummaryCharacter) string {
	if len(cs) == 0 {
		return "无"
	}
	parts := make([]string, 0, len(cs))
	uploaded := 0
	for _, c := range cs {
		if c.Library {
			parts = append(parts, libraryCharacterLine(c))
			continue
		}
		if name := flatten(c.AssetName); name != "" {
			parts = append(parts, name)
			continue
		}
		uploaded++
		parts = append(parts, fmt.Sprintf("上传的人物图 #%d", uploaded))
	}
	return strings.Join(parts, "、")
}

// libraryCharacterLine 是角色库角色的一项：`柚子君（形象：…；音色：…；有参考音频，不发送给模型）`。
//
// ⚠️ 形象 / 音色是不可信的用户文本：只放在固定标签之后，换行压成空格、截到 SummaryTextMax 码点
// （防伪造段标题，同卖点）。名称已在角色库按禁用字符校验过，这里仍压一次换行。
func libraryCharacterLine(c SummaryCharacter) string {
	name := flatten(c.Name)
	if name == "" {
		name = "营销角色"
	}
	var attrs []string
	if a := clip(flatten(c.Appearance), SummaryTextMax); a != "" {
		attrs = append(attrs, "形象："+a)
	}
	if v := clip(flatten(c.VoiceText), SummaryTextMax); v != "" {
		attrs = append(attrs, "音色："+v)
	}
	if c.HasVoice {
		attrs = append(attrs, "有参考音频，不发送给模型")
	}
	if len(attrs) == 0 {
		return name
	}
	return name + "（" + strings.Join(attrs, "；") + "）"
}

// clip 按码点截断，截掉了就补「…」。
func clip(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

func optionLine(o SummaryOption) string {
	s := flatten(o.Name)
	// code 跟在名称后面：submit_plan 的 picks 填的是 code（会话 107398945801502720 里模型把名称当 code 传）。
	// 版本取不到时名称已退回成 code，就不再重复一遍。
	if c := flatten(o.Code); c != "" && c != s {
		s += "（code " + c + "）"
	}
	if d := flatten(o.Description); d != "" {
		s += " —— " + d
	}
	var rec []string
	if len(o.RecommendedModels) > 0 {
		names := make([]string, 0, len(o.RecommendedModels))
		for _, m := range o.RecommendedModels {
			if m = flatten(m); m != "" {
				names = append(names, m)
			}
		}
		if len(names) > 0 {
			rec = append(rec, strings.Join(names, " / "))
		}
	}
	if a := flatten(o.AspectRatio); a != "" {
		rec = append(rec, a)
	}
	switch {
	case len(o.DurationSec) == 1 || (len(o.DurationSec) == 2 && o.DurationSec[0] == o.DurationSec[1]):
		rec = append(rec, fmt.Sprintf("%d 秒", o.DurationSec[0]))
	case len(o.DurationSec) == 2:
		rec = append(rec, fmt.Sprintf("%d~%d 秒", o.DurationSec[0], o.DurationSec[1]))
	}
	if len(rec) > 0 {
		s += "（推荐：" + strings.Join(rec, " · ") + "）"
	}
	return s
}

// summaryLabel 是「所选创意」「所选 Hook」「所选风格」：中文与英文之间留一个空格（§4.2 原样）。
// planLabel 是规划时新钉的选项的标签（「规划中用到的创意」/「规划中用到的 Hook」）。
func planLabel(k Kind) string {
	l := k.Label()
	if l != "" && l[0] < 0x80 {
		return "规划中用到的 " + l
	}
	return "规划中用到的" + l
}

func summaryLabel(k Kind) string {
	l := k.Label()
	if l != "" && l[0] < 0x80 {
		return "所选 " + l
	}
	return "所选" + l
}
