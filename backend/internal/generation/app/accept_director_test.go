package app

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

// 3D 导演台约束段在受理链路里的拼接（DRT-07，DRD-18）。受理主体要真库，拼接这一步单独测。

// stubGuard 模拟目录域：按种类回固定的约束段，记下被问过的种类。
type stubGuard struct{ asked [][]string }

func (g *stubGuard) DirectorGuardPrompt(_ context.Context, kinds []string) (string, error) {
	g.asked = append(g.asked, kinds)
	var parts []string
	for _, k := range kinds {
		switch k {
		case "capture":
			parts = append(parts, "【截图约束】")
		case "preview":
			parts = append(parts, "【预演约束】")
		}
	}
	return strings.Join(parts, "\n"), nil
}

func directorService(presets PresetSource, marketing MarketingSource) (*Service, *stubGuard) {
	s := &Service{}
	s.SetPresets(presets)
	if marketing != nil {
		s.SetMarketing(marketing)
	}
	g := &stubGuard{}
	s.SetDirectorGuard(g)
	return s, g
}

func TestComposeSuffix_导演台截图约束在预设之后营销之前(t *testing.T) {
	s, _ := directorService(stubPresets{part: "【预设】按白模构图出图"}, &stubMarketing{text: "【营销】配方"})
	in := AcceptInput{Params: json.RawMessage(`{"prompt":"两个人对峙","preset":"whitebox_to_real","marketing":{}}`)}
	got, _, part, err := s.composeSuffix(context.Background(), in, nil, []string{"capture"}, "水墨风格")
	if err != nil {
		t.Fatal(err)
	}
	want := "【预设】按白模构图出图\n【截图约束】\n【营销】配方\n水墨风格"
	if got != want {
		t.Fatalf("顺序应为 预设 → 导演台约束 → 营销 → 素材词，得到 %q", got)
	}
	if part != "【截图约束】" {
		t.Errorf("要把导演台段单独返回（算长度用），得到 %q", part)
	}
}

func TestComposeSuffix_截图与预演两段都加(t *testing.T) {
	s, g := directorService(stubPresets{}, nil)
	got, _, _, err := s.composeSuffix(context.Background(), AcceptInput{Params: json.RawMessage(`{}`)}, nil,
		[]string{"capture", "preview"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if got != "【截图约束】\n【预演约束】" {
		t.Fatalf("截图与预演都在时两段都加、先图后视频，得到 %q", got)
	}
	if len(g.asked) != 1 {
		t.Errorf("约束段只问一次，得到 %d 次", len(g.asked))
	}
}

// MKR-10 同理：没有导演台输入的生成，附加段与改动前逐字相等，也不去问约束段。
func TestComposeSuffix_没有导演台输入快照逐字不变(t *testing.T) {
	for _, presets := range []PresetSource{nil, stubPresets{part: "预设"}} {
		s, g := directorService(presets, nil)
		s.presets = presets // nil 的情形要保留「没有预设来源」
		before := &Service{presets: presets}
		in := AcceptInput{Params: json.RawMessage(`{"prompt":"一只猫"}`)}
		want, _, _, err := before.composeSuffix(context.Background(), in, nil, nil, " 水墨风格 ")
		if err != nil {
			t.Fatal(err)
		}
		got, _, part, err := s.composeSuffix(context.Background(), in, nil, nil, " 水墨风格 ")
		if err != nil || got != want || part != "" {
			t.Fatalf("没有导演台输入时附加段应逐字不变：%q vs %q（%v）", got, want, err)
		}
		if len(g.asked) != 0 {
			t.Fatal("没有导演台输入时不该去问约束段")
		}
	}
	// 有导演台输入却没接来源：明确报错，不静默少一段。
	s := &Service{}
	if _, _, _, err := s.composeSuffix(context.Background(), AcceptInput{}, nil, []string{"capture"}, ""); err == nil {
		t.Fatal("有导演台输入而约束段来源未接入时应报错")
	}
}

func TestComposeSuffix_约束段计入AppendixChars(t *testing.T) {
	s, _ := directorService(stubPresets{}, &stubMarketing{text: "营销五个字"})
	in := AcceptInput{Params: json.RawMessage(`{"marketing":{}}`)}
	_, m, part, err := s.composeSuffix(context.Background(), in, nil, []string{"capture"}, "")
	if err != nil {
		t.Fatal(err)
	}
	// prepare 里报价的 AppendixChars = 营销段 + 导演台段（码点数）。
	if n := utf8.RuneCountInString(m.Text) + utf8.RuneCountInString(part); n != 5+len([]rune("【截图约束】")) {
		t.Fatalf("附加段长度应计入导演台段，得到 %d", n)
	}
}
