package repo

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/li904338203/tabtv/backend/internal/marketing/domain"
)

// 营销角色（平台预置）的**管理端**读写（MKT-T-36；dev/05 §3.2、MKD-30）。
//
// ⚠️ 管理端看「配了什么」：列表含停用的；用户端「只返回启用」是另一套查询（MKT-T-34 的 preset.go），
// 别共用一个查询加布尔参数。
// ⚠️ version 只随**内容保存**推进：启停与排序不推进（同营销选项，dev/02 R-2 #3）——
// 用户「添加」按 (预置, 版本) 去重复制，启停推进版本的话同一份内容会被复制两次。
// ⚠️ 没有删除：用户已添加的副本按 source_preset_id 追溯，删行就断了。
// 本文件的名字都带 Admin / Preset 前缀，与用户侧的只读查询（preset.go）分开，避免并行开发撞名。

// AdminPreset 是管理端看到的一行营销角色。
type AdminPreset struct {
	ID int64
	domain.PresetInput
	Sort             int
	IsEnabled        bool
	Version          int
	CreatedByAdminID int64
	UpdatedByAdminID int64
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// PresetAdminFilter 是管理端列表筛选。
type PresetAdminFilter struct {
	Enabled *bool
	Q       string
	// 游标：上一页最后一行的 (sort, id)；CursorID=0 = 第一页。
	CursorSort int
	CursorID   int64
	Limit      int
}

const adminPresetCols = `id, name, summary, image_media_ids, appearance, voice_text, coalesce(voice_media_id, 0),
	sort, is_enabled, version, coalesce(created_by_admin_id, 0), coalesce(updated_by_admin_id, 0),
	created_at, updated_at`

func scanAdminPreset(row pgx.Row) (AdminPreset, error) {
	var (
		p     AdminPreset
		raw   []byte
		voice int64
	)
	if err := row.Scan(&p.ID, &p.Name, &p.Summary, &raw, &p.Appearance, &p.VoiceText, &voice, &p.Sort,
		&p.IsEnabled, &p.Version, &p.CreatedByAdminID, &p.UpdatedByAdminID, &p.CreatedAt, &p.UpdatedAt); err != nil {
		return AdminPreset{}, mapNoRows(err)
	}
	ids, err := idsFromJSON(raw)
	if err != nil {
		return AdminPreset{}, err
	}
	p.ImageMediaIDs = ids
	if voice > 0 {
		p.VoiceMediaID = &voice
	}
	return p, nil
}

func voiceArg(v *int64) int64 {
	if v == nil {
		return 0
	}
	return *v
}

// InsertPreset 新建（一律停用、version=1），排在最后（sort = 当前最大 + 1）。名称重复 → ErrDuplicate。
func (r *Repo) InsertPreset(ctx context.Context, id, adminID int64, p domain.PresetInput) (AdminPreset, error) {
	imgs, err := idsToJSON(p.ImageMediaIDs)
	if err != nil {
		return AdminPreset{}, err
	}
	out, err := scanAdminPreset(r.pool.QueryRow(ctx, `
		insert into marketing_preset_character (id, name, summary, image_media_ids, appearance, voice_text,
			voice_media_id, sort, is_enabled, version, created_by_admin_id, updated_by_admin_id)
		values ($1, $2, $3, $4::jsonb, $5, $6, nullif($7::bigint, 0),
			(select coalesce(max(sort), 0) + 1 from marketing_preset_character), false, 1,
			nullif($8::bigint, 0), nullif($8::bigint, 0))
		returning `+adminPresetCols,
		id, p.Name, p.Summary, string(imgs), p.Appearance, p.VoiceText, voiceArg(p.VoiceMediaID), adminID))
	if err != nil {
		return AdminPreset{}, mapDuplicate(err)
	}
	return out, nil
}

// AdminGetPreset 按 ID 取（含停用的）。
func (r *Repo) AdminGetPreset(ctx context.Context, id int64) (AdminPreset, error) {
	return scanAdminPreset(r.pool.QueryRow(ctx,
		`select `+adminPresetCols+` from marketing_preset_character where id = $1`, id))
}

// UpdatePreset 整份更新（乐观锁，version+1）。版本不匹配 → ErrStale；不存在 → ErrNotFound；重名 → ErrDuplicate。
func (r *Repo) UpdatePreset(ctx context.Context, id int64, expect int, adminID int64,
	p domain.PresetInput) (AdminPreset, error) {
	imgs, err := idsToJSON(p.ImageMediaIDs)
	if err != nil {
		return AdminPreset{}, err
	}
	out, err := scanAdminPreset(r.pool.QueryRow(ctx, `
		update marketing_preset_character
		   set name = $3, summary = $4, image_media_ids = $5::jsonb, appearance = $6, voice_text = $7,
		       voice_media_id = nullif($8::bigint, 0), updated_by_admin_id = nullif($9::bigint, 0),
		       version = version + 1, updated_at = now()
		 where id = $1 and version = $2
		returning `+adminPresetCols,
		id, expect, p.Name, p.Summary, string(imgs), p.Appearance, p.VoiceText, voiceArg(p.VoiceMediaID), adminID))
	if errors.Is(err, ErrNotFound) {
		return AdminPreset{}, staleOrMissing(ctx, r.pool, "marketing_preset_character", id)
	}
	if err != nil {
		return AdminPreset{}, mapDuplicate(err)
	}
	return out, nil
}

// SetPresetEnabled 启用 / 停用。已是目标状态时不改，返回 changed=false 与当前行。**不推进 version**。
func (r *Repo) SetPresetEnabled(ctx context.Context, id int64, enabled bool, adminID int64) (AdminPreset, bool, error) {
	out, err := scanAdminPreset(r.pool.QueryRow(ctx, `
		update marketing_preset_character set is_enabled = $2, updated_by_admin_id = nullif($3::bigint, 0),
			updated_at = now()
		where id = $1 and is_enabled <> $2
		returning `+adminPresetCols, id, enabled, adminID))
	if errors.Is(err, ErrNotFound) {
		cur, err := r.AdminGetPreset(ctx, id)
		return cur, false, err
	}
	return out, err == nil, err
}

// AdminListPresets 管理端列表（含停用），按 (sort, id) 升序、游标分页；多取一行判断是否还有下一页。
func (r *Repo) AdminListPresets(ctx context.Context, f PresetAdminFilter) ([]AdminPreset, bool, error) {
	var cond []string
	var args []any
	add := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}
	if f.Enabled != nil {
		cond = append(cond, "is_enabled = "+add(*f.Enabled))
	}
	if q := strings.TrimSpace(f.Q); q != "" {
		cond = append(cond, "name ilike "+add("%"+escapeLike(q)+"%")+" escape '\\'")
	}
	if f.CursorID > 0 {
		cond = append(cond, "(sort, id) > ("+add(f.CursorSort)+", "+add(f.CursorID)+")")
	}
	limit := f.Limit
	if limit <= 0 {
		limit = 50
	}
	where := ""
	if len(cond) > 0 {
		where = " where " + strings.Join(cond, " and ")
	}
	rows, err := r.pool.Query(ctx, `select `+adminPresetCols+` from marketing_preset_character`+where+
		` order by sort, id limit `+add(limit+1), args...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var out []AdminPreset
	for rows.Next() {
		p, err := scanAdminPreset(rows)
		if err != nil {
			return nil, false, err
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	more := len(out) > limit
	if more {
		out = out[:limit]
	}
	return out, more, nil
}

// SortPresets 按 ids 顺序排成 1..N（不推进 version）。任一 ID 不存在整笔回滚并返回 ErrNotFound。
func (r *Repo) SortPresets(ctx context.Context, ids []int64) error {
	return r.sortRows(ctx, `
		update marketing_preset_character p set sort = t.ord, updated_at = now()
		from unnest($1::bigint[]) with ordinality as t(id, ord)
		where p.id = t.id`, ids)
}
