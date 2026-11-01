// Package repo 是整图 DAG 的数据访问（P08-02）。
package repo

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrNotFound  = errors.New("运行记录不存在")
	ErrConflict  = errors.New("运行状态已变")
	ErrNodeNotFd = errors.New("节点运行记录不存在")
)

type Repo struct{ pool *pgxpool.Pool }

func New(pool *pgxpool.Pool) *Repo { return &Repo{pool: pool} }

// Run 是一次整图运行。
type Run struct {
	ID              int64
	WorkspaceID     int64
	ProjectID       int64
	PayerAccountID  int64
	CreatedByUserID int64
	// GraphSnapshot 固定本次运行的图版本、参数与资源引用（NEW-03）。
	//
	// ⚠️ 没有它就回答不了"这次运行到底执行的是什么"：用户在运行期间
	// 改了画布，事后看到的是改后的图，而跑的是改前的 —— 对不上账，
	// 也复现不了问题。
	GraphSnapshot json.RawMessage
	TargetNodeIDs json.RawMessage
	Status        string
	TotalCredits  int
}

const runCols = `select id, workspace_id, project_id, payer_account_id,
	created_by_user_id, graph_snapshot, target_node_ids, status, total_credits
	from workflow_run `

func scanRun(row pgx.Row) (Run, error) {
	var r Run
	err := row.Scan(&r.ID, &r.WorkspaceID, &r.ProjectID, &r.PayerAccountID,
		&r.CreatedByUserID, &r.GraphSnapshot, &r.TargetNodeIDs, &r.Status, &r.TotalCredits)
	if errors.Is(err, pgx.ErrNoRows) {
		return Run{}, ErrNotFound
	}
	return r, err
}

// NodeRun 是运行里的一个节点。
type NodeRun struct {
	ID        int64
	RunID     int64
	NodeID    string
	DependsOn json.RawMessage
	Status    string
	// GenerationID 是这个节点对应的生成任务（子任务引用）。
	//
	// ⚠️ 节点**不自己调 Provider**：重复消息与进程重启续跑因此落到
	// generation 已有的幂等与 CAS 上，不重复扣费（NEW-05）。
	GenerationID int64
	Attempt      int
	CreditsCost  int
	IsReused     bool
	ErrorCode    string
	ErrorMessage string
}

const nodeCols = `select id, run_id, node_id, depends_on, status,
	coalesce(generation_id,0), attempt, credits_cost, is_reused,
	coalesce(error_code,''), coalesce(error_message,'')
	from workflow_node_run `

func scanNode(row pgx.Row) (NodeRun, error) {
	var n NodeRun
	err := row.Scan(&n.ID, &n.RunID, &n.NodeID, &n.DependsOn, &n.Status,
		&n.GenerationID, &n.Attempt, &n.CreditsCost, &n.IsReused,
		&n.ErrorCode, &n.ErrorMessage)
	if errors.Is(err, pgx.ErrNoRows) {
		return NodeRun{}, ErrNodeNotFd
	}
	return n, err
}

// ---------------------------------------------------------------- 建运行

