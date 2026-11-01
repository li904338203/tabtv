package domain

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	tl "github.com/li904338203/tabtv/backend/internal/timeline/domain"
)

// ---------------------------------------------------------------- 测试助手

func testBase() tl.Content {
	return tl.Content{Version: 3, AspectRatio: "9:16", FPS: 30, Width: 1080, Height: 1920,
		Tracks: []tl.ContentTrack{{ID: "1001", Kind: tl.TrackVideo}, {ID: "1003", Kind: tl.TrackSubtitle, Lang: "zh-CN"},
			{ID: "1004", Kind: tl.TrackText}}}
}

func videoCard(no string, ms int) Card {
	id, _ := strconv.ParseInt("7"+strings.ReplaceAll(no, "-", "0"), 10, 64)
	return Card{No: no, Kind: InputVideo, MediaID: id, DurationMs: ms, HasAudio: true, SourceNodeID: "n" + no}
}

func imageCard(no string) Card {
	id, _ := strconv.ParseInt("7"+no, 10, 64)
	return Card{No: no, Kind: InputImage, MediaID: id, SourceNodeID: "n" + no}
}

func musicCard(no string, ms int) Card {
	id, _ := strconv.ParseInt("8"+no, 10, 64)
	return Card{No: no, Kind: InputAudio, MediaID: id, DurationMs: ms, AudioRole: RoleMusic, SourceNodeID: "n" + no}
}

// mustValid 编译结果必须能原样整份保存：过 ValidateContent，且 Layout 算出的总长与结果一致。
func mustValid(t *testing.T, r Result) tl.LayoutResult {
	t.Helper()
	raw, err := json.Marshal(r.Content)
	if err != nil {
		t.Fatal(err)
	}
	if err := tl.ValidateContent(raw); err != nil {
		t.Fatalf("编译结果过不了整份保存的校验：%v\n%s", err, raw)
	}
	lay := tl.Layout(r.Content)
	if lay.EffectiveMs != r.TotalMs {
		t.Fatalf("总长对不上：Layout %d，结果 %d", lay.EffectiveMs, r.TotalMs)
	}
	return lay
}

func video(r Result) []tl.ContentClip {
	for _, tr := range r.Content.Tracks {
		if tr.Kind == tl.TrackVideo {
			return tr.Clips
		}
	}
	return nil
}

func segs(specs ...string) []Segment {
	// "3:hook:3:cut:full"
	var out []Segment
	for _, s := range specs {
		p := strings.Split(s, ":")
		w, _ := strconv.Atoi(p[2])
		out = append(out, Segment{Clip: p[0], Role: p[1], Weight: w, Transition: p[3], Take: Take{Kind: p[4]}})
	}
	return out
}

func mixInput(cards []Card, spec Spec) PlanInput {
	return PlanInput{Mode: ModeMix, Base: testBase(), Cards: cards, Spec: spec}
}

// ---------------------------------------------------------------- 可用长度

func TestRegion_可用长度(t *testing.T) {
	v := videoCard("1", 10000)
	cases := []struct {
		card    Card
		take    Take
		in, out int
	}{
		{v, Take{Kind: TakeFull}, 0, 10000},
		{v, Take{Kind: TakeHead}, 0, 4000},
		{v, Take{Kind: TakeMiddle}, 3000, 7000},
		{v, Take{Kind: TakeTail}, 6000, 10000},
		{v, Take{Kind: TakeAround, AroundMs: 2500}, 500, 4500},
		{v, Take{Kind: TakeAround, AroundMs: 9800}, 6000, 10000}, // 夹在素材里
		{v, Take{Kind: TakeHead, RatioPermille: 600}, 0, 6000},
		{videoCard("2", 2000), Take{Kind: TakeHead}, 0, 1500},     // 40% 不到 1.5 秒按 1.5 秒
		{videoCard("3", 1000), Take{Kind: TakeTail}, 0, 1000},     // 不超过源时长
		{imageCard("4"), Take{Kind: TakeHead}, 0, ImageDefaultMs}, // 图片默认 3 秒
	}
	for i, c := range cases {
		in, out := region(c.card, c.take)
		if in != c.in || out != c.out {
			t.Errorf("第 %d 条 %+v：期望 [%d,%d]，实际 [%d,%d]", i, c.take, c.in, c.out, in, out)
		}
	}
}

// ---------------------------------------------------------------- 分配

