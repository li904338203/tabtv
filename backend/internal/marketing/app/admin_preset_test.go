package app

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/li904338203/tabtv/backend/internal/marketing/domain"
	"github.com/li904338203/tabtv/backend/internal/marketing/repo"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
)

// 内存版营销角色仓储：只模拟本服务依赖的语义（乐观锁、启停不推进版本、名称唯一）。
type fakePresetStore struct {
	rows   map[int64]repo.AdminPreset
	sorted []int64
}

func (f *fakePresetStore) InsertPreset(_ context.Context, id, _ int64, p domain.PresetInput) (repo.AdminPreset, error) {
	for _, r := range f.rows {
		if strings.EqualFold(r.Name, p.Name) {
			return repo.AdminPreset{}, repo.ErrDuplicate
		}
	}
	row := repo.AdminPreset{ID: id, PresetInput: p, Version: 1}
	f.rows[id] = row
	return row, nil
}

func (f *fakePresetStore) UpdatePreset(_ context.Context, id int64, expect int, _ int64,
	p domain.PresetInput) (repo.AdminPreset, error) {
	cur, ok := f.rows[id]
	if !ok {
		return repo.AdminPreset{}, repo.ErrNotFound
	}
	if cur.Version != expect {
		return repo.AdminPreset{}, repo.ErrStale
	}
	cur.PresetInput, cur.Version = p, cur.Version+1
	f.rows[id] = cur
	return cur, nil
}

func (f *fakePresetStore) AdminGetPreset(_ context.Context, id int64) (repo.AdminPreset, error) {
	r, ok := f.rows[id]
	if !ok {
		return repo.AdminPreset{}, repo.ErrNotFound
	}
	return r, nil
}

func (f *fakePresetStore) AdminListPresets(context.Context, repo.PresetAdminFilter) ([]repo.AdminPreset, bool, error) {
	out := make([]repo.AdminPreset, 0, len(f.rows))
	for _, r := range f.rows {
		out = append(out, r)
	}
	return out, false, nil
}

func (f *fakePresetStore) SetPresetEnabled(_ context.Context, id int64, enabled bool, _ int64) (repo.AdminPreset, bool, error) {
	r, ok := f.rows[id]
	if !ok {
		return repo.AdminPreset{}, false, repo.ErrNotFound
	}
	if r.IsEnabled == enabled {
		return r, false, nil
	}
	r.IsEnabled = enabled
	f.rows[id] = r
	return r, true, nil
}

func (f *fakePresetStore) SortPresets(_ context.Context, ids []int64) error {
	for _, id := range ids {
		if _, ok := f.rows[id]; !ok {
			return repo.ErrNotFound
		}
	}
	f.sorted = ids
	return nil
}

const (
	coverAudio = int64(504)
	coverGIF   = int64(505)
)

func newTestPresetAdmin() (*PresetAdmin, *fakePresetStore, *recAudit) {
	st := &fakePresetStore{rows: map[int64]repo.AdminPreset{}}
	n := int64(2000)
	a := NewPresetAdmin(st, fakeMedia{
		coverImg:   {ID: coverImg, Purpose: purposeMaterialCover, ContentType: "image/png"},
		userImg:    {ID: userImg, WorkspaceID: 9, Purpose: "upload_reference", ContentType: "image/png"},
		coverVideo: {ID: coverVideo, Purpose: purposeMaterialCover, ContentType: "video/mp4"},
		coverAudio: {ID: coverAudio, Purpose: purposeMaterialCover, ContentType: "audio/mpeg"},
		coverGIF:   {ID: coverGIF, Purpose: purposeMaterialCover, ContentType: "image/gif"},
	}, func() int64 { n++; return n })
	au := &recAudit{}
	a.SetAudit(au)
	return a, st, au
}

func presetInput() domain.PresetInput {
	audio := coverAudio
	return domain.PresetInput{Name: "柚子君", Summary: "圆脸柚子拟人", Appearance: "绿色叶子发型",
		VoiceText: "活泼少年音", ImageMediaIDs: []int64{coverImg}, VoiceMediaID: &audio}
}

func TestPresetAdminSvc_新建默认停用_无图可存草稿启用40905(t *testing.T) {
	a, _, au := newTestPresetAdmin()
	ctx := context.Background()
	in := presetInput()
	in.ImageMediaIDs = nil
	v, err := a.Create(ctx, 1, in)
	if err != nil {
		t.Fatalf("无图应可存草稿：%v", err)
	}
	if v.IsEnabled || v.Version != 1 {
		t.Fatalf("新建应停用、version=1：%+v", v.AdminPreset)
	}
	if !hasIssue(v.Warnings, "imageMediaIds", domain.IssueRequired) {
		t.Fatalf("无图应告警：%+v", v.Warnings)
	}
	_, err = a.Enable(ctx, 1, v.ID)
	be := bizErr(t, err)
	if be.Code != errcode.InvalidStateTransition || !hasIssue(issuesOf(t, be), "imageMediaIds", domain.IssueRequired) {
		t.Fatalf("无图启用应 40905 并点名 imageMediaIds：%v %+v", be.Code, be.Details)
	}
	if len(au.entries) != 1 {
		t.Fatalf("启用失败不写审计：%d", len(au.entries))
	}
}

