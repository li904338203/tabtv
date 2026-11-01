package domain

import (
	"strings"
	"testing"
)

// ⚠️ 顺序不可换：识别信息（画什么）必须排在审美词前 ——
// 反过来的话模型先照着风格词画，把用户真正要的内容挤掉。
func TestOrderAppendix_预设在素材词之前(t *testing.T) {
	got := OrderAppendix("预设版式", "", "", "水墨风格")
	if strings.Index(got, "预设版式") > strings.Index(got, "水墨风格") {
		t.Fatalf("预设应排在素材词之前:\n%s", got)
	}
}

func TestOrderAppendix_任一为空时不留空行(t *testing.T) {
	if got := OrderAppendix("", "", "", "水墨"); got != "水墨" {
		t.Fatalf("不该留空行: %q", got)
	}
	if got := OrderAppendix("预设", "", "", "  "); got != "预设" {
		t.Fatalf("空白也算空: %q", got)
	}
	if got := OrderAppendix("", "", "", ""); got != "" {
		t.Fatalf("都空时应为空: %q", got)
	}
}

// 营销段（MKT-T-21）夹在预设与素材词之间；任一段为空都不留空行。
func TestOrderAppendix_三段顺序与空段(t *testing.T) {
	cases := []struct {
		name                        string
		preset, marketing, material string
		want                        string
	}{
		{"三段齐", "预设", "创意\n约束", "水墨", "预设\n创意\n约束\n水墨"},
		{"只有营销", "", "创意\n约束", "", "创意\n约束"},
		{"营销为空等于两段", "预设", "", "水墨", "预设\n水墨"},
		{"营销空白也算空", "预设", " \n ", "水墨", "预设\n水墨"},
		{"无预设", "", "创意", "水墨", "创意\n水墨"},
		{"无素材", "预设", "创意", "", "预设\n创意"},
		{"两端空白被去掉", " 预设 ", "\n创意\n", " 水墨\n", "预设\n创意\n水墨"},
		{"全空", "", "", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := OrderAppendix(c.preset, "", c.marketing, c.material); got != c.want {
				t.Fatalf("得到 %q，期望 %q", got, c.want)
			}
		})
	}
}

// 3D 导演台约束段排在预设之后、营销之前（DRD-18）；空段跳过。
func TestOrderAppendix_四段顺序(t *testing.T) {
	got := OrderAppendix("预设版式", "白模约束", "营销配方", "水墨风格")
	if got != "预设版式\n白模约束\n营销配方\n水墨风格" {
		t.Fatalf("顺序应为 预设 → 导演台 → 营销 → 素材，得到 %q", got)
	}
}

func TestOrderAppendix_空段跳过(t *testing.T) {
	if got := OrderAppendix("", "白模约束", "  ", "水墨"); got != "白模约束\n水墨" {
		t.Fatalf("空段不留空行，得到 %q", got)
	}
}
