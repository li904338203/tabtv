package domain

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 共享 fixture（docs/agent/smartcut dev/02 §6）：前端的保存前校验读同一份。
const contentFixtureDir = "../../../../api/fixtures/smartcut/content"

func readContentFixture(t *testing.T, name string) json.RawMessage {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(contentFixtureDir, name))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestValidateContent_合法内容全部通过(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(contentFixtureDir, "valid-*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) < 4 {
		t.Fatalf("合法用例只有 %d 个", len(files))
	}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if err := ValidateContent(raw); err != nil {
			t.Errorf("%s 应当合法：%v", filepath.Base(f), err)
		}
	}
}

// TestValidateContent_非法内容报出字段路径：每个 invalid-*.json 只有一处毛病，Path 必须与 fixture 一致（前端报同一个）。
func TestValidateContent_非法内容报出字段路径(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(contentFixtureDir, "invalid-*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) < 60 {
		t.Fatalf("非法用例只有 %d 个", len(files))
	}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var c struct {
			Why     string          `json:"why"`
			Content json.RawMessage `json:"content"`
			Path    string          `json:"path"`
		}
		if err := json.Unmarshal(raw, &c); err != nil {
			t.Fatalf("%s：%v", f, err)
		}
		err = ValidateContent(c.Content)
		var ve *ValidationError
		if !errors.As(err, &ve) {
			t.Errorf("%s（%s）：期望字段错误 %q，实际 %v", filepath.Base(f), c.Why, c.Path, err)
			continue
		}
		if ve.Path != c.Path {
			t.Errorf("%s（%s）：期望 path %q，实际 %q（%s）", filepath.Base(f), c.Why, c.Path, ve.Path, ve.Reason)
		}
	}
}

// TestDecodeContent_未知字段原样保留：变换、样式、位置框、锚点里的未知字段解析后再序列化仍在（GO-35）。
func TestDecodeContent_未知字段原样保留(t *testing.T) {
	c, err := DecodeContent(readContentFixture(t, "valid-full.json"))
	if err != nil {
		t.Fatal(err)
	}
	back, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"futureTransform", "futureCrop", "futureVideoTrackStyle", "futureDuck",
		"futureMusicStyle", "futureSubtitleStyle", "futureSubStyle", "futureAnchor", "futureTextStyle", "futureBox"} {
		if !strings.Contains(string(back), key) {
			t.Errorf("序列化后丢了 %s", key)
		}
	}
	// 再序列化的结果必须仍然合法：编译器产出的内容就是走这一条路回到整份保存的。
	if err := ValidateContent(back); err != nil {
		t.Fatalf("解析再序列化后不合法：%v", err)
	}
	v := c.Tracks[0].Clips[0]
	if v.MediaID != 77 || v.Origin != OriginUser || v.SourceNodeID != "node_abc" || len(v.Alternates) != 2 ||
		v.Alternates[1].GenerationID != 9001 || v.Alternates[1].Kind != AlternateRegen {
		t.Errorf("片段字段解析不对：%+v", v)
	}
	if c.Tracks[0].Clips[2].Transform != nil {
		t.Errorf("transform: null 应规范成空，实际 %s", c.Tracks[0].Clips[2].Transform)
	}
	if c.Tracks[1].Role != RoleMusic || c.Tracks[3].Lang != "zh-CN" || !c.Tracks[4].IsHidden {
		t.Errorf("轨道字段解析不对：%+v", c.Tracks)
	}
	if a, ok := ParseAnchor(c.Subtitles[0].Anchor); ok {
		t.Errorf("tmp- 片段的锚点在落库换成正式 ID 之前解析不出来才对，实际 %+v", a)
	}
}

