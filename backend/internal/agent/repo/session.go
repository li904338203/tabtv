// Package repo 是画布 Agent 的数据访问层（迁移 00040 / 00041）。
//
// ⚠️ 只读写 agent_* 表与平台的 outbox_message；**不 import 任何其他域的 repo 包**
// （dev/01 T-1：agent 只做编排，跨域一律走 app 端口）。
// ⚠️ 每个状态推进方法都是带当前状态条件的 `update ... where id=$1 and status=$2`（CAS），
// 返回「本次是否真的改了状态」—— 调用方必须看它：不看的话重复消息会退两次款、
// 晚到的结果会覆盖终态（§5.3）。
// ID 一律由调用方用雪花发号器生成后传入（同其他域的 repo）；
// jsonb 列用 json.RawMessage 原样存取，保留未知字段（§5.4）。
package repo

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/li904338203/tabtv/backend/internal/agent/domain"
)

var (
	ErrNotFound  = errors.New("资源不存在")
	ErrDuplicate = errors.New("记录已存在")
)

// Querier 是连接池与事务的公共子集：既能在事务里调，也能直接在池上调。
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

type Repo struct{ pool *pgxpool.Pool }

func New(pool *pgxpool.Pool) *Repo { return &Repo{pool: pool} }

// Begin 开一个事务，给需要「锁会话行 + 多步写入」的调用方用。
func (r *Repo) Begin(ctx context.Context) (pgx.Tx, error) { return r.pool.Begin(ctx) }

