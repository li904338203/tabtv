package domain

import (
	"strings"
	"testing"
)

func okModel() ModelFields {
	return ModelFields{Code: "gpt_image_2", Name: "图片模型", Domain: "image",
		UpstreamModelCode: "gpt-image-2", SubmitPath: "/images/generations", Status: "disabled", EstSeconds: 30}
}

func TestValidateModelFields(t *testing.T) {
	if err := ValidateModelFields(okModel()); err != nil {
		t.Fatalf("合法的模型应通过：%v", err)
	}
	// 现有数据的编码形态都要放行（含连字符与数字开头的时间戳后缀）
	for _, code := range []string{"fake-image-1790146008", "kling_3_0_omni", "claude_opus_4_8"} {
		m := okModel()
		m.Code = code
		if err := ValidateModelFields(m); err != nil {
			t.Errorf("编码 %q 应通过：%v", code, err)
		}
	}
	bad := map[string]func(*ModelFields){
		"大写编码":    func(m *ModelFields) { m.Code = "GPT" },
		"空格编码":    func(m *ModelFields) { m.Code = "gpt image" },
		"未知产出":    func(m *ModelFields) { m.Domain = "3d" },
		"未知状态":    func(m *ModelFields) { m.Status = "on" },
		"名称超长":    func(m *ModelFields) { m.Name = strings.Repeat("字", 65) },
		"上游名为空":   func(m *ModelFields) { m.UpstreamModelCode = " " },
		"路径超长":    func(m *ModelFields) { m.SubmitPath = "/" + strings.Repeat("a", 256) },
		"输入上限为 0": func(m *ModelFields) { z := 0; m.MaxInputChars = &z },
		"估时过大":    func(m *ModelFields) { m.EstSeconds = 3601 },
	}
	for name, mutate := range bad {
		m := okModel()
		mutate(&m)
		if err := ValidateModelFields(m); err == nil {
			t.Errorf("%s 应被拒", name)
		}
	}
	// 名称按字符数：64 个汉字是 192 字节，按字节算会把合法名称挡掉
	m := okModel()
	m.Name = strings.Repeat("字", 64)
	if err := ValidateModelFields(m); err != nil {
		t.Fatalf("64 个字的名称应通过：%v", err)
	}
}

// 视频能力三项（AGT-05）：只对视频模型有意义，时长 1~600 秒。
func TestValidateModelFields_视频能力(t *testing.T) {
	intp := func(v int) *int { return &v }
	boolp := func(v bool) *bool { return &v }
	video := func() ModelFields {
		m := okModel()
		m.Domain = "video"
		return m
	}
	cases := []struct {
		name    string
		m       ModelFields
		wantErr string // 空 = 应通过；否则错误里必须点名这个字段
	}{
		{"视频模型可设三项", func() ModelFields {
			m := video()
			m.MaxDurationSec, m.IsMultiShot, m.HasNativeAudio = intp(10), boolp(true), boolp(true)
			return m
		}(), ""},
		{"视频模型时长取上限", func() ModelFields { m := video(); m.MaxDurationSec = intp(600); return m }(), ""},
		{"视频模型不设三项", video(), ""},
		{"图片模型传默认值放行", func() ModelFields {
			m := okModel()
			m.IsMultiShot, m.HasNativeAudio = boolp(false), boolp(false)
			return m
		}(), ""},
		{"图片模型设时长被拒", func() ModelFields { m := okModel(); m.MaxDurationSec = intp(10); return m }(), "maxDurationSec"},
		{"图片模型设多镜头被拒", func() ModelFields { m := okModel(); m.IsMultiShot = boolp(true); return m }(), "isMultiShot"},
		{"音频模型设音轨被拒", func() ModelFields {
			m := okModel()
			m.Domain = "audio"
			m.HasNativeAudio = boolp(true)
			return m
		}(), "hasNativeAudio"},
		{"时长为0被拒", func() ModelFields { m := video(); m.MaxDurationSec = intp(0); return m }(), "maxDurationSec"},
		{"时长为负被拒", func() ModelFields { m := video(); m.MaxDurationSec = intp(-5); return m }(), "maxDurationSec"},
		{"时长超上限被拒", func() ModelFields { m := video(); m.MaxDurationSec = intp(601); return m }(), "maxDurationSec"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := ValidateModelFields(c.m)
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("应通过：%v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("应被拒且点名 %s，得到 %v", c.wantErr, err)
			}
		})
	}
}

func TestValidateProviderFields(t *testing.T) {
	ok := ProviderFields{Code: "claudecode_anthropic", Name: "中转", AdapterType: "http",
		BaseURL: "https://api.example.com/v1", Headers: []byte(`{"anthropic-version":"2023-06-01","X-Auth-Header-Name":"x-api-key"}`)}
	if err := ValidateProviderFields(ok); err != nil {
		t.Fatalf("合法的供应商应通过：%v", err)
	}
	bad := map[string]func(*ProviderFields){
		"地址无协议": func(p *ProviderFields) { p.BaseURL = "api.example.com" },
		"地址伪协议": func(p *ProviderFields) { p.BaseURL = "file:///etc/passwd" },
		"编码超长":  func(p *ProviderFields) { p.Code = strings.Repeat("a", 33) },
		// ⚠️ 不是字符串对象的请求头在运行时会被静默忽略，连带 X-Auth-Header-Name 一起丢掉
		"请求头是数组":  func(p *ProviderFields) { p.Headers = []byte(`["a"]`) },
		"请求头值是数字": func(p *ProviderFields) { p.Headers = []byte(`{"x-n":1}`) },
		"请求头名为空":  func(p *ProviderFields) { p.Headers = []byte(`{" ":"v"}`) },
	}
	for name, mutate := range bad {
		p := ok
		mutate(&p)
		if err := ValidateProviderFields(p); err == nil {
			t.Errorf("%s 应被拒", name)
		}
	}
}

func TestValidateMaterialAndVoice_列宽(t *testing.T) {
	if err := ValidateMaterial(Material{Code: "Water Color", Name: "水彩", Type: MaterialStyle, Prompt: "p"}); err == nil {
		t.Error("素材编码含大写与空格应被拒")
	}
	if err := ValidateMaterial(Material{Code: "watercolor", Name: "水彩", Type: MaterialStyle, Prompt: "p",
		Category: strings.Repeat("类", 33)}); err == nil {
		t.Error("分类超过 32 字应被拒")
	}
	if err := ValidateVoice(Voice{Code: "voice_1", Name: "音色", Language: strings.Repeat("z", 17)}); err == nil {
		t.Error("语言超过 16 个字符应被拒")
	}
	if err := ValidateVoice(Voice{Code: "voice_1", Name: "音色"}); err != nil {
		t.Errorf("未启用、未配上游的音色可以先存草稿：%v", err)
	}
}

func TestValidateParamDefFields(t *testing.T) {
	for _, code := range []string{"aspect_ratio", "video_mode", "seed", "style_ref"} {
		if err := ValidateParamDefFields(code, "名称"); err != nil {
			t.Errorf("现有编码 %q 应通过：%v", code, err)
		}
	}
	for _, code := range []string{"AspectRatio", "aspect-ratio", "1seed", "a b", ""} {
		if err := ValidateParamDefFields(code, "名称"); err == nil {
			t.Errorf("编码 %q 应被拒", code)
		}
	}
	if err := ValidateParamDefFields("seed", strings.Repeat("字", 65)); err == nil {
		t.Error("名称超长应被拒")
	}
}
