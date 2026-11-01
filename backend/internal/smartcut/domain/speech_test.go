package domain

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	tl "github.com/li904338203/tabtv/backend/internal/timeline/domain"
)

// chars 一串字，每个字 dur 毫秒，从 at 起首尾相接。
func chars(text string, at, dur int) []Word {
	var out []Word
	for _, r := range text {
		out = append(out, Word{Text: string(r), StartMs: at, EndMs: at + dur})
		at += dur
	}
	return out
}

func utter(words ...[]Word) Utterance {
	var all []Word
	for _, w := range words {
		all = append(all, w...)
	}
	return Utterance{Text: joinWords(all), StartMs: all[0].StartMs, EndMs: all[len(all)-1].EndMs, Words: all}
}

// talkFixture 一段 10 秒的口播：
//
//	s1 大家好（500~1100）
//	s2 嗯 今天介绍（嗯 1600~1900，今天介绍 2100~2900）
//	s3 今天介绍（3600~4400，重复）
//	s4 这款产品（5200~6000）
func talkFixture() PlanInput {
	card := videoCard("1", 10000)
	tr := Transcript{Language: "zh", Granularity: "char", Utterances: []Utterance{
		utter(chars("大家好", 500, 200)),
		utter([]Word{{Text: "嗯", StartMs: 1600, EndMs: 1900}}, chars("今天介绍", 2100, 200)),
		utter(chars("今天介绍", 3600, 200)),
		utter(chars("这款产品", 5200, 200)),
	}}
	return PlanInput{Mode: ModeTalk, Base: testBase(), Cards: []Card{card},
		Transcripts: map[string]Transcript{"1": tr}, Spec: Spec{WithSubtitles: true}}
}

func TestTalkSentences_按识别句编号(t *testing.T) {
	sents := TalkSentences(talkFixture())
	if len(sents) != 4 || sents[0].Ref != "s1" || sents[3].Ref != "s4" || sents[1].Text != "嗯今天介绍" {
		t.Fatalf("%+v", sents)
	}
	if sents[1].StartMs != 1600 || sents[1].EndMs != 2900 {
		t.Fatalf("句子起止取字级时间：%+v", sents[1])
	}
}

// TestCompileTalk_剪点落在词边界外扩区间内（SC-AC-42 编译部分）。
func TestCompileTalk_剪点落在词边界外扩区间内(t *testing.T) {
	in := talkFixture()
	d := Draft{Mode: ModeTalk, Keep: []SentenceRange{{From: "s1", To: "s2"}, {From: "s4", To: "s4"}},
		Drop: []SentenceDrop{{Sentence: "s3", Reason: "重复"}}}
	r, err := Compile(in, d)
	if err != nil {
		t.Fatal(err)
	}
	lay := mustValid(t, r)
	clips := video(r)
	// 大家好 | （嗯 去掉）今天介绍 | （s3 去掉）这款产品
	want := [][2]int{{300, 1300}, {2000, 3100}, {5000, 6200}}
	if len(clips) != len(want) {
		t.Fatalf("期望 %d 段，实际 %d：%+v", len(want), len(clips), clips)
	}
	for i, c := range clips {
		if c.SourceInMs != want[i][0] || c.SourceOutMs != want[i][1] {
			t.Errorf("第 %d 段：期望 %v，实际 [%d,%d]", i+1, want[i], c.SourceInMs, c.SourceOutMs)
		}
		if c.FadeInMs != CutFadeMs || c.FadeOutMs != CutFadeMs {
			t.Errorf("每个剪点 30ms 声音淡入淡出：%+v", c)
		}
	}
	for _, tr := range r.Content.Tracks {
		if tr.Kind == tl.TrackVideo && tr.Role != RoleVoice {
			t.Fatal("口播的画面轨标人声（闪避按它压配乐）")
		}
	}
	var subs []string
	for _, s := range r.Content.Subtitles {
		p := lay.Subtitles[s.ID]
		if p.Hidden {
			t.Fatalf("字幕不该被隐藏：%+v", s)
		}
		subs = append(subs, s.Text)
	}
	if strings.Join(subs, "|") != "大家好|今天介绍|这款产品" {
		t.Fatalf("字幕跟随片段，去掉口癖：%v", subs)
	}
	if !strings.Contains(r.Summary, "去掉口癖 1 处") || !containsText(r.Notes, "删掉了 s3：重复") {
		t.Fatalf("摘要与说明：%s %v", r.Summary, r.Notes)
	}
}

// TestCompileTalk_能量校正不进字里：剪点在 ±60ms 内取波形最低处，但窗口夹在相邻两个字之间。
func TestCompileTalk_能量校正不进字里(t *testing.T) {
	card := videoCard("1", 10000)
	in := PlanInput{Mode: ModeTalk, Base: testBase(), Cards: []Card{card}, Spec: Spec{WithSubtitles: true},
		Transcripts: map[string]Transcript{"1": {Utterances: []Utterance{
			utter(chars("大家好", 500, 200)),                          // 500~1100
			utter([]Word{{Text: "我", StartMs: 1180, EndMs: 1380}}), // 下一句紧跟着：停顿只有 80ms
		}}}}
	vals := make([]byte, 1000)
	for i := range vals {
		vals[i] = 120
	}
	// 起点：外扩到 300，±60 的窗口 [240, 360] 里 340 最安静。
	vals[34] = 3
	// 终点：停顿 80ms → 外扩 40 到 1140，±60 是 [1080, 1200]；但 1080 在「好」里面、1180 起是「我」，
	// 窗口夹成 [1100, 1180]：最安静的 1080 不能用，取窗口里的 1170。
	vals[108] = 1
	vals[117] = 4
	in.Peaks = map[string]Peaks{"1": {IntervalMs: 10, Values: vals}}
	r, err := Compile(in, Draft{Mode: ModeTalk, Keep: []SentenceRange{{From: "s1", To: "s1"}}})
	if err != nil {
		t.Fatal(err)
	}
	mustValid(t, r)
	c := video(r)[0]
	if c.SourceInMs != 340 {
		t.Fatalf("起点校正到最安静处：%d", c.SourceInMs)
	}
	if c.SourceOutMs != 1170 {
		t.Fatalf("终点不进字里、取窗口里最安静处：%d", c.SourceOutMs)
	}
}

