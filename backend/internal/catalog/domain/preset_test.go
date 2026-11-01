package domain

import (
	"encoding/json"
	"strings"
	"testing"
)

func lightPreset() Preset {
	return Preset{
		Code: "relight", Name: "重新打光", Group: "tuning", Spec: "retouch",
		InputMode: InputImageRequired,
		Prompt:    "保持人物与构图不变，只改变光照。",
		Dimensions: []PresetDimension{
			{
				Code: "direction", Name: "光线方向", Default: "front",
				Options: []PresetOption{
					{Value: "front", Label: "正面光", Prompt: "光源来自正前方，面部均匀受光。"},
					{Value: "rim", Label: "轮廓光", Prompt: "光源来自背侧，勾出人物轮廓。"},
				},
			},
			{
				Code: "mood", Name: "氛围", Default: "natural",
				Options: []PresetOption{
					{Value: "natural", Label: "自然", Prompt: "色温自然，不做风格化。"},
					{Value: "warm", Label: "暖调", Prompt: "整体偏暖，接近黄昏色温。"},
				},
			},
		},
	}
}

// ---------------------------------------------------------------- 输入校验

// ⚠️ 不校验的话，用户对一个空节点点"打光"，钱扣了、上游收到一句没有图的
// 提示词，出来一张跟他的照片毫无关系的图。
func TestValidatePresetInput_必须有图的预设(t *testing.T) {
	p := lightPreset()
	if err := ValidatePresetInput(p, false, "随便写点"); err == nil {
		t.Fatal("没有图时应被拒绝")
	} else if !strings.Contains(err.Error(), "重新打光") {
		t.Fatalf("错误要说清是哪个工具: %v", err)
	}
	if err := ValidatePresetInput(p, true, ""); err != nil {
		t.Fatalf("有图就该放行: %v", err)
	}
}

func TestValidatePresetInput_必须有文字的预设(t *testing.T) {
	p := Preset{Name: "文生图", InputMode: InputTextRequired}
	if err := ValidatePresetInput(p, true, "   "); err == nil {
		t.Fatal("空白文字应被拒绝（全是空格也是空）")
	}
	if err := ValidatePresetInput(p, false, "一只猫"); err != nil {
		t.Fatalf("有文字就该放行: %v", err)
	}
}

func TestValidatePresetInput_两者有其一(t *testing.T) {
	p := Preset{Name: "通用", InputMode: InputEither}
	if err := ValidatePresetInput(p, false, ""); err == nil {
		t.Fatal("两样都没有应被拒绝")
	}
	if err := ValidatePresetInput(p, true, ""); err != nil {
		t.Fatal("只有图应放行")
	}
	if err := ValidatePresetInput(p, false, "词"); err != nil {
		t.Fatal("只有词应放行")
	}
}

// ---------------------------------------------------------------- 拼接

// ⚠️ 跳过未选维度的话，同一个预设会"时有时无地缺几句"：
// 打开过面板的用户和没打开过的用户拿到的是两种效果，而他们选的是同一个预设。
func TestComposePreset_没选过的维度按默认出词(t *testing.T) {
	got := ComposePreset(lightPreset(), nil)
	if !strings.Contains(got, "正前方") {
		t.Fatalf("未选时应用 direction 的默认值:\n%s", got)
	}
	if !strings.Contains(got, "色温自然") {
		t.Fatalf("未选时应用 mood 的默认值:\n%s", got)
	}
}

func TestComposePreset_选过的维度用选中的(t *testing.T) {
	got := ComposePreset(lightPreset(), map[string]string{"direction": "rim"})
	if strings.Contains(got, "正前方") {
		t.Fatalf("选了轮廓光就不该再出正面光:\n%s", got)
	}
	if !strings.Contains(got, "勾出人物轮廓") {
		t.Fatalf("应出轮廓光的词:\n%s", got)
	}
	// 没选的那一维仍然按默认出词。
	if !strings.Contains(got, "色温自然") {
		t.Fatalf("另一维应仍用默认:\n%s", got)
	}
}

