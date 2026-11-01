package domain

import (
	"encoding/json"
	"math"
)

// 渲染快照（NEW-10；docs/agent/smartcut SCD-27 重构：SCT-11 画面、SCT-12 音频、SCT-14 字幕与文字）。
//
// ⚠️ 冻结是 NEW-10 的核心要求："固定时间线版本"。不冻结的话，用户在渲染排队时改了一刀，
// 出来的片子既不是他点渲染时看到的，也不是他现在看到的 —— 而事后完全复现不了。
//
// ⚠️ 排队中的旧任务在升级之后还要能渲染：旧格式（Schema 0）只有一组 Audio / AudioMuted / AudioVolume
// 与不带样式的 Subtitles，读出来先过 UpgradeLegacy 换成新结构，后面只认新结构。

// SnapshotSchema 当前快照的结构版本。⚠️ 与时间线的 version（乐观锁）不是一回事。
const SnapshotSchema = 2

// RenderSnapshot 是一次渲染冻结下来的全部输入。时间一律是成片时间（SCK-15）。
type RenderSnapshot struct {
	// Schema 快照结构版本：0 = SCT-11 之前受理的旧任务。
	Schema          int `json:"schema,omitempty"`
	TimelineID      int64
	TimelineVersion int
	Width           int
	Height          int
	FPS             int
	// Resolution 导出档位（720p / 1080p，SCT-15）；旧快照为空。
	Resolution string
	// Clips 画面轨（成片顺序）。
	Clips []SnapClip
	// VideoRole 画面原声的用途：voice 表示口播（参与人声闪避）；空 = 普通原声。
	VideoRole   string
	VideoMuted  bool
	VideoVolume int
	// AudioTracks 音频轨（按轨道顺序）。片段的 StartMs 是成片起点（按起点放置，不首尾相接）。
	AudioTracks []SnapAudioTrack
	// SubtitleTracks 字幕轨（按轨道顺序：第一条在最下方）；Texts 文字层（SCT-14）。
	SubtitleTracks []SnapSubtitleTrack
	Texts          []SnapText
	// Watermark 是这次渲染要叠的水印。
	//
	// ⚠️ 水印配置**也要进快照**：不冻结的话，排队期间运营换了 logo，
	// 出来的片子带的是新 logo，而任务记的是旧的 —— 事后对不上。
	Watermark *Watermark

	// 旧格式（Schema 0）。只读：UpgradeLegacy 之后清空，新快照不写。
	Audio       []SnapClip
	AudioMuted  bool
	AudioVolume int
	Subtitles   []SnapSub
}

// snapshotJSON 是 RenderSnapshot 的 JSON 形状（键名与旧快照一致，旧字段只在有值时写出）。
type snapshotJSON struct {
	Schema          int                 `json:"schema,omitempty"`
	TimelineID      int64               `json:"timelineId"`
	TimelineVersion int                 `json:"timelineVersion"`
	Width           int                 `json:"width"`
	Height          int                 `json:"height"`
	FPS             int                 `json:"fps"`
	Resolution      string              `json:"resolution,omitempty"`
	Clips           []SnapClip          `json:"clips"`
	VideoRole       string              `json:"videoRole,omitempty"`
	VideoMuted      bool                `json:"videoMuted,omitempty"`
	VideoVolume     *int                `json:"videoVolume,omitempty"`
	AudioTracks     []SnapAudioTrack    `json:"audioTracks,omitempty"`
	SubtitleTracks  []SnapSubtitleTrack `json:"subtitleTracks,omitempty"`
	Texts           []SnapText          `json:"texts,omitempty"`
	Watermark       *Watermark          `json:"watermark,omitempty"`
	Audio           []SnapClip          `json:"audio,omitempty"`
	AudioMuted      bool                `json:"audioMuted,omitempty"`
	AudioVolume     int                 `json:"audioVolume,omitempty"`
	Subtitles       []SnapSub           `json:"subtitles,omitempty"`
}

