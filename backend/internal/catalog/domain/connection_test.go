package domain

import (
	"strings"
	"testing"
)

// 连线矩阵（03 §2.6 / 01 §3）：
// 文→图/视/音/文 · 图→文/图/视 · 视→文/视 · 音→视 · 风格→仅图片 · 特效→仅视频
func standardRules() RuleSet {
	return NewRuleSet([]Rule{
		{"text", "text", true, "text2text"},
		{"text", "image", true, "text2img"},
		{"text", "video", true, "text2video"},
		{"text", "audio", true, "text2audio"},
		{"image", "text", true, "img2prompt"},
		{"image", "image", true, "img2img"},
		{"image", "video", true, "img2video"},
		{"video", "text", true, "video2prompt"},
		{"video", "video", true, "video2video"},
		{"audio", "video", true, "ref2video"},
		{"style", "image", true, ""},
		{"effect", "video", true, ""},
	})
}

func in(order int, src string) Input { return Input{Order: order, SourceType: src} }

// 逐格验证矩阵，尤其是**不允许**的组合。
func TestRuleSet_矩阵(t *testing.T) {
	rs := standardRules()
	allowed := [][2]string{
		{"text", "text"}, {"text", "image"}, {"text", "video"}, {"text", "audio"},
		{"image", "text"}, {"image", "image"}, {"image", "video"},
		{"video", "text"}, {"video", "video"},
		{"audio", "video"},
		{"style", "image"}, {"effect", "video"},
	}
	for _, p := range allowed {
		if err := rs.ValidateConnections(p[1], []Input{in(1, p[0])}); err != nil {
			t.Errorf("%s→%s 应被允许: %v", p[0], p[1], err)
		}
	}

	denied := [][2]string{
		{"audio", "image"}, {"audio", "text"}, {"audio", "audio"},
		{"video", "image"}, {"video", "audio"},
		{"image", "audio"},
		// ⚠️ 风格只修饰图片、特效只修饰视频，不能互换。
		{"style", "video"}, {"style", "text"}, {"style", "audio"},
		{"effect", "image"}, {"effect", "text"}, {"effect", "audio"},
	}
	for _, p := range denied {
		if err := rs.ValidateConnections(p[1], []Input{in(1, p[0])}); err == nil {
			t.Errorf("%s→%s 应被拒绝", p[0], p[1])
		}
	}
}

// order 断号/重号会让首尾帧与参考图锚定错位，而且**不会报错**。
func TestValidateInputOrder(t *testing.T) {
	ok := []Input{in(1, "image"), in(2, "image"), in(3, "image")}
	if err := ValidateInputOrder(ok); err != nil {
		t.Errorf("连续 order 应通过: %v", err)
	}
	// 乱序但集合连续 —— 应通过（顺序由 order 值决定，不是数组下标）。
	if err := ValidateInputOrder([]Input{in(3, "image"), in(1, "image"), in(2, "image")}); err != nil {
		t.Errorf("乱序但连续应通过: %v", err)
	}
	for _, bad := range [][]Input{
		{in(0, "image")},                 // 从 0 起
		{in(1, "image"), in(3, "image")}, // 断号
		{in(1, "image"), in(1, "image")}, // 重号
		{in(2, "image"), in(3, "image")}, // 不从 1 起
	} {
		if err := ValidateInputOrder(bad); err == nil {
			t.Errorf("非法 order 应被拒绝: %v", bad)
		}
	}
	if err := ValidateInputOrder(nil); err != nil {
		t.Errorf("空输入应通过: %v", err)
	}
}

