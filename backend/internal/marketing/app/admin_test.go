package app

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/li904338203/tabtv/backend/internal/marketing/domain"
	"github.com/li904338203/tabtv/backend/internal/marketing/repo"
	"github.com/li904338203/tabtv/backend/internal/platform/audit"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// ---------------------------------------------------------------- 假端口

type fakeStore struct {
	opts     map[int64]repo.Option
	versions map[int64][]repo.OptionVersion
	cats     []repo.Category
}

func newFakeStore() *fakeStore {
	return &fakeStore{opts: map[int64]repo.Option{}, versions: map[int64][]repo.OptionVersion{},
		cats: []repo.Category{{ID: 1, Category: domain.Category{Kind: domain.KindCreative, Code: "drop",
			Name: "从天而降", IsEnabled: true}}, {ID: 2, Category: domain.Category{Kind: domain.KindHook,
			Code: "sound", Name: "声音", IsEnabled: true}}}}
}

func (f *fakeStore) InsertOption(_ context.Context, id, vid, _ int64, o domain.Option, snap json.RawMessage) (repo.Option, error) {
	for _, x := range f.opts {
		if x.Code == o.Code {
			return repo.Option{}, repo.ErrDuplicate
		}
	}
	o.Version = 1
	row := repo.Option{ID: id, Option: o}
	f.opts[id] = row
	f.versions[id] = append(f.versions[id], repo.OptionVersion{ID: vid, OptionID: id, Version: 1, Snapshot: snap})
	return row, nil
}

func (f *fakeStore) UpdateOption(_ context.Context, id int64, expect int, vid, _ int64, o domain.Option,
	snap json.RawMessage) (repo.Option, error) {
	cur, ok := f.opts[id]
	if !ok {
		return repo.Option{}, repo.ErrNotFound
	}
	if cur.Version != expect {
		return repo.Option{}, repo.ErrStale
	}
	o.Version = expect + 1
	o.IsEnabled = cur.IsEnabled
	cur.Option = o
	f.opts[id] = cur
	f.versions[id] = append(f.versions[id], repo.OptionVersion{ID: vid, OptionID: id, Version: o.Version, Snapshot: snap})
	return cur, nil
}

func (f *fakeStore) GetOption(_ context.Context, id int64) (repo.Option, error) {
	o, ok := f.opts[id]
	if !ok {
		return repo.Option{}, repo.ErrNotFound
	}
	return o, nil
}

func (f *fakeStore) SetOptionEnabled(_ context.Context, id int64, enabled bool, _ int64) (repo.Option, bool, error) {
	o, ok := f.opts[id]
	if !ok {
		return repo.Option{}, false, repo.ErrNotFound
	}
	if o.IsEnabled == enabled {
		return o, false, nil
	}
	o.IsEnabled = enabled
	f.opts[id] = o
	return o, true, nil
}

