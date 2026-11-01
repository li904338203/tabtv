package domain

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	tl "github.com/li904338203/tabtv/backend/internal/timeline/domain"
)

// 编译：AI 方案 → 时间线内容（docs/agent/smartcut SCD-26、dev/01 §4.3）。
//
// ⚠️ 纯函数：不碰库、不发网络、不取当前时间、不用随机数，也不遍历无序的 map ——
// 同样的输入逐字节得到同样的输出（api/fixtures/smartcut/compile/ 才复现得了）。
// ⚠️ 时间全部由这里算（SCK-14）：模型写的时间字段在解析时就丢掉了。画面轨不写 startMs
// （服务端按 Layout 排），其它轨写成片时间（SCR-1）。
//
// 编译只管「AI 管的那部分」（merge）：画面轨的全部片段；选了配乐时第一条配乐轨；解说的配音轨；
// 生成字幕时第一条字幕轨；有标题时第一条文字轨开头那一段。其余（用户自己加的音效、别的字幕轨、
// 中间的贴字）原样保留 —— 写入前存了快照，「撤销这轮 AI」能整份回去。

// 剪辑方式。edit / translate 只用于解析（追加指令、翻译）。
const (
	ModeMix       = "mix"
	ModeBoard     = "board"
	ModeTalk      = "talk"
	ModeNarr      = "narr"
	ModeEdit      = "edit"
	ModeTranslate = "translate"
)

// 片段在方案里的作用。
const (
	SegHook   = "hook"
	SegBody   = "body"
	SegClimax = "climax"
	SegEnd    = "end"
)

// 取段方式。
const (
	TakeFull   = "full"
	TakeHead   = "head"
	TakeMiddle = "middle"
	TakeTail   = "tail"
	TakeAround = "around"
)

// 方案里的转场写法（cut = 硬切，落到时间线上是没有转场）。
const TransCut = "cut"

// 剪辑参数（SC-PARAM-26，设计建议值：调整只改这里，不改需求）。
const (
	// ImageDefaultMs 图片默认 3 秒；允许放慢时最长按 0.75× 折算成 4 秒（ImageSlowMs）。
	ImageDefaultMs = 3000
	ImageSlowMs    = ImageDefaultMs * 100 / MinSlowSpeed
	// TakePartPermille head / middle / tail / around 默认取源时长的 40%，不少于 TakePartMinMs、不超过源时长。
	TakePartPermille = 400
	TakePartMinMs    = 1500
	// MinSlowSpeed 放慢下限 0.75×（speed 按百分比）。
	MinSlowSpeed = 75
	// DefaultTransitionMs 转场默认时长；实际不超过相邻两段较短者的一半，不到 tl.MinTransitionMs 就改成硬切。
	DefaultTransitionMs = 500
	// 标题：默认 2 秒，1~5 秒，不超过成片。
	TitleDefaultMs = 2000
	TitleMinMs     = 1000
	TitleMaxMs     = 5000
	// MusicFadeOutMs 配乐在结尾淡出（不超过配乐本身的四分之一）。
	MusicFadeOutMs = 1500
	// DefaultSubtitleLang 新建字幕轨的语言。
	DefaultSubtitleLang = "zh-CN"
)

// 角色系数（dev/01 §4.3：钩子 1.2、结尾 1.1）。按千分比存，分配全走整数。
var segFactorPermille = map[string]int{SegHook: 1200, SegEnd: 1100, SegBody: 1000, SegClimax: 1000}

// Card 是一条可以编译的素材（素材卡片）：编号、种类、媒体与时长，以及给模型看的描述。
//
// 图片的 DurationMs 为 0（没有时长）；视频与音频的时长必须已知 —— 时长未知没法分配，
// CardsFromInputs 不收它们并说明。
type Card struct {
	No           string `json:"no"`
	Kind         string `json:"kind"`
	MediaID      int64  `json:"mediaId,string,omitempty"`
	DurationMs   int    `json:"durationMs,omitempty"`
	HasAudio     bool   `json:"hasAudio,omitempty"`
	AudioRole    string `json:"audioRole,omitempty"`
	SourceNodeID string `json:"sourceNodeId,omitempty"`
	Title        string `json:"title,omitempty"`
	Description  string `json:"description,omitempty"`
	Text         string `json:"text,omitempty"`
}

// IsPicture 能放上画面轨（视频或图片）。
func (c Card) IsPicture() bool { return c.Kind == InputVideo || c.Kind == InputImage }

