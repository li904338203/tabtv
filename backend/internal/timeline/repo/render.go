package repo

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// 渲染任务的数据访问（NEW-10）。

// ErrRenderBusy 任务不在可执行状态（已被领走或已是终态）。
var ErrRenderBusy = errors.New("渲染任务不在可执行状态")

type RenderJob struct {
	ID              int64
	WorkspaceID     int64
	TimelineID      int64
	TimelineVersion int
	Snapshot        json.RawMessage
	FFmpegVersion   string
	PayerAccountID  int64
	CreatedByUserID int64
	Status          string
	Progress        int
	Stage           string
	OutputMediaID   int64
	CreditsCost     int
	ErrorCode       string
	ErrorMessage    string
	// WatermarkApplied 这次**到底加没加**水印。
	WatermarkApplied bool
	// WorkID 成片登记的作品（0 表示还没登记）。
	WorkID     int64
	DeadlineAt *time.Time
	CreatedAt  time.Time
	UpdatedAt  time.Time
	// IdempotencyKey 受理时的幂等键（SCB-3）；Spec 导出规格；SelfCheck 导出后自检（SCT-15）。
	IdempotencyKey string
	Spec           json.RawMessage
	SelfCheck      json.RawMessage
	// ProjectID / CanvasNodeID 经时间线取（render_job 没有这两列）：事件投到 project:{id}；
	// 绑定了画布节点的时间线导出后不登记作品（SCK-9）。时间线已被删时为 0 / 空。
	ProjectID    int64
	CanvasNodeID string
}

// InsertRender 在**调用方的事务里**写渲染任务与 outbox 消息。
//
// ⚠️ 扣费、写任务、写 outbox 必须同一个事务：拆开的话中间崩溃会留下
// 「扣了钱没有任务」或「有任务没扣钱」，而这两种都只能靠对账人工修（FLOW-01）。
func (r *Repo) InsertRenderInTx(ctx context.Context, tx pgx.Tx, j RenderJob,
	outboxID int64, topic string, payload json.RawMessage) error {
	var spec any
	if len(j.Spec) > 0 {
		spec = string(j.Spec)
	}
	if _, err := tx.Exec(ctx, `
		insert into render_job (id, workspace_id, timeline_id, timeline_version,
			snapshot, payer_account_id, created_by_user_id, status, credits_cost,
			deadline_at, idempotency_key, spec)
		values ($1,$2,$3,$4,$5::jsonb,$6,$7,'queued',$8,$9,nullif($10,''),$11::jsonb)`,
		j.ID, j.WorkspaceID, j.TimelineID, j.TimelineVersion, string(j.Snapshot),
		j.PayerAccountID, j.CreatedByUserID, j.CreditsCost, j.DeadlineAt, j.IdempotencyKey, spec); err != nil {
		if isUniqueViolation(err) {
			return ErrRenderIdemTaken
		}
		return err
	}
	_, err := tx.Exec(ctx, `
		insert into outbox_message (id, aggregate_type, aggregate_id, topic, payload)
		values ($1,'render',$2,$3,$4)`, outboxID, j.ID, topic, payload)
	return err
}

// ⚠️ `work_id` 用 **left join 的标量子查询**而不是 join：内连接会让
// 还没登记作品的任务（排队中、失败的）整行消失 —— 症状是"我的渲染
// 列表里刚提交的那条不见了"，而任务其实在正常跑。
const renderCols = `select r.id, r.workspace_id, r.timeline_id, r.timeline_version,
	r.snapshot, coalesce(r.ffmpeg_version,''), r.payer_account_id,
	r.created_by_user_id, r.status, r.progress, coalesce(r.stage,''),
	coalesce(r.output_media_id,0), r.credits_cost, coalesce(r.error_code,''),
	coalesce(r.error_message,''), r.watermark_applied,
	coalesce((select w.id from work w
	          where w.source_type='render' and w.source_id = r.id), 0),
	r.deadline_at, r.created_at, r.updated_at, coalesce(r.idempotency_key,''), r.spec, r.self_check,
	coalesce((select t.project_id from timeline t where t.id = r.timeline_id), 0),
	coalesce((select t.canvas_node_id from timeline t where t.id = r.timeline_id), '')
	from render_job r `

