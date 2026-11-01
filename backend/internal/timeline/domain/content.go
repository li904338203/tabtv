package domain

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// 整份内容（docs/agent/smartcut dev/01 §4.1）：整份保存的请求体，也是 AI 剪辑编译的产出。
//
// 规则只有这一份：前端保存前的校验（frontend/utils/smartcutContent.ts）报同一个字段路径，
// 两边由 api/fixtures/smartcut/content/ 绊住（SC-N-15）。改规则时两边与 fixture 一起改。

// ---------------------------------------------------------------- 画幅

// AspectCustom 「跟随素材」的自定义尺寸（SC-F-21）：宽高由客户端给，只校验偶数与范围。
const AspectCustom = "custom"

// AspectPreset 是一档固定画幅与它的默认尺寸。
type AspectPreset struct {
	Ratio  string `json:"aspectRatio"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
}

// AspectPresets 固定画幅与默认尺寸。前端那一份由 api/fixtures/smartcut/content/aspects.json 绊住。
var AspectPresets = []AspectPreset{
	{"16:9", 1920, 1080},
	{"9:16", 1080, 1920},
	{"1:1", 1080, 1080},
	{"4:3", 1440, 1080},
	{"3:4", 1080, 1440},
	{"21:9", 2560, 1080},
}

// 尺寸与帧率的范围。
const (
	MinFrameSide = 64
	MaxFrameSide = 7680
	MinFPS       = 1
	MaxFPS       = 120
)

// DefaultSize 固定画幅的默认尺寸。`custom` 与未知画幅回 ok=false（尺寸只能由调用方给）。
func DefaultSize(aspect string) (w, h int, ok bool) {
	for _, p := range AspectPresets {
		if p.Ratio == aspect {
			return p.Width, p.Height, true
		}
	}
	return 0, 0, false
}

// ValidateFormat 校验画幅、帧率与尺寸，错误带字段路径（`aspectRatio` / `fps` / `width` / `height`）。
//
// ⚠️ 宽高必须是偶数（yuv420p 的要求，不拦的话 ffmpeg 在渲染时才报错）。
// 固定画幅还要求宽高与画幅自洽（容差 2%，留给编码器对齐）：不一致的话界面按画幅画预览框、
// ffmpeg 按尺寸出片，用户看到的预览与拿到的成片是两个形状。`custom` 不做这一项。
func ValidateFormat(aspect string, fps, w, h int) *ValidationError {
	for _, ve := range []*ValidationError{
		validateAspect(aspect), validateFPS(fps), validateSide("width", w), validateSide("height", h),
	} {
		if ve != nil {
			return ve
		}
	}
	return validateRatio(aspect, w, h)
}

func validateAspect(aspect string) *ValidationError {
	if _, _, ok := DefaultSize(aspect); ok || aspect == AspectCustom {
		return nil
	}
	return &ValidationError{Path: "aspectRatio", Reason: "不支持的画幅 " + aspect}
}

func validateFPS(fps int) *ValidationError {
	if fps < MinFPS || fps > MaxFPS {
		return &ValidationError{Path: "fps", Reason: fmt.Sprintf("帧率只支持 %d~%d，当前 %d", MinFPS, MaxFPS, fps)}
	}
	return nil
}

func validateSide(path string, v int) *ValidationError {
	if v < MinFrameSide || v > MaxFrameSide {
		return &ValidationError{Path: path,
			Reason: fmt.Sprintf("宽高只支持 %d~%d 像素，当前 %d", MinFrameSide, MaxFrameSide, v)}
	}
	if v%2 != 0 {
		return &ValidationError{Path: path, Reason: fmt.Sprintf("宽高必须是偶数，当前 %d", v)}
	}
	return nil
}

// validateRatio 固定画幅的宽高自洽（容差 2%）；custom 不判。错误记在 height 上。
func validateRatio(aspect string, w, h int) *ValidationError {
	dw, dh, ok := DefaultSize(aspect)
	if !ok || h == 0 {
		return nil
	}
	want := float64(dw) / float64(dh)
	got := float64(w) / float64(h)
	if math.Abs(got-want)/want > 0.02 {
		return &ValidationError{Path: "height",
			Reason: fmt.Sprintf("尺寸 %d×%d 与画幅 %s 不符（应接近 %d×%d）", w, h, aspect, dw, dh)}
	}
	return nil
}

// ---------------------------------------------------------------- 内容

// ValidationError 是一处校验失败：Path 指到具体字段，前端报同一个 Path。
//
// Path 的格式是定死的（与导演台同一套）：点号分段、数组用方括号下标，例如
// `tracks[2].clips[5].transform.scale`、`subtitles[0].anchor.clipId`。整份不是对象时为 `$`。
type ValidationError struct {
	Path   string
	Reason string
}

func (e *ValidationError) Error() string { return e.Path + "：" + e.Reason }

// 内容里各种字符串的长度上限（按字符数，不按字节）。
const (
	MaxTrackNameRunes  = 64
	MaxSourceNodeIDLen = 64
	MaxLangLen         = 16
)

// Content 是整份内容。ID 都是字符串：正式 ID 是数字串，新建的对象用 `tmp-` 开头的临时 ID，
// 保存后由响应的 idMap 给出「临时 → 正式」（轨道、片段、字幕、文字共用一张表）。
//
// ⚠️ 变换、样式、位置框、锚点是原样 JSON：未知字段原样保留（GO-35），只校验认得的那几项。
type Content struct {
	Version     int               `json:"version"`
	AspectRatio string            `json:"aspectRatio"`
	FPS         int               `json:"fps"`
	Width       int               `json:"width"`
	Height      int               `json:"height"`
	Tracks      []ContentTrack    `json:"tracks"`
	Subtitles   []ContentSubtitle `json:"subtitles,omitempty"`
	Texts       []ContentText     `json:"texts,omitempty"`
}

// ContentTrack 是一条轨道。`Volume` 为 nil = 100。
type ContentTrack struct {
	ID       string          `json:"id"`
	Kind     string          `json:"kind"`
	Name     string          `json:"name,omitempty"`
	Role     string          `json:"role,omitempty"`
	Lang     string          `json:"lang,omitempty"`
	IsHidden bool            `json:"isHidden,omitempty"`
	IsMuted  bool            `json:"isMuted,omitempty"`
	Volume   *int            `json:"volume,omitempty"`
	Style    json.RawMessage `json:"style,omitempty"`
	Clips    []ContentClip   `json:"clips,omitempty"`
}

// ContentClip 是一个片段。
//
// ⚠️ 画面轨的 `StartMs` 传了也忽略（服务端按顺序与转场算）；其它轨必填，是成片时间。
// 指针为 nil 表示没传：`volume: 0` 是静音，不能拿零值当「没传」。
type ContentClip struct {
	ID            string          `json:"id"`
	MediaID       int64           `json:"mediaId,string"`
	SourceInMs    int             `json:"sourceInMs"`
	SourceOutMs   int             `json:"sourceOutMs"`
	StartMs       *int            `json:"startMs,omitempty"`
	Speed         *int            `json:"speed,omitempty"`
	Volume        *int            `json:"volume,omitempty"`
	FadeInMs      int             `json:"fadeInMs,omitempty"`
	FadeOutMs     int             `json:"fadeOutMs,omitempty"`
	TransitionIn  *Transition     `json:"transitionIn,omitempty"`
	TransitionOut *Transition     `json:"transitionOut,omitempty"`
	Transform     json.RawMessage `json:"transform,omitempty"`
	Origin        string          `json:"origin,omitempty"`
	SourceNodeID  string          `json:"sourceNodeId,omitempty"`
	SourceShotID  string          `json:"sourceShotId,omitempty"`
	Alternates    []Alternate     `json:"alternates,omitempty"`
	// Anchor 跟随画面轨的一段（只在音频轨上，SC-F-56）。形状同字幕的锚点。
	Anchor json.RawMessage `json:"anchor,omitempty"`
}

// PlayLen 这一段在成片里占多久（与 Clip.PlayLen 同一个算法：整数除法向下取整）。
func (c ContentClip) PlayLen() int {
	return Clip{SourceInMs: c.SourceInMs, SourceOutMs: c.SourceOutMs, Speed: intOr(c.Speed, DefaultSpeed)}.PlayLen()
}

// ContentSubtitle 是一条字幕。起止为成片时间。
type ContentSubtitle struct {
	ID      string          `json:"id"`
	TrackID string          `json:"trackId"`
	StartMs int             `json:"startMs"`
	EndMs   int             `json:"endMs"`
	Text    string          `json:"text"`
	Style   json.RawMessage `json:"style,omitempty"`
	Anchor  json.RawMessage `json:"anchor,omitempty"`
}

// ContentText 是一个文字层。起止为成片时间。
type ContentText struct {
	ID      string          `json:"id"`
	TrackID string          `json:"trackId"`
	StartMs int             `json:"startMs"`
	EndMs   int             `json:"endMs"`
	Text    string          `json:"text"`
	Style   json.RawMessage `json:"style,omitempty"`
	Box     json.RawMessage `json:"box,omitempty"`
}

// IsTempID 是不是新建对象的临时 ID。
func IsTempID(id string) bool { return strings.HasPrefix(id, "tmp-") }

// DecodeContent 先校验再解析。校验不过时返回 *ValidationError。
//
// ⚠️ 原样 JSON 字段里的 `null` 规范成空：encoding/json 会把 null 原样塞进 json.RawMessage，
// 不规范的话下游要在每个读取点分别判「空」与「null」。
func DecodeContent(raw json.RawMessage) (Content, error) {
	if err := ValidateContent(raw); err != nil {
		return Content{}, err
	}
	var c Content
	if err := json.Unmarshal(raw, &c); err != nil {
		// 校验已经逐字段看过类型，走到这里说明两边规则不一致 —— 照样当字段错误报出去。
		return Content{}, &ValidationError{Path: "$", Reason: "内容解析失败：" + err.Error()}
	}
	for i := range c.Tracks {
		t := &c.Tracks[i]
		t.Style = nullToEmpty(t.Style)
		for j := range t.Clips {
			t.Clips[j].Transform = nullToEmpty(t.Clips[j].Transform)
		}
	}
	for i := range c.Subtitles {
		c.Subtitles[i].Style = nullToEmpty(c.Subtitles[i].Style)
		c.Subtitles[i].Anchor = nullToEmpty(c.Subtitles[i].Anchor)
	}
	for i := range c.Texts {
		c.Texts[i].Style = nullToEmpty(c.Texts[i].Style)
		c.Texts[i].Box = nullToEmpty(c.Texts[i].Box)
	}
	return c, nil
}

func nullToEmpty(raw json.RawMessage) json.RawMessage {
	if strings.TrimSpace(string(raw)) == "null" {
		return nil
	}
	return raw
}

func intOr(p *int, def int) int {
	if p == nil {
		return def
	}
	return *p
}

// ValidateContent 校验整份内容的形状、上限、种类与轨道的对应、变换与文字样式的取值，
// 错误带字段路径（*ValidationError）。
//
// ⚠️ 按固定顺序遍历、遇到第一处错误就停：同一份坏数据每次报同一个 Path，前端也照这个顺序。
// 顺序：version → aspectRatio → fps → width → height → tracks（逐条：id → kind → name → role →
// lang → isHidden → isMuted → volume → style → clips 逐段）→ 轨道条数 → 片段总数 →
// subtitles（条数 → 逐条）→ texts（条数 → 逐条）。
//
// ⚠️ 只拦结构性损坏与上限，不顺手修复。素材种类（画面轨只收视频与图片）要查媒体表，
// 不在这里判（整份保存时由 app 层判）；同轨重叠与成片落点在 Layout（SCT-04）里判。
func ValidateContent(raw json.RawMessage) error {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return &ValidationError{Path: "$", Reason: "不是合法 JSON：" + err.Error()}
	}
	root, ok := v.(map[string]any)
	if !ok {
		return &ValidationError{Path: "$", Reason: "内容必须是 JSON 对象"}
	}
	c := contentChecker{ids: map[string]string{}, trackKind: map[string]string{}, clipIDs: map[string]bool{},
		clipKinds: map[string]string{}}
	c.root(root)
	if c.err != nil {
		return c.err
	}
	return nil
}

var (
	contentIDPattern = regexp.MustCompile(`^([0-9]{1,19}|tmp-[A-Za-z0-9_-]{1,40})$`)
	numericIDPattern = regexp.MustCompile(`^[0-9]{1,19}$`)
	langPattern      = regexp.MustCompile(`^[A-Za-z]{2,3}(-[A-Za-z0-9]{2,8}){0,3}$`)
	colorPattern     = regexp.MustCompile(`^#([0-9a-fA-F]{3}|[0-9a-fA-F]{6}|[0-9a-fA-F]{8})$`)
)