func TestWaterFill_按权重分配且不超过上限(t *testing.T) {
	cases := []struct {
		need          int
		weights, caps []int
		want          []int
	}{
		{20, []int{1, 1, 2}, []int{10, 10, 10}, []int{5, 5, 10}},
		{20, []int{1, 1, 2}, []int{3, 10, 10}, []int{3, 7, 10}},
		{10, []int{1, 1, 1}, []int{10, 10, 10}, []int{4, 3, 3}}, // 余数按最大余数、平局取下标小的
		{0, []int{1, 2}, []int{5, 5}, []int{0, 0}},
		{9, []int{1200, 1000}, []int{100, 100}, []int{5, 4}},
	}
	for i, c := range cases {
		got := waterFill(c.need, c.weights, c.caps)
		if strings.Join(intsToStrings(got), ",") != strings.Join(intsToStrings(c.want), ",") {
			t.Errorf("第 %d 条：期望 %v，实际 %v", i, c.want, got)
		}
		if sumInts(c.caps) >= c.need && sumInts(got) != c.need {
			t.Errorf("第 %d 条：总和 %d ≠ %d", i, sumInts(got), c.need)
		}
	}
}

func intsToStrings(v []int) []string {
	out := make([]string, len(v))
	for i, x := range v {
		out[i] = strconv.Itoa(x)
	}
	return out
}

// TestCompileMix_素材不够的三种处理（SC-AC-38）：7 段各约 2 秒、目标 20 秒。
func TestCompileMix_素材不够的三种处理(t *testing.T) {
	var cards []Card
	var specs []string
	for i := 1; i <= 7; i++ {
		no := strconv.Itoa(i)
		cards = append(cards, videoCard(no, 2040))
		specs = append(specs, no+":body:1:cut:full")
	}
	d := Draft{Mode: ModeMix, Segments: segs(specs...)}

	r, err := Compile(mixInput(cards, Spec{TargetMs: 20000}), d)
	if err != nil {
		t.Fatal(err)
	}
	mustValid(t, r)
	if r.TotalMs != 14280 {
		t.Fatalf("默认如实剪：期望 14280，实际 %d", r.TotalMs)
	}
	if !containsText(r.Warnings, "素材共 14.3 秒，不足目标 20 秒，已按 14.3 秒剪") {
		t.Fatalf("警告应写明素材不够：%v", r.Warnings)
	}
	for _, c := range video(r) {
		if c.Speed != nil {
			t.Fatalf("默认不放慢：%+v", c)
		}
	}
	if len(video(r)) != 7 {
		t.Fatalf("默认不重复：%d 段", len(video(r)))
	}
	if r.Shortage == nil || r.Shortage.TotalMs != 14280 || r.Shortage.TargetMs != 20000 {
		t.Fatalf("素材不够要有结构化的 shortage：%+v", r.Shortage)
	}

	r, err = Compile(mixInput(cards, Spec{TargetMs: 20000, AllowSlow: true}), d)
	if err != nil {
		t.Fatal(err)
	}
	mustValid(t, r)
	if r.TotalMs < 18500 || r.TotalMs > 20000 {
		t.Fatalf("允许放慢后应接近 20 秒：%d", r.TotalMs)
	}
	for _, c := range video(r) {
		if c.Speed == nil || *c.Speed < MinSlowSpeed {
			t.Fatalf("放慢不低于 0.75×：%+v", c)
		}
	}
	if !containsText(r.Warnings, "放慢了 7 段（最慢 0.75×）") {
		t.Fatalf("要说明放慢了：%v", r.Warnings)
	}

	r, err = Compile(mixInput(cards, Spec{TargetMs: 20000, AllowRepeat: true}), d)
	if err != nil {
		t.Fatal(err)
	}
	mustValid(t, r)
	if r.TotalMs != 20000 {
		t.Fatalf("允许重复后应凑满 20 秒：%d", r.TotalMs)
	}
	if len(video(r)) <= 7 || !containsText(r.Warnings, "重复用了编号") {
		t.Fatalf("要重复且说明：%d 段 %v", len(video(r)), r.Warnings)
	}
	if r.Shortage != nil {
		t.Fatalf("凑满了就没有 shortage：%+v", r.Shortage)
	}
}