func scanRender(row pgx.Row) (RenderJob, error) {
	var j RenderJob
	err := row.Scan(&j.ID, &j.WorkspaceID, &j.TimelineID, &j.TimelineVersion,
		&j.Snapshot, &j.FFmpegVersion, &j.PayerAccountID, &j.CreatedByUserID,
		&j.Status, &j.Progress, &j.Stage, &j.OutputMediaID, &j.CreditsCost,
		&j.ErrorCode, &j.ErrorMessage, &j.WatermarkApplied, &j.WorkID,
		&j.DeadlineAt, &j.CreatedAt, &j.UpdatedAt, &j.IdempotencyKey, &j.Spec, &j.SelfCheck,
		&j.ProjectID, &j.CanvasNodeID)
	if errors.Is(err, pgx.ErrNoRows) {
		return RenderJob{}, ErrNotFound
	}
	return j, err
}

func (r *Repo) Render(ctx context.Context, workspaceID, id int64) (RenderJob, error) {
	return scanRender(r.pool.QueryRow(ctx,
		renderCols+`where r.id = $1 and r.workspace_id = $2`, id, workspaceID))
}

// RenderForRun 供 Worker 按 id 取（消息里只有任务 id）。
func (r *Repo) RenderForRun(ctx context.Context, id int64) (RenderJob, error) {
	return scanRender(r.pool.QueryRow(ctx, renderCols+`where r.id = $1`, id))
}

