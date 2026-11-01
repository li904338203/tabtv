package repo

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// ErrNotClaimable 表示任务不在可领取状态（已被别的 Worker 领走、已取消、已完成）。
var ErrNotClaimable = errors.New("任务不可领取")

// ErrConcurrencyLimit 表示该付费账户的运行中任务已达上限。
var ErrConcurrencyLimit = errors.New("并发已达上限")

// ClaimForRun 领取一个任务：queued → running，**带状态条件的更新**（CAS）。
//
// ⚠️ 三件事必须在同一条语句/同一个事务里：
//
//  1. **CAS**：`where status='queued'` 让重复消息的第二次领取影响 0 行 ——
//     消费幂等靠这个，不靠"先查再改"（查和改之间别的 Worker 就领走了）。
//  2. **并发上限跨实例生效**：在库里数 running 条数，不能用单进程的 goroutine 数
//     代替用户级限制（GO-22）—— Worker 是可以水平扩容的，进程内的计数各数各的。
//  3. 记 started_at 与 heartbeat_at，供恢复扫描判断"卡住多久了"。
func (r *Repo) ClaimForRun(ctx context.Context, id int64, maxRunningPerPayer int) (
	Generation, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return Generation{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// 先锁住这一行，避免并发领取时两个事务都读到 queued。
	var payerID, modelID int64
	var status string
	err = tx.QueryRow(ctx,
		`select payer_account_id, model_id, status from generation where id = $1 for update`, id).
		Scan(&payerID, &modelID, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return Generation{}, ErrNotFound
	}
	if err != nil {
		return Generation{}, err
	}
	if status != "queued" {
		return Generation{}, ErrNotClaimable
	}

	if maxRunningPerPayer > 0 {
		var running int
		if err := tx.QueryRow(ctx, `
			select count(*) from generation
			where payer_account_id = $1 and status = 'running'`, payerID).Scan(&running); err != nil {
			return Generation{}, err
		}
		if running >= maxRunningPerPayer {
			// ⚠️ 达到上限**不是失败**：任务留在 queued，消息 nack 重回队列稍后再试。
			// 判失败的话，用户批量提交十个任务会有七个直接失败。
			return Generation{}, ErrConcurrencyLimit
		}
	}

	// 供应商级并发（P06-03）。
	//
	// ⚠️ 付费主体级护的是用户之间的公平，护不住**上游**：一个工作区把十个任务
	// 全压到同一家 Provider 上，就会撞上对方的账号级并发限制，换回来一批 429。
	// 那批任务按「可重试」放回队列，下一轮又一起冲上去 —— 退避只在单个任务上生效，
	// 一整批同时退避等于同时重试，问题原样复现。
	//
	// ⚠️ 上限**读库不读进程内配置**：Worker 有多个实例，进程内计数各算各的，
	// 合起来照样超。同理这里数的是"库里此刻 running 的行数"，跨实例天然一致。
	var limit, providerRunning int
	err = tx.QueryRow(ctx, `
		select p.max_concurrent,
		       (select count(*) from generation g
		          join model gm on gm.id = g.model_id
		         where gm.provider_id = p.id and g.status = 'running')
		from model m join provider_config p on p.id = m.provider_id
		where m.id = $1`, modelID).Scan(&limit, &providerRunning)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// 查不到模型/Provider（被删了）。⚠️ **不在这里拦**：并发上限是节流，
		// 不是校验入口。拦下来的话任务会一直卡在 queued 且没有任何错误信息，
		// 而积分已经扣了。照常放行，让适配器去报"取模型配置失败" ——
		// 那条路会判永久失败并退款，用户至少知道发生了什么。
	case err != nil:
		return Generation{}, err
	case limit > 0 && providerRunning >= limit:
		return Generation{}, ErrConcurrencyLimit
	}

	g, err := scanOne(tx.QueryRow(ctx, selectCols+`
		from generation where id = $1`, id))
	if err != nil {
		return Generation{}, err
	}
	if _, err := tx.Exec(ctx, `
		update generation
		set status = 'running', started_at = coalesce(started_at, now()),
		    heartbeat_at = now(), attempt = attempt + 1, updated_at = now()
		where id = $1 and status = 'queued'`, id); err != nil {
		return Generation{}, err
	}
	g.Status = "running"
	return g, tx.Commit(ctx)
}

// RecordProviderTask 落库上游 task ID。
//
// ⚠️ 这一步与"发出提交请求"之间就是 GO-14 的**不确定窗口**：
// 请求已经到了上游、我们却还没记下 task ID。崩在这里的任务会带着
// `attempt>0 且 provider_task_id is null` 被恢复扫描捞出来专门处理，
// **绝不无条件重发**。
func (r *Repo) RecordProviderTask(ctx context.Context, id int64, taskID string) error {
	_, err := r.pool.Exec(ctx, `
		update generation set provider_task_id = $2, heartbeat_at = now(), updated_at = now()
		where id = $1 and status = 'running'`, id, taskID)
	return err
}

// Heartbeat 刷新心跳，让恢复扫描知道这个任务还活着。
func (r *Repo) Heartbeat(ctx context.Context, id int64, progress int) error {
	_, err := r.pool.Exec(ctx, `
		update generation set heartbeat_at = now(),
		    progress = greatest(progress, $2), updated_at = now()
		where id = $1 and status = 'running'`, id, progress)
	return err
}

// FinishInput 是一次终态写入。
type FinishInput struct {
	ID           int64
	Status       string
	ErrorCode    string
	ErrorMessage string
	// ErrorDetail 上游原始报错（只给管理端与日志）；ErrorMessage 是给用户看的文案。
	ErrorDetail string
	RawResult   json.RawMessage
	Progress    int
}