func TestComposePreset_正文在维度之前(t *testing.T) {
	got := ComposePreset(lightPreset(), nil)
	if strings.Index(got, "保持人物与构图不变") > strings.Index(got, "正前方") {
		t.Fatalf("预设正文应排在维度微调之前:\n%s", got)
	}
}

func TestComposePreset_不认识的取值当作没选(t *testing.T) {
	// 后台下线了某个选项，而历史节点还存着它 —— 不能因此少一整段词。
	got := ComposePreset(lightPreset(), map[string]string{"direction": "已下线的值"})
	if strings.Contains(got, "正前方") || strings.Contains(got, "轮廓") {
		t.Fatalf("匹配不到取值时这一维不出词，但别的维要照常:\n%s", got)
	}
	if !strings.Contains(got, "色温自然") {
		t.Fatalf("别的维应照常出词:\n%s", got)
	}
}

func TestComposePreset_没有正文也没有维度时为空(t *testing.T) {
	if got := ComposePreset(Preset{Code: "x"}, nil); got != "" {
		t.Fatalf("应为空: %q", got)
	}
}

// ---------------------------------------------------------------- 参数

// ⚠️ 反过来的话，用户调好画幅再选一个预设，画幅被悄悄改回去了 ——
// 而他完全不知道是谁改的。
func TestApplyPresetParams_用户设过的值优先(t *testing.T) {
	node := json.RawMessage(`{"aspectRatio":"16:9","prompt":"我的词"}`)
	preset := json.RawMessage(`{"aspectRatio":"1:1","quality":"high"}`)
	out := ApplyPresetParams(node, preset)

	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	if m["aspectRatio"] != "16:9" {
		t.Fatalf("用户设过的画幅不该被预设覆盖: %v", m["aspectRatio"])
	}
	if m["quality"] != "high" {
		t.Fatalf("用户没设过的应由预设补上: %v", m["quality"])
	}
	if m["prompt"] != "我的词" {
		t.Fatal("用户的其他参数要原样保留")
	}
}

func TestApplyPresetParams_坏数据原样返回(t *testing.T) {
	node := json.RawMessage(`{"a":1}`)
	if got := string(ApplyPresetParams(node, []byte(`{坏的`))); got != `{"a":1}` {
		t.Fatalf("预设参数坏了不该弄丢节点参数: %s", got)
	}
	if got := string(ApplyPresetParams(nil, []byte(`{"b":2}`))); !strings.Contains(got, `"b"`) {
		t.Fatalf("节点参数为空时应只有预设的: %s", got)
	}
}

// ---------------------------------------------------------------- 其他

func TestParseDimensions_坏数据返回空而不是报错(t *testing.T) {
	// ⚠️ 一个维度配坏了不该让整个预设不可用 —— 正文还在，
	// 用户至少能用默认效果出图。
	if got := ParseDimensions([]byte(`{不是数组`)); got != nil {
		t.Fatalf("应返回空: %v", got)
	}
	if got := ParseDimensions(nil); got != nil {
		t.Fatalf("空输入应返回空: %v", got)
	}
}

// ⚠️ 按字典序排的话，"空间与机位"会排在"设定图"前面 ——
// 而用户的工作顺序是先出设定图再调空间。
func TestSortPresets_按分组的工作顺序(t *testing.T) {
	type row struct {
		g string
		s int
	}
	items := []row{{"space", 1}, {"sheet", 2}, {"tuning", 1}, {"sheet", 1}}
	SortPresets(items, func(r row) string { return r.g }, func(r row) int { return r.s })
	got := make([]string, 0, len(items))
	for _, r := range items {
		got = append(got, r.g)
	}
	if strings.Join(got, ",") != "sheet,sheet,tuning,space" {
		t.Fatalf("分组顺序不对: %v", got)
	}
	if items[0].s != 1 {
		t.Fatal("同组内应按 sort 排")
	}
}
