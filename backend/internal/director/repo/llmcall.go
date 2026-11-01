// Package repo 是 3D 导演台 AI 搭景的调用记录仓储（docs/agent/director，DRT-19）。
//
// AI 搭景免费（DRK-4），这张表不是账本：它只回答「谁、什么时候、用哪个模型、成没成」，
// 并承担限流（DRD-23）。场景与媒体记录归画布域存（DRD-11），不在这里。
package repo

import (
	"context"
	"errors"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/li904338203/tabtv/backend/internal/platform/pgtext"
)

// Repo 是 director_llm_call 的仓储。
type Repo struct{ pool *pgxpool.Pool }

func New(pool *pgxpool.Pool) *Repo { return &Repo{pool: pool} }

// 调用记录的状态（ck_director_llm_call_status）。
const (
	StatusRunning   = "running"
	StatusSucceeded = "succeeded"
	StatusFailed    = "failed"
)

// ErrorStale 是恢复扫描判失败时写的错误码。
const ErrorStale = "stale"

// maxErrorMessage 是 error_message 的列宽（varchar(500)）。
const maxErrorMessage = 500

// ErrNotFound 表示记录不存在。
var ErrNotFound = errors.New("调用记录不存在")

// LLMCall 是一次 AI 搭景调用。
type LLMCall struct {
	ID          int64
	WorkspaceID int64
	ProjectID   int64
	SceneID     int64
	UserID      int64
	// Source 是发起方：web / agent / mcp（ck_director_llm_call_source）。
	Source         string
	AgentSessionID int64
	McpGrantID     int64
	ModelCode      string
	PromptChars    int
	ImageCount     int
}

// Limits 是限流的两条线（DR-PARAM-07，设计建议值）。0 表示不限。
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

// LimitDecision 是一次限流判断的结果。Reject 为空时调用记录已经插入（running）。
type LimitDecision struct {
	Reject Reject
	// RetryAfter 是小时额度被拒时，最早能再发起的等待时间（向上取整到秒，至少 1 秒）。
	RetryAfter time.Duration
	// Running / UsedThisHour 是**插入之前**的计数。
	Running      int
	UsedThisHour int
}

