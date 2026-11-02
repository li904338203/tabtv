package repo

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// Skill 是 agent_skill 的一行。CoverMediaID 为 0 表示没有。
type Skill struct {
	ID            int64
	Code          string
	Name          string
	Category      string
	Description   string
	Tags          json.RawMessage
	CoverMediaID  int64
	Body          string
	Steps         json.RawMessage
	Tools         json.RawMessage // 工具白名单（只能收窄，见 domain.AllowedTools）
	ModelBindings json.RawMessage
	IsQuickEntry  bool
	Sort          int
	IsEnabled     bool
	Version       int
	// 场景扩展（MKT-T-05）：所属场景、输入槽声明、预填描述。
	Scenes      json.RawMessage // jsonb 数组；[] = 只在通用场景
	InputSlots  json.RawMessage // jsonb 数组；形状由营销域校验
	PrefillText string
	// IsBatch 一次出多个变体的技能（MKT-T-32，迁移 00062）：只用于展示「最多 N 个变体」，不是计费依据。
	IsBatch bool
	// IsBuiltin / IsUserModified：随包内置的技能与「用户改过」标志（迁移 00083）；设置页据此显示「内置 / 已修改」。
	IsBuiltin      bool
	IsUserModified bool
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// SceneList 解出 Scenes（坏值按空处理：一行脏数据不该让整个技能列表打不开）。
func (s Skill) SceneList() []string {
	var out []string
	_ = json.Unmarshal(s.Scenes, &out)
	return out
}

// SkillReference 是 agent_skill_reference 的一行（按需读取的附属文档）。
type SkillReference struct {
	ID        int64
	SkillID   int64
	Name      string
	Body      string
	Sort      int
	CreatedAt time.Time
	UpdatedAt time.Time
}

const skillCols = `id, code, name, category, description, tags, coalesce(cover_media_id, 0), body,
	steps, tools, model_bindings, is_quick_entry, sort, is_enabled, version, scenes, input_slots,
	prefill_text, is_batch, builtin_hash is not null, is_user_modified, created_at, updated_at`

func scanSkill(row pgx.Row) (Skill, error) {
	var s Skill
	err := row.Scan(&s.ID, &s.Code, &s.Name, &s.Category, &s.Description, &s.Tags, &s.CoverMediaID,
		&s.Body, &s.Steps, &s.Tools, &s.ModelBindings, &s.IsQuickEntry, &s.Sort, &s.IsEnabled,
		&s.Version, &s.Scenes, &s.InputSlots, &s.PrefillText, &s.IsBatch, &s.IsBuiltin, &s.IsUserModified, &s.CreatedAt, &s.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Skill{}, ErrNotFound
	}
	return s, err
}

// ListEnabledSkills 列出启用的技能（sort 升序）。
func (r *Repo) ListEnabledSkills(ctx context.Context) ([]Skill, error) {
	rows, err := r.pool.Query(ctx, `select `+skillCols+` from agent_skill
		where is_enabled order by sort, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Skill
	for rows.Next() {
		s, err := scanSkill(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// GetSkillByCode 按 code 取技能（含停用的，由调用方看 IsEnabled：已加载过的会话要能说清「已停用」）。
func (r *Repo) GetSkillByCode(ctx context.Context, code string) (Skill, error) {
	return scanSkill(r.pool.QueryRow(ctx, `select `+skillCols+` from agent_skill where code = $1`, code))
}

// SkillReferenceByName 取技能的一份附属文档。
func (r *Repo) SkillReferenceByName(ctx context.Context, skillID int64, name string) (SkillReference, error) {
	var ref SkillReference
	err := r.pool.QueryRow(ctx, `
		select id, skill_id, name, body, sort, created_at, updated_at from agent_skill_reference
		where skill_id = $1 and name = $2`, skillID, name).
		Scan(&ref.ID, &ref.SkillID, &ref.Name, &ref.Body, &ref.Sort, &ref.CreatedAt, &ref.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return SkillReference{}, ErrNotFound
	}
	return ref, err
}

// UpsertSkill 按 code 写入技能（seed 用；管理端写接口在 AGT-60）。
// 内容有变化时 version +1；完全相同时不动（seed 可重复跑）。返回最终的技能 ID。
func (r *Repo) UpsertSkill(ctx context.Context, s Skill) (int64, error) {
	var id int64
	err := r.pool.QueryRow(ctx, `
		insert into agent_skill (id, code, name, category, description, tags, cover_media_id, body,
			steps, tools, model_bindings, is_quick_entry, sort, is_enabled)
		values ($1,$2,$3,$4,$5,$6,nullif($7::bigint,0),$8,$9,$10,$11,$12,$13,$14)
		on conflict (code) do update set
			name = excluded.name, category = excluded.category, description = excluded.description,
			tags = excluded.tags, cover_media_id = excluded.cover_media_id, body = excluded.body,
			steps = excluded.steps, tools = excluded.tools, model_bindings = excluded.model_bindings,
			is_quick_entry = excluded.is_quick_entry, sort = excluded.sort, is_enabled = excluded.is_enabled,
			version = agent_skill.version + 1, updated_at = now()
		where (agent_skill.name, agent_skill.category, agent_skill.description, agent_skill.tags,
			agent_skill.cover_media_id, agent_skill.body, agent_skill.steps, agent_skill.tools,
			agent_skill.model_bindings, agent_skill.is_quick_entry, agent_skill.sort, agent_skill.is_enabled)
			is distinct from
			(excluded.name, excluded.category, excluded.description, excluded.tags,
			excluded.cover_media_id, excluded.body, excluded.steps, excluded.tools,
			excluded.model_bindings, excluded.is_quick_entry, excluded.sort, excluded.is_enabled)
		returning id`,
		s.ID, s.Code, s.Name, s.Category, s.Description, jsonOr(s.Tags, "[]"), s.CoverMediaID, s.Body,
		jsonOr(s.Steps, "[]"), jsonOr(s.Tools, "[]"), jsonOr(s.ModelBindings, "{}"), s.IsQuickEntry,
		s.Sort, s.IsEnabled).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		// 内容没变，do update 的 where 不成立 → 没有 returning 行；取已有的 ID。
		err = r.pool.QueryRow(ctx, `select id from agent_skill where code = $1`, s.Code).Scan(&id)
	}
	return id, err
}

// UpsertSkillReference 按（技能, 名称）写入附属文档（seed 用）。
func (r *Repo) UpsertSkillReference(ctx context.Context, ref SkillReference) error {
	_, err := r.pool.Exec(ctx, `
		insert into agent_skill_reference (id, skill_id, name, body, sort)
		values ($1, $2, $3, $4, $5)
		on conflict (skill_id, name) do update set body = excluded.body, sort = excluded.sort,
			updated_at = now()
		where (agent_skill_reference.body, agent_skill_reference.sort)
			is distinct from (excluded.body, excluded.sort)`,
		ref.ID, ref.SkillID, ref.Name, ref.Body, ref.Sort)
	return err
}
