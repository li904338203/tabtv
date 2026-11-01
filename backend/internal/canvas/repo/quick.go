package repo

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
)

// 创造页「直接生成」用到的三条写入/查询（AGT-42）。
//
// ⚠️ 单独一个文件：repo.go 是多个任务同时在改的共享文件。

// InsertWithIdemKey 建项目并记下创建幂等键（迁移 00039）。
// p.Origin 非空时一并写入来源（MKD-3，迁移 00058）；空串写 null。
//
// 同一工作区同一个键已存在时返回 ErrDuplicate（`uk_project_create_idem`），
// 调用方据此改走「返回已建的那个」—— 并发的两个同键请求都查不到已有行、
// 都来插入时，只有一个能成功，另一个就走这条路。
func (r *Repo) InsertWithIdemKey(ctx context.Context, p Project, key string) error {
	canvas := p.CanvasData
	if len(canvas) == 0 {
		canvas = json.RawMessage(`{}`)
	}
	_, err := r.pool.Exec(ctx, `
		insert into project (id, workspace_id, created_by_user_id, name,
			folder_id, canvas_data, status, create_idempotency_key, origin)
		values ($1, $2, $3, $4, nullif($5::bigint,0), $6, 'active', $7, nullif($8, ''))`,
		p.ID, p.WorkspaceID, p.CreatedBy, p.Name, p.FolderID, canvas, key, p.Origin)
	return mapDuplicate(err)
}

// FindByCreateIdemKey 按创建幂等键取项目（含画布）。
//
// ⚠️ 不过滤 status：用户建完又删进了回收站，再收到一次同键重放时
// 也必须认出"这个键已经用过"，否则会再建一个新项目。
func (r *Repo) FindByCreateIdemKey(ctx context.Context, workspaceID int64, key string) (Project, error) {
	p, err := scanProject(r.pool.QueryRow(ctx,
		`select `+projectCols+` from project
		 where workspace_id = $1 and create_idempotency_key = $2`, workspaceID, key))
	if err != nil {
		return p, err
	}
	err = r.pool.QueryRow(ctx,
		`select coalesce(canvas_data, '{}'::jsonb) from project where id = $1`, p.ID).Scan(&p.CanvasData)
	return p, err
}

// MarkNodeResult 把一个节点的 `result` 写成给定值（受理成功 → 跟踪中；受理失败 → 原话）。
//
// ⚠️ 带版本 CAS：只在画布仍是 expectVersion 时写入并进一位。
// 用户已经打开并保存过的话他手上的画布才是事实，服务端这一笔**放弃**（返回 false），
// 而不是覆盖 —— 覆盖等于在他不知情时改他的画布（§5.21 后台改不了画布同一条）。
//
// 只改那个节点的 `result` 键，节点与文档上的其余键（含不认识的）原样保留（§5.4）。
func (r *Repo) MarkNodeResult(ctx context.Context, workspaceID, projectID int64, nodeID string,
	result json.RawMessage, expectVersion int, editorUserID int64) (bool, error) {
	var canvas json.RawMessage
	err := r.pool.QueryRow(ctx, `
		select coalesce(canvas_data, '{}'::jsonb) from project
		where id = $1 and workspace_id = $2 and status = 'active' and version = $3`,
		projectID, workspaceID, expectVersion).Scan(&canvas)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	next, found, err := setNodeResult(canvas, nodeID, result)
	if err != nil || !found {
		return false, err
	}
	tag, err := r.pool.Exec(ctx, `
		update project set canvas_data = $4, version = version + 1, updated_at = now(),
			last_editor_user_id = nullif($5::bigint, 0)
		where id = $1 and workspace_id = $2 and status = 'active' and version = $3`,
		projectID, workspaceID, expectVersion, next, editorUserID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// setNodeResult 在文档里找到 nodeID 那个节点，把它的 result 换掉。
// 用 RawMessage 逐层解开，其余键一个字节都不动。
func setNodeResult(canvas json.RawMessage, nodeID string, result json.RawMessage) (json.RawMessage, bool, error) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(canvas, &doc); err != nil {
		return nil, false, err
	}
	var nodes []map[string]json.RawMessage
	if raw, ok := doc["nodes"]; ok {
		if err := json.Unmarshal(raw, &nodes); err != nil {
			return nil, false, err
		}
	}
	found := false
	for _, n := range nodes {
		var id string
		if json.Unmarshal(n["id"], &id) == nil && id == nodeID {
			n["result"] = result
			found = true
			break
		}
	}
	if !found {
		return nil, false, nil
	}
	rawNodes, err := json.Marshal(nodes)
	if err != nil {
		return nil, false, err
	}
	doc["nodes"] = rawNodes
	out, err := json.Marshal(doc)
	return out, true, err
}