var (
	trackKinds      = map[string]bool{TrackVideo: true, TrackAudio: true, TrackSubtitle: true, TrackText: true}
	clipOrigins     = map[string]bool{OriginAI: true, OriginUser: true, OriginSync: true}
	alternateKinds  = map[string]bool{AlternateOriginal: true, AlternateRegen: true, AlternateDesubtitle: true}
	textAligns      = map[string]bool{"left": true, "center": true, "right": true}
	boxAnchors      = map[string]bool{"top": true, "center": true, "bottom": true}
	cropSides       = []string{"left", "top", "right", "bottom"}
	trackKindLimits = []struct {
		kind string
		max  int
	}{{TrackAudio, MaxAudioTracks}, {TrackSubtitle, MaxSubtitleTracks}, {TrackText, MaxTextTracks}}
	trackKindLabels = map[string]string{TrackVideo: "画面轨", TrackAudio: "音频轨", TrackSubtitle: "字幕轨", TrackText: "文字轨"}
)

type contentChecker struct {
	err *ValidationError
	// ids 已出现过的 ID → 它的路径（四类对象共用一张 idMap，ID 必须全局唯一）。
	ids map[string]string
	// trackKind 轨道 ID → 种类；clipIDs 片段 ID 集合（锚点要引用它）。
	trackKind map[string]string
	clipIDs   map[string]bool
	// clipKinds 片段 ID → 所在轨道的种类；clipAnchors 音频片段的锚点（所有轨道看完之后再查：锚点指向的画面片段
	// 可能排在后面）。
	clipKinds   map[string]string
	clipAnchors []pendingAnchor
	clipCount   int
	// textTracks 文字轨的 ID，按内容里的顺序（文字层的同轨重叠按这个顺序查）。
	textTracks []string
}

