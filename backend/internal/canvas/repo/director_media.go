package repo

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

// ---------------------------------------------------------------- 3D 导演台媒体记录（docs/agent/director，DRD-12）
//
// 截图、深度图、骨架图、预演视频、泼溅文件，每个文件一条。记录**不可变**，只能隐藏：
// 节点 params 镜像的是记录里的指纹与机位快照，记录一改，镜像就说不清当时是什么样子。

// DirectorMedia 是一条导演台媒体记录。可空列用指针：0 与「没有」是两件事（预演之外没有时长）。
type DirectorMedia struct {
	ID                int64
	WorkspaceID       int64
	ProjectID         int64
	SceneID           int64
	Kind              string
	MediaID           int64
	Width             *int
	Height            *int
	DurationMs        *int
	FPS               *int
	CameraID          *string
	CameraName        *string
	Camera            json.RawMessage // SQL NULL 时为 nil
	SceneVersion      *int
	RenderFingerprint *string
	Hidden            bool
	CreatedByUserID   int64
	CreatedAt         time.Time
}

// ListDirectorMedia 列出场景的全部媒体记录（含已隐藏的），按时间倒序。
//
// ⚠️ 条件里带 workspace_id 与 project_id（同场景查询）：记录的 scene_id 是全局雪花号。
func (r *Repo) ListDirectorMedia(ctx context.Context, workspaceID, projectID, sceneID int64) (
	[]DirectorMedia, error) {
	rows, err := r.pool.Query(ctx, `
		select id, workspace_id, project_id, scene_id, kind, media_id, width, height, duration_ms, fps,
			camera_id, camera_name, camera, scene_version, render_fingerprint, is_hidden,
			coalesce(created_by_user_id, 0), created_at
		from director_media
		where workspace_id = $1 and project_id = $2 and scene_id = $3
		order by created_at desc, id desc`, workspaceID, projectID, sceneID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DirectorMedia{}
	for rows.Next() {
		var m DirectorMedia
		var camera []byte
		if err := rows.Scan(&m.ID, &m.WorkspaceID, &m.ProjectID, &m.SceneID, &m.Kind, &m.MediaID,
			&m.Width, &m.Height, &m.DurationMs, &m.FPS, &m.CameraID, &m.CameraName, &camera,
			&m.SceneVersion, &m.RenderFingerprint, &m.Hidden, &m.CreatedByUserID, &m.CreatedAt); err != nil {
			return nil, err
		}
		if camera != nil {
			m.Camera = json.RawMessage(camera)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

const directorMediaCols = `id, workspace_id, project_id, scene_id, kind, media_id, width, height, duration_ms, fps,
	camera_id, camera_name, camera, scene_version, render_fingerprint, is_hidden,
	coalesce(created_by_user_id, 0), created_at`

func scanDirectorMedia(row pgx.Row) (DirectorMedia, error) {
	var m DirectorMedia
	var camera []byte
	err := row.Scan(&m.ID, &m.WorkspaceID, &m.ProjectID, &m.SceneID, &m.Kind, &m.MediaID,
		&m.Width, &m.Height, &m.DurationMs, &m.FPS, &m.CameraID, &m.CameraName, &camera,
		&m.SceneVersion, &m.RenderFingerprint, &m.Hidden, &m.CreatedByUserID, &m.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return DirectorMedia{}, ErrNotFound
	}
	if err != nil {
		return DirectorMedia{}, err
	}
	if camera != nil {
		m.Camera = json.RawMessage(camera)
	}
	return m, nil
}

// InsertDirectorMedia 插入一条媒体记录。
//
// ⚠️ 带「场景属于本项目、项目未删除」的条件：上传文件要几秒到几十秒，这期间项目可能被删进回收站 ——
// 往回收站里的项目写记录，等于写进一个随时会被清理任务删掉的地方。0 行时返回 ErrNotFound，
// 调用方据此软删刚上传的文件（否则那个文件 purpose 隐藏、用户永远删不到）。
func (r *Repo) InsertDirectorMedia(ctx context.Context, m DirectorMedia) (DirectorMedia, error) {
	var camera []byte
	if len(m.Camera) > 0 {
		camera = m.Camera
	}
	return scanDirectorMedia(r.pool.QueryRow(ctx, `
		insert into director_media (id, workspace_id, project_id, scene_id, kind, media_id, width, height,
			duration_ms, fps, camera_id, camera_name, camera, scene_version, render_fingerprint, created_by_user_id)
		select $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13::jsonb, $14, $15, nullif($16::bigint, 0)
		where exists (select 1 from director_scene s join project p on p.id = s.project_id
			where s.id = $4 and s.workspace_id = $2 and s.project_id = $3
				and p.workspace_id = $2 and p.status = 'active')
		returning `+directorMediaCols,
		m.ID, m.WorkspaceID, m.ProjectID, m.SceneID, m.Kind, m.MediaID, m.Width, m.Height,
		m.DurationMs, m.FPS, m.CameraID, m.CameraName, camera, m.SceneVersion, m.RenderFingerprint,
		m.CreatedByUserID))
}

// SetDirectorMediaHidden 只改隐藏标记（DR-F-75）：不删文件，删除保护照常成立。
func (r *Repo) SetDirectorMediaHidden(ctx context.Context, workspaceID, projectID, sceneID, recordID int64,
	hidden bool) (DirectorMedia, error) {
	return scanDirectorMedia(r.pool.QueryRow(ctx, `
		update director_media set is_hidden = $5
		where id = $1 and workspace_id = $2 and project_id = $3 and scene_id = $4
			and exists (select 1 from project p
				where p.id = $3 and p.workspace_id = $2 and p.status = 'active')
		returning `+directorMediaCols, recordID, workspaceID, projectID, sceneID, hidden))
}

// DirectorMediaReferences 是删除保护的计算式检查器（DRD-16，媒体域 UsageDirectorScene）：
// 返回引用该媒体的场景 ID —— 场景正文引用的全景 / 泼溅（media_ids），与导演台媒体记录（截图、预演、泼溅）。
//
// ⚠️ **不按项目状态过滤**：回收站里的项目仍算引用。否则「删项目 → 删截图 → 30 天内恢复项目」
// 会得到一张裂图，而这三步用户每一步都做对了（同 references.go 的封面检查器）。
// ⚠️ 跨工作区查（同 media 的 ReferenceChecker 约定）：按工作区过滤会漏掉引用，而漏掉就是误删。
func (r *Repo) DirectorMediaReferences(ctx context.Context, mediaID int64) ([]int64, error) {
	rows, err := r.pool.Query(ctx, `
		select scene_id from director_media where media_id = $1
		union
		select id from director_scene where media_ids @> array[$2::text]
		order by 1`, mediaID, strconv.FormatInt(mediaID, 10))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// copyDirectorMedia 在事务里把源场景的媒体记录复制到目标场景：新 ID，媒体 ID 不变 ——
// 同一份文件被两个场景引用，删除保护照常成立（清理源项目时，被副本引用着的文件不会被回收）。
// 记录原样保留截图时的版本、指纹与时间（它们是「当时」的快照，复制不改变当时）。
func copyDirectorMedia(ctx context.Context, tx pgx.Tx, workspaceID, srcSceneID int64, dst DirectorScene,
	nextID func() int64) error {
	rows, err := tx.Query(ctx, `select id from director_media where workspace_id = $1 and scene_id = $2`,
		workspaceID, srcSceneID)
	if err != nil {
		return err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, id := range ids {
		if _, err := tx.Exec(ctx, `
			insert into director_media (id, workspace_id, project_id, scene_id, kind, media_id, width, height,
				duration_ms, fps, camera_id, camera_name, camera, scene_version, render_fingerprint, is_hidden,
				created_by_user_id, created_at)
			select $1, workspace_id, $2, $3, kind, media_id, width, height, duration_ms, fps, camera_id,
				camera_name, camera, scene_version, render_fingerprint, is_hidden, created_by_user_id, created_at
			from director_media where id = $4 and workspace_id = $5`,
			nextID(), dst.ProjectID, dst.ID, id, workspaceID); err != nil {
			return err
		}
	}
	return nil
}