// TestCompileMix_突出钩子（SC-AC-40）：方案把 3 标为钩子并加权：编译后 3 在开头，时长不少于其余段的平均值。
func TestCompileMix_突出钩子(t *testing.T) {
	var cards []Card
	for i := 1; i <= 5; i++ {
		c := videoCard(strconv.Itoa(i), 6000)
		c.Description = "街景"
		if i == 3 {
			c.Description = "产品特写"
		}
		cards = append(cards, c)
	}
	d := Draft{Mode: ModeMix, Segments: segs("1:body:1:cut:full", "2:body:1:dissolve:full", "3:hook:3:fade:full",
		"4:body:1:cut:full", "5:end:2:fade:full")}
	r, err := Compile(mixInput(cards, Spec{TargetMs: 15000}), d)
	if err != nil {
		t.Fatal(err)
	}
	mustValid(t, r)
	clips := video(r)
	if clips[0].MediaID != cards[2].MediaID {
		t.Fatalf("钩子应在开头：%v", r.UsedClips)
	}
	if clips[0].TransitionIn != nil {
		t.Fatal("第一段与钩子硬切")
	}
	if clips[len(clips)-1].MediaID != cards[4].MediaID {
		t.Fatal("结尾段应在最后")
	}
	hook := clips[0].PlayLen()
	others := 0
	for _, c := range clips[1:] {
		others += c.PlayLen()
	}
	if hook*(len(clips)-1) < others {
		t.Fatalf("钩子时长 %d 应不少于其余段的平均值 %d", hook, others/(len(clips)-1))
	}
	if r.TotalMs != 15000 {
		t.Fatalf("够长时总长等于目标：%d", r.TotalMs)
	}
}

// TestCompileMix_卡点（SC-AC-39）：120 BPM，切换点与最近节拍的偏差不超过一帧（30fps），总长与目标差不超过一拍。
func TestCompileMix_卡点(t *testing.T) {
	var beats, downs []int
	for ms := 0; ms <= 30000; ms += 500 {
		beats = append(beats, ms)
		if ms%2000 == 0 {
			downs = append(downs, ms)
		}
	}
	for _, trans := range []string{"cut", "dissolve"} {
		var cards []Card
		var specs []string
		for i := 1; i <= 6; i++ {
			no := strconv.Itoa(i)
			cards = append(cards, videoCard(no, 3000+i*137))
			specs = append(specs, no+":body:"+strconv.Itoa(1+i%3)+":"+trans+":full")
		}
		cards = append(cards, musicCard("9", 30000))
		in := mixInput(cards, Spec{TargetMs: 13300})
		in.Beats = map[string]Beats{"9": {BeatsMs: beats, DownbeatsMs: downs}}
		r, err := Compile(in, Draft{Mode: ModeMix, Segments: segs(specs...), Music: "9"})
		if err != nil {
			t.Fatal(err)
		}
		lay := mustValid(t, r)
		clips := video(r)
		for k := 1; k < len(clips); k++ {
			p := lay.Clips[clips[k].ID]
			sw := p.StartMs
			if clips[k].TransitionIn != nil {
				sw += clips[k].TransitionIn.DurationMs / 2
			}
			dev := sw % 500
			dev = min(dev, 500-dev)
			if dev > 33 {
				t.Fatalf("%s：第 %d 处切换点 %d 离节拍 %dms", trans, k, sw, dev)
			}
		}
		if d := absInt(r.TotalMs - 13300); d > 500 {
			t.Fatalf("%s：总长 %d 与目标差 %d，超过一拍", trans, r.TotalMs, d)
		}
		music := audioClips(r, RoleMusic)
		if len(music) != 1 || music[0].SourceOutMs != r.TotalMs || music[0].FadeOutMs <= 0 {
			t.Fatalf("%s：配乐裁到总长并淡出：%+v", trans, music)
		}
	}
}

func audioClips(r Result, role string) []tl.ContentClip {
	for _, tr := range r.Content.Tracks {
		if tr.Kind == tl.TrackAudio && tr.Role == role {
			return tr.Clips
		}
	}
	return nil
}

func TestSnap_附近没有节拍就不吸并说明(t *testing.T) {
	cards := []Card{videoCard("1", 5000), videoCard("2", 5000), videoCard("3", 5000), musicCard("9", 4000)}
	in := mixInput(cards, Spec{TargetMs: 12000})
	// 配乐只有 4 秒：4 秒之后没有拍点。
	in.Beats = map[string]Beats{"9": {BeatsMs: []int{0, 500, 1000, 1500, 2000, 2500, 3000, 3500, 4000}}}
	r, err := Compile(in, Draft{Mode: ModeMix, Segments: segs("1:body:1:cut:full", "2:body:1:cut:full", "3:body:1:cut:full"), Music: "9"})
	if err != nil {
		t.Fatal(err)
	}
	mustValid(t, r)
	if !containsText(r.Warnings, "附近没有合适的节拍") || !containsText(r.Warnings, "配乐只有 4 秒") {
		t.Fatalf("要说明没卡上与配乐太短：%v", r.Warnings)
	}
}