// CardsFromInputs 素材条 → 卡片。只收就绪的：视频与音频要有媒体与时长，图片要有媒体，文本要有文字；
// 镜头组（脚本 / 视频组）不在这里展开（分镜成片另走装配）。descriptions 按编号给出描述
// （生成提示词、分镜描述、缓存的看图描述）。skipped 是没收进来的就绪素材的说明。
func CardsFromInputs(items []Input, descriptions map[string]string) (cards []Card, skipped []string) {
	for _, it := range items {
		if it.Status != InputReady {
			continue
		}
		c := Card{No: it.No, Kind: it.Kind, SourceNodeID: it.SourceNodeID, Title: it.Title,
			Description: strings.TrimSpace(descriptions[it.No]), AudioRole: it.AudioRole}
		if it.HasAudio != nil {
			c.HasAudio = *it.HasAudio
		}
		switch it.Kind {
		case InputText:
			c.Text = it.Text
			if strings.TrimSpace(c.Text) == "" {
				continue
			}
		case InputVideo, InputAudio, InputImage:
			id, err := strconv.ParseInt(it.MediaID, 10, 64)
			if err != nil || id <= 0 {
				skipped = append(skipped, "编号 "+it.No+" 还没有可用的媒体，没有用上")
				continue
			}
			c.MediaID = id
			if it.Kind != InputImage {
				if it.DurationMs == nil || *it.DurationMs < tl.MinClipMs {
					skipped = append(skipped, "编号 "+it.No+" 的时长未知，没有用上")
					continue
				}
				c.DurationMs = *it.DurationMs
			}
			if it.Kind == InputAudio && c.AudioRole == "" {
				c.AudioRole = RoleMusic
			}
		default:
			continue
		}
		cards = append(cards, c)
	}
	return cards, skipped
}

// Spec 是这一次剪辑的规格与开关。
type Spec struct {
	// TargetMs 目标时长（成片时间），0 = 自动（用满各段的可用长度）。
	TargetMs int `json:"targetMs"`
	// AllowSlow / AllowRepeat 素材不够时的两个开关（SC-F-92）：默认如实报告，不放慢、不重复。
	AllowSlow   bool `json:"allowSlow,omitempty"`
	AllowRepeat bool `json:"allowRepeat,omitempty"`
	// WithSubtitles 生成字幕（混剪按段落、分镜按台词、口播按识别、解说按原稿）。缺省由应用层按模式定。
	WithSubtitles bool `json:"withSubtitles,omitempty"`
	// IgnoreBeat 有配乐也不卡点（用户选了「不卡点」）。
	IgnoreBeat bool `json:"ignoreBeat,omitempty"`
	// KeepFillers 口播精剪保留口癖（默认去掉）。
	KeepFillers bool `json:"keepFillers,omitempty"`
	// SubtitleLang 新建字幕轨的语言（空 = zh-CN）。
	SubtitleLang string `json:"subtitleLang,omitempty"`
	// TalkSpans 口播精剪只用素材的哪几段（成片顺序；SCT-34）。空 = 每份口播素材整段、按卡片顺序。
	// ⚠️ 在发起时按时间线定下来并跟着规格存进调用记录：切换方案、强制应用时照这份重新编号 ——
	// 按当时的时间线现算的话，那时轨道上已经是 AI 剪出来的段，句子编号整体错位，选的句子就成了别的句子。
	TalkSpans []TalkSpan `json:"talkSpans,omitempty"`
}

// TalkSpan 口播素材里要用的一段（素材时间，左闭右开）。
type TalkSpan struct {
	MediaID int64 `json:"mediaId,string"`
	InMs    int   `json:"inMs"`
	OutMs   int   `json:"outMs"`
}

// Beats 是一条配乐的节拍（media_analysis kind=beats 的 beatsMs / downbeatsMs，配乐自己的时间）。
type Beats struct {
	BeatsMs     []int `json:"beatsMs"`
	DownbeatsMs []int `json:"downbeatsMs,omitempty"`
}

// PlanInput 是编译的全部输入（应用层准备好的事实）。
type PlanInput struct {
	Mode string
	// Base 当前的时间线内容（版本、画幅与全部轨道）：编译在它上面替换 AI 管的那部分。
	Base  tl.Content
	Cards []Card
	Spec  Spec
	// Beats 配乐的节拍，按素材编号。
	Beats map[string]Beats
	// Shots 分镜成片：装配好的镜头（顺序与取用区间以装配为准，模型不改）。
	Shots []BoardShot
	// Transcripts 识别结果，按素材编号（口播的视频、解说的配音）。
	Transcripts map[string]Transcript
	// Peaks 波形，按素材编号（口播的能量校正）。
	Peaks map[string]Peaks
	// Script 解说稿：连进来的文本素材（没有时用 VoiceScripts，再没有用识别文字）。
	Script string
	// VoiceScripts 每条配音那次文字转语音的原稿（从生成快照还原），按素材编号；不是文字转语音来的不在里面。
	VoiceScripts map[string]string
}

// ClipReason 一段为什么这样用（模型给的理由，原样转述）。
type ClipReason struct {
	Clip   string `json:"clip"`
	Reason string `json:"reason"`
}

// Result 是一次编译的结果。Content 与整份保存同形（新对象带 tmp- 临时 ID），可以直接写入。
type Result struct {
	Content tl.Content `json:"content"`
	// TotalMs 成片总长（扣掉转场重叠）。
	TotalMs   int          `json:"totalMs"`
	UsedClips []string     `json:"usedClips"`
	Reasons   []ClipReason `json:"reasons,omitempty"`
	// Warnings 要让用户知道的：素材不够、没用上的素材、卡点偏差、被跳过的段与原因。
	Warnings []string `json:"warnings"`
	// Notes 模型的说明与编译的附注（不是问题）。
	Notes []string `json:"notes,omitempty"`
	// Changes 追加指令改了哪几处（人话）。
	Changes      []string `json:"changes,omitempty"`
	UsedFallback bool     `json:"usedFallback,omitempty"`
	// Shortage 素材不够目标时长（前端据此给出路，不匹配警告文案）。解说成片里 TargetMs 是配音时长。
	Shortage *Shortage `json:"shortage,omitempty"`
	Summary  string    `json:"summary"`
}

