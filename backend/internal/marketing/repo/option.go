package repo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/li904338203/tabtv/backend/internal/marketing/domain"
)

// Option 是 marketing_option 的一行：领域字段 + 行元数据。
type Option struct {
	ID int64
	domain.Option
	CreatedByAdminID int64
	UpdatedByAdminID int64
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

const optionCols = `id, kind, code, name, category_code, description, cover_media_ids,
	coalesce(preview_media_id, 0), prompt, output_types, input_requirements, is_audio_required,
	recommended_models, recommended_params, coalesce(default_skill_code, ''), tags, start_at, end_at,
	sort, is_enabled, version, coalesce(created_by_admin_id, 0), coalesce(updated_by_admin_id, 0),
	created_at, updated_at`

func scanOption(row pgx.Row) (Option, error) {
	var (
		o                                Option
		kind                             string
		covers, outs, reqs, models, tags []byte
		params                           []byte
		preview                          int64
	)
	err := row.Scan(&o.ID, &kind, &o.Code, &o.Name, &o.CategoryCode, &o.Description, &covers,
		&preview, &o.Prompt, &outs, &reqs, &o.IsAudioRequired, &models, &params, &o.DefaultSkillCode,
		&tags, &o.StartAt, &o.EndAt, &o.Sort, &o.IsEnabled, &o.Version, &o.CreatedByAdminID,
		&o.UpdatedByAdminID, &o.CreatedAt, &o.UpdatedAt)
	if err != nil {
		return Option{}, mapNoRows(err)
	}
	o.Kind = domain.Kind(kind)
	if preview > 0 {
		p := preview
		o.PreviewMediaID = &p
	}
	o.RecommendedParams = json.RawMessage(params)
	for _, x := range []struct {
		raw []byte
		dst any
	}{{covers, &o.CoverMediaIDs}, {outs, &o.OutputTypes}, {reqs, &o.InputRequirements},
		{models, &o.RecommendedModels}, {tags, &o.Tags}} {
		if err := json.Unmarshal(x.raw, x.dst); err != nil {
			return Option{}, fmt.Errorf("解析选项 %d 的 jsonb 列失败：%w", o.ID, err)
		}
	}
	return o, nil
}

// optionArgs 把内容字段编码成 SQL 参数（顺序与 insert / update 的 $n 对应）。
func optionArgs(o domain.Option) ([]any, error) {
	enc := func(v any, def string) (json.RawMessage, error) {
		b, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}
		return jsonOr(b, def), nil
	}
	covers, err := enc(o.CoverMediaIDs, "[]")
	if err != nil {
		return nil, err
	}
	outs, err := enc(o.OutputTypes, `["video"]`)
	if err != nil {
		return nil, err
	}
	reqs, err := enc(o.InputRequirements, "{}")
	if err != nil {
		return nil, err
	}
	models, err := enc(o.RecommendedModels, "[]")
	if err != nil {
		return nil, err
	}
	tags, err := enc(o.Tags, "[]")
	if err != nil {
		return nil, err
	}
	var preview int64
	if o.PreviewMediaID != nil {
		preview = *o.PreviewMediaID
	}
	return []any{o.Name, o.CategoryCode, o.Description, covers, preview, o.Prompt, outs, reqs,
		o.IsAudioRequired, models, jsonOr(o.RecommendedParams, "{}"), o.DefaultSkillCode, tags,
		o.StartAt, o.EndAt, o.Sort}, nil
}

