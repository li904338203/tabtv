// Package repo 是智能剪辑 AI 调用的记录仓储（docs/agent/smartcut SCT-27，SCD-24）。
//
// AI 剪辑、追加指令、字幕翻译都免费（SCK-2），这张表不是账本：它回答「谁、什么时候、用哪个模型、走到哪一步、
// 出了几套方案、用没用兜底」，并承担限流（同一用户同时 1 次、每小时 20 次，多方案一次请求计 1 次）。
// 时间线与快照归时间线域存，不在这里。
package repo

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/li904338203/tabtv/backend/internal/platform/pgtext"
)

// Repo 是 smartcut_llm_call 的仓储。
type Repo struct{ pool *pgxpool.Pool }

func New(pool *pgxpool.Pool) *Repo { return &Repo{pool: pool} }

// 调用记录的状态（ck_sc_call_status）。
const (
	StatusRunning = "running"
	StatusSuccess = "success"
	StatusFailed  = "failed"
)

// 调用的种类（ck_sc_call_kind）。
const (
	KindPlan      = "plan"
	KindEdit      = "edit"
	KindTranslate = "translate"
)

// 阶段（ck_sc_call_stage）：同步素材 → 理解素材 → 规划 → 编译 → 写入 → 结束。
const (
	StageSync       = "sync"
	StageUnderstand = "understand"
	StagePlan       = "plan"
	StageCompile    = "compile"
	StageApply      = "apply"
	StageDone       = "done"
)

// StaleMessage 是恢复扫描判失败时写的原因。
const StaleMessage = "调用超时未结束（请求被中断或服务重启）"

// maxErrorMessage 是 error_message 的列宽（varchar(1024)）。
const maxErrorMessage = 1024

// ErrNotFound 表示记录不存在（或不在这条时间线上）。
var ErrNotFound = errors.New("调用记录不存在")

// Call 是一次调用（插入时的字段）。
type Call struct {
	ID           int64
	WorkspaceID  int64
	ProjectID    int64
	TimelineID   int64
	CanvasNodeID string
	UserID       int64
	// Source 是发起方：web / agent / mcp（ck_sc_call_source）。
	Source           string
	Kind             string
	Mode             string
	Instruction      string
	Spec             json.RawMessage
	VariantCount     int
	InputFingerprint string
	ModelCode        string
	BaseVersion      int
}

// Limits 是限流的两条线（SC-PARAM-19，设计建议值）。0 表示不限。
type Limits struct {
	// Concurrency 同一用户同时进行的调用数。
	Concurrency int
	// PerHour 同一用户近 1 小时的调用次数（失败的也算：上游的额度照样用掉了）。
	PerHour int
}

// Reject 是限流的结论。
type Reject string

const (
	RejectNone        Reject = ""
	RejectConcurrency Reject = "concurrency"
	RejectHourly      Reject = "hourly"
)

// LimitDecision 是一次限流判断的结果。Reject 为空时调用记录已经插入（running、stage=sync）。
type LimitDecision struct {
	Reject Reject
	// RetryAfter 是小时额度被拒时，最早能再发起的等待时间（向上取整到秒，至少 1 秒）。
	RetryAfter time.Duration
	// Running / UsedThisHour 是**插入之前**的计数。
	Running      int
	UsedThisHour int
}

