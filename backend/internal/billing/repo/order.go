package repo

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

// 订单与履约（P11-01）。
//
// ⚠️ **下单与履约是两件事**：下单只是"我要买这个"，履约才发权益。
// 合成一步的话，P11-02 接真实渠道时会发现没有地方插入"等支付结果"——
// 而那时已经有人拿着未付款的订单在用会员了。

// 订单类型与状态。与 `purchase_order` 的 CHECK 是同一份取值。
const (
	OrderMembership = "membership"
	OrderPackage    = "credit_package"

	PayPending   = "pending"
	PayPaid      = "paid"
	PayCancelled = "cancelled"
	PayRefunded  = "refunded"
	PayExpired   = "expired"
)

// 会员订单的三种意图。存在 `purchase_order` 之外的原因见 CreateOrderInput.Intent。
const (
	IntentNew     = "new"
	IntentRenew   = "renew"
	IntentUpgrade = "upgrade"
)

// Order 是一张订单。
type Order struct {
	ID          int64
	PayerID     int64
	PlacedBy    int64
	Type        string
	RefID       int64
	AmountCents int64
	Currency    string
	PayStatus   string
	Channel     string
	ChannelTxn  string
	PaidAt      *time.Time
	ExpireAt    *time.Time
	CreatedAt   time.Time
	// 下面几项是 join 出来的展示字段，不在 purchase_order 上。
	Title string
	// Credits 这张订单会到账多少积分（会员是赠分、积分包是包内积分）。
	Credits int
	// 对账结论（P11-03）。
	ReconcileStatus string
	ReconcileDetail string
}

// CreateOrderInput 建单。
type CreateOrderInput struct {
	PayerID     int64
	PlacedBy    int64
	Type        string
	RefID       int64
	AmountCents int64
	// Credits 这张单履约时要发多少积分。**下单时就定死**。
	//
	// ⚠️ 不能等履约时再查目录算一遍：用户下单后运营改了赠分，
	// 他付的是旧价、拿到的是新赠分（或更糟，更少的赠分）。
	// 价格同理 —— 快照思路与生成任务的 params_snapshot 一致。
	Credits int
	// MembershipExpireAt 会员订单履约后的到期时间。同样**下单时算好**。
	MembershipExpireAt *time.Time
	// MembershipStartAt 会员订单履约后的起始时间。
	MembershipStartAt *time.Time
	// LevelID 会员订单对应的等级。
	LevelID        int64
	IdempotencyKey string
	ExpireAt       *time.Time
}

// ErrDuplicateOrder 幂等键重复。
var ErrDuplicateOrder = errors.New("订单已存在")