// Shortage 成片实际多长、目标多长。
type Shortage struct {
	TotalMs  int `json:"totalMs"`
	TargetMs int `json:"targetMs"`
}

// FallbackNote 兜底方案的固定说明（SC-F-85、SC-AC-32）。
const FallbackNote = "AI 没能给出方案，已按规则剪好"

// ErrUnsuitable 输入不适合这个剪辑方式（应用层映射为 42217，Reason 进 details.reason）。
type ErrUnsuitable struct {
	Reason string
	Detail string
}

func (e *ErrUnsuitable) Error() string { return e.Detail }

// 42217 的原因（details.reason）。
const (
	UnsuitableNoPicture  = "no_picture"
	UnsuitableNoMusic    = "no_music"
	UnsuitableNoVoice    = "no_voice"
	UnsuitableASRMissing = "asr_missing"
	UnsuitableNoShots    = "no_shots"
)

// Compile 按方案编译。方案应当先过 ParseDraft（编号、枚举、比例都已校验）。
func Compile(in PlanInput, d Draft) (Result, error) {
	switch in.Mode {
	case ModeMix:
		return compileMix(in, d, false)
	case ModeBoard:
		return compileBoard(in, d, false)
	case ModeTalk:
		return compileTalk(in, d, false)
	case ModeNarr:
		return compileNarr(in, d, false)
	}
	return Result{}, fmt.Errorf("不支持的剪辑方式 %q", in.Mode)
}

// ---------------------------------------------------------------- 画面轨的一段

// cut 是画面轨上的一段（编译的中间态）：素材、取用区间、速度与转入。
type cut struct {
	card  Card
	inMs  int
	outMs int
	speed int
	trans tl.Transition
	// subtitle 这一段的字幕（混剪按段落、分镜按台词）。
	subtitle   string
	alternates []tl.Alternate
	// take 取段方式（解说成片向后补长画面时按它决定从哪头扩）。
	take Take
}

func (c cut) playLen() int {
	return tl.Clip{SourceInMs: c.inMs, SourceOutMs: c.outMs, Speed: c.speed}.PlayLen()
}

// srcLenFor 成片里要占 film 毫秒、速度 speed 时的取用长度：PlayLen 是整数除法向下取整，
// 取最小的 L 使 PlayLen(L) ≥ film（放慢时可能多出 1 毫秒，不影响卡点的一帧精度）。
func srcLenFor(film, speed int) int {
	if speed == tl.DefaultSpeed || speed <= 0 {
		return film
	}
	l := (film*speed + 99) / 100
	for l > 0 && (l-1)*100/speed >= film {
		l--
	}
	for l*100/speed < film {
		l++
	}
	return l
}

// clipOf 一段 → 整份内容里的片段（画面轨不写 startMs）。
func (c cut) clipOf(id string) tl.ContentClip {
	cc := tl.ContentClip{ID: id, MediaID: c.card.MediaID, SourceInMs: c.inMs, SourceOutMs: c.outMs,
		Origin: tl.OriginAI, SourceNodeID: c.card.SourceNodeID, Alternates: c.alternates}
	if c.speed != tl.DefaultSpeed && c.speed > 0 {
		cc.Speed = intPtr(c.speed)
	}
	if c.trans.IsActive() {
		t := c.trans
		cc.TransitionIn = &t
	}
	return cc
}

// placeCuts 各段的成片起点与成片总长（与服务端 Layout 同一个算法）。
func placeCuts(cuts []cut) (starts []int, total int) {
	clips := make([]tl.Clip, len(cuts))
	for i, c := range cuts {
		clips[i] = tl.Clip{SourceInMs: c.inMs, SourceOutMs: c.outMs, Speed: c.speed, TransitionIn: c.trans}
	}
	return tl.VideoPlacement(clips)
}

// videoClipIDs 画面轨片段的临时 ID：tmp-v1、tmp-v2……（确定性，同样的输入同样的 ID）。
func videoClipID(i int) string { return "tmp-v" + strconv.Itoa(i+1) }

// ---------------------------------------------------------------- 取段

// Take 是从一段素材里取哪一部分。
type Take struct {
	Kind string `json:"kind"`
	// AroundMs 围绕素材的第几毫秒（Kind = around）。
	AroundMs int `json:"aroundMs,omitempty"`
	// RatioPermille 取段比例（千分比，100~1000）；0 = 默认 40%。只对 head / middle / tail / around 有意义。
	RatioPermille int `json:"ratioPermille,omitempty"`
}

// region 这一段的可用区域（素材时间）：整段 → 源时长；前 / 中 / 后 / 围绕 → 源时长的 40%
// （不少于 1.5 秒、不超过源时长）。图片：[0, 3 秒]。
func region(c Card, t Take) (in, out int) {
	if c.Kind == InputImage {
		return 0, ImageDefaultMs
	}
	d := c.DurationMs
	if t.Kind == TakeFull || t.Kind == "" {
		return 0, d
	}
	ratio := t.RatioPermille
	if ratio <= 0 {
		ratio = TakePartPermille
	}
	l := (d*ratio + 500) / 1000
	l = max(l, min(TakePartMinMs, d))
	l = min(l, d)
	switch t.Kind {
	case TakeHead:
		return 0, l
	case TakeTail:
		return d - l, d
	case TakeMiddle:
		in = (d - l) / 2
		return in, in + l
	case TakeAround:
		in = min(max(t.AroundMs-l/2, 0), d-l)
		return in, in + l
	}
	return 0, d
}

