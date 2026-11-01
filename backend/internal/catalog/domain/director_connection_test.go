package domain

import (
	"strings"
	"testing"
)

// 3D 导演台的连线（DRT-05，DRD-20）：导演台的产出是截图，一律按 image 算。

func directorRules() RuleSet {
	rules := []Rule{}
	for _, r := range standardRules() {
		rules = append(rules, r)
	}
	rules = append(rules,
		Rule{"image", "director", true, ""},
		Rule{"director", "image", true, "img2img"},
		Rule{"director", "video", true, "ref2video"},
	)
	return NewRuleSet(rules)
}

func TestMediaRoleOf_导演台算图片(t *testing.T) {
	cases := map[string]string{
		"image": "image", "video": "video", "audio": "audio",
		"director": "image",
		"text":     "", "style": "", "effect": "", "script": "", "": "",
	}
	for nodeType, want := range cases {
		if got := MediaRoleOf(nodeType); got != want {
			t.Errorf("MediaRoleOf(%q) = %q，期望 %q", nodeType, got, want)
		}
	}
}

func TestValidateVideoMode_导演台按图片计数(t *testing.T) {
	if err := ValidateVideoMode("img2video", []Input{in(1, "director")}); err != nil {
		t.Fatalf("图生视频应接受一张导演台截图：%v", err)
	}
	if err := ValidateVideoMode("ref2video", []Input{in(1, "director"), in(2, "audio")}); err != nil {
		t.Fatalf("全能参考应接受导演台截图 + 音频：%v", err)
	}
	// 计数照常：图生视频恰好一张，导演台截图也算一张。
	err := ValidateVideoMode("img2video", []Input{in(1, "director"), in(2, "image")})
	if err == nil {
		t.Fatal("图生视频连两张图（导演台 + 图片）应被拒绝")
	}
	if !strings.Contains(err.Error(), "实际 2 个媒体输入") {
		t.Errorf("错误要说清实际数量：%v", err)
	}
	// 视频生视频不收导演台：报错仍写原节点类型，用户才知道是哪条线的问题。
	err = ValidateVideoMode("video2video", []Input{in(1, "director")})
	if err == nil || !strings.Contains(err.Error(), "director") {
		t.Errorf("视频生视频不该收导演台，且错误要写出 director：%v", err)
	}
}

func TestValidateVideoMode_首尾帧收两张导演台截图(t *testing.T) {
	if err := ValidateVideoMode("first_last_frame", []Input{in(1, "director"), in(2, "director")}); err != nil {
		t.Fatalf("首尾帧应接受两张导演台截图：%v", err)
	}
}

func TestValidateConnections_导演台三条规则(t *testing.T) {
	rs := directorRules()
	for _, p := range [][2]string{{"image", "director"}, {"director", "image"}, {"director", "video"}} {
		if err := rs.ValidateConnections(p[1], []Input{in(1, p[0])}); err != nil {
			t.Errorf("%s→%s 应被允许：%v", p[0], p[1], err)
		}
	}
	if op, err := rs.ResolveOperation("image", "", []Input{in(1, "director")}); err != nil || op != "img2img" {
		t.Errorf("导演台 → 图片默认图生图，实际 %q（%v）", op, err)
	}
	if op, err := rs.ResolveOperation("video", "img2video", []Input{in(1, "director")}); err != nil || op != "img2video" {
		t.Errorf("视频节点以所选模式为准，实际 %q（%v）", op, err)
	}
}

func TestValidateConnections_导演台不能连音频(t *testing.T) {
	rs := directorRules()
	for _, p := range [][2]string{{"director", "audio"}, {"director", "text"}, {"video", "director"}, {"text", "director"}} {
		if err := rs.ValidateConnections(p[1], []Input{in(1, p[0])}); err == nil {
			t.Errorf("%s→%s 应被拒绝", p[0], p[1])
		}
	}
}

func TestCheckConnection_导演台三条规则与音频(t *testing.T) {
	rs := directorRules()
	cases := []struct {
		name, target, mode string
		inputs             []Input
		valid              bool
		op                 string
	}{
		{"图片连导演台：没有生成操作，照样合法", "director", "", []Input{in(1, "image")}, true, ""},
		{"导演台连图片：默认图生图", "image", "", []Input{in(1, "director")}, true, "img2img"},
		{"导演台连视频：以所选模式为准", "video", "img2video", []Input{in(1, "director")}, true, "img2video"},
		{"导演台连视频没选模式：不合法", "video", "", []Input{in(1, "director")}, false, ""},
		{"导演台连音频：规则不允许", "audio", "", []Input{in(1, "director")}, false, ""},
	}
	for _, c := range cases {
		v := rs.CheckConnection(c.target, c.mode, c.inputs)
		if v.Valid != c.valid || v.Operation != c.op {
			t.Errorf("%s：得到 %+v，期望 valid=%v op=%q", c.name, v, c.valid, c.op)
		}
		if !v.Valid && v.Reason == "" {
			t.Errorf("%s：不合法时要说明原因", c.name)
		}
	}
}

// DRB-1 修好之后前端会真的看 valid：规则允许、只是没有默认操作的连线不能被判成非法。
func TestCheckConnection_风格单独连图片也算连得上(t *testing.T) {
	v := directorRules().CheckConnection("image", "", []Input{in(1, "style")})
	if !v.Valid || v.Operation != "" {
		t.Fatalf("风格 → 图片应为合法且没有建议操作，得到 %+v", v)
	}
}