func TestCutPad_外扩取停顿的一半(t *testing.T) {
	cases := map[int]int{0: 0, 20: 20, 50: 30, 100: 50, 300: 150, 1000: 200, -5: 0}
	for gap, want := range cases {
		if got := cutPad(gap); got != want {
			t.Errorf("停顿 %d：期望外扩 %d，实际 %d", gap, want, got)
		}
	}
}

func TestTalkCuts_合并与长停顿(t *testing.T) {
	card := videoCard("1", 6000)
	// 两个字之间停顿 650ms（> 600，剪掉）；再两个字之间 100ms（外扩之后间隔 < 120，合并）。
	words := []talkWord{
		{Word: Word{Text: "一", StartMs: 100, EndMs: 300}, keep: true},
		{Word: Word{Text: "二", StartMs: 950, EndMs: 1150}, keep: true},
		{Word: Word{Text: "嗯", StartMs: 1200, EndMs: 1250}, keep: false, filler: true},
		{Word: Word{Text: "三", StartMs: 1300, EndMs: 1500}, keep: true},
	}
	ivs, pauses := talkCuts(card, words, Peaks{}, 0, card.DurationMs)
	if pauses != 1 {
		t.Fatalf("剪掉 1 处长停顿：%d", pauses)
	}
	// 一 [50, 500]（后面停顿 650 → 外扩 200）；二 [750, 1180]（后面是去掉的「嗯」，停顿 50 → 外扩 30）、
	// 三 [1270, 1700]：两段间隔 90 < 120 → 合并成 [750, 1700]
	if len(ivs) != 2 || ivs[0].start != 50 || ivs[0].end != 500 || ivs[1].start != 750 || ivs[1].end != 1700 {
		t.Fatalf("%+v", ivs)
	}
}

func TestFiller_弱口癖看位置(t *testing.T) {
	in := talkFixture()
	in.Transcripts["1"] = Transcript{Utterances: []Utterance{
		utter([]Word{{Text: "啊", StartMs: 100, EndMs: 300}}, chars("我们开始", 500, 200)),
		utter(chars("好啊", 2000, 200)),
	}}
	sents := TalkSentences(in)
	words := talkWords(sents, 0, []bool{true, true}, false)
	var removed []string
	for _, w := range words {
		if !w.keep {
			removed = append(removed, w.Text)
		}
	}
	if strings.Join(removed, "") != "啊" {
		t.Fatalf("句首的「啊」是口癖，「好啊」的不是：去掉了 %v", removed)
	}
	words = talkWords(sents, 0, []bool{true, true}, true)
	for _, w := range words {
		if !w.keep {
			t.Fatal("保留口癖时一个都不去")
		}
	}
}

// ---------------------------------------------------------------- 解说

// narrFixture 一段 4 秒的配音（TTS 逐字读原稿，每字 200ms，从 0 起），两段画面素材。
func narrFixture(script string) PlanInput {
	voice := Card{No: "1", Kind: InputAudio, MediaID: 91, DurationMs: 4000, AudioRole: RoleVoice, SourceNodeID: "nv"}
	var words []Word
	at := 0
	for _, r := range script {
		if hasSpoken(string(r)) {
			words = append(words, Word{Text: string(r), StartMs: at, EndMs: at + 200})
			at += 200
		}
	}
	return PlanInput{Mode: ModeNarr, Base: testBase(), Script: script,
		Cards:       []Card{voice, videoCard("2", 5000), videoCard("3", 5000)},
		Transcripts: map[string]Transcript{"1": {Language: "zh", Utterances: []Utterance{{Text: script, StartMs: 0, EndMs: at, Words: words}}}},
		Spec:        Spec{WithSubtitles: true}}
}

func TestAlignUnits_插入删除替换(t *testing.T) {
	asr := func(s string) []unit {
		var out []unit
		for i, tk := range tokenize(s) {
			out = append(out, unit{text: tk, start: i * 100, end: i*100 + 100})
		}
		return out
	}
	cases := []struct {
		script, heard string
		want          []int
	}{
		{"今天天气好", "今天天气好", []int{0, 1, 2, 3, 4}},
		{"今天天气好", "今天天气很好", []int{0, 1, 2, 3, 5}},     // 识别多了一个字
		{"今天天气很好", "今天天气好", []int{0, 1, 2, 3, -1, 4}}, // 识别少了一个字
		{"今天天气好", "今天田气好", []int{0, 1, 2, 3, 4}},      // 替换：时间照样用
		{"hello world", "Hello, World!", []int{0, 1}},
	}
	for _, c := range cases {
		got := alignUnits(tokenize(c.script), asr(c.heard), nil)
		if strings.Join(intsToStrings(got), ",") != strings.Join(intsToStrings(c.want), ",") {
			t.Errorf("%s / %s：期望 %v，实际 %v", c.script, c.heard, c.want, got)
		}
	}
}

