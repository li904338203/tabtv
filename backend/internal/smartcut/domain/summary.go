package domain

// 摘要里的迷你时间轴与封面（dev/02 §4.2 SmartcutSummary，SCK-13）。

// MaxStripItems 节点卡迷你时间轴最多给多少项（契约 strip.maxItems）。
const MaxStripItems = 40

// maxStripAudio 音频那一行最多占几项：画面那一行是节点卡的主体，音频只要看得出「有配乐、在哪」。
// 画面不够 30 项时音频可以多占，反之亦然 —— 总数不超过 MaxStripItems。
const maxStripAudio = 10

// StripItem 迷你时间轴上的一项。Kind 是 video / image / audio；时间是成片时间。
type StripItem struct {
	Kind          string
	StartMs       int
	DurationMs    int
	PosterMediaID int64
	// MediaID 这一段用的素材。节点卡在没有封面帧时（上传的视频不截封面帧）用它直接显示视频画面。
	MediaID int64
}

// Strip 取画面轨与第一条音频轨的片段做迷你时间轴（按各自的顺序，不重排）。
// 超过上限时：音频先让出到 maxStripAudio 以内，余下的给画面；画面不够时音频补满。
func Strip(video, audio []StripItem) []StripItem {
	audioQuota := min(len(audio), maxStripAudio)
	videoTake := min(len(video), MaxStripItems-audioQuota)
	audioTake := min(len(audio), MaxStripItems-videoTake)
	out := make([]StripItem, 0, videoTake+audioTake)
	out = append(out, video[:videoTake]...)
	out = append(out, audio[:audioTake]...)
	return out
}

// Cover 节点卡封面：最近一次成功导出的成片的封面帧；没有就第一段画面的封面（视频取封面帧、图片就是它自己）。
// 都没有（封面帧还没截出来、时间线是空的）回 0，前端画占位。
func Cover(renderPoster int64, video []StripItem, firstMediaID int64) int64 {
	if renderPoster != 0 {
		return renderPoster
	}
	if len(video) == 0 {
		return 0
	}
	if video[0].Kind == "image" {
		return firstMediaID
	}
	return video[0].PosterMediaID
}
