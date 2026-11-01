package repo

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/li904338203/tabtv/backend/internal/billing/domain"
)

// 会员目录、会员资格与订单（P11-01）。
//
// ⚠️ 会员挂在 **payer_account** 上而不是 user：团队工作区的成员应当
// 继承工作区的等级权益 —— 挂 user 的话，团队买了年卡而成员各自还是免费档，
// 他们会发现"同一个工作区里，有人能用那个模型有人不能"。

// Level 是会员等级。
type Level struct {
	ID        int64
	Code      string
	Name      string
	Sort      int
	IsEnabled bool
}

// Plan 是某等级的一个周期套餐。
type Plan struct {
	ID          int64
	LevelID     int64
	LevelCode   string
	LevelName   string
	LevelSort   int
	Period      string
	PriceCents  int64
	GiftCredits int
	Sort        int
	IsEnabled   bool
}

// Package 是积分包。
type Package struct {
	ID         int64
	Code       string
	Name       string
	Credits    int
	PriceCents int64
	Sort       int
	IsEnabled  bool
}

// Membership 是一条会员资格记录。
type Membership struct {
	ID            int64
	PayerID       int64
	LevelID       int64
	LevelCode     string
	LevelName     string
	LevelSort     int
	StartAt       time.Time
	ExpireAt      time.Time
	SourceOrderID int64
	Status        string
}

// ---------------------------------------------------------------- 目录