// InsertWithLimits 在**一个事务**里限流并插入一条进行中的调用记录（SCD-24）。
//
// ⚠️ 计数与插入必须在同一把锁里：先数再插的两步在并发下会一起放行（两个请求都看到「0 个进行中」，然后各插一条）。
// 按用户取事务级 advisory lock，同一用户的 AI 剪辑在这里排队，别的用户不受影响。
// ⚠️ 限流读库不读进程内计数：API 多实例时各算各的，合起来照样超。
// ⚠️ 多方案（variants 2~3）一次请求只插这一行，计 1 次（SC-F-87）。
func (r *Repo) InsertWithLimits(ctx context.Context, c Call, lim Limits) (LimitDecision, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return LimitDecision{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx,
		`select pg_advisory_xact_lock(hashtextextended('smartcut_plan:' || $1::bigint::text, 0))`, c.UserID); err != nil {
		return LimitDecision{}, err
	}
	var d LimitDecision
	if err := tx.QueryRow(ctx, `
		select count(*) filter (where status = 'running'),
		       count(*) filter (where created_at > now() - interval '1 hour')
		from smartcut_llm_call where user_id = $1 and (status = 'running' or created_at > now() - interval '1 hour')`,
		c.UserID).Scan(&d.Running, &d.UsedThisHour); err != nil {
		return LimitDecision{}, err
	}
	if lim.Concurrency > 0 && d.Running >= lim.Concurrency {
		d.Reject = RejectConcurrency
		return d, nil
	}
	if lim.PerHour > 0 && d.UsedThisHour >= lim.PerHour {
		d.Reject = RejectHourly
		// 要等到窗口里足够多的旧调用滑出去：现有 n 条、上限 p 条时，第 (n−p+1) 旧的那条滑出之后才低于上限。
		var secs float64
		if err := tx.QueryRow(ctx, `
			select extract(epoch from (created_at + interval '1 hour' - now()))
			from smartcut_llm_call
			where user_id = $1 and created_at > now() - interval '1 hour'
			order by created_at offset $2 limit 1`,
			c.UserID, d.UsedThisHour-lim.PerHour).Scan(&secs); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return LimitDecision{}, err
		}
		d.RetryAfter = time.Duration(math.Max(1, math.Ceil(secs))) * time.Second
		return d, nil
	}
	variants := max(c.VariantCount, 1)
	if _, err := tx.Exec(ctx, `
		insert into smartcut_llm_call (id, workspace_id, project_id, timeline_id, canvas_node_id, user_id, source,
			kind, mode, instruction, spec, variant_count, input_fingerprint, model_code, base_version)
		values ($1, $2, $3, $4, nullif($5, ''), $6, $7, $8, nullif($9, ''), nullif($10, ''), $11, $12,
			nullif($13, ''), nullif($14, ''), nullif($15::int, 0))`,
		c.ID, c.WorkspaceID, c.ProjectID, c.TimelineID, c.CanvasNodeID, c.UserID, c.Source, c.Kind, c.Mode,
		c.Instruction, nullJSON(c.Spec), variants, c.InputFingerprint, c.ModelCode, c.BaseVersion); err != nil {
		return LimitDecision{}, err
	}
	return d, tx.Commit(ctx)
}

func nullJSON(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	return []byte(raw)
}