func TestClampOverlaps_转场不超过较短一段的一半(t *testing.T) {
	cards := []Card{videoCard("1", 600), videoCard("2", 3000)}
	r, err := Compile(mixInput(cards, Spec{}), Draft{Mode: ModeMix, Segments: segs("1:body:1:cut:full", "2:body:1:dissolve:full")})
	if err != nil {
		t.Fatal(err)
	}
	mustValid(t, r)
	c := video(r)[1]
	if c.TransitionIn == nil || c.TransitionIn.DurationMs*2 > video(r)[0].PlayLen() {
		t.Fatalf("转场要收短：%+v / %d", c.TransitionIn, video(r)[0].PlayLen())
	}
	// 两段都很短：收不到 100ms 就改硬切。
	cards = []Card{imageCard("1"), videoCard("2", 150)}
	r, err = Compile(mixInput(cards, Spec{}), Draft{Mode: ModeMix, Segments: segs("1:body:1:cut:full", "2:body:1:fade:full")})
	if err != nil {
		t.Fatal(err)
	}
	mustValid(t, r)
	if video(r)[1].TransitionIn != nil {
		t.Fatalf("不到 100ms 的转场改硬切：%+v", video(r)[1].TransitionIn)
	}
}

func TestCompileMix_字幕标题与合并(t *testing.T) {
	cards := []Card{videoCard("1", 4000), videoCard("2", 4000)}
	base := testBase()
	base.Subtitles = []tl.ContentSubtitle{{ID: "501", TrackID: "1003", StartMs: 0, EndMs: 900, Text: "旧字幕",
		Anchor: []byte(`{"clipId":"9999","srcStartMs":0,"srcEndMs":900}`)}}
	base.Texts = []tl.ContentText{{ID: "601", TrackID: "1004", StartMs: 0, EndMs: 1500, Text: "旧标题"},
		{ID: "602", TrackID: "1004", StartMs: 5000, EndMs: 6000, Text: "中间的贴字"}}
	in := PlanInput{Mode: ModeMix, Base: base, Cards: cards, Spec: Spec{WithSubtitles: true}}
	d := Draft{Mode: ModeMix, Segments: []Segment{
		{Clip: "1", Take: Take{Kind: TakeFull}, Role: SegHook, Weight: 2, Transition: TransCut, Subtitle: "第一句"},
		{Clip: "2", Take: Take{Kind: TakeFull}, Role: SegBody, Weight: 1, Transition: "dissolve", Subtitle: "第二句"},
	}, Title: &Title{Text: "新品上市", DurationMs: 2000}}
	r, err := Compile(in, d)
	if err != nil {
		t.Fatal(err)
	}
	lay := mustValid(t, r)
	if len(r.Content.Subtitles) != 2 {
		t.Fatalf("字幕替换成两条新的：%+v", r.Content.Subtitles)
	}
	s1, s2 := lay.Subtitles[r.Content.Subtitles[0].ID], lay.Subtitles[r.Content.Subtitles[1].ID]
	if s1.EndMs > s2.StartMs+1 || s1.StartMs != 0 || s2.EndMs != r.TotalMs {
		t.Fatalf("两条字幕在切换点衔接：%+v %+v（总长 %d）", s1, s2, r.TotalMs)
	}
	var texts []string
	for _, tx := range r.Content.Texts {
		texts = append(texts, tx.Text)
	}
	if strings.Join(texts, "|") != "新品上市|中间的贴字" {
		t.Fatalf("开头的旧标题换掉、中间的贴字保留：%v", texts)
	}
	if !containsText(r.Notes, "开头的文字「旧标题」换成了新标题") {
		t.Fatalf("要说明换掉了旧标题：%v", r.Notes)
	}
}

func TestCompileMix_确定性(t *testing.T) {
	cards := []Card{videoCard("1", 4100), videoCard("2", 3900), imageCard("3"), musicCard("9", 20000)}
	in := mixInput(cards, Spec{TargetMs: 9000})
	in.Beats = map[string]Beats{"9": {BeatsMs: []int{0, 480, 960, 1440, 1920, 2400, 2880, 3360, 3840, 4320, 4800, 5280, 5760, 6240, 6720, 7200, 7680, 8160, 8640, 9120, 9600}}}
	d := Draft{Mode: ModeMix, Segments: segs("2:hook:3:cut:head", "1:body:2:fade:middle", "3:end:1:wipe:full")}
	a, _ := Compile(in, d)
	b, _ := Compile(in, d)
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	if string(ja) != string(jb) {
		t.Fatal("同样的输入应得到逐字节相同的输出")
	}
	mustValid(t, a)
}

