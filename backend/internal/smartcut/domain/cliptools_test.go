package domain

import (
	"fmt"
	"testing"

	tl "github.com/li904338203/tabtv/backend/internal/timeline/domain"
)

func clipToolsContent() tl.Content {
	speed := 150
	return tl.Content{Version: 4, AspectRatio: "9:16", FPS: 30, Width: 1080, Height: 1920,
		Tracks: []tl.ContentTrack{{ID: "1001", Kind: tl.TrackVideo, Clips: []tl.ContentClip{
			{ID: "11", MediaID: 71, SourceInMs: 1000, SourceOutMs: 5000, Speed: &speed, FadeInMs: 200, Origin: tl.OriginAI},
			{ID: "12", MediaID: 72, SourceInMs: 0, SourceOutMs: 3000},
		}}}}
}

// SCT-37（SCD-35）：替换片段 —— 新素材从头取、长度取新旧较短的，原素材连同原区间进备选，其余设置保留。
func TestLandReplace_新素材从头取原素材进备选(t *testing.T) {
	c := clipToolsContent()
	out, ok := LandReplace(c, "11", 81, 6000, tl.Alternate{Kind: tl.AlternateOriginal, GenerationID: 901})
	if !ok {
		t.Fatal("片段在")
	}
	got := out.Tracks[0].Clips[0]
	if got.MediaID != 81 || got.SourceInMs != 0 || got.SourceOutMs != 4000 || *got.Speed != 150 || got.FadeInMs != 200 || got.Origin != tl.OriginAI {
		t.Fatalf("新素材取原来的 4 秒、设置保留：%+v", got)
	}
	if len(got.Alternates) != 1 || got.Alternates[0] != (tl.Alternate{MediaID: 71, SourceInMs: 1000, SourceOutMs: 5000,
		Kind: tl.AlternateOriginal, GenerationID: 901}) {
		t.Fatalf("原素材连同原区间进备选：%+v", got.Alternates)
	}
	if c.Tracks[0].Clips[0].MediaID != 71 {
		t.Fatal("原内容不变")
	}
	// 新素材比原来短：取新素材的全长；时长未知（图片）：按原来的长度。
	out, _ = LandReplace(c, "11", 82, 2500, tl.Alternate{Kind: tl.AlternateRegen})
	if out.Tracks[0].Clips[0].SourceOutMs != 2500 {
		t.Fatalf("取新素材全长：%+v", out.Tracks[0].Clips[0])
	}
	out, _ = LandReplace(c, "11", 83, 0, tl.Alternate{Kind: tl.AlternateRegen})
	if out.Tracks[0].Clips[0].SourceOutMs != 4000 {
		t.Fatalf("时长未知按原长度：%+v", out.Tracks[0].Clips[0])
	}
	if _, ok := LandReplace(c, "99", 81, 6000, tl.Alternate{}); ok {
		t.Fatal("片段不在")
	}
	// 备选最多 tl.MaxAlternates 个，丢最早的。
	cur := c
	for i := 0; i < tl.MaxAlternates+2; i++ {
		cur, _ = LandReplace(cur, "12", int64(100+i), 3000, tl.Alternate{Kind: tl.AlternateRegen})
	}
	if alts := cur.Tracks[0].Clips[1].Alternates; len(alts) != tl.MaxAlternates || alts[0].MediaID == 72 {
		t.Fatalf("备选上限：%d %+v", len(alts), alts[0])
	}
}

