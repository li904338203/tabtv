package app

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

// 营销片段在受理链路里的拼接（MKT-T-21）。受理主体要真库，拼接这一步单独测。

type stubPresets struct{ part string }

func (p stubPresets) PresetPrompt(context.Context, PresetAsk) (string, error) { return p.part, nil }

// stubMarketing 模拟营销域：params 里没有 marketing 段时回空（与真实实现同一语义）。
type stubMarketing struct {
	text  string
	err   error
	calls int
}

func (m *stubMarketing) Appendix(_ context.Context, _ MarketingScope, params json.RawMessage) (MarketingAppendix, error) {
	m.calls++
	if m.err != nil {
		return MarketingAppendix{}, m.err
	}
	var env map[string]json.RawMessage
	_ = json.Unmarshal(params, &env)
	if _, ok := env["marketing"]; !ok {
		return MarketingAppendix{}, nil
	}
	return MarketingAppendix{Text: m.text, Picks: []string{"creative:c1@1"}}, nil
}

// MKR-10：没有 params.marketing 的生成，附加段与改动前**逐字相等** ——
// 包括「没有预设来源时素材段原样保留、不去两端空白」这一条旧行为。
func TestAccept_无营销参数后缀不变(t *testing.T) {
	ctx := context.Background()
	in := AcceptInput{Params: json.RawMessage(`{"prompt":"一只猫","preset":"p1"}`)}
	material := " 水墨风格 "
	cases := []struct {
		name    string
		presets PresetSource
		want    string
	}{
		// 改动前：presets==nil 时 promptSuffix 就是解析给的原文。
		{"无预设来源", nil, material},
		// 改动前：OrderAppendix(preset, material)。
		{"有预设", stubPresets{"预设版式"}, "预设版式\n水墨风格"},
		{"预设为空", stubPresets{""}, "水墨风格"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for _, mk := range []MarketingSource{nil, &stubMarketing{text: "不该出现"}} {
				s := &Service{presets: c.presets, marketing: mk}
				got, m, _, err := s.composeSuffix(ctx, in, nil, nil, material)
				if err != nil {
					t.Fatal(err)
				}
				if got != c.want || m.Text != "" {
					t.Fatalf("注入营销来源=%v：得到 %q（营销 %q），期望 %q", mk != nil, got, m.Text, c.want)
				}
			}
		})
	}
}

func TestAccept_营销段夹在预设与素材词之间(t *testing.T) {
	mk := &stubMarketing{text: "【创意】A\n【约束】B"}
	s := &Service{presets: stubPresets{"预设版式"}, marketing: mk}
	in := AcceptInput{Params: json.RawMessage(`{"prompt":"一只猫","marketing":{"picks":[]}}`)}
	got, m, _, err := s.composeSuffix(context.Background(), in, nil, nil, "水墨风格")
	if err != nil {
		t.Fatal(err)
	}
	if want := "预设版式\n【创意】A\n【约束】B\n水墨风格"; got != want {
		t.Fatalf("得到 %q，期望 %q", got, want)
	}
	if m.Text != mk.text || len(m.Picks) != 1 {
		t.Fatalf("营销段应原样返回给报价与日志：%+v", m)
	}
	// 没有预设来源时营销段照样拼（不依赖预设是否接上）。
	s = &Service{marketing: mk}
	if got, _, _, _ := s.composeSuffix(context.Background(), in, nil, nil, ""); got != "【创意】A\n【约束】B" {
		t.Fatalf("无预设来源时得到 %q", got)
	}
}

// 营销段取不出来（钉住的版本不存在等）就在扣费之前失败，不悄悄少一段配方。
func TestAccept_营销段失败即受理失败(t *testing.T) {
	boom := errors.New("钉住的版本不存在")
	s := &Service{marketing: &stubMarketing{err: boom}}
	in := AcceptInput{Params: json.RawMessage(`{"marketing":{"picks":[]}}`)}
	if _, _, _, err := s.composeSuffix(context.Background(), in, nil, nil, ""); !errors.Is(err, boom) {
		t.Fatalf("应原样返回营销域的错误，得到 %v", err)
	}
}