// pendingAnchor 一个待查的片段锚点。
type pendingAnchor struct {
	path string
	v    any
}

func (c *contentChecker) fail(path, format string, args ...any) {
	if c.err == nil {
		c.err = &ValidationError{Path: path, Reason: fmt.Sprintf(format, args...)}
	}
}

func (c *contentChecker) ok() bool { return c.err == nil }

// set 记下一个现成的字段错误（已有错误时不覆盖）。
func (c *contentChecker) set(ve *ValidationError) {
	if c.err == nil && ve != nil {
		c.err = ve
	}
}

func at(base, key string) string {
	if base == "" {
		return key
	}
	return base + "." + key
}

func idx(base string, i int) string { return base + "[" + strconv.Itoa(i) + "]" }

func (c *contentChecker) root(m map[string]any) {
	c.integer(m, "", "version", true, 0, math.MaxInt32)
	if !c.ok() {
		return
	}
	// 画幅 → 帧率 → 宽 → 高 → 宽高自洽，与 ValidateFormat 同一个顺序。
	aspect, isStr := m["aspectRatio"].(string)
	switch {
	case m["aspectRatio"] == nil:
		c.fail("aspectRatio", "必填")
	case !isStr:
		c.fail("aspectRatio", "必须是字符串")
	default:
		c.set(validateAspect(aspect))
	}
	fps, _ := c.integer(m, "", "fps", true, math.MinInt32, math.MaxInt32)
	c.set(validateFPSIf(c.ok(), fps))
	w, _ := c.integer(m, "", "width", true, math.MinInt32, math.MaxInt32)
	c.set(validateSideIf(c.ok(), "width", w))
	h, _ := c.integer(m, "", "height", true, math.MinInt32, math.MaxInt32)
	c.set(validateSideIf(c.ok(), "height", h))
	if !c.ok() {
		return
	}
	c.set(validateRatio(aspect, w, h))

	tracks, _ := c.array(m, "", "tracks", true)
	if !c.ok() {
		return
	}
	kinds := map[string]int{}
	for i, t := range tracks {
		c.track(idx("tracks", i), t)
		if !c.ok() {
			return
		}
		kind, _ := t.(map[string]any)["kind"].(string)
		kinds[kind]++
	}
	if kinds[TrackVideo] != 1 {
		c.fail("tracks", "必须恰好有一条画面轨，当前 %d 条", kinds[TrackVideo])
		return
	}
	for _, l := range trackKindLimits {
		if kinds[l.kind] > l.max {
			c.fail("tracks", "%s最多 %d 条，当前 %d 条", trackKindLabels[l.kind], l.max, kinds[l.kind])
			return
		}
	}
	if c.clipCount > MaxClips {
		c.fail("tracks", "片段最多 %d 个，当前 %d 个", MaxClips, c.clipCount)
		return
	}
	// 音频片段的锚点（SC-F-56）：形状同字幕；指向这份内容里的片段时，那一段必须在画面轨上。
	for _, pa := range c.clipAnchors {
		c.anchor(pa.path, pa.v)
		if !c.ok() {
			return
		}
		target, _ := pa.v.(map[string]any)["clipId"].(string)
		if kind, found := c.clipKinds[target]; found && kind != TrackVideo {
			c.fail(at(pa.path, "clipId"), "只能跟随画面轨上的片段")
			return
		}
	}

	subs, _ := c.array(m, "", "subtitles", false)
	if !c.ok() {
		return
	}
	if len(subs) > MaxSubtitles {
		c.fail("subtitles", "字幕最多 %d 条，当前 %d 条", MaxSubtitles, len(subs))
		return
	}
	// 同一条字幕轨上起止完全相同的两条（重叠允许，完全相同多半是重复提交）。
	seenRange := map[string]string{}
	for i, s := range subs {
		p := idx("subtitles", i)
		c.timedItem(p, s, TrackSubtitle)
		if !c.ok() {
			return
		}
		sm := s.(map[string]any)
		c.textStyle(at(p, "style"), sm["style"], true)
		c.anchor(at(p, "anchor"), sm["anchor"])
		if !c.ok() {
			return
		}
		key := fmt.Sprintf("%v|%v|%v", sm["trackId"], sm["startMs"], sm["endMs"])
		if prev, dup := seenRange[key]; dup {
			c.fail(at(p, "startMs"), "与 %s 的起止完全相同", prev)
			return
		}
		seenRange[key] = p
	}

	texts, _ := c.array(m, "", "texts", false)
	if !c.ok() {
		return
	}
	if len(texts) > MaxTextLayers {
		c.fail("texts", "文字层最多 %d 个，当前 %d 个", MaxTextLayers, len(texts))
		return
	}
	byTrack := map[string][]span{}
	for i, t := range texts {
		p := idx("texts", i)
		c.timedItem(p, t, TrackText)
		if !c.ok() {
			return
		}
		tm := t.(map[string]any)
		c.textStyle(at(p, "style"), tm["style"], false)
		c.box(at(p, "box"), tm["box"])
		if !c.ok() {
			return
		}
		st, _ := tm["startMs"].(float64)
		en, _ := tm["endMs"].(float64)
		tid := tm["trackId"].(string)
		byTrack[tid] = append(byTrack[tid], span{start: int(st), end: int(en), index: i})
	}
	// 文字轨同轨不许重叠（SCD-15），按文字轨在内容里的顺序查；字幕允许重叠（两个人同时说话）。
	for _, tid := range c.textTracks {
		if k := firstOverlap(byTrack[tid]); k >= 0 {
			c.fail(at(idx("texts", k), "startMs"), "与同一条文字轨上的另一个文字层重叠")
			return
		}
	}
}