func TestNarrSentences_数字与标点差异(t *testing.T) {
	// 原稿写阿拉伯数字，配音念成汉字；识别结果没有标点。
	in := narrFixture("2024年我们出发。路上很顺利！")
	tr := in.Transcripts["1"]
	words := chars("二零二四年我们出发", 0, 200)
	words = append(words, chars("路上很顺利", 2000, 200)...)
	tr.Utterances = []Utterance{{Text: "二零二四年我们出发路上很顺利", StartMs: 0, EndMs: 3000, Words: words}}
	in.Transcripts["1"] = tr
	sents := NarrSentences(in)
	if len(sents) != 2 || sents[0].Text != "2024年我们出发。" || sents[1].Text != "路上很顺利！" {
		t.Fatalf("按原稿断句、用原稿文字：%+v", sents)
	}
	if sents[0].StartMs != 0 || sents[1].StartMs != 2000 || sents[1].EndMs != 3000 {
		t.Fatalf("时间取识别：%+v", sents)
	}
}

// TestCompileNarr_总长等于配音、字幕与原稿逐字一致。
func TestCompileNarr_总长等于配音字幕与原稿一致(t *testing.T) {
	script := "第一句话，很长的一句解说词需要在逗号处断开才放得下。第二句！"
	in := narrFixture(script)
	in.Cards[0].DurationMs = 6000
	sents := NarrSentences(in)
	d := Draft{Mode: ModeNarr, Lines: []NarrLine{
		{Sentence: "s1", Clips: []string{"2", "3"}, Take: Take{Kind: TakeHead}},
		{Sentence: "s2", Clips: []string{"3"}, Take: Take{Kind: TakeTail}},
	}}
	r, err := Compile(in, d)
	if err != nil {
		t.Fatal(err)
	}
	mustValid(t, r)
	if r.TotalMs != 6000 {
		t.Fatalf("总长等于配音时长：%d", r.TotalMs)
	}
	var text strings.Builder
	for _, s := range r.Content.Subtitles {
		text.WriteString(s.Text)
	}
	if text.String() != strings.Join([]string{sents[0].Text, sents[1].Text}, "") {
		t.Fatalf("字幕与原稿逐字一致：%q", text.String())
	}
	if len(r.Content.Subtitles) < 3 {
		t.Fatalf("长句在逗号处断开：%d 条", len(r.Content.Subtitles))
	}
	voice := audioClips(r, RoleVoice)
	if len(voice) != 1 || voice[0].SourceOutMs != 6000 {
		t.Fatalf("配音放上人声轨：%+v", voice)
	}
	for _, s := range r.Content.Subtitles {
		if id, _ := anchorClipID(s.Anchor); id != "tmp-voice-1" {
			t.Fatalf("字幕跟随配音片段：%s", s.Anchor)
		}
	}
}

func TestCompileNarr_画面不够长时结转并说明(t *testing.T) {
	in := narrFixture("第一句话。第二句话！")
	in.Cards[0].DurationMs = 8000
	in.Cards = []Card{in.Cards[0], videoCard("2", 2000), videoCard("3", 3000)}
	d := Draft{Mode: ModeNarr, Lines: []NarrLine{
		{Sentence: "s1", Clips: []string{"2"}, Take: Take{Kind: TakeFull}},
		{Sentence: "s2", Clips: []string{"3"}, Take: Take{Kind: TakeFull}},
	}}
	r, err := Compile(in, d)
	if err != nil {
		t.Fatal(err)
	}
	mustValid(t, r)
	if r.TotalMs != 5000 || !containsText(r.Warnings, "画面共 5 秒，不足配音 8 秒") {
		t.Fatalf("如实说明：%d %v", r.TotalMs, r.Warnings)
	}
	in.Spec.AllowRepeat = true
	r, err = Compile(in, d)
	if err != nil {
		t.Fatal(err)
	}
	mustValid(t, r)
	if r.TotalMs != 8000 || !containsText(r.Warnings, "重复用了编号") {
		t.Fatalf("允许重复时铺满：%d %v", r.TotalMs, r.Warnings)
	}
}

// TestNarrSentences_漏念的句子不出字幕并点名：配音漏念了第二句 —— 它在配音里没有时间，给它出字幕就是让字幕说一句
// 声音里没有的话；不出这一句、在警告里说清是原稿第几句（SCT-35：对齐失败不能静默）。
func TestNarrSentences_漏念的句子不出字幕并点名(t *testing.T) {
	in := narrFixture("第一句。第二句。第三句。")
	words := append(chars("第一句", 0, 200), chars("第三句", 1500, 200)...)
	in.Transcripts["1"] = Transcript{Utterances: []Utterance{{Text: "第一句第三句", StartMs: 0, EndMs: 2100, Words: words}}}
	sents, warns := narrSentences(in)
	var got []string
	for _, s := range sents {
		got = append(got, fmt.Sprintf("%s:%s:%d-%d", s.Ref, s.Text, s.StartMs, s.EndMs))
	}
	if strings.Join(got, " ") != "s1:第一句。:0-600 s2:第三句。:1500-2100" {
		t.Fatalf("%v", got)
	}
	if len(warns) != 1 || warns[0] != "解说稿第 2 句「第二句」在配音里没有找到（可能漏念了），没有出字幕" {
		t.Fatalf("点名漏念的那句：%q", warns)
	}
}

