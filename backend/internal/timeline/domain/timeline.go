// Package domain 是时间线的纯计算层（NEW-07~NEW-09）。
//
// ⚠️ 这一层**不碰数据库、不发网络**：排布、裁切、分段、装配全是算出来的，
// 所以每条规则都能单独测。与 workflow/domain 同一条分界 ——
// 时间线的错误几乎都是"错得很安静"（多一帧黑屏、少半秒对白），
// 只有把规则摊成纯函数才验得住。
package domain

import (
	"encoding/json"
	"fmt"
	"sort"
)

// 轨道种类。
const (
	TrackVideo    = "video"
	TrackAudio    = "audio"
	TrackSubtitle = "subtitle"
	// TrackText 文字轨（标题、贴字，docs/agent/smartcut SC-F-50）。不放素材，放文字层。
	TrackText = "text"
)

// 轨道用途（SC-F-22、SC-F-61）。人声闪避按它找「谁压谁」：配乐被人声压低。
//
// ⚠️ 画面轨可以标 voice：口播素材的人声在画面原声里，不在任何音频轨上。
const (
	RoleMusic = "music"
	RoleVoice = "voice"
	RoleSfx   = "sfx"
)

// 片段是谁放进来的（SCD-5）。用户动过的片段改写为 user，AI 的追加指令不改它。
const (
	OriginAI   = "ai"
	OriginUser = "user"
	// OriginSync 连线即入轨自动铺上来的（SC-F-13）。
	OriginSync = "sync"
)

// 时间线的各类上限（03 SC-PARAM-21，设计建议值，可调）。
const (
	MaxClips          = 500
	MaxAudioTracks    = 6
	MaxSubtitleTracks = 4
	MaxTextTracks     = 4
	MaxTextLayers     = 100
	MaxSubtitles      = 2000
	MaxTextChars      = 500
	MaxAlternates     = 10
	// MaxTimelineMs 成片最长 30 分钟（与渲染的 MaxRenderMs 同一个数）。
	MaxTimelineMs = 30 * 60 * 1000
)

// Alternate 是片段曾经用过的素材（重拍、去字幕之前的原片），连同原来的取用区间（SCD-35）。
//
// ⚠️ ID 在 JSON 里一律是字符串（GO-34）：这一份会原样进 jsonb 列、原样回给前端。
type Alternate struct {
	MediaID      int64  `json:"mediaId,string"`
	SourceInMs   int    `json:"sourceInMs"`
	SourceOutMs  int    `json:"sourceOutMs"`
	Kind         string `json:"kind"`
	GenerationID int64  `json:"generationId,string,omitempty"`
}

// 备选的来历。
const (
	AlternateOriginal   = "original"
	AlternateRegen      = "regen"
	AlternateDesubtitle = "desubtitle"
)

// PushAlternate 把一份素材压进备选，超过上限丢最早的。
func PushAlternate(list []Alternate, a Alternate) []Alternate {
	out := append(append([]Alternate(nil), list...), a)
	if len(out) > MaxAlternates {
		out = out[len(out)-MaxAlternates:]
	}
	return out
}

// TextLayer 是文字轨上的一段文字（SC-F-50）。起止为成片时间。
//
// `Style` / `Box` 原样存 jsonb：未知字段原样保留（GO-35），只校验认得的那几项。
type TextLayer struct {
	ID      int64
	TrackID int64
	StartMs int
	EndMs   int
	Text    string
	Style   json.RawMessage
	Box     json.RawMessage
}

// Anchor 是字幕或音效「跟随片段」的锚点（SC-F-56）：属于哪个片段、对应素材的哪一段时间。
type Anchor struct {
	ClipID     int64 `json:"clipId,string"`
	SrcStartMs int   `json:"srcStartMs"`
	SrcEndMs   int   `json:"srcEndMs"`
}

// ObjectOrNil 原样 JSON 只认对象：空、null、坏值、非对象（历史数据或手工改库留下的）一律当「没有」。
// 同 ParseTransition：一个字段坏了不该让整条时间线打不开。
func ObjectOrNil(raw json.RawMessage) json.RawMessage {
	var m map[string]json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &m) != nil || m == nil {
		return nil
	}
	return raw
}

