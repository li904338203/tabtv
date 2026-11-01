package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/li904338203/tabtv/backend/internal/marketing/domain"
	"github.com/li904338203/tabtv/backend/internal/marketing/repo"
	"github.com/li904338203/tabtv/backend/internal/platform/audit"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// 管理端营销角色（平台预置，MKT-T-36；02 MKT-F-120~124、MKT-D-52；dev/05 §3.2、MKD-30、MKD-32）。
//
// 写法照营销选项（admin.go）：新建一律停用、整份更新带 version 乐观锁、启用闸 40905 + issues、
// 启停幂等、排序整笔回滚、审计。
//
// ⚠️ 新建落**停用**：上线是一次显式操作，一次误保存不该等于全站上线。
// ⚠️ 没有删除（MKT-F-120）：用户已添加的副本按 source_preset_id 追溯，只停用。
// ⚠️ 媒体必须是管理端上传的平台素材（workspace_id=0 ∧ purpose=material_cover）：用户工作区的文件
//    任何时候都拦 —— 否则一个运营粘贴错的 ID 就把某个用户的私有图片发到了全站。
// ⚠️ 图片按 AllowedImageMime 收（png / jpeg / webp）：用户「添加」后副本按工作区角色的规则用，
//    GIF 在这里放进去，那边就成了一张存不回去的图。
// ⚠️ 审计 diff 只记字段名与长度（形象 / 音色描述可能很长，审计表不是存档）。

// PresetAdminStore 是营销角色管理端的读写（marketing/repo 实现）。
type PresetAdminStore interface {
	InsertPreset(ctx context.Context, id, adminID int64, p domain.PresetInput) (repo.AdminPreset, error)
	UpdatePreset(ctx context.Context, id int64, expect int, adminID int64, p domain.PresetInput) (repo.AdminPreset, error)
	AdminGetPreset(ctx context.Context, id int64) (repo.AdminPreset, error)
	AdminListPresets(ctx context.Context, f repo.PresetAdminFilter) ([]repo.AdminPreset, bool, error)
	SetPresetEnabled(ctx context.Context, id int64, enabled bool, adminID int64) (repo.AdminPreset, bool, error)
	SortPresets(ctx context.Context, ids []int64) error
}

// PresetAdmin 是营销角色管理端服务。
type PresetAdmin struct {
	store  PresetAdminStore
	media  MediaLookup
	audit  Auditor
	nextID func() int64
	now    Clock
}

// NewPresetAdmin 构造。media 必须给：缺了等于放行一切媒体引用。
func NewPresetAdmin(store PresetAdminStore, media MediaLookup, nextID func() int64) *PresetAdmin {
	return &PresetAdmin{store: store, media: media, nextID: nextID, now: func() time.Time { return time.Now().UTC() }}
}

// SetAudit 注入审计。
func (a *PresetAdmin) SetAudit(au Auditor) { a.audit = au }

// AdminPresetView 是管理端看到的一条营销角色 + 服务端算好的问题。
type AdminPresetView struct {
	repo.AdminPreset
	Warnings []domain.Issue
}

// AdminPresetPage 是列表的一页。
type AdminPresetPage struct {
	Items      []AdminPresetView
	NextCursor string
}

// AdminPresetFilter 是列表筛选（transport 解析好）。
type AdminPresetFilter struct {
	Enabled *bool
	Q       string
	Cursor  string
	Limit   int
}

// List 管理端列表（含停用），每行带校验问题。
func (a *PresetAdmin) List(ctx context.Context, f AdminPresetFilter) (AdminPresetPage, error) {
	rf := repo.PresetAdminFilter{Enabled: f.Enabled, Q: f.Q, Limit: f.Limit}
	if rf.Limit <= 0 || rf.Limit > 200 {
		rf.Limit = 50
	}
	if f.Cursor != "" {
		s, id, ok := parseCursor(f.Cursor)
		if !ok {
			return AdminPresetPage{}, httpx.Errf(errcode.InvalidArgument, "cursor 非法")
		}
		rf.CursorSort, rf.CursorID = s, id
	}
	rows, more, err := a.store.AdminListPresets(ctx, rf)
	if err != nil {
		return AdminPresetPage{}, httpx.Wrap(errcode.Internal, err)
	}
	lk := map[int64]*MediaFacts{}
	page := AdminPresetPage{Items: make([]AdminPresetView, 0, len(rows))}
	for _, p := range rows {
		v, err := a.evaluate(ctx, p.PresetInput, lk)
		if err != nil {
			return AdminPresetPage{}, err
		}
		page.Items = append(page.Items, AdminPresetView{AdminPreset: p, Warnings: v.listWarnings()})
	}
	if more && len(rows) > 0 {
		last := rows[len(rows)-1]
		page.NextCursor = fmt.Sprintf("%d_%d", last.Sort, last.ID)
	}
	return page, nil
}

