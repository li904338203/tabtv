package domain

import (
	"reflect"
	"strings"
	"testing"
)

var (
	ruleNodes = []RuleNode{{"text", "generator"}, {"image", "generator"}, {"video", "generator"},
		{"audio", "generator"}, {"style", "material"}, {"effect", "material"}, {"script", "composite"}}
	ruleOps = []RuleOperation{
		{"text2img", "text,style", "image"}, {"img2img", "image,style", "image"},
		{"img2video", "image,effect", "video"}, {"ref2video", "text,image,audio,effect", "video"},
		{"img2prompt", "image", "text"},
	}
)

// 现有 seed 的规则形态都要放行
func TestValidateConnectionRuleConfig_现有规则都放行(t *testing.T) {
	for _, c := range []struct {
		s, d string
		op   string
	}{{"text", "image", "text2img"}, {"image", "image", "img2img"}, {"image", "video", "img2video"},
		{"audio", "video", "ref2video"}, {"image", "text", "img2prompt"}, {"style", "image", ""}, {"effect", "video", ""}} {
		if err := ValidateConnectionRuleConfig(c.s, c.d, true, c.op, ruleNodes, ruleOps); err != nil {
			t.Errorf("%s→%s(%s) 应通过：%v", c.s, c.d, c.op, err)
		}
	}
}

func TestValidateConnectionRuleConfig_讲不通的拒掉(t *testing.T) {
	cases := map[string]struct {
		s, d    string
		allowed bool
		op, msg string
	}{
		"源不存在":      {"3d", "image", true, "text2img", "不存在"},
		"连向素材节点":    {"text", "style", true, "", "素材节点不接收输入"},
		"素材源带操作":    {"style", "image", true, "text2img", "不填默认操作"},
		"生成节点缺默认操作": {"text", "image", true, "", "必须有默认操作"},
		"操作产出对不上":   {"text", "video", true, "text2img", "对不上"},
		"操作不收源类型":   {"audio", "image", true, "text2img", "对不上"},
		"禁止却带操作":    {"text", "image", false, "text2img", "不需要默认操作"},
	}
	for name, c := range cases {
		err := ValidateConnectionRuleConfig(c.s, c.d, c.allowed, c.op, ruleNodes, ruleOps)
		if err == nil || !strings.Contains(err.Error(), c.msg) {
			t.Errorf("%s：应被拒（含「%s」），得到 %v", name, c.msg, err)
		}
	}
	// 禁止连接的规则、连向组合节点的规则可以没有默认操作
	if err := ValidateConnectionRuleConfig("text", "image", false, "", ruleNodes, ruleOps); err != nil {
		t.Errorf("禁止连接不需要操作：%v", err)
	}
	if err := ValidateConnectionRuleConfig("text", "script", true, "", ruleNodes, ruleOps); err != nil {
		t.Errorf("连向组合节点不强制默认操作：%v", err)
	}
}

func TestCompatibleOperations(t *testing.T) {
	if got := CompatibleOperations("image", "video", ruleOps); !reflect.DeepEqual(got, []string{"img2video", "ref2video"}) {
		t.Fatalf("图→视频兼容的操作：%v", got)
	}
}