// SetStage 推进一条进行中的调用的阶段；返回是否真的改了（已结束的不改）。
func (r *Repo) SetStage(ctx context.Context, id int64, stage string) (bool, error) {
	tag, err := r.pool.Exec(ctx, `update smartcut_llm_call set stage = $2 where id = $1 and status = 'running'`, id, stage)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// SetModel 记下这次用的模型与素材指纹（选模型在限流之后、调用之前）。
func (r *Repo) SetModel(ctx context.Context, id int64, modelCode, fingerprint string) error {
	_, err := r.pool.Exec(ctx, `
		update smartcut_llm_call set model_code = nullif($2, ''), input_fingerprint = coalesce(nullif($3, ''), input_fingerprint)
		where id = $1 and status = 'running'`, id, modelCode, fingerprint)
	return err
}

// FinishInput 是一次调用的结局。
type FinishInput struct {
	Status string // success / failed
	// Result 各套方案（含解析后的方案与编译摘要，方案切换时据此重新编译）。
	Result         json.RawMessage
	AppliedVariant *int
	UsedFallback   bool
	ErrorMessage   string
	DurationMs     int
}

// Finish 结束一条进行中的调用，返回「这一次是否真的改了状态」。
//
// ⚠️ 带 `status = 'running'` 条件（CAS）：恢复扫描可能已经抢先判了失败，晚到的结局不能把它改回成功。
// ⚠️ 报错原文按字截断（pgtext.Fit，CD-82）：超长的话整条更新失败，记录永远停在 running，
// 而它占着的并发额度要等 20 分钟后的恢复扫描才放出来。
func (r *Repo) Finish(ctx context.Context, id int64, in FinishInput) (bool, error) {
	tag, err := r.pool.Exec(ctx, `
		update smartcut_llm_call set status = $2, stage = 'done', result = coalesce($3, result),
			applied_variant = $4, used_fallback = $5, error_message = nullif($6, ''), duration_ms = $7, finished_at = now()
		where id = $1 and status = 'running'`,
		id, in.Status, nullJSON(in.Result), in.AppliedVariant, in.UsedFallback,
		pgtext.Fit(pgtext.Safe(in.ErrorMessage), maxErrorMessage), in.DurationMs)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// SetApplied 方案切换之后记下当前写进时间线的是哪一套（只认已结束的成功调用）。
func (r *Repo) SetApplied(ctx context.Context, id int64, variant int) error {
	_, err := r.pool.Exec(ctx, `update smartcut_llm_call set applied_variant = $2 where id = $1 and status = 'success'`,
		id, variant)
	return err
}

// Record 是一条已有的调用（方案切换、排障、摘要用）。
type Record struct {
	Call
	Status         string
	Stage          string
	Result         json.RawMessage
	AppliedVariant *int
	UsedFallback   bool
	ErrorMessage   string
	CreatedAt      time.Time
}

// Get 取本工作区、这条时间线上的一条调用；不在（含别的时间线的）时返回 ErrNotFound。
func (r *Repo) Get(ctx context.Context, workspaceID, timelineID, id int64) (Record, error) {
	var rec Record
	var spec, result []byte
	err := r.pool.QueryRow(ctx, `
		select id, workspace_id, project_id, timeline_id, coalesce(canvas_node_id, ''), user_id, source, kind,
			coalesce(mode, ''), coalesce(instruction, ''), spec, variant_count, coalesce(input_fingerprint, ''),
			coalesce(model_code, ''), coalesce(base_version, 0), status, stage, result, applied_variant, used_fallback,
			coalesce(error_message, ''), created_at
		from smartcut_llm_call where id = $1 and workspace_id = $2 and timeline_id = $3`,
		id, workspaceID, timelineID).Scan(&rec.ID, &rec.WorkspaceID, &rec.ProjectID, &rec.TimelineID, &rec.CanvasNodeID,
		&rec.UserID, &rec.Source, &rec.Kind, &rec.Mode, &rec.Instruction, &spec, &rec.VariantCount,
		&rec.InputFingerprint, &rec.ModelCode, &rec.BaseVersion, &rec.Status, &rec.Stage, &result, &rec.AppliedVariant,
		&rec.UsedFallback, &rec.ErrorMessage, &rec.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Record{}, ErrNotFound
	}
	rec.Spec, rec.Result = spec, result
	return rec, err
}

// Usage 是给可用性查询（/smartcut/config）的计数：只读，不插记录。
func (r *Repo) Usage(ctx context.Context, userID int64) (running, usedThisHour int, err error) {
	err = r.pool.QueryRow(ctx, `
		select count(*) filter (where status = 'running'),
		       count(*) filter (where created_at > now() - interval '1 hour')
		from smartcut_llm_call where user_id = $1 and (status = 'running' or created_at > now() - interval '1 hour')`,
		userID).Scan(&running, &usedThisHour)
	return running, usedThisHour, err
}

// Active 是一条时间线上进行中的调用（摘要的 activeCall）。
type Active struct {
	ID        int64
	Kind      string
	Stage     string
	StartedAt time.Time
}

// ActiveCalls 每条时间线最近一条进行中的调用（摘要一次取全）。
func (r *Repo) ActiveCalls(ctx context.Context, workspaceID int64, timelineIDs []int64) (map[int64]Active, error) {
	out := map[int64]Active{}
	if len(timelineIDs) == 0 {
		return out, nil
	}
	rows, err := r.pool.Query(ctx, `
		select distinct on (timeline_id) timeline_id, id, kind, stage, created_at
		from smartcut_llm_call
		where workspace_id = $1 and timeline_id = any($2) and status = 'running'
		order by timeline_id, created_at desc, id desc`, workspaceID, timelineIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var tid int64
		var a Active
		if err := rows.Scan(&tid, &a.ID, &a.Kind, &a.Stage, &a.StartedAt); err != nil {
			return nil, err
		}
		out[tid] = a
	}
	return out, rows.Err()
}

// RecoverStale 把「进行中」超过 olderThan 的调用判失败，返回条数。
//
// 请求被中断、进程重启时，调用会停在 running，并发额度被一直占着。判死阈值必须**大于**长耗时路由的上限
// （15 分钟）：否则一个合法地还在等模型的请求会被判死，随后又写回成功（Finish 的 CAS 会挡住，但用户已经看到失败）。
// 免费，所以没有退款分支（同导演台的恢复扫描）。
func (r *Repo) RecoverStale(ctx context.Context, olderThan time.Duration, limit int) (int, error) {
	tag, err := r.pool.Exec(ctx, `
		update smartcut_llm_call set status = 'failed', stage = 'done', error_message = $3, finished_at = now()
		where id in (select id from smartcut_llm_call
			where status = 'running' and created_at < now() - make_interval(secs => $1)
			order by created_at limit $2)
			and status = 'running'`,
		olderThan.Seconds(), limit, StaleMessage)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

// DeleteOlderThan 分批删除 before 之前的调用记录（保留 30 天，SC-PARAM-22），返回这一批删了几条。
//
// ⚠️ 一批最多 1000 条（调用方循环）：一次删几十万行会长时间持锁，把正在写调用记录的请求堵住。
// 进行中的不删：它还占着并发额度，删了就再也结束不了。
func (r *Repo) DeleteOlderThan(ctx context.Context, before time.Time, limit int) (int, error) {
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	tag, err := r.pool.Exec(ctx, `
		delete from smartcut_llm_call where id in (
			select id from smartcut_llm_call where created_at < $1 and status <> 'running'
			order by created_at limit $2)`, before, limit)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}
