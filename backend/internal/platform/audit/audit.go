// Package audit 记录业务审计（数据模型 §13）。
//
// ⚠️ 与请求日志是**两件事**，故意分表（GAP-24）：
//   - 请求日志回答「谁调了什么接口」，量大、可清理；
//   - 审计回答「什么对象从什么值变成了什么值、为什么」，是合规面，**不自动删**。
//
// 退款、配置发布、审核、调账、封禁、代编辑都必须写审计。
package audit

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

type ActorType string

const (
	ActorUser   ActorType = "user"
	ActorAdmin  ActorType = "admin"
	ActorSystem ActorType = "system"
)

type Entry struct {
	ActorType   ActorType
	ActorID     int64
	WorkspaceID int64
	Action      string
	ObjectType  string
	ObjectID    int64
	Before      any
	After       any
	Reason      string
}

type Logger struct {
	pool   *pgxpool.Pool
	nextID func() int64
	log    *slog.Logger
}

func New(pool *pgxpool.Pool, nextID func() int64, log *slog.Logger) *Logger {
	return &Logger{pool: pool, nextID: nextID, log: log}
}

// Write 记录一条审计。
//
// ⚠️ **失败只告警不阻断主流程**。审计写失败而让退款或封禁回滚，
// 是用一个次要问题制造一个更严重的问题。失败会留在应用日志里可查。
// 反过来说：审计表的可用性要有监控，长期写失败必须被发现。
func (l *Logger) Write(ctx context.Context, e Entry) {
	before, err1 := marshal(e.Before)
	after, err2 := marshal(e.After)
	if err1 != nil || err2 != nil {
		l.log.Error("审计内容序列化失败", "action", e.Action, "objectType", e.ObjectType)
		return
	}

	_, err := l.pool.Exec(ctx, `
		insert into audit_log
		  (id, actor_type, actor_id, workspace_id, action, object_type, object_id,
		   before_value, after_value, reason, trace_id)
		-- ⚠️ 必须显式 ::bigint。裸写 nullif($3,0) 会让 PG 按字面量 0 推断成 int4，
		-- 雪花 ID 直接溢出。这个错误曾让审计静默写不进去（操作全部成功、审计表为空）。
		values ($1,$2,nullif($3::bigint,0),nullif($4::bigint,0),$5,$6,nullif($7::bigint,0),$8,$9,nullif($10,''),nullif($11,''))`,
		l.nextID(), e.ActorType, e.ActorID, e.WorkspaceID, e.Action,
		e.ObjectType, e.ObjectID, before, after, e.Reason, httpx.TraceIDFrom(ctx))
	if err != nil {
		l.log.Error("写审计失败", "error", err,
			"action", e.Action, "objectType", e.ObjectType, "objectId", e.ObjectID,
			"traceId", httpx.TraceIDFrom(ctx))
	}
}

func marshal(v any) ([]byte, error) {
	if v == nil {
		return nil, nil
	}
	return json.Marshal(v)
}

// Query 是审计查询条件。
type Query struct {
	ObjectType string
	ObjectID   int64
	ActorType  string
	ActorID    int64
	Action     string
	// From / To 是**左闭右开**的时间范围（P13-04）。
	//
	// ⚠️ 没有它的话，「上个月谁动过这个订单」只能靠一直翻页去找 ——
	// 而审计最常见的问题恰恰带着一个时间点（"上周三出的事"）。
	From, To time.Time
	// CursorID 游标（上一页最后一条的 id）。
	//
	// ⚠️ 用游标不用 offset：审计一直在新增，offset 翻页会漏掉或重复
	// （CD-56 同一条）—— 而漏掉的那一条恰恰可能是要找的。
	CursorID int64
	Limit    int
}

type Record struct {
	ID         string          `json:"id"`
	ActorType  string          `json:"actorType"`
	ActorID    string          `json:"actorId,omitempty"`
	Action     string          `json:"action"`
	ObjectType string          `json:"objectType"`
	ObjectID   string          `json:"objectId,omitempty"`
	Before     json.RawMessage `json:"before,omitempty"`
	After      json.RawMessage `json:"after,omitempty"`
	Reason     string          `json:"reason,omitempty"`
	TraceID    string          `json:"traceId,omitempty"`
	CreatedAt  string          `json:"createdAt"`
}

func (l *Logger) List(ctx context.Context, q Query) ([]Record, error) {
	if q.Limit <= 0 || q.Limit > 200 {
		q.Limit = 50
	}
	// ⚠️ 排序改成按 **id 倒序**而不是 created_at：游标分页要求排序键唯一，
	// 而同一毫秒内写进来的几条审计（一次事务里连着写的）时间戳可能相同 ——
	// 按 created_at 翻页会在那几条上漏或重。id 是雪花号，天然按时间递增。
	rows, err := l.pool.Query(ctx, `
		select id, actor_type, coalesce(actor_id,0), action, object_type,
		       coalesce(object_id,0), before_value, after_value,
		       coalesce(reason,''), coalesce(trace_id,''), created_at
		from audit_log
		where ($1 = '' or object_type = $1)
		  and ($2::bigint = 0  or object_id = $2::bigint)
		  and ($3 = '' or actor_type = $3)
		  and ($4::bigint = 0  or actor_id = $4::bigint)
		  and ($5 = '' or action = $5)
		  and ($6::timestamptz is null or created_at >= $6::timestamptz)
		  and ($7::timestamptz is null or created_at <  $7::timestamptz)
		  and ($8::bigint = 0 or id < $8::bigint)
		order by id desc
		limit $9`,
		q.ObjectType, q.ObjectID, q.ActorType, q.ActorID, q.Action,
		nullTime(q.From), nullTime(q.To), q.CursorID, q.Limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Record
	for rows.Next() {
		var r Record
		var id, actorID, objectID int64
		var createdAt time.Time
		var before, after []byte
		if err := rows.Scan(&id, &r.ActorType, &actorID, &r.Action, &r.ObjectType,
			&objectID, &before, &after, &r.Reason, &r.TraceID, &createdAt); err != nil {
			return nil, err
		}
		r.ID = itoa(id)
		if actorID != 0 {
			r.ActorID = itoa(actorID)
		}
		if objectID != 0 {
			r.ObjectID = itoa(objectID)
		}
		r.Before, r.After = before, after
		r.CreatedAt = createdAt.UTC().Format("2006-01-02T15:04:05Z")
		out = append(out, r)
	}
	return out, rows.Err()
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	return string(b[i:])
}

// nullTime 把零值时间变成 SQL 的 null（"没传这个条件"）。
//
// ⚠️ 不能直接把零值 time.Time 传下去：那是公元 1 年，
// 条件会变成"从公元 1 年起"——看起来一切正常，实际把范围过滤整条作废了。
func nullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}