// InsertWithLimits 在**一个事务**里限流并插入一条进行中的调用记录（DRD-23）。
//
// ⚠️ 计数与插入必须在同一把锁里：先数再插的两步在并发下会一起放行（两个请求都看到「0 个进行中」，
// 然后各插一条）。按用户取事务级 advisory lock，同一用户的搭景在这里排队，别的用户不受影响。
// ⚠️ 限流读库不读进程内计数：API 多实例时各算各的，合起来照样超（同生成的并发上限）。
func (r *Repo) InsertWithLimits(ctx context.Context, call LLMCall, lim Limits) (LimitDecision, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return LimitDecision{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx,
		`select pg_advisory_xact_lock(hashtextextended('director_build:' || $1::bigint::text, 0))`,
		call.UserID); err != nil {
		return LimitDecision{}, err
	}
	var d LimitDecision
	if err := tx.QueryRow(ctx, `
		select count(*) filter (where status = 'running'),
		       count(*) filter (where created_at > now() - interval '1 hour')
		from director_llm_call where user_id = $1 and (status = 'running' or created_at > now() - interval '1 hour')`,
		call.UserID).Scan(&d.Running, &d.UsedThisHour); err != nil {
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
			from director_llm_call
			where user_id = $1 and created_at > now() - interval '1 hour'
			order by created_at offset $2 limit 1`,
			call.UserID, d.UsedThisHour-lim.PerHour).Scan(&secs); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return LimitDecision{}, err
		}
		d.RetryAfter = time.Duration(math.Max(1, math.Ceil(secs))) * time.Second
		return d, nil
	}
	if _, err := tx.Exec(ctx, `
		insert into director_llm_call (id, workspace_id, project_id, scene_id, user_id, source,
			agent_session_id, mcp_grant_id, model_code, prompt_chars, image_count)
		values ($1, $2, $3, $4, $5, $6, nullif($7::bigint, 0), nullif($8::bigint, 0), $9, $10, $11)`,
		call.ID, call.WorkspaceID, call.ProjectID, call.SceneID, call.UserID, call.Source,
		call.AgentSessionID, call.McpGrantID, call.ModelCode, call.PromptChars, call.ImageCount); err != nil {
		return LimitDecision{}, err
	}
	return d, tx.Commit(ctx)
}

// FinishInput 是一次调用的结局。
type FinishInput struct {
	Status       string // succeeded / failed
	ErrorCode    string
	ErrorMessage string
	ObjectCount  int
	DurationMs   int
}

// Finish 结束一条进行中的调用，返回「这一次是否真的改了状态」。
//
// ⚠️ 带 `status = 'running'` 条件（CAS）：恢复扫描可能已经抢先判了失败，晚到的结局不能把它改回成功。
// ⚠️ 报错原文按字截断（pgtext.Fit，CD-82）：超长的话整条更新失败，记录永远停在 running，
// 而它占着的并发额度要等 20 分钟后的恢复扫描才放出来。
func (r *Repo) Finish(ctx context.Context, id int64, in FinishInput) (bool, error) {
	tag, err := r.pool.Exec(ctx, `
		update director_llm_call set status = $2, error_code = nullif($3, ''), error_message = nullif($4, ''),
			object_count = nullif($5::int, 0), duration_ms = $6, finished_at = now()
		where id = $1 and status = 'running'`,
		id, in.Status, pgtext.Fit(in.ErrorCode, 32), pgtext.Fit(in.ErrorMessage, maxErrorMessage),
		in.ObjectCount, in.DurationMs)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// Usage 是给可用性查询（build-config）的计数：只读，不插记录。
func (r *Repo) Usage(ctx context.Context, userID int64) (running, usedThisHour int, err error) {
	err = r.pool.QueryRow(ctx, `
		select count(*) filter (where status = 'running'),
		       count(*) filter (where created_at > now() - interval '1 hour')
		from director_llm_call where user_id = $1 and (status = 'running' or created_at > now() - interval '1 hour')`,
		userID).Scan(&running, &usedThisHour)
	return running, usedThisHour, err
}

// RecoverStale 把「进行中」超过 olderThan 的调用判失败（错误码 stale），返回条数。
//
// 请求被中断、进程重启时，调用会停在 running，并发额度被一直占着。判死阈值必须**大于**长耗时路由的上限
// （15 分钟）：否则一个合法地还在等模型的请求会被判死，随后又写回成功（Finish 的 CAS 会挡住，但用户已经看到失败）。
// 免费，所以没有退款分支（同脚本域的恢复扫描，只少了退款）。
func (r *Repo) RecoverStale(ctx context.Context, olderThan time.Duration, limit int) (int, error) {
	tag, err := r.pool.Exec(ctx, `
		update director_llm_call set status = 'failed', error_code = $3,
			error_message = '调用超时未结束（请求被中断或服务重启）', finished_at = now()
		where id in (select id from director_llm_call
			where status = 'running' and created_at < now() - make_interval(secs => $1)
			order by created_at limit $2)
			and status = 'running'`,
		olderThan.Seconds(), limit, ErrorStale)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

// DeleteOlderThan 分批删除 before 之前的调用记录（保留 30 天，DR-PARAM-16），返回这一批删了几条。
//
// ⚠️ 一批最多 1000 条（调用方循环）：一次删几十万行会长时间持锁，把正在写调用记录的搭景请求堵住
// （同请求日志的清理）。进行中的不删：它还占着并发额度，删了就再也结束不了。
func (r *Repo) DeleteOlderThan(ctx context.Context, before time.Time, limit int) (int, error) {
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	tag, err := r.pool.Exec(ctx, `
		delete from director_llm_call where id in (
			select id from director_llm_call where created_at < $1 and status <> 'running'
			order by created_at limit $2)`, before, limit)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

// Get 按 ID 取一条调用记录（测试与排障用）。
func (r *Repo) Get(ctx context.Context, id int64) (LLMCall, string, error) {
	var c LLMCall
	var status string
	err := r.pool.QueryRow(ctx, `
		select id, workspace_id, project_id, scene_id, user_id, source, coalesce(agent_session_id, 0),
			coalesce(mcp_grant_id, 0), model_code, prompt_chars, image_count, status
		from director_llm_call where id = $1`, id).Scan(&c.ID, &c.WorkspaceID, &c.ProjectID, &c.SceneID,
		&c.UserID, &c.Source, &c.AgentSessionID, &c.McpGrantID, &c.ModelCode, &c.PromptChars, &c.ImageCount, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return LLMCall{}, "", ErrNotFound
	}
	return c, status, err
}
