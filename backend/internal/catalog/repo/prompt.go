package repo

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/li904338203/tabtv/backend/internal/catalog/domain"
)

type PromptTemplate struct {
	ID           int64
	Code         string
	ModelCode    string
	Content      string
	DraftContent string
	Status       domain.PromptStatus
	Version      int
	Note         string
}

func (r *Repo) ListPrompts(ctx context.Context) ([]PromptTemplate, error) {
	rows, err := r.pool.Query(ctx, `
		select id, code, coalesce(model_code,''), content,
		       coalesce(draft_content,''), status, version, coalesce(note,'')
		from prompt_template
		order by code, coalesce(model_code,'')`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PromptTemplate
	for rows.Next() {
		var p PromptTemplate
		if err := rows.Scan(&p.ID, &p.Code, &p.ModelCode, &p.Content,
			&p.DraftContent, &p.Status, &p.Version, &p.Note); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (r *Repo) GetPrompt(ctx context.Context, code, modelCode string) (PromptTemplate, error) {
	var p PromptTemplate
	err := r.pool.QueryRow(ctx, `
		select id, code, coalesce(model_code,''), content,
		       coalesce(draft_content,''), status, version, coalesce(note,'')
		from prompt_template
		where code = $1 and coalesce(model_code,'') = $2`, code, modelCode).
		Scan(&p.ID, &p.Code, &p.ModelCode, &p.Content, &p.DraftContent,
			&p.Status, &p.Version, &p.Note)
	if errors.Is(err, pgx.ErrNoRows) {
		return PromptTemplate{}, ErrNotFound
	}
	return p, err
}

// ResolvePrompt 按优先级取生效内容：**模型定制 > 通用**（07 §6）。
//
// ⚠️ 只取 published 的行。草稿不参与运行时解析 ——
// 否则编辑到一半的内容会直接影响线上生成。
func (r *Repo) ResolvePrompt(ctx context.Context, code, modelCode string) (string, bool, error) {
	var content string
	err := r.pool.QueryRow(ctx, `
		select content from prompt_template
		where code = $1 and status = 'published'
		  and (model_code = $2 or model_code is null)
		-- 模型定制排前面：null 排后。
		order by (model_code is null)
		limit 1`, code, modelCode).Scan(&content)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return content, true, nil
}

// SaveDraft 写草稿。**不影响线上内容**。
func (r *Repo) SaveDraft(ctx context.Context, id int64, code, modelCode, draft, note string) error {
	var mc any
	if modelCode != "" {
		mc = modelCode
	}
	// version 显式写 0，不吃列默认值：默认值曾是 1，于是新行凭空带一个
	// 没有快照、内容为空、永远回滚不到的「版本 1」（见迁移 00015）。
	_, err := r.pool.Exec(ctx, `
		insert into prompt_template (id, code, model_code, content, draft_content, status, version, note)
		values ($1, $2, $3, '', $4, 'draft', 0, nullif($5,''))
		on conflict (code, model_code) do update
		set draft_content = excluded.draft_content,
		    note = excluded.note, updated_at = now()`,
		id, code, mc, draft, note)
	return err
}

// Publish 把草稿发布为生效内容，并写一条不可变版本快照。
//
// ⚠️ 快照是回滚的唯一依据。不写快照的"版本号+1"只是个计数器，
// 回滚时无从知道当初那版长什么样。
func (r *Repo) Publish(ctx context.Context, versionID, templateID, adminID int64,
	content, note string, rollbackOf *int) (int, error) {

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var nextVersion int
	err = tx.QueryRow(ctx, `
		update prompt_template
		set content = $2, status = 'published', version = version + 1,
		    published_at = now(), note = nullif($3,''), updated_at = now()
		where id = $1
		returning version`, templateID, content, note).Scan(&nextVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, err
	}

	if _, err := tx.Exec(ctx, `
		insert into prompt_template_version
		  (id, template_id, version, content, note, published_by_admin_id, rollback_of_version)
		values ($1,$2,$3,$4,nullif($5,''),nullif($6::bigint,0),$7)`,
		versionID, templateID, nextVersion, content, note, adminID, rollbackOf); err != nil {
		return 0, err
	}
	return nextVersion, tx.Commit(ctx)
}

type PromptVersion struct {
	Version           int
	Content           string
	Note              string
	PublishedBy       int64
	PublishedAt       string
	RollbackOfVersion *int
}

func (r *Repo) ListPromptVersions(ctx context.Context, templateID int64, limit int) ([]PromptVersion, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := r.pool.Query(ctx, `
		select version, content, coalesce(note,''), coalesce(published_by_admin_id,0),
		       to_char(published_at at time zone 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'),
		       rollback_of_version
		from prompt_template_version
		where template_id = $1 order by version desc limit $2`, templateID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PromptVersion
	for rows.Next() {
		var v PromptVersion
		if err := rows.Scan(&v.Version, &v.Content, &v.Note, &v.PublishedBy,
			&v.PublishedAt, &v.RollbackOfVersion); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (r *Repo) GetPromptVersion(ctx context.Context, templateID int64, version int) (PromptVersion, error) {
	var v PromptVersion
	err := r.pool.QueryRow(ctx, `
		select version, content, coalesce(note,''), coalesce(published_by_admin_id,0),
		       to_char(published_at at time zone 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'),
		       rollback_of_version
		from prompt_template_version
		where template_id = $1 and version = $2`, templateID, version).
		Scan(&v.Version, &v.Content, &v.Note, &v.PublishedBy, &v.PublishedAt, &v.RollbackOfVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		return PromptVersion{}, ErrNotFound
	}
	return v, err
}

// ArchivePrompt 下线模板。运行时随即回落到通用模板或代码内置兜底。
func (r *Repo) ArchivePrompt(ctx context.Context, templateID int64) error {
	tag, err := r.pool.Exec(ctx,
		`update prompt_template set status = 'archived', updated_at = now() where id = $1`, templateID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
