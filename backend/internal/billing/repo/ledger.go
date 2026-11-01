// Package repo 是积分账本的数据访问层。
//
// ⚠️ 三条硬约束都在这一层兑现：
//   - **余额检查与扣减在同一个事务内**，靠 `select ... for update` 锁住批次行；
//     拆成两步就是超卖（GO-11）。
//   - **同一 (ref_type, ref_id, type) 只能成功一次**，靠唯一索引兜底 ——
//     **不依赖 Redis 命中**：Redis 挂掉只能表现为变慢，绝不能变成重复扣费。
//   - **一次消费可跨多批**，逐批写 detail，transaction 只记一条总额流水。
package repo

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/li904338203/tabtv/backend/internal/billing/domain"
	"github.com/li904338203/tabtv/backend/internal/platform/pgtext"
)

var (
	// ErrNotFound 流水、目录项或订单不存在（P11-01 起共用）。
	ErrNotFound  = errors.New("不存在")
	ErrDuplicate = errors.New("同一业务引用已有该类型流水")
)

type Repo struct {
	pool   *pgxpool.Pool
	nextID func() int64
}

func New(pool *pgxpool.Pool, nextID func() int64) *Repo {
	return &Repo{pool: pool, nextID: nextID}
}

// TxType 是流水类型。
const (
	TxGrant   = "grant"
	TxConsume = "consume"
	TxRefund  = "refund"
	TxExpire  = "expire"
	TxAdjust  = "adjust"
)

// Balance 是可用余额：**由批次推导**，不存 user.balance 单字段（GO-19）。
//
// 单字段余额与逐批明细迟早对不上，而对不上时没人知道哪个是对的。
func (r *Repo) Balance(ctx context.Context, payerID int64) (int, error) {
	var n int
	err := r.pool.QueryRow(ctx, `
		select coalesce(sum(remaining), 0)::int from credit_grant
		where payer_account_id = $1 and status = 'active'
		  and (expire_at is null or expire_at > now())`, payerID).Scan(&n)
	return n, err
}

// GrantInput 是一次发放。
type GrantInput struct {
	PayerID     int64
	Source      string
	SourceRefID string
	Amount      int
	ExpireAt    *time.Time
	// CompensationOf 指回被补偿的原批次（过期退款时用）。
	CompensationOf int64
	Reason         string
	OperatorAdmin  int64
}

// Grant 发放一个批次并记一条 grant 流水。
//
// 幂等靠 `uk_grant_source(source, source_ref_id)`：同一来源+引用只能发一次。
// ⚠️ 会员按月发放、注册赠送、支付回调都可能重放，靠应用层"先查再插"挡不住并发。
func (r *Repo) Grant(ctx context.Context, in GrantInput) (int64, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var srcRef any
	if in.SourceRefID != "" {
		srcRef = in.SourceRefID
	}
	var compOf any
	if in.CompensationOf != 0 {
		compOf = in.CompensationOf
	}
	grantID := r.nextID()
	_, err = tx.Exec(ctx, `
		insert into credit_grant (id, payer_account_id, source, source_ref_id,
			amount, remaining, expire_at, compensation_of_grant_id)
		values ($1,$2,$3,$4,$5,$5,$6,$7)`,
		grantID, in.PayerID, in.Source, srcRef, in.Amount, in.ExpireAt, compOf)
	if isDuplicate(err) {
		return 0, ErrDuplicate
	}
	if err != nil {
		return 0, err
	}

	refID := in.SourceRefID
	if refID == "" {
		// 没有业务引用时用批次 ID 兜底，保证流水的 (ref_type, ref_id) 唯一。
		refID = itoa(grantID)
	}
	if err := r.insertTx(ctx, tx, txInput{
		PayerID: in.PayerID, Type: TxGrant, Change: in.Amount,
		RefType: "grant", RefID: refID, Reason: in.Reason,
		OperatorAdmin: in.OperatorAdmin,
	}); err != nil {
		return 0, err
	}
	return grantID, tx.Commit(ctx)
}

// ConsumeInput 是一次消费。
type ConsumeInput struct {
	PayerID int64
	Amount  int
	RefType string
	RefID   string
	UserID  int64
	Reason  string
}

// ConsumeResult 记录这次消费落在哪些批次上。
type ConsumeResult struct {
	TransactionID int64
	Allocations   []domain.Allocation
	BalanceAfter  int
}

