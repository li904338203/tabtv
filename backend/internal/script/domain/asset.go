package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"
)

// 资产 reconcile、描述拼接与版式规范。

// AssetStatus 是资产的生成状态。
type AssetStatus string

const (
	AssetPending    AssetStatus = "pending"
	AssetGenerating AssetStatus = "generating"
	AssetReady      AssetStatus = "ready"
	AssetFailed     AssetStatus = "failed"
)

// AssetSource 是资产图的来源。
type AssetSource string

const (
	SourceAI       AssetSource = "ai"
	SourceUpload   AssetSource = "upload"
	SourceTemplate AssetSource = "template"
)

// Asset 是一个资产（领域视角）。
type Asset struct {
	ID            int64
	Type          AssetType
	Name          string
	Description   string
	Profile       AssetProfile
	MediaObjectID int64
	Source        AssetSource
	Status        AssetStatus
}

// IsReady 资产能不能当参考图用。
func (a Asset) IsReady() bool { return a.Status == AssetReady && a.MediaObjectID != 0 }

// ---------------------------------------------------------------- 版式规范

// SheetSpec 是一种资产的**构图规范**。
//
// ⚠️ 这段版式**由代码固定，不让模型生成**。让模型写的话，它会自作主张
// 给角色塞一把武器、给空场景加一个人 —— 而资产图的全部价值就是
// "下游每一镜都拿它当参考"，多出来的东西会被每一镜继承。
//
// 同理它也不进 `prompt_template`：那是给运营调**创作要求**的，
// 而这段是结构约束，改它等于改下游对参考图的解读方式。
//
// 文字取自老系统的 AssetSheetSpec（CD-79）：那一版逐条写明了布局、光影与排除项，
// 排除项是参考图能复用的前提；新系统起初压成了一句话，出图的版式约束随之变弱。
func SheetSpec(t AssetType) string {
	switch t {
	case AssetScene:
		return sheetMarker + "高质量专业场景设定图，横向构图，以 2 行 2 列的干净网格四等分整齐排版，" +
			"每个格子都是独立的 16:9 横向画面，展示同一场景的四个大全景视角" +
			"（1 为正面中心线大全景视图，镜头正对场景中心轴，构图严格居中，画面同时包含顶面与底面，" +
			"尽量展示完整空间层次、更多环境细节和深景深；2 以 1 的中心线为参考，" +
			"摄像机移动到场景左前方 45 度位置的大全景视图，镜头仍对准场景核心区域；" +
			"3 为以 1 的中心线为参考，摄像机移动到场景右前方 45 度位置的大全景视图；" +
			"4 为镜头在最深处向外拍摄的正中心全景图）。四个视角也可以是东南西北四个方位的视角。" +
			"四个视角必须表现同一地点、同一时间、同一天气、同一光源、同一空间结构和同一美术风格。" +
			"环境清晰，细节丰富，景深较深，光影自然，专业摄影，超清画质。" +
			"不得出现任何人物（这是空场景参考图，必须空无一人），也不得出现人群、背影、剪影、人脸、" +
			"手脚、人物倒影、人物影子、照片人物、屏幕人物、镜中人物；" +
			"不得出现文字、水印、标签、logo。"
	case AssetProp:
		return sheetMarker + "高质量写实道具多角度展示图，横向构图，以 2 行 3 列的干净网格整齐排版，" +
			"展示道具的六个极正视角。纯白色纯净背景，专业产品影棚摄影，标准六视图参考。" +
			"六视图包括（必须是道具六视图）：绝对正前方视图、绝对正后方视图、绝对左侧视图、" +
			"绝对右侧视图、绝对正上方俯拍视图、绝对正下方仰拍视图。" +
			"所有视图必须是同一件道具，材质、颜色、比例、结构完全一致。" +
			"使用超长焦镜头或移轴镜头效果，将透视变形降到最低，物体所有本该平行的边缘在画面中保持平行，" +
			"接近正交投影。每个视图都像在专业产品影棚中用三脚架精密校准拍摄，构图绝对端正，" +
			"物体在每个格子中居中，无任何倾斜、旋转或透视畸变。" +
			"画面中不得出现任何人物、角色、人群、人影；不得出现手、脚、人脸、场景、建筑、自然景观；" +
			"无其他道具；必须无文字、无水印、无 logo、无 UI 元素，不要任何剧情事件，" +
			"保持道具本体清晰，保持完整轮廓，保持所有角度的材质和结构一致。"
	default:
		return sheetMarker + "高质量专业角色设定图，横向构图，纯白色纯净背景，中性摄影棚灯光，" +
			"平光布光；布局结构（必须是角色四视图）：正面面部头部特写（占图片水平 1/3 的空间）" +
			"+ [全身正面视图 + 全身左侧面视图 + 全身背面视图]（占图片水平剩余的 2/3 的空间，" +
			"并列排列），四个视图中间用淡灰色(#E2E2E2)的 2px 细线分割，无任何道具或背景物体。" +
			"光影：中性摄影棚灯光，柔和的前侧光，清晰的轮廓定义，自然的肤色，面部清晰服装可辨识，" +
			"平视镜头，完整全身，无裁剪。不得出现任何道具/武器/食物/饮料/手持物（角色必须空手）；" +
			"不得出现复杂动作、情绪表情、面部遮挡；不得出现环境背景（仅白色）；不得出现其他角色；" +
			"确保所有视图中的面部特征、发型、体型和服装保持一致；不得出现文字、水印、标签、logo。"
	}
}