func TestDecodeContent_显式null当没有(t *testing.T) {
	c, err := DecodeContent(readContentFixture(t, "valid-nulls.json"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Tracks[0].Style != nil || c.Tracks[0].Clips[0].Transform != nil || c.Subtitles[0].Anchor != nil ||
		c.Subtitles[0].Style != nil || c.Texts[0].Box != nil || c.Tracks[0].Role != "" {
		t.Errorf("null 没有规范成空：%+v", c)
	}
}

func TestAspectPresets_与fixture一致(t *testing.T) {
	var f struct {
		Presets []AspectPreset `json:"presets"`
		Formats []struct {
			Aspect string `json:"aspectRatio"`
			FPS    int    `json:"fps"`
			Width  int    `json:"width"`
			Height int    `json:"height"`
			Path   string `json:"path"`
		} `json:"formats"`
	}
	if err := json.Unmarshal(readContentFixture(t, "aspects.json"), &f); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(f.Presets) != fmt.Sprint(AspectPresets) {
		t.Errorf("画幅表与 fixture 不一致：\n代码 %v\nfixture %v", AspectPresets, f.Presets)
	}
	if w, h, ok := DefaultSize("3:4"); !ok || w != 1080 || h != 1440 {
		t.Errorf("3:4 默认 1080×1440，实际 %d×%d", w, h)
	}
	if _, _, ok := DefaultSize(AspectCustom); ok {
		t.Error("custom 没有默认尺寸，尺寸只能由调用方给")
	}
	for _, c := range f.Formats {
		ve := ValidateFormat(c.Aspect, c.FPS, c.Width, c.Height)
		got := ""
		if ve != nil {
			got = ve.Path
		}
		if got != c.Path {
			t.Errorf("%s %dfps %d×%d：期望 path %q，实际 %q（%v）", c.Aspect, c.FPS, c.Width, c.Height, c.Path, got, ve)
		}
	}
}

// 变换的取值边界：恰好在边界上合法，越过一点点报到具体字段。
func TestValidateContent_变换取值边界(t *testing.T) {
	cases := []struct {
		transform string
		path      string
	}{
		{`{"scale":0.1}`, ""},
		{`{"scale":0.09}`, "scale"},
		{`{"scale":5}`, ""},
		{`{"scale":5.01}`, "scale"},
		{`{"rotation":-180}`, ""},
		{`{"rotation":180.5}`, "rotation"},
		{`{"x":-100,"y":100}`, ""},
		{`{"x":-100.1}`, "x"},
		{`{"crop":{"left":45,"top":45,"right":0,"bottom":0}}`, ""},
		{`{"crop":{"bottom":45.1}}`, "crop.bottom"},
		{`{"crop":null}`, ""},
		{`{"crop":"none"}`, "crop"},
	}
	for _, c := range cases {
		raw := fmt.Sprintf(`{"version":1,"aspectRatio":"16:9","fps":30,"width":1920,"height":1080,
			"tracks":[{"id":"1","kind":"video","clips":[{"id":"2","mediaId":"3","sourceInMs":0,"sourceOutMs":1000,"transform":%s}]}]}`,
			c.transform)
		err := ValidateContent(json.RawMessage(raw))
		want := ""
		if c.path != "" {
			want = "tracks[0].clips[0].transform." + c.path
		}
		got := ""
		var ve *ValidationError
		if errors.As(err, &ve) {
			got = ve.Path
		} else if err != nil {
			got = err.Error()
		}
		if got != want {
			t.Errorf("%s：期望 %q，实际 %q", c.transform, want, got)
		}
	}
}

// 字幕条数的上限不放进 fixture（两千条太大），在这里钉住。
func TestValidateContent_字幕条数上限(t *testing.T) {
	build := func(n int) json.RawMessage {
		subs := make([]string, n)
		for i := range subs {
			subs[i] = fmt.Sprintf(`{"id":"tmp-s%d","trackId":"9","startMs":%d,"endMs":%d,"text":"字"}`, i, i*100, i*100+100)
		}
		return json.RawMessage(`{"version":1,"aspectRatio":"16:9","fps":30,"width":1920,"height":1080,
			"tracks":[{"id":"1","kind":"video","clips":[]},{"id":"9","kind":"subtitle"}],
			"subtitles":[` + strings.Join(subs, ",") + `]}`)
	}
	if err := ValidateContent(build(MaxSubtitles)); err != nil {
		t.Fatalf("恰好 %d 条应当合法：%v", MaxSubtitles, err)
	}
	var ve *ValidationError
	if err := ValidateContent(build(MaxSubtitles + 1)); !errors.As(err, &ve) || ve.Path != "subtitles" {
		t.Fatalf("超过上限应报 subtitles，实际 %v", err)
	}
}

func TestPushAlternate_超过上限丢最早的(t *testing.T) {
	var list []Alternate
	for i := 1; i <= MaxAlternates+2; i++ {
		list = PushAlternate(list, Alternate{MediaID: int64(i), SourceOutMs: 1000, Kind: AlternateRegen})
	}
	if len(list) != MaxAlternates {
		t.Fatalf("备选应保留 %d 个，实际 %d", MaxAlternates, len(list))
	}
	if list[0].MediaID != 3 || list[len(list)-1].MediaID != int64(MaxAlternates+2) {
		t.Errorf("应丢掉最早的两个：%v", list)
	}
	// 不改调用方手上的那一份（撤销栈里存的是旧切片）。
	orig := []Alternate{{MediaID: 1}}
	_ = PushAlternate(orig, Alternate{MediaID: 2})
	if len(orig) != 1 {
		t.Error("PushAlternate 改了传进来的切片")
	}
}

func TestParseAnchor_坏值当不跟随(t *testing.T) {
	for _, raw := range []string{``, `null`, `{}`, `"x"`, `{"clipId":"1","srcStartMs":500,"srcEndMs":500}`, `{"clipId":"tmp-1","srcStartMs":0,"srcEndMs":10}`} {
		if a, ok := ParseAnchor(json.RawMessage(raw)); ok {
			t.Errorf("%q 应当解析成不跟随，实际 %+v", raw, a)
		}
	}
	a, ok := ParseAnchor(json.RawMessage(`{"clipId":"42","srcStartMs":100,"srcEndMs":900,"future":1}`))
	if !ok || a.ClipID != 42 || a.SrcStartMs != 100 || a.SrcEndMs != 900 {
		t.Errorf("合法锚点解析不对：%+v %v", a, ok)
	}
}

// ValidateClip 与整份校验同一个最短口径：按时间线上的长度判。
func TestValidateClip_最短按成片长度判(t *testing.T) {
	c := Clip{MediaID: 1, SourceInMs: 0, SourceOutMs: 396, Speed: 400, Volume: 100}
	if err := ValidateClip(c, 0); err == nil {
		t.Fatal("4 倍速 396ms 只占 99ms，应当太短")
	}
	c.SourceOutMs = 400
	if err := ValidateClip(c, 0); err != nil {
		t.Fatalf("恰好 100ms 应当合法：%v", err)
	}
}

func TestObjectOrNil_只认对象(t *testing.T) {
	for _, raw := range []string{``, `null`, `[]`, `"x"`, `12`, `{bad`} {
		if got := ObjectOrNil(json.RawMessage(raw)); got != nil {
			t.Errorf("%q 应当当成没有，实际 %s", raw, got)
		}
	}
	if got := ObjectOrNil(json.RawMessage(`{"scale":2,"future":1}`)); string(got) != `{"scale":2,"future":1}` {
		t.Errorf("对象应原样保留，实际 %s", got)
	}
}

func TestParseAlternates_坏的跳过(t *testing.T) {
	raw := `[{"mediaId":"1","sourceInMs":0,"sourceOutMs":1000,"kind":"original"},
		{"mediaId":"2","sourceInMs":500,"sourceOutMs":500,"kind":"regen"},
		{"mediaId":"3","sourceInMs":0,"sourceOutMs":1000,"kind":"upscale"},
		{"mediaId":"x","sourceInMs":0,"sourceOutMs":1000,"kind":"regen"},
		"junk",
		{"mediaId":"4","sourceInMs":0,"sourceOutMs":900,"kind":"desubtitle","generationId":"77"}]`
	got := ParseAlternates(json.RawMessage(raw))
	if len(got) != 2 || got[0].MediaID != 1 || got[1].MediaID != 4 || got[1].GenerationID != 77 {
		t.Errorf("应只留下 1 与 4，实际 %+v", got)
	}
	if ParseAlternates(json.RawMessage(`{"not":"array"}`)) != nil || ParseAlternates(nil) != nil {
		t.Error("整列坏了应当当没有")
	}
}

func TestSplit_右半段不继承备选与进行中的生成(t *testing.T) {
	c := Clip{ID: 9, MediaID: 1, SourceOutMs: 4000, Speed: 100, Volume: 100, SourceNodeID: "node_a",
		Transform: json.RawMessage(`{"scale":1.5}`), Origin: OriginAI, PendingGenerationID: 88,
		Alternates: []Alternate{{MediaID: 5, SourceOutMs: 4000, Kind: AlternateOriginal}}}
	left, right, err := Split(c, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if left.PendingGenerationID != 88 || len(left.Alternates) != 1 {
		t.Errorf("左半段保留原片段的身份：%+v", left)
	}
	if right.PendingGenerationID != 0 || right.Alternates != nil {
		t.Errorf("右半段不该继承备选与进行中的生成：%+v", right)
	}
	if right.SourceNodeID != "node_a" || string(right.Transform) != `{"scale":1.5}` {
		t.Errorf("右半段照样来自同一个节点、同一个画面变换：%+v", right)
	}
}

// 刀口是硬切：转入留给左半段（与上一段的接缝），转出跟着右半段（原片段的结尾）。
func TestSplit_刀口是硬切且转出跟右半段走(t *testing.T) {
	c := Clip{ID: 9, MediaID: 1, SourceOutMs: 6000, Speed: 100, Volume: 100,
		TransitionIn:  Transition{Kind: TransitionDissolve, DurationMs: 1000},
		TransitionOut: Transition{Kind: TransitionFade, DurationMs: 800}}
	left, right, err := Split(c, 2000)
	if err != nil {
		t.Fatal(err)
	}
	if !left.TransitionIn.IsActive() || left.TransitionOut.IsActive() {
		t.Errorf("左半段保留转入、不带转出：%+v / %+v", left.TransitionIn, left.TransitionOut)
	}
	if right.TransitionIn.IsActive() || !right.TransitionOut.IsActive() {
		t.Errorf("右半段刀口硬切、带着转出：%+v / %+v", right.TransitionIn, right.TransitionOut)
	}
	// 切开之后成片总长不变：多一次叠化的话会短 1000。
	before := EffectiveDuration([]Clip{{SourceOutMs: 3000, Speed: 100}, c})
	after := EffectiveDuration([]Clip{{SourceOutMs: 3000, Speed: 100}, left, right})
	if before != after {
		t.Errorf("切一刀不该改变成片总长：%d → %d", before, after)
	}
}
