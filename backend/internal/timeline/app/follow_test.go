package app

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/li904338203/tabtv/backend/internal/timeline/domain"
	"github.com/li904338203/tabtv/backend/internal/timeline/repo"
)

// docs/agent/smartcut SCT-39（SC-F-56）：跟随片段的音效在读的时候按锚点现算；被裁掉的不进成片。

func followView(videoIn, videoOut, speed int) View {
	anchor, _ := json.Marshal(map[string]any{"clipId": "12", "srcStartMs": 2000, "srcEndMs": 10000})
	video := []domain.Clip{
		{ID: 11, MediaID: 71, SourceInMs: 0, SourceOutMs: 4000, Speed: 100, StartMs: 0, DurationMs: 4000},
		{ID: 12, MediaID: 72, SourceInMs: videoIn, SourceOutMs: videoOut, Speed: speed, StartMs: 4000},
	}
	video[1].DurationMs = video[1].PlayLen()
	return View{Timeline: repo.Timeline{ID: 1, Width: 1280, Height: 720, FPS: 30},
		Tracks: []TrackView{
			{Track: repo.Track{ID: 1, Kind: domain.TrackVideo, Volume: 100}, Clips: video},
			{Track: repo.Track{ID: 2, Kind: domain.TrackAudio, Role: domain.RoleSfx, Volume: 100}, Clips: []domain.Clip{
				// 存储值是旧的（发起时第二段在别处），读的时候按锚点重算。
				{ID: 21, MediaID: 81, SourceInMs: 0, SourceOutMs: 8000, Speed: 100, StartMs: 500, DurationMs: 8000, Volume: 100,
					Anchor: anchor},
				{ID: 22, MediaID: 82, SourceInMs: 0, SourceOutMs: 2000, Speed: 100, StartMs: 100, DurationMs: 2000, Volume: 100},
			}},
		}}
}

func indexByVideo(v View) (map[int64]domain.Clip, map[int64]domain.Clip) {
	video, all := map[int64]domain.Clip{}, map[int64]domain.Clip{}
	for _, tv := range v.Tracks {
		for _, c := range tv.Clips {
			all[c.ID] = c
			if tv.Track.Kind == domain.TrackVideo {
				video[c.ID] = c
			}
		}
	}
	return video, all
}

func TestFollowAnchored_裁切变速后音效跟着走(t *testing.T) {
	v := followView(4000, 10000, 200) // 第二段裁掉开头 2 秒、2 倍速
	video, all := indexByVideo(v)
	followAnchored(&v, video, all)
	clips := v.Tracks[1].Clips
	// 按起点重排：没跟随的那段（100）在前，跟随的（4000）在后。
	if clips[0].ID != 22 || clips[1].ID != 21 || clips[0].Idx != 1 || clips[1].Idx != 2 {
		t.Fatalf("按起点重排：%+v", clips)
	}
	got := clips[1]
	if got.StartMs != 4000 || got.SourceInMs != 2000 || got.SourceOutMs != 8000 || got.Speed != 200 || got.DurationMs != 3000 ||
		got.Hidden {
		t.Fatalf("跳过开头那 2 秒、2 倍速、从第二段的起点响：%+v", got)
	}
	if clips[0].StartMs != 100 || clips[0].SourceOutMs != 2000 {
		t.Fatalf("没跟随的不动：%+v", clips[0])
	}
}

func TestSnapshot_跟随片段被裁掉的音效不进成片(t *testing.T) {
	v := followView(12000, 14000, 100) // 第二段改取 12~14 秒：与锚点 2~10 秒不相交
	video, all := indexByVideo(v)
	followAnchored(&v, video, all)
	var hidden domain.Clip
	for _, c := range v.Tracks[1].Clips {
		if c.ID == 21 {
			hidden = c
		}
	}
	if !hidden.Hidden || hidden.StartMs != 500 {
		t.Fatalf("隐藏、按存储值留在原处：%+v", hidden)
	}
	s := &Service{}
	snap, _, err := s.snapshot(context.Background(), v, RenderInput{Resolution: "720p"},
		func(int64) (MediaInfo, bool) { return MediaInfo{Kind: "video"}, true }, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.AudioTracks) != 1 || len(snap.AudioTracks[0].Clips) != 1 || snap.AudioTracks[0].Clips[0].MediaID != 82 {
		t.Fatalf("被裁掉的音效不进成片，别的照常：%+v", snap.AudioTracks)
	}
}
