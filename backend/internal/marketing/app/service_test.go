package app

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/li904338203/tabtv/backend/internal/marketing/domain"
	"github.com/li904338203/tabtv/backend/internal/marketing/repo"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
)

// ---------------------------------------------------------------- 假端口（用户端）

// userStore 用 domain.Visible 在内存里模拟 repo 的同义谓词。
type userStore struct {
	opts     []repo.Option
	versions map[string][]repo.OptionVersion // code → 版本
	cats     []repo.Category
}

func (u *userStore) ListVisibleOptions(_ context.Context, f repo.VisibleFilter) ([]repo.Option, bool, error) {
	var out []repo.Option
	for _, o := range u.opts {
		if o.Kind != f.Kind || !domain.Visible(o.Option, f.Now) {
			continue
		}
		if f.Category != "" && o.CategoryCode != f.Category {
			continue
		}
		if f.Model != "" && !containsStr(o.RecommendedModels, f.Model) {
			continue
		}
		out = append(out, o)
	}
	return out, false, nil
}

func (u *userStore) VisibleRecommendedModels(_ context.Context, kind domain.Kind, now time.Time) ([]string, error) {
	var out []string
	for _, o := range u.opts {
		if o.Kind == kind && domain.Visible(o.Option, now) {
			for _, m := range o.RecommendedModels {
				if !containsStr(out, m) {
					out = append(out, m)
				}
			}
		}
	}
	return out, nil
}

func (u *userStore) GetOptionByCode(_ context.Context, code string) (repo.Option, error) {
	for _, o := range u.opts {
		if o.Code == code {
			return o, nil
		}
	}
	return repo.Option{}, repo.ErrNotFound
}

func (u *userStore) GetVersionByCode(_ context.Context, code string, version int) (repo.OptionVersion, error) {
	for _, v := range u.versions[code] {
		if v.Version == version {
			return v, nil
		}
	}
	return repo.OptionVersion{}, repo.ErrNotFound
}

func (u *userStore) ListCategories(_ context.Context, _ domain.Kind) ([]repo.Category, error) {
	return u.cats, nil
}

type fakeCatalog struct{ usable []ModelRef }

func (f fakeCatalog) UsableModels(_ context.Context, out string) ([]ModelRef, error) {
	if out != domain.OutputVideo {
		return nil, nil
	}
	return f.usable, nil
}

type namesLookup map[string]ModelFacts

func (n namesLookup) LookupModels(_ context.Context, codes []string) (map[string]ModelFacts, error) {
	out := map[string]ModelFacts{}
	for _, c := range codes {
		if f, ok := n[c]; ok {
			out[c] = f
		}
	}
	return out, nil
}

var t0 = time.Date(2026, 10, 21, 4, 0, 0, 0, time.UTC)

func tp(t time.Time) *time.Time { return &t }

func opt(id int64, kind domain.Kind, code string, mut func(*domain.Option)) repo.Option {
	o := domain.Option{Kind: kind, Code: code, Name: code + "名", CategoryCode: "drop", Prompt: "片段-" + code,
		CoverMediaIDs: []int64{900 + id}, OutputTypes: []string{"video"}, IsEnabled: true, Version: 1}
	if mut != nil {
		mut(&o)
	}
	return repo.Option{ID: id, Option: o}
}

func newUserSvc(u *userStore, usable []ModelRef, agentOn bool) *Service {
	s := NewService(u, fakeCatalog{usable: usable}, namesLookup{"m_off": {Code: "m_off", Name: "停用模型"}},
		Settings{MaxProductImages: 20, MaxVariants: 4, AgentEnabled: agentOn})
	s.SetClock(func() time.Time { return t0 })
	return s
}

// ---------------------------------------------------------------- 用例

