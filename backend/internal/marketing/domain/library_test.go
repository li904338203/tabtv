package domain

import (
	"strings"
	"testing"
)

func ids(n int) []int64 {
	out := make([]int64, n)
	for i := range out {
		out[i] = int64(1000 + i)
	}
	return out
}

func issueFields(is []Issue) map[string]IssueCode {
	m := map[string]IssueCode{}
	for _, i := range is {
		m[i.Field] = i.Code
	}
	return m
}

// 一次报完：每个字段的问题都要点名（MKT-AC-32 接口部分）。前端按 Field 标红。
func TestValidateProduct_一次报完(t *testing.T) {
	lim := ProductLimits{MaxImages: 10, MaxProducts: 500}
	ok := ProductInput{Name: "柚子汽水", ImageMediaIDs: ids(1)}
	if is := ValidateProduct(ok, lim); len(is) != 0 {
		t.Fatalf("合法商品不应有问题：%+v", is)
	}
	// 边界：恰好 10 张、30 码点名称（含 emoji，按 1 计）、300 码点卖点与描述。
	edge := ProductInput{Name: strings.Repeat("柚", 29) + "🍊", SellingPoints: strings.Repeat("好", 300),
		Description: strings.Repeat("😀", 300), ImageMediaIDs: ids(10)}
	if is := ValidateProduct(edge, lim); len(is) != 0 {
		t.Fatalf("边界值应通过：%+v", is)
	}

	cases := []struct {
		name string
		in   ProductInput
		want map[string]IssueCode
	}{
		{"零张图且无名称", ProductInput{}, map[string]IssueCode{"imageMediaIds": IssueRequired, "name": IssueRequired}},
		{"11 张", ProductInput{Name: "x", ImageMediaIDs: ids(11)}, map[string]IssueCode{"imageMediaIds": IssueTooMany}},
		{"名称 31 码点", ProductInput{Name: strings.Repeat("🍊", 31), ImageMediaIDs: ids(1)},
			map[string]IssueCode{"name": IssueTooLong}},
		{"卖点与描述 301 码点", ProductInput{Name: "x", ImageMediaIDs: ids(1),
			SellingPoints: strings.Repeat("a", 301), Description: strings.Repeat("的", 301)},
			map[string]IssueCode{"sellingPoints": IssueTooLong, "description": IssueTooLong}},
		{"重复图片与非法 ID", ProductInput{Name: "x", ImageMediaIDs: []int64{5, 0, 5}},
			map[string]IssueCode{"imageMediaIds[1]": IssueBadValue, "imageMediaIds[2]": IssueDuplicate}},
		{"全部同时出错", ProductInput{Name: strings.Repeat("名", 31), ImageMediaIDs: ids(12),
			SellingPoints: strings.Repeat("卖", 301), Description: strings.Repeat("描", 301)},
			map[string]IssueCode{"imageMediaIds": IssueTooMany, "name": IssueTooLong,
				"sellingPoints": IssueTooLong, "description": IssueTooLong}},
	}
	for _, c := range cases {
		got := issueFields(ValidateProduct(c.in.Normalized(), lim))
		if len(got) != len(c.want) {
			t.Errorf("%s：问题数不对，得到 %v 期望 %v", c.name, got, c.want)
			continue
		}
		for f, code := range c.want {
			if got[f] != code {
				t.Errorf("%s：字段 %s 期望 %s，得到 %v", c.name, f, code, got)
			}
		}
	}
	// 全是空白的名称等于没填。
	if got := issueFields(ValidateProduct(ProductInput{Name: "   ", ImageMediaIDs: ids(1)}.Normalized(), lim)); got["name"] != IssueRequired {
		t.Errorf("空白名称应报 required：%v", got)
	}
}

func TestAllowedImageMime(t *testing.T) {
	for ct, want := range map[string]bool{
		"image/png": true, "image/jpeg": true, "image/webp": true, "IMAGE/PNG": true, "image/jpeg; q=1": true,
		"image/gif": false, "image/bmp": false, "video/mp4": false, "audio/mpeg": false, "": false,
	} {
		if got := AllowedImageMime(ct); got != want {
			t.Errorf("AllowedImageMime(%q)=%v，期望 %v", ct, got, want)
		}
	}
}

// 上限取配置：传多少用多少；没传（≤0）才落默认值 10 / 500。
func TestProductLimits_取配置(t *testing.T) {
	if l := (ProductLimits{}).Normalize(); l.MaxImages != 10 || l.MaxProducts != 500 {
		t.Fatalf("默认值应为 10 / 500：%+v", l)
	}
	if l := (ProductLimits{MaxImages: 3, MaxProducts: 7}).Normalize(); l.MaxImages != 3 || l.MaxProducts != 7 {
		t.Fatalf("配置值应原样生效：%+v", l)
	}
	in := ProductInput{Name: "x", ImageMediaIDs: ids(4)}
	if got := issueFields(ValidateProduct(in, ProductLimits{MaxImages: 3})); got["imageMediaIds"] != IssueTooMany {
		t.Fatalf("配置 3 张时 4 张应超限：%v", got)
	}
	if MainImage(nil) != 0 || MainImage([]int64{9, 8}) != 9 {
		t.Fatal("主图应为第一张")
	}
}