// InsertRun 建运行并在**同一个事务**里写入全部节点。
//
// ⚠️ 必须一个事务：分两次写的话，节点插一半失败会留下一个**空运行** ——
// 它永远不会推进（没有节点可调度），而用户只看到一次"运行中"卡死在那里。
func (r *Repo) InsertRun(ctx context.Context, run Run, nodes []NodeRun) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `
		insert into workflow_run (id, workspace_id, project_id, payer_account_id,
			created_by_user_id, graph_snapshot, target_node_ids, status, started_at)
		values ($1,$2,$3,$4,$5,$6,coalesce($7,'[]'::jsonb),$8,now())`,
		run.ID, run.WorkspaceID, run.ProjectID, run.PayerAccountID,
		run.CreatedByUserID, run.GraphSnapshot, run.TargetNodeIDs, run.Status); err != nil {
		return err
	}
	for _, n := range nodes {
		if _, err := tx.Exec(ctx, `
			insert into workflow_node_run (id, run_id, node_id, depends_on, status,
				credits_cost, is_reused)
			values ($1,$2,$3,coalesce($4,'[]'::jsonb),$5,$6,$7)`,
			n.ID, n.RunID, n.NodeID, n.DependsOn, n.Status,
			n.CreditsCost, n.IsReused); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// GetRun 取运行（带工作区归属校验）。
func (r *Repo) GetRun(ctx context.Context, workspaceID, id int64) (Run, error) {
	return scanRun(r.pool.QueryRow(ctx,
		runCols+`where id=$1 and workspace_id=$2`, id, workspaceID))
}

// GetRunByID 不带归属校验 —— 仅供 Worker 回写用（它没有用户上下文）。
func (r *Repo) GetRunByID(ctx context.Context, id int64) (Run, error) {
	return scanRun(r.pool.QueryRow(ctx, runCols+`where id=$1`, id))
}

// ListRuns 列出项目的运行历史（最近在前）。
func (r *Repo) ListRuns(ctx context.Context, workspaceID, projectID int64, limit int) (
	[]Run, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := r.pool.Query(ctx, runCols+
		`where workspace_id=$1 and project_id=$2 order by created_at desc limit $3`,
		workspaceID, projectID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		v, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------- 节点

// ListNodes 列出运行内的全部节点（按 node_id 稳定排序）。
func (r *Repo) ListNodes(ctx context.Context, runID int64) ([]NodeRun, error) {
	rows, err := r.pool.Query(ctx, nodeCols+`where run_id=$1 order by node_id`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []NodeRun
	for rows.Next() {
		v, err := scanNode(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// ClaimNode 把节点从 pending 抢成 running。
//
// ⚠️ **带状态条件（CAS）**：两个调度动作同时看到同一个节点就绪时，
// 只有一个能抢到。没有它就是同一个节点被提交两次 —— 两个生成任务、两份钱。
// 返回 false 表示别人已经抢走了，调用方什么都不要做。
//
// ⚠️ 返回**抢到之后的 attempt**：幂等键要带上它。不带的话重试会命中
// 上一次那条失败的 generation（受理层认为是重复提交），
// 于是用户点多少次"重试"都是同一个失败结果 —— 他再也跑不动这个节点了
// （与 CD-34 里脚本轮次那条是同一个坑）。
func (r *Repo) ClaimNode(ctx context.Context, runID int64, nodeID string) (bool, int, error) {
	var attempt int
	err := r.pool.QueryRow(ctx, `
		update workflow_node_run set status='running', attempt=attempt+1,
			started_at=coalesce(started_at, now()), updated_at=now()
		where run_id=$1 and node_id=$2 and status='pending'
		returning attempt`, runID, nodeID).Scan(&attempt)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, 0, nil
	}
	if err != nil {
		return false, 0, err
	}
	return true, attempt, nil
}

// AttachGeneration 把生成任务挂到节点上（子任务引用）。
func (r *Repo) AttachGeneration(ctx context.Context, runID int64, nodeID string,
	generationID int64, credits int) error {
	_, err := r.pool.Exec(ctx, `
		update workflow_node_run set generation_id=$3, credits_cost=$4, updated_at=now()
		where run_id=$1 and node_id=$2`, runID, nodeID, generationID, credits)
	return err
}

// FinishNode 把节点推到终态。
//
// ⚠️ 只在**非终态**时生效：晚到的结果不能把已经取消的节点改成成功，
// 也不能把重试过的旧任务结果盖到新的上面。
func (r *Repo) FinishNode(ctx context.Context, runID int64, nodeID, status,
	errCode, errMsg string) (bool, error) {
	tag, err := r.pool.Exec(ctx, `
		update workflow_node_run set status=$3::varchar,
			error_code = nullif($4,''), error_message = nullif(left($5,1024),''),
			finished_at = now(), updated_at = now()
		where run_id=$1 and node_id=$2
		  and status not in ('success','failed','skipped','blocked','cancelled')`,
		runID, nodeID, status, errCode, errMsg)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// MarkNodes 批量把一组节点置成某个状态（用于 blocked 传播）。
func (r *Repo) MarkNodes(ctx context.Context, runID int64, nodeIDs []string,
	status, msg string) error {
	if len(nodeIDs) == 0 {
		return nil
	}
	_, err := r.pool.Exec(ctx, `
		update workflow_node_run set status=$3::varchar,
			error_message = nullif(left($4,1024),''), finished_at = now(), updated_at = now()
		where run_id=$1 and node_id = any($2)
		  and status not in ('success','failed','skipped','blocked','cancelled')`,
		runID, nodeIDs, status, msg)
	return err
}

// NodeByGeneration 按生成任务反查节点 —— Worker 回写时用。
func (r *Repo) NodeByGeneration(ctx context.Context, generationID int64) (NodeRun, error) {
	return scanNode(r.pool.QueryRow(ctx,
		nodeCols+`where generation_id=$1`, generationID))
}

// ---------------------------------------------------------------- 运行状态

// FinishRun 按**当前状态条件**推进运行的终态。
//
// ⚠️ 带条件：晚到的回写不能把已经取消的运行改成成功。
func (r *Repo) FinishRun(ctx context.Context, runID int64, status string,
	totalCredits int) error {
	_, err := r.pool.Exec(ctx, `
		update workflow_run set status=$2::varchar, total_credits=$3,
			finished_at = case when $2::varchar in ('success','partial_success','failed','cancelled')
			                   then now() else finished_at end,
			updated_at = now()
		where id=$1 and status in ('pending','running')`, runID, status, totalCredits)
	return err
}

// StaleRuns 找出还在跑的运行 —— 进程重启后的恢复扫描用（NEW-05）。
func (r *Repo) StaleRuns(ctx context.Context, limit int) ([]Run, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := r.pool.Query(ctx, runCols+
		`where status in ('pending','running') order by created_at limit $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		v, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// ResetNodes 把一批节点放回 pending，供重试用。
//
// ⚠️ 只动**已经不跑了且没成功**的：把 running 的放回 pending 会让它
// 被再次派发 —— 同一个节点两个任务、两份钱；把 success 的放回去
// 则是在花用户的钱做他没要的事。
//
// ⚠️ 顺手清掉上一次的错误与 generation 引用：留着的话界面上
// 一个"排队中"的节点还挂着上次的失败原因，用户以为又失败了。
func (r *Repo) ResetNodes(ctx context.Context, runID int64, nodeIDs []string) (int, error) {
	if len(nodeIDs) == 0 {
		return 0, nil
	}
	tag, err := r.pool.Exec(ctx, `
		update workflow_node_run set status='pending', generation_id=null,
			error_code=null, error_message=null, finished_at=null, updated_at=now()
		where run_id=$1 and node_id = any($2)
		  and status in ('failed','blocked','cancelled')`, runID, nodeIDs)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

// ReopenRun 把已经结束的运行放回 running（重试时用）。
//
// ⚠️ 不放回去的话，重试的节点跑完也没人推进 —— `StaleRuns` 只捞
// pending/running，一个 success 的运行里躺着一个 pending 节点，
// 它永远等不到派发。
func (r *Repo) ReopenRun(ctx context.Context, runID int64) error {
	_, err := r.pool.Exec(ctx, `
		update workflow_run set status='running', finished_at=null, updated_at=now()
		where id=$1 and status in ('success','partial_success','failed')`, runID)
	return err
}

// CancelRun 取消运行：标记运行本身，并把**还没跑完的**节点标成 cancelled。
//
// ⚠️ 一条 SQL 里连节点一起标，而不是先查再逐条更新：
// 中间那一刻新派发的节点会漏网，于是"取消了但还有任务在跑"。
// 已经在跑的 generation 由调用方单独取消（那要走退款）。
func (r *Repo) CancelRun(ctx context.Context, runID int64, reason string) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `
		update workflow_node_run set status='cancelled',
			error_message = nullif(left($2,1024),''), finished_at=now(), updated_at=now()
		where run_id=$1 and status in ('pending','running')`, runID, reason); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		update workflow_run set status='cancelled', finished_at=now(), updated_at=now()
		where id=$1 and status in ('pending','running')`, runID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// MarkSkipped 把一个节点标成 skipped（用户主动跳过）。
func (r *Repo) MarkSkipped(ctx context.Context, runID int64, nodeID string) (bool, error) {
	tag, err := r.pool.Exec(ctx, `
		update workflow_node_run set status='skipped', error_message=null,
			finished_at=now(), updated_at=now()
		where run_id=$1 and node_id=$2 and status in ('pending','failed','blocked')`,
		runID, nodeID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// ActiveGenerations 列出运行内还在跑的节点的生成任务（取消时要逐个取消）。
func (r *Repo) ActiveGenerations(ctx context.Context, runID int64) ([]int64, error) {
	rows, err := r.pool.Query(ctx, `
		select generation_id from workflow_node_run
		where run_id=$1 and status in ('pending','running') and generation_id is not null
		order by node_id`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