// TestNarrSentences_没有时间时按字数均分：识别结果里一个字都没有（只给了整段文字的那类结果）时，按字数在整段配音里分。
func TestNarrSentences_没有时间时按字数均分(t *testing.T) {
	in := narrFixture("一二三。四五六七八九。")
	in.Cards[0].DurationMs = 9000
	in.Transcripts["1"] = Transcript{Granularity: "text"}
	sents := NarrSentences(in)
	if len(sents) != 2 || sents[0].StartMs != 0 || sents[0].EndMs != 3000 || sents[1].StartMs != 3000 || sents[1].EndMs != 9000 {
		t.Fatalf("%+v", sents)
	}
}

// TestNarrSentences_漏念第一句：反过来的等价对法同样不能把第二句拆开（第一遍可能把「第一句」的「第」对到「第二句」上，
// 第二遍只拿对得上的句子再对齐，第二句拿回自己的字）。
func TestNarrSentences_漏念第一句(t *testing.T) {
	in := narrFixture("第一句。第二句。")
	in.Transcripts["1"] = Transcript{Utterances: []Utterance{{Text: "第二句", StartMs: 1000, EndMs: 1600, Words: chars("第二句", 1000, 200)}}}
	sents, warns := narrSentences(in)
	if len(sents) != 1 || sents[0].Text != "第二句。" || sents[0].StartMs != 1000 || sents[0].EndMs != 1600 {
		t.Fatalf("%+v", sents)
	}
	if len(warns) != 1 || !strings.HasPrefix(warns[0], "解说稿第 1 句「第一句」在配音里没有找到") {
		t.Fatalf("%q", warns)
	}
}

// SCT-34：识别稿按时间线用到的那几段裁剪与编号 —— 段外的字不进稿子，跨段界的句子按段切开，空句丢掉；
// 同一份素材分两段用、中间夹着另一份时，编号与剪出来的段都按成片顺序。
func TestTalk_识别稿按片段区间裁剪与编号(t *testing.T) {
	in := PlanInput{Mode: ModeTalk, Cards: []Card{
		{No: "1", Kind: InputVideo, MediaID: 11, DurationMs: 10000},
		{No: "2", Kind: InputVideo, MediaID: 12, DurationMs: 3000},
	}, Transcripts: map[string]Transcript{
		"1": {Granularity: "char", Utterances: []Utterance{
			utter(chars("开头被剪掉", 0, 150)),     // 0~750：整句在第一段之前 → 丢掉
			utter(chars("第一句话", 1000, 200)),   // 1000~1800
			utter(chars("跨过段界的话", 4400, 200)), // 4400~5600：只留 5000 之前的「跨过段」
			utter(chars("后面一句", 7200, 200)),   // 7200~8000：在第三段
		}},
		"2": {Granularity: "char", Utterances: []Utterance{utter(chars("另一份", 100, 200))}},
	}, Spec: Spec{TalkSpans: []TalkSpan{{MediaID: 11, InMs: 1000, OutMs: 5000}, {MediaID: 12, InMs: 0, OutMs: 3000},
		{MediaID: 11, InMs: 7000, OutMs: 9000}, {MediaID: 99, InMs: 0, OutMs: 1000}}}}
	sents := TalkSentences(in)
	var got []string
	for _, s := range sents {
		got = append(got, fmt.Sprintf("%s:%s:%s:%d-%d", s.Ref, s.CardNo, s.Text, s.StartMs, s.EndMs))
	}
	want := []string{"s1:1:第一句话:1000-1800", "s2:1:跨过段:4400-5000", "s3:2:另一份:100-700", "s4:1:后面一句:7200-8000"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("编号与裁剪:\n got %v\nwant %v", got, want)
	}

	res, err := compileTalk(in, Draft{Mode: ModeTalk, Keep: []SentenceRange{{From: "s1", To: "s4"}}}, false)
	if err != nil {
		t.Fatal(err)
	}
	var video []string
	for _, tr := range res.Content.Tracks {
		if tr.Kind == tl.TrackVideo {
			for _, c := range tr.Clips {
				video = append(video, fmt.Sprintf("%d:%d-%d", c.MediaID, c.SourceInMs, c.SourceOutMs))
			}
		}
	}
	// 外扩不越过段界：第一段的剪点从 1000 起（不会扩回被剪掉的开头）、到 5000 止（段里两句之间停顿 2.6 秒，
	// 剪成两截）；按成片顺序 A、A、B、A —— 同一份素材分两段用时不会被并到一起。
	if fmt.Sprint(video) != "[11:1000-2000 11:4200-5000 12:50-900 11:7100-8200]" {
		t.Fatalf("剪出来的段: %v", video)
	}
}

func TestTalk_长稿分段不切句(t *testing.T) {
	mk := func(n int) Sentence { return Sentence{Text: strings.Repeat("字", n)} }
	chunks := TalkChunks([]Sentence{mk(1200), mk(1500), mk(400), mk(3500), mk(10)}, 3000)
	sizes := make([]int, len(chunks))
	for i, c := range chunks {
		sizes[i] = len(c)
	}
	// 1200+1500 ≤ 3000；再加 400 超了 → 新段；3500 一句就超 → 自成一段；最后 10 字另起。
	if fmt.Sprint(sizes) != "[2 1 1 1]" {
		t.Fatalf("分段: %v", sizes)
	}
	if got := TalkChunks(nil, 0); got != nil {
		t.Fatalf("空稿不分段: %v", got)
	}
}