func (s RenderSnapshot) MarshalJSON() ([]byte, error) {
	j := snapshotJSON{Schema: s.Schema, TimelineID: s.TimelineID, TimelineVersion: s.TimelineVersion,
		Width: s.Width, Height: s.Height, FPS: s.FPS, Resolution: s.Resolution, Clips: s.Clips,
		VideoRole: s.VideoRole, VideoMuted: s.VideoMuted, AudioTracks: s.AudioTracks,
		SubtitleTracks: s.SubtitleTracks, Texts: s.Texts, Watermark: s.Watermark,
		Audio: s.Audio, AudioMuted: s.AudioMuted, AudioVolume: s.AudioVolume, Subtitles: s.Subtitles}
	if s.Schema >= SnapshotSchema {
		v := s.VideoVolume
		j.VideoVolume = &v
	}
	if j.Clips == nil {
		j.Clips = []SnapClip{}
	}
	return json.Marshal(j)
}

func (s *RenderSnapshot) UnmarshalJSON(b []byte) error {
	var j snapshotJSON
	if err := json.Unmarshal(b, &j); err != nil {
		return err
	}
	*s = RenderSnapshot{Schema: j.Schema, TimelineID: j.TimelineID, TimelineVersion: j.TimelineVersion,
		Width: j.Width, Height: j.Height, FPS: j.FPS, Resolution: j.Resolution, Clips: j.Clips,
		VideoRole: j.VideoRole, VideoMuted: j.VideoMuted, VideoVolume: DefaultVolume,
		AudioTracks: j.AudioTracks, SubtitleTracks: j.SubtitleTracks, Texts: j.Texts, Watermark: j.Watermark,
		Audio: j.Audio, AudioMuted: j.AudioMuted, AudioVolume: j.AudioVolume, Subtitles: j.Subtitles}
	if j.VideoVolume != nil {
		s.VideoVolume = *j.VideoVolume
	}
	s.UpgradeLegacy()
	return nil
}

// UpgradeLegacy 把旧格式（Schema 0）换成新结构：一组 Audio 变成一条配乐轨（音量与静音保留，
// 起点照旧快照里的 —— 那时音频轨首尾相接，存下来的就是首尾相接的起点），Subtitles 变成一条默认样式的字幕轨。
// 新格式原样不动。重复调用无害。
func (s *RenderSnapshot) UpgradeLegacy() {
	if s.Schema >= SnapshotSchema {
		return
	}
	if len(s.Audio) > 0 {
		vol := s.AudioVolume
		if vol <= 0 && !s.AudioMuted {
			vol = DefaultVolume
		}
		clips := make([]SnapClip, len(s.Audio))
		for i, c := range s.Audio {
			if c.Kind == "" {
				c.Kind = "audio"
			}
			clips[i] = c
		}
		s.AudioTracks = append(s.AudioTracks, SnapAudioTrack{Role: RoleMusic, Muted: s.AudioMuted, Volume: vol,
			Clips: clips})
	}
	if len(s.Subtitles) > 0 {
		items := make([]SnapSubItem, 0, len(s.Subtitles))
		for _, sub := range s.Subtitles {
			items = append(items, SnapSubItem{StartMs: sub.StartMs, EndMs: sub.EndMs, Text: sub.Text})
		}
		s.SubtitleTracks = append(s.SubtitleTracks, SnapSubtitleTrack{Order: 1, Items: items})
	}
	for i := range s.Clips {
		if s.Clips[i].Kind == "" {
			s.Clips[i].Kind = "video"
		}
	}
	if s.VideoVolume <= 0 {
		s.VideoVolume = DefaultVolume
	}
	s.Audio, s.AudioMuted, s.AudioVolume, s.Subtitles = nil, false, 0, nil
	s.Schema = SnapshotSchema
}

