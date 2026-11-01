package domain

import (
	"strings"
	"testing"

	tl "github.com/li904338203/tabtv/backend/internal/timeline/domain"
)

func translateContent() tl.Content {
	return tl.Content{Version: 2, AspectRatio: "9:16", FPS: 30, Width: 1080, Height: 1920,
		Tracks: []tl.ContentTrack{{ID: "1001", Kind: tl.TrackVideo}, {ID: "1003", Kind: tl.TrackSubtitle, Lang: "zh-CN",
			Style: []byte(`{"fontSize":48}`)}},
		Subtitles: []tl.ContentSubtitle{
			{ID: "52", TrackID: "1003", StartMs: 2000, EndMs: 3000, Text: "第二句"},
			{ID: "51", TrackID: "1003", StartMs: 0, EndMs: 1000, Text: "第一句", Anchor: []byte(`{"clipId":"9","srcStartMs":0,"srcEndMs":1000}`)},
			{ID: "53", TrackID: "1003", StartMs: 4000, EndMs: 5000, Text: "第三句"},
		}}
}

// SCT-36（SC-F-125 / 126）：序号按起点排；新轨照抄时间、锚点与样式，只放译出来的；替换只改译出来的那几条。
func TestApplyTranslation_新轨与替换(t *testing.T) {
	c := translateContent()
	if got := SubtitleOrder(c, "1003"); len(got) != 3 || c.Subtitles[got[0]].Text != "第一句" || c.Subtitles[got[2]].Text != "第三句" {
		t.Fatalf("按起点排：%v", got)
	}
	texts := map[int]string{1: "First", 3: "Third"} // 第 2 条没译出来
	out, track := ApplyTranslation(c, "1003", texts, TranslateNewTrack, "en")
	if track != TranslatedTrackTmpID() || len(out.Tracks) != 3 || out.Tracks[2].Lang != "en" || string(out.Tracks[2].Style) != `{"fontSize":48}` {
		t.Fatalf("新建英文轨、样式照抄：%+v", out.Tracks)
	}
	var added []string
	for _, s := range out.Subtitles {
		if s.TrackID == track {
			added = append(added, s.Text+"@"+string(s.Anchor))
			if s.Text == "First" && (s.StartMs != 0 || s.EndMs != 1000) {
				t.Fatalf("时间照抄：%+v", s)
			}
		}
	}
	if strings.Join(added, " ") != `First@{"clipId":"9","srcStartMs":0,"srcEndMs":1000} Third@` || len(out.Subtitles) != 5 {
		t.Fatalf("只放译出来的、锚点照抄：%v", added)
	}
	if c.Subtitles[1].Text != "第一句" || len(c.Tracks) != 2 {
		t.Fatal("原内容不变")
	}

	out, track = ApplyTranslation(c, "1003", texts, TranslateReplace, "en")
	var texts2 []string
	for _, i := range SubtitleOrder(out, "1003") {
		texts2 = append(texts2, out.Subtitles[i].Text)
	}
	if track != "1003" || len(out.Tracks) != 2 || strings.Join(texts2, "|") != "First|第二句|Third" {
		t.Fatalf("替换只改译出来的：%v", texts2)
	}
}
