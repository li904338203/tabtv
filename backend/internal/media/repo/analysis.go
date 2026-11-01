package repo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// 分析缓存 `media_analysis`（docs/agent/smartcut SCT-16，SCD-30）：一行一个（媒体，种类，引擎，参数）。
//
// ⚠️ 排队与进行中也唯一（`uk_media_analysis … where status <> 'failed'`）：只在 success 上唯一的话，
// 两次同步会各排一行、各转一遍码。failed 不在唯一范围里，所以失败之后可以重新排队。

// Analysis 是一行分析。
type Analysis struct {
	ID             int64
	WorkspaceID    int64
	MediaObjectID  int64
	Kind           string
	Engine         string
	ParamsKey      string
	Status         string
	Attempts       int
	LeaseUntil     *time.Time
	Result         json.RawMessage
	DerivedMediaID int64
	GenerationID   int64
	ErrorMessage   string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// AnalysisKey 是排队的缓存键（不含工作区：工作区取媒体自己的）。
type AnalysisKey struct {
	MediaID   int64
	Kind      string
	Engine    string
	ParamsKey string
}

// EnqueueAnalyses 排进一批分析，返回新排进的行数；已在排队 / 进行中 / 成功的跳过。
//
// ⚠️ 归属在 SQL 里：只排本工作区、未删除的媒体（`media_object` 连接条件），
// 调用方传进别人的媒体 ID 只会被静默跳过，不会替别人转码。
func (r *Repo) EnqueueAnalyses(ctx context.Context, nextID func() int64, workspaceID int64,
	keys []AnalysisKey) (int, error) {
	if len(keys) == 0 {
		return 0, nil
	}
	ids := make([]int64, len(keys))
	media := make([]int64, len(keys))
	kinds := make([]string, len(keys))
	engines := make([]string, len(keys))
	params := make([]string, len(keys))
	for i, k := range keys {
		ids[i], media[i], kinds[i], engines[i], params[i] = nextID(), k.MediaID, k.Kind, k.Engine, k.ParamsKey
	}
	tag, err := r.pool.Exec(ctx, `
		insert into media_analysis (id, workspace_id, media_object_id, kind, engine, params_key)
		select k.id, m.workspace_id, m.id, k.kind, k.engine, k.params_key
		from unnest($2::bigint[], $3::bigint[], $4::text[], $5::text[], $6::text[])
		       as k(id, media_id, kind, engine, params_key)
		join media_object m on m.id = k.media_id and m.workspace_id = $1 and m.status = 'active'
		on conflict (media_object_id, kind, engine, params_key) where status <> 'failed' do nothing`,
		workspaceID, ids, media, kinds, engines, params)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

// ErrNoAnalysis 表示没有可领取的分析。
var ErrNoAnalysis = errors.New("没有待处理的分析")

const analysisCols = `id, workspace_id, media_object_id, kind, engine, params_key, status, attempts,
	lease_until, result, coalesce(derived_media_id,0), coalesce(generation_id,0), coalesce(error_message,''),
	created_at, updated_at`

func scanAnalysis(row pgx.Row) (Analysis, error) {
	var a Analysis
	err := row.Scan(&a.ID, &a.WorkspaceID, &a.MediaObjectID, &a.Kind, &a.Engine, &a.ParamsKey, &a.Status,
		&a.Attempts, &a.LeaseUntil, &a.Result, &a.DerivedMediaID, &a.GenerationID, &a.ErrorMessage,
		&a.CreatedAt, &a.UpdatedAt)
	return a, err
}

// ClaimAnalysis 领取一行：排队中且到了重试时间的，或租约已过期的进行中的；改成 running、写租约、attempts + 1。
//
// ⚠️ 一条 SQL、`for update skip locked`：两个扫描器同时来领，各拿到不同的行，不会互相等，也不会领到同一行。
// 「先查再改」分两步的话，两个实例会在中间那一刻看到同一行都是 queued。
// ⚠️ 排队行上的 `lease_until` 表示「不早于这个时间重试」（失败后的退避），进行中行上表示租约。
// ⚠️ 只领 attempts 还没满的：满了的过期行由 ReapAnalyses 判失败（见那里的说明）。
func (r *Repo) ClaimAnalysis(ctx context.Context, kinds []string, lease time.Duration,
	maxAttempts int) (Analysis, error) {
	a, err := scanAnalysis(r.pool.QueryRow(ctx, `
		update media_analysis a
		   set status = 'running', lease_until = now() + $2::interval, attempts = a.attempts + 1, updated_at = now()
		 where a.id = (
		       select id from media_analysis
		        where kind = any($1) and attempts < $3
		          and ((status = 'queued' and (lease_until is null or lease_until <= now()))
		               or (status = 'running' and lease_until < now()))
		        order by created_at, id
		        for update skip locked
		        limit 1)
		   -- 外层再判一次（第二道闸）：子查询的结果只算一次，行被并发改过之后 PG 只会重查外层条件 ——
		   -- 少了这一条，锁子句一旦被改丢，等锁的那个会把别人刚领走的行再领一遍。
		   and (a.status = 'queued' or a.lease_until < now())
		returning `+analysisCols, kinds, interval(lease), maxAttempts))
	if errors.Is(err, pgx.ErrNoRows) {
		return Analysis{}, ErrNoAnalysis
	}
	return a, err
}

// ReapAnalyses 把「已经试满次数、最后一次又中途断掉（租约过期）」的行判失败，返回判了几行。
//
// ⚠️ 进程被杀、机器重启都不会写失败原因 —— 不封顶的话，一份能把 Worker 撑爆的文件会在
// 「领取 → 崩溃 → 租约过期 → 再领取」之间无限循环。
func (r *Repo) ReapAnalyses(ctx context.Context, kinds []string, maxAttempts int) (int, error) {
	tag, err := r.pool.Exec(ctx, `
		update media_analysis
		   set status = 'failed', lease_until = null, updated_at = now(),
		       error_message = left(coalesce(nullif(error_message,'') || '；', '') ||
		                            '处理中断 ' || attempts || ' 次（进程退出或超时），不再自动重试', 512)
		 where kind = any($1) and status = 'running' and lease_until < now() and attempts >= $2`,
		kinds, maxAttempts)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

// RenewAnalysisLease 续租约（执行者活着就一直续）。返回 false 表示这一行已经不归自己了
// （被接管、被判失败、被删），调用方要立即停手。
//
// ⚠️ 条件里带 attempts：租约过期被别的实例接管后 attempts 已经 + 1，原执行者的续期与写回都不再成立。
func (r *Repo) RenewAnalysisLease(ctx context.Context, id int64, attempts int, lease time.Duration) (bool, error) {
	tag, err := r.pool.Exec(ctx, `
		update media_analysis set lease_until = now() + $3::interval, updated_at = now()
		 where id = $1 and status = 'running' and attempts = $2`, id, attempts, interval(lease))
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// CompleteAnalysis 写成功与结果。返回 false 表示这一行已经不归自己了（晚到的写回作废）。
func (r *Repo) CompleteAnalysis(ctx context.Context, id int64, attempts int, result json.RawMessage,
	derivedMediaID int64) (bool, error) {
	tag, err := r.pool.Exec(ctx, `
		update media_analysis
		   set status = 'success', result = $3::jsonb, derived_media_id = nullif($4, 0::bigint),
		       error_message = null, lease_until = null, updated_at = now()
		 where id = $1 and status = 'running' and attempts = $2`, id, attempts, string(result), derivedMediaID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// FailAnalysis 记一次失败：还没试满就放回排队并退避 retryAfter，试满了（或 permanent）判 failed。
// 返回最终状态；""表示这一行已经不归自己了。
func (r *Repo) FailAnalysis(ctx context.Context, id int64, attempts int, msg string, retryAfter time.Duration,
	maxAttempts int, permanent bool) (string, error) {
	var status string
	err := r.pool.QueryRow(ctx, `
		update media_analysis
		   set status = case when $5 or attempts >= $4 then 'failed' else 'queued' end,
		       lease_until = case when $5 or attempts >= $4 then null else now() + $6::interval end,
		       error_message = left($3, 512), updated_at = now()
		 where id = $1 and status = 'running' and attempts = $2
		returning status`, id, attempts, msg, maxAttempts, permanent, interval(retryAfter)).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return status, err
}

// ListAnalyses 列出一份媒体的全部分析行（新的在前），归属在 SQL 里。
func (r *Repo) ListAnalyses(ctx context.Context, workspaceID, mediaID int64) ([]Analysis, error) {
	rows, err := r.pool.Query(ctx, `select `+analysisCols+`
		from media_analysis where media_object_id = $1 and workspace_id = $2
		order by created_at desc, id desc`, mediaID, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Analysis
	for rows.Next() {
		a, err := scanAnalysis(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// GetAnalysis 按 ID 取一行（测试与排障用）。
func (r *Repo) GetAnalysis(ctx context.Context, id int64) (Analysis, error) {
	a, err := scanAnalysis(r.pool.QueryRow(ctx, `select `+analysisCols+` from media_analysis where id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Analysis{}, ErrNotFound
	}
	return a, err
}

// interval 把时长写成 PG 能直接转 interval 的字符串（微秒精度足够）。
func interval(d time.Duration) string {
	return fmt.Sprintf("%d microseconds", d.Microseconds())
}

// ReleaseAnalysis 进程退出时把正在做的那一行放回排队，并退回这一次的计数（不是它的错）。
// 放不回去也无妨：租约过期后别的实例会接管。
func (r *Repo) ReleaseAnalysis(ctx context.Context, id int64, attempts int) error {
	_, err := r.pool.Exec(ctx, `
		update media_analysis
		   set status = 'queued', attempts = greatest(attempts - 1, 0), lease_until = null, updated_at = now()
		 where id = $1 and status = 'running' and attempts = $2`, id, attempts)
	return err
}

// LatestResults 每份媒体某种分析最近一次成功的结果（归属在 SQL 里）。engine 为空时不限引擎
// （识别由模型码区分引擎，规划只关心「有没有识别结果」）。查不到的不在结果里。
func (r *Repo) LatestResults(ctx context.Context, workspaceID int64, mediaIDs []int64, kind, engine string) (
	map[int64]json.RawMessage, error) {
	out := map[int64]json.RawMessage{}
	if len(mediaIDs) == 0 {
		return out, nil
	}
	rows, err := r.pool.Query(ctx, `
		select distinct on (a.media_object_id) a.media_object_id, a.result
		from media_analysis a
		where a.workspace_id = $1 and a.media_object_id = any($2) and a.kind = $3 and a.status = 'success'
		  and ($4 = '' or a.engine = $4) and a.result is not null
		order by a.media_object_id, a.updated_at desc, a.id desc`, workspaceID, mediaIDs, kind, engine)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var raw []byte
		if err := rows.Scan(&id, &raw); err != nil {
			return nil, err
		}
		out[id] = raw
	}
	return out, rows.Err()
}

// SaveCaption 写一份看图描述（kind=caption、engine=模型码，docs/agent/smartcut SCT-27）：已有的覆盖成最新的。
// 归属在 SQL 里：不是本工作区、已删除的媒体静默跳过。
func (r *Repo) SaveCaption(ctx context.Context, id, workspaceID, mediaID int64, engine string, result json.RawMessage) error {
	_, err := r.pool.Exec(ctx, `
		insert into media_analysis (id, workspace_id, media_object_id, kind, engine, params_key, status, result)
		select $1, m.workspace_id, m.id, 'caption', $4, '', 'success', $5
		from media_object m where m.id = $3 and m.workspace_id = $2 and m.status = 'active'
		on conflict (media_object_id, kind, engine, params_key) where status <> 'failed'
		do update set result = excluded.result, status = 'success', updated_at = now()`,
		id, workspaceID, mediaID, engine, []byte(result))
	return err
}