func TestValidateVideoMode(t *testing.T) {
	cases := []struct {
		mode   string
		inputs []Input
		ok     bool
		why    string
	}{
		{"text2video", nil, true, "无输入"},
		{"text2video", []Input{in(1, "text")}, true, "纯文本"},
		{"text2video", []Input{in(1, "image")}, false, "文生视频不接受图片"},

		{"img2video", []Input{in(1, "image")}, true, "恰好 1 图"},
		{"img2video", []Input{in(1, "image"), in(2, "image")}, false, "2 图超出"},
		{"img2video", nil, false, "缺图"},

		{"first_last_frame", []Input{in(1, "image"), in(2, "image")}, true, "首尾帧 2 图"},
		{"first_last_frame", []Input{in(1, "image")}, false, "只有 1 图"},
		{"first_last_frame", []Input{in(1, "image"), in(2, "image"), in(3, "image")}, false, "3 图"},

		{"ref2video", []Input{in(1, "image"), in(2, "audio")}, true, "图+音参考"},
		// ⚠️ DRK-12（3D 导演台）起规则层的 ref2video 也收参考视频：某个模型收不收由能力矩阵判断（GAP-22 的两层仍在，
		// 只是这一格放开了）。图生视频这类操作仍然不收视频，见下面的 img2video 用例。
		{"ref2video", []Input{in(1, "video")}, true, "参考视频（由能力矩阵把关）"},
		{"ref2video", nil, false, "至少要一个参考"},
		{"img2video", []Input{in(1, "video")}, false, "图生视频不收视频"},

		{"video2video", []Input{in(1, "video")}, true, "恰好 1 视频"},
		{"video2video", []Input{in(1, "image")}, false, "不接受图片"},

		{"unknown_mode", nil, false, "未知模式"},
	}
	for _, c := range cases {
		err := ValidateVideoMode(c.mode, c.inputs)
		if (err == nil) != c.ok {
			t.Errorf("%s (%s): 期望 ok=%v，实际 err=%v", c.mode, c.why, c.ok, err)
		}
	}
}

// style/effect 是修饰输入，不计入媒体数量，但连线本身仍要被允许。
func TestValidateVideoMode_修饰输入不计数(t *testing.T) {
	// 1 图 + 1 特效，img2video 仍应通过（特效不占媒体名额）。
	if err := ValidateVideoMode("img2video", []Input{in(1, "image"), in(2, "effect")}); err != nil {
		t.Errorf("特效不应计入媒体数量: %v", err)
	}
	// 纯特效则没有媒体输入，img2video 应失败。
	if err := ValidateVideoMode("img2video", []Input{in(1, "effect")}); err == nil {
		t.Error("只有特效没有图片，img2video 应失败")
	}
}

// 视频节点以 mode 为准；default_operation 只是默认建议，多义时不得覆盖 mode。
func TestResolveOperation(t *testing.T) {
	rs := standardRules()

	// 图片输入连到视频节点，规则的 default 是 img2video，
	// 但用户选了 first_last_frame —— 必须以 mode 为准。
	op, err := rs.ResolveOperation("video", "first_last_frame",
		[]Input{in(1, "image"), in(2, "image")})
	if err != nil {
		t.Fatal(err)
	}
	if op != "first_last_frame" {
		t.Errorf("视频节点应以 mode 为准，实际 %q", op)
	}

	// 视频节点不给 mode 必须报错，不能猜。
	if _, err := rs.ResolveOperation("video", "", []Input{in(1, "image")}); err == nil {
		t.Error("视频节点缺 mode 应报错")
	}
	if _, err := rs.ResolveOperation("video", "bogus", nil); err == nil {
		t.Error("未知 mode 应报错")
	}

	// 非视频节点用规则的 default_operation。
	op, err = rs.ResolveOperation("image", "", []Input{in(1, "text")})
	if err != nil {
		t.Fatal(err)
	}
	if op != "text2img" {
		t.Errorf("期望 text2img，实际 %q", op)
	}

	// 素材节点无 default_operation，单独连不出操作。
	if _, err := rs.ResolveOperation("image", "", []Input{in(1, "style")}); err == nil {
		t.Error("仅素材输入应无法判定 operation")
	}
}

func TestValidateConnections_order先校验(t *testing.T) {
	rs := standardRules()
	// order 非法时应先报 order 问题，而不是先报连线不允许。
	err := rs.ValidateConnections("video", []Input{in(2, "image"), in(3, "image")})
	if err == nil || !strings.Contains(err.Error(), "order") {
		t.Fatalf("应先报 order 错误，实际 %v", err)
	}
}

// DRK-12：全能参考收参考视频；与图、音频混用照样计入媒体数量（上限 8）。
func TestValidateVideoMode_全能参考收视频(t *testing.T) {
	if err := ValidateVideoMode("ref2video", []Input{in(1, "image"), in(2, "video")}); err != nil {
		t.Fatalf("全能参考应接受 图 + 参考视频：%v", err)
	}
	if err := ValidateVideoMode("ref2video", []Input{in(1, "director"), in(2, "video"), in(3, "audio")}); err != nil {
		t.Fatalf("全能参考应接受 导演台截图 + 预演视频 + 音频：%v", err)
	}
}

func TestValidateVideoMode_全能参考仍需至少一个媒体(t *testing.T) {
	if err := ValidateVideoMode("ref2video", []Input{in(1, "text")}); err == nil {
		t.Fatal("全能参考只有文本时应被拒绝")
	}
}
