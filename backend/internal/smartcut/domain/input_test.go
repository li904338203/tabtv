package domain

import "testing"

type noLookup struct{}

func (noLookup) Generation(string) (GenerationFact, bool) { return GenerationFact{}, false }
func (noLookup) Media(string) (MediaFact, bool)           { return MediaFact{}, false }

func ms(v int) *int { return &v }

// 指纹对编号、媒体、时长与就绪与否敏感；对标题、文字内容不敏感（dev/01 §4.6）。
func TestInputFingerprint(t *testing.T) {
	base := []Input{{No: "1", Status: InputReady, MediaID: "11", DurationMs: ms(4000)},
		{No: "2", Status: InputReady, MediaID: "12"}}
	fp := InputFingerprint(base)
	if len(fp) != 64 || InputFingerprint(base) != fp {
		t.Fatalf("sha256 十六进制、确定：%s", fp)
	}
	mut := func(f func([]Input)) string {
		c := append([]Input(nil), base...)
		f(c)
		return InputFingerprint(c)
	}
	for name, f := range map[string]func([]Input){
		"换了媒体": func(c []Input) { c[1].MediaID = "13" },
		"时长变了": func(c []Input) { c[0].DurationMs = ms(4100) },
		"编号变了": func(c []Input) { c[0].No, c[1].No = "2", "1" },
		"不再就绪": func(c []Input) { c[1].Status = InputPending },
		"顺序换了": func(c []Input) { c[0], c[1] = c[1], c[0] },
	} {
		if mut(f) == fp {
			t.Errorf("%s 应改变指纹", name)
		}
	}
	if mut(func(c []Input) { c[0].Title = "新标题"; c[1].Used = true }) != fp {
		t.Error("标题与用没用到不影响指纹")
	}
	if InputFingerprint(nil) == fp {
		t.Error("空素材条也有自己的指纹")
	}
}

// 参数里的 ID 可能是数字（前端 String(31) 也是 "31"）。
func TestResolveInputs_数字形式的脚本ID(t *testing.T) {
	items, _ := ResolveInputs([]Source{{NodeID: "s", Type: "script", Params: map[string]any{"scriptId": float64(31)}}},
		noLookup{}, nil)
	if len(items) != 1 || items[0].Status != InputReady || items[0].Kind != InputShotGroup {
		t.Fatalf("%+v", items)
	}
	if paramID(1.5) != "1.5" || paramID(true) != "" || paramID("x") != "x" {
		t.Fatal("paramID")
	}
}

func TestDefaultAudioRole(t *testing.T) {
	gen := Source{Result: SourceResult{ID: "1"}}
	cases := []struct {
		name string
		src  Source
		op   string
		want string
	}{
		{"回查的操作码优先", gen, "text2audio", RoleVoice},
		{"回查是别的操作", gen, "video2audio", RoleMusic},
		{"上传的默认配乐", Source{}, "", RoleMusic},
		{"有生成、没有入边：默认文字转语音", gen, "", RoleVoice},
		{"有生成、入边是配乐", Source{Result: SourceResult{ID: "1"}, FirstInboundOperation: "text2music"}, "", RoleMusic},
		{"复制来的也算有生成", Source{Result: SourceResult{SourceGenerationID: "9"}, FirstInboundOperation: "text2audio"}, "", RoleVoice},
	}
	for _, c := range cases {
		if got := DefaultAudioRole(c.src, c.op); got != c.want {
			t.Errorf("%s：%s，应为 %s", c.name, got, c.want)
		}
	}
}
