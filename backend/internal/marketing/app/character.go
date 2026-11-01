package app

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/li904338203/tabtv/backend/internal/marketing/domain"
	"github.com/li904338203/tabtv/backend/internal/marketing/repo"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// 角色库与营销角色用户端（MKT-T-34；02 MK-23~25、MKT-F-101~105、109~112；dev/05 MKD-23、30、31、33、36）。
//
// ⚠️ 工作区隔离靠仓储里每条 SQL 的 workspace_id 条件；本服务不负责权限（路由层工作区闸，MKD-32）。
// ⚠️ 预置角色的媒体**永远不直接作为生成输入**（MKD-30）：用户「添加」时复制字节进本工作区，
// 下游看到的只有本工作区的媒体，不新开任何跨租户读取路径。

// CharacterStore 是工作区角色的读写（marketing/repo 实现）。
type CharacterStore interface {
	InsertCharacter(ctx context.Context, c repo.Character, max int) (repo.Character, error)
	GetCharacter(ctx context.Context, workspaceID, id int64) (repo.Character, error)
	GetCharacterByPreset(ctx context.Context, workspaceID, presetID int64, version int) (repo.Character, error)
	UpdateCharacter(ctx context.Context, c repo.Character, version int) (repo.Character, error)
	SoftDeleteCharacter(ctx context.Context, workspaceID, id, userID int64) (repo.Character, error)
	ListCharacters(ctx context.Context, f repo.CharacterFilter) ([]repo.Character, bool, error)
}

// PresetReader 是营销角色（预置）的用户端只读查询：只认启用的。
type PresetReader interface {
	ListEnabledPresets(ctx context.Context, f repo.PresetFilter) ([]repo.PresetCharacter, bool, error)
	GetEnabledPreset(ctx context.Context, id int64) (repo.PresetCharacter, error)
}

// SetCharacters 注入角色库依赖（MKT-T-34）。copier 为 nil 时 adopt 回 500。
func (l *Library) SetCharacters(chars CharacterStore, presets PresetReader, copier MediaCopier) {
	l.characters, l.presets, l.copier = chars, presets, copier
}

func (l *Library) maxCharacters() int {
	if l.cfg.MaxCharacters > 0 {
		return l.cfg.MaxCharacters
	}
	return domain.DefaultMaxCharacters
}

func (l *Library) charactersReady() error {
	if l.characters == nil {
		return httpx.Errf(errcode.Internal, "服务端未接入角色库")
	}
	return nil
}

// CharacterPage 是一页角色。
type CharacterPage struct {
	Items      []repo.Character
	NextCursor string
}

// ListCharacters 列出本工作区角色（含 source=preset 的副本；**不含**脚本角色资产，那由前端另取）。
func (l *Library) ListCharacters(ctx context.Context, a LibraryActor, q ProductQuery) (CharacterPage, error) {
	if err := l.charactersReady(); err != nil {
		return CharacterPage{}, err
	}
	sort := repo.ProductSort(q.Sort)
	if q.Sort == "" {
		sort = repo.ProductSortUpdated
	}
	if !sort.Valid() {
		return CharacterPage{}, httpx.Errf(errcode.InvalidArgument, "sort 只能是 updated / created / name")
	}
	limit := clampPageSize(q.Limit)
	items, more, err := l.characters.ListCharacters(ctx, repo.CharacterFilter{WorkspaceID: a.WorkspaceID,
		Q: q.Q, Sort: sort, Cursor: q.Cursor, Limit: limit})
	if errors.Is(err, repo.ErrBadCursor) {
		return CharacterPage{}, httpx.Errf(errcode.InvalidArgument, "cursor 非法（排序变了要从第一页开始）")
	}
	if err != nil {
		return CharacterPage{}, httpx.Wrap(errcode.Internal, err)
	}
	page := CharacterPage{Items: items}
	if more && len(items) > 0 {
		page.NextCursor = repo.EncodeCharacterCursor(sort, items[len(items)-1])
	}
	return page, nil
}