// window 在可用区域里取 srcLen 长的一段：前 → 从区域起点、后 → 到区域终点、中与围绕 → 居中。
// 区域不够长时向整段素材扩（卡点会让一段长出几百毫秒），仍不够就夹到素材本身。
func window(c Card, t Take, regionIn, regionOut, srcLen int) (in, out int) {
	limit := c.DurationMs
	if c.Kind == InputImage {
		// 图片没有「素材本身」：取用区间就是显示时长。
		return 0, srcLen
	}
	srcLen = min(srcLen, limit)
	switch t.Kind {
	case TakeTail:
		in = regionOut - srcLen
	case TakeMiddle, TakeAround:
		in = regionIn + (regionOut-regionIn-srcLen)/2
	default:
		in = regionIn
	}
	in = min(max(in, 0), limit-srcLen)
	return in, in + srcLen
}

// ---------------------------------------------------------------- 分配

// waterFill 按权重把 need 分给各项、每项不超过 caps[i]（Σ caps ≥ need 时结果之和恰为 need）。
// 全走整数：先把份额超过上限的项钉在上限，再按权重分剩下的，余数按最大余数法、平局取下标小的。
func waterFill(need int, weights, caps []int) []int {
	n := len(weights)
	out := make([]int, n)
	active := make([]bool, n)
	for i := range weights {
		active[i] = weights[i] > 0 && caps[i] > 0
	}
	remaining := need
	for {
		w := 0
		for i := range weights {
			if active[i] {
				w += weights[i]
			}
		}
		if w == 0 || remaining <= 0 {
			break
		}
		capped := false
		for i := range weights {
			// 份额 remaining*w_i/W 不小于上限：钉在上限（钉掉一项之后其余各项的份额只会变大，一轮里钉全也不会钉错）。
			if active[i] && caps[i]*w <= remaining*weights[i] {
				out[i], active[i] = caps[i], false
				remaining -= caps[i]
				capped = true
			}
		}
		if capped {
			continue
		}
		type rem struct{ i, r int }
		var rs []rem
		given := 0
		for i := range weights {
			if !active[i] {
				continue
			}
			q := remaining * weights[i] / w
			out[i] = q
			given += q
			rs = append(rs, rem{i, remaining * weights[i] % w})
		}
		sort.SliceStable(rs, func(a, b int) bool { return rs[a].r > rs[b].r })
		for k := 0; k < remaining-given && k < len(rs); k++ {
			out[rs[k].i]++
		}
		break
	}
	return out
}

// ---------------------------------------------------------------- 节拍

// beatGrid 节拍：全部拍点（升序、去重）、强拍、搜索半窗（拍距中位数的一半）。
type beatGrid struct {
	beats []int
	down  map[int]bool
	half  int
}

func newBeatGrid(b Beats) (beatGrid, bool) {
	beats := uniqSorted(b.BeatsMs)
	if len(beats) < 2 {
		return beatGrid{}, false
	}
	gaps := make([]int, 0, len(beats)-1)
	for i := 1; i < len(beats); i++ {
		gaps = append(gaps, beats[i]-beats[i-1])
	}
	sort.Ints(gaps)
	g := beatGrid{beats: beats, down: map[int]bool{}, half: gaps[len(gaps)/2] / 2}
	for _, d := range b.DownbeatsMs {
		g.down[d] = true
	}
	return g, g.half > 0
}

// candidates 搜索窗（±半拍）里的拍点，按优先顺序：离得近的在前；一样近时强拍在前，再一样取早的（SCR-5：找不到就不吸附）。
func (g beatGrid) candidates(t int) []int {
	lo := sort.SearchInts(g.beats, t-g.half)
	var out []int
	for i := lo; i < len(g.beats) && g.beats[i] <= t+g.half; i++ {
		out = append(out, g.beats[i])
	}
	sort.SliceStable(out, func(a, b int) bool {
		da, db := absInt(out[a]-t), absInt(out[b]-t)
		if da != db {
			return da < db
		}
		if g.down[out[a]] != g.down[out[b]] {
			return g.down[out[a]]
		}
		return out[a] < out[b]
	})
	return out
}

// before 不晚于 hi、不早于 lo 的最后一个拍点。
func (g beatGrid) before(hi, lo int) (int, bool) {
	i := sort.SearchInts(g.beats, hi+1) - 1
	if i >= 0 && g.beats[i] >= lo {
		return g.beats[i], true
	}
	return 0, false
}

// nearestDistance 离最近一个拍点多远（报偏差用）；没有拍点时 -1。
func (g beatGrid) nearestDistance(t int) int {
	if len(g.beats) == 0 {
		return -1
	}
	i := sort.SearchInts(g.beats, t)
	best := -1
	for _, j := range []int{i - 1, i} {
		if j >= 0 && j < len(g.beats) {
			if d := absInt(g.beats[j] - t); best < 0 || d < best {
				best = d
			}
		}
	}
	return best
}