// Get 取一条（含停用的）。
func (a *PresetAdmin) Get(ctx context.Context, id int64) (AdminPresetView, error) {
	p, err := a.store.AdminGetPreset(ctx, id)
	if err != nil {
		return AdminPresetView{}, mapErr(err, "营销角色不存在")
	}
	return a.view(ctx, p)
}

func (a *PresetAdmin) view(ctx context.Context, p repo.AdminPreset) (AdminPresetView, error) {
	v, err := a.evaluate(ctx, p.PresetInput, map[int64]*MediaFacts{})
	if err != nil {
		return AdminPresetView{}, err
	}
	return AdminPresetView{AdminPreset: p, Warnings: v.listWarnings()}, nil
}

// Create 新建：默认停用；无图可存草稿（告警），名称 / 媒体来源问题任何时候拦。
func (a *PresetAdmin) Create(ctx context.Context, adminID int64, in domain.PresetInput) (AdminPresetView, error) {
	in = in.Normalized()
	v, err := a.evaluate(ctx, in, map[int64]*MediaFacts{})
	if err != nil {
		return AdminPresetView{}, err
	}
	if err := v.presetSaveError(false); err != nil {
		return AdminPresetView{}, err
	}
	row, err := a.store.InsertPreset(ctx, a.nextID(), adminID, in)
	if err != nil {
		return AdminPresetView{}, presetWriteErr(err, in.Name)
	}
	a.write(ctx, adminID, "marketing_preset_character.create", row.ID, nil, presetAudit(row))
	return AdminPresetView{AdminPreset: row, Warnings: v.saveWarnings(false)}, nil
}

// Update 按 version 乐观锁整份更新，version+1。
//
// ⚠️ 已启用的按**启用标准**校验：在投的营销角色被改成没有图，等于绕过了启用闸。
func (a *PresetAdmin) Update(ctx context.Context, adminID, id int64, version int, in domain.PresetInput) (AdminPresetView, error) {
	before, err := a.store.AdminGetPreset(ctx, id)
	if err != nil {
		return AdminPresetView{}, mapErr(err, "营销角色不存在")
	}
	in = in.Normalized()
	v, err := a.evaluate(ctx, in, map[int64]*MediaFacts{})
	if err != nil {
		return AdminPresetView{}, err
	}
	if err := v.presetSaveError(before.IsEnabled); err != nil {
		return AdminPresetView{}, err
	}
	row, err := a.store.UpdatePreset(ctx, id, version, adminID, in)
	if err != nil {
		if errors.Is(err, repo.ErrStale) {
			be := httpx.Errf(errcode.VersionConflict, "营销角色已被其他人修改，请重新加载后再保存（你的修改不会被静默覆盖）")
			if cur, gerr := a.store.AdminGetPreset(ctx, id); gerr == nil {
				be = be.WithDetails(map[string]any{"currentVersion": cur.Version})
			}
			return AdminPresetView{}, be
		}
		return AdminPresetView{}, presetWriteErr(err, in.Name)
	}
	a.write(ctx, adminID, "marketing_preset_character.update", id, presetAudit(before), presetAudit(row))
	return AdminPresetView{AdminPreset: row, Warnings: v.saveWarnings(row.IsEnabled)}, nil
}