// CreateOrder 建一张待支付订单。
//
// ⚠️ 履约要用的数据（发多少分、会员到期到哪天）**在这里就算好存下来**。
// 履约时再查一遍目录的话，用户下单后运营改了价或赠分，他付的是旧价、
// 拿到的是新权益 —— 而两边各自都"没错"。这与生成任务冻结 params_snapshot
// 是同一条规则：**一旦受理，后台此后的配置改动不得改变这一单的解释**。
func (r *Repo) CreateOrder(ctx context.Context, in CreateOrderInput) (Order, error) {
	id := r.nextID()
	_, err := r.pool.Exec(ctx, `
		insert into purchase_order (id, payer_account_id, placed_by_user_id, type,
			ref_id, amount_cents, idempotency_key, expire_at, fulfil_credits,
			fulfil_level_id, fulfil_start_at, fulfil_expire_at)
		values ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
		id, in.PayerID, in.PlacedBy, in.Type, nullInt(in.RefID), in.AmountCents,
		in.IdempotencyKey, in.ExpireAt, in.Credits, nullInt(in.LevelID),
		in.MembershipStartAt, in.MembershipExpireAt)
	if isDuplicate(err) {
		// 幂等键重复 —— 把已有的那张回给调用方，而不是报错。
		// 报错的话用户重复点提交会看到"下单失败"，而订单其实建好了。
		return r.OrderByIdempotency(ctx, in.PayerID, in.IdempotencyKey)
	}
	if err != nil {
		return Order{}, err
	}
	return r.Order(ctx, in.PayerID, id)
}

func nullInt(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}

const orderCols = `select o.id, o.payer_account_id, o.placed_by_user_id, o.type,
	coalesce(o.ref_id,0), o.amount_cents, o.currency, o.pay_status,
	coalesce(o.channel,''), coalesce(o.channel_txn_id,''), o.paid_at, o.expire_at,
	o.created_at, o.fulfil_credits, o.reconcile_status,
	coalesce(o.reconcile_detail,'') from purchase_order o `

func scanOrder(row pgx.Row) (Order, error) {
	var o Order
	err := row.Scan(&o.ID, &o.PayerID, &o.PlacedBy, &o.Type, &o.RefID,
		&o.AmountCents, &o.Currency, &o.PayStatus, &o.Channel, &o.ChannelTxn,
		&o.PaidAt, &o.ExpireAt, &o.CreatedAt, &o.Credits,
		&o.ReconcileStatus, &o.ReconcileDetail)
	return o, err
}

func (r *Repo) Order(ctx context.Context, payerID, id int64) (Order, error) {
	o, err := scanOrder(r.pool.QueryRow(ctx,
		orderCols+`where o.id = $1 and o.payer_account_id = $2`, id, payerID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Order{}, ErrNotFound
	}
	return o, err
}

func (r *Repo) OrderByIdempotency(ctx context.Context, payerID int64, key string) (
	Order, error) {
	o, err := scanOrder(r.pool.QueryRow(ctx,
		orderCols+`where o.payer_account_id = $1 and o.idempotency_key = $2`,
		payerID, key))
	if errors.Is(err, pgx.ErrNoRows) {
		return Order{}, ErrNotFound
	}
	return o, err
}

// ListOrders 订单列表（按时间倒序，游标分页）。
func (r *Repo) ListOrders(ctx context.Context, payerID int64, limit int,
	cursorID int64) ([]Order, error) {
	rows, err := r.pool.Query(ctx,
		orderCols+`where o.payer_account_id = $1 and ($2 = 0 or o.id < $2)
		order by o.id desc limit $3`, payerID, cursorID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Order
	for rows.Next() {
		o, err := scanOrder(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// FulfilResult 履约结果。
type FulfilResult struct {
	Order Order
	// GrantID 本次发放的积分批次（0 表示这一单不发积分）。
	GrantID int64
	// MembershipID 本次写入的会员资格（0 表示这一单不是会员单）。
	MembershipID int64
	// AlreadyDone 这一单之前已经履约过（重复回调）。
	AlreadyDone bool
}

// Fulfil 履约：订单转 paid + 发积分 + 写会员资格，**一个事务**。
//
// ⚠️ 三件事必须同一个事务：分开做的话中间崩一次，就会出现
// "订单显示已付款但没有会员"或"发了积分但订单还是待支付"，
// 而两种都只能人工对账。
//
// ⚠️ 幂等靠**带状态条件的更新**（CAS）：`where pay_status='pending'`。
// 0 行更新说明已经履约过，直接回既有状态 —— 重复回调不得重复发放权益
// （GO-04 原文）。靠"先查状态再写"挡不住并发的两个回调。
func (r *Repo) Fulfil(ctx context.Context, payerID, orderID int64,
	channel, channelTxn string, now time.Time) (FulfilResult, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return FulfilResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var (
		typ      string
		credits  int
		levelID  int64
		startAt  *time.Time
		expireAt *time.Time
	)
	err = tx.QueryRow(ctx, `
		update purchase_order set pay_status = 'paid', paid_at = $3,
			channel = $4, channel_txn_id = nullif($5,''), updated_at = now()
		where id = $1 and payer_account_id = $2 and pay_status = 'pending'
		returning type, fulfil_credits, coalesce(fulfil_level_id,0),
			fulfil_start_at, fulfil_expire_at`,
		orderID, payerID, now, channel, channelTxn).
		Scan(&typ, &credits, &levelID, &startAt, &expireAt)
	if errors.Is(err, pgx.ErrNoRows) {
		// 没抢到 —— 要么订单不存在，要么已经不是 pending 了。
		o, ferr := r.Order(ctx, payerID, orderID)
		if ferr != nil {
			return FulfilResult{}, ferr
		}
		return FulfilResult{Order: o, AlreadyDone: o.PayStatus == PayPaid}, nil
	}
	if err != nil {
		return FulfilResult{}, err
	}

	out := FulfilResult{}
	if credits > 0 {
		// ⚠️ 积分批次的有效期跟着订单类型走：会员赠分**随会员到期**，
		// 积分包发**永久**积分（GO-04 原文）。写反了的话，用户买的永久积分
		// 会在会员到期那天凭空消失 —— 而他完全不知道为什么。
		var grantExpire *time.Time
		if typ == OrderMembership {
			grantExpire = expireAt
		}
		gid, err := r.grantInTx(ctx, tx, GrantInput{
			PayerID: payerID, Source: sourceFor(typ),
			// ⚠️ 幂等引用用**订单 ID**：`uk_grant_source(source, source_ref_id)`
			// 于是成了"一张订单只能发一次积分"的库级保证。
			SourceRefID: itoa(orderID), Amount: credits, ExpireAt: grantExpire,
			Reason: "订单 " + itoa(orderID),
		})
		if err != nil {
			return FulfilResult{}, err
		}
		out.GrantID = gid
	}

	if typ == OrderMembership && levelID != 0 && expireAt != nil {
		mid := r.nextID()
		st := now
		if startAt != nil {
			st = *startAt
		}
		// ⚠️ 续费/升级是**改同一条会员资格**还是**插一条新的**？插新的。
		// 改旧的话历史就没了 —— 用户问"我这年卡是什么时候买的"、
		// 客服要查"这次升级补了多少钱"，都得靠订单与资格一一对应。
		// 旧的那条同时标 superseded（见下），所以 CurrentMembership 取到的是新的。
		// ⚠️ 标 `superseded` 不是 `expired`：升级保持原到期时间，
		// 那条旧行的 expire_at 仍在**未来** —— 标成 expired 的话
		// 「当前有效会员」会同时命中两行，取到哪一行取决于排序，
		// 而排序一改权益就变了且没有任何报错。
		if _, err := tx.Exec(ctx, `
			update membership set status = 'superseded', updated_at = now()
			where payer_account_id = $1 and status = 'active'`, payerID); err != nil {
			return FulfilResult{}, err
		}
		if _, err := tx.Exec(ctx, `
			insert into membership (id, payer_account_id, level_id, start_at,
				expire_at, source_order_id, status)
			values ($1,$2,$3,$4,$5,$6,'active')`,
			mid, payerID, levelID, st, *expireAt, orderID); err != nil {
			return FulfilResult{}, err
		}
		out.MembershipID = mid
	}

	if err := tx.Commit(ctx); err != nil {
		return FulfilResult{}, err
	}
	o, err := r.Order(ctx, payerID, orderID)
	if err != nil {
		return FulfilResult{}, err
	}
	out.Order = o
	return out, nil
}

func sourceFor(orderType string) string {
	if orderType == OrderMembership {
		return "membership"
	}
	return "purchase"
}

// CancelOrder 取消一张待支付订单。返回是否真的改了状态。
//
// ⚠️ 返回"本次是否真的改了"而不是只返回 error：已经付款的订单被取消
// 必须明确拒绝，静默成功的话用户以为退了钱而其实没有。
func (r *Repo) CancelOrder(ctx context.Context, payerID, orderID int64) (bool, error) {
	tag, err := r.pool.Exec(ctx, `
		update purchase_order set pay_status = 'cancelled', updated_at = now()
		where id = $1 and payer_account_id = $2 and pay_status = 'pending'`,
		orderID, payerID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// ExpireOrders 把超时未支付的订单标成 expired。
//
// ⚠️ 与会员到期作业不同，这个**必须跑**：订单占着幂等键，
// 不清掉的话用户再下同一单会命中那张僵尸订单（同一个幂等键），
// 看到的是一张早就付不了的单。
func (r *Repo) ExpireOrders(ctx context.Context, limit int) (int, error) {
	tag, err := r.pool.Exec(ctx, `
		update purchase_order set pay_status = 'expired', updated_at = now()
		where id in (select id from purchase_order
			where pay_status = 'pending' and expire_at is not null
			and expire_at <= now() limit $1)`, limit)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

// grantInTx 是 Grant 的事务内版本 —— 履约要和订单状态同一个事务。
func (r *Repo) grantInTx(ctx context.Context, tx pgx.Tx, in GrantInput) (int64, error) {
	var srcRef any
	if in.SourceRefID != "" {
		srcRef = in.SourceRefID
	}
	grantID := r.nextID()
	_, err := tx.Exec(ctx, `
		insert into credit_grant (id, payer_account_id, source, source_ref_id,
			amount, remaining, expire_at)
		values ($1,$2,$3,$4,$5,$5,$6)`,
		grantID, in.PayerID, in.Source, srcRef, in.Amount, in.ExpireAt)
	if err != nil {
		// ⚠️ 这里**不该**出现 uk_grant_source 冲突：订单状态的 CAS
		// 已经保证一张单只履约一次。真出现了说明有别的路径在发同一笔，
		// 如实报错让事务回滚 —— 吞掉的话订单会变成"已付款但没到账"。
		return 0, err
	}
	refID := in.SourceRefID
	if refID == "" {
		refID = itoa(grantID)
	}
	if err := r.insertTx(ctx, tx, txInput{
		PayerID: in.PayerID, Type: TxGrant, Change: in.Amount,
		RefType: in.Source, RefID: refID, Reason: in.Reason,
	}); err != nil {
		return 0, err
	}
	return grantID, nil
}

// OrderByOutTradeNo 按商户订单号找单（回调用）。
//
// ⚠️ **不带 payer 条件**：回调是公开端点，那里没有"当前用户"。
// 归属由订单自己带出来（回调里只有订单号），后续的履约按它写。
func (r *Repo) OrderByOutTradeNo(ctx context.Context, outTradeNo string) (Order, error) {
	id, err := strconv.ParseInt(outTradeNo, 10, 64)
	if err != nil {
		// 订单号就是订单 ID —— 解不成数字说明这条回调根本不是给我们的
		// （多半串了环境）。当成"订单不存在"处理，由上层记 order_not_found。
		return Order{}, ErrNotFound
	}
	// ⚠️ 别名是 `o`（`orderCols` 里写的）。写成别的字母的话 PG 报
	// "missing FROM-clause entry"，而上层把**任何**错误都当成"订单不存在"
	// 并告诉渠道不要重试 —— 于是一条本该履约的回调被永久丢掉，
	// 而日志里只有一句"回调的订单不存在"。实测在联调里撞到过。
	o, err := scanOrder(r.pool.QueryRow(ctx, orderCols+`where o.id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Order{}, ErrNotFound
	}
	return o, err
}

// MarkSynced 记一次查单结果。
//
// ⚠️ 查单结果**只是留痕不改状态**：状态由履约那条 CAS 决定。
// 在这里顺手改状态的话，一次"查到未付"会把已经付过的订单打回去。
func (r *Repo) MarkSynced(ctx context.Context, orderID int64, note string) error {
	_, err := r.pool.Exec(ctx, `
		update purchase_order set last_synced_at = now(), sync_note = left($2, 255),
			updated_at = now() where id = $1`, orderID, note)
	return err
}

// MarkChannel 记下这张单**最后一次**是用哪个渠道去拉起支付的。
//
// ⚠️ 必须记：没有它的话 `/sync` 不知道该问哪一家 —— 而"回调没来，
// 我去查一下"恰恰是刷新页面之后才会做的事，那时前端也不记得了。
//
// ⚠️ 只在**待支付**时写：已付款的订单上那个值是"实际收款渠道"，
// 覆盖掉的话对账时会把钱算到另一家头上。
func (r *Repo) MarkChannel(ctx context.Context, orderID int64, channel string) error {
	_, err := r.pool.Exec(ctx, `
		update purchase_order set channel = $2, updated_at = now()
		where id = $1 and pay_status = 'pending'`, orderID, channel)
	return err
}