// SCT-34：保留句里面的口癖也剪掉 —— 句中的「嗯」把一句切成两段，两段都不盖住它；选了保留口癖时整句一段。
func TestTalk_口癖在保留句内也剪掉(t *testing.T) {
	in := talkFixture()
	in.Transcripts["1"] = Transcript{Granularity: "char", Utterances: []Utterance{
		utter(chars("这个", 500, 200), []Word{{Text: "嗯", StartMs: 1100, EndMs: 1400}}, chars("产品很好", 1600, 200)),
	}}
	keep := Draft{Mode: ModeTalk, Keep: []SentenceRange{{From: "s1", To: "s1"}}}
	r, err := Compile(in, keep)
	if err != nil {
		t.Fatal(err)
	}
	mustValid(t, r)
	var got []string
	for _, c := range video(r) {
		if c.SourceInMs < 1400 && c.SourceOutMs > 1100 {
			t.Fatalf("剪出来的段盖住了口癖 [1100,1400]：%+v", c)
		}
		got = append(got, fmt.Sprintf("%d-%d", c.SourceInMs, c.SourceOutMs))
	}
	// 这个 [500,900]：前面没有字 → 外扩 200、后面停顿 200 → 外扩 100；产品很好 [1600,2400] 同理。
	if strings.Join(got, " ") != "300-1000 1500-2600" || !strings.Contains(r.Summary, "去掉口癖 1 处") {
		t.Fatalf("剪点 %v，摘要 %s", got, r.Summary)
	}

	in.Spec.KeepFillers = true
	r, err = Compile(in, keep)
	if err != nil {
		t.Fatal(err)
	}
	if c := video(r); len(c) != 1 || c[0].SourceInMs != 300 || c[0].SourceOutMs != 2600 || strings.Contains(r.Summary, "口癖") {
		t.Fatalf("保留口癖时整句一段：%+v %s", c, r.Summary)
	}
}

// SCT-34（SC-F-100）：自动字幕每条带锚点、指向自己所在的片段，按保留下来的字取起止；字幕轨语言取识别语言。
// 裁掉片段开头之后，被裁区间里的字幕隐藏、后面的字幕跟着前移（与时间线读取同一个 Layout）。
func TestTalk_自动字幕带锚点(t *testing.T) {
	in := talkFixture()
	in.Base.Tracks = []tl.ContentTrack{{ID: "1001", Kind: tl.TrackVideo}}
	d := Draft{Mode: ModeTalk, Keep: []SentenceRange{{From: "s1", To: "s2"}, {From: "s4", To: "s4"}}}
	r, err := Compile(in, d)
	if err != nil {
		t.Fatal(err)
	}
	lay := mustValid(t, r)
	clips := video(r)
	type want struct {
		text       string
		clip       int
		start, end int
	}
	// 「嗯」不进字幕：第二条从「今」（2100）起。
	wants := []want{{"大家好", 0, 500, 1100}, {"今天介绍", 1, 2100, 2900}, {"这款产品", 2, 5200, 6000}}
	if len(r.Content.Subtitles) != len(wants) {
		t.Fatalf("字幕条数：%+v", r.Content.Subtitles)
	}
	before := map[string]int{}
	for i, s := range r.Content.Subtitles {
		// 整份内容里的锚点指向临时片段 ID（tmp-v1…），保存时换成正式 ID。
		var a struct {
			ClipID     string `json:"clipId"`
			SrcStartMs int    `json:"srcStartMs"`
			SrcEndMs   int    `json:"srcEndMs"`
		}
		err := json.Unmarshal(s.Anchor, &a)
		w := wants[i]
		if err != nil || s.Text != w.text || a.ClipID != clips[w.clip].ID || a.SrcStartMs != w.start || a.SrcEndMs != w.end {
			t.Fatalf("第 %d 条：%+v（锚点 %+v）", i+1, s, a)
		}
		before[s.Text] = lay.Subtitles[s.ID].StartMs
	}
	var lang string
	for _, tr := range r.Content.Tracks {
		if tr.Kind == tl.TrackSubtitle {
			lang = tr.Lang
		}
	}
	if lang != "zh-CN" {
		t.Fatalf("字幕轨语言取识别语言（zh → zh-CN）：%q", lang)
	}

	// 裁掉第一段开头 600ms：「大家好」只剩三分之一露在外面 → 隐藏；后面两条整体前移 600。
	for i := range r.Content.Tracks {
		if r.Content.Tracks[i].Kind == tl.TrackVideo {
			r.Content.Tracks[i].Clips[0].SourceInMs += 600
		}
	}
	after := tl.Layout(r.Content)
	for _, s := range r.Content.Subtitles {
		p := after.Subtitles[s.ID]
		switch s.Text {
		case "大家好":
			if !p.Hidden {
				t.Fatalf("被裁区间里的字幕要隐藏：%+v", p)
			}
		default:
			if p.Hidden || before[s.Text]-p.StartMs != 600 {
				t.Fatalf("「%s」要跟着前移 600：前 %d 后 %+v", s.Text, before[s.Text], p)
			}
		}
	}

	// 模型说不要字幕 / 请求关了字幕：都不生成。
	off := false
	d.Subtitle = &off
	if r, _ := Compile(in, d); len(r.Content.Subtitles) != 0 {
		t.Fatalf("subtitle=false 不生成字幕：%+v", r.Content.Subtitles)
	}
	d.Subtitle = nil
	in.Spec.WithSubtitles = false
	if r, _ := Compile(in, d); len(r.Content.Subtitles) != 0 {
		t.Fatalf("关了字幕不生成：%+v", r.Content.Subtitles)
	}
}