// Enable 启用：完整校验（≥ 1 张图、名称合法、媒体来源与类型），不过 40905 + details.issues[]。已启用时幂等。
func (a *PresetAdmin) Enable(ctx context.Context, adminID, id int64) (AdminPresetView, error) {
	before, err := a.store.AdminGetPreset(ctx, id)
	if err != nil {
		return AdminPresetView{}, mapErr(err, "营销角色不存在")
	}
	if before.IsEnabled {
		return a.view(ctx, before)
	}
	v, err := a.evaluate(ctx, before.PresetInput, map[int64]*MediaFacts{})
	if err != nil {
		return AdminPresetView{}, err
	}
	if block := append(append([]domain.Issue{}, v.hard...), v.escalated()...); len(block) > 0 {
		return AdminPresetView{}, issuesErr(errcode.InvalidStateTransition, "营销角色不能启用", block, v.warn)
	}
	return a.setEnabled(ctx, adminID, before, true)
}

// Disable 停用（不删行；用户已添加的副本照常可用）。已停用时幂等。
func (a *PresetAdmin) Disable(ctx context.Context, adminID, id int64) (AdminPresetView, error) {
	before, err := a.store.AdminGetPreset(ctx, id)
	if err != nil {
		return AdminPresetView{}, mapErr(err, "营销角色不存在")
	}
	return a.setEnabled(ctx, adminID, before, false)
}

func (a *PresetAdmin) setEnabled(ctx context.Context, adminID int64, before repo.AdminPreset, enabled bool) (AdminPresetView, error) {
	row, changed, err := a.store.SetPresetEnabled(ctx, before.ID, enabled, adminID)
	if err != nil {
		return AdminPresetView{}, mapErr(err, "营销角色不存在")
	}
	if changed {
		action := "marketing_preset_character.disable"
		if enabled {
			action = "marketing_preset_character.enable"
		}
		a.write(ctx, adminID, action, row.ID,
			map[string]any{"isEnabled": before.IsEnabled, "version": before.Version},
			map[string]any{"isEnabled": row.IsEnabled, "version": row.Version})
	}
	return a.view(ctx, row)
}

// Sort 按 ids 顺序排成 1..N；任一不存在整笔回滚。
func (a *PresetAdmin) Sort(ctx context.Context, adminID int64, ids []int64) error {
	if err := checkSortIDs(ids); err != nil {
		return err
	}
	if err := a.store.SortPresets(ctx, ids); err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			return httpx.Errf(errcode.InvalidArgument, "ids 里有不存在的营销角色（整笔未生效）")
		}
		return httpx.Wrap(errcode.Internal, err)
	}
	a.write(ctx, adminID, "marketing_preset_character.sort", 0, nil, map[string]any{"ids": idStrings(ids)})
	return nil
}

// ---------------------------------------------------------------- 校验

// evaluate 一次报完：domain 规则（草稿 / 启用两遍，分出 enableOnly）+ 媒体存在 / 来源 / 类型（任何时候拦）。
func (a *PresetAdmin) evaluate(ctx context.Context, p domain.PresetInput, lk map[int64]*MediaFacts) (verdict, error) {
	var v verdict
	hardKeys := map[string]bool{}
	for _, i := range domain.ValidatePreset(p, false) {
		if i.Level == domain.LevelError {
			v.hard = append(v.hard, i)
			hardKeys[i.Field+"|"+string(i.Code)] = true
		} else {
			v.warn = append(v.warn, i)
		}
	}
	// 启用时才拦的（没图）：从草稿告警里挪到 enableOnly，避免同一条既是 warn 又是 enableOnly。
	enableKeys := map[string]bool{}
	for _, i := range domain.ValidatePreset(p, true) {
		if i.Level == domain.LevelError && !hardKeys[i.Field+"|"+string(i.Code)] {
			v.enableOnly = append(v.enableOnly, i)
			enableKeys[i.Field+"|"+string(i.Code)] = true
		}
	}
	warn := v.warn[:0]
	for _, w := range v.warn {
		if !enableKeys[w.Field+"|"+string(w.Code)] {
			warn = append(warn, w)
		}
	}
	v.warn = warn

	for i, id := range p.ImageMediaIDs {
		if id <= 0 || indexOfID(p.ImageMediaIDs[:i], id) >= 0 {
			continue // domain 已报非法 / 重复
		}
		is, err := a.checkMedia(ctx, lk, fmt.Sprintf("imageMediaIds[%d]", i), "角色图", id, true)
		if err != nil {
			return verdict{}, err
		}
		v.hard = append(v.hard, is...)
	}
	if p.VoiceMediaID != nil && *p.VoiceMediaID > 0 {
		is, err := a.checkMedia(ctx, lk, "voiceMediaId", "参考音频", *p.VoiceMediaID, false)
		if err != nil {
			return verdict{}, err
		}
		v.hard = append(v.hard, is...)
	}
	return v, nil
}

