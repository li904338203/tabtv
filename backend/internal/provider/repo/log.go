// Package repo 落 Provider 请求日志。
package repo

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/li904338203/tabtv/backend/internal/provider/runcfg"
)

type Repo struct {
	pool   *pgxpool.Pool
	nextID func() int64
}

func New(pool *pgxpool.Pool, nextID func() int64) *Repo {
	return &Repo{pool: pool, nextID: nextID}
}

// Entry 是一条 Provider 请求日志。
//
// 直接复用 `runcfg.LogEntry`，于是本仓储**就是**一个 `runcfg.RequestLogger`，
// 两个 main 里不用再写转换适配器。
type Entry = runcfg.LogEntry

// Write 写一条日志。
//
// ⚠️ **写日志失败绝不能影响主流程**：一次日志写不进去就让生成失败，
// 等于把一个观测设施变成了单点故障。调用方拿到 error 只记一条告警。
//
// ⚠️ 入库前必须已经脱敏与截断（transport.RedactJSON / Truncate）——
// 这里不重复做，因为脱敏需要知道内容是 JSON 还是别的形态，
// 而那是调用方才知道的事。
func (r *Repo) Write(ctx context.Context, e Entry) error {
	_, err := r.pool.Exec(ctx, `
		insert into provider_request_log (id, trace_id, generation_id, provider_code,
			model_code, operation_code, url, http_status, error_type, duration_ms,
			request_body, response_body)
		values ($1,nullif($2,''),nullif($3::bigint,0),$4,nullif($5,''),nullif($6,''),
		        left($7,512),nullif($8,0),$9,$10,nullif($11,''),nullif($12,''))`,
		r.nextID(), e.TraceID, e.GenerationID, e.ProviderCode, e.ModelCode,
		e.OperationCode, e.URL, e.HTTPStatus, defaultErrorType(e.ErrorType),
		int(e.Duration.Milliseconds()), e.RequestBody, e.ResponseBody)
	return err
}

// defaultErrorType 兜底：列上有 CHECK 约束，传个没在枚举里的值会整条写不进去。
func defaultErrorType(s string) string {
	switch s {
	case "none", "timeout", "connect", "dns", "http", "stream", "decode":
		return s
	}
	return "none"
}

// CleanupBefore 删除过期日志，返回删除条数。
//
// ⚠️ 这是**会无限增长的表**之一：每次生成都写一条，带着请求体。
// 清理间隔要密（每小时而不是每天）—— 单次删几十万行会长时间持锁，
// 把正在写日志的线程堵住。
func (r *Repo) CleanupBefore(ctx context.Context, before time.Time, limit int) (int64, error) {
	tag, err := r.pool.Exec(ctx, `
		delete from provider_request_log
		where id in (select id from provider_request_log
		             where created_at < $1 order by created_at limit $2)`, before, limit)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// ListLogs 检索 Provider 请求日志（P13-04；AD-07）。
//
// ⚠️ 读在这里而不是让可观测域直接查这张表：表归本域（写在这儿），
// 跨域直接读对方的表结构是本仓库明确禁止的 —— 一旦列改了，编译不会报错，
// 而日志页会在运行时整页打不开。类型走 `runcfg`（只依赖标准库的叶子包），
// 所以两边**不需要任何转换适配器**。
func (r *Repo) ListLogs(ctx context.Context, f runcfg.LogFilter) ([]runcfg.LogRecord, error) {
	args := []any{}
	cond := []string{}
	add := func(v any) string {
		args = append(args, v)
		return "$" + strconv.Itoa(len(args))
	}
	if f.TraceID != "" {
		cond = append(cond, "trace_id = "+add(f.TraceID))
	}
	if f.GenerationID > 0 {
		cond = append(cond, "generation_id = "+add(f.GenerationID))
	}
	if f.ProviderCode != "" {
		cond = append(cond, "provider_code = "+add(f.ProviderCode))
	}
	if f.ModelCode != "" {
		cond = append(cond, "model_code = "+add(f.ModelCode))
	}
	if f.FailedOnly {
		// ⚠️ 按 error_type 不按 http_status：上游的业务错误常藏在 200 里，
		// 而超时/断连根本没有状态码。
		cond = append(cond, "error_type <> 'none'")
	}
	if !f.From.IsZero() {
		cond = append(cond, "created_at >= "+add(f.From))
	}
	if !f.To.IsZero() {
		cond = append(cond, "created_at < "+add(f.To)) // 左闭右开
	}
	if f.CursorID > 0 {
		cond = append(cond, "id < "+add(f.CursorID))
	}
	where := ""
	if len(cond) > 0 {
		where = " where " + strings.Join(cond, " and ")
	}
	limit := add(f.Limit)

	rows, err := r.pool.Query(ctx, `
		select id, coalesce(trace_id,''), coalesce(generation_id,0), provider_code,
		       coalesce(model_code,''), coalesce(operation_code,''), url,
		       coalesce(http_status,0), error_type, duration_ms,
		       coalesce(request_body,''), coalesce(response_body,''), created_at
		  from provider_request_log`+where+`
		 order by id desc limit `+limit, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []runcfg.LogRecord
	for rows.Next() {
		var rec runcfg.LogRecord
		var ms int
		if err := rows.Scan(&rec.ID, &rec.TraceID, &rec.GenerationID, &rec.ProviderCode,
			&rec.ModelCode, &rec.OperationCode, &rec.URL, &rec.HTTPStatus,
			&rec.ErrorType, &ms, &rec.RequestBody, &rec.ResponseBody,
			&rec.CreatedAt); err != nil {
			return nil, err
		}
		rec.Duration = time.Duration(ms) * time.Millisecond
		out = append(out, rec)
	}
	return out, rows.Err()
}