// ---------------------------------------------------------------- 时间排布（切换点）

// span 是一段在排布阶段的事实：分到的成片时长、转入的重叠、排布时最多能放多长（成片毫秒）。
type span struct {
	dur     int
	overlap int
	snapCap int
}

// layoutSpans 由切换点算每段的时长。
//
// 切换点（dev/01 §4.3 第 4 条）：第 k-1 段与第 k 段的切换点 S_k = 第 k 段的成片起点 + 转场时长的一半
// （硬切就是起点本身）。S_k 固定时改转场时长不动切换点 —— 转场两边各让一半，卡在拍上的切换点仍在拍上。
func layoutSpans(switches []int, end int, overlaps []int) []int {
	n := len(overlaps)
	starts := make([]int, n)
	for k := 1; k < n; k++ {
		starts[k] = switches[k] - overlaps[k]/2
	}
	durs := make([]int, n)
	for k := 0; k < n; k++ {
		e := end
		if k+1 < n {
			e = starts[k+1] + overlaps[k+1]
		}
		durs[k] = e - starts[k]
	}
	return durs
}

// nominalSwitches 按分到的时长与重叠算出切换点与结尾。
func nominalSwitches(spans []span) (switches []int, end int) {
	n := len(spans)
	switches = make([]int, n)
	start := 0
	for k := 0; k < n; k++ {
		if k > 0 {
			start = start + spans[k-1].dur - spans[k].overlap
			switches[k] = start + spans[k].overlap/2
		}
	}
	if n > 0 {
		end = start + spans[n-1].dur
	}
	return switches, end
}

// snapResult 卡点的结果：切换点与结尾。
type snapResult struct {
	switches []int
	end      int
}

// missedBeat 没卡上的切换点：第 k 处（第 k 段与第 k+1 段之间）与离最近拍点的距离。
type missedBeat struct {
	k, distance int
}

// missedBeats 排布定下来之后，离最近拍点超过一帧的切换点（SC-AC-39 的判据：偏差不超过一帧）。
func missedBeats(g beatGrid, switches []int, fps int) []missedBeat {
	frame := 1000 / max(fps, 1)
	var out []missedBeat
	for k := 1; k < len(switches); k++ {
		if d := g.nearestDistance(switches[k]); d > frame {
			out = append(out, missedBeat{k: k, distance: d})
		}
	}
	return out
}

// snapToBeats 依次把切换点吸到最近的拍点（优先强拍，搜索窗 ±半拍），误差不累积：
// 每个切换点都从名义位置出发去找拍点，前一处的偏差自然结转进下一段的时长。
// 一段的时长不超过它在排布时的上限（snapCap）；找不到合适的拍点就留在名义位置并记下偏差（不硬吸）。
func snapToBeats(spans []span, g beatGrid) snapResult {
	n := len(spans)
	nominal, nominalEnd := nominalSwitches(spans)
	res := snapResult{switches: make([]int, n)}
	prevStart := 0
	for k := 1; k < n; k++ {
		o := spans[k].overlap
		// 上一段最长到哪：prevStart + snapCap，即切换点不晚于 prevStart + snapCap − o + o/2。
		maxSwitch := prevStart + spans[k-1].snapCap - o + o/2
		chosen, snapped := 0, false
		for _, b := range g.candidates(nominal[k]) {
			st := b - o/2
			if d := st + o - prevStart; d > 0 && b <= maxSwitch {
				chosen, snapped = b, true
				break
			}
		}
		if !snapped {
			// 窗口里的拍点放不下（上一段已经用到头）：退到窗口里、不晚于 maxSwitch 的最后一个拍点（最多早一拍），
			// 切换点仍在拍上，多出的误差结转给下一段。
			if b, ok := g.before(min(maxSwitch, nominal[k]+g.half), nominal[k]-2*g.half); ok && b-o/2+o-prevStart > 0 {
				chosen, snapped = b, true
			}
		}
		if !snapped {
			chosen = min(nominal[k], maxSwitch)
			chosen = max(chosen, prevStart+o/2+1)
		}
		res.switches[k] = chosen
		prevStart = chosen - o/2
	}
	// 结尾也吸一次（配乐在拍上收住）；配乐比成片短、附近没有拍点时留在名义位置。
	end := prevStart + (nominalEnd - lastStart(nominal, spans))
	if n > 0 {
		maxEnd := prevStart + spans[n-1].snapCap
		for _, b := range g.candidates(end) {
			if b > prevStart && b <= maxEnd {
				end = b
				break
			}
		}
		end = min(end, maxEnd)
	}
	res.end = end
	return res
}

// lastStart 名义排布下最后一段的成片起点。
func lastStart(switches []int, spans []span) int {
	n := len(spans)
	if n <= 1 {
		return 0
	}
	return switches[n-1] - spans[n-1].overlap/2
}

// clampOverlaps 转场时长不超过相邻两段较短者的一半，不到 tl.MinTransitionMs 改硬切（dev/01 §4.3 第 5 条）。
// 切换点固定，缩短转场只会让两段都短一点 —— 反复收敛到满足为止。
func clampOverlaps(switches []int, end int, overlaps []int) []int {
	n := len(overlaps)
	out := append([]int(nil), overlaps...)
	for iter := 0; iter < 16; iter++ {
		durs := layoutSpans(switches, end, out)
		changed := false
		for k := 1; k < n; k++ {
			if out[k] == 0 {
				continue
			}
			lim := min(durs[k-1], durs[k]) / 2
			o := min(out[k], lim)
			if o < tl.MinTransitionMs {
				o = 0
			}
			if o != out[k] {
				out[k] = o
				changed = true
			}
		}
		if !changed {
			break
		}
	}
	return out
}