// Consume 预扣即消费：**真实扣减 remaining**，不引入冻结态。
//
// 余额口径因此唯一 —— 用户看到的余额就是 sum(active remaining)，
// 不需要再解释"可用 / 冻结中"两个数（状态与计费 §3.1）。
// 代价是任务失败时余额先降后升，所以界面必须显示退款流水，
// 不能只是数字变回去，否则用户以为系统在乱扣。
func (r *Repo) Consume(ctx context.Context, in ConsumeInput) (ConsumeResult, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return ConsumeResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	res, err := r.ConsumeInTx(ctx, tx, in)
	if err != nil {
		return ConsumeResult{}, err
	}
	return res, tx.Commit(ctx)
}

// ConsumeInTx 在**调用方的事务里**扣费。
//
// ⚠️ 生成受理必须把「写任务 + 扣费 + 写 outbox」放进同一个事务：
// 拆成两个事务的话，中间崩溃会留下「扣了钱没有任务」或「有任务没扣钱」，
// 而这两种都只能靠对账人工修（FLOW-01）。
//
// 提供 tx 版本而不是让调用方自己拼 SQL：批次选择顺序、逐批明细、
// 唯一键兜底这些规则只应存在一份。
func (r *Repo) ConsumeInTx(ctx context.Context, tx pgx.Tx, in ConsumeInput) (ConsumeResult, error) {
	// ⚠️ `for update` 锁住本账户的可消费批次：余额检查与扣减必须在同一个事务里，
	// 否则两个并发请求会各自读到同一份余额、各扣一次（超卖）。
	grants, err := r.lockConsumable(ctx, tx, in.PayerID)
	if err != nil {
		return ConsumeResult{}, err
	}
	allocs, err := domain.PlanConsume(grants, in.Amount)
	if err != nil {
		return ConsumeResult{}, err
	}

	for _, a := range allocs {
		// 条件里带 remaining >= amount：即便规划有误也不会把余额扣成负数。
		tag, err := tx.Exec(ctx, `
			update credit_grant
			set remaining = remaining - $2,
			    status = case when remaining - $2 = 0 then 'exhausted' else status end,
			    updated_at = now()
			where id = $1 and remaining >= $2`, a.GrantID, a.Amount)
		if err != nil {
			return ConsumeResult{}, err
		}
		if tag.RowsAffected() == 0 {
			return ConsumeResult{}, domain.ErrInsufficient
		}
	}

	after, err := balanceInTx(ctx, tx, in.PayerID)
	if err != nil {
		return ConsumeResult{}, err
	}
	txID := r.nextID()
	if err := r.insertTxID(ctx, tx, txID, txInput{
		PayerID: in.PayerID, Type: TxConsume, Change: -in.Amount, BalanceAfter: after,
		RefType: in.RefType, RefID: in.RefID, Reason: in.Reason, OperatorUser: in.UserID,
	}); err != nil {
		return ConsumeResult{}, err
	}
	// 逐批明细：退款要按原批次回补，没有明细就只能凭空发永久积分（历史 bug）。
	for _, a := range allocs {
		if _, err := tx.Exec(ctx, `
			insert into credit_transaction_detail (id, transaction_id, grant_id, amount)
			values ($1,$2,$3,$4)`, r.nextID(), txID, a.GrantID, a.Amount); err != nil {
			return ConsumeResult{}, err
		}
	}
	return ConsumeResult{TransactionID: txID, Allocations: allocs, BalanceAfter: after}, nil
}

// RefundInput 是一次退款。
type RefundInput struct {
	PayerID int64
	RefType string
	RefID   string
	Reason  string
	// CompensationValidFor 补偿批次有效期（PARAM-02，未批准；由配置传入，不写死）。
	CompensationValidFor time.Duration
}

type RefundResult struct {
	Total        int
	Compensated  int
	BalanceAfter int
}