// ParseAlternates 从 jsonb 读备选。整列坏了当没有；单条坏的（缺素材、区间倒置、来历不认识）跳过。
func ParseAlternates(raw json.RawMessage) []Alternate {
	var items []json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &items) != nil {
		return nil
	}
	out := make([]Alternate, 0, len(items))
	for _, it := range items {
		var a Alternate
		if json.Unmarshal(it, &a) != nil || a.MediaID == 0 || a.SourceOutMs <= a.SourceInMs ||
			(a.Kind != AlternateOriginal && a.Kind != AlternateRegen && a.Kind != AlternateDesubtitle) {
			continue
		}
		out = append(out, a)
	}
	if len(out) > MaxAlternates {
		out = out[len(out)-MaxAlternates:]
	}
	return out
}

// ParseAnchor 从 jsonb 读锚点。空值与坏值都当「不跟随」——坏的锚点不该让整条时间线打不开。
func ParseAnchor(raw json.RawMessage) (Anchor, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return Anchor{}, false
	}
	var a Anchor
	if err := json.Unmarshal(raw, &a); err != nil || a.ClipID == 0 || a.SrcEndMs <= a.SrcStartMs {
		return Anchor{}, false
	}
	return a, true
}

// Clip 是轨道上的一段。
type Clip struct {
	ID      int64
	TrackID int64
	// Idx 从 1 起，轨内唯一且连续。
	Idx     int
	MediaID int64
	// SourceInMs / SourceOutMs 是**素材内**的取用区间。
	SourceInMs  int
	SourceOutMs int
	// StartMs 是**时间线上**的落点。
	//
	// ⚠️ 视频轨的 StartMs 由服务端按顺序重算，**不信客户端**（见 Reflow）。
	StartMs    int
	DurationMs int
	Speed      int
	Volume     int
	FadeInMs   int
	FadeOutMs  int
	// SourceShotID 是自动装配的溯源；手工加的段为 0。
	SourceShotID int64
	// TransitionIn 与上一段的衔接（第一段上＝从黑场淡入）。
	//
	// ⚠️ 接缝的转场**只记在后一段上**，不在前一段记一份 ——
	// 两边各记一半的话，改了一边渲染该听谁的没人说得清（见 transition.go）。
	TransitionIn Transition
	// TransitionOut **只在最后一段**有意义（淡出到黑场）。
	TransitionOut Transition

	// Transform 画面变换（SC-F-43），原样 JSON：未知字段原样保留，读取用 ParseTransform。
	Transform json.RawMessage
	// Origin 谁放进来的（OriginAI / OriginUser / OriginSync）。空串按 user 处理。
	Origin string
	// SourceNodeID 来自哪个画布节点（素材编号、过期判断、「来源已断开」）。
	SourceNodeID string
	// Alternates 曾经用过的素材，可以切回（SCD-35）。
	Alternates []Alternate
	// PendingGenerationID 片段级生成进行中（导出预检据此拦下，SC-F-138）。
	PendingGenerationID int64
	// Anchor 音频轨上的片段跟随画面轨的一段（SC-F-56，按画面生成的音效）：{clipId, srcStartMs, srcEndMs}，原样 JSON。
	// 读的时候按它现算落点、取用区间与速度（FollowClip）。
	Anchor json.RawMessage
	// Hidden 读取时算出、**不落库**：跟随的那一段被裁到看不见了（不渲染、不播放，恢复区间后重新出现）。
	Hidden bool
}

// SourceLen 取用区间的长度。
func (c Clip) SourceLen() int { return c.SourceOutMs - c.SourceInMs }

// PlayLen 这一段在时间线上占多久。
//
// ⚠️ 变速之后**时间线时长 ≠ 素材时长**：2 倍速的 10 秒素材只占 5 秒。
// 直接拿 SourceLen 当时长的话，后面每一段的落点都错，
// 而画面看上去只是"对不上音"——最难查的一类。
func (c Clip) PlayLen() int {
	if c.Speed <= 0 {
		return c.SourceLen()
	}
	return c.SourceLen() * 100 / c.Speed
}

// DefaultSpeed / DefaultVolume 是新片段的默认值。
const (
	DefaultSpeed  = 100
	DefaultVolume = 100
	// MinClipMs 一段最短多久。
	//
	// ⚠️ 不是洁癖：比一帧还短的片段在 ffmpeg 的 concat 里会被丢掉，
	// 于是成片比时间线短 —— 而时间线上看着是有那一段的。
	// 30fps 下一帧约 33ms，取 100ms 留足余量。
	MinClipMs = 100
)

