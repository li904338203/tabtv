package repo

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// ---------------------------------------------------------------- 3D 导演台场景（docs/agent/director，DRT-02）
//
// 形状照抄项目文档（document.go）：
//
// ⚠️ 每条查询都带 workspace_id **与** project_id：场景 ID 是全局雪花号，只按 id 查的话，
// 带着自己工作区的头就能读别人的场景；只带 workspace_id 不带 project_id 的话，
// 副本项目的导演台节点会读到源项目那份（复制时 sceneId 没重绑上的那种），而两份内容一模一样。
//
// ⚠️ 场景没有 status 列（迁移 00065）：删除导演台节点不删场景（撤销要能恢复，DR-F-06），
// 场景只随项目清理物理删除（DRD-30）。所以这里的「有效」只看项目。

// DirectorScene 是一个导演台场景（含正文）。
type DirectorScene struct {
	ID                int64
	WorkspaceID       int64
	ProjectID         int64
	CanvasNodeID      string
	Scene             json.RawMessage
	SchemaVersion     int
	Version           int
	RenderFingerprint string
	MediaIDs          []string
	// Source 是最后一次写入的来源：user / agent / mcp。
	Source         string
	EditorID       int64
	EditorName     string
	AgentSessionID int64
	McpGrantID     int64
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// DirectorSceneSummary 是场景摘要：**不带正文**（一个场景可达 512 KiB）。
type DirectorSceneSummary struct {
	ID                int64
	CanvasNodeID      string
	Version           int
	RenderFingerprint string
	// OutputCameraID 为空串表示正文里没有输出机位（缺省或 null）。
	OutputCameraID string
	EditorName     string
	Source         string
	UpdatedAt      time.Time
}

// DirectorSceneWrite 是一次保存要写的内容。正文已由 app 层校验、算好指纹与媒体 ID。
type DirectorSceneWrite struct {
	Scene             json.RawMessage
	SchemaVersion     int
	RenderFingerprint string
	MediaIDs          []string
	Source            string
	EditorID          int64
	AgentSessionID    int64
	McpGrantID        int64
}

const directorSceneCols = `d.id, d.workspace_id, d.project_id, d.canvas_node_id, d.scene, d.schema_version,
	d.version, d.render_fingerprint, d.media_ids, d.source,
	coalesce(d.last_editor_user_id, 0), coalesce(u.nickname, ''),
	coalesce(d.agent_session_id, 0), coalesce(d.mcp_grant_id, 0), d.created_at, d.updated_at`

// ⚠️ `left join` 不是 `join`：最后编辑者为空（副本新建）或账号已清理时，
// 内连接会让整行消失，调用方拿到的是 ErrNotFound —— 界面说"场景不存在"。
const directorSceneFrom = ` from director_scene d left join app_user u on u.id = d.last_editor_user_id `

func scanDirectorScene(row pgx.Row) (DirectorScene, error) {
	var s DirectorScene
	var scene []byte
	err := row.Scan(&s.ID, &s.WorkspaceID, &s.ProjectID, &s.CanvasNodeID, &scene, &s.SchemaVersion,
		&s.Version, &s.RenderFingerprint, &s.MediaIDs, &s.Source, &s.EditorID, &s.EditorName,
		&s.AgentSessionID, &s.McpGrantID, &s.CreatedAt, &s.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return DirectorScene{}, ErrNotFound
	}
	if err != nil {
		return DirectorScene{}, err
	}
	s.Scene = json.RawMessage(scene)
	if s.MediaIDs == nil {
		s.MediaIDs = []string{}
	}
	return s, nil
}

// DirectorFork 是分叉建场景的复制来源：源场景的媒体记录随正文一起复制（同一事务）。
type DirectorFork struct {
	SourceSceneID int64
	NextID        func() int64
}

// CreateDirectorScene 新建场景，按（项目，画布节点）幂等（DRD-17）。
//
// 返回值第二项是「这次真的新建了」：该节点已有场景时返回已有那份与 false ——
// 网络重试、两个标签页同时打开同一个粘贴出来的节点，都会走到这里，要收敛到同一份。
// fork 非空时在**同一事务**里复制源场景的媒体记录：分开做的话中途失败会留下一个没有截图历史的副本，
// 而副本节点 params 里镜像的截图只被源场景的记录保护着 —— 源项目一清理，副本节点就是裂图。
//
// ⚠️ 带 `exists(project active)` 条件：调用方先查过项目，但两步之间项目可能刚被删进回收站 ——
// 往回收站里的项目写场景，等于写进一个随时会被清理任务删掉的地方。
// 0 行插入有两种原因：节点已有场景（回读它）与项目不在了（回读也查不到 → ErrNotFound）。
func (r *Repo) CreateDirectorScene(ctx context.Context, s DirectorScene, fork *DirectorFork) (
	DirectorScene, bool, error) {
	mediaIDs := s.MediaIDs
	if mediaIDs == nil {
		mediaIDs = []string{}
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return DirectorScene{}, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	row := tx.QueryRow(ctx, `
		with ins as (
			insert into director_scene (id, workspace_id, project_id, canvas_node_id, scene, schema_version,
				render_fingerprint, media_ids, source, last_editor_user_id, agent_session_id, mcp_grant_id)
			select $1, $2, $3, $4, $5::jsonb, $6, $7, $8, $9,
				nullif($10::bigint, 0), nullif($11::bigint, 0), nullif($12::bigint, 0)
			where exists (select 1 from project p
				where p.id = $3 and p.workspace_id = $2 and p.status = 'active')
			on conflict (project_id, canvas_node_id) do nothing
			returning *)
		select `+directorSceneCols+` from ins d left join app_user u on u.id = d.last_editor_user_id`,
		s.ID, s.WorkspaceID, s.ProjectID, s.CanvasNodeID, []byte(s.Scene), s.SchemaVersion,
		s.RenderFingerprint, mediaIDs, s.Source, s.EditorID, s.AgentSessionID, s.McpGrantID)
	created, err := scanDirectorScene(row)
	if err == nil {
		if fork != nil {
			if err := copyDirectorMedia(ctx, tx, s.WorkspaceID, fork.SourceSceneID, created, fork.NextID); err != nil {
				return DirectorScene{}, false, err
			}
		}
		if err := tx.Commit(ctx); err != nil {
			return DirectorScene{}, false, err
		}
		return created, true, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return DirectorScene{}, false, err
	}
	_ = tx.Rollback(ctx)
	existing, err := r.GetDirectorSceneByNode(ctx, s.WorkspaceID, s.ProjectID, s.CanvasNodeID)
	if err != nil {
		return DirectorScene{}, false, err // 含 ErrNotFound：项目不在了
	}
	return existing, false, nil
}

// GetDirectorScene 按 ID 取场景。
func (r *Repo) GetDirectorScene(ctx context.Context, workspaceID, projectID, id int64) (DirectorScene, error) {
	return scanDirectorScene(r.pool.QueryRow(ctx, `select `+directorSceneCols+directorSceneFrom+`
		where d.id = $1 and d.workspace_id = $2 and d.project_id = $3`, id, workspaceID, projectID))
}

// GetDirectorSceneByNode 按画布节点取场景（幂等建时回读已有那份）。
func (r *Repo) GetDirectorSceneByNode(ctx context.Context, workspaceID, projectID int64,
	canvasNodeID string) (DirectorScene, error) {
	return scanDirectorScene(r.pool.QueryRow(ctx, `select `+directorSceneCols+directorSceneFrom+`
		where d.workspace_id = $1 and d.project_id = $2 and d.canvas_node_id = $3`,
		workspaceID, projectID, canvasNodeID))
}

// GetDirectorSceneInWorkspace 只按工作区取场景：分叉（`sourceSceneId`）时取源场景用。
//
// ⚠️ 这里刻意不带 project_id —— 源场景可以在本工作区的另一个项目里（跨项目粘贴节点）。
// 源项目是否可读由 app 层再查一次；工作区条件是这条查询的底线，不能去掉。
func (r *Repo) GetDirectorSceneInWorkspace(ctx context.Context, workspaceID, id int64) (DirectorScene, error) {
	return scanDirectorScene(r.pool.QueryRow(ctx, `select `+directorSceneCols+directorSceneFrom+`
		where d.id = $1 and d.workspace_id = $2`, id, workspaceID))
}

// UpdateDirectorScene 乐观锁保存正文。
//
// 0 行更新有两种原因，**必须在这一层分开**（同 UpdateDocument）：版本不一致（冲突，可恢复）
// 与场景不存在 / 项目已删（404）。冲突时返回服务端当前那份（含最后编辑者），
// app 层据此说出"是谁改的"。
//
// ⚠️ 成功时用 `returning` 取回**这一次**写下的行：先改后查的话，查之前别人可能又存了一版，
// 返回的版本号就不是自己写的那个，前端拿它当基线再存就会无故冲突。
func (r *Repo) UpdateDirectorScene(ctx context.Context, workspaceID, projectID, id int64, version int,
	in DirectorSceneWrite) (DirectorScene, error) {
	mediaIDs := in.MediaIDs
	if mediaIDs == nil {
		mediaIDs = []string{}
	}
	row := r.pool.QueryRow(ctx, `
		with upd as (
			update director_scene d set scene = $5::jsonb, schema_version = $6, render_fingerprint = $7,
				media_ids = $8, source = $9, last_editor_user_id = nullif($10::bigint, 0),
				-- Agent 会话与 MCP 授权要随每次写入重写：一次真人保存之后还留着上一次的会话 ID 的话，
				-- 冲突详情会把这次真人保存说成「Agent 改的」（同画布 AGT-10）。
				agent_session_id = nullif($11::bigint, 0), mcp_grant_id = nullif($12::bigint, 0),
				version = d.version + 1, updated_at = now()
			where d.id = $1 and d.workspace_id = $2 and d.project_id = $3 and d.version = $4
				and exists (select 1 from project p
					where p.id = $3 and p.workspace_id = $2 and p.status = 'active')
			returning *)
		select `+directorSceneCols+` from upd d left join app_user u on u.id = d.last_editor_user_id`,
		id, workspaceID, projectID, version, []byte(in.Scene), in.SchemaVersion, in.RenderFingerprint,
		mediaIDs, in.Source, in.EditorID, in.AgentSessionID, in.McpGrantID)
	saved, err := scanDirectorScene(row)
	if err == nil {
		return saved, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return DirectorScene{}, err
	}
	cur, err := r.GetDirectorScene(ctx, workspaceID, projectID, id)
	if err != nil {
		return DirectorScene{}, err // 含 ErrNotFound
	}
	// 行还在却没改到：版本不一致，或项目在两步之间被删进了回收站。
	// 后者由 app 层的 projectWritable 先挡，这里统一按冲突回报（当前行原样带回）。
	return cur, ErrVersionConflict
}

// ListDirectorSceneSummaries 列出项目的场景摘要，按建立先后排序。
//
// ⚠️ 不选 `scene` 列：一个场景可达 512 KiB，摘要只为节点卡判断过期与重连对账。
// 输出机位用 `scene->>'outputCameraId'` 在库里取出来（null 与缺省都是 SQL NULL）。
func (r *Repo) ListDirectorSceneSummaries(ctx context.Context, workspaceID, projectID int64) (
	[]DirectorSceneSummary, error) {
	rows, err := r.pool.Query(ctx, `
		select d.id, d.canvas_node_id, d.version, d.render_fingerprint,
			coalesce(d.scene->>'outputCameraId', ''), coalesce(u.nickname, ''), d.source, d.updated_at
		`+directorSceneFrom+`
		where d.workspace_id = $1 and d.project_id = $2
		order by d.created_at, d.id`, workspaceID, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DirectorSceneSummary{}
	for rows.Next() {
		var s DirectorSceneSummary
		if err := rows.Scan(&s.ID, &s.CanvasNodeID, &s.Version, &s.RenderFingerprint, &s.OutputCameraID,
			&s.EditorName, &s.Source, &s.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
