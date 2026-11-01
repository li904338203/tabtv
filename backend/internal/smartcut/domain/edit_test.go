package domain

import (
	"strings"
	"testing"

	tl "github.com/li904338203/tabtv/backend/internal/timeline/domain"
)

// editBase 四段：c1 用户改过的、c2~c4 是 AI 剪的；c3 上跟着一条铺满的字幕，c4 上跟着一条半段的字幕；一个文字层。
func editBase() (tl.Content, []Card) {
	cards := []Card{videoCard("1", 8000), videoCard("2", 8000), videoCard("3", 8000), videoCard("4", 8000), videoCard("5", 9000)}
	base := testBase()
	clip := func(id string, c Card, origin string, in, out int) tl.ContentClip {
		return tl.ContentClip{ID: id, MediaID: c.MediaID, SourceInMs: in, SourceOutMs: out, Origin: origin, SourceNodeID: c.SourceNodeID}
	}
	base.Tracks[0].Clips = []tl.ContentClip{
		clip("101", cards[0], tl.OriginUser, 0, 3000),
		clip("102", cards[1], tl.OriginAI, 1000, 4000),
		clip("103", cards[2], tl.OriginAI, 0, 4000),
		clip("104", cards[3], tl.OriginAI, 2000, 6000),
	}
	base.Subtitles = []tl.ContentSubtitle{
		{ID: "201", TrackID: "1003", StartMs: 6000, EndMs: 10000, Text: "第三段的字幕",
			Anchor: []byte(`{"clipId":"103","srcStartMs":0,"srcEndMs":4000}`)},
		{ID: "202", TrackID: "1003", StartMs: 10000, EndMs: 11000, Text: "第四段的半句",
			Anchor: []byte(`{"clipId":"104","srcStartMs":2000,"srcEndMs":3000}`)},
	}
	base.Texts = []tl.ContentText{{ID: "301", TrackID: "1004", StartMs: 0, EndMs: 2000, Text: "旧标题"}}
	return base, cards
}

func parseEdit(t *testing.T, base tl.Content, cards []Card, raw string) Draft {
	t.Helper()
	refs, texts := EditRefs(base, cards)
	clips, trefs := EditRefNames(refs, texts)
	pr, err := ParseDraft(ModeEdit, raw, Refs{Cards: cards, Clips: clips, Texts: trefs})
	if err != nil {
		t.Fatal(err)
	}
	if len(pr.Dropped) > 0 {
		t.Fatalf("方案不该有不合规的：%v", pr.Dropped)
	}
	return pr.Draft
}

func mustValidContent(t *testing.T, c tl.Content) tl.LayoutResult {
	t.Helper()
	return mustValid(t, Result{Content: c, TotalMs: tl.Layout(c).EffectiveMs})
}

func TestEditRefs_清单与来源(t *testing.T) {
	base, cards := editBase()
	refs, texts := EditRefs(base, cards)
	if len(refs) != 4 || refs[0].Ref != "c1" || refs[0].Origin != tl.OriginUser || refs[2].CardNo != "3" || refs[1].PlayMs != 3000 {
		t.Fatalf("%+v", refs)
	}
	if len(texts) != 1 || texts[0].Ref != "t1" || texts[0].Text != "旧标题" {
		t.Fatalf("%+v", texts)
	}
}

// TestApplyEdit_只改被提到的AI片段（SC-AC-44 编译部分）：「把第 3 段换短一点」只有第 3 段变短；
// 用户改过的第 1 段不动并说明；改动清单列出改了哪几处。
func TestApplyEdit_只改被提到的AI片段(t *testing.T) {
	base, cards := editBase()
	d := parseEdit(t, base, cards, `{"ops":[{"op":"trim","clip":"c3","take":"head","ratio":0.6},{"op":"trim","clip":"c1","take":"head","ratio":0.5}],"summary":"第 3 段改短"}`)
	r, err := ApplyEdit(base, d, cards)
	if err != nil {
		t.Fatal(err)
	}
	mustValidContent(t, r.Content)
	got := r.Content.Tracks[0].Clips
	if got[2].SourceInMs != 0 || got[2].SourceOutMs != 2400 {
		t.Fatalf("第 3 段取前 60%%：%+v", got[2])
	}
	for i, c := range got {
		if i == 2 {
			continue
		}
		o := base.Tracks[0].Clips[i]
		if c.SourceInMs != o.SourceInMs || c.SourceOutMs != o.SourceOutMs || c.Origin != o.Origin {
			t.Fatalf("其余片段不变：%+v → %+v", o, c)
		}
	}
	if strings.Join(r.Changes, "|") != "第 3 段：只取前 60%（4 秒 → 2.4 秒）" {
		t.Fatalf("改动清单：%v", r.Changes)
	}
	if !containsText(r.Warnings, "第 1 段是你改过的，没有动") {
		t.Fatalf("说明跳过了用户改过的：%v", r.Warnings)
	}
	if r.Summary != "第 3 段改短" {
		t.Fatalf("摘要用模型的：%s", r.Summary)
	}
	// 铺满第 3 段的字幕仍然跟着它（Layout 按新的取用区间换算），不被删
	if len(r.Content.Subtitles) != 2 {
		t.Fatalf("字幕不该少：%+v", r.Content.Subtitles)
	}
}