func clampPageSize(n int) int {
	switch {
	case n <= 0:
		return defaultLibraryPageSize
	case n > maxLibraryPageSize:
		return maxLibraryPageSize
	}
	return n
}

// GetCharacter 取详情。他人工作区 / 已删除 / 不存在一律 404。
func (l *Library) GetCharacter(ctx context.Context, a LibraryActor, id int64) (repo.Character, error) {
	if err := l.charactersReady(); err != nil {
		return repo.Character{}, err
	}
	c, err := l.characters.GetCharacter(ctx, a.WorkspaceID, id)
	if err != nil {
		return repo.Character{}, characterErr(err)
	}
	return c, nil
}

// CreateCharacter 新建角色（source=user）。重名 409 details{field:"name",code:"duplicate"}；配额满 409 reason=quota。
func (l *Library) CreateCharacter(ctx context.Context, a LibraryActor, in domain.CharacterInput) (repo.Character, error) {
	if err := l.charactersReady(); err != nil {
		return repo.Character{}, err
	}
	in = in.Normalized()
	if err := l.validateCharacter(ctx, a.WorkspaceID, in); err != nil {
		return repo.Character{}, err
	}
	max := l.maxCharacters()
	c, err := l.characters.InsertCharacter(ctx, repo.Character{ID: l.nextID(), WorkspaceID: a.WorkspaceID,
		Name: in.Name, ImageMediaIDs: in.ImageMediaIDs, Appearance: in.Appearance, StyleCode: in.StyleCode,
		VoiceText: in.VoiceText, VoiceMediaID: in.VoiceMediaID, Source: repo.CharacterSourceUser,
		CreatedBy: a.UserID}, max)
	switch {
	case errors.Is(err, repo.ErrQuota):
		return repo.Character{}, quotaErr("角色", max)
	case errors.Is(err, repo.ErrDuplicate):
		return repo.Character{}, duplicateNameErr(in.Name)
	case err != nil:
		return repo.Character{}, httpx.Wrap(errcode.Internal, err)
	}
	return c, nil
}

// UpdateCharacter 整份更新（乐观锁）。来源字段不可改（transport 已拒）。
func (l *Library) UpdateCharacter(ctx context.Context, a LibraryActor, id int64, version int,
	in domain.CharacterInput) (repo.Character, error) {
	if err := l.charactersReady(); err != nil {
		return repo.Character{}, err
	}
	if version < 1 {
		return repo.Character{}, issuesErr(errcode.InvalidArgument, "角色内容有问题",
			[]domain.Issue{errIssue("version", domain.IssueRequired, "缺少 version（乐观锁）")}, nil)
	}
	in = in.Normalized()
	// 先确认存在：他人工作区的角色回 404，而不是先报「图片不属于本工作区」。
	if _, err := l.characters.GetCharacter(ctx, a.WorkspaceID, id); err != nil {
		return repo.Character{}, characterErr(err)
	}
	if err := l.validateCharacter(ctx, a.WorkspaceID, in); err != nil {
		return repo.Character{}, err
	}
	c, err := l.characters.UpdateCharacter(ctx, repo.Character{ID: id, WorkspaceID: a.WorkspaceID,
		Name: in.Name, ImageMediaIDs: in.ImageMediaIDs, Appearance: in.Appearance, StyleCode: in.StyleCode,
		VoiceText: in.VoiceText, VoiceMediaID: in.VoiceMediaID, UpdatedBy: a.UserID}, version)
	switch {
	case errors.Is(err, repo.ErrStale):
		return repo.Character{}, l.characterConflict(ctx, a, id)
	case errors.Is(err, repo.ErrDuplicate):
		return repo.Character{}, duplicateNameErr(in.Name)
	case err != nil:
		return repo.Character{}, characterErr(err)
	}
	return c, nil
}

