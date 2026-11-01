// Package repo 是媒体对象的数据访问层。
package repo

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrNotFound = errors.New("媒体对象不存在")

type Repo struct{ pool *pgxpool.Pool }

func New(pool *pgxpool.Pool) *Repo { return &Repo{pool: pool} }

type MediaObject struct {
	ID              int64
	WorkspaceID     int64
	Bucket          string
	ObjectKey       string
	ContentType     string
	SizeBytes       int64
	Checksum        string
	Purpose         string
	CreatedByUserID int64
	// CreatedAt 只有 ListByWorkspace 取（资产库「我上传的」显示日期）；别的查询不取，为零值。
	CreatedAt time.Time
	// Width / Height 是图片的像素尺寸；未知时为 0（格式不认识或没记下来）。
	//
	// ⚠️ 蒙版校验靠它（NEW-13）：尺寸对不上的话上游会把蒙版拉伸后贴上去，
	// 用户圈的是人脸、改掉的是半张背景，而任务是"成功"的。
	Width  int
	Height int
	// DurationMs 音视频时长（毫秒）；0 表示未知。
	//
	// ⚠️ 时间线的裁切校验靠它（NEW-07）：没有时长就没法判断"取到 30 秒"
	// 是不是超出了一段 10 秒的素材，而超出的后果是成片里一段黑屏。
	DurationMs int

	// SourceMediaID / DeriveKind / DeriveIndex / DeriveMeta 是派生溯源（NEW-14）。
	//
	// ⚠️ **逻辑外键且不级联**：源图删了，切块仍然有效（NEW-16 要求
	// "源节点删除不破坏仍有效引用"）。切块是一份独立的字节，
	// 它的存在不依赖源图还在不在 —— 查不到源图按"源已删除"显示，不是错误。
	SourceMediaID int64
	DeriveKind    string
	// DeriveKey 是同一种派生下的变体（宫格填 "3x3"）。没有它的话
	// 一张图一辈子只能切一种格数 —— 先 2×2 再 3×3 会撞唯一索引。
	DeriveKey   string
	DeriveIndex int
	DeriveMeta  json.RawMessage
}

func (r *Repo) Insert(ctx context.Context, m MediaObject) error {
	var meta any
	if len(m.DeriveMeta) > 0 {
		meta = string(m.DeriveMeta)
	}
	_, err := r.pool.Exec(ctx, `
		insert into media_object
		  (id, workspace_id, bucket, object_key, content_type, size_bytes,
		   checksum, purpose, created_by_user_id, width, height, duration_ms,
		   source_media_id, derive_kind, derive_key, derive_index, derive_meta)
		values ($1,$2,$3,$4,$5,$6,$7,$8,$9,nullif($10,0),nullif($11,0),nullif($12,0),
		        nullif($13,0::bigint),nullif($14,''),nullif($15,''),nullif($16,0),$17::jsonb)`,
		m.ID, m.WorkspaceID, m.Bucket, m.ObjectKey, m.ContentType,
		m.SizeBytes, m.Checksum, m.Purpose, m.CreatedByUserID, m.Width, m.Height,
		m.DurationMs, m.SourceMediaID, m.DeriveKind, m.DeriveKey, m.DeriveIndex, meta)
	return err
}