func containsText(list []string, sub string) bool {
	for _, s := range list {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------- 兜底（SC-AC-32）

func TestFallback_四种模式都不留空时间线(t *testing.T) {
	cards := []Card{videoCard("1", 3000), videoCard("2", 3000), imageCard("3"), musicCard("9", 20000)}
	r, err := Fallback(mixInput(cards, Spec{TargetMs: 7000}))
	if err != nil {
		t.Fatal(err)
	}
	mustValid(t, r)
	if !r.UsedFallback || !containsText(r.Notes, FallbackNote) || r.TotalMs != 7000 {
		t.Fatalf("混剪兜底：%+v", r)
	}
	if got := video(r); len(got) != 3 || got[2].SourceOutMs != 1000 {
		t.Fatalf("按编号顺序拼接、从尾部截：%+v", got)
	}

	shots := NumberShots([]BoardShot{{ShotNo: 1, MediaID: 71, DurationMs: 3000, SourceOutMs: 3000, Dialogue: "你好。"},
		{ShotNo: 2, MediaID: 72, DurationMs: 2000, SourceOutMs: 2000}})
	r, err = Fallback(PlanInput{Mode: ModeBoard, Base: testBase(), Shots: shots, Spec: Spec{WithSubtitles: true}})
	if err != nil {
		t.Fatal(err)
	}
	mustValid(t, r)
	if !r.UsedFallback || len(video(r)) != 2 || len(r.Content.Subtitles) != 1 {
		t.Fatalf("分镜兜底：%+v", r)
	}

	talk := talkFixture()
	r, err = Fallback(talk)
	if err != nil {
		t.Fatal(err)
	}
	mustValid(t, r)
	if !r.UsedFallback || len(video(r)) == 0 {
		t.Fatalf("口播兜底：%+v", r)
	}

	narr := narrFixture("第一句话。第二句话！")
	r, err = Fallback(narr)
	if err != nil {
		t.Fatal(err)
	}
	mustValid(t, r)
	if !r.UsedFallback || len(video(r)) == 0 || r.TotalMs != 4000 {
		t.Fatalf("解说兜底：%d %+v", r.TotalMs, r)
	}
}

// TestMergeTiny_吸完太短的段并入相邻段：第 2 段只分到 159ms，前一处切换点吸到 500 之后它只剩 1ms ——
// 并给后一段（留下在拍上的 500），不留一个渲染时会被丢掉的碎段。
func TestMergeTiny_吸完太短的段并入相邻段(t *testing.T) {
	var beats []int
	for ms := 0; ms <= 10000; ms += 500 {
		beats = append(beats, ms)
	}
	cards := []Card{videoCard("1", 5000), videoCard("2", 5000), videoCard("3", 5000), musicCard("9", 10000)}
	in := mixInput(cards, Spec{TargetMs: 1000})
	in.Beats = map[string]Beats{"9": {BeatsMs: beats}}
	r, err := Compile(in, Draft{Mode: ModeMix, Segments: segs("1:body:2:cut:full", "2:body:1:cut:full", "3:end:3:cut:full"), Music: "9"})
	if err != nil {
		t.Fatal(err)
	}
	mustValid(t, r)
	clips := video(r)
	if len(clips) != 2 || clips[0].MediaID != cards[0].MediaID || clips[1].MediaID != cards[2].MediaID ||
		clips[0].PlayLen() != 500 || clips[1].PlayLen() != 500 {
		t.Fatalf("碎段并掉、切换点留在 500：%+v", clips)
	}
	if containsText(r.Warnings, "附近没有合适的节拍") || !containsText(r.Warnings, "没用上的素材：编号 2") {
		t.Fatalf("%v", r.Warnings)
	}
}

// TestFitTarget_转场收短后仍对齐目标：分配时按默认转场留了重叠，段太短收短了转场，成片会变长 —— 收回来。
func TestFitTarget_转场收短后仍对齐目标(t *testing.T) {
	cards := []Card{videoCard("1", 4000), videoCard("2", 4000), videoCard("3", 4000)}
	r, err := Compile(mixInput(cards, Spec{TargetMs: 1200}), Draft{Mode: ModeMix,
		Segments: segs("1:body:1:cut:full", "2:body:1:dissolve:full", "3:body:1:dissolve:full")})
	if err != nil {
		t.Fatal(err)
	}
	mustValid(t, r)
	if d := absInt(r.TotalMs - 1200); d > 34 {
		t.Fatalf("总长 %d 与目标差 %d", r.TotalMs, d)
	}
	for k, c := range video(r)[1:] {
		if c.TransitionIn == nil || c.TransitionIn.DurationMs*2 > min(c.PlayLen(), video(r)[k].PlayLen()) {
			t.Fatalf("转场不超过较短一段的一半：%+v", c)
		}
	}
}