// DeleteCharacter 软删，然后尽力删除它自有的上传媒体（图片 + 参考音频，MKD-31）。
//
// ⚠️ 顺序同商品：先软删再删媒体（检查器只认未删除的角色）。被会话快照、另一个角色、
// 生成检查器（generation_output）保护的媒体会被拒 —— 正常，保留即可。
// adopt 复制来的字节也是 upload_reference，属于本工作区自有，一并尝试删除。
func (l *Library) DeleteCharacter(ctx context.Context, a LibraryActor, id int64) error {
	if err := l.charactersReady(); err != nil {
		return err
	}
	c, err := l.characters.SoftDeleteCharacter(ctx, a.WorkspaceID, id, a.UserID)
	if err != nil {
		return characterErr(err)
	}
	ids := append([]int64{}, c.ImageMediaIDs...)
	if c.VoiceMediaID > 0 {
		ids = append(ids, c.VoiceMediaID)
	}
	l.releaseMedia(ctx, a.WorkspaceID, "marketing_character", c.ID, ids)
	return nil
}

// validateCharacter 字段校验 + 图片 / 音频归属与格式，一次报完（MKT-AC-36）。
func (l *Library) validateCharacter(ctx context.Context, ws int64, in domain.CharacterInput) error {
	issues := domain.ValidateCharacter(in)
	bad := map[string]bool{}
	for _, i := range issues {
		bad[i.Field] = true
	}
	mediaIssues, err := l.imageIssues(ctx, ws, in.ImageMediaIDs, domain.MaxCharacterImages, bad, "角色图片")
	if err != nil {
		return err
	}
	issues = append(issues, mediaIssues...)
	if in.VoiceMediaID > 0 && !bad["voiceMediaId"] {
		f, found, err := l.media.LookupWorkspaceMedia(ctx, ws, in.VoiceMediaID)
		if err != nil {
			return httpx.Wrap(errcode.Internal, err)
		}
		switch {
		case !found || f.WorkspaceID != ws:
			issues = append(issues, errIssue("voiceMediaId", domain.IssueBadValue, "参考音频不存在或不属于当前工作区"))
		case !domain.AllowedAudioMime(f.ContentType):
			issues = append(issues, errIssue("voiceMediaId", domain.IssueBadFormat,
				"参考音频必须是音频文件（MP3 / WAV / OGG），视频与图片不能作为参考音频"))
		}
	}
	if domain.HasError(issues) {
		return issuesErr(errcode.InvalidArgument, "角色内容有问题", issues, nil)
	}
	return nil
}

// characterConflict 给出「被谁改了」（CD-58 冲突可解释）。
func (l *Library) characterConflict(ctx context.Context, a LibraryActor, id int64) error {
	be := httpx.Errf(errcode.VersionConflict, "角色已被修改，请重新加载后再保存（你的修改不会被静默覆盖）")
	cur, err := l.characters.GetCharacter(ctx, a.WorkspaceID, id)
	if err != nil {
		return be
	}
	bySelf := cur.UpdatedBy != 0 && cur.UpdatedBy == a.UserID
	if bySelf {
		be = httpx.Errf(errcode.VersionConflict, "你在另一个窗口修改过这个角色，请重新加载后再保存")
	} else if cur.UpdatedByName != "" {
		be = httpx.Errf(errcode.VersionConflict,
			fmt.Sprintf("「%s」刚修改过这个角色，请重新加载后再保存（你的修改不会被静默覆盖）", cur.UpdatedByName))
	}
	return be.WithDetails(map[string]any{
		"currentVersion":    cur.Version,
		"currentUpdatedAt":  cur.UpdatedAt.UTC().Format(time.RFC3339),
		"currentEditorId":   strconv.FormatInt(cur.UpdatedBy, 10),
		"currentEditorName": cur.UpdatedByName,
		"bySelf":            bySelf,
	})
}

func duplicateNameErr(name string) error {
	return httpx.Errf(errcode.Conflict, fmt.Sprintf("当前工作区已有名为「%s」的角色，请换一个名字", name)).
		WithDetails(map[string]any{"field": "name", "code": string(domain.IssueDuplicate)})
}

func characterErr(err error) error {
	if errors.Is(err, repo.ErrNotFound) {
		return httpx.Errf(errcode.NotFound, "角色不存在")
	}
	return httpx.Wrap(errcode.Internal, err)
}