func (c *contentChecker) track(p string, v any) {
	m, ok := v.(map[string]any)
	if !ok {
		c.fail(p, "轨道必须是对象")
		return
	}
	c.id(p, m)
	kind, _ := m["kind"].(string)
	if c.ok() && !trackKinds[kind] {
		c.fail(at(p, "kind"), "不支持的轨道种类 %q", kind)
	}
	if !c.ok() {
		return
	}
	c.trackKind[m["id"].(string)] = kind
	if kind == TrackText {
		c.textTracks = append(c.textTracks, m["id"].(string))
	}
	if name, present := m["name"]; present && name != nil {
		s, isStr := name.(string)
		if !isStr {
			c.fail(at(p, "name"), "必须是字符串")
		} else if utf8.RuneCountInString(s) > MaxTrackNameRunes {
			c.fail(at(p, "name"), "名称最多 %d 个字", MaxTrackNameRunes)
		}
	}
	if rp, s, ok := c.optString(m, p, "role"); ok && s != "" {
		valid := false
		switch kind {
		case TrackAudio:
			valid = s == RoleMusic || s == RoleVoice || s == RoleSfx
		case TrackVideo:
			// 画面轨只能标人声：口播素材的人声在画面原声里。
			valid = s == RoleVoice
		}
		if !valid {
			c.fail(rp, "%s不能标用途 %q", trackKindLabels[kind], s)
		}
	}
	if lp, s, ok := c.optString(m, p, "lang"); ok && s != "" {
		switch {
		case kind != TrackSubtitle:
			c.fail(lp, "只有字幕轨有语言")
		case len(s) > MaxLangLen || !langPattern.MatchString(s):
			c.fail(lp, "语言代码格式不对：%q", s)
		}
	}
	c.optBool(m, p, "isHidden")
	c.optBool(m, p, "isMuted")
	c.integer(m, p, "volume", false, 0, 200)
	if !c.ok() {
		return
	}
	if style, present := m["style"]; present && style != nil {
		if _, isObj := style.(map[string]any); !isObj {
			c.fail(at(p, "style"), "必须是对象")
			return
		}
		switch kind {
		case TrackSubtitle:
			c.textStyle(at(p, "style"), style, true)
		case TrackAudio:
			c.duck(at(at(p, "style"), "duck"), style.(map[string]any)["duck"])
		}
		if !c.ok() {
			return
		}
	}
	clips, _ := c.array(m, p, "clips", false)
	if !c.ok() {
		return
	}
	if len(clips) > 0 && (kind == TrackSubtitle || kind == TrackText) {
		c.fail(at(p, "clips"), "%s不放素材", trackKindLabels[kind])
		return
	}
	prevLen := 0
	for j, cv := range clips {
		cp := idx(at(p, "clips"), j)
		playLen := c.clip(cp, cv, kind, prevLen, j == len(clips)-1)
		if !c.ok() {
			return
		}
		prevLen = playLen
	}
	c.clipCount += len(clips)
	// 音频轨按起点放置，同轨不许重叠（SCD-15）：报在按起点排序后重叠的后一段上。
	// ⚠️ 跟随片段的不参与（SC-F-56）：它的落点跟着画面走，用户裁一下画面就可能挨到别的音效 —— 拦下来的话，
	// 用户只是裁了画面，整份保存却报「音频重叠」。渲染逐段混音，重叠不影响出片。
	if kind == TrackAudio {
		items := make([]span, 0, len(clips))
		for j, cv := range clips {
			cm := cv.(map[string]any)
			if a, has := cm["anchor"]; has && a != nil {
				continue
			}
			st, _ := cm["startMs"].(float64)
			items = append(items, span{start: int(st), end: int(st) + clipPlayLen(cm), index: j})
		}
		if j := firstOverlap(items); j >= 0 {
			c.fail(at(idx(at(p, "clips"), j), "startMs"), "与同一条音频轨上的另一段重叠")
			return
		}
	}
	if kind == TrackVideo {
		total := 0
		for j, cv := range clips {
			cm := cv.(map[string]any)
			total += clipPlayLen(cm)
			if j > 0 {
				if t := transitionOf(cm["transitionIn"]); t.IsActive() {
					total -= t.DurationMs
				}
			}
		}
		if total > MaxTimelineMs {
			c.fail(at(p, "clips"), "成片最长 %d 分钟，当前 %.1f 分钟", MaxTimelineMs/60000, float64(total)/60000)
		}
	}
}