// Finish 把任务推进到终态，**带当前状态条件**。
//
// 返回 false 表示没有影响任何行 —— 说明任务已经是终态了（晚到的结果、重复消息）。
// ⚠️ 调用方据此**跳过退款与通知**：重复退款是账本最怕的事，
// 而"成功的产物被晚到的失败覆盖"是用户最怕的事（GO-18、FLOW-04）。
func (r *Repo) Finish(ctx context.Context, in FinishInput) (bool, error) {
	tag, err := r.pool.Exec(ctx, `
		update generation
		set status = $2, error_code = nullif($3,''), error_message = nullif($4,''),
		    provider_raw_result = $5, progress = $6, error_detail = nullif($7,''),
		    finished_at = now(), updated_at = now()
		where id = $1 and status in ('queued','running')`,
		in.ID, in.Status, in.ErrorCode, in.ErrorMessage, in.RawResult, in.Progress, in.ErrorDetail)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// ReleaseToQueue 把任务放回队列（并发达上限、可重试失败）。
//
// ⚠️ 只针对 running，且**不清空 attempt** —— 清空等于把重试次数上限作废。
func (r *Repo) ReleaseToQueue(ctx context.Context, id int64) (bool, error) {
	tag, err := r.pool.Exec(ctx, `
		update generation set status = 'queued', heartbeat_at = null, updated_at = now()
		where id = $1 and status = 'running'`, id)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// StaleKind 是恢复扫描捞出的任务类型。
type StaleKind string

const (
	// StaleExpired 已过死线。
	StaleExpired StaleKind = "expired"
	// StaleUncertain 提交不确定窗口：跑过但没有上游 task ID。
	StaleUncertain StaleKind = "uncertain"
	// StaleOrphanQueued 受理成功但一直没被领取（投递丢了、Worker 全挂）。
	StaleOrphanQueued StaleKind = "orphan_queued"
)

// StaleTask 是一条待恢复的任务。
type StaleTask struct {
	Generation
	Kind StaleKind
}

// FindStale 捞出需要恢复处理的任务。
//
// ⚠️ 用**存下来的** deadline_at 判超时，不按当前 est 重算（GO-24）：
// 管理员调大 est 之后，一批本该判超时的僵尸任务会突然"又活了"。
func (r *Repo) FindStale(ctx context.Context, orphanAfter time.Duration, limit int) (
	[]StaleTask, error) {
	rows, err := r.pool.Query(ctx, selectCols+`,
		case
			when deadline_at is not null and deadline_at <= now() then 'expired'
			when status = 'running' and provider_task_id is null then 'uncertain'
			else 'orphan_queued'
		end as kind
		from generation
		where status in ('queued','running')
		  and (
		    (deadline_at is not null and deadline_at <= now())
		    or (status = 'running' and provider_task_id is null
		        and heartbeat_at is not null and heartbeat_at < now() - $1::interval)
		    or (status = 'queued' and created_at < now() - $1::interval)
		  )
		order by created_at
		limit $2`, orphanAfter.String(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StaleTask
	for rows.Next() {
		var s StaleTask
		var kind string
		// ⚠️ 复用 scanTargets，不再自己抄一份列表：抄的那份漏掉新加的列时
		// 恢复扫描会静默失效（只打一条日志），而超时任务永远不会被判失败。
		if err := rows.Scan(append(scanTargets(&s.Generation), &kind)...); err != nil {
			return nil, err
		}
		s.Kind = StaleKind(kind)
		out = append(out, s)
	}
	return out, rows.Err()
}

// FindUnrefundedFailures 捞出**扣了费、失败了、却没有退款流水**的任务（CD-82）。
//
// ⚠️ 失败路径上的退款失败只留一行日志（任务状态已经落定，不能为退款把它变回 running），
// 各处注释都说「由对账作业补」—— 而那个作业此前**并不存在**：一段超长的退款原因
// 让 6 个失败任务的 288 积分一直没退回去，没有任何东西是红的。这条查询就是那个作业的输入。
//
// `settleAfter`：刚失败的任务给失败路径自己的退款留一点时间，免得和它抢（抢到也无妨，账本唯一索引兜底）。
// 只看最近 7 天：正常情况下一分钟内就补上了，更早的需要人工核对而不是自动补。
func (r *Repo) FindUnrefundedFailures(ctx context.Context, settleAfter time.Duration, limit int) (
	[]Generation, error) {
	rows, err := r.pool.Query(ctx, selectCols+`
		from generation g
		where g.status = 'failed' and g.credits_cost > 0
		  and g.updated_at < now() - $1::interval
		  and g.updated_at > now() - interval '7 days'
		  and exists (select 1 from credit_transaction t
		               where t.ref_type = 'generation' and t.ref_id = g.id::text and t.type = 'consume')
		  and not exists (select 1 from credit_transaction t
		                   where t.ref_type = 'generation' and t.ref_id = g.id::text and t.type = 'refund')
		order by g.updated_at
		limit $2`, settleAfter.String(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Generation
	for rows.Next() {
		var g Generation
		if err := rows.Scan(scanTargets(&g)...); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// CountRunning 供观测与限流查询。
func (r *Repo) CountRunning(ctx context.Context, payerID int64) (int, error) {
	var n int
	err := r.pool.QueryRow(ctx, `
		select count(*) from generation
		where payer_account_id = $1 and status = 'running'`, payerID).Scan(&n)
	return n, err
}
