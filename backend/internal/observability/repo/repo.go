// Package repo 是可观测域的数据访问层（P13-04）。
package repo

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/li904338203/tabtv/backend/internal/platform/logredact"
)

type Repo struct {
	pool   *pgxpool.Pool
	nextID func() int64
}

func New(pool *pgxpool.Pool, nextID func() int64) *Repo {
	return &Repo{pool: pool, nextID: nextID}
}

func itoa(v int) string { return strconv.Itoa(v) }

// ---------------------------------------------------------------- API 请求日志

// APILog 是一条 API 请求日志。
type APILog struct {
	ID          int64
	TraceID     string
	Method      string
	Path        string
	HTTPStatus  int
	BizCode     int
	DurationMs  int
	UserID      int64
	AdminID     int64
	WorkspaceID int64
	IP          string
	RequestBody string
	CreatedAt   time.Time
}

// WriteAPILog 落一条 API 请求日志。
//
// ⚠️ **写日志失败绝不能影响主流程**（与 Provider 日志同一条）：
// 一次日志写不进去就让请求失败，等于把观测设施变成单点故障。
// 调用方拿到 error 只记一条告警。
//
// ⚠️ `request_body` 入库前必须已经脱敏与截断（`logredact.RedactJSON`）——
// 这里不重复做：脱敏需要知道内容是 JSON 还是别的形态，而那是调用方才知道的。
// `path` 在 SQL 里 `left(...,512)`：超长的 query string 会让整条 insert 失败，
// 而那时丢的是一条**本来最该看**的日志（超长 URL 往往就是出问题的那次）。
func (r *Repo) WriteAPILog(ctx context.Context, l APILog) error {
	_, err := r.pool.Exec(ctx, `
		insert into api_request_log (id, trace_id, method, path, http_status,
			biz_code, duration_ms, user_id, admin_id, workspace_id, ip, request_body)
		values ($1,$2,$3,left($4,512),$5,nullif($6,0),$7,
		        nullif($8::bigint,0),nullif($9::bigint,0),nullif($10::bigint,0),
		        nullif($11,''),nullif($12,''))`,
		r.nextID(), l.TraceID, l.Method, l.Path, l.HTTPStatus, l.BizCode,
		l.DurationMs, l.UserID, l.AdminID, l.WorkspaceID,
		logredact.SafeText(l.IP), l.RequestBody)
	return err
}

// APILogFilter 限定 API 日志检索。
type APILogFilter struct {
	// TraceID 是排障的第一入口：用户报障时给的就是它。
	TraceID string
	Method  string
	// Path 前缀匹配（"这个接口今天出过多少错"）。
	Path string
	// FailedOnly 只看 4xx/5xx。
	FailedOnly bool
	// Keyword 一个框查三样：userId / adminId / workspaceId。
	Keyword  string
	From, To time.Time
	Limit    int
	CursorID int64
}

const apiLogCols = `id, trace_id, method, path, http_status, coalesce(biz_code,0),
	duration_ms, coalesce(user_id,0), coalesce(admin_id,0),
	coalesce(workspace_id,0), coalesce(ip,''), coalesce(request_body,''), created_at`

// ListAPILogs 检索 API 请求日志。
func (r *Repo) ListAPILogs(ctx context.Context, f APILogFilter) ([]APILog, error) {
	args := []any{}
	cond := []string{}
	add := func(v any) string {
		args = append(args, v)
		return "$" + itoa(len(args))
	}
	if f.TraceID != "" {
		cond = append(cond, "trace_id = "+add(f.TraceID))
	}
	if f.Method != "" {
		cond = append(cond, "method = "+add(strings.ToUpper(f.Method)))
	}
	if p := strings.TrimSpace(f.Path); p != "" {
		// ⚠️ 前缀匹配并转义元字符：路径来自后台输入框，而后台账号权限最大。
		// 一个 `%` 不转义就能拖出全表。
		cond = append(cond, "path like "+add(escapeLike(p)+"%")+" escape '\\'")
	}
	if f.FailedOnly {
		cond = append(cond, "http_status >= 400")
	}
	if id := ParseID(f.Keyword); id > 0 {
		p := add(id)
		cond = append(cond, "(user_id = "+p+" or admin_id = "+p+" or workspace_id = "+p+")")
	}
	if !f.From.IsZero() {
		cond = append(cond, "created_at >= "+add(f.From))
	}
	if !f.To.IsZero() {
		// 左闭右开（见 domain.Range）。
		cond = append(cond, "created_at < "+add(f.To))
	}
	if f.CursorID > 0 {
		cond = append(cond, "id < "+add(f.CursorID))
	}
	where := ""
	if len(cond) > 0 {
		where = " where " + strings.Join(cond, " and ")
	}
	limit := add(f.Limit)

	rows, err := r.pool.Query(ctx, `select `+apiLogCols+` from api_request_log`+
		where+` order by id desc limit `+limit, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []APILog
	for rows.Next() {
		var l APILog
		if err := rows.Scan(&l.ID, &l.TraceID, &l.Method, &l.Path, &l.HTTPStatus,
			&l.BizCode, &l.DurationMs, &l.UserID, &l.AdminID, &l.WorkspaceID,
			&l.IP, &l.RequestBody, &l.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// CleanupAPILogsBefore 删除过期的 API 日志，返回删除条数。
//
// ⚠️ `api_request_log` 与 `provider_request_log` 是**唯二会无限增长的表**
// （每次请求/每次上游调用各一条，还带着请求体）。清理间隔要**密**
// （每小时而不是每天）：单次删几十万行会长时间持锁，把正在写日志的线程堵住
// —— 而那时表现为**所有接口都变慢**，看起来完全不像日志的问题。
// 分批删（`limit`）是同一个理由。
func (r *Repo) CleanupAPILogsBefore(ctx context.Context, before time.Time, limit int) (int64, error) {
	tag, err := r.pool.Exec(ctx, `
		delete from api_request_log
		where id in (select id from api_request_log
		             where created_at < $1 order by created_at limit $2)`, before, limit)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func escapeLike(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "%", "\\%")
	return strings.ReplaceAll(s, "_", "\\_")
}

// ParseID 把关键词当雪花号解析；不是纯数字时回 0（表示"这不是个 ID"）。
//
// ⚠️ 解析失败**不报错**：为一个格式问题把整个列表挡掉不值。
func ParseID(s string) int64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	var v int64
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0
		}
		v = v*10 + int64(c-'0')
		if v < 0 {
			return 0
		}
	}
	return v
}
