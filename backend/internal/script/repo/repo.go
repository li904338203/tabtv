// Package repo 是脚本域的数据访问。
package repo

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrNotFound        = errors.New("脚本不存在")
	ErrDuplicate       = errors.New("该画布节点已经有脚本了")
	ErrVersionConflict = errors.New("脚本已被他人修改")
)

type Repo struct{ pool *pgxpool.Pool }

func New(pool *pgxpool.Pool) *Repo { return &Repo{pool: pool} }

func (r *Repo) Pool() *pgxpool.Pool { return r.pool }

// Script 是脚本实例。
type Script struct {
	ID            int64
	WorkspaceID   int64
	ProjectID     int64
	CanvasNodeID  string
	SourceType    string
	CurrentStep   int
	Status        string
	RawInput      string
	GlobalStyle   string
	AssetProfiles json.RawMessage
	Version       int
}

const scriptCols = `select id, workspace_id, project_id, canvas_node_id, source_type,
	current_step, status, coalesce(raw_input,''), coalesce(global_style,''),
	asset_profiles, version from script_instance `

func scanScript(row pgx.Row) (Script, error) {
	var s Script
	err := row.Scan(&s.ID, &s.WorkspaceID, &s.ProjectID, &s.CanvasNodeID, &s.SourceType,
		&s.CurrentStep, &s.Status, &s.RawInput, &s.GlobalStyle, &s.AssetProfiles, &s.Version)
	if errors.Is(err, pgx.ErrNoRows) {
		return Script{}, ErrNotFound
	}
	return s, err
}

// Insert 建脚本。
//
// ⚠️ `uk_script_canvas_node` 保证脚本与画布节点 **1:1**。撞唯一索引时
// 返回 ErrDuplicate 而不是内部错误 —— 调用方据此改为「打开已有脚本」，
// 这正是「按节点恢复已有脚本」（AV-05）的实现方式。
func (r *Repo) Insert(ctx context.Context, s Script) error {
	profiles := s.AssetProfiles
	if len(profiles) == 0 {
		profiles = json.RawMessage(`{}`)
	}
	_, err := r.pool.Exec(ctx, `
		insert into script_instance (id, workspace_id, project_id, canvas_node_id,
			source_type, current_step, status, raw_input, global_style, asset_profiles)
		values ($1,$2,$3,$4,$5,$6,$7,nullif($8,''),nullif($9,''),$10)`,
		s.ID, s.WorkspaceID, s.ProjectID, s.CanvasNodeID, s.SourceType,
		s.CurrentStep, s.Status, s.RawInput, s.GlobalStyle, profiles)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return ErrDuplicate
	}
	return err
}

func (r *Repo) Get(ctx context.Context, workspaceID, id int64) (Script, error) {
	return scanScript(r.pool.QueryRow(ctx, scriptCols+
		`where id = $1 and workspace_id = $2`, id, workspaceID))
}

// GetByNode 按画布节点取脚本（打开节点时恢复）。
func (r *Repo) GetByNode(ctx context.Context, workspaceID, projectID int64, nodeID string) (
	Script, error) {
	return scanScript(r.pool.QueryRow(ctx, scriptCols+
		`where workspace_id = $1 and project_id = $2 and canvas_node_id = $3`,
		workspaceID, projectID, nodeID))
}

// PatchInput 是一次脚本元信息更新。
type PatchInput struct {
	ID              int64
	WorkspaceID     int64
	ExpectedVersion int
	RawInput        *string
	GlobalStyle     *string
	CurrentStep     *int
	Status          *string
	AssetProfiles   json.RawMessage
}

// Patch 带版本的部分更新。
//
// ⚠️ 乐观锁条件里**必须带 workspace_id**：只按 id + version 更新的话，
// 猜到一个 id 就能改别人工作区的脚本。归属校验与并发控制放在同一条
// where 里，漏掉任一条都不会有编译错误，只会有一个安全洞。
func (r *Repo) Patch(ctx context.Context, in PatchInput) (Script, error) {
	tag, err := r.pool.Exec(ctx, `
		update script_instance set
			raw_input      = coalesce($4, raw_input),
			global_style   = coalesce($5, global_style),
			current_step   = coalesce($6, current_step),
			status         = coalesce($7, status),
			asset_profiles = coalesce($8, asset_profiles),
			version = version + 1, updated_at = now()
		where id = $1 and workspace_id = $2 and version = $3`,
		in.ID, in.WorkspaceID, in.ExpectedVersion,
		in.RawInput, in.GlobalStyle, in.CurrentStep, in.Status, nilJSON(in.AssetProfiles))
	if err != nil {
		return Script{}, err
	}
	if tag.RowsAffected() == 0 {
		// 分不清是「不存在」还是「版本过期」时，先查一次再回答 ——
		// 两者对前端的含义完全不同：一个该关掉页面，一个该刷新重试。
		if _, err := r.Get(ctx, in.WorkspaceID, in.ID); err != nil {
			return Script{}, err
		}
		return Script{}, ErrVersionConflict
	}
	return r.Get(ctx, in.WorkspaceID, in.ID)
}

func nilJSON(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	return raw
}
