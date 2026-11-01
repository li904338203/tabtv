package repo

import (
	"context"
	"time"
)

// 统计口径（MCP-N-09、MCPT-14）。⚠️ 每条查询上面那段注释就是定义；只取 mcp_call_log 与 mcp_quote
// 两张表，不从别处拼 —— 调用审计是 MCP 自己的事实来源，其他表里的「MCP 产出」只是它的结果。
//
// 范围最多 30 天（= mcp_call_log 的保留期）：超过的部分已经被清理了，给出一个更长的范围
// 只会让数字看起来像「这段时间几乎没人用」。夹范围由 app 层做，这里照传入的算。

// StatsFilter 是统计的筛选：时间范围（左闭右开）+ 与连接列表**同一个** GrantFilter（游标与 Limit 不用）。
type StatsFilter struct {
	GrantFilter
	From time.Time
	To   time.Time
}

// ClientCount 是一个客户端名下的调用数。ClientName 为空 = 未知客户端（单列，不丢）。
type ClientCount struct {
	ClientName string
	Calls      int64
	Errors     int64
}

// Stats 是统计的原始计数（比率由 app 层算，-1 表示没有数据）。
type Stats struct {
	// Calls 是范围内的全部调用（任何 result）。
	Calls int64
	// Errors 是 result='error' 的调用。denied / rate_limited / needs_confirm 不算错误：
	// 前两者是我们按规则拒绝的、后者是按设计停下来等用户确认的，并进错误率的话
	// 一个尽职的限流器会让错误率看起来在上升。
	Errors       int64
	Denied       int64
	RateLimited  int64
	NeedsConfirm int64
	// QuotesIssued 是范围内签发的报价（mcp_quote.created_at）；QuotesUsed 是其中已被使用的（used_at 非空）。
	// 「确认通过率」= Used / Issued：签发了却没用掉的，是用户拒绝、过期或客户端放弃了。
	// ⚠️ 因价格变化**作废**的报价同样写了 used_at（detail.voided 非空），不算「通过」——
	// 算进去的话价格波动越频繁，确认通过率反而越高。
	QuotesIssued int64
	QuotesUsed   int64
	// ByClient 按客户端名分组，调用数倒序。⚠️ **各项之和 == Calls**（不截断：截断的部分要算进
	// 「其他」由调用方合并，否则各客户端之和对不上总数，而差额没人解释得了）。
	ByClient []ClientCount
}

// GetStats 算统计。三条查询都 join mcp_grant 并套用 grantWhere —— 与列表同一套条件。
func (r *Repo) GetStats(ctx context.Context, f StatsFilter) (Stats, error) {
	var st Stats
	where, args := grantWhere(f.GrantFilter, []any{f.From, f.To})

	// 调用量 / 错误 / 拒绝 / 限流 / 待确认：按 mcp_call_log.created_at ∈ [from, to)。
	if err := r.pool.QueryRow(ctx, `
		select count(*),
		       count(*) filter (where l.result = 'error'),
		       count(*) filter (where l.result = 'denied'),
		       count(*) filter (where l.result = 'rate_limited'),
		       count(*) filter (where l.result = 'needs_confirm')
		from mcp_call_log l
		join mcp_grant g on g.id = l.grant_id
		left join mcp_client c on c.id = g.client_id
		where l.created_at >= $1 and l.created_at < $2 and `+where, args...).
		Scan(&st.Calls, &st.Errors, &st.Denied, &st.RateLimited, &st.NeedsConfirm); err != nil {
		return Stats{}, err
	}

	// 确认通过率的分子分母：按 mcp_quote.created_at ∈ [from, to)（签发时间，不按使用时间 ——
	// 按使用时间的话，范围边界前签发、边界后使用的报价分子有分母没有，比率可能超过 100%）。
	if err := r.pool.QueryRow(ctx, `
		select count(*), count(*) filter (where q.used_at is not null and coalesce(q.detail->>'voided', '') = '')
		from mcp_quote q
		join mcp_grant g on g.id = q.grant_id
		left join mcp_client c on c.id = g.client_id
		where q.created_at >= $1 and q.created_at < $2 and `+where, args...).
		Scan(&st.QuotesIssued, &st.QuotesUsed); err != nil {
		return Stats{}, err
	}

	// 按客户端：按调用时记下的 client_name 分组（mcp_call_log.client_name 是调用当时的快照，
	// 授权之后改过名的仍归旧名下 —— 这是「这段时间里谁在调」的如实回答）。空名归「未知」。
	rows, err := r.pool.Query(ctx, `
		select coalesce(nullif(l.client_name, ''), ''), count(*), count(*) filter (where l.result = 'error')
		from mcp_call_log l
		join mcp_grant g on g.id = l.grant_id
		left join mcp_client c on c.id = g.client_id
		where l.created_at >= $1 and l.created_at < $2 and `+where+`
		group by 1
		order by 2 desc, 1`, args...)
	if err != nil {
		return Stats{}, err
	}
	defer rows.Close()
	st.ByClient = []ClientCount{}
	for rows.Next() {
		var c ClientCount
		if err := rows.Scan(&c.ClientName, &c.Calls, &c.Errors); err != nil {
			return Stats{}, err
		}
		st.ByClient = append(st.ByClient, c)
	}
	return st, rows.Err()
}