// sheetMarker 是描述里已含版式的标记。
//
// ⚠️ 用它判断"要不要再套一次模板"：不判的话，编辑过的描述会被
// 二次套模板，版式串被塞进"识别特征"的槽位 —— 重复且零约束，
// 模型只会更困惑（老系统的真实 bug）。
const sheetMarker = "构图规范："

// descriptionLabels 是资产描述里**由代码写的**段落标签（质量检查不把它们当成「表单字段直贴」）。
var descriptionLabels = map[string]bool{
	"名称": true, "描述": true, "服装": true, "特征": true, "前背景": true, "氛围": true,
	"构图规范": true, "视觉风格": true, "用途": true, "主体": true,
}

// BuildDescription 把档案拼成资产描述。
//
// 形态照老系统（CD-79）：分段、带标签、段与段之间空一行 ——
//
//	名称：唐僧 金蝉子
//	描述：<外形>
//	服装：<逐部位服装>        （仅角色）
//	特征：<辨识细节>          （角色、道具）
//	前背景：<背景层与前景层>  （仅场景）
//	氛围：<氛围基调>          （仅场景）
//	构图规范：<按类型固定的版式>
//	视觉风格：<全片风格>
//
// 界面上这一整段可读可改，出图时原样发出去（所见即所得）。此前是 `名称 / 外形 / 服装 / 特征` 挤在一行，
// 用户分不清哪一段是什么，改的时候也不知道该往哪写。
//
// ⚠️ 模型漏给某个实体的档案时**退回模板描述而不是编造**：
// 编造出来的外观会被下游每一镜继承，而它跟剧情毫无关系。
// 缺档案是个该让人知道的事实，调用方要把漏掉的名单汇总告警。
func BuildDescription(name string, t AssetType, p AssetProfile, globalStyle string) string {
	var b strings.Builder
	b.WriteString("名称：" + name)
	if alias := strings.TrimSpace(p.Alias); alias != "" {
		b.WriteString(" " + alias)
	}
	section := func(label, value string) {
		if value = strings.TrimSpace(value); value != "" {
			b.WriteString("\n\n" + label + "：" + value)
		}
	}
	var fields []string
	if t == AssetScene {
		fields = []string{p.Appearance, p.Layers, p.SceneMood()}
	} else {
		fields = []string{p.Appearance, p.Costume, p.Features}
	}
	if strings.TrimSpace(strings.Join(fields, "")) == "" {
		// 一个档案字段都没有：退回模板描述。
		section("描述", templateHint(t))
	} else {
		section("描述", p.Appearance)
		switch t {
		case AssetScene:
			section("前背景", p.Layers)
			section("氛围", p.SceneMood())
		case AssetCharacter:
			section("服装", p.Costume)
			section("特征", p.Features)
		default:
			section("特征", p.Features)
		}
	}
	b.WriteString("\n\n" + SheetSpec(t))
	section("视觉风格", globalStyle)
	return b.String()
}

func templateHint(t AssetType) string {
	switch t {
	case AssetCharacter:
		return "剧情中出现的角色，外观未在剧本中明确描述"
	case AssetScene:
		return "剧情中出现的场景，环境未在剧本中明确描述"
	case AssetProp:
		return "剧情中出现的道具，外观未在剧本中明确描述"
	}
	return ""
}

