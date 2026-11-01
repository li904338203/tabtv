package domain

import (
	"encoding/json"

	tl "github.com/li904338203/tabtv/backend/internal/timeline/domain"
)

// 片段级生成的产物落点（docs/agent/smartcut SCT-37~39，SCD-33 / SCD-35）：纯函数，回写那一步照它改内容。

// LandReplace 把一段换成新素材（片段重拍、去字幕）：
//
//   - 新素材从头取，取用长度 = min(新素材时长, 原来的取用长度)（图片与时长未知的素材按原来的长度）——
//     重拍出来的往往比原来长，按原长度取才不会把后面的段都往后推；
//   - 原素材连同原来的取用区间进备选（SCD-35：切回原片要连区间一起回去），最多 tl.MaxAlternates 个；
//   - 片段的其余设置（速度、音量、淡入淡出、转场、画面变换、来源）原样保留。
//
// 片段不在时返回 false（内容不变）。
func LandReplace(c tl.Content, clipID string, media int64, mediaMs int, old tl.Alternate) (tl.Content, bool) {
	out := c
	out.Tracks = append([]tl.ContentTrack(nil), c.Tracks...)
	for ti := range out.Tracks {
		for ci, cl := range out.Tracks[ti].Clips {
			if cl.ID != clipID {
				continue
			}
			clips := append([]tl.ContentClip(nil), out.Tracks[ti].Clips...)
			length := cl.SourceOutMs - cl.SourceInMs
			if mediaMs > 0 {
				length = min(length, mediaMs)
			}
			old.MediaID, old.SourceInMs, old.SourceOutMs = cl.MediaID, cl.SourceInMs, cl.SourceOutMs
			next := cl
			next.MediaID, next.SourceInMs, next.SourceOutMs = media, 0, max(length, tl.MinClipMs)
			next.Alternates = tl.PushAlternate(cl.Alternates, old)
			clips[ci] = next
			out.Tracks[ti].Clips = clips
			return out, true
		}
	}
	return c, false
}

// LandInsert 在画面轨上插入一段新素材（补镜）：插在 afterClipID 之后，afterClipID 为空时插在最前面。
// 位置那一段已经不在时返回 false（不自动找一个地方插：用户在发起时选的是「这两段之间」）。
// 新片段的来源记 user：是用户要的一段，AI 重新剪的时候不该把它当成 AI 自己放的换掉。
func LandInsert(c tl.Content, afterClipID string, media int64, mediaMs int, tmpID string) (tl.Content, bool) {
	out := c
	out.Tracks = append([]tl.ContentTrack(nil), c.Tracks...)
	for ti, t := range out.Tracks {
		if t.Kind != tl.TrackVideo {
			continue
		}
		at := 0
		if afterClipID != "" {
			at = -1
			for ci, cl := range t.Clips {
				if cl.ID == afterClipID {
					at = ci + 1
				}
			}
			if at < 0 {
				return c, false
			}
		}
		length := mediaMs
		if length <= 0 {
			length = ImageDefaultMs
		}
		clip := tl.ContentClip{ID: tmpID, MediaID: media, SourceInMs: 0, SourceOutMs: max(length, tl.MinClipMs), Origin: tl.OriginUser}
		clips := make([]tl.ContentClip, 0, len(t.Clips)+1)
		clips = append(clips, t.Clips[:at]...)
		clips = append(clips, clip)
		clips = append(clips, t.Clips[at:]...)
		out.Tracks[ti].Clips = clips
		return out, true
	}
	return c, false
}

// FindClip 内容里的一段（任何轨）。
func FindClip(c tl.Content, clipID string) (tl.ContentClip, string, bool) {
	for _, t := range c.Tracks {
		for _, cl := range t.Clips {
			if cl.ID == clipID {
				return cl, t.Kind, true
			}
		}
	}
	return tl.ContentClip{}, "", false
}

// 生成配乐 / 生成音效的落点（SCT-39）：放进音频轨。轨道的选取与输入同步同一个顺序（timeline/app.audioTrackFor）：
// 同用途的轨 → 认领一条还没定用途的轨（默认那条「音频」轨）→ 新建一条（名字为配乐 / 音效）；音频轨已满（tl.MaxAudioTracks）
// 时放不下，返回 false（产物只进生成记录，不挤掉用户的轨道）。

// 新建音频轨时的名字（与输入同步一致）。
var audioTrackNames = map[string]string{tl.RoleMusic: "配乐", tl.RoleSfx: "音效"}

// LandMusic 把一段配乐放进配乐轨：起点 atMs，长度 = min(素材时长, 想要的长度)（时长未知按想要的；都没有按 3 秒）。
//
// 与已有的配乐重叠时放进**下一条**不重叠的配乐轨，都重叠时新建一条 —— 不覆盖、不报错（同轨不许重叠，SCD-15）。
// 新片段的来源记 user：是用户要的一段，AI 重新剪的时候不该把它当成自己放的换掉。
func LandMusic(c tl.Content, media int64, mediaMs, atMs, wantMs int, tmpClip, tmpTrack string) (tl.Content, bool) {
	length := wantMs
	if mediaMs > 0 && (length <= 0 || mediaMs < length) {
		length = mediaMs
	}
	if length <= 0 {
		length = ImageDefaultMs
	}
	length = max(length, tl.MinClipMs)
	start := max(atMs, 0)
	clip := tl.ContentClip{ID: tmpClip, MediaID: media, SourceInMs: 0, SourceOutMs: length, StartMs: &start, Origin: tl.OriginUser}
	out := cloneTracks(c)
	free := func(t tl.ContentTrack) bool { return !overlapsAt(t, start, start+length) }
	if ti := pickAudioTrack(&out, tl.RoleMusic, free, tmpTrack); ti >= 0 {
		out.Tracks[ti].Clips = append(out.Tracks[ti].Clips, clip)
		return out, true
	}
	return c, false
}

