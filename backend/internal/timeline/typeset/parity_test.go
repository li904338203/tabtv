package typeset_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/li904338203/tabtv/backend/internal/timeline/domain"
	"github.com/li904338203/tabtv/backend/internal/timeline/typeset"
)

// 排版接口与时间线内容的保存校验（SCT-03）各有一张取值范围表。两张表不一致的后果是：
// 存得进去的样式在预览里报 40001，或者预览能排的样式保存时被拒 —— 两边各自看都「没错」。
// 这里用同一组样式分别过两边，结论（合法与否、指出的字段）必须一致。

func TestMaxTextRunesMatchesContentLimit(t *testing.T) {
	if typeset.MaxTextRunes != domain.MaxTextChars {
		t.Fatalf("排版上限 %d 与内容上限 %d 不一致", typeset.MaxTextRunes, domain.MaxTextChars)
	}
}

func TestStyleRangesMatchContentValidation(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "api", "fixtures", "smartcut", "content", "valid-minimal.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := domain.ValidateContent(raw); err != nil {
		t.Fatalf("基准 fixture 本身不合法：%v", err)
	}
	type variant struct {
		name  string
		style map[string]any
		box   map[string]any
	}
	common := []variant{
		{"字号下限", map[string]any{"fontSizePct": 2}, nil},
		{"字号上限", map[string]any{"fontSizePct": 20}, nil},
		{"字号过小", map[string]any{"fontSizePct": 1.99}, nil},
		{"字号过大", map[string]any{"fontSizePct": 20.01}, nil},
		{"#RGB", map[string]any{"color": "#FFF"}, nil},
		{"#RRGGBB", map[string]any{"color": "#FFD400"}, nil},
		{"#RRGGBBAA", map[string]any{"color": "#00000080"}, nil},
		{"#RGBA", map[string]any{"color": "#FFFF"}, nil},
		{"颜色名", map[string]any{"color": "red"}, nil},
		{"颜色是数字", map[string]any{"color": 123}, nil},
		{"描边上限", map[string]any{"stroke": map[string]any{"color": "#000", "widthPct": 5}}, nil},
		{"描边过粗", map[string]any{"stroke": map[string]any{"widthPct": 5.1}}, nil},
		{"描边颜色", map[string]any{"stroke": map[string]any{"color": "black"}}, nil},
		{"描边不是对象", map[string]any{"stroke": "x"}, nil},
		{"描边为 null", map[string]any{"stroke": nil}, nil},
		{"底框上限", map[string]any{"background": map[string]any{"opacity": 1, "paddingPct": 10}}, nil},
		{"底框透明度", map[string]any{"background": map[string]any{"opacity": 1.1}}, nil},
		{"底框内边距", map[string]any{"background": map[string]any{"paddingPct": 10.5}}, nil},
		{"底框颜色", map[string]any{"background": map[string]any{"color": "#12"}}, nil},
		{"对齐", map[string]any{"align": "right"}, nil},
		{"对齐不认识", map[string]any{"align": "justify"}, nil},
		{"最大宽度下限", map[string]any{"maxWidthPct": 10}, nil},
		{"最大宽度过小", map[string]any{"maxWidthPct": 9}, nil},
		{"最大宽度过大", map[string]any{"maxWidthPct": 101}, nil},
		{"未知字段", map[string]any{"glow": map[string]any{"radius": 3}}, nil},
	}
	texts := append(common,
		variant{"位置框边界", nil, map[string]any{"xPct": 0, "yPct": 100, "anchor": "bottom"}},
		variant{"位置框 x 越界", nil, map[string]any{"xPct": -1}},
		variant{"位置框 y 越界", nil, map[string]any{"yPct": 101}},
		variant{"锚点不认识", nil, map[string]any{"anchor": "middle"}},
		variant{"文字层上的 bottomPct 是未知字段", map[string]any{"bottomPct": 99}, nil},
	)
	subtitles := append(common,
		variant{"距底上限", map[string]any{"bottomPct": 50}, nil},
		variant{"距底越界", map[string]any{"bottomPct": 51}, nil},
	)

	check := func(t *testing.T, kind, prefix string, v variant, patch func(doc map[string]any)) {
		var doc map[string]any
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatal(err)
		}
		patch(doc)
		patched, _ := json.Marshal(doc)
		contentErr := domain.ValidateContent(patched)
		typesetErr := typeset.ValidateStyle(kind, v.style, v.box)

		var ce *domain.ValidationError
		var fe *typeset.FieldError
		switch {
		case contentErr == nil && typesetErr == nil:
		case errors.As(contentErr, &ce) && errors.As(typesetErr, &fe):
			if ce.Path != prefix+fe.Path {
				t.Errorf("%s：保存校验指出 %s，排版接口指出 %s", v.name, ce.Path, fe.Path)
			}
		default:
			t.Errorf("%s：保存校验 %v，排版接口 %v —— 两边结论不一致", v.name, contentErr, typesetErr)
		}
	}
	for _, v := range texts {
		check(t, typeset.KindText, "texts[0].", v, func(doc map[string]any) {
			text := doc["texts"].([]any)[0].(map[string]any)
			if v.style != nil {
				text["style"] = v.style
			}
			if v.box != nil {
				text["box"] = v.box
			}
		})
	}
	for _, v := range subtitles {
		check(t, typeset.KindSubtitle, "tracks[2].", v, func(doc map[string]any) {
			track := doc["tracks"].([]any)[2].(map[string]any)
			if track["kind"] != "subtitle" {
				t.Fatalf("fixture 的第 3 条轨道不是字幕轨：%v", track["kind"])
			}
			track["style"] = v.style
		})
	}
}
