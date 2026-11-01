package repo

import (
	"context"

	"github.com/li904338203/tabtv/backend/internal/observability/domain"
)

// 统计口径的**唯一实现**（P13-04；GAP-24）。
//
// ⚠️ GAP-24 的原话是「不把旧 SQL 名称直接当业务定义」。每条查询上面那段注释
// 就是那个定义 —— 看板上的数字与这段话必须一一对应，而 `docs/design` 里的
// 口径文档是它的副本，**不是第二个定义**。
//
// ⚠️ **实时算，不预聚合**。`stat_daily` 那张表暂时空着，理由写在 P13-04 挂账里：
// 预聚合要回答"今天算到几点了"，而那正是最容易让人看错数的地方
// （今天那一列是半天的数据，而看板上没有任何地方写着这件事）。
// 将来数据量到了要填它时，**必须由这里的同一份 SQL 产出** —— 各写一份的话
// 两个数字都"没错"却不一致，而报表只会读其中一个。

// Totals 是一段时间内的汇总。
type Totals struct {
	ActiveUsers int64
	// ⚠️ 消耗与退还**分开给，不合并成净值**（第五次）：把退回的悄悄减掉
	// 之后，看的人拿这个数去对账本流水会对不上，而他无从知道差额是怎么来的。
	CreditsConsumed int64
	CreditsRefunded int64

	Generations         int64
	GenerationSuccess   int64
	GenerationFailed    int64
	GenerationCancelled int64

	Renders       int64
	RenderSuccess int64

	PaidOrders    int64
	RevenueCents  int64
	RefundedCents int64

	ProviderCalls  int64
	ProviderErrors int64
}

// Overview 算一段时间内的全部汇总。
//
// ⚠️ 一次查询取齐，不是十几次往返：分开查的话每一条各自"没错"，
// 但它们**跑在不同的时刻**，于是"生成数"与"消耗积分"会对不上 ——
// 而那种不一致没有任何报错。
func (r *Repo) Overview(ctx context.Context, rg domain.Range) (Totals, error) {
	var t Totals
	err := r.pool.QueryRow(ctx, `
		with
		-- ⚠️ 真实活跃 = 当天**产生过消费或产出**的人，不是"登录过的人"。
		--    按 last_login_at 算的话（旧系统的做法，GAP-24），一个把页面挂在
		--    后台的浏览器每天都算一个活跃用户，而这个数只会涨。
		--    取并集：只看生成的话，纯写脚本/剪时间线的人会被算成"不活跃"。
		act as (
		  select created_by_user_id as uid from generation
		   where created_at >= $1 and created_at < $2
		  union
		  select created_by_user_id from render_job
		   where created_at >= $1 and created_at < $2
		  union
		  select created_by_user_id from script_llm_call
		   where created_at >= $1 and created_at < $2 and created_by_user_id is not null
		),
		-- ⚠️ 积分口径**只认账本**，不按各业务表的 credits_cost 相加 ——
		--    那一列是**报价**，任务失败退款之后它不会变，于是一个失败又
		--    退款的任务会被算成花了钱（CD-60 同一条）。
		--    change 是有符号的：消费为负、退款为正，所以消费取 abs。
		tx as (
		  select type, sum(abs(change))::bigint as total
		    from credit_transaction
		   where created_at >= $1 and created_at < $2
		   group by type
		),
		gen as (
		  select count(*)::bigint as n,
		         count(*) filter (where status = 'success')::bigint as ok,
		         count(*) filter (where status = 'failed')::bigint as bad,
		         -- ⚠️ 取消**单列不并进失败**：它是用户行为不是故障。
		         --    并进去的话用户多取消几次会让成功率看起来在下降，
		         --    而系统一切正常。
		         count(*) filter (where status = 'cancelled')::bigint as cancel
		    from generation where created_at >= $1 and created_at < $2
		),
		rnd as (
		  select count(*)::bigint as n,
		         count(*) filter (where status = 'success')::bigint as ok
		    from render_job where created_at >= $1 and created_at < $2
		),
		-- ⚠️ 收入**只算已支付的**：把待支付的也算进去会让报表凭空多出一截
		--    （CD-56）。按 paid_at 而不是 created_at 分组 —— 一张昨天下单、
		--    今天付款的单，收入属于今天。
		ord as (
		  select count(*)::bigint as n, coalesce(sum(amount_cents),0)::bigint as cents
		    from purchase_order
		   where pay_status = 'paid' and paid_at >= $1 and paid_at < $2
		),
		-- 退款额**单列，不从收入里减掉**（同上）。
		ref as (
		  select coalesce(sum(amount_cents),0)::bigint as cents
		    from refund
		   where status = 'success' and created_at >= $1 and created_at < $2
		),
		-- ⚠️ 这是**调用次数不是成本**。真实成本要各 Provider 的单价，
		--    而那是 PARAM-04 未定稿的内容 —— 编一个数出来比没有更糟：
		--    它会被当成真的，而没有人知道它是编的。
		prov as (
		  select count(*)::bigint as n,
		         count(*) filter (where error_type <> 'none')::bigint as err
		    from provider_request_log where created_at >= $1 and created_at < $2
		)
		select (select count(*)::bigint from act),
		       coalesce((select total from tx where type = 'consume'),0),
		       coalesce((select total from tx where type = 'refund'),0),
		       gen.n, gen.ok, gen.bad, gen.cancel,
		       rnd.n, rnd.ok,
		       ord.n, ord.cents, ref.cents,
		       prov.n, prov.err
		  from gen, rnd, ord, ref, prov`, rg.From, rg.To).
		Scan(&t.ActiveUsers, &t.CreditsConsumed, &t.CreditsRefunded,
			&t.Generations, &t.GenerationSuccess, &t.GenerationFailed,
			&t.GenerationCancelled, &t.Renders, &t.RenderSuccess,
			&t.PaidOrders, &t.RevenueCents, &t.RefundedCents,
			&t.ProviderCalls, &t.ProviderErrors)
	return t, err
}