// clip 校验一个片段，返回它在成片里的长度（给下一段的转场判断用）。
func (c *contentChecker) clip(p string, v any, kind string, prevLen int, last bool) int {
	m, ok := v.(map[string]any)
	if !ok {
		c.fail(p, "片段必须是对象")
		return 0
	}
	c.id(p, m)
	if !c.ok() {
		return 0
	}
	c.clipIDs[m["id"].(string)] = true
	c.clipKinds[m["id"].(string)] = kind
	if av, has := m["anchor"]; has && av != nil {
		if kind != TrackAudio {
			c.fail(at(p, "anchor"), "只有音频轨上的片段可以跟随画面轨的一段")
			return 0
		}
		c.clipAnchors = append(c.clipAnchors, pendingAnchor{path: at(p, "anchor"), v: av})
	}
	c.numericID(m, p, "mediaId", true)
	in, _ := c.integer(m, p, "sourceInMs", true, 0, math.MaxInt32)
	out, _ := c.integer(m, p, "sourceOutMs", true, 0, math.MaxInt32)
	if c.ok() && out <= in {
		c.fail(at(p, "sourceOutMs"), "取用终点必须晚于起点")
	}
	// 画面轨的 startMs 传了也忽略：落点由服务端按顺序与转场算。
	start := 0
	if kind != TrackVideo {
		start, _ = c.integer(m, p, "startMs", true, 0, math.MaxInt32)
	}
	c.integer(m, p, "speed", false, 25, 400)
	c.integer(m, p, "volume", false, 0, 200)
	if !c.ok() {
		return 0
	}
	playLen := clipPlayLen(m)
	if playLen < MinClipMs {
		c.fail(at(p, "sourceOutMs"), "片段太短（成片里不足 %d 毫秒），渲染时会被丢掉", MinClipMs)
		return 0
	}
	fadeIn, _ := c.integer(m, p, "fadeInMs", false, 0, math.MaxInt32)
	fadeOut, _ := c.integer(m, p, "fadeOutMs", false, 0, math.MaxInt32)
	if c.ok() && fadeIn+fadeOut > playLen {
		c.fail(at(p, "fadeOutMs"), "淡入淡出合计 %d 毫秒，超过片段本身的 %d 毫秒", fadeIn+fadeOut, playLen)
	}
	if !c.ok() {
		return 0
	}
	// 转场：画面轨的转入要同时照顾上一段（第一段的转入是从黑场淡入，没有上一段可吃）；
	// 转出只在最后一段有意义，中间段的不报错（复制来的片段可能带着它）。
	prev := 0
	if kind == TrackVideo {
		prev = prevLen
	}
	c.transition(at(p, "transitionIn"), m["transitionIn"], prev, playLen)
	if kind == TrackVideo && !last {
		c.transition(at(p, "transitionOut"), m["transitionOut"], -1, playLen)
	} else {
		c.transition(at(p, "transitionOut"), m["transitionOut"], 0, playLen)
	}
	c.transform(at(p, "transform"), m["transform"])
	if origin, s, ok := c.optString(m, p, "origin"); ok && s != "" && !clipOrigins[s] {
		c.fail(origin, "不支持的来源 %q", s)
	}
	if node, s, ok := c.optString(m, p, "sourceNodeId"); ok && len(s) > MaxSourceNodeIDLen {
		c.fail(node, "节点 ID 最长 %d 个字符", MaxSourceNodeIDLen)
	}
	c.numericID(m, p, "sourceShotId", false)
	c.alternates(at(p, "alternates"), m["alternates"])
	if c.ok() && kind != TrackVideo && start+playLen > MaxTimelineMs {
		c.fail(at(p, "startMs"), "超出成片最长 %d 分钟", MaxTimelineMs/60000)
	}
	return playLen
}