// ListDerived 列出某张源图在某种格数下切出来的派生媒体，按编号升序。
//
// ⚠️ 带上 workspace 条件：派生列表也是一个"按 id 探测别人有什么"的入口，
// 少一个条件就等于把别人的切块列给当前用户看。
func (r *Repo) ListDerived(ctx context.Context, workspaceID, sourceID int64,
	kind, key string) ([]MediaObject, error) {
	rows, err := r.pool.Query(ctx, `
		select id, workspace_id, bucket, object_key, content_type, size_bytes,
		       coalesce(checksum,''), purpose, created_by_user_id,
		       coalesce(width,0), coalesce(height,0), coalesce(derive_index,0),
		       coalesce(derive_meta,'{}'::jsonb)
		from media_object
		where source_media_id = $1 and derive_kind = $2
		  and coalesce(derive_key,'') = $4
		  and status = 'active' and workspace_id = $3
		order by derive_index`, sourceID, kind, workspaceID, key)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MediaObject
	for rows.Next() {
		m := MediaObject{SourceMediaID: sourceID, DeriveKind: kind, DeriveKey: key}
		if err := rows.Scan(&m.ID, &m.WorkspaceID, &m.Bucket, &m.ObjectKey, &m.ContentType,
			&m.SizeBytes, &m.Checksum, &m.Purpose, &m.CreatedByUserID,
			&m.Width, &m.Height, &m.DeriveIndex, &m.DeriveMeta); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (r *Repo) Get(ctx context.Context, id int64) (MediaObject, error) {
	var m MediaObject
	err := r.pool.QueryRow(ctx, `
		select id, workspace_id, bucket, object_key, content_type,
		       size_bytes, coalesce(checksum,''), purpose, created_by_user_id,
		       coalesce(width,0), coalesce(height,0)
		from media_object where id = $1 and status = 'active'`, id).
		Scan(&m.ID, &m.WorkspaceID, &m.Bucket, &m.ObjectKey, &m.ContentType,
			&m.SizeBytes, &m.Checksum, &m.Purpose, &m.CreatedByUserID,
			&m.Width, &m.Height)
	if errors.Is(err, pgx.ErrNoRows) {
		return MediaObject{}, ErrNotFound
	}
	return m, err
}

// FindDerived 取一份媒体某种派生件（按 derive_key），只取 active 的。没有返回 ok=false。
//
// 不带 workspace 条件：只给「调用方已判过可见性」的公开路径用（公开缩略图，见 media/app/public_link.go）。
func (r *Repo) FindDerived(ctx context.Context, sourceID int64, kind, key string) (MediaObject, bool, error) {
	var m MediaObject
	err := r.pool.QueryRow(ctx, `
		select id, workspace_id, bucket, object_key, content_type,
		       size_bytes, coalesce(checksum,''), purpose, created_by_user_id,
		       coalesce(width,0), coalesce(height,0)
		from media_object
		where source_media_id = $1 and derive_kind = $2 and coalesce(derive_key,'') = $3 and status = 'active'
		order by id limit 1`, sourceID, kind, key).
		Scan(&m.ID, &m.WorkspaceID, &m.Bucket, &m.ObjectKey, &m.ContentType,
			&m.SizeBytes, &m.Checksum, &m.Purpose, &m.CreatedByUserID,
			&m.Width, &m.Height)
	if errors.Is(err, pgx.ErrNoRows) {
		return MediaObject{}, false, nil
	}
	return m, err == nil, err
}

// ErrInUse 表示媒体仍被活动引用占用。
var ErrInUse = errors.New("媒体被活动引用占用")

// GetScoped 按工作区取媒体。
//
// ⚠️ **归属校验必须在查询里**，不能"先按 id 查出来再比对 workspace"——
// 那种写法一旦有人忘了比对就静默放行，而查询里带条件是查不到就没有。
// workspace_id = 0 是平台级素材（封面、试听），登录即可读。
func (r *Repo) GetScoped(ctx context.Context, workspaceID, id int64) (MediaObject, error) {
	var m MediaObject
	err := r.pool.QueryRow(ctx, `
		select id, workspace_id, bucket, object_key, content_type, size_bytes,
		       coalesce(checksum,''), purpose, created_by_user_id,
		       coalesce(width,0), coalesce(height,0), coalesce(duration_ms,0),
		       coalesce(source_media_id,0), coalesce(derive_kind,''),
		       coalesce(derive_index,0)
		from media_object
		where id = $1 and status = 'active' and (workspace_id = $2 or workspace_id = 0)`,
		id, workspaceID).Scan(&m.ID, &m.WorkspaceID, &m.Bucket, &m.ObjectKey,
		&m.ContentType, &m.SizeBytes, &m.Checksum, &m.Purpose, &m.CreatedByUserID,
		&m.Width, &m.Height, &m.DurationMs, &m.SourceMediaID, &m.DeriveKind, &m.DeriveIndex)
	if errors.Is(err, pgx.ErrNoRows) {
		return MediaObject{}, ErrNotFound
	}
	return m, err
}

// Usage 是一条活动引用。
type Usage struct {
	UsageType string
	RefID     int64
	CreatedAt time.Time
}

// AddUsage 登记引用。幂等：同一 (media, type, ref) 重复登记不报错 ——
// 重试、重复保存都会走到这里，报错只会把调用方逼着去 catch。
func (r *Repo) AddUsage(ctx context.Context, id, mediaID int64, usageType string, refID int64) error {
	_, err := r.pool.Exec(ctx, `
		insert into media_usage (id, media_object_id, usage_type, usage_ref_id)
		values ($1, $2, $3, $4)
		on conflict (media_object_id, usage_type, usage_ref_id) do nothing`,
		id, mediaID, usageType, refID)
	return err
}

// ReleaseUsage 解除某个业务对象的全部引用（项目被彻底删除、任务被取消等）。
func (r *Repo) ReleaseUsage(ctx context.Context, usageType string, refID int64) (int64, error) {
	tag, err := r.pool.Exec(ctx,
		`delete from media_usage where usage_type = $1 and usage_ref_id = $2`, usageType, refID)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func (r *Repo) ListUsages(ctx context.Context, mediaID int64) ([]Usage, error) {
	rows, err := r.pool.Query(ctx, `
		select usage_type, usage_ref_id, created_at from media_usage
		where media_object_id = $1 order by created_at`, mediaID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Usage
	for rows.Next() {
		var u Usage
		if err := rows.Scan(&u.UsageType, &u.RefID, &u.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// SoftDelete 软删媒体。
//
// ⚠️ 有活动引用时**拒绝**，不是静默成功：删掉一个正在被进行中任务引用的文件，
// 任务会在 Worker 那侧取不到输入而失败，而用户以为自己只是清理了一张图。
func (r *Repo) SoftDelete(ctx context.Context, workspaceID, id int64) error {
	var n int
	if err := r.pool.QueryRow(ctx,
		`select count(*) from media_usage where media_object_id = $1`, id).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return ErrInUse
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, err := tx.Exec(ctx, `
		update media_object set status = 'deleted', deleted_at = now()
		where id = $1 and workspace_id = $2 and status = 'active'`, id, workspaceID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	// 剪辑台的预览素材随源一起软删（SCD-37）：它们是附属品，源没了它们就没有用处，留着只占空间，
	// 而且会在回收站里「复活」成一份没有来源的预览。同一个事务：不会出现源删了、预览还在列表里的瞬间。
	// 宫格切块与共享导入不在其中 —— 它们是独立素材（NEW-16）。
	if _, err := tx.Exec(ctx, `
		update media_object set status = 'deleted', deleted_at = now()
		where source_media_id = $1 and workspace_id = $2 and status = 'active' and derive_kind = any($3)`,
		id, workspaceID, previewDeriveKinds); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// previewDeriveKinds 剪辑台的预览素材（与 media/app 的 DerivePreviewProxy / DeriveFilmstrip / DeriveSpeechAudio 一致）。
// 公开缩略图（public_thumb，2026-11-02）同理：源没了它就没有用处。
var previewDeriveKinds = []string{"preview_proxy", "filmstrip", "speech_audio", "public_thumb"}

// FindCollectable 找出可回收的媒体：**用户已删除 且 无引用 且 已超保留期**。
//
// ⚠️ 三个条件都要，而**第一个是最重要的那个**。
//
// 原先这里还有第二个分支 ——「`status='active'` 且超期且 `purpose='upload_reference'`
// 且无引用」，意图是"用户传了图却没用，也该按同样期限回收"。它会**删掉用户素材库里
// 还看得见的文件**，而且这件事有两层：
//
//  1. `/assets` 素材库列的**正是** `status='active' and purpose='upload_reference'`。
//     这个分支删的不是"孤儿"，是用户当前正看着的那一屏。
//  2. 「无 `media_usage` 行」被当成了「用户不要了」，可八个 `UsageType` 常量里
//     **只有两个真的有人登记**（`project_cover` 与 `generation_output`）——
//     脚本资产、时间线片段、生成输入一条都没有登记过。于是"没人用"对
//     几乎每一个素材都成立。
//
// 两层各自都足以造成数据丢失，合起来则是**必然**：用户上传一张角色参考图、
// 当天就用它出了图，七天后这张图连同对象一起消失，而**没有任何报错** ——
// 库里 `script_asset.media_object_id` 还指着它，界面上是一张裂图。
// 所有测试都是绿的（没有测试会等七天），`db-verify` 也是绿的。
//
// ⚠️ **留存期是 PARAM-05，未定稿**。在没有批准策略时，默认值只能是「不删用户
// 看得见的东西」—— 与水印那条默认值同构（"加错了看得见，漏了看不见"；这里是
// "留着看得见，删了看不见"）。自动回收只处理**用户自己删过**的东西：那是一次
// 明确的意图，保留期只是给他反悔的窗口。
//
// 「传了从不用的图占着空间」是**配额问题不是回收问题**，同样等 PARAM-05 定稿；
// 在那之前多占一点存储，远好过把用户的素材悄悄删掉。
func (r *Repo) FindCollectable(ctx context.Context, before time.Time, limit int) ([]MediaObject, error) {
	rows, err := r.pool.Query(ctx, `
		select m.id, m.workspace_id, m.bucket, m.object_key, m.content_type,
		       m.size_bytes, coalesce(m.checksum,''), m.purpose, m.created_by_user_id
		from media_object m
		where not exists (select 1 from media_usage u where u.media_object_id = m.id)
		  and ((m.status = 'deleted' and m.deleted_at is not null and m.deleted_at < $1)
		       -- 剪辑台的预览素材：源已经不在（被永久删除、或被删了还没回收）时随回收处理（SCD-37）。
		       -- 源被删时它们本来就跟着软删了；这一支兜住源删在这条规则之前、或预览晚于删除才生成的。
		       or (m.status = 'active' and m.derive_kind = any($3)
		           and not exists (select 1 from media_object s where s.id = m.source_media_id and s.status = 'active')))
		order by m.deleted_at nulls first
		limit $2`, before, limit, previewDeriveKinds)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MediaObject
	for rows.Next() {
		var m MediaObject
		if err := rows.Scan(&m.ID, &m.WorkspaceID, &m.Bucket, &m.ObjectKey, &m.ContentType,
			&m.SizeBytes, &m.Checksum, &m.Purpose, &m.CreatedByUserID); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// DeleteRow 在对象存储删除成功后删库行。
//
// ⚠️ 顺序是**先删对象再删行**：反过来的话对象存储里会留下谁也不认识的文件，
// 只能靠对账脚本捞。删对象失败就留着行，下一轮再试。
//
// 连带删掉以它为源、或以它为产物的分析行（SCT-16）：分析行还说「成功」而指着的预览版已经不在的话，
// 剪辑台永远取不到、也永远不会重做。一条语句（CTE）里删，不会出现删了媒体留下分析行的中间态。
func (r *Repo) DeleteRow(ctx context.Context, id int64) error {
	_, err := r.pool.Exec(ctx, `
		with a as (delete from media_analysis where media_object_id = $1 or derived_media_id = $1)
		delete from media_object where id = $1`, id)
	return err
}

// ListByWorkspace 列出工作区内的上传参考，按 id 倒序（id 是雪花，天然按时间倒序）。
//
// ⚠️ 只列 `upload_reference`：生成产物属于"作品"，有自己的列表与可见性规则，
// 混进"我的素材"会让用户在这里误删产物。
func (r *Repo) ListByWorkspace(ctx context.Context, workspaceID int64, kind string,
	limit int, cursorID int64) ([]MediaObject, error) {
	sql := `select id, workspace_id, bucket, object_key, content_type, size_bytes,
	               coalesce(checksum,''), purpose, created_by_user_id, created_at
	        from media_object
	        where workspace_id = $1 and status = 'active' and purpose = 'upload_reference'
	          and (derive_kind is null or derive_kind not in ('preview_proxy', 'filmstrip', 'speech_audio', 'video_poster', 'video_lastframe'))`
	args := []any{workspaceID}
	if kind != "" {
		args = append(args, kind+"/%")
		sql += ` and content_type like $` + strconv.Itoa(len(args))
	}
	if cursorID > 0 {
		args = append(args, cursorID)
		sql += ` and id < $` + strconv.Itoa(len(args))
	}
	args = append(args, limit)
	sql += ` order by id desc limit $` + strconv.Itoa(len(args))

	rows, err := r.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MediaObject
	for rows.Next() {
		var m MediaObject
		if err := rows.Scan(&m.ID, &m.WorkspaceID, &m.Bucket, &m.ObjectKey, &m.ContentType,
			&m.SizeBytes, &m.Checksum, &m.Purpose, &m.CreatedByUserID, &m.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// FindVideosWithoutFrames 找出 `kinds` 里还缺任何一种帧（封面帧 / 末帧）的视频，新的先来（CD-83）。
//
// ⚠️ 「有没有这一帧」不看状态：封面帧被删过（用户删了那段视频、或运维清理）就不再补，
// 否则唯一索引会让每一轮都插入失败、一直重试下去。
// `exclude` 是本进程里刚截失败的那几个（畸形文件），免得每分钟都在同一批上空转。
func (r *Repo) FindVideosWithoutFrames(ctx context.Context, kinds []string, exclude []int64, limit int) (
	[]MediaObject, error) {
	if exclude == nil {
		exclude = []int64{}
	}
	rows, err := r.pool.Query(ctx, `
		select v.id, v.workspace_id, v.bucket, v.object_key, v.content_type, v.size_bytes,
		       coalesce(v.checksum,''), v.purpose, v.created_by_user_id, coalesce(v.duration_ms,0)
		from media_object v
		where v.status = 'active' and v.source_media_id is null and v.content_type like 'video/%'
		  -- 上传的视频也截（2026-10-30）：资产库「我上传的」、智能剪辑节点卡与「补镜」的末帧都要它；
		  -- 帧与视频同 purpose，所以「我上传的」列表另外排除了 video_poster / video_lastframe（ListByWorkspace）
		  and v.purpose in ('generation_output', 'render_output', 'upload_reference')
		  and not (v.id = any($1::bigint[]))
		  and exists (select 1 from unnest($3::text[]) k(kind)
		               where not exists (select 1 from media_object p
		                                  where p.source_media_id = v.id and p.derive_kind = k.kind))
		order by v.id desc
		limit $2`, exclude, limit, kinds)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MediaObject
	for rows.Next() {
		var m MediaObject
		if err := rows.Scan(&m.ID, &m.WorkspaceID, &m.Bucket, &m.ObjectKey, &m.ContentType,
			&m.SizeBytes, &m.Checksum, &m.Purpose, &m.CreatedByUserID, &m.DurationMs); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