// SnapClip 是快照里的一段。
//
// ⚠️ 存的是 `MediaID` 而不是 objectKey/URL：渲染时按 ID 重新解析，
// 中途素材被删会**明确失败**，而不是拿着一个失效地址去下载、
// 得到一个 0 字节文件然后渲出一段黑屏。
type SnapClip struct {
	MediaID int64 `json:"mediaId"`
	// Kind 素材种类：video / image / audio（旧快照为空：画面轨按 video、音频轨按 audio）。
	Kind        string `json:"kind,omitempty"`
	SourceInMs  int    `json:"sourceInMs"`
	SourceOutMs int    `json:"sourceOutMs"`
	// StartMs 成片起点；DurationMs 成片长度（算上变速）。
	StartMs    int `json:"startMs"`
	DurationMs int `json:"durationMs"`
	Speed      int `json:"speed"`
	Volume     int `json:"volume"`
	FadeInMs   int `json:"fadeInMs"`
	FadeOutMs  int `json:"fadeOutMs"`
	// Transition 转入（与上一段的衔接；第一段上的不渲染）。
	Transition Transition `json:"transition"`
	// TransitionOut 只在画面轨最后一段上有：淡出到黑场（SC-F-44）。
	TransitionOut *Transition `json:"transitionOut,omitempty"`
	// Transform 画面变换（SC-F-43）；nil = 等比适配画布、居中。
	Transform *Transform `json:"transform,omitempty"`
}

// PlayLenOf 这一段在时间线上占多久（算上变速）。
//
// ⚠️ 补静音时要用**时间线时长**而不是素材时长：2 倍速的 4 秒素材
// 在片子里只占 2 秒，补 4 秒静音的话后面全错位。
func (c SnapClip) PlayLenOf() int {
	if c.DurationMs > 0 {
		return c.DurationMs
	}
	speed := c.Speed
	if speed <= 0 {
		speed = 100
	}
	return (c.SourceOutMs - c.SourceInMs) * 100 / speed
}

// IsImage 这一段是不是图片（图片要 `-loop 1` 才有持续画面，SCB-1）。
func (c SnapClip) IsImage() bool { return c.Kind == "image" }

// SnapSub 是旧快照里的一条字幕（只读）。
type SnapSub struct {
	StartMs int    `json:"startMs"`
	EndMs   int    `json:"endMs"`
	Text    string `json:"text"`
}

// SnapAudioTrack 是一条音频轨（SCT-12）。
type SnapAudioTrack struct {
	// Role 用途：music / voice / sfx（决定进哪条总线；配乐被人声压低）。
	Role   string `json:"role"`
	Muted  bool   `json:"muted,omitempty"`
	Volume int    `json:"volume"`
	// Duck 人声闪避（只对配乐轨有意义）；nil = 缺省开启、DefaultDuckDepthDb。
	Duck  *Duck      `json:"duck,omitempty"`
	Clips []SnapClip `json:"clips"`
}

// Duck 人声闪避（SC-F-62）：有人声时把配乐压低 DepthDb。
type Duck struct {
	Enabled bool `json:"enabled"`
	DepthDb int  `json:"depthDb"`
}

// 人声闪避的缺省值（SC-PARAM-28，设计建议值）：压低 12 dB、起效 50ms、恢复 300ms。
const (
	DefaultDuckDepthDb = 12
	DuckAttackMs       = 50
	DuckReleaseMs      = 300
)

// DuckOf 配乐轨的闪避设置：没设过时缺省开启。
func (t SnapAudioTrack) DuckOf() Duck {
	if t.Duck == nil {
		return Duck{Enabled: true, DepthDb: DefaultDuckDepthDb}
	}
	d := *t.Duck
	if d.DepthDb <= 0 {
		d.DepthDb = DefaultDuckDepthDb
	}
	return d
}

// SnapSubtitleTrack 是一条字幕轨（SCT-14）。Style 是轨道样式（缺省值与轨道 style 合并后的结果）。
type SnapSubtitleTrack struct {
	TrackID int64           `json:"trackId,string,omitempty"`
	Lang    string          `json:"lang,omitempty"`
	Style   json.RawMessage `json:"style,omitempty"`
	// Order 轨道顺序（1 起）：第一条在最下方，后面的依次往上让出行高。
	Order int           `json:"order"`
	Items []SnapSubItem `json:"items"`
}

