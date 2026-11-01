package repo

import (
	"context"
	"strings"
	"time"

	"github.com/li904338203/tabtv/backend/internal/content/domain"
)

// 后台任务运营台（P13-03；GAP-07、AD-08）。
//
// ⚠️ 这里与用户侧的任务中心**共用同一个 union**（`taskUnionSQL`），
// 只是换了租户条件、多挑几列。另写一份 union 的话，新增一个状态词时会
// 漏掉一处 —— 而漏掉的症状是**某一类任务在那个筛选下整体消失**，
// 没有任何报错（CD-62 已经记过一次）。
//
// ⚠️ 它放在内容域，是因为「四类任务的统一读模型」本来就在这里。
// 运维台是这个读模型的**第二个消费者**，不是第二份实现。
// 编排（重投、补偿）在 `internal/ops` —— 那些会改钱和状态，不属于内容域。

// OpsTask 是运营台的一行：在用户侧那几列之上，多了排障真正要看的东西。
type OpsTask struct {
	Task
	WorkspaceID    int64
	PayerAccountID int64
	CreatedByUser  int64
	// RawStatus 是**各表自己的原始状态**。统一状态回答"给用户怎么显示"，
	// 原始状态回答"库里到底是什么" —— 排障问的是后者。
	RawStatus    string
	Attempt      int
	ProviderCode string
	ModelCode    string
	ErrorCode    string
	// ErrorDetail 上游原话（只有生成任务有）。用户侧的 Error 是归类后的说法，原话只给运维看。
	ErrorDetail string
	DeadlineAt  *time.Time
	FinishedAt  *time.Time
}

// OpsTaskFilter 限定运营台检索。
type OpsTaskFilter struct {
	Kind  domain.TaskKind
	State domain.TaskState
	// Keyword 一个框查三样：任务 ID / 工作区 ID / 付费主体 ID。
	//
	// ⚠️ 运维手上拿到的可能是任意一个（用户报障给的是任务 ID，
	// 财务给的是付费主体，客服给的是工作区）。分成三个框的话他得先判断
	// "这串数字是什么" —— **而那正是他来查的原因**（同后台订单检索）。
	Keyword      string
	ProviderCode string
	StuckOnly    bool
	Limit        int
	CursorAt     *time.Time
	CursorID     int64
}

