package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/li904338203/tabtv/backend/internal/marketing/domain"
	"github.com/li904338203/tabtv/backend/internal/marketing/repo"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
)

// 生成侧营销段（MKT-T-21）：只按钉住的 (code, version) 取历史原文 + 场景约束段。

type fakeRenderer struct {
	text  string
	err   error
	codes []string
}

func (f *fakeRenderer) RenderPrompt(_ context.Context, code, _ string, _ map[string]string) (string, error) {
	f.codes = append(f.codes, code)
	return f.text, f.err
}

const constraintText = "【约束】商品与参考图一致；画面不出现文字与水印。"

// appendixSvc 造一个三类选项各有两个版本的营销服务；v2 是当前版。
// creative 已下线、hook 已过期 —— 钉住的旧版本仍要照常拼。
func appendixSvc(t *testing.T) (*Service, *fakeRenderer) {
	t.Helper()
	versions := map[string][]repo.OptionVersion{}
	var opts []repo.Option
	for i, k := range []domain.Kind{domain.KindCreative, domain.KindHook, domain.KindStyle} {
		code := "c_" + string(k)
		cur := opt(int64(i+1), k, code, func(o *domain.Option) {
			o.Version, o.Prompt = 2, string(k)+"-v2"
			switch k {
			case domain.KindCreative:
				o.IsEnabled = false
			case domain.KindHook:
				o.EndAt = tp(t0.Add(-1))
			}
		})
		v1 := cur.Option
		v1.Version, v1.Prompt = 1, string(k)+"-v1"
		b1, _ := snapshotJSON(v1)
		b2, _ := snapshotJSON(cur.Option)
		versions[code] = []repo.OptionVersion{{Version: 1, Snapshot: b1}, {Version: 2, Snapshot: b2}}
		opts = append(opts, cur)
	}
	s := newUserSvc(&userStore{opts: opts, versions: versions}, nil, true)
	r := &fakeRenderer{text: constraintText}
	s.SetPrompts(r)
	return s, r
}

func params(picks string) json.RawMessage {
	return json.RawMessage(`{"prompt":"画面描述","marketing":{"picks":` + picks + `}}`)
}

func TestAppendix_按版本取历史(t *testing.T) {
	s, _ := appendixSvc(t)
	ctx := context.Background()
	// 故意乱序：顺序由类型决定（创意 → Hook → 风格 → 约束）。
	got, err := s.Appendix(ctx, params(`[{"kind":"style","code":"c_style","version":1},
		{"kind":"hook","code":"c_hook","version":1},{"kind":"creative","code":"c_creative","version":1}]`))
	if err != nil {
		t.Fatal(err)
	}
	if want := "creative-v1\nhook-v1\nstyle-v1\n" + constraintText; got.Text != want {
		t.Fatalf("钉住 v1 应拼 v1 原文（已下线 / 已过期照常拼）：\n%q\n期望\n%q", got.Text, want)
	}
	if strings.Join(got.Picks, ",") != "style:c_style@1,hook:c_hook@1,creative:c_creative@1" {
		t.Fatalf("日志用的 picks 应为 kind:code@version（按入参顺序）：%v", got.Picks)
	}
	// 同一个 code 钉 v2 → 拼 v2。
	got, err = s.Appendix(ctx, params(`[{"kind":"creative","code":"c_creative","version":2}]`))
	if err != nil || got.Text != "creative-v2\n"+constraintText {
		t.Fatalf("钉住 v2 应拼 v2：%v %q", err, got.Text)
	}
}

// 不存在的版本 / kind 对不上 / 形状坏 → 40001（只可能是手改 params），details 点名。
func TestAppendix_钉住的版本不存在回40001(t *testing.T) {
	s, _ := appendixSvc(t)
	ctx := context.Background()
	for _, p := range []string{
		`[{"kind":"creative","code":"c_creative","version":9}]`,
		`[{"kind":"hook","code":"c_creative","version":1}]`,
		`[{"kind":"creative","code":"nope_code","version":1}]`,
	} {
		_, err := s.Appendix(ctx, params(p))
		be := bizErr(t, err)
		if be.Code != errcode.InvalidArgument || be.Details["code"] == nil || be.Details["version"] == nil {
			t.Fatalf("%s 应 40001 并点名 code / version：%+v", p, be)
		}
	}
	for _, raw := range []string{`{"marketing":{"picks":[{"kind":"x","code":"c_style","version":1}]}}`,
		`{"marketing":{"picks":[{"kind":"style","code":"c_style","version":0}]}}`, `{"marketing":"坏"}`} {
		if _, err := s.Appendix(ctx, json.RawMessage(raw)); bizErr(t, err).Code != errcode.InvalidArgument {
			t.Fatalf("%s 应 40001：%v", raw, err)
		}
	}
}

