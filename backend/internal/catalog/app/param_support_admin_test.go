package app

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestValidateParamSupport(t *testing.T) {
	cases := []struct {
		name string
		in   ParamSupportInput
		want string // 空 = 通过
	}{
		{"可选值+默认", ParamSupportInput{ParamCode: "d", AllowedValues: json.RawMessage(`["4","15"]`), DefaultValue: "4"}, ""},
		{"只给覆盖", ParamSupportInput{ParamCode: "d", Override: json.RawMessage(`{"options":[{"label":"5 秒","value":"5"}],"default":"5"}`)}, ""},
		{"数字可选值与字符串默认等价", ParamSupportInput{ParamCode: "d", AllowedValues: json.RawMessage(`[5,10]`), DefaultValue: "10"}, ""},
		{"缺参数", ParamSupportInput{DefaultValue: "x"}, "参数必选"},
		{"全空", ParamSupportInput{ParamCode: "d", AllowedValues: json.RawMessage(`null`)}, "至少填一项"},
		{"可选值不是数组", ParamSupportInput{ParamCode: "d", AllowedValues: json.RawMessage(`{"a":1}`)}, "JSON 数组"},
		{"可选值含对象", ParamSupportInput{ParamCode: "d", AllowedValues: json.RawMessage(`[{"a":1}]`)}, "字符串、数字或布尔"},
		{"覆盖不是对象", ParamSupportInput{ParamCode: "d", Override: json.RawMessage(`[1]`)}, "JSON 对象"},
		{"默认值不在可选项", ParamSupportInput{ParamCode: "d", AllowedValues: json.RawMessage(`["4"]`), DefaultValue: "8"}, "不在可选项"},
	}
	for _, c := range cases {
		err := ValidateParamSupport(c.in)
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s：不该报错，得到 %v", c.name, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%s：应报「%s」，得到 %v", c.name, c.want, err)
		}
	}
}