func isDuplicate(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

func mapDuplicate(err error) error {
	if isDuplicate(err) {
		return ErrDuplicate
	}
	return err
}

func jsonOr(v json.RawMessage, def string) json.RawMessage {
	if len(v) == 0 {
		return json.RawMessage(def)
	}
	return v
}

func nullStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// Session 是 agent_session 的一行。
type Session struct {
	ID               int64
	WorkspaceID      int64
	ProjectID        int64
	PayerAccountID   int64
	CreatedByUserID  int64
	Title            string
	State            domain.State
	SkillCodes       json.RawMessage
	Prefs            json.RawMessage
	Todo             json.RawMessage
	Summary          string
	ApprovedBudget   int
	PlanDocVersion   *int
	RunnerID         string
	RunnerLeaseUntil *time.Time
	StopRequestedAt  *time.Time
	// CreateIdempotencyKey 为空表示建会话的请求没带幂等键。
	CreateIdempotencyKey string
	// Scene 会话场景（MKT-T-17，迁移 00061）：建后不可改，没有任何更新方法写它。
	Scene domain.Scene
	// SceneInput 营销会话的输入快照（商品图 ID、名称卖点、角色、选择项 (code,version)）；
	// 通用会话为 `{}`。原样存取，形状见 agent/app.SceneInput。
	SceneInput   json.RawMessage
	LastActiveAt time.Time
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

const sessionCols = `id, workspace_id, project_id, payer_account_id, created_by_user_id, title, state,
	skill_codes, prefs, todo, coalesce(summary, ''), approved_budget, plan_doc_version,
	coalesce(runner_id, ''), runner_lease_until, stop_requested_at,
	coalesce(create_idempotency_key, ''), last_active_at, created_at, updated_at, scene, scene_input`

func scanSession(row pgx.Row) (Session, error) {
	var s Session
	err := row.Scan(&s.ID, &s.WorkspaceID, &s.ProjectID, &s.PayerAccountID, &s.CreatedByUserID,
		&s.Title, &s.State, &s.SkillCodes, &s.Prefs, &s.Todo, &s.Summary, &s.ApprovedBudget,
		&s.PlanDocVersion, &s.RunnerID, &s.RunnerLeaseUntil, &s.StopRequestedAt,
		&s.CreateIdempotencyKey, &s.LastActiveAt, &s.CreatedAt, &s.UpdatedAt, &s.Scene, &s.SceneInput)
	if errors.Is(err, pgx.ErrNoRows) {
		return Session{}, ErrNotFound
	}
	return s, err
}

// Create 建会话。幂等键撞上已有会话时返回 ErrDuplicate，调用方再用 GetByCreateKey 取回。
//
// 通常在「建会话 → 写首条消息 → 建轮次 → outbox」的事务里调用（dev/01 §9）。
func (r *Repo) Create(ctx context.Context, q Querier, s Session) (Session, error) {
	state := s.State
	if state == "" {
		state = domain.StateIdle
	}
	scene := s.Scene
	if scene == "" {
		scene = domain.SceneGeneral
	}
	row := q.QueryRow(ctx, `
		insert into agent_session (id, workspace_id, project_id, payer_account_id, created_by_user_id,
			title, state, skill_codes, prefs, todo, approved_budget, create_idempotency_key, scene, scene_input)
		values ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
		returning `+sessionCols,
		s.ID, s.WorkspaceID, s.ProjectID, s.PayerAccountID, s.CreatedByUserID, s.Title, state,
		jsonOr(s.SkillCodes, "[]"), jsonOr(s.Prefs, "{}"), jsonOr(s.Todo, "[]"), s.ApprovedBudget,
		nullStr(s.CreateIdempotencyKey), scene, jsonOr(s.SceneInput, "{}"))
	out, err := scanSession(row)
	if err != nil {
		return Session{}, mapDuplicate(err)
	}
	return out, nil
}

// GetByCreateKey 按建会话的幂等键取已建的会话。
func (r *Repo) GetByCreateKey(ctx context.Context, workspaceID int64, key string) (Session, error) {
	return scanSession(r.pool.QueryRow(ctx, `select `+sessionCols+` from agent_session
		where workspace_id = $1 and create_idempotency_key = $2`, workspaceID, key))
}

// Get 按工作区取会话。⚠️ 带 workspace_id 条件：请求头里的工作区是环境权限（CD-04）。
func (r *Repo) Get(ctx context.Context, workspaceID, id int64) (Session, error) {
	return scanSession(r.pool.QueryRow(ctx, `select `+sessionCols+` from agent_session
		where id = $1 and workspace_id = $2`, id, workspaceID))
}

// GetByID 不带工作区条件，**只给 Worker 用**（消息里只有会话 ID，归属在受理时已校验）。
func (r *Repo) GetByID(ctx context.Context, id int64) (Session, error) {
	return scanSession(r.pool.QueryRow(ctx, `select `+sessionCols+` from agent_session where id = $1`, id))
}

// ListByProject 列项目下的会话（含他人发起的），最近活跃在前。
func (r *Repo) ListByProject(ctx context.Context, workspaceID, projectID int64, limit int) ([]Session, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := r.pool.Query(ctx, `select `+sessionCols+` from agent_session
		where workspace_id = $1 and project_id = $2
		order by last_active_at desc, id desc limit $3`, workspaceID, projectID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Session
	for rows.Next() {
		s, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// LockForUpdate 在事务里锁住会话行并读出来。
//
// 额度检查 + 预留、消息 seq 分配都依赖这把锁：并发的付费工具 / 并发发消息在这里排队。
func (r *Repo) LockForUpdate(ctx context.Context, tx pgx.Tx, id int64) (Session, error) {
	return scanSession(tx.QueryRow(ctx, `select `+sessionCols+` from agent_session
		where id = $1 for update`, id))
}

// TransitionState CAS 推进会话状态：只有当前状态仍是 from 时才改成 to。
//
// ⚠️ 迁移是否合法由 domain.CanTransition 决定，这里只保证「不在别人改过之后再改一次」。
func (r *Repo) TransitionState(ctx context.Context, q Querier, id int64, from, to domain.State) (bool, error) {
	// 从「没有轮在跑」进入「有轮在跑」时清掉上一次的停止请求（04 文档 P-06 / P-18）：
	//   - 那次停止针对的是当时在跑的那一轮，它已经结束了。留着的话，详情接口的 stopRequested
	//     从此恒为 true，前端的停止键永久不可用；
	//   - 按停止之前就已排队的下一轮，是用户的另一句话，不该一开始就被这次停止停掉。
	// 运行中（planning / executing 之间）迁移不清：那次停止仍针对正在跑的这一轮。
	clearStop := domain.HasActiveTurn(to) && !domain.HasActiveTurn(from)
	tag, err := q.Exec(ctx, `
		update agent_session set state = $3, updated_at = now(), last_active_at = now(),
			stop_requested_at = case when $4 then null else stop_requested_at end
		where id = $1 and state = $2`, id, from, to, clearStop)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// ClaimLease 取运行租约（dev/01 §4.2 第 1 步）：没人持有或已过期才取得到。
//
// 0 行 = 已有别的 Worker 在跑这个会话，调用方 ack 丢弃（at-least-once 的正常结局）。
func (r *Repo) ClaimLease(ctx context.Context, id int64, runnerID string, ttl time.Duration) (bool, error) {
	tag, err := r.pool.Exec(ctx, `
		update agent_session
		set runner_id = $2, runner_lease_until = now() + $3::bigint * interval '1 millisecond', updated_at = now()
		where id = $1 and (runner_lease_until is null or runner_lease_until < now())`,
		id, runnerID, ttl.Milliseconds())
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// RenewLease 续租约：只有当前持有者能续。false = 租约已被别人抢走，调用方应立即停止。
func (r *Repo) RenewLease(ctx context.Context, id int64, runnerID string, ttl time.Duration) (bool, error) {
	tag, err := r.pool.Exec(ctx, `
		update agent_session
		set runner_lease_until = now() + $3::bigint * interval '1 millisecond', updated_at = now()
		where id = $1 and runner_id = $2 and runner_lease_until is not null`,
		id, runnerID, ttl.Milliseconds())
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// ReleaseLease 释放租约：只释放自己持有的那一份。
func (r *Repo) ReleaseLease(ctx context.Context, id int64, runnerID string) (bool, error) {
	tag, err := r.pool.Exec(ctx, `
		update agent_session set runner_id = null, runner_lease_until = null, updated_at = now()
		where id = $1 and runner_id = $2`, id, runnerID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// RequestStop 记一次「停止当前轮」请求（AG-19）。Runner 在流式回调与每次工具前检查。
// 已取消的会话不再记（返回 false）。
func (r *Repo) RequestStop(ctx context.Context, id int64) (bool, error) {
	tag, err := r.pool.Exec(ctx, `
		update agent_session set stop_requested_at = now(), updated_at = now()
		where id = $1 and state <> 'cancelled'`, id)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// ClearStop 清掉停止请求（新一轮开始时）。
func (r *Repo) ClearStop(ctx context.Context, q Querier, id int64) error {
	_, err := q.Exec(ctx, `update agent_session set stop_requested_at = null where id = $1`, id)
	return err
}

// SetApprovedBudget 写批准额度（规划确认卡：= 报价合计），同时记批准时规划文档的版本。
// 必须在锁住会话行的事务里调用。planDocVersion=nil 表示清空（规划作废）。
func (r *Repo) SetApprovedBudget(ctx context.Context, tx pgx.Tx, id int64, amount int, planDocVersion *int) error {
	tag, err := tx.Exec(ctx, `
		update agent_session set approved_budget = $2, plan_doc_version = $3, updated_at = now()
		where id = $1`, id, amount, planDocVersion)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// AddApprovedBudget 追加额度（追加额度卡）。必须在锁住会话行的事务里调用。返回新额度。
func (r *Repo) AddApprovedBudget(ctx context.Context, tx pgx.Tx, id int64, delta int) (int, error) {
	var out int
	err := tx.QueryRow(ctx, `
		update agent_session set approved_budget = approved_budget + $2, updated_at = now()
		where id = $1 returning approved_budget`, id, delta).Scan(&out)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrNotFound
	}
	return out, err
}

// UpdateTodo 写任务清单（进度条）。
func (r *Repo) UpdateTodo(ctx context.Context, id int64, todo json.RawMessage) error {
	return r.exec1(ctx, `update agent_session set todo = $2, updated_at = now() where id = $1`,
		id, jsonOr(todo, "[]"))
}

// UpdateSkills 写已加载的技能 code 列表。
func (r *Repo) UpdateSkills(ctx context.Context, id int64, skillCodes json.RawMessage) error {
	return r.exec1(ctx, `update agent_session set skill_codes = $2, updated_at = now() where id = $1`,
		id, jsonOr(skillCodes, "[]"))
}

// UpdateSummary 写截断后的早先对话摘要（空串 = 清空）。
func (r *Repo) UpdateSummary(ctx context.Context, id int64, summary string) error {
	return r.exec1(ctx, `update agent_session set summary = $2, updated_at = now() where id = $1`,
		id, nullStr(summary))
}

func (r *Repo) exec1(ctx context.Context, sql string, args ...any) error {
	tag, err := r.pool.Exec(ctx, sql, args...)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