// SCT-34：模型两次都不合规时的兜底 —— 一句不删，只去掉口癖与超过 600ms 的停顿，并写明剪了几处；
// 一处停顿都没有时也要说出来。
func TestTalk_两次不合规走去停顿兜底(t *testing.T) {
	r, err := Fallback(talkFixture())
	if err != nil {
		t.Fatal(err)
	}
	mustValid(t, r)
	if !r.UsedFallback || len(r.Notes) == 0 || r.Notes[0] != FallbackNote {
		t.Fatalf("兜底说明：%+v", r.Notes)
	}
	// s2→s3 停顿 700、s3→s4 停顿 800：两处长停顿；s2 开头的「嗯」去掉；重复的 s3 也保留（兜底不删句）。
	for _, want := range []string{"保留了 4 句（共 4 句）", "去掉口癖 1 处", "剪掉长停顿 2 处"} {
		if !strings.Contains(r.Summary, want) {
			t.Fatalf("摘要缺「%s」：%s", want, r.Summary)
		}
	}
	if n := len(video(r)); n != 4 {
		t.Fatalf("四句各一段（中间的长停顿剪掉）：%d", n)
	}

	in := talkFixture()
	in.Transcripts["1"] = Transcript{Granularity: "char", Utterances: []Utterance{utter(chars("一口气说完", 500, 200))}}
	r, err = Fallback(in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.Summary, "没有超过 0.6 秒的长停顿") {
		t.Fatalf("没剪到停顿也要说：%s", r.Summary)
	}
}

// SCT-34：知道字幕语言时（口播取识别语言），放进语言对得上的字幕轨；没有就用还没定语言的那条并填上语言，
// 不把中文字幕塞进英文轨；别的轨上的字幕不动。
func TestTalk_字幕放进语言对得上的字幕轨(t *testing.T) {
	d := Draft{Mode: ModeTalk, Keep: []SentenceRange{{From: "s1", To: "s1"}}}
	subTracks := func(r Result) map[string]string {
		out := map[string]string{}
		for _, tr := range r.Content.Tracks {
			if tr.Kind == tl.TrackSubtitle {
				out[tr.ID] = tr.Lang
			}
		}
		return out
	}
	trackOf := func(r Result, text string) string {
		for _, s := range r.Content.Subtitles {
			if s.Text == text {
				return s.TrackID
			}
		}
		return ""
	}
	in := talkFixture()
	in.Base.Tracks = []tl.ContentTrack{{ID: "1001", Kind: tl.TrackVideo}, {ID: "1003", Kind: tl.TrackSubtitle, Lang: "en"},
		{ID: "1005", Kind: tl.TrackSubtitle}}
	in.Base.Subtitles = []tl.ContentSubtitle{{ID: "9", TrackID: "1003", StartMs: 0, EndMs: 500, Text: "Hello"}}
	r, err := Compile(in, d)
	if err != nil {
		t.Fatal(err)
	}
	mustValid(t, r)
	if got := subTracks(r); got["1003"] != "en" || got["1005"] != "zh-CN" || trackOf(r, "大家好") != "1005" || trackOf(r, "Hello") != "1003" {
		t.Fatalf("放进没定语言的那条并填上 zh-CN、英文轨不动：%v %+v", got, r.Content.Subtitles)
	}

	in.Base.Tracks[2].Lang = "zh-CN"
	in.Base.Tracks[1], in.Base.Tracks[2] = in.Base.Tracks[2], in.Base.Tracks[1]
	r, _ = Compile(in, d)
	if trackOf(r, "大家好") != "1005" || trackOf(r, "Hello") != "1003" {
		t.Fatalf("语言对得上的那条：%+v", r.Content.Subtitles)
	}

	// 识别结果没给语言（与混剪、分镜一样语言未知）：照旧放第一条字幕轨，不改它的语言。
	in.Base.Tracks = []tl.ContentTrack{{ID: "1001", Kind: tl.TrackVideo}, {ID: "1003", Kind: tl.TrackSubtitle, Lang: "en"}}
	in.Transcripts["1"] = Transcript{Granularity: "char", Utterances: in.Transcripts["1"].Utterances}
	r, _ = Compile(in, d)
	if got := subTracks(r); len(got) != 1 || got["1003"] != "en" || trackOf(r, "大家好") != "1003" {
		t.Fatalf("语言未知时放第一条字幕轨：%v", got)
	}
}

// SCT-34：长稿分段时的一段一句不留是合法的（Refs.Partial），但要是模型明确作的决定；不分段时仍不合规。
func TestParseDraft_分段里的一段可以一句不留(t *testing.T) {
	refs := Refs{Sentences: []string{"s3", "s4"}}
	part := refs
	part.Partial = true
	for _, c := range []struct {
		raw     string
		partial bool
		ok      bool
	}{
		{`{"keep":[],"drop":[{"sentence":"s3","reason":"重录"},{"sentence":"s4"}]}`, true, true},
		{`{"keep":[]}`, true, true},
		{`{"drop":["s3","s4"]}`, true, true},
		{`{"subtitle":true}`, true, false},                  // 什么决定都没写
		{`{"keep":[{"from":"s1","to":"s1"}]}`, true, false}, // 引用了别的段的句子：要回灌修
		{`{"keep":[],"drop":["s3","s4"]}`, false, false},    // 不分段时一句不留不合规
	} {
		r := refs
		if c.partial {
			r = part
		}
		res, err := ParseDraft(ModeTalk, c.raw, r)
		if (err == nil) != c.ok {
			t.Errorf("%s（partial=%v）：期望 ok=%v，实际 %v %+v", c.raw, c.partial, c.ok, err, res.Dropped)
		}
		if err == nil && len(res.Draft.Keep) != 0 {
			t.Errorf("%s：这一段一句不留：%+v", c.raw, res.Draft.Keep)
		}
	}
}

