package repo

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// ---------------------------------------------------------------- 项目文档（AGT-09）
//
// ⚠️ 每条查询都带 workspace_id **与** project_id：文档 ID 是全局雪花号，
// 只按 id 查的话，带着自己工作区的头就能读别人的文档（CD-04 第 2 条）；
// 只带 workspace_id 不带 project_id 的话，副本项目的文档节点会读到源项目那份
// （复制时 docId 没重绑上的那种），而两份内容看起来一模一样、改一份另一份也变。

// Document 是一份项目文档。
type Document struct {
	ID             int64
	WorkspaceID    int64
	ProjectID      int64
	Path           string
	Content        string
	Version        int
	Source         string
	EditorID       int64
	EditorName     string
	AgentSessionID int64
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// DocumentSummary 是列表项：**不带正文**（一份可达 256 KiB，列表只为显示名字）。
type DocumentSummary struct {
	ID        int64
	Path      string
	Version   int
	Source    string
	Bytes     int
	UpdatedAt time.Time
}

const docCols = `d.id, d.workspace_id, d.project_id, d.path, d.content, d.version, d.source,
	coalesce(d.last_editor_user_id, 0), coalesce(u.nickname, ''),
	coalesce(d.agent_session_id, 0), d.created_at, d.updated_at`

// ⚠️ `left join` 不是 `join`：最后编辑者为空（Agent 写的）或账号已清理时，
// 内连接会让整行消失，调用方拿到的是 ErrNotFound —— 界面说"文档不存在"。
const docFrom = ` from project_document d left join app_user u on u.id = d.last_editor_user_id `

func scanDocument(row pgx.Row) (Document, error) {
	var d Document
	err := row.Scan(&d.ID, &d.WorkspaceID, &d.ProjectID, &d.Path, &d.Content, &d.Version,
		&d.Source, &d.EditorID, &d.EditorName, &d.AgentSessionID, &d.CreatedAt, &d.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Document{}, ErrNotFound
	}
	return d, err
}

// ListDocuments 列出项目的有效文档，按路径排序（固定文档 / 在前、/notes 在后）。
func (r *Repo) ListDocuments(ctx context.Context, workspaceID, projectID int64) ([]DocumentSummary, error) {
	rows, err := r.pool.Query(ctx, `
		select id, path, version, source, octet_length(content), updated_at
		from project_document
		where workspace_id = $1 and project_id = $2 and status = 'active'
		order by path`, workspaceID, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DocumentSummary{}
	for rows.Next() {
		var s DocumentSummary
		if err := rows.Scan(&s.ID, &s.Path, &s.Version, &s.Source, &s.Bytes, &s.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// GetDocument 按 ID 取一份有效文档。
func (r *Repo) GetDocument(ctx context.Context, workspaceID, projectID, id int64) (Document, error) {
	return scanDocument(r.pool.QueryRow(ctx, `select `+docCols+docFrom+`
		where d.id = $1 and d.workspace_id = $2 and d.project_id = $3 and d.status = 'active'`,
		id, workspaceID, projectID))
}

// GetDocumentByPath 按路径取（Agent 按固定路径读规划与记忆，经 Documents 端口）。
func (r *Repo) GetDocumentByPath(ctx context.Context, workspaceID, projectID int64, path string) (Document, error) {
	return scanDocument(r.pool.QueryRow(ctx, `select `+docCols+docFrom+`
		where d.workspace_id = $1 and d.project_id = $2 and d.path = $3 and d.status = 'active'`,
		workspaceID, projectID, path))
}

// CreateDocument 新建文档。同项目同路径已有有效文档时返回 ErrDuplicate。
//
// ⚠️ 带 `exists(project active)` 条件：调用方先查过项目，但两步之间项目可能刚被删进回收站 ——
// 往回收站里的项目写文档，等于写进一个随时会被清理任务删掉的地方。
func (r *Repo) CreateDocument(ctx context.Context, d Document) (Document, error) {
	tag, err := r.pool.Exec(ctx, `
		insert into project_document (id, workspace_id, project_id, path, content, source,
			last_editor_user_id, agent_session_id)
		select $1, $2, $3, $4, $5, $6, nullif($7::bigint, 0), nullif($8::bigint, 0)
		where exists (select 1 from project p
			where p.id = $3 and p.workspace_id = $2 and p.status = 'active')`,
		d.ID, d.WorkspaceID, d.ProjectID, d.Path, d.Content, d.Source, d.EditorID, d.AgentSessionID)
	if err := affected(tag, mapDuplicate(err)); err != nil {
		return Document{}, err
	}
	return r.GetDocument(ctx, d.WorkspaceID, d.ProjectID, d.ID)
}

// UpdateDocument 乐观锁保存正文。source 是这次写入的来源（agent / user / mcp），保存成功时一并改写 ——
// 列的语义是「最后一次由谁写入」（契约与前端「Agent 写入 / 手动编辑」都按这个读）；
// 只在新建时记的话，用户改过的 Agent 文档仍显示「Agent 写入」，MCP 改过的用户文档也说不出是机器写的。
// 空串表示不改（兼容）。
//
// 0 行更新有两种原因，**必须在这一层分开**（同 SaveCanvas）：版本不一致（冲突，可恢复）
// 与文档不存在/项目已删（404）。冲突时返回服务端当前那份（含最后编辑者），
// app 层据此说出"是谁改的"，前端据此提供「载入最新 / 以我为准」。
func (r *Repo) UpdateDocument(ctx context.Context, workspaceID, projectID, id int64, version int,
	content string, editorUserID, agentSessionID int64, source string) (Document, error) {
	tag, err := r.pool.Exec(ctx, `
		update project_document d set content = $5, version = d.version + 1, updated_at = now(),
			last_editor_user_id = nullif($6::bigint, 0), agent_session_id = nullif($7::bigint, 0),
			source = coalesce(nullif($8::text, ''), d.source)
		where d.id = $1 and d.workspace_id = $2 and d.project_id = $3 and d.status = 'active'
			and d.version = $4
			and exists (select 1 from project p
				where p.id = $3 and p.workspace_id = $2 and p.status = 'active')`,
		id, workspaceID, projectID, version, content, editorUserID, agentSessionID, source)
	if err != nil {
		return Document{}, err
	}
	cur, err := r.GetDocument(ctx, workspaceID, projectID, id)
	if err != nil {
		return Document{}, err // 含 ErrNotFound
	}
	if tag.RowsAffected() == 0 {
		return cur, ErrVersionConflict
	}
	return cur, nil
}

// CopyIDMaps 是复制项目时「源 ID → 副本 ID」的映射：文档（AGT-09）、3D 导演台场景（DRD-30）
// 与智能剪辑的时间线（SCT-07）。
type CopyIDMaps struct {
	Docs      map[int64]int64
	Scenes    map[int64]int64
	Timelines map[int64]int64
}

// CopyHook 在复制项目的事务里做别的域的复制（时间线），返回「源 → 副本」。为 nil 时不做。
type CopyHook func(ctx context.Context, tx pgx.Tx) (map[int64]int64, error)

// InsertCopy 在**一个事务**里建副本项目，并复制源项目的有效文档、导演台场景与场景的媒体记录（AGT-09、DRD-30）。
//
// ⚠️ 必须同一个事务：分开做的话中间失败会留下一个副本项目，它的文档 / 导演台节点
// 指向源项目的文档或场景 ID —— 打开是"不存在"，或者（ID 没重绑时）更糟。
// build 拿到映射后产出副本的画布（重绑 docId 与 sceneId）。
func (r *Repo) InsertCopy(ctx context.Context, p Project, srcProjectID int64, nextID func() int64,
	build func(ids CopyIDMaps) (json.RawMessage, error), copyTimelines CopyHook) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	docs, err := idMap(ctx, tx, `
		select id from project_document
		where workspace_id = $1 and project_id = $2 and status = 'active'`,
		p.WorkspaceID, srcProjectID, nextID)
	if err != nil {
		return err
	}
	scenes, err := idMap(ctx, tx, `
		select id from director_scene where workspace_id = $1 and project_id = $2`,
		p.WorkspaceID, srcProjectID, nextID)
	if err != nil {
		return err
	}

	var timelines map[int64]int64
	if copyTimelines != nil {
		// 时间线行引用的是副本项目的 ID（逻辑外键），先于项目行插入没有问题；与项目行同一事务提交或一起回滚。
		if timelines, err = copyTimelines(ctx, tx); err != nil {
			return err
		}
	}
	canvas, err := build(CopyIDMaps{Docs: docs, Scenes: scenes, Timelines: timelines})
	if err != nil {
		return err
	}
	p.CanvasData = canvas
	if err := insertProject(ctx, tx, p); err != nil {
		return err
	}
	// 副本：版本从 1 起、不带最后编辑者与 Agent 会话（会话属于源项目），来源原样保留。
	for oldID, newID := range docs {
		if _, err := tx.Exec(ctx, `
			insert into project_document (id, workspace_id, project_id, path, content, source)
			select $1, workspace_id, $2, path, content, source
			from project_document where id = $3 and workspace_id = $4`,
			newID, p.ID, oldID, p.WorkspaceID); err != nil {
			return err
		}
	}
	// 导演台场景：版本从 1 起，来源记 user，不带最后编辑者、Agent 会话与 MCP 授权（它们属于源项目）；
	// canvas_node_id 不变（副本画布的节点 ID 不变）；正文、指纹与媒体引用原样复制 ——
	// 正文没变，指纹就该相同，镜像在节点上的截图不会被误判成过期。
	for oldID, newID := range scenes {
		if _, err := tx.Exec(ctx, `
			insert into director_scene (id, workspace_id, project_id, canvas_node_id, scene, schema_version,
				render_fingerprint, media_ids, source)
			select $1, workspace_id, $2, canvas_node_id, scene, schema_version, render_fingerprint, media_ids, 'user'
			from director_scene where id = $3 and workspace_id = $4`,
			newID, p.ID, oldID, p.WorkspaceID); err != nil {
			return err
		}
		if err := copyDirectorMedia(ctx, tx, p.WorkspaceID, oldID,
			DirectorScene{ID: newID, ProjectID: p.ID}, nextID); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// idMap 执行一条只选 id 的查询，给每个 ID 分配一个新 ID。
func idMap(ctx context.Context, tx pgx.Tx, sql string, workspaceID, projectID int64,
	nextID func() int64) (map[int64]int64, error) {
	rows, err := tx.Query(ctx, sql, workspaceID, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := map[int64]int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids[id] = nextID()
	}
	return ids, rows.Err()
}
