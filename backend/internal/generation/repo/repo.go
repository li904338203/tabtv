// Package repo 是生成任务的数据访问层。
package repo

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrNotFound  = errors.New("任务不存在")
	ErrDuplicate = errors.New("幂等键已存在")
)

type Repo struct{ pool *pgxpool.Pool }

func New(pool *pgxpool.Pool) *Repo { return &Repo{pool: pool} }

// Pool 暴露给需要开事务的调用方（受理要把写任务、扣费、outbox 放进同一个事务）。
func (r *Repo) Pool() *pgxpool.Pool { return r.pool }

type Generation struct {
	ID              int64
	WorkspaceID     int64
	PayerAccountID  int64
	CreatedByUserID int64
	ProjectID       int64
	NodeID          string
	// GroupItemID 非 0 表示这次任务属于某个组的子项，终态要回写那一项。
	//
	// ⚠️ 与 ScriptAssetID 是同一类东西，也会犯同一个错：不带下去的话
	// 子项**永远停在 queued** —— 产物已经归档了，但组里那一格一直转圈。
	GroupItemID int64
	// ShotID 非 0 表示这次任务产出属于某个镜头（视频组要按镜头找最新分镜图）。
	ShotID int64
	// InputFingerprint 是这次产出对应的输入指纹（整图复用判定用）。
	// 为空表示"不知道对应什么设置"，一律不复用。
	InputFingerprint string
	// ScriptAssetID 非 0 表示这次任务是在给某个脚本资产出图。
	// 成功后要回写资产状态 —— 没有这个字段的话，资产会永远停在 generating。
	ScriptAssetID  int64
	OperationID    int64
	ModelID        int64
	ResultType     string
	ParamsSnapshot json.RawMessage
	InputsSnapshot json.RawMessage
	RequestCount   int
	CreditsCost    int
	IdempotencyKey string
	RetryOfID      int64
	Status         string
	Progress       int
	Attempt        int
	ErrorCode      string
	ErrorMessage   string
	DeadlineAt     *time.Time
	CreatedAt      time.Time
	UpdatedAt      time.Time
	// SmartcutTarget 智能剪辑名下的任务要回写到哪里（`generation.smartcut_target`，docs/agent/smartcut SCD-32/33）：
	// `{timelineId, kind, mediaId?, clipId?, …}`，ID 一律字符串（索引按 `->>'timelineId'` 取）。为空表示不是。
	SmartcutTarget json.RawMessage
}

// InsertInTx 在调用方的事务里写入任务。
//
// ⚠️ 幂等靠 `uk_generation_idem(payer_account_id, idempotency_key)`：
// 并发重复提交时，后到的那个会在这里撞唯一索引并整体回滚 ——
// 连同它那一次扣费一起回滚，因为它们在同一个事务里（FLOW-01）。
func (r *Repo) InsertInTx(ctx context.Context, tx pgx.Tx, g Generation) error {
	_, err := tx.Exec(ctx, `
		insert into generation (id, workspace_id, payer_account_id, created_by_user_id,
			project_id, node_id, group_item_id, shot_id, script_asset_id,
			operation_id, model_id, result_type,
			params_snapshot, inputs_snapshot, request_count, credits_cost,
			idempotency_key, retry_of_id, status, deadline_at, input_fingerprint, smartcut_target)
		values ($1,$2,$3,$4,$5,nullif($6,''),
		        nullif($7::bigint,0), nullif($8::bigint,0), nullif($9::bigint,0),
		        $10,$11,$12,$13,$14,$15,$16,$17,
		        nullif($18::bigint,0),$19,$20,nullif($21,''),$22)`,
		g.ID, g.WorkspaceID, g.PayerAccountID, g.CreatedByUserID, g.ProjectID, g.NodeID,
		g.GroupItemID, g.ShotID, g.ScriptAssetID,
		g.OperationID, g.ModelID, g.ResultType, g.ParamsSnapshot,
		g.InputsSnapshot, g.RequestCount, g.CreditsCost, g.IdempotencyKey, g.RetryOfID,
		g.Status, g.DeadlineAt, g.InputFingerprint, nullJSON(g.SmartcutTarget))
	if isDuplicate(err) {
		return ErrDuplicate
	}
	return err
}

// FindByIdempotencyKey 取已存在的任务。
//
// ⚠️ 幂等重放必须返回**同一个任务**，而不是报错：客户端重试是正常行为，
// 报错会让它以为失败了、接着换个键再试一次，那才真的变成两次扣费（AC-04）。
func (r *Repo) FindByIdempotencyKey(ctx context.Context, payerID int64, key string) (Generation, error) {
	return scanOne(r.pool.QueryRow(ctx, selectCols+`
		from generation where payer_account_id = $1 and idempotency_key = $2`, payerID, key))
}

