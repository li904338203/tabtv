package domain

import (
	"fmt"
	"strings"
	"testing"

	tl "github.com/li904338203/tabtv/backend/internal/timeline/domain"
)

// asrContent 画面轨：A（素材 71 的 0~4 秒）、B（同一份素材的 6~9 秒）、C（素材 72，没有识别结果）；
// 配音轨：D（素材 81，从成片 1 秒起）；配乐轨：E（不出字幕）。字幕轨 1003 上有一条手写字幕与一条旧的识别字幕（跟随 A）。
func asrContent() tl.Content {
	start := func(ms int) *int { return &ms }
	return tl.Content{Version: 3, AspectRatio: "9:16", FPS: 30, Width: 1080, Height: 1920,
		Tracks: []tl.ContentTrack{
			{ID: "1001", Kind: tl.TrackVideo, Clips: []tl.ContentClip{
				{ID: "11", MediaID: 71, SourceInMs: 0, SourceOutMs: 4000},
				{ID: "12", MediaID: 71, SourceInMs: 6000, SourceOutMs: 9000},
				{ID: "13", MediaID: 72, SourceInMs: 0, SourceOutMs: 3000},
			}},
			{ID: "1002", Kind: tl.TrackAudio, Role: RoleVoice, Clips: []tl.ContentClip{
				{ID: "21", MediaID: 81, StartMs: start(1000), SourceInMs: 0, SourceOutMs: 2000}}},
			{ID: "1005", Kind: tl.TrackAudio, Role: RoleMusic, Clips: []tl.ContentClip{
				{ID: "31", MediaID: 91, StartMs: start(0), SourceInMs: 0, SourceOutMs: 9000}}},
			{ID: "1003", Kind: tl.TrackSubtitle, Lang: "zh-CN"},
		},
		Subtitles: []tl.ContentSubtitle{
			{ID: "501", TrackID: "1003", StartMs: 8000, EndMs: 9000, Text: "手写的字幕"},
			{ID: "502", TrackID: "1003", StartMs: 0, EndMs: 800, Text: "旧的识别字幕",
				Anchor: []byte(`{"clipId":"11","srcStartMs":0,"srcEndMs":800}`)},
		}}
}

func asrTranscripts() map[int64]Transcript {
	return map[int64]Transcript{
		71: {Language: "zh-CN", Granularity: "char", Utterances: []Utterance{
			utter(chars("大家好", 500, 200), chars("今天讲剪辑", 1500, 200)),
			utter(chars("第二句话", 3500, 200)), // 3500~4300：跨过 A 的结尾 4000
			utter(chars("后半段", 6500, 200)),  // 在 B 里
		}},
		81: {Granularity: "char", Utterances: []Utterance{utter(chars("配音", 100, 200))}},
	}
}

func subLines(c tl.Content) []string {
	lay := tl.Layout(c)
	var out []string
	for _, s := range c.Subtitles {
		p := lay.Subtitles[s.ID]
		id, _ := anchorClipID(s.Anchor)
		out = append(out, fmt.Sprintf("%s@%s:%d-%d", s.Text, id, p.StartMs, p.EndMs))
	}
	return out
}

// SCT-36（SC-F-120）：按片段的取用区间取句 —— 同一份素材被用了两段，各段只出自己区间里的字；配音轨的片段也出；
// 配乐轨不出；没有识别结果的片段不出、也不算覆盖到。
func TestSubtitlesFromASR_按片段区间取句(t *testing.T) {
	c := asrContent()
	c.Subtitles = nil
	subs, covered := ASRSubtitles(c, nil, asrTranscripts())
	c.Subtitles = subs
	got := strings.Join(subLines(c), " ")
	// A 从 0 起；B 接在 A 后（4000 起），「后半段」6500 → 4000+500；配音 D 从 1000 起，「配音」100 → 1100。
	want := "大家好@11:500-1100 今天讲剪辑@11:1500-2500 第二句@11:3500-4000 后半段@12:4500-5100 配音@21:1100-1500"
	if got != want {
		t.Fatalf("\n got %s\nwant %s", got, want)
	}
	if fmt.Sprint(covered) != "map[11:true 12:true 21:true]" {
		t.Fatalf("覆盖到的片段：%v", covered)
	}
	// 只识别选中的片段：只出 B 的。
	subs, covered = ASRSubtitles(c, []string{"12"}, asrTranscripts())
	if len(subs) != 1 || subs[0].Text != "后半段" || len(covered) != 1 {
		t.Fatalf("只出选中的片段：%+v %v", subs, covered)
	}
}