// SeriesPoint 是折线上的一天。
type SeriesPoint struct {
	Date  string
	Value int64
}

// Series 按天取一个指标的序列。
//
// ⚠️ **补零**：没有数据的那天要出现在序列里且值为 0。不补的话折线会把
// 两个相隔一周的点直接连起来，看上去像"平稳增长" —— 而中间那一周其实
// 一条记录都没有（服务挂了也长这样）。用 `generate_series` 在 SQL 里补，
// 不在应用层补：应用层补要自己处理时区与跨月，而那是另一处会错的地方。
//
// ⚠️ 指标名是**白名单映射**出来的，不是拼进 SQL 的字符串 ——
// 它来自 HTTP 查询参数，拼进去就是注入。
func (r *Repo) Series(ctx context.Context, m domain.Metric, rg domain.Range) (
	[]SeriesPoint, error) {
	expr, ok := seriesExpr[m]
	if !ok {
		return nil, nil
	}
	rows, err := r.pool.Query(ctx, `
		with days as (
		  select generate_series(date_trunc('day', $1::timestamptz),
		                         date_trunc('day', $2::timestamptz),
		                         interval '1 day')::date as d
		),
		v as (`+expr+`)
		select to_char(days.d, 'YYYY-MM-DD'), coalesce(v.n, 0)::bigint
		  from days left join v on v.d = days.d
		 order by days.d`, rg.From, rg.To)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SeriesPoint{}
	for rows.Next() {
		var p SeriesPoint
		if err := rows.Scan(&p.Date, &p.Value); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// seriesExpr 是指标 → 按天聚合的 SQL 片段。
//
// ⚠️ **白名单**：能画成折线的指标只有这几个，别的一律回空序列而不是报错
// （前端可能比后端新）。每个片段都必须产出 `(d date, n bigint)` 两列。
//
// ⚠️ 每条的口径与 `Overview` 里那条**必须一致** —— 不一致的表现是
// 「折线各天之和 ≠ 概览上的总数」，而看的人只会觉得"这个看板不准"，
// 说不出哪里不准。集成测试用同一批数据同时验两边。
var seriesExpr = map[domain.Metric]string{
	domain.MetricActiveUsers: `
		select d, count(distinct uid)::bigint as n from (
		  select date_trunc('day', created_at)::date as d, created_by_user_id as uid
		    from generation where created_at >= $1 and created_at < $2
		  union
		  select date_trunc('day', created_at)::date, created_by_user_id
		    from render_job where created_at >= $1 and created_at < $2
		  union
		  select date_trunc('day', created_at)::date, created_by_user_id
		    from script_llm_call
		   where created_at >= $1 and created_at < $2 and created_by_user_id is not null
		) t group by d`,
	domain.MetricCreditsConsumed: `
		select date_trunc('day', created_at)::date as d, sum(abs(change))::bigint as n
		  from credit_transaction
		 where type = 'consume' and created_at >= $1 and created_at < $2
		 group by d`,
	domain.MetricCreditsRefunded: `
		select date_trunc('day', created_at)::date as d, sum(abs(change))::bigint as n
		  from credit_transaction
		 where type = 'refund' and created_at >= $1 and created_at < $2
		 group by d`,
	domain.MetricGenerations: `
		select date_trunc('day', created_at)::date as d, count(*)::bigint as n
		  from generation where created_at >= $1 and created_at < $2 group by d`,
	domain.MetricGenerationSuccess: `
		select date_trunc('day', created_at)::date as d, count(*)::bigint as n
		  from generation
		 where status = 'success' and created_at >= $1 and created_at < $2 group by d`,
	domain.MetricGenerationFailed: `
		select date_trunc('day', created_at)::date as d, count(*)::bigint as n
		  from generation
		 where status = 'failed' and created_at >= $1 and created_at < $2 group by d`,
	domain.MetricGenerationCancelled: `
		select date_trunc('day', created_at)::date as d, count(*)::bigint as n
		  from generation
		 where status = 'cancelled' and created_at >= $1 and created_at < $2 group by d`,
	domain.MetricRenders: `
		select date_trunc('day', created_at)::date as d, count(*)::bigint as n
		  from render_job where created_at >= $1 and created_at < $2 group by d`,
	domain.MetricRenderSuccess: `
		select date_trunc('day', created_at)::date as d, count(*)::bigint as n
		  from render_job
		 where status = 'success' and created_at >= $1 and created_at < $2 group by d`,
	domain.MetricRevenueCents: `
		select date_trunc('day', paid_at)::date as d, sum(amount_cents)::bigint as n
		  from purchase_order
		 where pay_status = 'paid' and paid_at >= $1 and paid_at < $2 group by d`,
	domain.MetricRefundedCents: `
		select date_trunc('day', created_at)::date as d, sum(amount_cents)::bigint as n
		  from refund
		 where status = 'success' and created_at >= $1 and created_at < $2 group by d`,
	domain.MetricProviderCalls: `
		select date_trunc('day', created_at)::date as d, count(*)::bigint as n
		  from provider_request_log where created_at >= $1 and created_at < $2 group by d`,
	domain.MetricProviderErrors: `
		select date_trunc('day', created_at)::date as d, count(*)::bigint as n
		  from provider_request_log
		 where error_type <> 'none' and created_at >= $1 and created_at < $2 group by d`,
}

// SeriesMetrics 返回能画成折线的指标清单，供前端与契约对齐。
func SeriesMetrics() []domain.Metric {
	out := make([]domain.Metric, 0, len(seriesExpr))
	for m := range seriesExpr {
		out = append(out, m)
	}
	return out
}

// ProviderBreakdown 是按供应商分组的调用量。
type ProviderBreakdown struct {
	ProviderCode string
	Calls        int64
	Errors       int64
	// AvgDurationMs 平均耗时。⚠️ 平均值会被一两条极慢的请求带偏，
	// 但它便宜且够用来发现"某家突然变慢了"；真要定位要看 P95，
	// 那要另建索引，随 P14 的性能工作一起做。
	AvgDurationMs int64
}

// ByProvider 按供应商分组。
//
// ⚠️ 标题必须写"调用次数"不能写"成本"：真实成本要各 Provider 的单价，
// 而那是 PARAM-04 未定稿的内容。
func (r *Repo) ByProvider(ctx context.Context, rg domain.Range) ([]ProviderBreakdown, error) {
	rows, err := r.pool.Query(ctx, `
		select provider_code, count(*)::bigint,
		       count(*) filter (where error_type <> 'none')::bigint,
		       coalesce(avg(duration_ms),0)::bigint
		  from provider_request_log
		 where created_at >= $1 and created_at < $2
		 group by provider_code order by count(*) desc`, rg.From, rg.To)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ProviderBreakdown{}
	for rows.Next() {
		var b ProviderBreakdown
		if err := rows.Scan(&b.ProviderCode, &b.Calls, &b.Errors, &b.AvgDurationMs); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}