// SCT-37：补镜插在记下的位置之后（为空时最前面）；那一段已经不在时不插。
func TestLandInsert_插在记下的位置(t *testing.T) {
	c := clipToolsContent()
	out, ok := LandInsert(c, "11", 91, 5000, "tmp-fill")
	if !ok || len(out.Tracks[0].Clips) != 3 || out.Tracks[0].Clips[1].ID != "tmp-fill" || out.Tracks[0].Clips[1].SourceOutMs != 5000 ||
		out.Tracks[0].Clips[1].Origin != tl.OriginUser {
		t.Fatalf("插在第一段之后：%+v", out.Tracks[0].Clips)
	}
	out, _ = LandInsert(c, "", 91, 5000, "tmp-fill")
	if out.Tracks[0].Clips[0].ID != "tmp-fill" {
		t.Fatal("没给位置时插在最前面")
	}
	if _, ok := LandInsert(c, "99", 91, 5000, "tmp-fill"); ok {
		t.Fatal("位置那一段不在时不插")
	}
	if len(c.Tracks[0].Clips) != 2 {
		t.Fatal("原内容不变")
	}
}

func withAudio(c tl.Content, tracks ...tl.ContentTrack) tl.Content {
	c.Tracks = append(append([]tl.ContentTrack(nil), c.Tracks...), tracks...)
	return c
}

func at(ms int) *int { return &ms }

// SCT-39：配乐放在 atMs，长度取素材与想要的较短者；先认领默认那条没定用途的音频轨（定成配乐）。
func TestLandMusic_按播放头放进配乐轨(t *testing.T) {
	c := withAudio(clipToolsContent(), tl.ContentTrack{ID: "2001", Kind: tl.TrackAudio, Name: "音频"})
	out, ok := LandMusic(c, 91, 30000, 1500, 20000, "tmp-music-1", "tmp-music-track-1")
	if !ok || len(out.Tracks) != 2 || out.Tracks[1].Role != tl.RoleMusic {
		t.Fatalf("认领默认音频轨、定成配乐：%+v", out.Tracks)
	}
	got := out.Tracks[1].Clips[0]
	if got.MediaID != 91 || *got.StartMs != 1500 || got.SourceInMs != 0 || got.SourceOutMs != 20000 || got.Origin != tl.OriginUser {
		t.Fatalf("起点 = 播放头、取想要的 20 秒：%+v", got)
	}
	if c.Tracks[1].Role != "" {
		t.Fatal("原内容不变")
	}
	// 素材比想要的短：取素材全长；时长未知：按想要的。
	out, _ = LandMusic(c, 92, 8000, 0, 20000, "tmp-music-1", "tmp-music-track-1")
	if out.Tracks[1].Clips[0].SourceOutMs != 8000 {
		t.Fatalf("取素材全长：%+v", out.Tracks[1].Clips[0])
	}
	out, _ = LandMusic(c, 93, 0, 0, 12000, "tmp-music-1", "tmp-music-track-1")
	if out.Tracks[1].Clips[0].SourceOutMs != 12000 {
		t.Fatalf("时长未知按想要的：%+v", out.Tracks[1].Clips[0])
	}
}