// clipPlayLen 与 Clip.PlayLen 同一个算法（字段已校验过类型）。
func clipPlayLen(m map[string]any) int {
	speed := DefaultSpeed
	if s, ok := m["speed"].(float64); ok {
		speed = int(s)
	}
	in, _ := m["sourceInMs"].(float64)
	out, _ := m["sourceOutMs"].(float64)
	return Clip{SourceInMs: int(in), SourceOutMs: int(out), Speed: speed}.PlayLen()
}

func transitionOf(v any) Transition {
	m, ok := v.(map[string]any)
	if !ok {
		return Transition{}
	}
	kind, _ := m["kind"].(string)
	d, _ := m["durationMs"].(float64)
	return Transition{Kind: kind, DurationMs: int(d)}
}

// transition 校验一次转场。prevLen：-1 = 只校验取值（中间段的转出会被忽略）；
// 0 = 没有上一段；>0 = 上一段的成片长度。
func (c *contentChecker) transition(p string, v any, prevLen, selfLen int) {
	if !c.ok() || v == nil {
		return
	}
	m, ok := v.(map[string]any)
	if !ok {
		c.fail(p, "必须是对象")
		return
	}
	kind, isStr := m["kind"].(string)
	if !isStr || !transitionKinds[kind] {
		c.fail(at(p, "kind"), "不支持的转场类型 %v", m["kind"])
		return
	}
	d, okD := c.integer(m, p, "durationMs", false, 0, math.MaxInt32)
	if !c.ok() || !okD {
		return
	}
	t := Transition{Kind: kind, DurationMs: d}
	if !t.IsActive() {
		return
	}
	if d < MinTransitionMs || d > MaxTransitionMs {
		c.fail(at(p, "durationMs"), "转场时长只支持 %d~%d 毫秒，当前 %d", MinTransitionMs, MaxTransitionMs, d)
		return
	}
	if prevLen < 0 {
		return
	}
	if err := ValidateTransition(t, prevLen, selfLen); err != nil {
		c.fail(at(p, "durationMs"), "%s", err.Error())
	}
}

// transform 画面变换（dev/02 §2）。未知字段原样保留，只校验认得的。
func (c *contentChecker) transform(p string, v any) {
	if !c.ok() || v == nil {
		return
	}
	m, ok := v.(map[string]any)
	if !ok {
		c.fail(p, "必须是对象")
		return
	}
	c.number(m, p, "x", -100, 100)
	c.number(m, p, "y", -100, 100)
	c.number(m, p, "scale", 0.1, 5)
	c.number(m, p, "rotation", -180, 180)
	if !c.ok() {
		return
	}
	if crop, present := m["crop"]; present && crop != nil {
		cm, isObj := crop.(map[string]any)
		if !isObj {
			c.fail(at(p, "crop"), "必须是对象")
			return
		}
		for _, side := range cropSides {
			c.number(cm, at(p, "crop"), side, 0, 45)
		}
	}
}

func (c *contentChecker) alternates(p string, v any) {
	if !c.ok() || v == nil {
		return
	}
	list, ok := v.([]any)
	if !ok {
		c.fail(p, "必须是数组")
		return
	}
	if len(list) > MaxAlternates {
		c.fail(p, "备选最多 %d 个，当前 %d 个", MaxAlternates, len(list))
		return
	}
	for i, a := range list {
		ap := idx(p, i)
		m, isObj := a.(map[string]any)
		if !isObj {
			c.fail(ap, "必须是对象")
			return
		}
		c.numericID(m, ap, "mediaId", true)
		in, _ := c.integer(m, ap, "sourceInMs", true, 0, math.MaxInt32)
		out, _ := c.integer(m, ap, "sourceOutMs", true, 0, math.MaxInt32)
		if c.ok() && out <= in {
			c.fail(at(ap, "sourceOutMs"), "取用终点必须晚于起点")
		}
		if kind, isStr := m["kind"].(string); c.ok() && (!isStr || !alternateKinds[kind]) {
			c.fail(at(ap, "kind"), "不支持的备选来历 %v", m["kind"])
		}
		c.numericID(m, ap, "generationId", false)
		if !c.ok() {
			return
		}
	}
}

