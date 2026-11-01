package domain

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestModelParamOverride(t *testing.T) {
	cases := []struct {
		name     string
		override string
		allowed  string
		def      string
		want     string
	}{
		{
			name:     "override 原样下发，default 由列补上",
			override: `{"unit":"秒","options":[{"label":"4 秒","value":"4"},{"label":"15 秒","value":"15"}]}`,
			def:      "4",
			want:     `{"unit":"秒","options":[{"label":"4 秒","value":"4"},{"label":"15 秒","value":"15"}],"default":"4"}`,
		},
		{
			name:     "override 自带 default 时不被列覆盖",
			override: `{"options":["720p","1080p"],"default":"1080p"}`,
			def:      "720p",
			want:     `{"options":["720p","1080p"],"default":"1080p"}`,
		},
		{
			name:    "没有 options 时按 allowed_values 生成，数字也归一成字符串",
			allowed: `[4, 5, "6"]`,
			def:     "5",
			want:    `{"options":[{"label":"4","value":"4"},{"label":"5","value":"5"},{"label":"6","value":"6"}],"default":"5"}`,
		},
		{
			name:     "default 不在可选项里就丢掉，不替用户选一个上游不收的值",
			override: `{"options":[{"label":"720P","value":"720p"}]}`,
			def:      "1080p",
			want:     `{"options":[{"label":"720P","value":"720p"}]}`,
		},
		{
			name:     "options 不是数组时不检查 default（配置可以比代码新）",
			override: `{"restrictBy":{"resolution":{"1080p":["16:9"]}}}`,
			def:      "16:9",
			want:     `{"restrictBy":{"resolution":{"1080p":["16:9"]}},"default":"16:9"}`,
		},
		{
			name:     "列是 JSON null 当成空",
			override: `null`,
			def:      "",
			want:     `{}`,
		},
		{
			name: "三列都空就是空覆盖",
			want: `{}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ModelParamOverride(bytesOrNil(tc.override), bytesOrNil(tc.allowed), tc.def)
			if err != nil {
				t.Fatalf("不该报错: %v", err)
			}
			var want map[string]any
			if err := json.Unmarshal([]byte(tc.want), &want); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				g, _ := json.Marshal(got)
				t.Fatalf("得到 %s，期望 %s", g, tc.want)
			}
		})
	}
}

func TestModelParamOverride_配坏了要报错而不是静默当空(t *testing.T) {
	if _, err := ModelParamOverride([]byte(`["16:9"]`), nil, ""); err == nil {
		t.Fatal("override 是数组时应报错")
	}
	if _, err := ModelParamOverride(nil, []byte(`{"a":1}`), ""); err == nil {
		t.Fatal("allowed_values 不是数组时应报错")
	}
}

func bytesOrNil(s string) []byte {
	if s == "" {
		return nil
	}
	return []byte(s)
}