func TestApplyEdit_各种操作(t *testing.T) {
	base, cards := editBase()
	d := parseEdit(t, base, cards, `{"ops":[
		{"op":"swap","a":"c2","b":"c4"},
		{"op":"replace","clip":"c3","with":"5"},
		{"op":"speed","clip":"*","value":1.25},
		{"op":"transition","clip":"c2","kind":"dissolve"},
		{"op":"text","target":"t1","text":"新标题"},
		{"op":"text","text":"看这里","clip":"c3"},
		{"op":"insert","with":"5","after":"start","take":"tail"},
		{"op":"delete","clip":"c4"}]}`)
	r, err := ApplyEdit(base, d, cards)
	if err != nil {
		t.Fatal(err)
	}
	lay := mustValidContent(t, r.Content)
	got := r.Content.Tracks[0].Clips
	// 插在最前（素材 5 的后 40%）、c1（用户的，没动）、c2 原来的位置现在是 c4 —— 但 c4 被删了、c3 换成了 5、c2 换到了原 c4 的位置。
	if len(got) != 4 {
		t.Fatalf("段数：%+v", got)
	}
	if got[0].MediaID != cards[4].MediaID || got[0].SourceInMs != 5400 || got[0].SourceOutMs != 9000 || got[0].Origin != tl.OriginAI {
		t.Fatalf("插入在最前、取后 40%%：%+v", got[0])
	}
	if got[1].ID != "101" || got[1].Speed != nil {
		t.Fatalf("用户的 c1 不改速度：%+v", got[1])
	}
	if got[2].ID != "103" || got[2].MediaID != cards[4].MediaID || len(got[2].Alternates) != 1 ||
		got[2].Alternates[0].MediaID != cards[2].MediaID || got[2].Alternates[0].Kind != tl.AlternateOriginal {
		t.Fatalf("c3 换成素材 5，原素材进备选：%+v", got[2])
	}
	if got[3].ID != "102" || got[3].TransitionIn == nil || got[3].TransitionIn.Kind != tl.TransitionDissolve ||
		got[3].Speed == nil || *got[3].Speed != 125 {
		t.Fatalf("c2 换到原 c4 的位置、加了叠化与 1.25×：%+v", got[3])
	}
	want := []string{
		"第 2 段与第 4 段互换了位置",
		"第 3 段换成了编号 5",
		"3 段的速度改为 1.25×",
		"第 2 段的转场改成「叠化」",
		"文字「旧标题」改成「新标题」",
		"加了文字「看这里」",
		"在最前面插入了编号 5",
		"删掉了第 4 段",
	}
	if strings.Join(r.Changes, "|") != strings.Join(want, "|") {
		t.Fatalf("改动清单：\n%q\n%q", r.Changes, want)
	}
	// 字幕：c3 那条铺满的跟到新素材上；c4 被删，它的字幕一起去掉。
	if len(r.Content.Subtitles) != 1 || !strings.Contains(string(r.Content.Subtitles[0].Anchor), `"srcStartMs":0`) {
		t.Fatalf("字幕随片段：%+v", r.Content.Subtitles)
	}
	if !containsText(r.Notes, "1 条字幕一起去掉了") {
		t.Fatalf("说明去掉的字幕：%v", r.Notes)
	}
	// 新文字跟着 c3 的时间出现、放进不重叠的文字轨。
	var added *tl.ContentText
	for i := range r.Content.Texts {
		if r.Content.Texts[i].Text == "看这里" {
			added = &r.Content.Texts[i]
		}
	}
	if added == nil {
		t.Fatalf("新文字：%+v", r.Content.Texts)
	}
	p := lay.Clips["103"]
	if added.StartMs != p.StartMs || added.EndMs != p.StartMs+p.DurationMs {
		t.Fatalf("新文字跟着 c3：%+v / %+v", added, p)
	}
}

func TestApplyEdit_至少留一段(t *testing.T) {
	base, cards := editBase()
	base.Tracks[0].Clips = base.Tracks[0].Clips[1:2]
	base.Subtitles = nil
	d := parseEdit(t, base, cards, `{"ops":[{"op":"delete","clip":"c1"}]}`)
	r, err := ApplyEdit(base, d, cards)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Content.Tracks[0].Clips) != 1 || !containsText(r.Warnings, "至少要留一段") || !containsText(r.Warnings, "没有改动任何片段") {
		t.Fatalf("%+v", r)
	}
}
