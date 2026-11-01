package domain

import (
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"os/exec"
	"strings"
	"testing"
)

var photo = SourceFacts{MediaID: 7, Width: 1536, Height: 1024}

func TestOperationOf_工具节点一律图生图(t *testing.T) {
	for _, tool := range []string{"multi_angle", "relight", "expression_edit", "upscale"} {
		if got := OperationOf(map[string]any{"tool": tool}); got != OpImg2Img {
			t.Errorf("tool=%s：%q，期望 img2img", tool, got)
		}
	}
	if OperationOf(map[string]any{"tool": "blur"}) != "" || OperationOf(map[string]any{"operation": "img2img"}) != "" {
		t.Error("不是工具节点时交给常规判定")
	}
}

func TestPrepare_未知字段与未知工具报错并带路径(t *testing.T) {
	_, err := Prepare(ToolMultiAngle, map[string]any{"horizontl": 90}, photo)
	var fe *FieldError
	if !errors.As(err, &fe) || fe.Path != "toolOptions.horizontl" || !strings.Contains(fe.Msg, "不认识的选项") {
		t.Fatalf("键名写错要报错并指到那个字段：%v", err)
	}
	_, err = Prepare(ToolMultiAngle, map[string]any{"horizontal": "九十"}, photo)
	if !errors.As(err, &fe) || fe.Path != "toolOptions.horizontal" {
		t.Fatalf("类型不对要指到字段：%v", err)
	}
	if _, err := Prepare("blur", map[string]any{}, photo); err == nil {
		t.Fatal("未知工具要报错")
	}
}

func TestPrepare_通用参数跟着源图(t *testing.T) {
	p, err := Prepare(ToolRelight, RelightOptions{Smart: true}, photo)
	if err != nil {
		t.Fatal(err)
	}
	if p.Params["aspect_ratio"] != "3:2" || p.Params["quality"] != "high" || p.Count != 1 {
		t.Fatalf("1536×1024 → 3:2、高画质：%v", p.Params)
	}
	if NearestRatio(1080, 1920) != "9:16" || NearestRatio(0, 10) != "" {
		t.Fatal("画幅取最接近的标准值；尺寸未知不给")
	}
}

func TestMultiAngle_机位翻译成提示词(t *testing.T) {
	zoom := 9
	p, err := Prepare(ToolMultiAngle, MultiAngleOptions{Preset: "custom", Horizontal: 180, Vertical: 0, Zoom: &zoom}, photo)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"背面视角", "平视", "景别为特写", MultiAngleConstraint} {
		if !strings.Contains(p.Prompt, want) {
			t.Errorf("提示词缺 %q：%s", want, p.Prompt)
		}
	}
	fish, _ := Prepare(ToolMultiAngle, MultiAngleOptions{Preset: "fisheye", Prompt: "夜景"}, photo)
	if !strings.Contains(fish.Prompt, "鱼眼镜头") || !strings.Contains(fish.Prompt, "夜景。") {
		t.Fatalf("预设附加句 + 用户补充：%s", fish.Prompt)
	}
	if !strings.Contains(HorizontalPhrase(45), "右前侧") || !strings.Contains(HorizontalPhrase(300), "左前侧") ||
		!strings.Contains(VerticalPhrase(80), "鸟瞰") || !strings.Contains(VerticalPhrase(-30), "仰拍") {
		t.Fatal("方位与俯仰描述")
	}
	for _, bad := range []MultiAngleOptions{{Horizontal: 361}, {Vertical: -31}, {Vertical: 91}, {Zoom: ptr(11)},
		{Preset: "drone"}, {Prompt: strings.Repeat("字", 201)}} {
		if _, err := Prepare(ToolMultiAngle, bad, photo); err == nil {
			t.Errorf("应拒绝 %+v", bad)
		}
	}
}

func TestZoomLabel_分档边界(t *testing.T) {
	cases := map[int]string{0: "大远景", 1: "大远景", 2: "远景", 3: "远景", 4: "中景", 6: "中景", 7: "近景", 8: "近景", 9: "特写", 10: "特写"}
	for z, want := range cases {
		if got := ZoomLabel(z); got != want {
			t.Errorf("zoom=%d：%s，期望 %s", z, got, want)
		}
	}
}

func TestLightDirection_十二个描述(t *testing.T) {
	cases := []struct {
		az, el int
		want   string
	}{
		{0, 0, "正前方"}, {45, 0, "右前方"}, {90, 0, "右侧"}, {135, 0, "右后方（逆光）"},
		{180, 0, "正后方（逆光）"}, {225, 0, "左后方（逆光）"}, {270, 0, "左侧"}, {315, 0, "左前方"},
		{340, 0, "正前方"}, {0, 80, "正上方（顶光）"}, {0, -80, "正下方（底光）"}, {90, 30, "右侧略高处"},
	}
	for _, c := range cases {
		if got := LightDirection(c.az, c.el); got != c.want {
			t.Errorf("(%d,%d)：%s，期望 %s", c.az, c.el, got, c.want)
		}
	}
}