// ValidateClip 校验一段的取值。
func ValidateClip(c Clip, sourceDurationMs int) error {
	if c.MediaID == 0 {
		return fmt.Errorf("片段缺少素材")
	}
	if c.SourceInMs < 0 {
		return fmt.Errorf("取用起点不能为负")
	}
	// ⚠️ 按**时间线上的长度**判，不按素材长度：4 倍速下 100ms 的素材只占 25ms，
	// 不到一帧，照样会被 concat 丢掉。整份保存（ValidateContent）与切分（Split）也按这个判。
	if c.PlayLen() < MinClipMs {
		return fmt.Errorf("片段太短（时间线上不足 %d 毫秒），渲染时会被丢掉", MinClipMs)
	}
	// ⚠️ 素材时长未知时**放行**：拿不到数字就没法比较，拒绝等于把一批
	// ffprobe 认不出的格式挡在门外（与蒙版尺寸未知时放行同一条）。
	if sourceDurationMs > 0 && c.SourceOutMs > sourceDurationMs {
		return fmt.Errorf("取用区间超出素材本身：素材 %d 毫秒，取到 %d 毫秒",
			sourceDurationMs, c.SourceOutMs)
	}
	if c.Speed < 25 || c.Speed > 400 {
		return fmt.Errorf("变速只支持 0.25~4 倍")
	}
	if c.Volume < 0 || c.Volume > 200 {
		return fmt.Errorf("音量只支持 0~200")
	}
	if c.FadeInMs < 0 || c.FadeOutMs < 0 {
		return fmt.Errorf("淡入淡出不能为负")
	}
	// ⚠️ 淡入 + 淡出不能超过这一段本身：超了的话 ffmpeg 的 afade 会把整段
	// 压成静音，而用户以为自己只是加了个柔和的收尾。
	if c.FadeInMs+c.FadeOutMs > c.PlayLen() {
		return fmt.Errorf("淡入淡出合计 %d 毫秒，超过片段本身的 %d 毫秒",
			c.FadeInMs+c.FadeOutMs, c.PlayLen())
	}
	return nil
}

// Reflow 按 **Idx 顺序**把画面轨的每段接起来，重算 Idx 与 StartMs（成片时间，见 Sequence）。
// 用在"从库里读出来"的场合 —— 那时顺序只在 Idx 里。只给画面轨用。
//
// ⚠️ 视频轨的 StartMs **必须由服务端算**，不能信客户端传来的值。
// 信它的结果是成片里出现黑帧（有空隙）或画面重叠（有交叠），
// 而时间线界面上看着是连续的 —— 前端的像素取整与服务端的毫秒不是一回事。
//
// ⚠️ Idx 也在这里重编成 1..N 连续：留空号的话，之后任何"插在第 3 段后面"
// 的操作都要先猜"第 3 段的 Idx 到底是几"。
func Reflow(clips []Clip) []Clip {
	sorted := make([]Clip, len(clips))
	copy(sorted, clips)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Idx < sorted[j].Idx })
	return Sequence(sorted)
}

// Sequence 把**切片顺序**当成权威，重编 Idx 并重算落点（画面轨专用）。
//
// ⚠️ 已经把顺序排好的调用方（移动、插入）必须走这个，**不能走 Reflow** ——
// Reflow 会按 Idx 重新排序，而那些 Idx 还是移动**之前**的值，
// 于是刚排好的顺序被原样撤销。症状是"拖了没反应"，
// 而每一步单独看都是对的（第一次写就踩了这一个）。
//
// ⚠️ StartMs 是**成片时间**（SCK-15）：有转场时后一段的起点已经扣掉重叠，
// 与渲染的 xfade offset、EffectiveDuration 同一个实现（layout.go 的 placeVideo）。
// 只给画面轨用：音频 / 字幕 / 文字轨按起点放置（PlaceByStart）。
func Sequence(clips []Clip) []Clip {
	out := make([]Clip, len(clips))
	copy(out, clips)
	starts, _ := VideoPlacement(out)
	for i := range out {
		out[i].Idx = i + 1
		out[i].StartMs = starts[i]
		out[i].DurationMs = out[i].PlayLen()
	}
	return out
}

// TotalDuration 视频轨的总时长。
func TotalDuration(clips []Clip) int {
	total := 0
	for _, c := range clips {
		total += c.PlayLen()
	}
	return total
}

