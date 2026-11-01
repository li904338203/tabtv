package app

import timelinedomain "github.com/li904338203/tabtv/backend/internal/timeline/domain"

func testContent() timelinedomain.Content {
	return timelinedomain.Content{Version: 3, AspectRatio: "9:16", FPS: 30, Width: 1080, Height: 1920,
		Tracks: []timelinedomain.ContentTrack{{ID: "1001", Kind: timelinedomain.TrackVideo}}}
}