func (r *Repo) Get(ctx context.Context, workspaceID, id int64) (Generation, error) {
	return scanOne(r.pool.QueryRow(ctx, selectCols+`
		from generation where id = $1 and workspace_id = $2`, id, workspaceID))
}

// GetForOps 按 ID 取任务，**不带工作区条件**（P13-03）。
//
// ⚠️ 这是本仓储里唯一绕过租户条件的查询。后台运维**没有工作区上下文** ——
// 运维手上只有一个任务 ID，归属由 `ops:read`/`ops:write` 权限位把关。
//
// ⚠️ 名字里带 ForOps 是为了让调用处一眼看得出来：任何用户侧路径用到它
// 都是一个越权漏洞，而那个漏洞**不会报错**（同 `GetForShare` 的处理）。
func (r *Repo) GetForOps(ctx context.Context, id int64) (Generation, error) {
	return scanOne(r.pool.QueryRow(ctx, selectCols+`
		from generation where id = $1`, id))
}

const selectCols = `
	select id, workspace_id, payer_account_id, created_by_user_id, project_id,
	       coalesce(node_id,''), coalesce(group_item_id,0), coalesce(shot_id,0),
	       coalesce(script_asset_id,0), operation_id, model_id, result_type,
	       params_snapshot, inputs_snapshot, request_count, credits_cost,
	       idempotency_key, coalesce(retry_of_id,0), status, progress, attempt,
	       coalesce(error_code,''), coalesce(error_message,''),
	       deadline_at, created_at, updated_at, smartcut_target `

// scanTargets 是 `selectCols` 的扫描目标，**必须与它逐列对应**。
//
// ⚠️ 两者是一对，改一个就要改另一个。此前 `FindStale` 自己抄了一份
// 扫描列表，给 selectCols 加一列时只改了 scanOne —— 恢复扫描于是
// 每次都扫描失败、返回 0 个任务，而它只打一条日志就算了。
// 超时任务因此永远不会被判失败、永远不退款，而**没有任何东西是红的**。
// 现在只有这一份列表。
func scanTargets(g *Generation) []any {
	return []any{&g.ID, &g.WorkspaceID, &g.PayerAccountID, &g.CreatedByUserID,
		&g.ProjectID, &g.NodeID, &g.GroupItemID, &g.ShotID, &g.ScriptAssetID,
		&g.OperationID, &g.ModelID, &g.ResultType,
		&g.ParamsSnapshot, &g.InputsSnapshot, &g.RequestCount, &g.CreditsCost,
		&g.IdempotencyKey, &g.RetryOfID, &g.Status, &g.Progress, &g.Attempt,
		&g.ErrorCode, &g.ErrorMessage, &g.DeadlineAt, &g.CreatedAt, &g.UpdatedAt, &g.SmartcutTarget}
}

// nullJSON 让空的 jsonb 字段落成 NULL（而不是解析失败的空串）。
func nullJSON(b json.RawMessage) any {
	if len(b) == 0 {
		return nil
	}
	return []byte(b)
}

func scanOne(row pgx.Row) (Generation, error) {
	var g Generation
	err := row.Scan(scanTargets(&g)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return Generation{}, ErrNotFound
	}
	return g, err
}

