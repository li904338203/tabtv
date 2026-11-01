package domain

import (
	"encoding/json"
	"strings"
	"testing"
)

func cfg(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// 选项类控件必须有 options 或 domainFrom，否则前端渲染出空下拉，
// 用户会以为功能坏了。
func TestValidateControlConfig_选项类必须有来源(t *testing.T) {
	for _, ct := range []string{"select", "segmented", "grid", "tag_group"} {
		if err := ValidateControlConfig(ct, cfg(t, map[string]any{})); err == nil {
			t.Errorf("%s 缺少 options/domainFrom 应被拒绝", ct)
		}
		// 懒加载大列表（模型/风格/音色）用 domainFrom，不内联下发。
		if err := ValidateControlConfig(ct, cfg(t, map[string]any{"domainFrom": "models"})); err != nil {
			t.Errorf("%s 声明 domainFrom 应通过: %v", ct, err)
		}
		ok := map[string]any{"options": []Option{{Value: "a", Label: "甲"}}}
		if err := ValidateControlConfig(ct, cfg(t, ok)); err != nil {
			t.Errorf("%s 正常配置应通过: %v", ct, err)
		}
	}
}

// 重复 value 会让前端选中态错乱：选 A 高亮 B。
func TestValidateControlConfig_选项value不得重复(t *testing.T) {
	bad := map[string]any{"options": []Option{
		{Value: "16:9", Label: "横屏"}, {Value: "16:9", Label: "宽屏"},
	}}
	err := ValidateControlConfig("select", cfg(t, bad))
	if err == nil || !strings.Contains(err.Error(), "重复") {
		t.Fatalf("重复 value 应被拒绝，实际 %v", err)
	}
}

func TestValidateControlConfig_选项缺字段(t *testing.T) {
	for _, bad := range []map[string]any{
		{"options": []Option{{Value: "", Label: "甲"}}},
		{"options": []Option{{Value: "a", Label: ""}}},
	} {
		if err := ValidateControlConfig("select", cfg(t, bad)); err == nil {
			t.Errorf("选项缺字段应被拒绝: %v", bad)
		}
	}
}

func TestValidateControlConfig_slider(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	cases := []struct {
		name string
		c    ControlConfig
		ok   bool
	}{
		{"正常", ControlConfig{Min: f(1), Max: f(60), Step: f(1)}, true},
		{"缺 min", ControlConfig{Max: f(60)}, false},
		{"缺 max", ControlConfig{Min: f(1)}, false},
		{"min ≥ max", ControlConfig{Min: f(60), Max: f(1)}, false},
		{"step 为 0", ControlConfig{Min: f(1), Max: f(60), Step: f(0)}, false},
		// step 大于区间会让滑块根本动不了。
		{"step 超过区间", ControlConfig{Min: f(1), Max: f(5), Step: f(10)}, false},
	}
	for _, c := range cases {
		err := ValidateControlConfig("slider", cfg(t, c.c))
		if (err == nil) != c.ok {
			t.Errorf("%s: 期望 ok=%v，实际 err=%v", c.name, c.ok, err)
		}
	}
}

// 上传控件的张数/体积是**配置不是代码**：写死在前端就等于把配置驱动作废。
func TestValidateControlConfig_upload(t *testing.T) {
	n := func(v int) *int { return &v }
	sz := func(v int64) *int64 { return &v }

	ok := ControlConfig{MaxCount: n(6), MaxSizeBytes: sz(50 << 20), AcceptMIME: []string{"image/png"}}
	if err := ValidateControlConfig("upload", cfg(t, ok)); err != nil {
		t.Errorf("正常上传配置应通过: %v", err)
	}
	// 不声明 accept 等于允许任意文件，用户传完才被服务端拒。
	noAccept := ControlConfig{MaxCount: n(1), MaxSizeBytes: sz(1024)}
	if err := ValidateControlConfig("upload", cfg(t, noAccept)); err == nil {
		t.Error("upload 未声明 accept 应被拒绝")
	}
	for _, bad := range []ControlConfig{
		{MaxCount: n(0), AcceptMIME: []string{"image/png"}},
		{MaxSizeBytes: sz(0), AcceptMIME: []string{"image/png"}},
	} {
		if err := ValidateControlConfig("upload", cfg(t, bad)); err == nil {
			t.Errorf("非法上传配置应被拒绝: %+v", bad)
		}
	}
}

func TestValidateControlConfig_未知控件(t *testing.T) {
	if err := ValidateControlConfig("magic", nil); err == nil {
		t.Fatal("未知控件类型应被拒绝")
	}
}

func TestValidateControlConfig_非法JSON(t *testing.T) {
	if err := ValidateControlConfig("switch", []byte("{not json")); err == nil {
		t.Fatal("非法 JSON 应被拒绝")
	}
}

// 合并是**逐字段覆盖**，不是整体替换：
// 整体替换会让节点覆盖 maxCount 时把 accept 一并清空。
func TestMergeConfig_逐字段覆盖(t *testing.T) {
	base := cfg(t, map[string]any{"maxCount": 1, "accept": []string{"image/png"}, "maxSize": 1024})
	nodeOverride := cfg(t, map[string]any{"maxCount": 6})
	modelOverride := cfg(t, map[string]any{"maxSize": 2048})

	merged, err := MergeConfig(base, nodeOverride, modelOverride)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(merged, &m); err != nil {
		t.Fatal(err)
	}
	if m["maxCount"].(float64) != 6 {
		t.Errorf("节点覆盖未生效: maxCount=%v", m["maxCount"])
	}
	if m["maxSize"].(float64) != 2048 {
		t.Errorf("模型覆盖未生效: maxSize=%v", m["maxSize"])
	}
	if _, ok := m["accept"]; !ok {
		t.Error("未被覆盖的字段应保留，accept 被整体替换掉了")
	}
}

func TestMergeConfig_空层被跳过(t *testing.T) {
	merged, err := MergeConfig(nil, cfg(t, map[string]any{"a": 1}), nil)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	_ = json.Unmarshal(merged, &m)
	if m["a"].(float64) != 1 {
		t.Errorf("合并结果 = %v", m)
	}
}