// SnapSubItem 是一条字幕：起止为成片时间（跟随片段的已按 Layout 换算，被裁掉的不进快照）。
// Style 是条目自己的样式（覆盖轨道样式），没有为空。
type SnapSubItem struct {
	StartMs int             `json:"startMs"`
	EndMs   int             `json:"endMs"`
	Text    string          `json:"text"`
	Style   json.RawMessage `json:"style,omitempty"`
}

// SnapText 是一个文字层（标题、贴字）：起止为成片时间。
type SnapText struct {
	TrackOrder int             `json:"trackOrder,omitempty"`
	StartMs    int             `json:"startMs"`
	EndMs      int             `json:"endMs"`
	Text       string          `json:"text"`
	Style      json.RawMessage `json:"style,omitempty"`
	Box        json.RawMessage `json:"box,omitempty"`
}

// ---------------------------------------------------------------- 画面变换

// Transform 画面变换（dev/02 §2）：
//   - X / Y：片段中心相对画面中心的偏移，按画面宽 / 高的百分比（-100~100）；
//   - Scale：相对「裁切之后等比适配画布」的倍数（0.1~5，缺省 1）；
//   - Rotation：顺时针角度（-180~180）；
//   - Crop：四边各裁掉素材本身宽 / 高的百分比（0~45）。
//
// 顺序是：裁切（按素材本身）→ 等比适配画布 × Scale → 旋转（露出的角透明）→ 中心放到 (X, Y) → 叠到黑底上。
// 剪辑台的预览（SCT-21）照同一个顺序摆。
type Transform struct {
	X        float64 `json:"x,omitempty"`
	Y        float64 `json:"y,omitempty"`
	Scale    float64 `json:"scale,omitempty"`
	Rotation float64 `json:"rotation,omitempty"`
	Crop     Crop    `json:"crop,omitempty"`
}

// Crop 四边裁切（素材本身宽高的百分比）。
type Crop struct {
	Left   float64 `json:"left,omitempty"`
	Top    float64 `json:"top,omitempty"`
	Right  float64 `json:"right,omitempty"`
	Bottom float64 `json:"bottom,omitempty"`
}

// ParseTransform 读片段的 transform（原样 JSON）。等于缺省（适配、居中、不旋转不裁切）或解析不了时回 nil ——
// 一个字段坏了不该让整条时间线渲不出来（同 ParseTransition）。数值夹回合法范围。
func ParseTransform(raw json.RawMessage) *Transform {
	if len(raw) == 0 {
		return nil
	}
	var t Transform
	if err := json.Unmarshal(raw, &t); err != nil {
		return nil
	}
	t.X, t.Y = clampF(t.X, -100, 100), clampF(t.Y, -100, 100)
	if t.Scale == 0 {
		t.Scale = 1
	}
	t.Scale = clampF(t.Scale, 0.1, 5)
	t.Rotation = clampF(t.Rotation, -180, 180)
	t.Crop = Crop{clampF(t.Crop.Left, 0, 45), clampF(t.Crop.Top, 0, 45), clampF(t.Crop.Right, 0, 45),
		clampF(t.Crop.Bottom, 0, 45)}
	if t.IsIdentity() {
		return nil
	}
	return &t
}

// IsIdentity 是不是缺省（不需要变换链）。
func (t Transform) IsIdentity() bool {
	return t.X == 0 && t.Y == 0 && (t.Scale == 0 || t.Scale == 1) && t.Rotation == 0 && t.Crop == (Crop{})
}

func clampF(v, lo, hi float64) float64 {
	if math.IsNaN(v) {
		return 0
	}
	return math.Max(lo, math.Min(hi, v))
}

// MediaFile 是渲染用到的一份素材在本机的样子（按媒体去重下载，SCB-9）。
type MediaFile struct {
	Path     string
	HasVideo bool
	HasAudio bool
	// AudioChannels 音轨的声道数（单声道要复制成双声道而不是按默认矩阵上混 —— 默认会低 3dB）。
	AudioChannels int
}