// ProfileSummary 取描述里「长什么样」那部分，压成一行，给镜头提示词的参考图对应关系用。
//
// ⚠️ 构图规范（以及排在它后面的视觉风格）属于**设定图**：喂给分镜图提示词的话，
// 实拍镜头会收到「纯白背景、四视图、角色必须空手」，与镜头本身的场景直接冲突。
// 名称行也去掉（对应关系里已经写了名字）。
func ProfileSummary(desc string) string {
	if i := strings.Index(desc, sheetMarker); i >= 0 {
		desc = desc[:i]
	}
	var parts []string
	for _, line := range strings.Split(desc, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "名称：") {
			continue
		}
		parts = append(parts, line)
	}
	return strings.Join(parts, "；")
}

// reuseJob 这张参考图要让下游保持什么一致（出图提示词的「用途」，取自老系统）。
func reuseJob(t AssetType) string {
	switch t {
	case AssetScene:
		return "固定该场景的地理关系与观察方向，供后续镜头核对入口、路径与光向"
	case AssetProp:
		return "固定该道具的尺度、形制与可操作部位，供后续镜头核对持握与状态"
	}
	return "保持该角色跨镜头的身份识别与当前造型"
}

// AssetPrompt 把资产描述变成出图提示词。
//
// ⚠️ 描述里已经含「构图规范：」就**直接照发**，不再套一遍模板 ——
// 套第二遍的结果是把版式串塞进"识别特征"的槽位，重复且零约束。
//
// 形态照老系统（CD-79）：`用途：…。` + 描述原文。描述里已有构图规范与视觉风格，
// 不在这里二次拼装 —— 那样会与描述里已有的同名段落重复，甚至冲突（同时要求「四视图」和「六视图」）。
func AssetPrompt(a Asset) string {
	head := "用途：" + reuseJob(a.Type) + "。\n"
	desc := strings.TrimSpace(a.Description)
	if desc == "" {
		return head + "主体：" + a.Name + "。\n\n" + SheetSpec(a.Type)
	}
	if strings.Contains(desc, sheetMarker) {
		return head + desc
	}
	// 用户手写、没带版式的描述：补上类型默认版式，避免出成随手一张插画
	return head + desc + "\n\n" + SheetSpec(a.Type)
}

// ---------------------------------------------------------------- 质量检查

// Lint 是一条资产质量提示。
type Lint struct {
	Code    string
	Message string
}

// LintAsset 检查资产描述的质量。
//
// ⚠️ **只警告不拦截**。这几条都是质量问题不是错误：
// 名字没有语义（"角色1"）、描述里直贴了表单字段（"身份:女配"）、
// 描述里写了别的角色名 —— 拦下来的话用户就走不下去了，
// 而他可能只是还没来得及改。把问题**指出来**，由他决定。
func LintAsset(a Asset, otherNames []string) []Lint {
	var out []Lint
	if isPlaceholderName(a.Name) {
		out = append(out, Lint{"name_not_semantic",
			fmt.Sprintf("「%s」看起来是占位名，改成有语义的名字才好在镜头里 @ 引用", a.Name)})
	}
	if field, ok := hasFormField(a.Description); ok {
		out = append(out, Lint{"form_field_pasted",
			fmt.Sprintf("描述里直贴了表单字段「%s」，模型会把它当画面内容", field)})
	}
	for _, other := range otherNames {
		if other == a.Name || other == "" {
			continue
		}
		if strings.Contains(a.Description, other) {
			// ⚠️ 这一条最要紧：资产图是给下游当参考的，描述里写了别人的名字，
			// 出来的图会把两个角色画进同一张，参考价值直接归零。
			out = append(out, Lint{"other_name_in_description",
				fmt.Sprintf("描述里出现了另一个资产「%s」，出图会把两个对象画进同一张", other)})
			break
		}
	}
	return out
}

func isPlaceholderName(name string) bool {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return true
	}
	// "角色1" "场景A" "道具 2" 这类：去掉类型词与数字后什么都不剩。
	for _, word := range []string{"角色", "场景", "道具", "人物", "character", "scene", "prop"} {
		rest := strings.TrimSpace(strings.TrimPrefix(strings.ToLower(trimmed), word))
		if rest != strings.ToLower(trimmed) && isAlnum(rest) {
			return true
		}
	}
	return false
}

func isAlnum(s string) bool {
	for _, r := range s {
		if !(r >= '0' && r <= '9') && !(r >= 'a' && r <= 'z') && !(r >= 'A' && r <= 'Z') {
			return false
		}
	}
	return true
}