func TestResolve_下线未开始已结束产出不符各给reason(t *testing.T) {
	u := &userStore{opts: []repo.Option{
		opt(1, domain.KindCreative, "live_one", nil),
		opt(2, domain.KindCreative, "off_one", func(o *domain.Option) { o.IsEnabled = false }),
		opt(3, domain.KindCreative, "future_one", func(o *domain.Option) { o.StartAt = tp(t0.Add(time.Hour)) }),
		opt(4, domain.KindCreative, "past_one", func(o *domain.Option) { o.EndAt = tp(t0) }), // [start,end)：此刻已结束
		opt(5, domain.KindStyle, "img_style", func(o *domain.Option) { o.OutputTypes = []string{"image"} }),
		opt(6, domain.KindHook, "hook_one", nil),
	}}
	s := newUserSvc(u, nil, true)
	ctx := context.Background()

	cases := []struct {
		pick        PickRef
		reason      string
		userFixable bool
	}{
		{PickRef{domain.KindCreative, "off_one"}, ReasonDisabled, false},
		{PickRef{domain.KindCreative, "future_one"}, ReasonNotStarted, false},
		{PickRef{domain.KindCreative, "past_one"}, ReasonEnded, false},
		{PickRef{domain.KindStyle, "img_style"}, ReasonOutputMismatch, true},
		{PickRef{domain.KindCreative, "no_such"}, ReasonNotFound, false},
		{PickRef{domain.KindCreative, "hook_one"}, ReasonNotFound, false}, // kind 对不上
		{PickRef{domain.KindCreative, "BAD CODE"}, ReasonNotFound, false},
	}
	for _, c := range cases {
		_, err := s.Resolve(ctx, []PickRef{c.pick}, "video", t0)
		be := bizErr(t, err)
		if be.Code != errcode.MarketingOptionUnavailable {
			t.Fatalf("%s：应 42215，得 %v", c.pick.Code, be.Code)
		}
		if be.Details["reason"] != c.reason || be.Details["userFixable"] != c.userFixable ||
			be.Details["kind"] != string(c.pick.Kind) || be.Details["code"] != c.pick.Code {
			t.Fatalf("%s：details 不对 %v", c.pick.Code, be.Details)
		}
	}

	// 在投的照常钉住当前版本，片段原文给服务端。
	snaps, err := s.Resolve(ctx, []PickRef{{domain.KindCreative, "live_one"}, {domain.KindHook, "hook_one"}}, "video", t0)
	if err != nil || len(snaps) != 2 || snaps[0].Version != 1 || snaps[0].Prompt != "片段-live_one" {
		t.Fatalf("在投选项应解析成功：%v %+v", err, snaps)
	}
	// 产出为空不判适用产出。
	if _, err := s.Resolve(ctx, []PickRef{{domain.KindStyle, "img_style"}}, "", t0); err != nil {
		t.Fatalf("产出未定时不判：%v", err)
	}
	// 同一维度两项是客户端的错。
	_, err = s.Resolve(ctx, []PickRef{{domain.KindCreative, "live_one"}, {domain.KindCreative, "past_one"}}, "video", t0)
	if bizErr(t, err).Code != errcode.InvalidArgument {
		t.Fatal("同一维度两项应 40001")
	}
}

func TestSnapshot_不看是否在投(t *testing.T) {
	snap := func(o domain.Option) json.RawMessage {
		b, _ := snapshotJSON(o)
		return b
	}
	off := opt(2, domain.KindCreative, "off_one", func(o *domain.Option) { o.IsEnabled = false; o.Version = 2 })
	v1 := off.Option
	v1.Version, v1.Prompt = 1, "第一版片段"
	u := &userStore{opts: []repo.Option{off}, versions: map[string][]repo.OptionVersion{
		"off_one": {{OptionID: 2, Version: 1, Snapshot: snap(v1)}, {OptionID: 2, Version: 2, Snapshot: snap(off.Option)}},
	}}
	s := newUserSvc(u, nil, true)
	got, err := s.Snapshot(context.Background(), []domain.Pick{{Kind: domain.KindCreative, Code: "off_one", Version: 1}})
	if err != nil || len(got) != 1 || got[0].Prompt != "第一版片段" || got[0].Version != 1 {
		t.Fatalf("已下线但钉住的版本应照常返回原文：%v %+v", err, got)
	}
	// 新会话再选它 → 42215 disabled（同一个选项，两个入口语义相反）。
	_, err = s.Resolve(context.Background(), []PickRef{{domain.KindCreative, "off_one"}}, "video", t0)
	if be := bizErr(t, err); be.Details["reason"] != ReasonDisabled {
		t.Fatalf("新会话提交已下线 code 应 disabled：%v", be.Details)
	}
	// 版本不存在 / kind 对不上 → not_found，不悄悄跳过。
	for _, p := range []domain.Pick{{Kind: domain.KindCreative, Code: "off_one", Version: 9},
		{Kind: domain.KindHook, Code: "off_one", Version: 1}} {
		_, err := s.Snapshot(context.Background(), []domain.Pick{p})
		if be := bizErr(t, err); be.Code != errcode.MarketingOptionUnavailable || be.Details["reason"] != ReasonNotFound {
			t.Fatalf("%+v 应 42215 not_found：%v", p, be)
		}
	}
}