func (a *PresetAdmin) checkMedia(ctx context.Context, lk map[int64]*MediaFacts, field, label string, id int64,
	image bool) ([]domain.Issue, error) {
	m, ok := lk[id]
	if !ok {
		if a.media == nil {
			return nil, httpx.Errf(errcode.Internal, "服务端未接入媒体域，无法校验营销角色媒体")
		}
		f, found, err := a.media.LookupMedia(ctx, id)
		if err != nil {
			return nil, httpx.Wrap(errcode.Internal, err)
		}
		if found {
			m = &f
		}
		lk[id] = m
	}
	switch {
	case m == nil:
		return []domain.Issue{errIssue(field, IssueNotFound, label+"媒体不存在或已删除")}, nil
	case m.WorkspaceID != 0 || m.Purpose != purposeMaterialCover:
		return []domain.Issue{errIssue(field, IssueBadSource,
			label+"须为管理端上传的平台素材（用途 material_cover），不能引用用户工作区的文件")}, nil
	case image && !domain.AllowedImageMime(m.ContentType):
		return []domain.Issue{errIssue(field, domain.IssueBadValue,
			fmt.Sprintf("%s只收 PNG / JPEG / WebP，当前是 %s", label, m.ContentType))}, nil
	case !image && !strings.HasPrefix(m.ContentType, "audio/"):
		return []domain.Issue{errIssue(field, domain.IssueBadValue,
			fmt.Sprintf("%s须为音频文件，当前是 %s", label, m.ContentType))}, nil
	}
	return nil, nil
}

func indexOfID(xs []int64, x int64) int {
	for i, v := range xs {
		if v == x {
			return i
		}
	}
	return -1
}

// presetSaveError 同 verdict.saveError（草稿只拦 hard，已启用的连 enableOnly 一起拦），标题换成营销角色。
func (v verdict) presetSaveError(enabled bool) error {
	block := append([]domain.Issue{}, v.hard...)
	if enabled {
		block = append(block, v.escalated()...)
	}
	if len(block) == 0 {
		return nil
	}
	return issuesErr(errcode.InvalidArgument, "营销角色配置有问题", block, v.saveWarnings(enabled))
}

func presetWriteErr(err error, name string) error {
	switch {
	case errors.Is(err, repo.ErrDuplicate):
		return httpx.Errf(errcode.DuplicateName, fmt.Sprintf("营销角色「%s」已存在（名称不区分大小写全局唯一）", name)).
			WithDetails(map[string]any{"issues": []domain.Issue{errIssue("name", domain.IssueDuplicate, "名称已存在")},
				"warnings": []domain.Issue{}})
	case errors.Is(err, repo.ErrNotFound):
		return httpx.Errf(errcode.NotFound, "营销角色不存在")
	}
	return httpx.Wrap(errcode.Internal, err)
}

// presetAudit 是审计里的内容：**文本只记长度**，不记原文。
func presetAudit(p repo.AdminPreset) map[string]any {
	var voice any
	if p.VoiceMediaID != nil {
		voice = fmt.Sprint(*p.VoiceMediaID)
	}
	return map[string]any{
		"nameLength": domain.RuneLen(p.Name), "summaryLength": domain.RuneLen(p.Summary),
		"appearanceLength": domain.RuneLen(p.Appearance), "voiceTextLength": domain.RuneLen(p.VoiceText),
		"imageMediaIds": idStrings(p.ImageMediaIDs), "voiceMediaId": voice,
		"sort": p.Sort, "isEnabled": p.IsEnabled, "version": p.Version,
	}
}

func (a *PresetAdmin) write(ctx context.Context, adminID int64, action string, id int64, before, after any) {
	if a.audit == nil {
		return
	}
	a.audit.Write(ctx, audit.Entry{ActorType: audit.ActorAdmin, ActorID: adminID, Action: action,
		ObjectType: "marketing_preset_character", ObjectID: id, Before: before, After: after})
}