// ListByWorkspace 列出任务，可按项目过滤。
func (r *Repo) ListByWorkspace(ctx context.Context, workspaceID, projectID int64,
	limit int, cursorID int64) ([]Generation, error) {
	sql := selectCols + `from generation where workspace_id = $1`
	args := []any{workspaceID}
	if projectID > 0 {
		args = append(args, projectID)
		sql += ` and project_id = $` + strconv.Itoa(len(args))
	}
	if cursorID > 0 {
		args = append(args, cursorID)
		sql += ` and id < $` + strconv.Itoa(len(args))
	}
	args = append(args, limit)
	sql += ` order by id desc limit $` + strconv.Itoa(len(args))

	rows, err := r.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Generation
	for rows.Next() {
		g, err := scanOne(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// InsertOutboxInTx 在同一个事务里写投递记录。
//
// ⚠️ 这是「写库成功则消息一定会发」的全部依据（GO-13）：
// 事务内直接 publish 到 MQ 的话，事务回滚了消息却已经发出去，
// Worker 会去执行一个并不存在的任务；先 commit 再 publish 的话，
// 两步之间崩溃就永远不会发。outbox 把这个二选一变成"稍后一定会发"。
func (r *Repo) InsertOutboxInTx(ctx context.Context, tx pgx.Tx, id int64,
	aggregateType string, aggregateID int64, topic string, payload json.RawMessage) error {
	_, err := tx.Exec(ctx, `
		insert into outbox_message (id, aggregate_type, aggregate_id, topic, payload)
		values ($1,$2,$3,$4,$5)`, id, aggregateType, aggregateID, topic, payload)
	return err
}

// OutboxMessage 是待投递消息。
type OutboxMessage struct {
	ID        int64
	Topic     string
	Payload   json.RawMessage
	Attempt   int
	Aggregate int64
}

// ClaimOutbox 取一批未投递的消息。
//
// `for update skip locked` 让多个投递器可以并行工作而不互相阻塞 ——
// 没有 skip locked 的话，第二个投递器会卡在第一个持有的行上直到它提交。
func (r *Repo) ClaimOutbox(ctx context.Context, limit int) ([]OutboxMessage, error) {
	rows, err := r.pool.Query(ctx, `
		select id, topic, payload, attempt, aggregate_id from outbox_message
		where published_at is null
		order by created_at limit $1
		for update skip locked`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OutboxMessage
	for rows.Next() {
		var m OutboxMessage
		if err := rows.Scan(&m.ID, &m.Topic, &m.Payload, &m.Attempt, &m.Aggregate); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// MarkOutboxPublished 标记已投递。
func (r *Repo) MarkOutboxPublished(ctx context.Context, id int64) error {
	_, err := r.pool.Exec(ctx,
		`update outbox_message set published_at = now() where id = $1`, id)
	return err
}

// MarkOutboxFailed 记一次失败，留给下一轮重试。
//
// ⚠️ 失败**不删除消息**：删了就等于这条消息永远不会发，而它对应的任务
// 已经扣过费、已经在库里 queued 了 —— 用户会看到一个永远排队的任务。
func (r *Repo) MarkOutboxFailed(ctx context.Context, id int64, reason string) error {
	_, err := r.pool.Exec(ctx, `
		update outbox_message set attempt = attempt + 1, last_error = left($2, 1024)
		where id = $1`, id, reason)
	return err
}

func isDuplicate(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// NodeSnapshot 是某个画布节点最近一次成功任务的快照。
type NodeSnapshot struct {
	NodeID         string
	GenerationID   int64
	OperationCode  string
	ModelCode      string
	ParamsSnapshot json.RawMessage
	InputsSnapshot json.RawMessage
	// InputFingerprint 是受理时**算一次存下来**的值。
	//
	// ⚠️ 不要试图从上面两个快照反算：计划器手上是画布节点的 params 与
	// 上游节点 ID，这里是解析后的 params 与产出 ID —— 表示形态不同，
	// 反算出来的值与计划器的**永远对不上**，于是复用永远不成立
	// （见迁移 00018 的说明）。
	InputFingerprint string
}

// LatestSuccessByNode 取项目内每个节点**最近一次成功**任务的快照。
//
// ⚠️ 只取最近一次：一个节点可能成功过很多次，全取回来调用方还得自己挑，
// 而"复用"的语义只关心最新那一次。
//
// ⚠️ 只取 success：失败/取消的任务没有可复用的产出，
// 把它们算进来会让计划器以为"已经有图了"。
func (r *Repo) LatestSuccessByNode(ctx context.Context, projectID int64) (
	[]NodeSnapshot, error) {
	// ⚠️ **left join 不是 join**：模型或操作被删掉之后，内连接会把这些行
	// 整条丢掉 —— 表现为"复用突然失效、每次都重跑"，而没有任何报错。
	// 用左连接的话码值为空，指纹因此与当前设置对不上，那个节点**重跑**，
	// 这是安全的方向（宁可多跑一次，也不能把一张对不上的图当成本次结果）。
	rows, err := r.pool.Query(ctx, `
		select distinct on (g.node_id)
		       g.node_id, g.id, coalesce(o.code,''), coalesce(m.code,''),
		       g.params_snapshot, g.inputs_snapshot, coalesce(g.input_fingerprint,'')
		from generation g
		left join operation o on o.id = g.operation_id
		left join model m on m.id = g.model_id
		where g.project_id = $1 and g.status = 'success' and g.node_id is not null
		order by g.node_id, g.finished_at desc nulls last, g.id desc`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []NodeSnapshot
	for rows.Next() {
		var s NodeSnapshot
		if err := rows.Scan(&s.NodeID, &s.GenerationID, &s.OperationCode, &s.ModelCode,
			&s.ParamsSnapshot, &s.InputsSnapshot, &s.InputFingerprint); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