// SCT-36：一句跨了片段边界就切开 —— 「第二句话」只有「第二句」在 A 里（「句」一半在区间里算，「话」不算），
// 字幕的锚点到区间边界为止；逗号处断成两条，文字不带标点。
func TestSubtitlesFromASR_跨片段边界切开(t *testing.T) {
	c := asrContent()
	tr := asrTranscripts()
	tr[71] = Transcript{Granularity: "char", Utterances: []Utterance{
		{Text: "大家好，今天讲剪辑。", StartMs: 500, EndMs: 2500,
			Words: append(chars("大家好", 500, 200), chars("今天讲剪辑", 1500, 200)...)},
		utter(chars("第二句话", 3500, 200)),
	}}
	subs, _ := ASRSubtitles(c, []string{"11"}, tr)
	var got []string
	for _, s := range subs {
		got = append(got, fmt.Sprintf("%s:%s", s.Text, s.Anchor))
	}
	want := []string{`大家好:{"clipId":"11","srcStartMs":500,"srcEndMs":1100}`,
		`今天讲剪辑:{"clipId":"11","srcStartMs":1500,"srcEndMs":2500}`,
		`第二句:{"clipId":"11","srcStartMs":3500,"srcEndMs":4000}`}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("\n got %v\nwant %v", got, want)
	}
	// 没有字级时间的整句：一半以上在区间里才出，起止夹到区间里。
	tr[71] = Transcript{Granularity: "segment", Utterances: []Utterance{
		{Text: "整句一", StartMs: 3000, EndMs: 4600}, // 1000 在 A 里（≥ 一半）
		{Text: "整句二", StartMs: 3800, EndMs: 5000}, // 只有 200 在 A 里
	}}
	subs, _ = ASRSubtitles(c, []string{"11"}, tr)
	if len(subs) != 1 || subs[0].Text != "整句一" || string(subs[0].Anchor) != `{"clipId":"11","srcStartMs":3000,"srcEndMs":4000}` {
		t.Fatalf("%+v", subs)
	}
}

// SCT-36（SC-F-56 / SC-AC-19）：字幕带锚点、跟随片段 —— 裁掉 A 的开头 1 秒，落在被裁区间里的「大家好」隐藏，
// 其余整体前移 1 秒；恢复区间后重新出现。合并时只替换这条字幕轨上跟随覆盖到的片段的字幕，手写的不动。
func TestSubtitlesFromASR_带锚点(t *testing.T) {
	c := asrContent()
	subs, covered := ASRSubtitles(c, nil, asrTranscripts())
	merged, changed, ok := MergeASRSubtitles(c, subs, covered, "zh-CN")
	if !ok || !changed {
		t.Fatal("要写入")
	}
	var texts []string
	for _, s := range merged.Subtitles {
		if s.TrackID != "1003" {
			t.Fatalf("放进语言对得上的那条字幕轨：%+v", s)
		}
		texts = append(texts, s.Text)
	}
	if strings.Join(texts, "|") != "手写的字幕|大家好|今天讲剪辑|第二句|后半段|配音" {
		t.Fatalf("旧的识别字幕换掉、手写的保留：%v", texts)
	}
	before := tl.Layout(merged)
	trimmed := merged
	trimmed.Tracks = append([]tl.ContentTrack(nil), merged.Tracks...)
	trimmed.Tracks[0].Clips = append([]tl.ContentClip(nil), merged.Tracks[0].Clips...)
	trimmed.Tracks[0].Clips[0].SourceInMs = 1000
	after := tl.Layout(trimmed)
	for _, s := range merged.Subtitles {
		b, a := before.Subtitles[s.ID], after.Subtitles[s.ID]
		switch s.Text {
		case "大家好":
			if !a.Hidden {
				t.Fatalf("被裁区间里的要隐藏：%+v", a)
			}
		case "今天讲剪辑", "第二句", "后半段":
			if a.Hidden || b.StartMs-a.StartMs != 1000 {
				t.Fatalf("「%s」整体前移 1 秒：%+v → %+v", s.Text, b, a)
			}
		}
	}
	if p := tl.Layout(merged).Subtitles; p[merged.Subtitles[1].ID].Hidden {
		t.Fatal("恢复区间后重新出现")
	}

	// 没有对得上的语言时用还没定语言的那条并填上语言；什么都没识别出来（也没有旧的）时不用写。
	c2 := asrContent()
	c2.Tracks[3].Lang = ""
	c2.Subtitles = nil
	merged, _, _ = MergeASRSubtitles(c2, subs, covered, "zh-CN")
	if merged.Tracks[3].Lang != "zh-CN" || len(merged.Subtitles) != len(subs) {
		t.Fatalf("填上语言：%+v", merged.Tracks[3])
	}
	if _, changed, _ := MergeASRSubtitles(c2, nil, map[string]bool{"13": true}, "zh-CN"); changed {
		t.Fatal("没有字幕、也没有要换掉的：不用写（填语言也不算）")
	}
}