func (f *fakeStore) ListOptions(_ context.Context, _ repo.OptionFilter) ([]repo.Option, bool, error) {
	var out []repo.Option
	for _, o := range f.opts {
		out = append(out, o)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, false, nil
}

func (f *fakeStore) SortOptions(context.Context, domain.Kind, string, []int64) error { return nil }

func (f *fakeStore) ListVersions(_ context.Context, id int64) ([]repo.OptionVersion, error) {
	return f.versions[id], nil
}

func (f *fakeStore) ListCategories(context.Context, domain.Kind) ([]repo.Category, error) {
	return f.cats, nil
}

func (f *fakeStore) GetCategory(_ context.Context, id int64) (repo.Category, error) {
	for _, c := range f.cats {
		if c.ID == id {
			return c, nil
		}
	}
	return repo.Category{}, repo.ErrNotFound
}

func (f *fakeStore) InsertCategory(_ context.Context, c repo.Category) (repo.Category, error) {
	f.cats = append(f.cats, c)
	return c, nil
}

func (f *fakeStore) UpdateCategory(_ context.Context, id int64, name string, enabled bool) (repo.Category, error) {
	for i := range f.cats {
		if f.cats[i].ID == id {
			f.cats[i].Name, f.cats[i].IsEnabled = name, enabled
			return f.cats[i], nil
		}
	}
	return repo.Category{}, repo.ErrNotFound
}

func (f *fakeStore) SortCategories(context.Context, domain.Kind, []int64) error { return nil }

type fakeModels map[string]ModelFacts

func (m fakeModels) LookupModels(_ context.Context, codes []string) (map[string]ModelFacts, error) {
	out := map[string]ModelFacts{}
	for _, c := range codes {
		if f, ok := m[c]; ok {
			out[c] = f
		}
	}
	return out, nil
}

type fakeSkills map[string]SkillFacts

func (s fakeSkills) LookupSkills(_ context.Context, codes []string) (map[string]SkillFacts, error) {
	out := map[string]SkillFacts{}
	for _, c := range codes {
		if f, ok := s[c]; ok {
			out[c] = f
		}
	}
	return out, nil
}

type fakeMedia map[int64]MediaFacts

func (m fakeMedia) LookupMedia(_ context.Context, id int64) (MediaFacts, bool, error) {
	f, ok := m[id]
	return f, ok, nil
}

type recAudit struct{ entries []audit.Entry }

func (r *recAudit) Write(_ context.Context, e audit.Entry) { r.entries = append(r.entries, e) }

const (
	coverImg   = int64(501)
	userImg    = int64(502)
	coverVideo = int64(503)
)

func newTestAdmin() (*Admin, *fakeStore, *recAudit) {
	st := newFakeStore()
	n := int64(1000)
	a := NewAdmin(st,
		fakeModels{
			"seedance":  {Code: "seedance", Domain: "video", Enabled: true, OutputTypes: []string{"video"}},
			"kling_off": {Code: "kling_off", Domain: "video", Enabled: false, OutputTypes: []string{"video"}},
			"img_only":  {Code: "img_only", Domain: "image", Enabled: true, OutputTypes: []string{"image"}},
		},
		fakeSkills{"mkt_skill": {Code: "mkt_skill", Enabled: true, ScenesKnown: true, Scenes: []string{"marketing"}},
			"gen_skill": {Code: "gen_skill", Enabled: true, ScenesKnown: true}},
		fakeMedia{
			coverImg:   {ID: coverImg, Purpose: purposeMaterialCover, ContentType: "image/png"},
			userImg:    {ID: userImg, WorkspaceID: 9, Purpose: "upload_reference", ContentType: "image/png"},
			coverVideo: {ID: coverVideo, Purpose: purposeMaterialCover, ContentType: "video/mp4"},
		},
		func() int64 { n++; return n })
	a.SetClock(func() time.Time { return time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC) })
	au := &recAudit{}
	a.SetAudit(au)
	return a, st, au
}

func creativeInput() OptionInput {
	return OptionInput{Kind: domain.KindCreative, Code: "giant_drop", Name: "巨物从天而降", CategoryCode: "drop",
		Description: "商品像巨物一样砸进城市", Prompt: "开场：商品从天而降砸进街道。", OutputTypes: []string{"video"}}
}

func bizErr(t *testing.T, err error) *httpx.BizError {
	t.Helper()
	var be *httpx.BizError
	if !errors.As(err, &be) {
		t.Fatalf("期望业务错误，实际 %v", err)
	}
	return be
}

func issuesOf(t *testing.T, be *httpx.BizError) []domain.Issue {
	t.Helper()
	is, _ := be.Details["issues"].([]domain.Issue)
	return is
}