func (r *Repo) ListRenders(ctx context.Context, workspaceID, timelineID int64,
	limit int) ([]RenderJob, error) {
	rows, err := r.pool.Query(ctx,
		renderCols+`where r.workspace_id = $1 and r.timeline_id = $2
		order by r.id desc limit $3`, workspaceID, timelineID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RenderJob
	for rows.Next() {
		j, err := scanRender(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// ErrRenderFull 全站同时渲染数已满（不是失败：消息放回队列，任务留在 queued）。
var ErrRenderFull = errors.New("同时渲染数已满")

// ErrRenderIdemTaken 同一工作区同一把幂等键已有任务（唯一索引兜底，调用方回查那一条）。
var ErrRenderIdemTaken = errors.New("幂等键已有任务")

// ClaimRender 领取任务：在一个事务里「锁 → 数 running → CAS queued → running」。
//
// ⚠️ 消费幂等全靠 CAS：at-least-once 投递意味着同一条消息会重复到达，
// 没有 CAS 的话同一次渲染会跑两遍 —— 两个 ffmpeg 同时啃同一台机器的 CPU，而只有一个的结果会被采信。
//
// ⚠️ 并发上限的数与领取必须原子（SCB-10）：先数再领分两步的话，两个实例同时数到 0、同时领到，
// 全站就有两路渲染。用一把固定键的事务级 advisory lock 串起来（maxConcurrent ≤ 0 表示不限）。
//
// ⚠️ 死线是受理时按成片时长算好存下的，领取**不改**它（SCB-4：以前在这里覆盖成 now+2h，
// 一条 30 秒的片子卡死也要两小时后才被判死）；只有存量任务的死线为空时才补一个。
func (r *Repo) ClaimRender(ctx context.Context, id int64, ffmpegVersion string,
	fallbackDeadline time.Time, maxConcurrent int) (RenderJob, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return RenderJob{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if maxConcurrent > 0 {
		if _, err := tx.Exec(ctx, `select pg_advisory_xact_lock(hashtextextended('render_claim', 0))`); err != nil {
			return RenderJob{}, err
		}
		var running int
		if err := tx.QueryRow(ctx, `select count(*) from render_job where status = 'running'`).Scan(&running); err != nil {
			return RenderJob{}, err
		}
		if running >= maxConcurrent {
			// 先确认它还在排队：已被领走或已是终态的重复消息不该被当成「满了」反复放回队列。
			var st string
			if err := tx.QueryRow(ctx, `select status from render_job where id = $1`, id).Scan(&st); err != nil ||
				st != "queued" {
				return RenderJob{}, ErrRenderBusy
			}
			return RenderJob{}, ErrRenderFull
		}
	}
	tag, err := tx.Exec(ctx, `
		update render_job
		set status = 'running', stage = 'probing', progress = 0,
		    ffmpeg_version = $2, started_at = now(), deadline_at = coalesce(deadline_at, $3), updated_at = now()
		where id = $1 and status = 'queued'`, id, ffmpegVersion, fallbackDeadline)
	if err != nil {
		return RenderJob{}, err
	}
	if tag.RowsAffected() == 0 {
		return RenderJob{}, ErrRenderBusy
	}
	if err := tx.Commit(ctx); err != nil {
		return RenderJob{}, err
	}
	return r.RenderForRun(ctx, id)
}

// RenderStatus 只取状态（Worker 在 ffmpeg 运行期间每秒查一次，被取消就停进程，SCB-5）。
func (r *Repo) RenderStatus(ctx context.Context, id int64) (string, error) {
	var st string
	err := r.pool.QueryRow(ctx, `select status from render_job where id = $1`, id).Scan(&st)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	return st, err
}

// RenderByIdem 按（工作区，幂等键）取已有的任务（SCB-3）。
func (r *Repo) RenderByIdem(ctx context.Context, workspaceID int64, key string) (RenderJob, error) {
	return scanRender(r.pool.QueryRow(ctx, renderCols+`where r.workspace_id = $1 and r.idempotency_key = $2`,
		workspaceID, key))
}

// UserRenderLoad 这个用户排队中 + 渲染中的任务数、近 1 小时受理的任务数（导出按用户限流，SCK-3）。
// 在调用方的事务里、拿到用户锁之后数（见 LockUserRenders）。
func (r *Repo) UserRenderLoad(ctx context.Context, tx pgx.Tx, userID int64) (active, lastHour int,
	oldest time.Time, err error) {
	var oldestPtr *time.Time
	err = tx.QueryRow(ctx, `
		select count(*) filter (where status in ('queued','running')),
		       count(*) filter (where created_at > now() - interval '1 hour'),
		       min(created_at) filter (where created_at > now() - interval '1 hour')
		from render_job where created_by_user_id = $1`, userID).Scan(&active, &lastHour, &oldestPtr)
	if oldestPtr != nil {
		oldest = *oldestPtr
	}
	return active, lastHour, oldest, err
}

// LockUserRenders 事务级 advisory lock，按用户串起「数一数 → 建任务」。
//
// ⚠️ 不锁的话两次并发受理各数到 0、各建一个，「同时只能 1 个」就破了（真并发测试钉住）。
func (r *Repo) LockUserRenders(ctx context.Context, tx pgx.Tx, userID int64) error {
	_, err := tx.Exec(ctx, `select pg_advisory_xact_lock(hashtextextended('render_user:' || $1::bigint::text, 0))`, userID)
	return err
}

// RenderProgress 推进进度与阶段。
//
// ⚠️ 带 `status='running'` 条件：任务被取消之后晚到的进度不能把它拉回 running，
// 否则用户按了取消却看着进度条继续走。
func (r *Repo) RenderProgress(ctx context.Context, id int64, stage string, progress int) error {
	_, err := r.pool.Exec(ctx, `
		update render_job set stage = $2, progress = greatest(progress, $3),
			updated_at = now()
		where id = $1 and status = 'running'`, id, stage, progress)
	return err
}

// FinishRender 标记成功，带上导出后自检（只是警告，SCT-15）。
func (r *Repo) FinishRender(ctx context.Context, id, mediaID int64, selfCheck json.RawMessage) error {
	var check any
	if len(selfCheck) > 0 {
		check = string(selfCheck)
	}
	tag, err := r.pool.Exec(ctx, `
		update render_job set status = 'success', progress = 100, stage = null,
			output_media_id = $2, self_check = $3::jsonb, finished_at = now(), updated_at = now()
		where id = $1 and status = 'running'`, id, mediaID, check)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrRenderBusy
	}
	return nil
}

// FailRender 标记失败。返回是否真的由本次调用改成失败 ——
// 调用方据此决定要不要退款（**退款只能发生一次**）。
func (r *Repo) FailRender(ctx context.Context, id int64, code, msg string) (bool, error) {
	tag, err := r.pool.Exec(ctx, `
		update render_job set status = 'failed', stage = null,
			error_code = $2, error_message = left($3,1024),
			finished_at = now(), updated_at = now()
		where id = $1 and status in ('queued','running')`, id, code, msg)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// CancelRender 用户取消。
//
// ⚠️ 只有 queued/running 能取消，且**返回是否真的取消了** ——
// 终态任务的取消请求必须明确告诉用户"已经跑完了"，
// 而不是静默成功然后让他等一个不会来的取消。
func (r *Repo) CancelRender(ctx context.Context, workspaceID, id int64) (bool, error) {
	tag, err := r.pool.Exec(ctx, `
		update render_job set status = 'cancelled', stage = null,
			error_code = 'cancelled', finished_at = now(), updated_at = now()
		where id = $1 and workspace_id = $2 and status in ('queued','running')`,
		id, workspaceID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// StaleRenders 找出撞了死线还没结束的任务。
//
// ⚠️ 必须有：ffmpeg 可能卡死（畸形输入、磁盘满），而卡着的任务会一直占着
// 并发额度 —— 后面排队的所有渲染都跟着不动，而它们各自看都是"排队中"。
func (r *Repo) StaleRenders(ctx context.Context, limit int) ([]RenderJob, error) {
	rows, err := r.pool.Query(ctx,
		renderCols+`where r.status in ('queued','running')
		and r.deadline_at is not null and r.deadline_at < now()
		order by r.deadline_at limit $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RenderJob
	for rows.Next() {
		j, err := scanRender(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// RunningRenders 当前正在跑的数量（跨 Worker 实例）。
//
// ⚠️ **读库不读进程内计数**：Worker 多实例时进程内各算各的，
// 合起来照样超 —— 而渲染是 CPU 密集的，超了会把整台机器拖垮，
// 连带所有查询接口一起变慢（与生成任务的并发上限同一条）。
func (r *Repo) RunningRenders(ctx context.Context) (int, error) {
	var n int
	err := r.pool.QueryRow(ctx,
		`select count(*) from render_job where status = 'running'`).Scan(&n)
	return n, err
}

// MarkWatermarked 记下这次渲染加了水印。
//
// ⚠️ 与"配置里开没开"是两件事：用户可能自己关了、也可能那会儿平台还没配图。
// 只看配置的话，事后对不上账 —— 而"这个片子怎么没水印"是个会被追问的问题。
func (r *Repo) MarkWatermarked(ctx context.Context, id int64) error {
	_, err := r.pool.Exec(ctx,
		`update render_job set watermark_applied = true where id = $1`, id)
	return err
}

// RenderSetting 是平台级渲染配置（单行）。
type RenderSetting struct {
	WatermarkMediaID int64
	Position         string
	Opacity          int
	Scale            int
	Version          int
}

// RenderSetting 取配置。没有行时回零值（＝不加水印）。
//
// ⚠️ 没配**不是错误**：平台可以先不配 logo。但渲染任务上要记下没加
// （见 MarkWatermarked 的注释）。
func (r *Repo) RenderSetting(ctx context.Context) (RenderSetting, error) {
	var s RenderSetting
	err := r.pool.QueryRow(ctx, `
		select coalesce(watermark_media_id,0), watermark_position,
		       watermark_opacity, watermark_scale, version
		from render_setting where id = $1`, renderSettingID).
		Scan(&s.WatermarkMediaID, &s.Position, &s.Opacity, &s.Scale, &s.Version)
	if errors.Is(err, pgx.ErrNoRows) {
		return RenderSetting{}, nil
	}
	return s, err
}

// renderSettingID 是这张单行配置表的**固定主键**。
//
// ⚠️ 单行表的 id **必须写死**（与 `auth_setting` 同一个做法），
// 不能每次现取雪花 ID：那样 `on conflict (id)` 永远不命中，每保存一次
// 就多一行，而读取用的是 `order by id limit 1` —— 拿到的是**最早**那行。
// 症状是"后台改了配置、保存成功了、渲染出来还是老水印"，
// 而配置页上显示的是新值（它回显的是刚提交的东西）。实测踩到过。
const renderSettingID = 1

// SaveRenderSetting 保存配置（管理端）。单行表：有则更新、无则插入。
func (r *Repo) SaveRenderSetting(ctx context.Context, s RenderSetting,
	adminID int64) error {
	_, err := r.pool.Exec(ctx, `
		insert into render_setting (id, watermark_media_id, watermark_position,
			watermark_opacity, watermark_scale, updated_by_admin_id)
		values ($1, nullif($2,0::bigint), $3, $4, $5, $6)
		on conflict (id) do update set
			watermark_media_id = excluded.watermark_media_id,
			watermark_position = excluded.watermark_position,
			watermark_opacity = excluded.watermark_opacity,
			watermark_scale = excluded.watermark_scale,
			version = render_setting.version + 1,
			updated_by_admin_id = excluded.updated_by_admin_id,
			updated_at = now()`,
		renderSettingID, s.WatermarkMediaID, s.Position, s.Opacity, s.Scale, adminID)
	return err
}

// InsertWork 把成片登记成一件作品。
//
// ⚠️ **进作品库不等于公开发布**：`review_status='pending'`、
// `submitted_at` 为空、`is_featured=false`。公开可见性是
// `approved ∧ featured ∧ ¬offline` —— 三个条件都要运营显式操作。
// 一步就把成片推到首页的话，任何人渲一个片子就上了公开墙。
//
// ⚠️ 幂等靠 `uk_work_source(source_type, source_id)`：
// 重复消息导致的重渲不会产生第二件作品。
func (r *Repo) InsertWork(ctx context.Context, id, workspaceID, renderID,
	timelineID, mediaID int64) error {
	// ⚠️ 标题取时间线名并**在 SQL 里截断**（`work.title` 是 varchar(100)、
	// 时间线名 varchar(64) 现在装得下，但两处长度将来各改各的）——
	// 超长的后果是 22001 让整条登记失败，而片子其实已经出来了。
	_, err := r.pool.Exec(ctx, `
		insert into work (id, workspace_id, source_type, source_id,
			media_object_id, title, review_status)
		select $1,$2,'render',$3,$4, left(t.name,100), 'pending'
		from timeline t where t.id = $5
		on conflict (source_type, source_id) do update set
			media_object_id = excluded.media_object_id, updated_at = now()`,
		id, workspaceID, renderID, mediaID, timelineID)
	return err
}

// RenderTrace 是后台追溯一次渲染时要的全部东西（P10-04）。
type RenderTrace struct {
	Job RenderJob
	// Timeline 渲染时那条时间线的**当前**样子。
	//
	// ⚠️ 与快照里的可能已经不一样 —— 这正是要同时给两份的原因：
	// 排查「片子怎么不对」时，要能看出「当时渲的是什么」与
	// 「现在时间线是什么」的差别。
	Timeline Timeline
	// MediaIDs 快照里用到的全部素材。
	MediaIDs []int64
	// WorkID 成片登记的作品（0 表示还没登记）。
	WorkID int64
}

// TraceRender 后台按渲染 ID 追溯。**不带工作区条件** ——
// 管理员本来就是跨工作区排障的（权限由 `system:read` 把关）。
func (r *Repo) TraceRender(ctx context.Context, id int64) (RenderTrace, error) {
	job, err := r.RenderForRun(ctx, id)
	if err != nil {
		return RenderTrace{}, err
	}
	out := RenderTrace{Job: job}
	// 时间线可能已被删除 —— 那不是错误，只是追溯到此为止。
	if tl, err := scanTimeline(r.pool.QueryRow(ctx,
		timelineCols+`where id = $1`, job.TimelineID)); err == nil {
		out.Timeline = tl
	}
	var snap struct {
		Clips []struct {
			MediaID int64 `json:"mediaId"`
		} `json:"clips"`
		Audio []struct {
			MediaID int64 `json:"mediaId"`
		} `json:"audio"`
		// 新格式（SCT-12）：音频按轨冻结。
		AudioTracks []struct {
			Clips []struct {
				MediaID int64 `json:"mediaId"`
			} `json:"clips"`
		} `json:"audioTracks"`
		Watermark *struct {
			MediaID int64 `json:"mediaId"`
		} `json:"watermark"`
	}
	if err := json.Unmarshal(job.Snapshot, &snap); err == nil {
		seen := map[int64]bool{}
		add := func(id int64) {
			if id != 0 && !seen[id] {
				seen[id] = true
				out.MediaIDs = append(out.MediaIDs, id)
			}
		}
		for _, c := range snap.Clips {
			add(c.MediaID)
		}
		for _, c := range snap.Audio {
			add(c.MediaID)
		}
		for _, t := range snap.AudioTracks {
			for _, c := range t.Clips {
				add(c.MediaID)
			}
		}
		if snap.Watermark != nil {
			add(snap.Watermark.MediaID)
		}
	}
	_ = r.pool.QueryRow(ctx,
		`select id from work where source_type='render' and source_id=$1`, id).
		Scan(&out.WorkID)
	return out, nil
}

// isUniqueViolation 是不是唯一约束冲突（23505）。
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
