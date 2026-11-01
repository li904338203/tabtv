// Package repo 是任务运维域的数据访问层（P13-03）。
package repo

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/li904338203/tabtv/backend/internal/ops/domain"
	"github.com/li904338203/tabtv/backend/internal/platform/pgtext"
)

var ErrNotFound = errors.New("资源不存在")

type Repo struct {
	pool   *pgxpool.Pool
	nextID func() int64
}

func New(pool *pgxpool.Pool, nextID func() int64) *Repo {
	return &Repo{pool: pool, nextID: nextID}
}

func itoa(v int) string { return strconv.Itoa(v) }

// ---------------------------------------------------------------- 死信

// DeadLetter 是一条落库的死信。
type DeadLetter struct {
	ID            int64
	Queue         string
	RoutingKey    string
	Payload       string
	AggregateType string
	AggregateID   int64
	Reason        string
	Status        domain.DeadStatus
	ReplayCount   int
	HandledBy     int64
	HandledAt     *time.Time
	Note          string
	CreatedAt     time.Time
}

const deadCols = `id, queue, routing_key, payload, coalesce(aggregate_type,''),
	coalesce(aggregate_id,0), reason, status, replay_count,
	coalesce(handled_by_admin_id,0), handled_at, coalesce(note,''), created_at`

func scanDead(row pgx.Row) (DeadLetter, error) {
	var d DeadLetter
	err := row.Scan(&d.ID, &d.Queue, &d.RoutingKey, &d.Payload, &d.AggregateType,
		&d.AggregateID, &d.Reason, &d.Status, &d.ReplayCount, &d.HandledBy,
		&d.HandledAt, &d.Note, &d.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return DeadLetter{}, ErrNotFound
	}
	return d, err
}

// Record 落一条死信。重复投递靠部分唯一索引收敛。
//
// ⚠️ payload 与 reason 都要过 `pgtext.Safe`：上游/broker 给的字节
// 可能含 `0x00`，而 PG 的 text 列**拒收**它 —— 不清洗的话整条 insert 回滚，
// 排空器判失败、消息回队列、再试一次、再失败……**一条毒消息会让排空器
// 永远转圈**，而后面所有死信都排不进来。
//
// ⚠️ 返回「是不是新的一条」：重复投递（at-least-once 的正常结局）不该在
// 日志里刷成告警，而**真的**新增一条死信是要有人看的。
func (r *Repo) Record(ctx context.Context, d DeadLetter) (DeadLetter, bool, error) {
	var aggType any
	if d.AggregateType != "" {
		aggType = d.AggregateType
	}
	var aggID any
	if d.AggregateID != 0 {
		aggID = d.AggregateID
	}
	row := r.pool.QueryRow(ctx, `
		insert into dead_letter (id, queue, routing_key, payload,
			aggregate_type, aggregate_id, reason)
		values ($1,$2,$3,$4,$5,$6,$7)
		on conflict (queue, md5(payload)) where status = 'pending' do nothing
		returning `+deadCols,
		r.nextID(), d.Queue, pgtext.Safe(d.RoutingKey),
		pgtext.Safe(d.Payload), aggType, aggID, pgtext.Safe(d.Reason))
	out, err := scanDead(row)
	if errors.Is(err, ErrNotFound) {
		// 冲突：已有一条同样的待处理死信。不是错误。
		return DeadLetter{}, false, nil
	}
	if err != nil {
		return DeadLetter{}, false, err
	}
	return out, true, nil
}

// DeadFilter 限定死信列表。
type DeadFilter struct {
	Status   domain.DeadStatus
	Queue    string
	Limit    int
	CursorID int64
}

// ListDead 列出死信，按 id 倒序游标分页。
func (r *Repo) ListDead(ctx context.Context, f DeadFilter) ([]DeadLetter, error) {
	args := []any{}
	cond := []string{}
	if f.Status != "" {
		args = append(args, string(f.Status))
		cond = append(cond, "status = $"+itoa(len(args)))
	}
	if f.Queue != "" {
		args = append(args, f.Queue)
		cond = append(cond, "queue = $"+itoa(len(args)))
	}
	if f.CursorID > 0 {
		args = append(args, f.CursorID)
		cond = append(cond, "id < $"+itoa(len(args)))
	}
	where := ""
	if len(cond) > 0 {
		where = " where " + strings.Join(cond, " and ")
	}
	args = append(args, f.Limit)
	rows, err := r.pool.Query(ctx, `select `+deadCols+` from dead_letter`+where+
		` order by id desc limit $`+itoa(len(args)), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DeadLetter
	for rows.Next() {
		d, err := scanDead(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// CountDeadPending 统计待处理条数，供运维台顶上的红点。
func (r *Repo) CountDeadPending(ctx context.Context) (int, error) {
	var n int
	err := r.pool.QueryRow(ctx,
		`select count(*)::int from dead_letter where status = 'pending'`).Scan(&n)
	return n, err
}

func (r *Repo) GetDead(ctx context.Context, id int64) (DeadLetter, error) {
	return scanDead(r.pool.QueryRow(ctx,
		`select `+deadCols+` from dead_letter where id = $1`, id))
}

// MarkDead 用 **CAS** 把一条死信从 pending 推到终态。
//
// ⚠️ 必须带 `status = 'pending'`：两个运维同时点重投时只有一个成功 ——
// 不带的话两条消息都会被投出去，而其中一类死信重投两次就是让上游计两次费。
//
// 返回 0 行即"别人先动了"，调用方一律按冲突处理，**不能当成功**。
func (r *Repo) MarkDead(ctx context.Context, id int64, next domain.DeadStatus,
	adminID int64, note string, bumpReplay bool) (DeadLetter, error) {
	inc := 0
	if bumpReplay {
		inc = 1
	}
	row := r.pool.QueryRow(ctx, `
		update dead_letter
		   set status = $2, handled_by_admin_id = $3, note = nullif($4,''),
		       handled_at = now(), replay_count = replay_count + $5,
		       updated_at = now()
		 where id = $1 and status = 'pending'
		returning `+deadCols, id, string(next), adminID, note, inc)
	return scanDead(row)
}

// AppendDeadNote 在已处置的死信上补一句话（例如"重投时投递失败"）。
//
// ⚠️ 这是**追加事实不是改判**：状态不回退。重投已经标过了，投递失败时
// 把它退回 pending 的话，另一个运维会再投一次 —— 而第一次可能其实投出去了。
func (r *Repo) AppendDeadNote(ctx context.Context, id int64, note string) error {
	_, err := r.pool.Exec(ctx, `
		update dead_letter
		   set note = left(coalesce(note || ' / ', '') || $2, 255), updated_at = now()
		 where id = $1`, id, pgtext.Safe(note))
	return err
}