// 与已有配乐重叠时放进下一条不重叠的配乐轨，都重叠时新建一条（不覆盖、不报错）；音频轨满了放不下。
func TestLandMusic_重叠时新建配乐轨(t *testing.T) {
	music := tl.ContentTrack{ID: "2001", Kind: tl.TrackAudio, Role: tl.RoleMusic, Clips: []tl.ContentClip{
		{ID: "21", MediaID: 81, SourceInMs: 0, SourceOutMs: 10000, StartMs: at(0)}}}
	c := withAudio(clipToolsContent(), music)
	out, ok := LandMusic(c, 91, 6000, 5000, 6000, "tmp-music-1", "tmp-music-track-1")
	if !ok || len(out.Tracks) != 3 || out.Tracks[2].ID != "tmp-music-track-1" || out.Tracks[2].Role != tl.RoleMusic ||
		out.Tracks[2].Name != "配乐" || len(out.Tracks[1].Clips) != 1 {
		t.Fatalf("重叠时新建一条配乐轨、原来那条不动：%+v", out.Tracks)
	}
	// 不重叠时放进已有的那条。
	out, _ = LandMusic(c, 92, 6000, 10000, 6000, "tmp-music-2", "tmp-music-track-2")
	if len(out.Tracks) != 2 || len(out.Tracks[1].Clips) != 2 {
		t.Fatalf("首尾相接不算重叠，放进已有的配乐轨：%+v", out.Tracks)
	}
	// 跟随片段的音效不挡配乐：同轨的跟随片段不算重叠。
	anchored := tl.ContentTrack{ID: "2002", Kind: tl.TrackAudio, Role: tl.RoleMusic, Clips: []tl.ContentClip{
		{ID: "22", MediaID: 82, SourceInMs: 0, SourceOutMs: 4000, StartMs: at(5000), Anchor: []byte(`{"clipId":"12","srcStartMs":0,"srcEndMs":3000}`)}}}
	out, _ = LandMusic(withAudio(clipToolsContent(), anchored), 93, 3000, 5000, 3000, "tmp-music-3", "tmp-music-track-3")
	if len(out.Tracks) != 2 || len(out.Tracks[1].Clips) != 2 {
		t.Fatalf("跟随片段的不算重叠：%+v", out.Tracks)
	}
	// 音频轨已满、每条都重叠：放不下（产物只进生成记录）。
	full := clipToolsContent()
	for i := 0; i < tl.MaxAudioTracks; i++ {
		full = withAudio(full, tl.ContentTrack{ID: fmt.Sprintf("30%02d", i), Kind: tl.TrackAudio, Role: tl.RoleMusic,
			Clips: []tl.ContentClip{{ID: fmt.Sprintf("4%02d", i), MediaID: 81, SourceInMs: 0, SourceOutMs: 10000, StartMs: at(0)}}})
	}
	if _, ok := LandMusic(full, 94, 6000, 2000, 6000, "tmp-music-4", "tmp-music-track-4"); ok {
		t.Fatal("音频轨满了放不下")
	}
}

// SCT-39（SC-F-56）：音效放进音效轨并跟随那一段：锚点是生成时的区间（比音频长时按音频收窄），落点是那一段的成片起点；
// 那一段之后被移动、裁切、变速，音效跟着走。
func TestLandSfx_跟随片段(t *testing.T) {
	c := clipToolsContent() // 第 1 段 [1000,5000] 1.5 倍速 → 成片 2666；第 2 段从 2666 起
	out, ok := LandSfx(c, "12", []int{0, 3000}, 95, 2900, "tmp-sfx-1", "tmp-sfx-track-1")
	if !ok || len(out.Tracks) != 2 || out.Tracks[1].Role != tl.RoleSfx || out.Tracks[1].Name != "音效" {
		t.Fatalf("新建一条音效轨：%+v", out.Tracks)
	}
	got := out.Tracks[1].Clips[0]
	if *got.StartMs != 2666 || got.SourceInMs != 0 || got.SourceOutMs != 2900 || got.Origin != tl.OriginUser {
		t.Fatalf("落在第 2 段的起点、取音频全长：%+v", got)
	}
	if string(got.Anchor) != `{"clipId":"12","srcEndMs":2900,"srcStartMs":0}` {
		t.Fatalf("锚点按音频长度收窄：%s", got.Anchor)
	}
	// 把第 2 段挪到最前面：音效跟着到 0。
	moved := cloneTracks(out)
	moved.Tracks[0].Clips = []tl.ContentClip{out.Tracks[0].Clips[1], out.Tracks[0].Clips[0]}
	if p := tl.Layout(moved).Clips["tmp-sfx-1"]; p.StartMs != 0 || p.Follow == nil || p.Follow.Hidden {
		t.Fatalf("移动片段后音效跟着走：%+v", p)
	}
	// 那一段已经不在画面轨上：放不下（只进生成记录）。
	if _, ok := LandSfx(c, "99", []int{0, 3000}, 95, 2900, "tmp-sfx-1", "tmp-sfx-track-1"); ok {
		t.Fatal("那一段不在")
	}
}