// hasFormField 检测 `字段:值` 这种直贴。
func hasFormField(desc string) (string, bool) {
	for _, line := range strings.Split(desc, "\n") {
		line = strings.TrimSpace(line)
		for _, sep := range []string{"：", ":"} {
			i := strings.Index(line, sep)
			if i <= 0 {
				continue
			}
			key := strings.TrimSpace(line[:i])
			// 描述的段落标签（名称 / 描述 / 服装 / 构图规范 / 视觉风格…）是代码写的，不算。
			if descriptionLabels[key] {
				continue
			}
			if n := utf8.RuneCountInString(key); n > 0 && n <= 6 && !strings.ContainsAny(key, "，。,. ") {
				return line[:i+len(sep)], true
			}
		}
	}
	return "", false
}

// ---------------------------------------------------------------- reconcile

// ReconcileInput 是一次资产对账的输入。
type ReconcileInput struct {
	// Entities 本脚本全部镜头里出现的实体（已按镜头顺序去重）。
	Entities []Entity
	// Profiles 模型给的全片档案。
	Profiles []AssetProfile
	// Existing **本脚本已有的**资产。
	//
	// ⚠️ 必须是"本脚本的"，不能是工作区全部资产。老系统按用户级同名匹配，
	// 结果新节点里的 `@唐僧` 直接命中数周前的记录，把别的节点的成品图
	// **和 failed 状态一起**继承过来 —— 用户看到凭空出现的"生成失败"
	// 和一张不认识的占位图。自动识别**绝不跨脚本复用**；
	// 跨脚本复用是用户**手动挑选**的动作（AV-20）。
	Existing []Asset
	// GlobalStyle 全片风格，写进每个资产描述的「视觉风格」段（老系统同款，CD-79）。
	GlobalStyle string
}

// ReconcilePlan 是对账结果。
type ReconcilePlan struct {
	Create []Asset
	Update []Asset
	// Unmatched 在镜头里出现、但模型没给档案的实体名。
	//
	// ⚠️ 这不是错误，是**该让人知道的事实**：这些资产会退回模板描述。
	// 调用方要汇总成一条告警，而不是逐个静默处理。
	Unmatched []string
	// Orphan 已有资产里，本次镜头中不再出现的。
	//
	// ⚠️ **只报不删**：用户可能刚把某一镜删掉、正准备加回来，
	// 而资产图是花钱生成的。删掉的代价不可逆，留着的代价只是列表长一点。
	Orphan []Asset
}

// Reconcile 按镜头实体与档案，算出资产该建、该更新、哪些缺档案。
func Reconcile(in ReconcileInput) ReconcilePlan {
	profiles := map[string]AssetProfile{}
	for _, p := range in.Profiles {
		profiles[p.Name] = p
	}
	existing := map[string]Asset{}
	for _, a := range in.Existing {
		existing[a.Name] = a
	}

	var plan ReconcilePlan
	seen := map[string]bool{}
	for _, e := range in.Entities {
		name := strings.TrimSpace(e.Name)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true

		profile, hasProfile := profiles[name]
		if !hasProfile {
			plan.Unmatched = append(plan.Unmatched, name)
			profile = AssetProfile{Name: name, Type: e.Type}
		}
		typ := e.Type
		if hasProfile && validAssetType(profile.Type) {
			typ = profile.Type
		}

		old, exists := existing[name]
		if !exists {
			plan.Create = append(plan.Create, Asset{
				Type: typ, Name: name, Profile: profile,
				Description: BuildDescription(name, typ, profile, in.GlobalStyle),
				Source:      SourceAI, Status: AssetPending,
			})
			continue
		}
		// ⚠️ 已经出过图或被用户改过的资产**不覆盖描述**：
		// 重新生成分镜会重跑 reconcile，而用户可能已经把描述调好、
		// 甚至已经花钱出了图。覆盖等于把他的工作删掉。
		if old.Status == AssetReady || old.Source == SourceUpload {
			continue
		}
		next := old
		next.Type = typ
		next.Profile = profile
		next.Description = BuildDescription(name, typ, profile, in.GlobalStyle)
		if next.Description != old.Description || next.Type != old.Type {
			plan.Update = append(plan.Update, next)
		}
	}

	for _, a := range in.Existing {
		if !seen[a.Name] {
			plan.Orphan = append(plan.Orphan, a)
		}
	}
	sort.Strings(plan.Unmatched)
	return plan
}

// ---------------------------------------------------------------- 引用排序

// ShotRef 是一条镜头 → 资产的引用。
type ShotRef struct {
	AssetID int64
	Name    string
	Sort    int
}

