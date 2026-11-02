package desktop

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

// freePricing：所有操作都是 0 积分（TT-T-09，dev/01 修订块 R-7）。
//
// 来源系统每一处扣费都写着「费用 > 0 才扣」，所以只要价格全为 0，账本、预扣、退款整段跳过，
// 不用替换任何计费端口。两件事：给每个没有兜底价的操作补一条 0 积分兜底价（否则报价与发布检查
// 判「缺价格」）；把已有价格一律归零（seed 带了渲染按秒计价等非零价）。每次启动执行，幂等。
func freePricing(ctx context.Context, pool *pgxpool.Pool, nextID func() int64) error {
	if _, err := pool.Exec(ctx, `update pricing set credits = 0, updated_at = now() where credits <> 0`); err != nil {
		return err
	}
	rows, err := pool.Query(ctx, `
		select o.id from operation o
		where not exists (select 1 from pricing p where p.operation_id = o.id and p.model_id is null)`)
	if err != nil {
		return err
	}
	var ops []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ops = append(ops, id)
	}
	rows.Close()
	for _, op := range ops {
		if _, err := pool.Exec(ctx, `
			insert into pricing (id, operation_id, model_id, credits, unit, status)
			values ($1, $2, null, 0, 'per_time', 'enabled') on conflict do nothing`, nextID(), op); err != nil {
			return err
		}
	}
	return nil
}
