package domain

import (
	"strings"
	"testing"
)

func TestCharacterGenOperation(t *testing.T) {
	for _, c := range []struct {
		refs int
		want string
	}{{0, "text2img"}, {-1, "text2img"}, {1, "img2img"}, {2, "img2img"}, {9, "img2img"}} {
		if got := CharacterGenOperation(c.refs); got != c.want {
			t.Errorf("%d 张参考图应选 %s，得到 %s", c.refs, c.want, got)
		}
	}
}

func TestValidateGenRequest(t *testing.T) {
	ok := CharacterGenRequest{ModelCode: "m-img", Description: "圆脸柚子拟人", Count: 1}
	if is := ValidateGenRequest(ok); len(is) != 0 {
		t.Fatalf("合法请求不应有问题：%+v", is)
	}
	// 边界：500 码点（emoji 计 1）、4 张、9 张参考图。
	edge := CharacterGenRequest{ModelCode: "m-img", Description: strings.Repeat("😀", 500),
		Count: MaxCharacterGenCount, ReferenceMediaIDs: ids(MaxCharacterGenRefs)}
	if is := ValidateGenRequest(edge); len(is) != 0 {
		t.Fatalf("边界值应通过：%+v", is)
	}

	cases := []struct {
		name  string
		in    CharacterGenRequest
		field string
		code  IssueCode
	}{
		{"无模型", CharacterGenRequest{Description: "x", Count: 1}, "modelCode", IssueRequired},
		{"空描述", CharacterGenRequest{ModelCode: "m", Description: "  ", Count: 1}, "description", IssueRequired},
		{"描述 501", CharacterGenRequest{ModelCode: "m", Description: strings.Repeat("柚", 501), Count: 1},
			"description", IssueTooLong},
		{"张数 0", CharacterGenRequest{ModelCode: "m", Description: "x"}, "count", IssueBadValue},
		{"张数 5", CharacterGenRequest{ModelCode: "m", Description: "x", Count: 5}, "count", IssueBadValue},
		{"参考图 10 张", CharacterGenRequest{ModelCode: "m", Description: "x", Count: 1,
			ReferenceMediaIDs: ids(10)}, "referenceMediaIds", IssueTooMany},
		{"参考图重复", CharacterGenRequest{ModelCode: "m", Description: "x", Count: 1,
			ReferenceMediaIDs: []int64{7, 7}}, "referenceMediaIds[1]", IssueDuplicate},
		{"参考图 ID 非法", CharacterGenRequest{ModelCode: "m", Description: "x", Count: 1,
			ReferenceMediaIDs: []int64{0}}, "referenceMediaIds[0]", IssueBadValue},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := issueFields(ValidateGenRequest(c.in))
			if got[c.field] != c.code {
				t.Errorf("应在 %s 报 %s：%v", c.field, c.code, got)
			}
		})
	}

	// 一次报完：三处问题都要出现。
	all := ValidateGenRequest(CharacterGenRequest{Count: 9, ReferenceMediaIDs: []int64{1, 1}})
	f := issueFields(all)
	for _, k := range []string{"modelCode", "description", "count", "referenceMediaIds[1]"} {
		if _, ok := f[k]; !ok {
			t.Errorf("应一次报完，缺 %s：%v", k, f)
		}
	}
}