// OrderRefs 按**镜头实体的出现顺序**给引用排序。
//
// ⚠️ 顺序即锚定：参考图的编号（`@图1`、`refer_image` 的 id）与提示词里
// 提到它们的顺序必须一一对应。顺序错了，模型会把 A 的特征画到 B 身上，
// 而画面看上去"挺正常"—— 这种错最难被发现。
//
// ⚠️ 按实体顺序而不是按资产 ID/名字排序：同一个资产在不同镜头里的
// 位次不同，排序必须是**每个镜头各算一次**的。
func OrderRefs(entities []Entity, byName map[string]int64) []ShotRef {
	var out []ShotRef
	seen := map[string]bool{}
	for _, e := range entities {
		name := strings.TrimSpace(e.Name)
		if name == "" || seen[name] {
			continue
		}
		id, ok := byName[name]
		if !ok {
			// 资产还没建出来（reconcile 之前），跳过而不是插一个空位：
			// 空位会让后面所有编号错位。
			continue
		}
		seen[name] = true
		out = append(out, ShotRef{AssetID: id, Name: name, Sort: len(out) + 1})
	}
	return out
}

// ---------------------------------------------------------------- 批量生成

// BatchDecision 是批量生成里对一项的处理决定。
type BatchDecision struct {
	Asset   Asset
	Skip    bool
	Reason  string
	Stopped bool
}

// PlanBatch 规划一批资产生成。
//
// ⚠️ **余额不足要停掉后续项**（AV-13），不是逐项失败：逐项失败的话，
// 用户会收到十条一模一样的"积分不足"，而且每一条都占了一次任务记录。
// 停下来并说清楚"还差多少"才是有用的。
//
// ⚠️ 已经 ready 的资产**跳过**而不是重做：重做要再花一次钱，
// 而用户点的是"批量生成"，他的意思是"把没有的补上"。
func PlanBatch(assets []Asset, unitCost, balance int) []BatchDecision {
	out := make([]BatchDecision, 0, len(assets))
	remaining := balance
	stopped := false
	for _, a := range assets {
		switch {
		case stopped:
			out = append(out, BatchDecision{Asset: a, Skip: true,
				Reason: "积分不足，已停止后续生成", Stopped: true})
		case a.IsReady():
			out = append(out, BatchDecision{Asset: a, Skip: true, Reason: "已有资产图，跳过"})
		case remaining < unitCost:
			stopped = true
			out = append(out, BatchDecision{Asset: a, Skip: true,
				Reason:  fmt.Sprintf("积分不足：还需 %d，可用 %d", unitCost, remaining),
				Stopped: true})
		default:
			remaining -= unitCost
			out = append(out, BatchDecision{Asset: a})
		}
	}
	return out
}

// ---------------------------------------------------------------- 上传约束

// MaxAssetUploadBytes 资产图上传上限（AV-12）。
const MaxAssetUploadBytes int64 = 20 << 20

// AllowedAssetMIME 允许的资产图类型。
//
// ⚠️ 以**实际文件头**为准，不信客户端声明的 MIME（GO-27）——
// 校验在媒体域上传时已经做过，这里只约束"哪些类型能当资产图"。
var AllowedAssetMIME = map[string]bool{
	"image/png":  true,
	"image/jpeg": true,
	"image/webp": true,
}

func ValidateAssetMedia(mime string, size int64) error {
	if !AllowedAssetMIME[mime] {
		return fmt.Errorf("资产图只支持 PNG / JPEG / WebP，当前是 %s", mime)
	}
	if size > MaxAssetUploadBytes {
		return fmt.Errorf("资产图不能超过 %d MiB，当前 %.1f MiB",
			MaxAssetUploadBytes>>20, float64(size)/float64(1<<20))
	}
	return nil
}

// ValidateAssetName 校验资产名。
func ValidateAssetName(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("资产名不能为空")
	}
	if utf8.RuneCountInString(name) > MaxEntityNameRunes {
		// 与实体名同一个上限：名字要能在镜头描述里 @ 出来。
		return fmt.Errorf("资产名不能超过 %d 个字", MaxEntityNameRunes)
	}
	return nil
}

// FingerprintOf 取一段文本的短指纹，用于幂等键的可变部分。
//
// ⚠️ 改了描述再点生成应当是**一次新任务**，而不是命中上一次的结果 ——
// 用户改描述的意思就是"上次那张不对，重来一张"。
// ⚠️ 必须先哈希：描述可能上千字，而幂等键列宽有限。
func FingerprintOf(s string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(s)))
	return hex.EncodeToString(sum[:4])
}