// ListLevels 列等级。`onlyEnabled` 供用户端用。
func (r *Repo) ListLevels(ctx context.Context, onlyEnabled bool) ([]Level, error) {
	rows, err := r.pool.Query(ctx, `
		select id, code, name, sort, is_enabled from member_level
		where (not $1 or is_enabled) order by sort, id`, onlyEnabled)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Level
	for rows.Next() {
		var l Level
		if err := rows.Scan(&l.ID, &l.Code, &l.Name, &l.Sort, &l.IsEnabled); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

const planCols = `select p.id, p.level_id, l.code, l.name, l.sort, p.period,
	p.price_cents, p.gift_credits, p.sort, p.is_enabled
	from member_plan p join member_level l on l.id = p.level_id `

func scanPlan(row pgx.Row) (Plan, error) {
	var p Plan
	err := row.Scan(&p.ID, &p.LevelID, &p.LevelCode, &p.LevelName, &p.LevelSort,
		&p.Period, &p.PriceCents, &p.GiftCredits, &p.Sort, &p.IsEnabled)
	return p, err
}

// ListPlans 列套餐。
//
// ⚠️ 用户端只看**两边都启用**的：等级停用了而套餐还挂着的话，
// 用户能买到一个买完就没有权益的等级。
func (r *Repo) ListPlans(ctx context.Context, onlyEnabled bool) ([]Plan, error) {
	rows, err := r.pool.Query(ctx,
		planCols+`where (not $1 or (p.is_enabled and l.is_enabled))
		order by l.sort, p.sort, p.id`, onlyEnabled)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Plan
	for rows.Next() {
		p, err := scanPlan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (r *Repo) Plan(ctx context.Context, id int64) (Plan, error) {
	p, err := scanPlan(r.pool.QueryRow(ctx, planCols+`where p.id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Plan{}, ErrNotFound
	}
	return p, err
}

// PlanFor 按等级 + 周期取套餐（升级报价要拿"当前等级的同周期套餐"作基准）。
func (r *Repo) PlanFor(ctx context.Context, levelID int64, period string) (Plan, error) {
	p, err := scanPlan(r.pool.QueryRow(ctx,
		planCols+`where p.level_id = $1 and p.period = $2`, levelID, period))
	if errors.Is(err, pgx.ErrNoRows) {
		return Plan{}, ErrNotFound
	}
	return p, err
}

func (r *Repo) ListPackages(ctx context.Context, onlyEnabled bool) ([]Package, error) {
	rows, err := r.pool.Query(ctx, `
		select id, code, name, credits, price_cents, sort, is_enabled
		from credit_package where (not $1 or is_enabled) order by sort, id`, onlyEnabled)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Package
	for rows.Next() {
		var p Package
		if err := rows.Scan(&p.ID, &p.Code, &p.Name, &p.Credits,
			&p.PriceCents, &p.Sort, &p.IsEnabled); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (r *Repo) Package(ctx context.Context, id int64) (Package, error) {
	var p Package
	err := r.pool.QueryRow(ctx, `
		select id, code, name, credits, price_cents, sort, is_enabled
		from credit_package where id = $1`, id).
		Scan(&p.ID, &p.Code, &p.Name, &p.Credits, &p.PriceCents, &p.Sort, &p.IsEnabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return Package{}, ErrNotFound
	}
	return p, err
}

// ---------------------------------------------------------------- 目录写入（管理端）

// SaveLevel 新增或更新等级。code 是业务键，冲突即更新。
func (r *Repo) SaveLevel(ctx context.Context, l Level) error {
	_, err := r.pool.Exec(ctx, `
		insert into member_level (id, code, name, sort, is_enabled)
		values ($1,$2,$3,$4,$5)
		on conflict (code) do update set
			name = excluded.name, sort = excluded.sort,
			is_enabled = excluded.is_enabled, updated_at = now()`,
		r.nextID(), l.Code, l.Name, l.Sort, l.IsEnabled)
	return err
}

// SavePlan 新增或更新套餐。(level_id, period) 是业务键。
func (r *Repo) SavePlan(ctx context.Context, p Plan) error {
	_, err := r.pool.Exec(ctx, `
		insert into member_plan (id, level_id, period, price_cents, gift_credits,
			sort, is_enabled)
		values ($1,$2,$3,$4,$5,$6,$7)
		on conflict (level_id, period) do update set
			price_cents = excluded.price_cents, gift_credits = excluded.gift_credits,
			sort = excluded.sort, is_enabled = excluded.is_enabled, updated_at = now()`,
		r.nextID(), p.LevelID, p.Period, p.PriceCents, p.GiftCredits, p.Sort, p.IsEnabled)
	return err
}

func (r *Repo) SavePackage(ctx context.Context, p Package) error {
	_, err := r.pool.Exec(ctx, `
		insert into credit_package (id, code, name, credits, price_cents, sort, is_enabled)
		values ($1,$2,$3,$4,$5,$6,$7)
		on conflict (code) do update set
			name = excluded.name, credits = excluded.credits,
			price_cents = excluded.price_cents, sort = excluded.sort,
			is_enabled = excluded.is_enabled, updated_at = now()`,
		r.nextID(), p.Code, p.Name, p.Credits, p.PriceCents, p.Sort, p.IsEnabled)
	return err
}

// ---------------------------------------------------------------- 会员资格

const membershipCols = `select m.id, m.payer_account_id, m.level_id, l.code, l.name,
	l.sort, m.start_at, m.expire_at, coalesce(m.source_order_id,0), m.status
	from membership m join member_level l on l.id = m.level_id `

func scanMembership(row pgx.Row) (Membership, error) {
	var m Membership
	err := row.Scan(&m.ID, &m.PayerID, &m.LevelID, &m.LevelCode, &m.LevelName,
		&m.LevelSort, &m.StartAt, &m.ExpireAt, &m.SourceOrderID, &m.Status)
	return m, err
}

// CurrentMembership 取当前有效的会员资格。
//
// ⚠️ 判据是 **`expire_at > now()`**，不是 `status='active'`：状态由过期作业
// 刷新，作业停一天就有一整天的人在用已经过期的等级权益。状态只用于列表展示
// 与统计，**权益判定永远按时间算**（与提示词 ready/stale、导出过期同一条）。
//
// ⚠️ 同时排除 `refunded` 与 `superseded`：前者退款后行还在（要留痕），
// 后者是升级时被顶替的旧行 —— 它的 expire_at 仍在**未来**（升级保持原到期日），
// 只按时间筛的话会同时命中两行。
func (r *Repo) CurrentMembership(ctx context.Context, payerID int64) (Membership, error) {
	m, err := scanMembership(r.pool.QueryRow(ctx,
		membershipCols+`where m.payer_account_id = $1
		and m.status not in ('refunded','superseded')
		and m.expire_at > now() order by l.sort desc, m.expire_at desc limit 1`,
		payerID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Membership{}, ErrNotFound
	}
	return m, err
}

// ListMemberships 会员历史（含已过期），用于"我的会员"页。
func (r *Repo) ListMemberships(ctx context.Context, payerID int64, limit int) (
	[]Membership, error) {
	rows, err := r.pool.Query(ctx,
		membershipCols+`where m.payer_account_id = $1
		order by m.expire_at desc limit $2`, payerID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Membership
	for rows.Next() {
		m, err := scanMembership(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ExpireMemberships 把过了期的行标成 expired（供列表与统计用）。
//
// ⚠️ 这个作业**不决定权益**（见 CurrentMembership 的注释）——
// 它停了不会有人多拿权益，只是列表里的状态字面值滞后。
func (r *Repo) ExpireMemberships(ctx context.Context, limit int) (int, error) {
	tag, err := r.pool.Exec(ctx, `
		update membership set status = 'expired', updated_at = now()
		where id in (select id from membership
			where status = 'active' and expire_at <= now() limit $1)`, limit)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

// UpgradePlanOf 把套餐行翻成升级报价要的纯数据。
//
// ⚠️ 放在 repo 而不是 app：翻译两个相邻层的类型属于边界胶水，
// 散到 app 里的话每个调用点都要抄一遍，而抄漏一个字段（比如 LevelSort）
// 的后果是降级判定失效 —— 用户能"降级"到一个更便宜的等级并且不用补钱。
func UpgradePlanOf(p Plan) domain.UpgradePlan {
	return domain.UpgradePlan{
		LevelID: p.LevelID, LevelSort: p.LevelSort, Period: p.Period,
		PriceCents: p.PriceCents, GiftCredits: p.GiftCredits,
	}
}
