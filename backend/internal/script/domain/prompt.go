package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

// 提示词的模式、状态与上游指纹（AV-15 / AV-16 / AV-17）。

// PromptKind 是提示词的种类。
type PromptKind string

const (
	PromptImage PromptKind = "image"
	PromptVideo PromptKind = "video"
)

func ValidPromptKind(k PromptKind) bool { return k == PromptImage || k == PromptVideo }

// PromptStatus 是提示词的就绪状态。
type PromptStatus string

const (
	// PromptPending 从未合成过。
	PromptPending PromptStatus = "pending"
	// PromptReady 已合成且与当前上游一致。
	PromptReady PromptStatus = "ready"
	// PromptStale 已合成，但上游（镜头描述、引用资产、全片风格）之后变过。
	//
	// ⚠️ **不是失败**：那段词还在、还能用，只是可能已经和画面对不上了。
	// 直接清空或标 failed 会让用户丢掉自己改过的词；让他看到"可能过期"
	// 并自己决定要不要重算，才是对的。
	PromptStale PromptStatus = "stale"
	// PromptFailed 合成失败。
	PromptFailed PromptStatus = "failed"
)

// PromptSource 是提示词的来源（AV-16）。
type PromptSource string

const (
	// SourcePromptAI 模型合成 —— **收费**。
	SourcePromptAI PromptSource = "ai"
	// SourcePromptTemplate 模板组装 —— **0 积分**：它完全由代码拼，
	// 没有调用上游，收费没有任何依据。
	SourcePromptTemplate PromptSource = "template"
	// SourcePromptManual 用户手写。
	SourcePromptManual PromptSource = "manual"
)

func ValidPromptSource(s PromptSource) bool {
	return s == SourcePromptAI || s == SourcePromptTemplate || s == SourcePromptManual
}

// ---------------------------------------------------------------- 上游指纹

// Upstream 是一条提示词依赖的全部上游内容。
//
// ⚠️ 少算一项就会漏掉一类"上游变了"：比如不把资产描述算进去的话，
// 用户改完角色外观，提示词还显示 ready —— 他拿着一段过时的词去出图，
// 画面里那个角色还是旧的样子，而界面上一切正常。
type Upstream struct {
	GlobalStyle string
	Description string
	StartState  string
	EndState    string
	ShotSize    string
	Lighting    string
	Dialogue    string
	SoundEffect string
	CameraMove  string
	DurationSec int
	// Assets 按**引用顺序**给出（sort 升序）。顺序变了指纹也要变 ——
	// 参考图的锚定顺序变了，提示词里的编号就对不上了。
	Assets []PromptAsset
}

// PromptAsset 是提示词用到的一个参考资产。
type PromptAsset struct {
	Name string
	Type AssetType
	// Description 参与指纹：改了资产描述，下游提示词就该过期。
	Description string
}

// Fingerprint 算上游指纹。
//
// ⚠️ 用**有序拼接**而不是 map：map 的遍历顺序每次都不同，
// 算出来的指纹会随机变化，于是所有提示词永远是 stale。
func (u Upstream) Fingerprint() string {
	var b strings.Builder
	fmt.Fprintf(&b, "style=%s\n", u.GlobalStyle)
	fmt.Fprintf(&b, "desc=%s\nstart=%s\nend=%s\n", u.Description, u.StartState, u.EndState)
	fmt.Fprintf(&b, "size=%s\nlight=%s\nmove=%s\n", u.ShotSize, u.Lighting, u.CameraMove)
	fmt.Fprintf(&b, "dialogue=%s\nsfx=%s\ndur=%d\n", u.Dialogue, u.SoundEffect, u.DurationSec)
	for i, a := range u.Assets {
		fmt.Fprintf(&b, "asset%d=%s|%s|%s\n", i+1, a.Name, a.Type, a.Description)
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:8])
}

// EffectiveStatus 按指纹算出**当前**该显示的状态。
//
// ⚠️ 这是 stale 判定的**唯一正确来源**：在改动点主动标 stale 只是即时反馈，
// 漏掉任何一个调用点都不会有编译错误。指纹存在行上，任何路径改了上游
// 都会被比出来。
func EffectiveStatus(stored PromptStatus, storedFingerprint, current string) PromptStatus {
	switch stored {
	case PromptPending, PromptFailed:
		// 从没合成过 / 上次失败了：与上游变没变无关。
		return stored
	}
	if storedFingerprint == "" {
		// 有词但没有指纹（手写的、或旧数据）：无从比较，按它自己说的算。
		return stored
	}
	if storedFingerprint != current {
		return PromptStale
	}
	return PromptReady
}

// ---------------------------------------------------------------- 模板组装