// Reorder 把某一段移到新位置，返回重排后的清单。
//
// ⚠️ 位置从 1 起、越界夹回范围：前端传个 0 或 999 过来不该报错，
// 那不是用户能理解的错误，夹回去就是他想要的效果（拖到最前/最后）。
func Reorder(clips []Clip, clipID int64, toPos int) ([]Clip, error) {
	sorted := Reflow(clips)
	from := -1
	for i, c := range sorted {
		if c.ID == clipID {
			from = i
			break
		}
	}
	if from < 0 {
		return nil, fmt.Errorf("片段不在这条轨道上")
	}
	to := toPos - 1
	if to < 0 {
		to = 0
	}
	if to > len(sorted)-1 {
		to = len(sorted) - 1
	}
	moved := sorted[from]
	rest := append(sorted[:from:from], sorted[from+1:]...)
	out := make([]Clip, 0, len(sorted))
	out = append(out, rest[:to]...)
	out = append(out, moved)
	out = append(out, rest[to:]...)
	// ⚠️ 这里必须是 Sequence 不是 Reflow：out 的顺序已经是最终顺序，
	// 而每段的 Idx 还是移动之前的值。
	return Sequence(out), nil
}

// Trim 改一段的取用区间。
//
// ⚠️ 裁切改的是**素材内的取用区间**，不是时间线上的落点 ——
// 落点由 Reflow 重算。两件事混在一起的话，裁短第 2 段之后
// 第 3 段还停在原处，成片里多出一段黑屏。
func Trim(c Clip, inMs, outMs, sourceDurationMs int) (Clip, error) {
	next := c
	next.SourceInMs = inMs
	next.SourceOutMs = outMs
	if err := ValidateClip(next, sourceDurationMs); err != nil {
		return Clip{}, err
	}
	next.DurationMs = next.PlayLen()
	return next, nil
}

// Split 在**时间线上的某个偏移**把一段切成两段。
//
// `atMs` 是相对这一段起点的偏移（不是素材内的偏移）——
// 用户是在时间线上点的那一下，让调用方自己换算等于把这条规则散出去。
//
// ⚠️ 切点要换算回素材坐标时**必须带上变速**：2 倍速时时间线上的 1 秒
// 对应素材里的 2 秒。不换算的话切出来的两段接缝处会跳帧，
// 而两段各自看都是好的。
func Split(c Clip, atMs int) (left, right Clip, err error) {
	if atMs < MinClipMs || c.PlayLen()-atMs < MinClipMs {
		return Clip{}, Clip{}, fmt.Errorf(
			"切点太靠边：两侧都至少要有 %d 毫秒", MinClipMs)
	}
	speed := c.Speed
	if speed <= 0 {
		speed = DefaultSpeed
	}
	cut := c.SourceInMs + atMs*speed/100

	left = c
	left.SourceOutMs = cut
	left.DurationMs = left.PlayLen()
	// ⚠️ 左半段不带原来的淡出、右半段不带原来的淡入：
	// 都带上的话接缝处会出现一次"淡出再淡入"，而用户只是切了一刀。
	left.FadeOutMs = 0
	// 转出（淡出到黑场）跟着右半段走：它才是原片段的结尾。
	left.TransitionOut = Transition{}

	right = c
	right.ID = 0 // 新段，由仓储分配
	right.SourceInMs = cut
	right.DurationMs = right.PlayLen()
	right.FadeInMs = 0
	// ⚠️ 刀口是硬切：右半段继承原来的转入的话，接缝处凭空多一次叠化，成片还短了一个转场时长
	// （转入只属于左半段与上一段之间的那道接缝）。与前端 smartcutTimeline 的分割同一条。
	right.TransitionIn = Transition{}
	// ⚠️ 右半段**不继承溯源**：它不再是"第 3 镜自动装配出来的那一段"，
	// 继承的话自动装配的唯一约束会撞车，而且"只补缺口"会误判成已有。
	right.SourceShotID = 0
	// 备选记的是原片段换素材之前的取用区间，进行中的生成也只回填原片段（按片段 ID）：
	// 都留在左半段（它保留原来的 ID），右半段是一段新片段。
	right.Alternates = nil
	right.PendingGenerationID = 0
	return left, right, nil
}

// InsertAfter 把新段插在某段之后，返回重排后的清单。
func InsertAfter(clips []Clip, afterID int64, fresh Clip) []Clip {
	sorted := Reflow(clips)
	out := make([]Clip, 0, len(sorted)+1)
	inserted := false
	for _, c := range sorted {
		out = append(out, c)
		if c.ID == afterID {
			out = append(out, fresh)
			inserted = true
		}
	}
	if !inserted {
		out = append(out, fresh)
	}
	// 同上：顺序已定，不能再按旧 Idx 排一次。
	return Sequence(out)
}