func TestPresetAdminSvc_媒体来源与类型任何时候拦(t *testing.T) {
	a, _, _ := newTestPresetAdmin()
	ctx := context.Background()
	bad := int64(999)
	in := presetInput()
	in.ImageMediaIDs = []int64{userImg, coverVideo, coverGIF, bad}
	img := coverImg
	in.VoiceMediaID = &img
	_, err := a.Create(ctx, 1, in)
	be := bizErr(t, err)
	is := issuesOf(t, be)
	if be.Code != errcode.InvalidArgument ||
		!hasIssue(is, "imageMediaIds[0]", IssueBadSource) ||
		!hasIssue(is, "imageMediaIds[1]", domain.IssueBadValue) ||
		!hasIssue(is, "imageMediaIds[2]", domain.IssueBadValue) ||
		!hasIssue(is, "imageMediaIds[3]", IssueNotFound) ||
		!hasIssue(is, "voiceMediaId", domain.IssueBadValue) {
		t.Fatalf("一次报完：用户媒体 / 视频 / GIF / 不存在 / 音频位放了图：%v %+v", be.Code, is)
	}
}

func TestPresetAdminSvc_版本只随内容推进与冲突(t *testing.T) {
	a, st, au := newTestPresetAdmin()
	ctx := context.Background()
	v, err := a.Create(ctx, 1, presetInput())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Enable(ctx, 1, v.ID); err != nil {
		t.Fatalf("完整内容应能启用：%v", err)
	}
	if _, err := a.Enable(ctx, 1, v.ID); err != nil {
		t.Fatalf("重复启用应幂等：%v", err)
	}
	if err := a.Sort(ctx, 1, []int64{v.ID}); err != nil {
		t.Fatal(err)
	}
	if st.rows[v.ID].Version != 1 {
		t.Fatalf("启停与排序不推进版本：%d", st.rows[v.ID].Version)
	}
	for i := 1; i <= 3; i++ {
		in := presetInput()
		in.Summary = strings.Repeat("改", i)
		if _, err := a.Update(ctx, 1, v.ID, i, in); err != nil {
			t.Fatalf("第 %d 次更新：%v", i, err)
		}
	}
	if st.rows[v.ID].Version != 4 {
		t.Fatalf("改三次后 version 应为 4：%d", st.rows[v.ID].Version)
	}
	_, err = a.Update(ctx, 1, v.ID, 2, presetInput())
	be := bizErr(t, err)
	if be.Code != errcode.VersionConflict || be.Details["currentVersion"] != 4 {
		t.Fatalf("旧版本应 40902 带 currentVersion：%v %+v", be.Code, be.Details)
	}
	// 已启用的按启用标准校验：去掉图不许存。
	in := presetInput()
	in.ImageMediaIDs = nil
	_, err = a.Update(ctx, 1, v.ID, 4, in)
	if be := bizErr(t, err); be.Code != errcode.InvalidArgument {
		t.Fatalf("在投的营销角色去掉图应被拒：%v", be.Code)
	}
	// 审计不记全文。
	for _, e := range au.entries {
		raw, _ := json.Marshal([]any{e.Before, e.After})
		if strings.Contains(string(raw), "绿色叶子发型") || strings.Contains(string(raw), "活泼少年音") {
			t.Fatalf("审计不应记形象 / 音色原文：%s", raw)
		}
	}
}

func TestPresetAdminSvc_排序含不存在整笔拒_重名40904(t *testing.T) {
	a, st, _ := newTestPresetAdmin()
	ctx := context.Background()
	v, err := a.Create(ctx, 1, presetInput())
	if err != nil {
		t.Fatal(err)
	}
	err = a.Sort(ctx, 1, []int64{v.ID, 424242})
	if be := bizErr(t, err); be.Code != errcode.InvalidArgument || st.sorted != nil {
		t.Fatalf("含不存在 ID 应 40001 且不生效：%v %v", be.Code, st.sorted)
	}
	in := presetInput()
	in.Name = "柚子君"
	_, err = a.Create(ctx, 1, in)
	if be := bizErr(t, err); be.Code != errcode.DuplicateName || !hasIssue(issuesOf(t, be), "name", domain.IssueDuplicate) {
		t.Fatalf("重名应 40904 点名 name：%v %+v", be.Code, be.Details)
	}
}