// InsertOption 新建选项（version=1）并在**同一事务**写第 1 版历史。code 重复返回 ErrDuplicate。
func (r *Repo) InsertOption(ctx context.Context, id, versionID, adminID int64, o domain.Option,
	snapshot json.RawMessage) (Option, error) {
	args, err := optionArgs(o)
	if err != nil {
		return Option{}, err
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return Option{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	all := append([]any{id, string(o.Kind), o.Code, o.IsEnabled, adminID}, args...)
	out, err := scanOption(tx.QueryRow(ctx, `
		insert into marketing_option (id, kind, code, is_enabled, created_by_admin_id, updated_by_admin_id,
			name, category_code, description, cover_media_ids, preview_media_id, prompt, output_types,
			input_requirements, is_audio_required, recommended_models, recommended_params,
			default_skill_code, tags, start_at, end_at, sort, version)
		values ($1, $2, $3, $4, nullif($5::bigint, 0), nullif($5::bigint, 0),
			$6, $7, $8, $9, nullif($10::bigint, 0), $11, $12, $13, $14, $15, $16,
			nullif($17, ''), $18, $19, $20, $21, 1)
		returning `+optionCols, all...))
	if err != nil {
		return Option{}, mapDuplicate(err)
	}
	if err := insertVersion(ctx, tx, versionID, out.ID, out.Version, adminID, snapshot); err != nil {
		return Option{}, err
	}
	return out, tx.Commit(ctx)
}

// UpdateOption 按 version CAS 整份更新内容（code / kind / 启用状态不在这里改），version+1，
// 并在**同一事务**插入新版本的历史行。不存在 ErrNotFound，版本不匹配 ErrStale。
//
// snapshot 由调用方按**更新后**的内容生成（它的 version 字段必须是 expectVersion+1）。
func (r *Repo) UpdateOption(ctx context.Context, id int64, expectVersion int, versionID, adminID int64,
	o domain.Option, snapshot json.RawMessage) (Option, error) {
	args, err := optionArgs(o)
	if err != nil {
		return Option{}, err
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return Option{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	all := append([]any{id, expectVersion, adminID}, args...)
	out, err := scanOption(tx.QueryRow(ctx, `
		update marketing_option set name = $4, category_code = $5, description = $6,
			cover_media_ids = $7, preview_media_id = nullif($8::bigint, 0), prompt = $9,
			output_types = $10, input_requirements = $11, is_audio_required = $12,
			recommended_models = $13, recommended_params = $14, default_skill_code = nullif($15, ''),
			tags = $16, start_at = $17, end_at = $18, sort = $19,
			updated_by_admin_id = nullif($3::bigint, 0), version = version + 1, updated_at = now()
		where id = $1 and version = $2
		returning `+optionCols, all...))
	if errors.Is(err, ErrNotFound) {
		return Option{}, staleOrMissing(ctx, tx, "marketing_option", id)
	}
	if err != nil {
		return Option{}, err
	}
	if err := insertVersion(ctx, tx, versionID, out.ID, out.Version, adminID, snapshot); err != nil {
		return Option{}, err
	}
	return out, tx.Commit(ctx)
}

func insertVersion(ctx context.Context, q Querier, id, optionID int64, version int, adminID int64,
	snapshot json.RawMessage) error {
	_, err := q.Exec(ctx, `
		insert into marketing_option_version (id, option_id, version, snapshot, created_by_admin_id)
		values ($1, $2, $3, $4, nullif($5::bigint, 0))`, id, optionID, version, snapshot, adminID)
	return err
}

// staleOrMissing 区分「行不存在」与「版本不匹配」。table 只由本包传入常量。
func staleOrMissing(ctx context.Context, q Querier, table string, id int64) error {
	var exists bool
	if err := q.QueryRow(ctx, `select exists(select 1 from `+table+` where id = $1)`, id).
		Scan(&exists); err != nil {
		return err
	}
	if exists {
		return ErrStale
	}
	return ErrNotFound
}

// GetOption 按 ID 取（含停用的）。
func (r *Repo) GetOption(ctx context.Context, id int64) (Option, error) {
	return scanOption(r.pool.QueryRow(ctx, `select `+optionCols+` from marketing_option where id = $1`, id))
}

// GetOptionByCode 按 code 取（含停用的；可见性由调用方按 domain.Visible 判）。
func (r *Repo) GetOptionByCode(ctx context.Context, code string) (Option, error) {
	return scanOption(r.pool.QueryRow(ctx, `select `+optionCols+` from marketing_option where code = $1`, code))
}

// SetOptionEnabled 启用 / 停用。已是目标状态时不改，返回 changed=false 与当前行。
//
// ⚠️ **不推进 version**：version 是「内容版本」，会话按 (code, version) 钉的是内容；
// 启停推进它的话，要么历史里多出一个内容没变的版本，要么出现「当前版本在历史里不存在」。
// 也正因为如此，编辑表单的乐观锁不会被一次启停打断。
func (r *Repo) SetOptionEnabled(ctx context.Context, id int64, enabled bool, adminID int64) (Option, bool, error) {
	out, err := scanOption(r.pool.QueryRow(ctx, `
		update marketing_option set is_enabled = $2, updated_by_admin_id = nullif($3::bigint, 0),
			updated_at = now()
		where id = $1 and is_enabled <> $2
		returning `+optionCols, id, enabled, adminID))
	if errors.Is(err, ErrNotFound) {
		cur, err := r.GetOption(ctx, id)
		return cur, false, err
	}
	return out, err == nil, err
}

// OptionFilter 是管理端列表的筛选。
//
// ⚠️ 管理端**不按时段过滤**（看「配了什么」）；Lifecycle 只是一个可选的筛选条件，
// 与用户端「只返回在投」是两套口径 —— 别共用一个查询加布尔参数。
type OptionFilter struct {
	Kind      domain.Kind
	Category  string
	Enabled   *bool
	Lifecycle domain.LifecycleState // 空 = 不筛
	Q         string
	Now       time.Time
	// 游标：上一页最后一行的 (sort, id)。CursorID=0 表示第一页。
	CursorSort int
	CursorID   int64
	Limit      int
}

// ListOptions 管理端列表，按 (sort, id) 升序、游标分页。多取一行用于判断是否还有下一页。
func (r *Repo) ListOptions(ctx context.Context, f OptionFilter) ([]Option, bool, error) {
	var cond []string
	var args []any
	add := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}
	if f.Kind != "" {
		cond = append(cond, "kind = "+add(string(f.Kind)))
	}
	if f.Category != "" {
		cond = append(cond, "category_code = "+add(f.Category))
	}
	if f.Enabled != nil {
		cond = append(cond, "is_enabled = "+add(*f.Enabled))
	}
	if f.Lifecycle != "" {
		// ⚠️ $now 只在用得到的分支里登记：登记了却不引用，PG 推断不出参数类型（42P18）。
		now := func() string { return add(f.Now) }
		// 与 domain.Lifecycle 同义：停用恒为 disabled；未开始优先于已结束（CHECK 保证两者不重叠）。
		switch f.Lifecycle {
		case domain.LifecycleDisabled:
			cond = append(cond, "not is_enabled")
		case domain.LifecycleNotStarted:
			cond = append(cond, "is_enabled and start_at is not null and start_at > "+now())
		case domain.LifecycleEnded:
			cond = append(cond, "is_enabled and end_at is not null and end_at <= "+now()+
				" and (start_at is null or start_at <= "+now()+")")
		case domain.LifecycleLive:
			cond = append(cond, "is_enabled and (start_at is null or start_at <= "+now()+
				") and (end_at is null or end_at > "+now()+")")
		default:
			return nil, false, fmt.Errorf("未知的 lifecycle %q", f.Lifecycle)
		}
	}
	if q := strings.TrimSpace(f.Q); q != "" {
		p := add("%" + escapeLike(q) + "%")
		cond = append(cond, "(name ilike "+p+" escape '\\' or code ilike "+p+" escape '\\')")
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
	rows, err := r.pool.Query(ctx, `select `+optionCols+` from marketing_option`+where+
		` order by sort, id limit `+add(limit+1), args...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var out []Option
	for rows.Next() {
		o, err := scanOption(rows)
		if err != nil {
			return nil, false, err
		}
		out = append(out, o)
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

// SortOptions 按 ids 的顺序把同一 kind + 分类下的选项排成 1..N（不推进 version）。
// ids 里有任何一个不存在或不属于该 kind + 分类时整笔回滚并返回 ErrNotFound。
func (r *Repo) SortOptions(ctx context.Context, kind domain.Kind, category string, ids []int64) error {
	return r.sortRows(ctx, `
		update marketing_option o set sort = t.ord, updated_at = now()
		from unnest($1::bigint[]) with ordinality as t(id, ord)
		where o.id = t.id and o.kind = $2 and o.category_code = $3`, ids, string(kind), category)
}

func (r *Repo) sortRows(ctx context.Context, sql string, ids []int64, args ...any) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, err := tx.Exec(ctx, sql, append([]any{ids}, args...)...)
	if err != nil {
		return err
	}
	if int(tag.RowsAffected()) != len(ids) {
		return ErrNotFound
	}
	return tx.Commit(ctx)
}