// ---------------------------------------------------------------- 营销角色（预置）

// presetNotFound 是停用 / 不存在 / 下标越界的同一个回答（不给探测留缝）。
func presetNotFound() error { return httpx.Errf(errcode.NotFound, "营销角色不存在或已下线") }

// PresetPage 是一页营销角色。
type PresetPage struct {
	Items      []repo.PresetCharacter
	NextCursor string
}

// ListPresets 列出启用的营销角色（按 sort, id）。
func (l *Library) ListPresets(ctx context.Context, q, cursor string, limit int) (PresetPage, error) {
	if l.presets == nil {
		return PresetPage{}, httpx.Errf(errcode.Internal, "服务端未接入营销角色")
	}
	items, more, err := l.presets.ListEnabledPresets(ctx, repo.PresetFilter{Q: q, Cursor: cursor,
		Limit: clampPageSize(limit)})
	if errors.Is(err, repo.ErrBadCursor) {
		return PresetPage{}, httpx.Errf(errcode.InvalidArgument, "cursor 非法")
	}
	if err != nil {
		return PresetPage{}, httpx.Wrap(errcode.Internal, err)
	}
	page := PresetPage{Items: items}
	if more && len(items) > 0 {
		page.NextCursor = repo.EncodePresetCursor(items[len(items)-1])
	}
	return page, nil
}

func (l *Library) enabledPreset(ctx context.Context, id int64) (repo.PresetCharacter, error) {
	if l.presets == nil {
		return repo.PresetCharacter{}, httpx.Errf(errcode.Internal, "服务端未接入营销角色")
	}
	p, err := l.presets.GetEnabledPreset(ctx, id)
	if errors.Is(err, repo.ErrNotFound) {
		return repo.PresetCharacter{}, presetNotFound()
	}
	if err != nil {
		return repo.PresetCharacter{}, httpx.Wrap(errcode.Internal, err)
	}
	return p, nil
}

// PresetImageMedia 返回启用的营销角色第 index 张图（从 0 起）。越界与不可见同一个 404。
func (l *Library) PresetImageMedia(ctx context.Context, id int64, index int) (int64, error) {
	p, err := l.enabledPreset(ctx, id)
	if err != nil {
		return 0, err
	}
	if index < 0 || index >= len(p.ImageMediaIDs) {
		return 0, presetNotFound()
	}
	return p.ImageMediaIDs[index], nil
}

// PresetVoiceMedia 返回启用且有参考音频的营销角色的音频。没有音频与不可见同一个 404。
func (l *Library) PresetVoiceMedia(ctx context.Context, id int64) (int64, error) {
	p, err := l.enabledPreset(ctx, id)
	if err != nil {
		return 0, err
	}
	if p.VoiceMediaID <= 0 {
		return 0, presetNotFound()
	}
	return p.VoiceMediaID, nil
}

// adoptNameSuffix 是名称与「我的」已有角色撞名时追加的后缀（MKT-F-111）。
//
// ⚠️ 不能用括号（04 文档 P-49）：原先是「（营销角色）」，而全角括号在角色名禁用字符表里（MKD-36）——
// 加完后缀的副本不改名就保存不了，用户改任何字段都回 40001「角色名称不能包含（」。
const adoptNameSuffix = "-营销角色"