// timedItem 字幕与文字层共有的部分：id、所属轨道、起止、文字。
func (c *contentChecker) timedItem(p string, v any, wantKind string) {
	m, ok := v.(map[string]any)
	if !ok {
		c.fail(p, "必须是对象")
		return
	}
	c.id(p, m)
	if !c.ok() {
		return
	}
	trackID, isStr := m["trackId"].(string)
	switch {
	case !isStr || trackID == "":
		c.fail(at(p, "trackId"), "缺少所属轨道")
	case c.trackKind[trackID] == "":
		c.fail(at(p, "trackId"), "轨道 %s 不在这份内容里", trackID)
	case c.trackKind[trackID] != wantKind:
		c.fail(at(p, "trackId"), "轨道 %s 不是%s", trackID, trackKindLabels[wantKind])
	}
	start, _ := c.integer(m, p, "startMs", true, 0, math.MaxInt32)
	end, _ := c.integer(m, p, "endMs", true, 0, math.MaxInt32)
	if !c.ok() {
		return
	}
	if end <= start {
		c.fail(at(p, "endMs"), "结束必须晚于开始")
		return
	}
	if end > MaxTimelineMs {
		c.fail(at(p, "endMs"), "超出成片最长 %d 分钟", MaxTimelineMs/60000)
		return
	}
	text, isStr := m["text"].(string)
	switch {
	case !isStr || strings.TrimSpace(text) == "":
		c.fail(at(p, "text"), "文字不能为空")
	case utf8.RuneCountInString(text) > MaxTextChars:
		c.fail(at(p, "text"), "文字最多 %d 个字", MaxTextChars)
	}
}

// textStyle 文字与字幕的样式（dev/02 §2）。`bottomPct` 只对字幕有意义，文字层上出现了按未知字段留着。
func (c *contentChecker) textStyle(p string, v any, subtitle bool) {
	if !c.ok() || v == nil {
		return
	}
	m, ok := v.(map[string]any)
	if !ok {
		c.fail(p, "必须是对象")
		return
	}
	c.number(m, p, "fontSizePct", 2, 20)
	c.color(m, p, "color")
	if stroke, present := m["stroke"]; c.ok() && present && stroke != nil {
		sm, isObj := stroke.(map[string]any)
		if !isObj {
			c.fail(at(p, "stroke"), "必须是对象")
			return
		}
		c.color(sm, at(p, "stroke"), "color")
		c.number(sm, at(p, "stroke"), "widthPct", 0, 5)
	}
	if bg, present := m["background"]; c.ok() && present && bg != nil {
		bm, isObj := bg.(map[string]any)
		if !isObj {
			c.fail(at(p, "background"), "必须是对象")
			return
		}
		c.color(bm, at(p, "background"), "color")
		c.number(bm, at(p, "background"), "opacity", 0, 1)
		c.number(bm, at(p, "background"), "paddingPct", 0, 10)
	}
	if ap, s, ok := c.optString(m, p, "align"); ok && !textAligns[s] {
		c.fail(ap, "不支持的对齐方式 %q", s)
	}
	c.number(m, p, "maxWidthPct", 10, 100)
	if subtitle {
		c.number(m, p, "bottomPct", 0, 50)
	}
}

// box 文字层的位置。
func (c *contentChecker) box(p string, v any) {
	if !c.ok() || v == nil {
		return
	}
	m, ok := v.(map[string]any)
	if !ok {
		c.fail(p, "必须是对象")
		return
	}
	c.number(m, p, "xPct", 0, 100)
	c.number(m, p, "yPct", 0, 100)
	if ap, s, ok := c.optString(m, p, "anchor"); ok && !boxAnchors[s] {
		c.fail(ap, "不支持的锚点 %q", s)
	}
}

// anchor 跟随片段的锚点。
//
// 临时 ID 必须指向这份内容里的片段（保存时要按 idMap 改写，指向不存在的临时 ID 是客户端的错）；
// 正式 ID 可以指向已经不在的片段：片段被删时锚点失效，字幕按最后一次的成片时间留在原处（dev/02 §2）——
// 这是存得进库里的正常状态，拦下来的话删过片段的时间线就再也保存不了。
func (c *contentChecker) anchor(p string, v any) {
	if !c.ok() || v == nil {
		return
	}
	m, ok := v.(map[string]any)
	if !ok {
		c.fail(p, "必须是对象")
		return
	}
	clipID, isStr := m["clipId"].(string)
	switch {
	case !isStr || !contentIDPattern.MatchString(clipID):
		c.fail(at(p, "clipId"), "跟随的片段 ID 格式不对：%v", m["clipId"])
		return
	case IsTempID(clipID) && !c.clipIDs[clipID]:
		c.fail(at(p, "clipId"), "跟随的片段 %s 不在这份内容里", clipID)
		return
	}
	start, _ := c.integer(m, p, "srcStartMs", true, 0, math.MaxInt32)
	end, _ := c.integer(m, p, "srcEndMs", true, 0, math.MaxInt32)
	if c.ok() && end <= start {
		c.fail(at(p, "srcEndMs"), "结束必须晚于开始")
	}
}