// ListOpsTasks 后台跨租户检索。调用方必须已经过 `ops:read`。
func (r *Repo) ListOpsTasks(ctx context.Context, f OpsTaskFilter, now time.Time) (
	[]OpsTask, error) {
	args := []any{}
	cond := []string{}
	add := func(v any) string {
		args = append(args, v)
		return "$" + itoa(len(args))
	}
	if f.Kind != "" {
		cond = append(cond, "kind = "+add(string(f.Kind)))
	}
	if f.State != "" {
		cond = append(cond, "state = "+add(string(f.State)))
	}
	if f.ProviderCode != "" {
		cond = append(cond, "provider_code = "+add(f.ProviderCode))
	}
	if id := parseID(f.Keyword); id > 0 {
		// ⚠️ **精确匹配**：这三列都是雪花号，前缀匹配没有意义且会全表扫。
		// ⚠️ 一律**参数化**，绝不把关键词拼成字面量 —— 它来自后台输入框，
		// 而后台账号恰恰权限最大。
		p := add(id)
		cond = append(cond, "(id = "+p+" or workspace_id = "+p+" or payer_account_id = "+p+")")
	}
	if f.StuckOnly {
		// 「卡住」= 还没结束 且 死线已过。
		//
		// ⚠️ 不能只看"还没结束"：排队中的任务本来就没结束，那样筛出来的是
		// 整个待办队列，而运维要找的是**本该结束却没结束**的那几条。
		// ⚠️ 也不能靠一个作业去标记 —— 作业停一天，运维台就有一整天看不到
		// 新卡住的任务（与提示词 ready/stale、导出过期同一条）。
		cond = append(cond,
			"state in ('queued','running') and deadline_at is not null and deadline_at < "+
				add(now))
	}
	if f.CursorAt != nil {
		a := add(*f.CursorAt)
		b := add(f.CursorID)
		cond = append(cond, "(created_at < "+a+" or (created_at = "+a+" and id < "+b+"))")
	}
	where := ""
	if len(cond) > 0 {
		where = " where " + strings.Join(cond, " and ")
	}
	limit := add(f.Limit)

	rows, err := r.pool.Query(ctx, `
		with merged as (`+taskUnionSQL(allTenants())+`)
		select id, kind, state, title, credits, err, created_at, ref_id,
		       workspace_id, payer_account_id, created_by_user_id,
		       raw_status, attempt, provider_code, model_code, err_code, err_detail,
		       deadline_at, finished_at
		from merged`+where+`
		order by created_at desc, id desc limit `+limit, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OpsTask
	for rows.Next() {
		var t OpsTask
		if err := rows.Scan(&t.ID, &t.Kind, &t.State, &t.Title, &t.Credits,
			&t.Error, &t.CreatedAt, &t.RefID, &t.WorkspaceID, &t.PayerAccountID,
			&t.CreatedByUser, &t.RawStatus, &t.Attempt, &t.ProviderCode,
			&t.ModelCode, &t.ErrorCode, &t.ErrorDetail, &t.DeadlineAt, &t.FinishedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// OpsSummary 是运营台顶上的几个数。
//
// ⚠️ 与列表用**同一套过滤条件**（除了状态本身）：分开算的话运维看到的
// "共 132 条失败"与下面列出来的不是同一批 —— **而两个数字都"没错"**
// （后台订单汇总踩过同一条，CD-56）。
type OpsSummary struct {
	ByState map[domain.TaskState]int
	// Stuck 是「本该结束却没结束」的条数，运维台最该盯的那个数。
	Stuck int
}

// SummarizeOpsTasks 统计各状态条数与卡住条数。
func (r *Repo) SummarizeOpsTasks(ctx context.Context, f OpsTaskFilter, now time.Time) (
	OpsSummary, error) {
	args := []any{now}
	cond := []string{}
	add := func(v any) string {
		args = append(args, v)
		return "$" + itoa(len(args))
	}
	if f.Kind != "" {
		cond = append(cond, "kind = "+add(string(f.Kind)))
	}
	if f.ProviderCode != "" {
		cond = append(cond, "provider_code = "+add(f.ProviderCode))
	}
	if id := parseID(f.Keyword); id > 0 {
		p := add(id)
		cond = append(cond, "(id = "+p+" or workspace_id = "+p+" or payer_account_id = "+p+")")
	}
	where := ""
	if len(cond) > 0 {
		where = " where " + strings.Join(cond, " and ")
	}
	rows, err := r.pool.Query(ctx, `
		with merged as (`+taskUnionSQL(allTenants())+`)
		select state, count(*)::int,
		       count(*) filter (where state in ('queued','running')
		                          and deadline_at is not null and deadline_at < $1)::int
		from merged`+where+` group by state`, args...)
	if err != nil {
		return OpsSummary{}, err
	}
	defer rows.Close()
	out := OpsSummary{ByState: map[domain.TaskState]int{}}
	for rows.Next() {
		var s domain.TaskState
		var n, stuck int
		if err := rows.Scan(&s, &n, &stuck); err != nil {
			return OpsSummary{}, err
		}
		out.ByState[s] = n
		out.Stuck += stuck
	}
	return out, rows.Err()
}

// parseID 把关键词当雪花号解析；解析不出来回 0（表示"这不是个 ID"）。
//
// ⚠️ 解析失败**不报错**：为一个格式问题把整个列表挡掉不值
// （同后台订单检索里的时间参数）。
func parseID(s string) int64 {
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
		if v < 0 { // 溢出：那就不是一个合法的雪花号
			return 0
		}
	}
	return v
}