func TestAppendix_约束段总是追加(t *testing.T) {
	s, r := appendixSvc(t)
	ctx := context.Background()
	// marketing 段存在但一样没选：只有约束段。
	for _, raw := range []string{`{"marketing":{"picks":[]}}`, `{"marketing":{}}`} {
		got, err := s.Appendix(ctx, json.RawMessage(raw))
		if err != nil || got.Text != constraintText || len(got.Picks) != 0 {
			t.Fatalf("%s：应只有约束段：%v %+v", raw, err, got)
		}
	}
	if len(r.codes) == 0 || r.codes[0] != ConstraintTemplate {
		t.Fatalf("约束段应渲染 %s，实际 %v", ConstraintTemplate, r.codes)
	}
	// 没有 marketing 段（或为 null）：通用生成，一个字都不拼，也不去取约束段。
	r.codes = nil
	for _, raw := range []string{`{"prompt":"一只猫"}`, `{"marketing":null}`, ``, `{}`} {
		got, err := s.Appendix(ctx, json.RawMessage(raw))
		if err != nil || got.Text != "" || got.Picks != nil {
			t.Fatalf("%q：通用生成应为空：%v %+v", raw, err, got)
		}
	}
	if len(r.codes) != 0 {
		t.Fatalf("通用生成不该去渲染约束段：%v", r.codes)
	}
	// 运营把约束段清空：选了的照拼，不留空行。
	r.text = "  "
	got, _ := s.Appendix(ctx, params(`[{"kind":"style","code":"c_style","version":2}]`))
	if got.Text != "style-v2" {
		t.Fatalf("约束段为空时不留空行：%q", got.Text)
	}
	// 渲染失败不悄悄去掉约束段。
	r.err = errors.New("模板坏了")
	if _, err := s.Appendix(ctx, params(`[]`)); err == nil {
		t.Fatal("约束段渲染失败应让受理失败")
	}
}

// 与 E2E 共用同一份 fixture（api/fixtures/marketing/compose-order.json）：Appendix 的结果
// 与 domain.Compose 的期望逐字相等，证明生成侧没有另起一套顺序。
func TestAppendix_与共享fixture一致(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "api", "fixtures", "marketing", "compose-order.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fx struct {
		Snapshots []struct {
			Kind    domain.Kind `json:"kind"`
			Code    string      `json:"code"`
			Version int         `json:"version"`
			Prompt  string      `json:"prompt"`
		} `json:"snapshots"`
		Constraint string `json:"constraint"`
		Expected   string `json:"expected"`
	}
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatal(err)
	}
	versions := map[string][]repo.OptionVersion{}
	picks := make([]map[string]any, 0, len(fx.Snapshots))
	for _, sn := range fx.Snapshots {
		b, _ := json.Marshal(domain.Snapshot{Pick: domain.Pick{Kind: sn.Kind, Code: sn.Code, Version: sn.Version},
			Prompt: sn.Prompt})
		versions[sn.Code] = []repo.OptionVersion{{Version: sn.Version, Snapshot: b}}
		picks = append(picks, map[string]any{"kind": sn.Kind, "code": sn.Code, "version": sn.Version})
	}
	s := newUserSvc(&userStore{versions: versions}, nil, true)
	s.SetPrompts(&fakeRenderer{text: fx.Constraint})
	pj, _ := json.Marshal(map[string]any{"marketing": map[string]any{"picks": picks}})
	got, err := s.Appendix(context.Background(), pj)
	if err != nil || got.Text != fx.Expected {
		t.Fatalf("应与 fixture 逐字一致：%v\n%q", err, got.Text)
	}
}
