package app

import (
	"testing"

	"github.com/li904338203/tabtv/backend/internal/timeline/domain"
)

func syncBase() domain.Content {
	start := 0
	return domain.Content{Version: 3, AspectRatio: "9:16", FPS: 30, Width: 1080, Height: 1920, Tracks: []domain.ContentTrack{
		{ID: "1", Kind: domain.TrackVideo, Clips: []domain.ContentClip{{ID: "11", MediaID: 5, SourceOutMs: 4000}}},
		{ID: "2", Kind: domain.TrackAudio, Clips: []domain.ContentClip{}},
		{ID: "3", Kind: domain.TrackAudio, Role: "music", Clips: []domain.ContentClip{
			{ID: "31", MediaID: 6, SourceInMs: 1000, SourceOutMs: 5000, StartMs: &start}}},
	}}
}

// 视频与图片接到画面轨末尾（图片 3 秒）；音频接到同用途轨现有内容之后；没有同用途的轨就认领一条
// 还没定用途的；再没有就新建一条。时长未知的不铺。原内容不被改动（逐条试加失败要能丢掉）。
func TestWithSyncClip(t *testing.T) {
	c := syncBase()
	next, ref, ok := withSyncClip(c, SyncItem{MediaID: 7, Kind: "image", SourceNodeID: "n1"}, 0)
	if !ok || ref != "tmp-sync-1" || len(next.Tracks[0].Clips) != 2 || next.Tracks[0].Clips[1].SourceOutMs != SyncImageMs ||
		next.Tracks[0].Clips[1].Origin != domain.OriginSync || next.Tracks[0].Clips[1].SourceNodeID != "n1" {
		t.Fatalf("图片接到画面轨末尾、3 秒：%+v", next.Tracks[0])
	}
	if len(c.Tracks[0].Clips) != 1 {
		t.Fatal("原内容不能被改动")
	}
	// 配乐：接在已有配乐（1000→5000，成片 0~4000）之后。
	next, _, _ = withSyncClip(c, SyncItem{MediaID: 8, Kind: "audio", DurationMs: 9000, AudioRole: "music"}, 1)
	if got := next.Tracks[2].Clips[1]; got.StartMs == nil || *got.StartMs != 4000 || got.SourceOutMs != 9000 {
		t.Fatalf("接在配乐轨末尾：%+v", next.Tracks[2])
	}
	// 人声：认领那条还没定用途的轨。
	next, _, _ = withSyncClip(c, SyncItem{MediaID: 9, Kind: "audio", DurationMs: 3000, AudioRole: "voice"}, 2)
	if next.Tracks[1].Role != "voice" || len(next.Tracks[1].Clips) != 1 || *next.Tracks[1].Clips[0].StartMs != 0 {
		t.Fatalf("认领没定用途的音频轨：%+v", next.Tracks[1])
	}
	if c.Tracks[1].Role != "" {
		t.Fatal("认领只发生在副本上")
	}
	// 音效：没有空闲轨，新建一条「音效」轨。
	next, _, _ = withSyncClip(next, SyncItem{MediaID: 10, Kind: "audio", DurationMs: 1000, AudioRole: "sfx"}, 3)
	if last := next.Tracks[len(next.Tracks)-1]; last.Kind != domain.TrackAudio || last.Role != "sfx" || last.Name != "音效" ||
		!domain.IsTempID(last.ID) || len(last.Clips) != 1 {
		t.Fatalf("新建音效轨：%+v", last)
	}
	for _, it := range []SyncItem{{MediaID: 1, Kind: "video"}, {MediaID: 1, Kind: "audio"}, {MediaID: 1, Kind: "text"}} {
		if _, _, ok := withSyncClip(c, it, 9); ok {
			t.Fatalf("时长未知 / 不是能上轨的种类不铺：%+v", it)
		}
	}
}