// LandSfx 把一段按画面生成的音效放进音效轨，跟随那一段（SC-F-56）：锚点记下生成时的素材区间 rangeMs，
// 音频从 0 起与它一一对应（区间比音频长时按音频的长度收窄锚点，免得算出的取用区间超出音频本身）。
// 落点与取用区间由 tl.FollowClip 按那一段当前的位置算；那一段已经不在画面轨上时返回 false（只进生成记录）。
//
// 跟随片段的音效不参与同轨重叠（它跟着画面走），所以放进第一条音效轨即可。
func LandSfx(c tl.Content, clipID string, rangeMs []int, media int64, mediaMs int, tmpClip, tmpTrack string) (tl.Content, bool) {
	target, kind, ok := FindClip(c, clipID)
	if !ok || kind != tl.TrackVideo || len(rangeMs) != 2 || rangeMs[1] <= rangeMs[0] {
		return c, false
	}
	srcStart, srcEnd := rangeMs[0], rangeMs[1]
	if mediaMs > 0 {
		srcEnd = min(srcEnd, srcStart+mediaMs)
	}
	anchor, _ := json.Marshal(map[string]any{"clipId": clipID, "srcStartMs": srcStart, "srcEndMs": srcEnd})
	lay := tl.Layout(c)
	at := lay.Clips[clipID].StartMs
	// 发起之后那一段被改得与生成时的区间不相交了：音效照样落下（隐藏，按那一段的起点留着），恢复区间后重新出现。
	pl := tl.FollowClip(srcStart, srcEnd, at, contentClipOf(target),
		tl.Clip{SourceInMs: 0, SourceOutMs: srcEnd - srcStart, StartMs: at, Speed: tl.DefaultSpeed})
	start := pl.StartMs
	clip := tl.ContentClip{ID: tmpClip, MediaID: media, SourceInMs: pl.Follow.SourceInMs, SourceOutMs: pl.Follow.SourceOutMs,
		StartMs: &start, Origin: tl.OriginUser, Anchor: anchor}
	if pl.Follow.Speed != tl.DefaultSpeed {
		sp := pl.Follow.Speed
		clip.Speed = &sp
	}
	out := cloneTracks(c)
	if ti := pickAudioTrack(&out, tl.RoleSfx, func(tl.ContentTrack) bool { return true }, tmpTrack); ti >= 0 {
		out.Tracks[ti].Clips = append(out.Tracks[ti].Clips, clip)
		return out, true
	}
	return c, false
}

// pickAudioTrack 找一条这个用途、并且 fits 的音频轨（同用途 → 认领没定用途的 → 新建），返回下标；放不下为 -1。
func pickAudioTrack(c *tl.Content, role string, fits func(tl.ContentTrack) bool, tmpTrack string) int {
	free, audio := -1, 0
	for ti, t := range c.Tracks {
		if t.Kind != tl.TrackAudio {
			continue
		}
		audio++
		if t.Role == role && fits(t) {
			return ti
		}
		if t.Role == "" && free < 0 && fits(t) {
			free = ti
		}
	}
	if free >= 0 {
		c.Tracks[free].Role = role
		return free
	}
	if audio >= tl.MaxAudioTracks {
		return -1
	}
	c.Tracks = append(c.Tracks, tl.ContentTrack{ID: tmpTrack, Kind: tl.TrackAudio, Name: audioTrackNames[role], Role: role})
	return len(c.Tracks) - 1
}

// overlapsAt 这条轨上有没有与 [start, end) 重叠的片段（跟随片段的不算：它们不参与同轨重叠，见 tl.ValidateContent）。
func overlapsAt(t tl.ContentTrack, start, end int) bool {
	for _, cl := range t.Clips {
		if len(cl.Anchor) > 0 || cl.StartMs == nil {
			continue
		}
		if s := *cl.StartMs; s < end && start < s+cl.PlayLen() {
			return true
		}
	}
	return false
}

// cloneTracks 复制到轨道与片段这一层（返回的内容可以改，不碰原内容）。
func cloneTracks(c tl.Content) tl.Content {
	out := c
	out.Tracks = make([]tl.ContentTrack, len(c.Tracks))
	for i, t := range c.Tracks {
		t.Clips = append([]tl.ContentClip(nil), t.Clips...)
		out.Tracks[i] = t
	}
	return out
}

// contentClipOf 整份内容里的片段 → 算落点要用的领域片段。
func contentClipOf(cc tl.ContentClip) tl.Clip {
	c := tl.Clip{SourceInMs: cc.SourceInMs, SourceOutMs: cc.SourceOutMs, Speed: tl.DefaultSpeed}
	if cc.Speed != nil {
		c.Speed = *cc.Speed
	}
	return c
}