func hasIssue(is []domain.Issue, field string, code domain.IssueCode) bool {
	for _, i := range is {
		if i.Field == field && i.Code == code {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------- 用例

func TestAdmin_推荐模型不存在拒_不可用告警(t *testing.T) {
	a, _, _ := newTestAdmin()
	ctx := context.Background()

	in := creativeInput()
	in.RecommendedModels = []string{"seedance", "no_such_model"}
	_, err := a.Create(ctx, 1, in)
	be := bizErr(t, err)
	if be.Code != errcode.InvalidArgument || !hasIssue(issuesOf(t, be), "recommendedModels[1]", IssueNotFound) {
		t.Fatalf("不存在的推荐模型应 40001 并点名 recommendedModels[1]：%v %+v", be.Code, be.Details)
	}

	in.RecommendedModels = []string{"kling_off", "img_only"}
	v, err := a.Create(ctx, 1, in)
	if err != nil {
		t.Fatalf("停用 / 不支持只告警，应可保存：%v", err)
	}
	if !hasIssue(v.Warnings, "recommendedModels[0]", IssueUnavailable) ||
		!hasIssue(v.Warnings, "recommendedModels[1]", IssueUnavailable) {
		t.Fatalf("应对停用与不支持各给告警：%+v", v.Warnings)
	}
	if v.IsEnabled {
		t.Fatal("新建必须默认停用")
	}
}

func TestAdmin_默认技能不存在拒(t *testing.T) {
	a, _, _ := newTestAdmin()
	in := creativeInput()
	in.DefaultSkillCode = "ghost"
	_, err := a.Create(context.Background(), 1, in)
	if !hasIssue(issuesOf(t, bizErr(t, err)), "defaultSkillCode", IssueNotFound) {
		t.Fatal("不存在的默认技能应被拒并点名字段")
	}
	in.DefaultSkillCode = "mkt_skill"
	v, err := a.Create(context.Background(), 1, in)
	if err != nil || hasIssue(v.Warnings, "defaultSkillCode", IssueUnavailable) {
		t.Fatalf("营销场景的技能应可保存且无告警：%v %+v", err, v.Warnings)
	}
	in.Code, in.DefaultSkillCode = "giant_drop_2", "gen_skill"
	v, err = a.Create(context.Background(), 1, in)
	if err != nil || !hasIssue(v.Warnings, "defaultSkillCode", IssueUnavailable) {
		t.Fatalf("不属营销场景的技能应只告警：%v %+v", err, v.Warnings)
	}
}

func TestAdmin_封面只收管理端上传(t *testing.T) {
	a, _, _ := newTestAdmin()
	in := creativeInput()
	in.CoverMediaIDs = []int64{userImg, 999, coverVideo}
	_, err := a.Create(context.Background(), 1, in)
	is := issuesOf(t, bizErr(t, err))
	if !hasIssue(is, "coverMediaIds[0]", IssueBadSource) || !hasIssue(is, "coverMediaIds[1]", IssueNotFound) ||
		!hasIssue(is, "coverMediaIds[2]", domain.IssueBadValue) {
		t.Fatalf("三个封面问题应一次报完：%+v", is)
	}
}

func TestAdmin_启用时校验封面与口播(t *testing.T) {
	a, _, _ := newTestAdmin()
	ctx := context.Background()
	in := creativeInput()
	in.Tags = []string{domain.TagTalk}
	v, err := a.Create(ctx, 1, in)
	if err != nil {
		t.Fatalf("无封面、口播缺提示句的草稿应可保存：%v", err)
	}
	if !hasIssue(v.Warnings, "coverMediaIds", domain.IssueCoverCount) || !hasIssue(v.Warnings, "", domain.IssueMissingNote) {
		t.Fatalf("保存时应告警无封面与缺提示句：%+v", v.Warnings)
	}

	_, err = a.Enable(ctx, 1, v.ID)
	be := bizErr(t, err)
	is := issuesOf(t, be)
	if be.Code != errcode.InvalidStateTransition || !hasIssue(is, "coverMediaIds", domain.IssueCoverCount) ||
		!hasIssue(is, "", domain.IssueMissingNote) {
		t.Fatalf("启用应 40905 并列出封面与口播两条：%v %+v", be.Code, is)
	}

	// 补齐封面与提示句后可启用。
	in.CoverMediaIDs = []int64{coverImg}
	in.Description = domain.TalkDisclaimer
	if _, err := a.Update(ctx, 1, v.ID, v.Version, in); err != nil {
		t.Fatal(err)
	}
	got, err := a.Enable(ctx, 1, v.ID)
	if err != nil || !got.IsEnabled || got.Lifecycle != domain.LifecycleLive {
		t.Fatalf("补齐后应可启用：%v %+v", err, got)
	}
}

func TestAdmin_禁用词保存被拦并点名字段(t *testing.T) {
	a, _, _ := newTestAdmin()
	in := creativeInput()
	in.Description = "数字人带货"
	_, err := a.Create(context.Background(), 1, in)
	if !hasIssue(issuesOf(t, bizErr(t, err)), "description", domain.IssueBannedWord) {
		t.Fatal("禁用词应拦截并点名 description")
	}
}

func TestAdmin_重复启用幂等(t *testing.T) {
	a, st, au := newTestAdmin()
	ctx := context.Background()
	in := creativeInput()
	in.CoverMediaIDs = []int64{coverImg}
	v, err := a.Create(ctx, 1, in)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Enable(ctx, 1, v.ID); err != nil {
		t.Fatal(err)
	}
	n := len(au.entries)
	got, err := a.Enable(ctx, 1, v.ID)
	if err != nil || !got.IsEnabled {
		t.Fatalf("重复启用应幂等成功：%v", err)
	}
	if len(au.entries) != n {
		t.Fatal("重复启用不应再写审计")
	}
	if _, err := a.Disable(ctx, 1, v.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := st.opts[v.ID]; !ok {
		t.Fatal("停用不删行")
	}
	if got := st.opts[v.ID].Version; got != 1 {
		t.Fatalf("启停不推进内容版本，实际 %d", got)
	}
}

func TestAdmin_审计不含片段全文(t *testing.T) {
	a, _, au := newTestAdmin()
	ctx := context.Background()
	in := creativeInput()
	in.Prompt = "非常独特的片段原文-ABCXYZ"
	v, err := a.Create(ctx, 7, in)
	if err != nil {
		t.Fatal(err)
	}
	in.Prompt = "第二版片段原文-QWERTY"
	if _, err := a.Update(ctx, 7, v.ID, v.Version, in); err != nil {
		t.Fatal(err)
	}
	if len(au.entries) != 2 {
		t.Fatalf("增改各一条审计，实际 %d", len(au.entries))
	}
	for _, e := range au.entries {
		b, _ := json.Marshal([]any{e.Before, e.After})
		s := string(b)
		if strings.Contains(s, "ABCXYZ") || strings.Contains(s, "QWERTY") {
			t.Fatalf("审计含片段原文：%s", s)
		}
		if !strings.Contains(s, "promptSha256") || !strings.Contains(s, "promptLength") {
			t.Fatalf("审计应记片段长度与哈希：%s", s)
		}
		if e.ActorID != 7 || e.ObjectType != "marketing_option" {
			t.Fatalf("审计主体 / 对象不对：%+v", e)
		}
	}
}

func TestAdmin_更新不可改code与kind_版本冲突40902(t *testing.T) {
	a, st, _ := newTestAdmin()
	ctx := context.Background()
	v, err := a.Create(ctx, 1, creativeInput())
	if err != nil {
		t.Fatal(err)
	}
	in := creativeInput()
	in.Code = "other_code"
	in.Kind = domain.KindHook
	_, err = a.Update(ctx, 1, v.ID, v.Version, in)
	is := issuesOf(t, bizErr(t, err))
	if !hasIssue(is, "code", IssueImmutable) || !hasIssue(is, "kind", IssueImmutable) {
		t.Fatalf("改 code / kind 应被拒：%+v", is)
	}
	if _, err := a.Update(ctx, 1, v.ID, v.Version, creativeInput()); err != nil {
		t.Fatal(err)
	}
	_, err = a.Update(ctx, 1, v.ID, v.Version, creativeInput())
	be := bizErr(t, err)
	if be.Code != errcode.VersionConflict || be.Details["currentVersion"] != 2 {
		t.Fatalf("旧版本保存应 40902 并带 currentVersion：%v %+v", be.Code, be.Details)
	}
	if n := len(st.versions[v.ID]); n != 2 {
		t.Fatalf("版本历史应为 2 行，实际 %d", n)
	}
}

func TestAdmin_时段倒置被拒且问题一次报完(t *testing.T) {
	a, _, _ := newTestAdmin()
	in := creativeInput()
	s := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	e := s.Add(-time.Hour)
	in.StartAt, in.EndAt = &s, &e
	in.Name = ""
	_, err := a.Create(context.Background(), 1, in)
	is := issuesOf(t, bizErr(t, err))
	if !hasIssue(is, "endAt", domain.IssueBadWindow) || !hasIssue(is, "name", domain.IssueRequired) {
		t.Fatalf("时段倒置与名称为空应同时报出：%+v", is)
	}
}

func TestAdmin_列表带lifecycle(t *testing.T) {
	a, st, _ := newTestAdmin()
	now := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	future, past := now.Add(24*time.Hour), now.Add(-24*time.Hour)
	mk := func(id int64, start, end *time.Time) {
		st.opts[id] = repo.Option{ID: id, Option: domain.Option{Kind: domain.KindCreative, Code: "c" + string(rune('a'+id)) + "xx",
			Name: "x", CategoryCode: "drop", OutputTypes: []string{"video"}, CoverMediaIDs: []int64{coverImg},
			Prompt: "p", IsEnabled: true, StartAt: start, EndAt: end, Version: 1}}
	}
	mk(1, &future, nil)
	mk(2, nil, &past)
	page, err := a.List(context.Background(), ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 2 || page.Items[0].Lifecycle != domain.LifecycleNotStarted ||
		page.Items[1].Lifecycle != domain.LifecycleEnded {
		t.Fatalf("管理端列表应返回未开始与已结束并标出：%+v", page.Items)
	}
}