// Adopt 把一个启用的营销角色复制进本工作区（MKD-30）。返回 (角色, 是否本次新建)。
//
// 顺序：判启用 → 查 (工作区, 预置, 版本) 已有副本（有就回那一份）→ 逐张复制字节（图片 + 音频）
// → 插入 source=preset。
// ⚠️ CopyToWorkspace 不校验源归属：只能在判完「启用」之后、对**这个预置角色自己的**媒体调用。
// ⚠️ 插入撞「副本唯一」（两个请求同时 adopt）→ 回已有那一份；撞「名称唯一」→ 加后缀重试一次，仍冲突 409。
// 复制出来的字节若插入最终失败会留在工作区里（upload_reference），下次 adopt 按来源去重复用，不会越堆越多。
func (l *Library) Adopt(ctx context.Context, a LibraryActor, presetID int64) (repo.Character, bool, error) {
	if err := l.charactersReady(); err != nil {
		return repo.Character{}, false, err
	}
	p, err := l.enabledPreset(ctx, presetID)
	if err != nil {
		return repo.Character{}, false, err
	}
	if c, err := l.characters.GetCharacterByPreset(ctx, a.WorkspaceID, p.ID, p.Version); err == nil {
		return c, false, nil
	} else if !errors.Is(err, repo.ErrNotFound) {
		return repo.Character{}, false, httpx.Wrap(errcode.Internal, err)
	}
	if len(p.ImageMediaIDs) == 0 {
		// 启用校验（MKT-T-36）要求至少 1 张图；走到这里说明数据被绕过校验改过。
		return repo.Character{}, false, httpx.Errf(errcode.Conflict, "该营销角色暂时不能添加（缺少图片）")
	}
	if l.copier == nil {
		return repo.Character{}, false, httpx.Errf(errcode.Internal, "服务端未接入媒体复制")
	}
	images := make([]int64, 0, len(p.ImageMediaIDs))
	for _, mid := range p.ImageMediaIDs {
		nid, err := l.copier.CopyToWorkspace(ctx, mid, a.WorkspaceID, a.UserID)
		if err != nil {
			return repo.Character{}, false, adoptCopyErr(err)
		}
		images = append(images, nid)
	}
	var voice int64
	if p.VoiceMediaID > 0 {
		if voice, err = l.copier.CopyToWorkspace(ctx, p.VoiceMediaID, a.WorkspaceID, a.UserID); err != nil {
			return repo.Character{}, false, adoptCopyErr(err)
		}
	}
	row := repo.Character{WorkspaceID: a.WorkspaceID, Name: p.Name, ImageMediaIDs: images,
		Appearance: p.Appearance, VoiceText: p.VoiceText, VoiceMediaID: voice, Source: repo.CharacterSourcePreset,
		SourcePresetID: p.ID, SourcePresetVersion: p.Version, CreatedBy: a.UserID}
	max := l.maxCharacters()
	for attempt := 0; attempt < 2; attempt++ {
		if attempt == 1 {
			row.Name = withAdoptSuffix(p.Name)
		}
		row.ID = l.nextID()
		c, err := l.characters.InsertCharacter(ctx, row, max)
		switch {
		case err == nil:
			return c, true, nil
		case errors.Is(err, repo.ErrPresetCopyExists):
			c, gerr := l.characters.GetCharacterByPreset(ctx, a.WorkspaceID, p.ID, p.Version)
			if gerr != nil {
				return repo.Character{}, false, httpx.Wrap(errcode.Internal, gerr)
			}
			return c, false, nil
		case errors.Is(err, repo.ErrQuota):
			return repo.Character{}, false, quotaErr("角色", max)
		case errors.Is(err, repo.ErrDuplicate):
			continue
		default:
			return repo.Character{}, false, httpx.Wrap(errcode.Internal, err)
		}
	}
	return repo.Character{}, false, duplicateNameErr(row.Name)
}

// withAdoptSuffix 给名称加后缀，并保证总长不超过 MaxCharacterName 码点（列宽 varchar(50)）。
func withAdoptSuffix(name string) string {
	keep := domain.MaxCharacterName - domain.RuneLen(adoptNameSuffix)
	if r := []rune(name); len(r) > keep {
		name = string(r[:keep])
	}
	return name + adoptNameSuffix
}

// adoptCopyErr：源媒体已被清掉时说成「暂时不能添加」，不把媒体域的 404 原样漏给用户
// （「源媒体已不存在」会让他以为自己哪里删错了）。
func adoptCopyErr(err error) error {
	var be *httpx.BizError
	if errors.As(err, &be) && be.Code == errcode.NotFound {
		return httpx.Errf(errcode.Conflict, "该营销角色的素材暂不可用，暂时不能添加")
	}
	return err
}