// ComposeTemplate 用模板把上游拼成提示词 —— **完全由代码拼，0 积分**。
//
// ⚠️ 参考图必须**按引用顺序编号**，且编号与 `shot_asset_ref.sort` 一一对应。
// 错位的话模型会把 A 的特征画到 B 身上，而画面看上去"挺正常" ——
// 这种错最难被发现。
func ComposeTemplate(kind PromptKind, u Upstream) string {
	var b strings.Builder

	if u.GlobalStyle != "" {
		fmt.Fprintf(&b, "整体风格：%s\n", u.GlobalStyle)
	}
	// ⚠️ 识别信息排在审美词之前：反过来的话模型会先照着风格画，
	// 把用户真正要的内容挤掉（与生成侧的拼接顺序是同一条规则）。
	if len(u.Assets) > 0 {
		b.WriteString("参考图对应关系：\n")
		for i, a := range u.Assets {
			fmt.Fprintf(&b, "  图%d = %s（%s）：%s\n",
				i+1, a.Name, assetTypeLabel(a.Type), ProfileSummary(a.Description))
		}
	}
	fmt.Fprintf(&b, "画面内容：%s\n", orPlaceholder(u.Description, "（镜头描述为空）"))

	if kind == PromptImage {
		writeIfSet(&b, "景别", u.ShotSize)
		writeIfSet(&b, "光线", u.Lighting)
		if u.StartState != "" {
			// 图片取**起始状态**那一帧：分镜图是这一镜的第一画面。
			fmt.Fprintf(&b, "定格于：%s\n", u.StartState)
		}
		b.WriteString(imageSpec)
		return b.String()
	}

	writeIfSet(&b, "运镜", u.CameraMove)
	writeIfSet(&b, "景别", u.ShotSize)
	if u.StartState != "" || u.EndState != "" {
		fmt.Fprintf(&b, "运动边界：从「%s」到「%s」\n",
			orPlaceholder(u.StartState, "画面起始状态"), orPlaceholder(u.EndState, "画面结束状态"))
	}
	if u.DurationSec > 0 {
		fmt.Fprintf(&b, "时长：%d 秒\n", u.DurationSec)
	}
	writeIfSet(&b, "音效", u.SoundEffect)
	b.WriteString(videoSpec)
	return b.String()
}

// imageSpec / videoSpec 是**由代码固定**的使用规则与禁止项。
//
// ⚠️ 不让模型写这几段（与资产版式同一条理由）：模型每次重写会漂移，
// 改一个字下游行为就变；而且它会把输出撑长 —— 输出越长越容易在
// JSON 字符串里写出裸引号，整批作废。
//
// ⚠️ 它们也不进 `prompt_template`：那是给运营调**创作要求**的，
// 这几段是结构约束，改它等于改下游对参考图的解读方式。
const imageSpec = `参考图使用规则：只从参考图取人物/场景/道具的外观特征，
不要照搬参考图的构图、机位与排版（参考图是多视图拼版，不是画面本身）。
禁止项：不出现文字、水印、边框、分屏；不添加参考图里没有的人物。`

const videoSpec = `参考图使用规则：只从参考图取外观特征，不要照搬其构图与排版。
约束项：镜头内保持同一场景与同一批角色；不要在一镜之内切换机位或换人。
禁止项：不出现文字、水印、转场特效；不添加剧情里没有的动作。`

func assetTypeLabel(t AssetType) string {
	switch t {
	case AssetCharacter:
		return "角色"
	case AssetScene:
		return "场景"
	case AssetProp:
		return "道具"
	}
	return string(t)
}

func writeIfSet(b *strings.Builder, label, value string) {
	if strings.TrimSpace(value) != "" {
		fmt.Fprintf(b, "%s：%s\n", label, value)
	}
}

func orPlaceholder(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}

// ---------------------------------------------------------------- 批量

// ComposeTarget 是批量合成里的一项。
type ComposeTarget struct {
	ShotID int64
	ShotNo int
	Kinds  []PromptKind
}

// MaxComposeConcurrency 批量合成的并发上限（AV-17）。
//
// ⚠️ 上限是给**上游**留余地的，不是为了我们自己跑得快：同步 LLM 调用
// 一次并发太多会直接撞上游的账号级限流，换回来一批 429 ——
// 而那时每一条都已经扣过费了。
const MaxComposeConcurrency = 5

// BatchQuote 是一次批量合成的报价明细。
//
// ⚠️ 费用必须由**服务端统一报价**（GAP-21）。老系统前端用
// 「选中数 × 6」这种写死的估算，而后端按定价目录算 —— 两边对不上，
// 用户看到的价和实际扣的钱不是一回事。
type BatchQuote struct {
	// AICount 走模型合成的条数（收费）。
	AICount int
	// TemplateCount 模板组装的条数（0 积分）。
	TemplateCount int
	// SkipCount 已经 ready 且上游没变、无需重算的条数。
	SkipCount int
	UnitCost  int
	Total     int
}

// PlanCompose 规划一次批量合成。
//
// ⚠️ 已经 ready 且指纹一致的**跳过**：重算要再花一次钱，而用户点
// "批量合成"的意思是"把没有的和过期的补上"。
// ⚠️ `force` 为真时不跳过 —— 用户显式点了"重新生成"。
func PlanCompose(items []ComposeItem, source PromptSource, unitCost int, force bool) (
	[]ComposeItem, BatchQuote) {
	var quote BatchQuote
	quote.UnitCost = unitCost
	out := make([]ComposeItem, 0, len(items))
	for _, it := range items {
		if !force && it.Status == PromptReady {
			it.Skip = true
			it.Reason = "已就绪且上游未变，跳过"
			quote.SkipCount++
			out = append(out, it)
			continue
		}
		if source == SourcePromptTemplate {
			quote.TemplateCount++
		} else {
			quote.AICount++
		}
		out = append(out, it)
	}
	quote.Total = quote.AICount * unitCost
	return out, quote
}

// ComposeItem 是批量合成里的一条（镜头 × 种类）。
type ComposeItem struct {
	ShotID int64
	ShotNo int
	Kind   PromptKind
	Status PromptStatus
	Skip   bool
	Reason string
}

// SortComposeItems 让批量结果按镜号、种类稳定排序。
//
// 不排的话每次返回的顺序都不同，前端的进度列表会乱跳。
func SortComposeItems(items []ComposeItem) {
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].ShotNo != items[j].ShotNo {
			return items[i].ShotNo < items[j].ShotNo
		}
		return items[i].Kind < items[j].Kind
	})
}