// Refund 按**原消费批次**退款；过期批次不复活，改发补偿批次。
//
// 幂等靠 `uk_ct_ref_type(ref_type, ref_id, type)`：同一业务的退款只能成功一次。
// ⚠️ 这是最终兜底，不是"先查有没有退过"——并发下两个退款请求都会查到"没退过"。
func (r *Repo) Refund(ctx context.Context, in RefundInput) (RefundResult, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return RefundResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// 找到原消费流水与逐批明细。
	var consumeTxID int64
	err = tx.QueryRow(ctx, `
		select id from credit_transaction
		where ref_type = $1 and ref_id = $2 and type = 'consume'`,
		in.RefType, in.RefID).Scan(&consumeTxID)
	if errors.Is(err, pgx.ErrNoRows) {
		return RefundResult{}, ErrNotFound
	}
	if err != nil {
		return RefundResult{}, err
	}

	rows, err := tx.Query(ctx, `
		select grant_id, amount from credit_transaction_detail
		where transaction_id = $1 order by id`, consumeTxID)
	if err != nil {
		return RefundResult{}, err
	}
	var details []domain.Allocation
	for rows.Next() {
		var a domain.Allocation
		if err := rows.Scan(&a.GrantID, &a.Amount); err != nil {
			rows.Close()
			return RefundResult{}, err
		}
		details = append(details, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return RefundResult{}, err
	}

	// ⚠️ **先写退款流水占住唯一键，再做回补**。
	// 反过来的话，重复退款会先撞上"回补后超过发放总额"这类领域校验，
	// 报出来的是一句用户看不懂的话；而真正的原因是"这笔已经退过了"。
	// 事务保证两者同生共死，所以顺序只影响错误信息 —— 但错误信息就是接口的一部分。
	// balance_after 此刻还不知道，先写 0，回补完再更新（同一事务内，外部看不到中间态）。
	refundTxID := r.nextID()
	if err := r.insertTxID(ctx, tx, refundTxID, txInput{
		PayerID: in.PayerID, Type: TxRefund, Change: 0, BalanceAfter: 0,
		RefType: in.RefType, RefID: in.RefID, Reason: in.Reason,
	}); err != nil {
		return RefundResult{}, err
	}

	// 锁住要回补的批次，避免与并发消费交错。
	grants, err := lockGrantsByID(ctx, tx, details)
	if err != nil {
		return RefundResult{}, err
	}
	now := time.Now()
	plan, err := domain.PlanRefund(details, grants, now)
	if err != nil {
		return RefundResult{}, err
	}

	for _, t := range plan.Restore {
		if t.Expired {
			continue // 过期批次不复活
		}
		if _, err := tx.Exec(ctx, `
			update credit_grant
			set remaining = remaining + $2,
			    status = case when status = 'exhausted' then 'active' else status end,
			    updated_at = now()
			where id = $1`, t.GrantID, t.Amount); err != nil {
			return RefundResult{}, err
		}
	}

	// 过期部分改发补偿批次，指回原批次，流水里能看出"为什么有效期变了"。
	if plan.Compensate > 0 {
		var originGrant int64
		for _, t := range plan.Restore {
			if t.Expired {
				originGrant = t.GrantID
				break
			}
		}
		var compOf any
		if originGrant != 0 {
			compOf = originGrant
		}
		if _, err := tx.Exec(ctx, `
			insert into credit_grant (id, payer_account_id, source, source_ref_id,
				amount, remaining, expire_at, compensation_of_grant_id)
			values ($1,$2,'expiry_compensation',$3,$4,$4,$5,$6)`,
			r.nextID(), in.PayerID, in.RefType+":"+in.RefID, plan.Compensate,
			domain.CompensationExpiry(now, in.CompensationValidFor), compOf); err != nil {
			return RefundResult{}, err
		}
	}

	after, err := balanceInTx(ctx, tx, in.PayerID)
	if err != nil {
		return RefundResult{}, err
	}
	if _, err := tx.Exec(ctx, `
		update credit_transaction set change = $2, balance_after = $3 where id = $1`,
		refundTxID, plan.Total, after); err != nil {
		return RefundResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return RefundResult{}, err
	}
	return RefundResult{Total: plan.Total, Compensated: plan.Compensate, BalanceAfter: after}, nil
}

// ExpireGrants 把到期批次标记为 expired 并记流水，返回处理条数。
//
// ⚠️ 流水是必须的：用户要能看懂"为什么余额少了"。只改状态不记流水的话，
// 余额凭空变少，而账本里找不到任何解释。
func (r *Repo) ExpireGrants(ctx context.Context, limit int) (int, error) {
	rows, err := r.pool.Query(ctx, `
		update credit_grant set status = 'expired', updated_at = now()
		where id in (
			select id from credit_grant
			where status = 'active' and expire_at is not null and expire_at <= now()
			order by expire_at limit $1)
		returning id, payer_account_id, remaining`, limit)
	if err != nil {
		return 0, err
	}
	type expired struct {
		id, payer int64
		remaining int
	}
	var list []expired
	for rows.Next() {
		var e expired
		if err := rows.Scan(&e.id, &e.payer, &e.remaining); err != nil {
			rows.Close()
			return 0, err
		}
		list = append(list, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	for _, e := range list {
		if e.remaining == 0 {
			continue // 已经花完的批次到期不产生流水：余额没有变化
		}
		after, err := r.Balance(ctx, e.payer)
		if err != nil {
			return 0, err
		}
		if err := r.insertTx(ctx, nil, txInput{
			PayerID: e.payer, Type: TxExpire, Change: -e.remaining, BalanceAfter: after,
			RefType: "grant", RefID: itoa(e.id), Reason: "积分到期",
		}); err != nil && !errors.Is(err, ErrDuplicate) {
			return 0, err
		}
	}
	return len(list), nil
}

// HasTransaction 查某业务引用是否已有该类型流水。
//
// ⚠️ 只用于**读**（页面展示、幂等提示），**不能**拿它做"先查再写"的并发控制 ——
// 两个并发请求都会查到"没有"。唯一性由唯一索引保证。
func (r *Repo) HasTransaction(ctx context.Context, refType, refID, txType string) (bool, error) {
	var n int
	err := r.pool.QueryRow(ctx, `
		select count(*) from credit_transaction
		where ref_type = $1 and ref_id = $2 and type = $3`, refType, refID, txType).Scan(&n)
	return n > 0, err
}

// ---------------------------------------------------------------- 内部

func (r *Repo) lockConsumable(ctx context.Context, tx pgx.Tx, payerID int64) ([]domain.Grant, error) {
	// ⚠️ 顺序由 SQL 与领域层各写一次是有意的冗余：SQL 的 order by 让锁的获取顺序稳定
	// （避免并发事务交叉持锁死锁），领域层的排序保证业务规则可被单测钉住。
	rows, err := tx.Query(ctx, `
		select id, amount, remaining, expire_at from credit_grant
		where payer_account_id = $1 and status = 'active'
		  and (expire_at is null or expire_at > now()) and remaining > 0
		order by (expire_at is null), expire_at asc, id asc
		for update`, payerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Grant
	for rows.Next() {
		var g domain.Grant
		if err := rows.Scan(&g.ID, &g.Amount, &g.Remaining, &g.ExpireAt); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

func lockGrantsByID(ctx context.Context, tx pgx.Tx, details []domain.Allocation) (
	map[int64]domain.Grant, error) {
	out := map[int64]domain.Grant{}
	if len(details) == 0 {
		return out, nil
	}
	ids := make([]int64, 0, len(details))
	for _, d := range details {
		ids = append(ids, d.GrantID)
	}
	rows, err := tx.Query(ctx, `
		select id, amount, remaining, expire_at from credit_grant
		where id = any($1) order by id for update`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var g domain.Grant
		if err := rows.Scan(&g.ID, &g.Amount, &g.Remaining, &g.ExpireAt); err != nil {
			return nil, err
		}
		out[g.ID] = g
	}
	return out, rows.Err()
}

func balanceInTx(ctx context.Context, tx pgx.Tx, payerID int64) (int, error) {
	var n int
	err := tx.QueryRow(ctx, `
		select coalesce(sum(remaining), 0)::int from credit_grant
		where payer_account_id = $1 and status = 'active'
		  and (expire_at is null or expire_at > now())`, payerID).Scan(&n)
	return n, err
}

type txInput struct {
	PayerID       int64
	Type          string
	Change        int
	BalanceAfter  int
	RefType       string
	RefID         string
	Reason        string
	OperatorUser  int64
	OperatorAdmin int64
}

func (r *Repo) insertTx(ctx context.Context, tx pgx.Tx, in txInput) error {
	return r.insertTxID(ctx, tx, r.nextID(), in)
}

// reasonMaxRunes 是 `credit_transaction.reason varchar(255)` 的上限（PG 的 varchar 按字符计）。
const reasonMaxRunes = 255

func (r *Repo) insertTxID(ctx context.Context, tx pgx.Tx, id int64, in txInput) error {
	sql := `insert into credit_transaction (id, payer_account_id, type, change,
			balance_after, ref_type, ref_id, operator_user_id, operator_admin_id, reason)
		values ($1,$2,$3,$4,$5,$6,$7,nullif($8::bigint,0),nullif($9::bigint,0),nullif($10,''))`
	// ⚠️ 原因只是说明文字，存不下就截（CD-82）：退款的原因常是上游报错原文，一段 323 字的 JSON
	//    撞上 varchar(255) 会让**整笔退款**回滚 —— 而退款失败只留一行日志，钱就没退回去。
	args := []any{id, in.PayerID, in.Type, in.Change, in.BalanceAfter,
		in.RefType, in.RefID, in.OperatorUser, in.OperatorAdmin, pgtext.Fit(in.Reason, reasonMaxRunes)}

	var err error
	if tx != nil {
		_, err = tx.Exec(ctx, sql, args...)
	} else {
		_, err = r.pool.Exec(ctx, sql, args...)
	}
	if isDuplicate(err) {
		return ErrDuplicate
	}
	return err
}

func isDuplicate(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }

// Transaction 是一条账本流水。
type Transaction struct {
	ID           int64
	Type         string
	Change       int
	BalanceAfter int
	RefType      string
	RefID        string
	Reason       string
	CreatedAt    time.Time

	// 下面几项只给积分中心的流水展示用（2026-11-01）：按 ref_type 关回业务表，说清「哪个项目、什么生成、哪个模型」。
	// 关不上（引用的记录已清理、ref_type 没有对应的业务表，如购买 / 过期 / 调账）就是零值 —— 流水本身不依赖它们。
	ProjectID      int64
	ProjectName    string
	ProjectDeleted bool
	// OperationCode / OperationName 只有生成（ref_type=generation）有：text2img / 文生图 等。
	OperationCode string
	OperationName string
	// ModelName：生成与提示词合成按模型 ID 取名称，Agent 对话按会话那一轮的模型编码取。
	ModelName string
	// RefTitle：成片渲染是时间线名，Agent 对话是会话标题。
	RefTitle string
}

// ListTransactions 按 id 倒序分页（雪花 ID 天然按时间倒序）。
//
// ⚠️ 退款流水**必须展示给用户**：预扣即消费的代价是余额先降后升，
// 只让数字变回去而不显示这笔退款，用户会以为系统在乱扣（状态与计费 §3.1）。
//
// 展示用的关联（2026-11-01）：先在 CTE 里按付费主体取完这一页，再按 ref_type 左关联业务表 ——
// 关联只作用在这一页的几十行上，不拖慢翻页。ref_id 是字符串（各业务自己的 ID），只有纯数字时才转成
// bigint 去关联，否则一条非数字的 ref_id 会让整页报错。关联的业务表也都限定**同一个付费主体**：
// 雪花 ID 本不会撞，但账本上的 ref_id 是业务方写进来的自由字符串，不该靠「不会撞」来保证不串账。
func (r *Repo) ListTransactions(ctx context.Context, payerID int64, limit int, cursorID int64) (
	[]Transaction, error) {
	page := `select id, type, change, balance_after, ref_type, ref_id, coalesce(reason,'') as reason, created_at,
	                case when ref_id ~ '^[0-9]{1,18}$' then ref_id::bigint end as ref_num
	         from credit_transaction where payer_account_id = $1`
	args := []any{payerID}
	if cursorID > 0 {
		args = append(args, cursorID)
		page += ` and id < $` + strconv.Itoa(len(args))
	}
	args = append(args, limit)
	page += ` order by id desc limit $` + strconv.Itoa(len(args))

	sql := `with t as (` + page + `)
		select t.id, t.type, t.change, t.balance_after, t.ref_type, t.ref_id, t.reason, t.created_at,
		       coalesce(p.id, 0), coalesce(p.name, ''), coalesce(p.deleted_at is not null, false),
		       coalesce(op.code, ''), coalesce(op.name, ''),
		       coalesce(m.name, am.name, case when s.id is not null then atn.model_code end, ''),
		       coalesce(tl.name, s.title, '')
		from t
		left join generation g       on t.ref_type = 'generation' and g.id = t.ref_num and g.payer_account_id = $1
		left join operation op       on op.id = g.operation_id
		left join script_llm_call sc on t.ref_type = 'script_llm' and sc.id = t.ref_num and sc.payer_account_id = $1
		left join script_instance si on si.id = sc.script_id
		left join agent_turn atn     on t.ref_type = 'agent_turn' and atn.id = t.ref_num
		left join agent_session s    on s.id = atn.session_id and s.payer_account_id = $1
		left join render_job rj      on t.ref_type = 'render' and rj.id = t.ref_num and rj.payer_account_id = $1
		left join timeline tl        on tl.id = rj.timeline_id
		left join model m            on m.id = coalesce(g.model_id, sc.model_id)
		left join model am           on s.id is not null and am.code = atn.model_code
		left join project p          on p.id = coalesce(g.project_id, si.project_id, s.project_id, tl.project_id)
		order by t.id desc`

	rows, err := r.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Transaction
	for rows.Next() {
		var t Transaction
		if err := rows.Scan(&t.ID, &t.Type, &t.Change, &t.BalanceAfter,
			&t.RefType, &t.RefID, &t.Reason, &t.CreatedAt,
			&t.ProjectID, &t.ProjectName, &t.ProjectDeleted,
			&t.OperationCode, &t.OperationName, &t.ModelName, &t.RefTitle); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// GrantRow 是对外展示的批次。
type GrantRow struct {
	ID        int64
	Source    string
	Amount    int
	Remaining int
	ExpireAt  *time.Time
	// Status 批次状态。
	//
	// ⚠️ 用户端列可用批次时它恒为 `active`（查询本来就带了这个条件），
	// 但后台的订单详情要**含已回收的** —— 只列 active 的话，
	// 一张退过款的订单在详情页上看起来"从来没发过积分"。
	Status string
}

// ListActiveGrants 按消费顺序返回可用批次 —— 与实际扣减顺序一致，
// 用户看到的"下一笔会先花哪批"就是真的。
func (r *Repo) ListActiveGrants(ctx context.Context, payerID int64) ([]GrantRow, error) {
	rows, err := r.pool.Query(ctx, `
		select id, source, amount, remaining, expire_at, status from credit_grant
		where payer_account_id = $1 and status = 'active'
		  and (expire_at is null or expire_at > now()) and remaining > 0
		order by (expire_at is null), expire_at asc, id asc`, payerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []GrantRow
	for rows.Next() {
		var g GrantRow
		if err := rows.Scan(&g.ID, &g.Source, &g.Amount, &g.Remaining, &g.ExpireAt,
			&g.Status); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// TxRef 是一条流水的业务引用（ref_type + ref_id）。
type TxRef struct {
	Type string
	ID   string
}

// MaxTxRefs 是 TransactionsByRefs 一次最多接受的引用数。
// 一轮 Agent 对话的引用 = 1 条轮次 + 本轮提交的生成任务，几十条已经很多；
// 上限只是「别让一次请求拼出一条巨大的 SQL」。
const MaxTxRefs = 500

// TransactionsByRefs 按 (ref_type, ref_id) 批量取某个付费主体的流水（AGT-26：Agent 每轮对账）。
//
// ⚠️ **只读**，且带 payer_account_id 条件：引用来自调用方，不能让它借一个 refID
// 读到别的付费主体的流水。按 id 升序（雪花号 ≈ 时间序），消费在前、退款在后。
// ⚠️ 口径只能是账本：业务表上的 credits_cost 是**报价**，退款后不会变（§5.18）。
func (r *Repo) TransactionsByRefs(ctx context.Context, payerID int64, refs []TxRef) ([]Transaction, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	if len(refs) > MaxTxRefs {
		refs = refs[:MaxTxRefs]
	}
	types := make([]string, len(refs))
	ids := make([]string, len(refs))
	for i, ref := range refs {
		types[i], ids[i] = ref.Type, ref.ID
	}
	rows, err := r.pool.Query(ctx, `
		select t.id, t.type, t.change, t.balance_after, t.ref_type, t.ref_id,
		       coalesce(t.reason, ''), t.created_at
		from credit_transaction t
		join unnest($2::text[], $3::text[]) as k(ref_type, ref_id)
		  on t.ref_type = k.ref_type and t.ref_id = k.ref_id
		where t.payer_account_id = $1
		order by t.id`, payerID, types, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Transaction
	for rows.Next() {
		var t Transaction
		if err := rows.Scan(&t.ID, &t.Type, &t.Change, &t.BalanceAfter,
			&t.RefType, &t.RefID, &t.Reason, &t.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