// SCT-35：同音错字与漏字 —— 对上的字过半就是这一句，字幕照原稿出（标点按原稿），时间取识别；不报警告。
func TestAlign_同音错字与漏字(t *testing.T) {
	in := narrFixture("今天天气很好，我们去公园散步。")
	// 「气」听成「汽」，「步」没识别出来。
	words := append(chars("今天天汽很好", 0, 200), chars("我们去公园散", 1500, 200)...)
	in.Transcripts["1"] = Transcript{Utterances: []Utterance{{Text: "今天天汽很好我们去公园散", StartMs: 0, EndMs: 2700, Words: words}}}
	sents, warns := narrSentences(in)
	if len(warns) != 0 || len(sents) != 1 || sents[0].Text != "今天天气很好，我们去公园散步。" {
		t.Fatalf("%+v %q", sents, warns)
	}
	if sents[0].StartMs != 0 || sents[0].EndMs != 2700 {
		t.Fatalf("时间取识别（到最后一个识别出的字为止）：%+v", sents[0])
	}
}

// SCT-35：配音里多出的语气词不打断对齐：夹在两句之间的归时间上近的一句、句中的留在那一句里，开头的归第一句。
func TestAlign_多出的语气词(t *testing.T) {
	in := narrFixture("大家好。今天讲剪辑。")
	var words []Word
	words = append(words, Word{Text: "嗯", StartMs: 0, EndMs: 300})
	words = append(words, chars("大家好", 400, 200)...)                   // 400~1000
	words = append(words, Word{Text: "啊", StartMs: 1050, EndMs: 1250}) // 紧跟着「好」
	words = append(words, chars("今天", 2000, 200)...)                   // 2000~2400
	words = append(words, Word{Text: "那个", StartMs: 2500, EndMs: 2900})
	words = append(words, chars("讲剪辑", 3000, 200)...) // 3000~3600
	in.Transcripts["1"] = Transcript{Utterances: []Utterance{{Text: "嗯大家好啊今天那个讲剪辑", StartMs: 0, EndMs: 3600, Words: words}}}
	sents, warns := narrSentences(in)
	var got []string
	for _, s := range sents {
		got = append(got, fmt.Sprintf("%s:%d-%d", s.Text, s.StartMs, s.EndMs))
	}
	if len(warns) != 0 || strings.Join(got, " ") != "大家好。:0-1250 今天讲剪辑。:2000-3600" {
		t.Fatalf("%v %q", got, warns)
	}
}

// SCT-35：对不上的句子退回识别文字并点名 —— 配音在那个位置说的是另一句话，字幕改用识别出来的那句（时间也取它）；
// 整份原稿都对不上时（连错了文本）字幕全用识别文字，只报一条。
func TestAlign_对不上的句子退回识别文字(t *testing.T) {
	in := narrFixture("第一句话。完全不同的内容。第三句话。")
	in.Transcripts["1"] = Transcript{Utterances: []Utterance{
		{Text: "第一句话", StartMs: 0, EndMs: 800, Words: chars("第一句话", 0, 200)},
		{Text: "我们换个说法吧", StartMs: 1000, EndMs: 2400, Words: chars("我们换个说法吧", 1000, 200)},
		{Text: "第三句话", StartMs: 2600, EndMs: 3400, Words: chars("第三句话", 2600, 200)},
	}}
	sents, warns := narrSentences(in)
	var got []string
	for _, s := range sents {
		got = append(got, fmt.Sprintf("%s:%s:%d-%d", s.Ref, s.Text, s.StartMs, s.EndMs))
	}
	if strings.Join(got, " ") != "s1:第一句话。:0-800 s2:我们换个说法吧:1000-2400 s3:第三句话。:2600-3400" {
		t.Fatalf("%v", got)
	}
	if len(warns) != 1 || warns[0] != "解说稿第 2 句「完全不同的内容」和配音对不上，这段字幕用了识别出来的文字「我们换个说法吧」" {
		t.Fatalf("%q", warns)
	}

	// 长度差不多、逐字「替换」也能对上的两句话：仍按对不上处理（看逐字相同的比例，不看对齐上的位置）。
	in.Script = "今天天气很好。"
	in.Transcripts["1"] = Transcript{Utterances: []Utterance{{Text: "我们开始上课", StartMs: 0, EndMs: 1200, Words: chars("我们开始上课", 0, 200)}}}
	sents, warns = narrSentences(in)
	if len(sents) != 1 || sents[0].Text != "我们开始上课" || len(warns) != 1 || !strings.Contains(warns[0], "解说稿和配音对不上") {
		t.Fatalf("整份对不上：%+v %q", sents, warns)
	}
}