func TestRelight_提示词与智能模式(t *testing.T) {
	b, c := 90, "#FFB040"
	p, err := Prepare(ToolRelight, RelightOptions{Azimuth: 270, Brightness: &b, Color: &c, Rim: true}, photo)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"左侧", "很亮", "琥珀", "轮廓光", RelightConstraint} {
		if !strings.Contains(p.Prompt, want) {
			t.Errorf("提示词缺 %q：%s", want, p.Prompt)
		}
	}
	smart, _ := Prepare(ToolRelight, RelightOptions{Smart: true, Azimuth: 90, Brightness: &b, Rim: true}, photo)
	if strings.Contains(smart.Prompt, "轮廓光") || len(smart.Options) != 1 {
		t.Fatalf("智能模式忽略其余字段且不进快照：%s %v", smart.Prompt, smart.Options)
	}
	bad := "red"
	if _, err := Prepare(ToolRelight, RelightOptions{Color: &bad}, photo); err == nil {
		t.Fatal("颜色格式要校验")
	}
}

func TestUpscale_三档提示词(t *testing.T) {
	p, err := Prepare(ToolUpscale, UpscaleOptions{Level: "ultra"}, photo)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(p.Prompt, "极致") || !strings.Contains(p.Prompt, UpscaleConstraint) || p.Params["quality"] != "high" {
		t.Fatalf("极致档：%s %v", p.Prompt, p.Params)
	}
	def, _ := Prepare(ToolUpscale, nil, photo)
	if def.Options["level"] != "standard" {
		t.Fatalf("缺省标准档：%v", def.Options)
	}
	if _, err := Prepare(ToolUpscale, map[string]any{"level": "8k"}, photo); err == nil {
		t.Fatal("未知档要拒")
	}
	if _, err := Prepare(ToolUpscale, map[string]any{"factor": 4}, photo); err == nil {
		t.Fatal("旧的 factor 字段要拒（键名不认识就报错）")
	}
}

func TestEmotions_二十五格与中心(t *testing.T) {
	if len(Emotions) != 25 {
		t.Fatalf("应有 25 格，得到 %d", len(Emotions))
	}
	if e, _ := EmotionAt(0, 0); e.Name != "淡然自若" {
		t.Fatalf("中心应是淡然自若：%s", e.Name)
	}
	seen := map[string]bool{}
	for _, e := range Emotions {
		if seen[e.Name] || e.English == "" {
			t.Fatalf("名称重复或缺英文：%+v", e)
		}
		seen[e.Name] = true
	}
}

func TestExpression_裁切区域与提示词(t *testing.T) {
	two := []Role{
		{ID: "r2", Box: Box{X: 900, Y: 200, W: 200, H: 200}, Arousal: 2, Warmth: 2},
		{ID: "r1", Box: Box{X: 100, Y: 200, W: 200, H: 200}, Arousal: -1, Warmth: 0},
	}
	p, err := Prepare(ToolExpression, ExpressionOptions{SourceMediaID: "7", Count: 2, Roles: two}, photo)
	if err != nil {
		t.Fatal(err)
	}
	// 外接框 (100,200)-(1100,400)，外扩 25%（250 / 50）→ (0,150)-(1350,450)，左边夹到 0。
	if *p.Crop != (Box{X: 0, Y: 150, W: 1350, H: 300}) {
		t.Fatalf("裁切区域：%+v", *p.Crop)
	}
	if p.Count != 2 || p.Params["aspect_ratio"] != "21:9" {
		t.Fatalf("张数与按裁切区域定画幅：%d %v", p.Count, p.Params)
	}
	if !strings.Contains(p.Prompt, "局部画面") || strings.Index(p.Prompt, "若有所思") > strings.Index(p.Prompt, "欣喜若狂") ||
		!strings.Contains(p.Prompt, ExpressionConstraint) {
		t.Fatalf("从左到右、局部图的说明与约束句：%s", p.Prompt)
	}
	one, _ := Prepare(ToolExpression, ExpressionOptions{SourceMediaID: "7", Roles: two[:1]}, photo)
	if !strings.Contains(one.Prompt, "把画面主体的面部表情调整为欣喜若狂") {
		t.Fatalf("单人不写位置：%s", one.Prompt)
	}
	var sc *ErrSourceChanged
	if _, err := Prepare(ToolExpression, ExpressionOptions{SourceMediaID: "8", Roles: two}, photo); !errors.As(err, &sc) {
		t.Fatalf("源图变了要拒：%v", err)
	}
	seven := make([]Role, 7)
	for i := range seven {
		seven[i] = Role{Box: Box{X: i * 200, Y: 0, W: 100, H: 100}}
	}
	for _, bad := range []ExpressionOptions{
		{SourceMediaID: "7"},
		{SourceMediaID: "7", Roles: seven},
		{SourceMediaID: "7", Roles: []Role{{Box: Box{X: 0, Y: 0, W: 10, H: 10}}}},
		{SourceMediaID: "7", Roles: []Role{{Box: Box{X: 1500, Y: 0, W: 100, H: 100}}}},
		{SourceMediaID: "7", Roles: []Role{{Box: Box{W: 100, H: 100}, Arousal: 3}}},
		{SourceMediaID: "7", Count: 5, Roles: two},
	} {
		if _, err := Prepare(ToolExpression, bad, photo); err == nil {
			t.Errorf("应拒绝 %+v", bad)
		}
	}
	if _, err := Prepare(ToolExpression, map[string]any{"sourceMediaId": "7", "resolution": "2k",
		"roles": []any{}}, photo); err == nil {
		t.Fatal("旧的 resolution 字段要拒")
	}
	if CountOf(ToolExpression, map[string]any{"count": float64(3)}) != 3 || CountOf(ToolUpscale, nil) != 1 {
		t.Fatal("报价张数")
	}
}