// duck 配乐轨的人声闪避开关（SC-PARAM-28）。
func (c *contentChecker) duck(p string, v any) {
	if !c.ok() || v == nil {
		return
	}
	m, ok := v.(map[string]any)
	if !ok {
		c.fail(p, "必须是对象")
		return
	}
	c.optBool(m, p, "enabled")
	c.number(m, p, "depthDb", 0, 40)
}

// ---------------------------------------------------------------- 取值助手

// id 校验对象的 id：格式对、且在整份内容里唯一（四类对象共用 idMap）。
func (c *contentChecker) id(p string, m map[string]any) {
	if !c.ok() {
		return
	}
	s, isStr := m["id"].(string)
	if !isStr || !contentIDPattern.MatchString(s) {
		c.fail(at(p, "id"), "ID 必须是数字串或 tmp- 开头的临时 ID")
		return
	}
	if numericIDPattern.MatchString(s) {
		if n, err := strconv.ParseInt(s, 10, 64); err != nil || n <= 0 {
			c.fail(at(p, "id"), "ID 超出范围")
			return
		}
	}
	if prev, dup := c.ids[s]; dup {
		c.fail(at(p, "id"), "与 %s 的 ID 重复", prev)
		return
	}
	c.ids[s] = p
}

func (c *contentChecker) numericID(m map[string]any, p, key string, required bool) {
	if !c.ok() {
		return
	}
	v, present := m[key]
	if !present || v == nil {
		if required {
			c.fail(at(p, key), "必填")
		}
		return
	}
	s, isStr := v.(string)
	if !isStr || !numericIDPattern.MatchString(s) {
		c.fail(at(p, key), "必须是数字串")
		return
	}
	if n, err := strconv.ParseInt(s, 10, 64); err != nil || n <= 0 {
		c.fail(at(p, key), "超出范围")
	}
}

// integer 取一个整数字段。缺省时 required 报错、否则放行（返回 ok=false）。
func (c *contentChecker) integer(m map[string]any, p, key string, required bool, lo, hi int) (int, bool) {
	if !c.ok() {
		return 0, false
	}
	v, present := m[key]
	if !present || v == nil {
		if required {
			c.fail(at(p, key), "必填")
		}
		return 0, false
	}
	f, isNum := v.(float64)
	if !isNum || f != math.Trunc(f) || math.IsInf(f, 0) {
		c.fail(at(p, key), "必须是整数")
		return 0, false
	}
	if f < float64(lo) || f > float64(hi) {
		c.fail(at(p, key), "取值 %v 超出范围 %d~%d", f, lo, hi)
		return 0, false
	}
	return int(f), true
}

// number 取一个可选的数字字段并校验范围（缺省放行，前端补默认值）。
func (c *contentChecker) number(m map[string]any, p, key string, lo, hi float64) {
	if !c.ok() {
		return
	}
	v, present := m[key]
	if !present || v == nil {
		return
	}
	f, isNum := v.(float64)
	if !isNum {
		c.fail(at(p, key), "必须是数字")
		return
	}
	if f < lo || f > hi {
		c.fail(at(p, key), "取值 %v 超出范围 %v~%v", f, lo, hi)
	}
}

func (c *contentChecker) color(m map[string]any, p, key string) {
	if !c.ok() {
		return
	}
	v, present := m[key]
	if !present || v == nil {
		return
	}
	s, isStr := v.(string)
	if !isStr || !colorPattern.MatchString(s) {
		c.fail(at(p, key), "颜色只支持 #RGB / #RRGGBB / #RRGGBBAA")
	}
}

func (c *contentChecker) optBool(m map[string]any, p, key string) {
	if !c.ok() {
		return
	}
	if v, present := m[key]; present && v != nil {
		if _, isBool := v.(bool); !isBool {
			c.fail(at(p, key), "必须是布尔值")
		}
	}
}

// optString 取一个可选的字符串字段：返回它的路径、取值，以及「出现了且是字符串」。
func (c *contentChecker) optString(m map[string]any, p, key string) (string, string, bool) {
	if !c.ok() {
		return "", "", false
	}
	v, present := m[key]
	if !present || v == nil {
		return "", "", false
	}
	s, isStr := v.(string)
	if !isStr {
		c.fail(at(p, key), "必须是字符串")
		return "", "", false
	}
	return at(p, key), s, true
}

// array 取一个数组字段：缺省或 null 时 required 报错、否则视为空。
func (c *contentChecker) array(m map[string]any, p, key string, required bool) ([]any, bool) {
	if !c.ok() {
		return nil, false
	}
	v, present := m[key]
	if !present || v == nil {
		if required {
			c.fail(at(p, key), "必填")
		}
		return nil, false
	}
	list, isArr := v.([]any)
	if !isArr {
		c.fail(at(p, key), "必须是数组")
		return nil, false
	}
	return list, true
}

// validateFPSIf / validateSideIf：前一步已经出错时不再判（取到的是零值）。
func validateFPSIf(ok bool, fps int) *ValidationError {
	if !ok {
		return nil
	}
	return validateFPS(fps)
}

func validateSideIf(ok bool, path string, v int) *ValidationError {
	if !ok {
		return nil
	}
	return validateSide(path, v)
}