// ---------------------------------------------------------------- 合并进时间线

// arrangement 是编译出来的「AI 管的那部分」。
type arrangement struct {
	video []tl.ContentClip
	// videoRole 画面轨用途：口播精剪标 voice（人声在画面原声里），其余不动。
	videoRole string
	music     *tl.ContentClip
	// voices 解说的配音（多条首尾相接，各带成片起点）。
	voices []tl.ContentClip
	// subtitles 生成的字幕（TrackID 由 merge 填）；replaceSubtitles = 生成了字幕，替换第一条字幕轨上的全部字幕。
	subtitles        []tl.ContentSubtitle
	replaceSubtitles bool
	subtitleLang     string
	title            *tl.ContentText
}

// 新建轨道的临时 ID。
const (
	tmpVideoTrack    = "tmp-track-video"
	tmpMusicTrack    = "tmp-track-music"
	tmpVoiceTrack    = "tmp-track-voice"
	tmpSubtitleTrack = "tmp-track-sub"
	tmpTextTrack     = "tmp-track-text"
)

// merge 把编译结果放进当前内容（规则见文件头）。返回附注（替换掉了什么、放不下什么）。
func merge(base tl.Content, a arrangement) (tl.Content, []string) {
	out := tl.Content{Version: base.Version, AspectRatio: base.AspectRatio, FPS: base.FPS,
		Width: base.Width, Height: base.Height}
	var notes []string
	out.Tracks = make([]tl.ContentTrack, len(base.Tracks))
	for i, t := range base.Tracks {
		t.Clips = append([]tl.ContentClip(nil), t.Clips...)
		out.Tracks[i] = t
	}
	trackIndex := func(kind, role string) int {
		for i, t := range out.Tracks {
			if t.Kind == kind && (role == "" || t.Role == role) {
				return i
			}
		}
		return -1
	}
	count := func(kind string) int {
		n := 0
		for _, t := range out.Tracks {
			if t.Kind == kind {
				n++
			}
		}
		return n
	}

	// 画面轨：整条换掉。
	vi := trackIndex(tl.TrackVideo, "")
	if vi < 0 {
		out.Tracks = append([]tl.ContentTrack{{ID: tmpVideoTrack, Kind: tl.TrackVideo}}, out.Tracks...)
		vi = 0
	}
	out.Tracks[vi].Clips = a.video
	if a.videoRole != "" {
		out.Tracks[vi].Role = a.videoRole
	}

	place := func(clips []tl.ContentClip, role, tmpID, label string) {
		if len(clips) == 0 {
			return
		}
		i := trackIndex(tl.TrackAudio, role)
		if i < 0 {
			if count(tl.TrackAudio) >= tl.MaxAudioTracks {
				notes = append(notes, fmt.Sprintf("音频轨已满（最多 %d 条），%s没有放上去", tl.MaxAudioTracks, label))
				return
			}
			out.Tracks = append(out.Tracks, tl.ContentTrack{ID: tmpID, Kind: tl.TrackAudio, Role: role})
			i = len(out.Tracks) - 1
		}
		out.Tracks[i].Clips = clips
	}
	if a.music != nil {
		place([]tl.ContentClip{*a.music}, RoleMusic, tmpMusicTrack, "配乐")
	}
	place(a.voices, RoleVoice, tmpVoiceTrack, "配音")

	present := map[string]bool{}
	for _, t := range out.Tracks {
		for _, c := range t.Clips {
			present[c.ID] = true
		}
	}

	// 字幕：生成了就替换一条字幕轨上的全部字幕；没生成时，跟随被替换片段的字幕一起去掉（它们说的是那一段画面）。
	// 放进哪一条见 subtitleTrackFor。
	subTrack := ""
	if a.replaceSubtitles {
		si := subtitleTrackFor(out.Tracks, a.subtitleLang)
		if si < 0 {
			notes = append(notes, fmt.Sprintf("字幕轨已满（最多 %d 条），字幕没有放上去", tl.MaxSubtitleTracks))
		} else {
			if si == len(out.Tracks) {
				out.Tracks = append(out.Tracks, newSubtitleTrack(a.subtitleLang))
			}
			subTrack = out.Tracks[si].ID
		}
	}
	dropped := 0
	for _, s := range base.Subtitles {
		if subTrack != "" && s.TrackID == subTrack {
			continue
		}
		if id, ok := anchorClipID(s.Anchor); ok && !present[id] {
			dropped++
			continue
		}
		out.Subtitles = append(out.Subtitles, s)
	}
	if dropped > 0 {
		notes = append(notes, fmt.Sprintf("原来跟随片段的 %d 条字幕随片段一起换掉了", dropped))
	}
	if subTrack != "" {
		for _, s := range a.subtitles {
			s.TrackID = subTrack
			out.Subtitles = append(out.Subtitles, s)
		}
	}

	// 标题：放在第一条文字轨的开头，替换掉那条轨上与标题时间重叠的文字（同轨不许重叠）。
	out.Texts = append([]tl.ContentText(nil), base.Texts...)
	if a.title != nil {
		ti := trackIndex(tl.TrackText, "")
		if ti < 0 && count(tl.TrackText) < tl.MaxTextTracks {
			out.Tracks = append(out.Tracks, tl.ContentTrack{ID: tmpTextTrack, Kind: tl.TrackText})
			ti = len(out.Tracks) - 1
		}
		if ti < 0 {
			notes = append(notes, fmt.Sprintf("文字轨已满（最多 %d 条），标题没有放上去", tl.MaxTextTracks))
		} else {
			tid := out.Tracks[ti].ID
			kept := out.Texts[:0]
			for _, t := range out.Texts {
				if t.TrackID == tid && t.StartMs < a.title.EndMs && a.title.StartMs < t.EndMs {
					notes = append(notes, "开头的文字「"+headRunes(t.Text, 20)+"」换成了新标题")
					continue
				}
				kept = append(kept, t)
			}
			title := *a.title
			title.TrackID = tid
			out.Texts = append(kept, title)
			sortTexts(out.Texts)
		}
	}
	return out, notes
}