// IMG-AC-16 的核心：框外逐像素等于底图，框内换成编辑结果。
func TestComposite_框外逐像素不变(t *testing.T) {
	base := image.NewRGBA(image.Rect(0, 0, 200, 100))
	edited := image.NewRGBA(image.Rect(0, 0, 400, 200))
	for y := 0; y < 100; y++ {
		for x := 0; x < 200; x++ {
			base.Set(x, y, color.RGBA{uint8(x), uint8(y), 50, 255})
		}
	}
	for y := 0; y < 200; y++ {
		for x := 0; x < 400; x++ {
			edited.Set(x, y, color.RGBA{255, 0, 0, 255})
		}
	}
	box := Box{X: 60, Y: 30, W: 40, H: 40}
	out := Composite(base, edited, []Box{box})
	pad := int(float64(40) * BoxExpand)
	region := image.Rect(box.X-pad, box.Y-pad, box.X+box.W+pad, box.Y+box.H+pad)
	for y := 0; y < 100; y++ {
		for x := 0; x < 200; x++ {
			if (image.Point{x, y}).In(region) {
				continue
			}
			if out.RGBAAt(x, y) != base.RGBAAt(x, y) {
				t.Fatalf("框外 (%d,%d) 变了", x, y)
			}
		}
	}
	if c := out.RGBAAt(80, 50); c.R != 255 || c.G != 0 {
		t.Fatalf("框中心应是编辑结果：%v", c)
	}
}

// IMG-I-04：下发给前端的配置齐全，且**不含**提示词（附加句、英文情绪描述、高清各档的提示词）。
func TestBuildUIConfig_齐全且不漏提示词(t *testing.T) {
	c := BuildUIConfig()
	if len(c.Tools) != 4 || len(c.Emotions) != 25 || len(c.MultiAnglePresets) != 7 || len(c.LightPresets) != 6 ||
		len(c.NamedColors) != 16 || len(c.UpscaleLevels) != 3 {
		t.Fatalf("配置不齐：%+v", c)
	}
	raw, _ := json.Marshal(c)
	for _, leak := range []string{"鱼眼镜头", "calm and composed", "高清重绘", "fal-ai", "fal.ai", "Topaz"} {
		if strings.Contains(string(raw), leak) {
			t.Fatalf("下发配置不该含 %q", leak)
		}
	}
}

// 纯计算包的绊线：只依赖标准库与 x/image（不碰库、不发网络）。
func TestDeps_只依赖标准库与xImage(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", ".").Output()
	if err != nil {
		t.Skip("go list 不可用")
	}
	for _, dep := range strings.Fields(string(out)) {
		if strings.Contains(dep, ".") && !strings.HasPrefix(dep, "golang.org/x/image") &&
			!strings.HasPrefix(dep, "github.com/li904338203/tabtv/backend/internal/imagetool/domain") && !strings.HasPrefix(dep, "vendor/") {
			t.Fatalf("纯计算包不该依赖 %s", dep)
		}
	}
}

func ptr(n int) *int { return &n }

// 上游内容审核（2026-10-31 实测「怒不可遏」被拒）：提示词里不出现激烈词与「身份」。
func TestPrompts_避开内容审核敏感词(t *testing.T) {
	var all []string
	for _, e := range Emotions {
		all = append(all, e.English)
	}
	all = append(all, ExpressionConstraint, MultiAngleConstraint, RelightConstraint, UpscaleConstraint)
	for _, s := range all {
		for _, bad := range []string{"furious", "shout", "glare", "hopeless", "despondent", "身份"} {
			if strings.Contains(strings.ToLower(s), bad) {
				t.Fatalf("%q 含敏感词 %q", s, bad)
			}
		}
	}
}