func TestConfig_按模型筛选派生(t *testing.T) {
	u := &userStore{
		opts: []repo.Option{
			opt(1, domain.KindCreative, "live_a", func(o *domain.Option) { o.RecommendedModels = []string{"m_b", "m_a"} }),
			opt(2, domain.KindCreative, "live_b", func(o *domain.Option) { o.RecommendedModels = []string{"m_off"} }),
			// 已结束的创意推荐的模型不进筛选项。
			opt(3, domain.KindCreative, "ended_c", func(o *domain.Option) {
				o.RecommendedModels = []string{"m_c"}
				o.EndAt = tp(t0.Add(-time.Minute))
			}),
			// Hook 上的推荐模型不算（只看创意）。
			opt(4, domain.KindHook, "hook_d", func(o *domain.Option) { o.RecommendedModels = []string{"m_d"} }),
		},
		cats: []repo.Category{
			{ID: 1, Category: domain.Category{Kind: domain.KindCreative, Code: "drop", Name: "从天而降", Sort: 1, IsEnabled: true}},
			{ID: 2, Category: domain.Category{Kind: domain.KindCreative, Code: "gone", Name: "停用分类", Sort: 2}},
			{ID: 3, Category: domain.Category{Kind: domain.KindHook, Code: "sound", Name: "声音", Sort: 1, IsEnabled: true}},
		},
	}
	// 目录顺序 m_a, m_c, m_d, m_b；m_off 不可用（不在 usable 里）。
	usable := []ModelRef{{"m_a", "模型A"}, {"m_c", "模型C"}, {"m_d", "模型D"}, {"m_b", "模型B"}}
	s := newUserSvc(u, usable, true)
	v, err := s.Config(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(v.CreativeModelFilters, []ModelRef{{"m_a", "模型A"}, {"m_b", "模型B"}}) {
		t.Fatalf("按模型筛选项 = 在投创意推荐 ∩ 可用，按目录顺序：%+v", v.CreativeModelFilters)
	}
	// 停用该模型（从可用列表里消失）后筛选项消失（MKT-AC-13）。
	s.catalog = fakeCatalog{usable: []ModelRef{{"m_b", "模型B"}}}
	v, _ = s.Config(context.Background())
	if !reflect.DeepEqual(v.CreativeModelFilters, []ModelRef{{"m_b", "模型B"}}) {
		t.Fatalf("停用后应消失：%+v", v.CreativeModelFilters)
	}
	if len(v.Categories[domain.KindCreative]) != 1 || v.Categories[domain.KindCreative][0].Code != "drop" ||
		len(v.Categories[domain.KindHook]) != 1 || v.Categories[domain.KindStyle] == nil {
		t.Fatalf("分类字典只给启用的、三类都在：%+v", v.Categories)
	}
	if !v.Available || v.MaxProductImages != 20 || v.MaxVariants != 4 || len(v.Dimensions) != 4 ||
		v.Dimensions[0].Key != "skill" || v.Slots[0].MaxCount != 1 || v.Slots[0].Accept[0] != "marketing_product" {
		t.Fatalf("配置不对：%+v", v)
	}
	// 商品库 / 角色库（MKT-T-37）：上限、配额、禁用字符表都来自唯一来源，不在这里另抄。
	lc := v.Library
	if lc.MaxProductsPerSession != domain.MaxProductsPerSession || lc.MaxCharacterRefs != domain.MaxCharacterRefs ||
		lc.MaxCharacterImages != domain.MaxCharacterImages || lc.QuotaProducts != domain.DefaultMaxProducts ||
		lc.QuotaCharacters != domain.DefaultMaxCharacters || strings.Join(lc.CharacterNameForbidden, "") != domain.CharacterNameForbidden ||
		len(lc.AllowedImageMimes) != 3 || lc.MaxProductDescription != domain.MaxProductDescription {
		t.Fatalf("库配置不对：%+v", lc)
	}
	// B-5：Agent 关着 → available=false 带原因。
	off := newUserSvc(u, usable, false)
	if v, _ := off.Config(context.Background()); v.Available || v.UnavailableReason != UnavailableAgentDisabled {
		t.Fatalf("Agent 关闭时应不可用：%+v", v)
	}
}

func TestOptionDTO_无prompt字段(t *testing.T) {
	for _, typ := range []reflect.Type{reflect.TypeOf(PublicOption{}), reflect.TypeOf(RecommendedModel{})} {
		for i := 0; i < typ.NumField(); i++ {
			name := strings.ToLower(typ.Field(i).Name)
			if strings.Contains(name, "prompt") || strings.Contains(name, "mediaid") {
				t.Fatalf("%s 上不应有字段 %s（MKT-AC-06：片段不下发；媒体只按 code+下标取）", typ.Name(), typ.Field(i).Name)
			}
		}
	}
}

func TestGetOption_不可见与不存在同一个404(t *testing.T) {
	u := &userStore{opts: []repo.Option{
		opt(1, domain.KindCreative, "live_one", func(o *domain.Option) {
			o.RecommendedModels = []string{"m_a", "m_off", "m_gone"}
			o.RecommendedParams = json.RawMessage(`{"aspectRatio":"9:16"}`)
			o.DefaultSkillCode = "sk"
		}),
		opt(2, domain.KindCreative, "past_one", func(o *domain.Option) { o.EndAt = tp(t0) }),
	}}
	s := newUserSvc(u, []ModelRef{{"m_a", "模型A"}}, true)
	ctx := context.Background()
	p, err := s.GetOption(ctx, "live_one")
	if err != nil || p.Lifecycle != domain.LifecycleLive || p.CoverCount != 1 || p.HasPreview || p.DefaultSkillCode != "sk" {
		t.Fatalf("详情不对：%v %+v", err, p)
	}
	want := []RecommendedModel{{"m_a", "模型A", true}, {"m_off", "停用模型", false}}
	if !reflect.DeepEqual(p.RecommendedModels, want) {
		t.Fatalf("推荐模型带名称与可用标记、已删除的不列：%+v", p.RecommendedModels)
	}
	var msgs []string
	for _, code := range []string{"past_one", "no_such"} {
		_, err := s.GetOption(ctx, code)
		be := bizErr(t, err)
		if be.Code != errcode.NotFound {
			t.Fatalf("%s 应 404：%v", code, be.Code)
		}
		msgs = append(msgs, be.Message)
	}
	if msgs[0] != msgs[1] {
		t.Fatalf("不可见与不存在必须同一个错误：%q vs %q", msgs[0], msgs[1])
	}
	// 封面：下标越界、不可见、没有示例视频都是同一个 404。
	if id, err := s.CoverMedia(ctx, "live_one", 0); err != nil || id != 901 {
		t.Fatalf("封面 0 应取到：%v %d", err, id)
	}
	for _, f := range []func() error{
		func() error { _, err := s.CoverMedia(ctx, "live_one", 1); return err },
		func() error { _, err := s.CoverMedia(ctx, "past_one", 0); return err },
		func() error { _, err := s.PreviewMedia(ctx, "live_one"); return err },
	} {
		if be := bizErr(t, f()); be.Code != errcode.NotFound || be.Message != msgs[0] {
			t.Fatalf("应同一个 404：%v", be)
		}
	}
}

func TestListOptions_只列在投且kind必填(t *testing.T) {
	u := &userStore{opts: []repo.Option{
		opt(1, domain.KindCreative, "live_one", nil),
		opt(2, domain.KindCreative, "future_one", func(o *domain.Option) { o.StartAt = tp(t0.Add(time.Second)) }),
		opt(3, domain.KindHook, "hook_one", nil),
	}}
	s := newUserSvc(u, nil, true)
	page, err := s.ListOptions(context.Background(), OptionsQuery{Kind: domain.KindCreative})
	if err != nil || len(page.Items) != 1 || page.Items[0].Code != "live_one" {
		t.Fatalf("只列在投的创意：%v %+v", err, page.Items)
	}
	if page.Items[0].RecommendedModels == nil || page.Items[0].Tags == nil || page.Items[0].InputRequirements == nil {
		t.Fatal("数组 / 对象字段不能是 nil（前端读到 null）")
	}
	for _, q := range []OptionsQuery{{}, {Kind: "skill"}, {Kind: domain.KindCreative, Cursor: "x"}} {
		_, err := s.ListOptions(context.Background(), q)
		if bizErr(t, err).Code != errcode.InvalidArgument {
			t.Fatalf("%+v 应 40001", q)
		}
	}
}