func sortTexts(texts []tl.ContentText) {
	sort.SliceStable(texts, func(i, j int) bool {
		if texts[i].TrackID != texts[j].TrackID {
			return false
		}
		return texts[i].StartMs < texts[j].StartMs
	})
}

// anchorClipID 锚点里的片段 ID。
func anchorClipID(raw []byte) (string, bool) {
	var a struct {
		ClipID string `json:"clipId"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &a) != nil || a.ClipID == "" {
		return "", false
	}
	return a.ClipID, true
}

// ---------------------------------------------------------------- 字幕、标题、配乐

// subtitleStyle / titleStyle 默认样式（SC-PARAM-31：字幕字号 4.5%）。
var (
	titleStyle = []byte(`{"fontSizePct":8,"color":"#FFFFFF","stroke":{"color":"#000000","widthPct":0.6},"align":"center"}`)
	titleBox   = []byte(`{"xPct":50,"yPct":22,"anchor":"center"}`)
)

// anchoredSubtitle 一条跟随片段的字幕：素材区间 [srcIn, srcOut)，起止按片段当前的成片起点换算（SC-F-56）。
func anchoredSubtitle(id, text, clipID string, clipStart int, clip tl.Clip, srcIn, srcOut int) (tl.ContentSubtitle, bool) {
	if srcOut <= srcIn || strings.TrimSpace(text) == "" {
		return tl.ContentSubtitle{}, false
	}
	p := tl.PlaceAnchored(srcIn, srcOut, clipStart, clip, tl.SpanPlace{})
	if p.Hidden || p.EndMs <= p.StartMs {
		return tl.ContentSubtitle{}, false
	}
	anchor := fmt.Sprintf(`{"clipId":%q,"srcStartMs":%d,"srcEndMs":%d}`, clipID, srcIn, srcOut)
	return tl.ContentSubtitle{ID: id, StartMs: p.StartMs, EndMs: p.EndMs, Text: text, Anchor: []byte(anchor)}, true
}

// titleLayer 开头标题（文字层）：[0, min(标题时长, 成片)]。
func titleLayer(t *Title, total int) *tl.ContentText {
	if t == nil || strings.TrimSpace(t.Text) == "" || total < tl.MinClipMs {
		return nil
	}
	d := t.DurationMs
	if d <= 0 {
		d = TitleDefaultMs
	}
	d = min(max(d, TitleMinMs), TitleMaxMs, total)
	return &tl.ContentText{ID: "tmp-title", StartMs: 0, EndMs: d, Text: t.Text, Style: titleStyle, Box: titleBox}
}

// musicClip 配乐：从成片开头放起，裁到成片总长并在结尾淡出（dev/01 §4.3 第 1 条）。配乐比成片短时说出来。
func musicClip(c Card, total int) (*tl.ContentClip, string) {
	if total < tl.MinClipMs {
		return nil, ""
	}
	l := total
	warn := ""
	if c.DurationMs > 0 && c.DurationMs < total {
		l = c.DurationMs
		warn = fmt.Sprintf("配乐只有 %s 秒，比成片短 %s 秒，结尾没有配乐", secText(l), secText(total-l))
	}
	if l < tl.MinClipMs {
		return nil, warn
	}
	fade := min(MusicFadeOutMs, l/4)
	return &tl.ContentClip{ID: "tmp-music", MediaID: c.MediaID, StartMs: intPtr(0), SourceInMs: 0, SourceOutMs: l,
		FadeOutMs: fade, Origin: tl.OriginAI, SourceNodeID: c.SourceNodeID}, warn
}

// ---------------------------------------------------------------- 小工具

func intPtr(v int) *int { return &v }

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

func uniqSorted(in []int) []int {
	out := append([]int(nil), in...)
	sort.Ints(out)
	k := 0
	for i, v := range out {
		if v < 0 {
			continue
		}
		if i == 0 || k == 0 || v != out[k-1] {
			out[k] = v
			k++
		}
	}
	return out[:k]
}

// secText 毫秒 → 「14.3」「20」（四舍五入到 0.1 秒、整数运算；整秒不写 .0，同 03 SC-F-92 的例句）。
func secText(ms int) string {
	if ms < 0 {
		return "-" + secText(-ms)
	}
	tenths := (ms + 50) / 100
	if tenths%10 == 0 {
		return strconv.Itoa(tenths / 10)
	}
	return strconv.Itoa(tenths/10) + "." + strconv.Itoa(tenths%10)
}

func headRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

func runeLen(s string) int { return utf8.RuneCountInString(s) }

// joinNos 编号列表 → 「3、1、2」。
func joinNos(nos []string) string { return strings.Join(nos, "、") }

// cardIndex 按编号查卡片。
func cardIndex(cards []Card) map[string]Card {
	m := make(map[string]Card, len(cards))
	for _, c := range cards {
		m[c.No] = c
	}
	return m
}

// unusedPictureWarning 没用上的画面素材（SC-F-82）。
func unusedPictureWarning(cards []Card, used []string) string {
	seen := map[string]bool{}
	for _, u := range used {
		seen[u] = true
	}
	var nos []string
	for _, c := range cards {
		if c.IsPicture() && !seen[c.No] {
			nos = append(nos, c.No)
		}
	}
	if len(nos) == 0 {
		return ""
	}
	return "没用上的素材：编号 " + joinNos(nos)
}

// pickMusic 方案选的配乐；没选而素材里有配乐时用第一条（用户连了配乐，就是想用它），并说明。
func pickMusic(cards []Card, chosen string) (Card, bool, string) {
	idx := cardIndex(cards)
	if c, ok := idx[chosen]; ok && c.Kind == InputAudio {
		return c, true, ""
	}
	for _, c := range cards {
		if c.Kind == InputAudio && c.AudioRole == RoleMusic {
			return c, true, "方案没有选配乐，用了编号 " + c.No
		}
	}
	return Card{}, false, ""
}

// usedOrder 按成片顺序去重的编号。
func usedOrder(cuts []cut) []string {
	var out []string
	seen := map[string]bool{}
	for _, c := range cuts {
		if !seen[c.card.No] {
			seen[c.card.No] = true
			out = append(out, c.card.No)
		}
	}
	return out
}

// summaryOf 结果摘要：用了哪几段、总时长。
func summaryOf(used []string, total int) string {
	if len(used) == 0 {
		return "没有可用的片段"
	}
	return fmt.Sprintf("用了 %d 段素材（编号 %s），成片 %s 秒", len(used), joinNos(used), secText(total))
}

// finish 收尾：合并进时间线、算总长、附注去重。
func finish(in PlanInput, a arrangement, cuts []cut, res Result) Result {
	content, notes := merge(in.Base, a)
	res.Content = content
	_, res.TotalMs = placeCuts(cuts)
	if res.UsedClips == nil {
		res.UsedClips = usedOrder(cuts)
	}
	res.Notes = append(res.Notes, notes...)
	res.Warnings = dedupe(res.Warnings)
	res.Notes = dedupe(res.Notes)
	if res.Warnings == nil {
		res.Warnings = []string{}
	}
	if res.Summary == "" {
		res.Summary = summaryOf(res.UsedClips, res.TotalMs)
	}
	return res
}

func dedupe(list []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, s := range list {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

func clipOfCut(c cut) tl.Clip {
	return tl.Clip{SourceInMs: c.inMs, SourceOutMs: c.outMs, Speed: c.speed, TransitionIn: c.trans}
}

// subtitleTrackFor 生成的字幕放进哪一条字幕轨（会就地给轨道填上语言）：
//
//   - 语言未知时（混剪、分镜，或识别结果没给语言）是第一条字幕轨；
//   - 知道语言时（口播、解说、识别字幕取识别语言）是语言对得上的那条，其次是还没定语言的第一条（把语言填上 ——
//     之后的翻译按它认原文语言），都没有时新建一条；轨道满了就退回第一条。
//
// 返回下标；等于 len(tracks) 表示要新建一条（newSubtitleTrack），-1 表示没有字幕轨而且满了。
// 起因见 dev/01 R-15 第 8 条：默认字幕轨没有语言，原来的写法放进去之后轨道语言还是空的；有英文轨时会把中文字幕塞进英文轨。
func subtitleTrackFor(tracks []tl.ContentTrack, lang string) int {
	first, count := -1, 0
	for i, t := range tracks {
		if t.Kind == tl.TrackSubtitle {
			count++
			if first < 0 {
				first = i
			}
		}
	}
	if lang != "" {
		for i, t := range tracks {
			if t.Kind == tl.TrackSubtitle && t.Lang == lang {
				return i
			}
		}
		for i, t := range tracks {
			if t.Kind == tl.TrackSubtitle && t.Lang == "" {
				tracks[i].Lang = lang
				return i
			}
		}
	} else if first >= 0 {
		return first
	}
	if count < tl.MaxSubtitleTracks {
		return len(tracks)
	}
	return first
}

// newSubtitleTrack 新建的字幕轨（语言未知时用缺省语言）。
func newSubtitleTrack(lang string) tl.ContentTrack {
	if lang == "" {
		lang = DefaultSubtitleLang
	}
	return tl.ContentTrack{ID: tmpSubtitleTrack, Kind: tl.TrackSubtitle, Lang: lang}
}
