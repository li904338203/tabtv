package repo

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
)

// 管理端技能配置的读写（AGT-60）。
//
// ⚠️ 写一律带 version 乐观锁（where id = $1 and version = $2）：两个运营同时编辑同一个技能时，
// 后保存的那个收到冲突而不是静默覆盖前者。附属文档的增删改也**推进技能的 version**，
// 同一事务里先 CAS 技能行 —— 附属文档是技能内容的一部分（read_skill_ref 读它），
// 单独无锁写的话「我改的主说明」和「他改的附属文档」会拼出一个谁都没审过的组合。
// ⚠️ 不提供删除技能：历史会话的 skill_codes 要能追溯（只停用）。

// ErrStale 是 version 不匹配（乐观锁冲突）。
var ErrStale = errors.New("版本已变化")

// AdminSkill 是管理端列表的一行：技能 + 附属文档数。
type AdminSkill struct {
	Skill
	RefCount int
}

// ListAllSkills 列出全部技能（含停用的；管理端看「配了什么」）。
func (r *Repo) ListAllSkills(ctx context.Context) ([]AdminSkill, error) {
	rows, err := r.pool.Query(ctx, `select `+skillCols+`,
		(select count(*) from agent_skill_reference f where f.skill_id = agent_skill.id)
		from agent_skill order by sort, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AdminSkill
	for rows.Next() {
		var a AdminSkill
		s := &a.Skill
		if err := rows.Scan(&s.ID, &s.Code, &s.Name, &s.Category, &s.Description, &s.Tags,
			&s.CoverMediaID, &s.Body, &s.Steps, &s.Tools, &s.ModelBindings, &s.IsQuickEntry, &s.Sort,
			&s.IsEnabled, &s.Version, &s.Scenes, &s.InputSlots, &s.PrefillText, &s.IsBatch, &s.IsBuiltin,
			&s.IsUserModified, &s.CreatedAt, &s.UpdatedAt, &a.RefCount); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// GetSkillByID 按 ID 取技能（含停用的）。
func (r *Repo) GetSkillByID(ctx context.Context, id int64) (Skill, error) {
	return scanSkill(r.pool.QueryRow(ctx, `select `+skillCols+` from agent_skill where id = $1`, id))
}

// ListSkillReferences 列出技能的附属文档（sort、id 升序）。
func (r *Repo) ListSkillReferences(ctx context.Context, skillID int64) ([]SkillReference, error) {
	rows, err := r.pool.Query(ctx, `
		select id, skill_id, name, body, sort, created_at, updated_at from agent_skill_reference
		where skill_id = $1 order by sort, id`, skillID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SkillReference
	for rows.Next() {
		var f SkillReference
		if err := rows.Scan(&f.ID, &f.SkillID, &f.Name, &f.Body, &f.Sort, &f.CreatedAt, &f.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// InsertSkill 新建技能（version 从 1 起）。code 重复返回 ErrDuplicate。
func (r *Repo) InsertSkill(ctx context.Context, s Skill) (Skill, error) {
	out, err := scanSkill(r.pool.QueryRow(ctx, `
		insert into agent_skill (id, code, name, category, description, tags, cover_media_id, body,
			steps, tools, model_bindings, is_quick_entry, sort, is_enabled, scenes, input_slots, prefill_text,
			is_batch)
		values ($1,$2,$3,$4,$5,$6,nullif($7::bigint,0),$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18)
		returning `+skillCols,
		s.ID, s.Code, s.Name, s.Category, s.Description, jsonOr(s.Tags, "[]"), s.CoverMediaID, s.Body,
		jsonOr(s.Steps, "[]"), jsonOr(s.Tools, "[]"), jsonOr(s.ModelBindings, "{}"), s.IsQuickEntry,
		s.Sort, s.IsEnabled, jsonOr(s.Scenes, "[]"), jsonOr(s.InputSlots, "[]"), s.PrefillText, s.IsBatch))
	return out, mapDuplicate(err)
}

// UpdateSkill 按 version CAS 改技能内容（code 与启用状态不在这里改），version +1。
// 不存在返回 ErrNotFound，版本不匹配返回 ErrStale。
func (r *Repo) UpdateSkill(ctx context.Context, s Skill, expectVersion int) (Skill, error) {
	out, err := scanSkill(r.pool.QueryRow(ctx, `
		update agent_skill set name = $3, category = $4, description = $5, tags = $6,
			cover_media_id = nullif($7::bigint,0), body = $8, steps = $9, tools = $10,
			model_bindings = $11, is_quick_entry = $12, sort = $13,
			scenes = $14, input_slots = $15, prefill_text = $16, is_batch = $17,
			is_user_modified = true, version = version + 1, updated_at = now()
		where id = $1 and version = $2
		returning `+skillCols,
		s.ID, expectVersion, s.Name, s.Category, s.Description, jsonOr(s.Tags, "[]"), s.CoverMediaID,
		s.Body, jsonOr(s.Steps, "[]"), jsonOr(s.Tools, "[]"), jsonOr(s.ModelBindings, "{}"),
		s.IsQuickEntry, s.Sort, jsonOr(s.Scenes, "[]"), jsonOr(s.InputSlots, "[]"), s.PrefillText,
		s.IsBatch))
	if errors.Is(err, ErrNotFound) {
		return Skill{}, r.staleOrMissing(ctx, r.pool, s.ID)
	}
	return out, err
}

// SetSkillEnabled 启用 / 停用，version +1。已是目标状态时不改，返回 changed=false 与当前行。
func (r *Repo) SetSkillEnabled(ctx context.Context, id int64, enabled bool) (Skill, bool, error) {
	out, err := scanSkill(r.pool.QueryRow(ctx, `
		update agent_skill set is_enabled = $2, version = version + 1, updated_at = now()
		where id = $1 and is_enabled <> $2
		returning `+skillCols, id, enabled))
	if errors.Is(err, ErrNotFound) {
		cur, err := r.GetSkillByID(ctx, id)
		return cur, false, err
	}
	return out, err == nil, err
}

// SaveSkillReference 新建（ref.ID 为新 ID 且 isNew）或修改附属文档，同一事务推进技能 version。
// 返回技能的新 version。名称重复 ErrDuplicate；技能 / 文档不存在 ErrNotFound；版本不匹配 ErrStale。
func (r *Repo) SaveSkillReference(ctx context.Context, ref SkillReference, isNew bool,
	expectSkillVersion int) (int, error) {
	return r.withSkillBump(ctx, ref.SkillID, expectSkillVersion, func(tx pgx.Tx) error {
		if isNew {
			_, err := tx.Exec(ctx, `
				insert into agent_skill_reference (id, skill_id, name, body, sort)
				values ($1, $2, $3, $4, $5)`, ref.ID, ref.SkillID, ref.Name, ref.Body, ref.Sort)
			return mapDuplicate(err)
		}
		tag, err := tx.Exec(ctx, `
			update agent_skill_reference set name = $3, body = $4, sort = $5, updated_at = now()
			where id = $1 and skill_id = $2`, ref.ID, ref.SkillID, ref.Name, ref.Body, ref.Sort)
		if err != nil {
			return mapDuplicate(err)
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// DeleteSkillReference 删除附属文档，同一事务推进技能 version。
func (r *Repo) DeleteSkillReference(ctx context.Context, skillID, refID int64, expectSkillVersion int) (int, error) {
	return r.withSkillBump(ctx, skillID, expectSkillVersion, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `delete from agent_skill_reference where id = $1 and skill_id = $2`, refID, skillID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// withSkillBump 在一个事务里：先 CAS 推进技能 version，再执行 fn；fn 失败整笔回滚。
func (r *Repo) withSkillBump(ctx context.Context, skillID int64, expectVersion int,
	fn func(tx pgx.Tx) error) (int, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var v int
	err = tx.QueryRow(ctx, `
		update agent_skill set is_user_modified = true, version = version + 1, updated_at = now()
		where id = $1 and version = $2 returning version`, skillID, expectVersion).Scan(&v)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, r.staleOrMissing(ctx, tx, skillID)
	}
	if err != nil {
		return 0, err
	}
	if err := fn(tx); err != nil {
		return 0, err
	}
	return v, tx.Commit(ctx)
}

// staleOrMissing 区分「技能不存在」与「版本不匹配」。
func (r *Repo) staleOrMissing(ctx context.Context, q Querier, id int64) error {
	var exists bool
	if err := q.QueryRow(ctx, `select exists(select 1 from agent_skill where id = $1)`, id).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return ErrStale
	}
	return ErrNotFound
}

// SkillRestore 是「恢复内置版本」要写回的内容（来自随包 seed，TT-T-19）。
type SkillRestore struct {
	Name, Category, Description string
	Tags, Steps, Tools          json.RawMessage
	Body                        string
	QuickEntry                  bool
	Sort                        int
	Hash                        string
	// Refs 的 ID 只在新建附属文档时使用。
	Refs []SkillReference
}

// RestoreSkill 用内置内容覆盖技能：一个事务里改技能行（清 is_user_modified、写 builtin_hash、version +1）
// 并把附属文档同步成内置那一套 —— 用户后加的附属文档一并删掉，否则「恢复」之后的技能仍是一个没人审过的组合。
// 只覆盖 seed 管的字段：封面、模型绑定、场景、槽位、预填文本不在内置内容里，保持原样。
// 技能不存在返回 ErrNotFound。
func (r *Repo) RestoreSkill(ctx context.Context, id int64, c SkillRestore) (Skill, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return Skill{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	out, err := scanSkill(tx.QueryRow(ctx, `
		update agent_skill set name = $2, category = $3, description = $4, tags = $5, body = $6,
			steps = $7, tools = $8, is_quick_entry = $9, sort = $10, builtin_hash = $11,
			is_user_modified = false, version = version + 1, updated_at = now()
		where id = $1
		returning `+skillCols,
		id, c.Name, c.Category, c.Description, jsonOr(c.Tags, "[]"), c.Body, jsonOr(c.Steps, "[]"),
		jsonOr(c.Tools, "[]"), c.QuickEntry, c.Sort, c.Hash))
	if err != nil {
		return Skill{}, err
	}
	names := make([]string, 0, len(c.Refs))
	for _, f := range c.Refs {
		names = append(names, f.Name)
		if _, err := tx.Exec(ctx, `
			insert into agent_skill_reference (id, skill_id, name, body, sort)
			values ($1, $2, $3, $4, $5)
			on conflict (skill_id, name) do update set body = excluded.body, sort = excluded.sort, updated_at = now()`,
			f.ID, id, f.Name, f.Body, f.Sort); err != nil {
			return Skill{}, err
		}
	}
	if _, err := tx.Exec(ctx, `delete from agent_skill_reference where skill_id = $1 and not (name = any($2))`,
		id, names); err != nil {
		return Skill{}, err
	}
	return out, tx.Commit(ctx)
}