// SCT-35：几条配音按编号顺序首尾相接 —— 成片长度是各条之和，人声轨上各条依次排开，字幕跟随它所在的那一条
// （锚点用那一条自己的时间），句子记着出自哪一条。
func TestCompileNarr_多条配音首尾相接(t *testing.T) {
	in := narrFixture("第一条配音。第二条配音。")
	second := Card{No: "4", Kind: InputAudio, MediaID: 94, DurationMs: 2000, AudioRole: RoleVoice, SourceNodeID: "nv2"}
	in.Cards[0].DurationMs = 3000
	in.Cards = append(in.Cards, second)
	in.Transcripts["1"] = Transcript{Language: "zh", Utterances: []Utterance{{Text: "第一条配音", StartMs: 0, EndMs: 1000, Words: chars("第一条配音", 0, 200)}}}
	in.Transcripts["4"] = Transcript{Utterances: []Utterance{{Text: "第二条配音", StartMs: 500, EndMs: 1500, Words: chars("第二条配音", 500, 200)}}}
	sents, warns := narrSentences(in)
	if len(warns) != 0 || len(sents) != 2 || sents[1].StartMs != 3500 || sents[1].EndMs != 4500 || sents[0].CardNo != "1" || sents[1].CardNo != "4" {
		t.Fatalf("第二条的时间换算成成片时间：%+v %q", sents, warns)
	}
	r, err := Compile(in, Draft{Mode: ModeNarr, Lines: []NarrLine{
		{Sentence: "s1", Clips: []string{"2"}, Take: Take{Kind: TakeHead}},
		{Sentence: "s2", Clips: []string{"3"}, Take: Take{Kind: TakeHead}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	lay := mustValid(t, r)
	if r.TotalMs != 5000 {
		t.Fatalf("成片长度 = 两条配音之和：%d", r.TotalMs)
	}
	voices := audioClips(r, RoleVoice)
	if len(voices) != 2 || *voices[0].StartMs != 0 || *voices[1].StartMs != 3000 || voices[1].MediaID != 94 || voices[1].SourceOutMs != 2000 {
		t.Fatalf("人声轨上依次排开：%+v", voices)
	}
	var subs []string
	for _, s := range r.Content.Subtitles {
		id, _ := anchorClipID(s.Anchor)
		p := lay.Subtitles[s.ID]
		subs = append(subs, fmt.Sprintf("%s@%s:%d-%d", s.Text, id, p.StartMs, p.EndMs))
	}
	if strings.Join(subs, " ") != "第一条配音。@tmp-voice-1:0-1000 第二条配音。@tmp-voice-2:3500-4500" {
		t.Fatalf("字幕跟随所在的那一条：%v", subs)
	}
	// 某一条配音没有识别结果：说清是哪一条。
	delete(in.Transcripts, "4")
	if _, err := Compile(in, Draft{Mode: ModeNarr}); err == nil || !strings.Contains(err.Error(), "编号 4") {
		t.Fatalf("缺识别结果的那一条要点名：%v", err)
	}
}

// SCT-35：配音的原稿按那次文字转语音拼给上游的顺序还原 —— 按连线顺序的文本输入，再加节点自己的提示词；
// 别的输入（参考音色之类）与空白的文本不算。
func TestSpokenScript_与上游拼接顺序一致(t *testing.T) {
	got := SpokenScript([]ScriptPart{
		{Kind: "text", Order: 2, Text: "第二段。"},
		{Kind: "audio", Order: 3},
		{Kind: "text", Order: 1, Text: "第一段。"},
		{Kind: "text", Order: 4, Text: "  "},
	}, " 结尾一句。 ")
	if got != "第一段。\n第二段。\n结尾一句。" {
		t.Fatalf("%q", got)
	}
	if SpokenScript(nil, "") != "" {
		t.Fatal("什么都没有时为空")
	}
}

// SCT-35：上一句的最后一个字识别错（同音字）、紧接着是一句对不上的话 —— 那个错字归上一句，对不上的那句拿到
// 完整的识别文字。两种对法代价相同，没有「句子边界上多出来的话更便宜」这一条时，回溯会把上一句的结尾拉到多念那句的
// 末尾，那句的识别文字就没了着落（报成「没有找到」）。
func TestAlign_句尾同音字后面接一句对不上的(t *testing.T) {
	in := narrFixture("今天的节目。完全不同的内容。接下来是第二部分。")
	in.Cards[0].DurationMs = 6000
	in.Transcripts["1"] = Transcript{Utterances: []Utterance{
		{Text: "今天的节木", StartMs: 0, EndMs: 1000, Words: chars("今天的节木", 0, 200)},
		{Text: "我们先看第一个画面", StartMs: 1200, EndMs: 3000, Words: chars("我们先看第一个画面", 1200, 200)},
		{Text: "接下来是第二部分", StartMs: 3200, EndMs: 4800, Words: chars("接下来是第二部分", 3200, 200)},
	}}
	sents, warns := narrSentences(in)
	var got []string
	for _, s := range sents {
		got = append(got, fmt.Sprintf("%s:%d-%d", s.Text, s.StartMs, s.EndMs))
	}
	if strings.Join(got, " ") != "今天的节目。:0-1000 我们先看第一个画面:1200-3000 接下来是第二部分。:3200-4800" ||
		len(warns) != 1 || !strings.Contains(warns[0], "这段字幕用了识别出来的文字「我们先看第一个画面」") {
		t.Fatalf("%v %q", got, warns)
	}
	// 反过来：下一句的第一个字识别错。
	in.Script = "今天的节目。完全不同的内容。街下来是第二部分。"
	in.Transcripts["1"] = Transcript{Utterances: []Utterance{
		{Text: "今天的节目", StartMs: 0, EndMs: 1000, Words: chars("今天的节目", 0, 200)},
		{Text: "我们先看第一个画面", StartMs: 1200, EndMs: 3000, Words: chars("我们先看第一个画面", 1200, 200)},
		{Text: "接下来是第二部分", StartMs: 3200, EndMs: 4800, Words: chars("接下来是第二部分", 3200, 200)},
	}}
	sents, _ = narrSentences(in)
	got = nil
	for _, s := range sents {
		got = append(got, fmt.Sprintf("%s:%d-%d", s.Text, s.StartMs, s.EndMs))
	}
	if strings.Join(got, " ") != "今天的节目。:0-1000 我们先看第一个画面:1200-3000 街下来是第二部分。:3200-4800" {
		t.Fatalf("%v", got)
	}
}
