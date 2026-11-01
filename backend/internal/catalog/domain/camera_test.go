package domain

import (
	"encoding/json"
	"testing"
)

func TestCameraSelection(t *testing.T) {
	cases := []struct {
		name, params, code, label string
	}{
		{"运镜广场写的对象", `{"prompt":"x","camera":{"code":"push_in","name":"镜头前推"}}`, "push_in", "镜头前推"},
		{"只有 code 的字符串", `{"camera":" orbit "}`, "orbit", ""},
		{"没选", `{"prompt":"x"}`, "", ""},
		{"此前三组选项的老形状没有 code，按没选处理", `{"camera":{"movement":"推近","shot":"特写"}}`, "", ""},
		{"参数不是对象", `[1,2]`, "", ""},
		{"空参数", ``, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, name := CameraSelection(json.RawMessage(tc.params))
			if code != tc.code || name != tc.label {
				t.Fatalf("得到 (%q,%q)，期望 (%q,%q)", code, name, tc.code, tc.label)
			}
		})
	}
}
