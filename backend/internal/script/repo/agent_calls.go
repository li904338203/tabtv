package repo

import "context"

// 画布 Agent（AGT-50）对同步文本调用的只读查询：一次合成会产生几十次调用，
// Agent 的额度条目要挂上「这一次合成」的全部调用，扫描循环据此按账本结算。

// MaxCallID 取脚本上某类调用当前最大的 ID（没有返回 0）。合成前取一次，合成后取大于它的那些。
//
// ⚠️ 雪花号单调递增，所以「大于合成前的最大 ID 且是同一发起人」就是这一次合成产生的调用；
// 按条数或时间截取的话，镜头一多（> 50）就漏掉一截，漏掉的那些失败退款不会进额度账。
func (r *Repo) MaxCallID(ctx context.Context, scriptID int64, purpose string) (int64, error) {
	var id int64
	err := r.pool.QueryRow(ctx, `select coalesce(max(id), 0) from script_llm_call
		where script_id = $1 and purpose = $2`, scriptID, purpose).Scan(&id)
	return id, err
}

// CallIDsAfter 列出某脚本上 afterID 之后、由 userID 发起的某类调用 ID（升序）。
func (r *Repo) CallIDsAfter(ctx context.Context, workspaceID, scriptID int64, purpose string,
	userID, afterID int64) ([]int64, error) {
	rows, err := r.pool.Query(ctx, `select id from script_llm_call
		where workspace_id = $1 and script_id = $2 and purpose = $3 and id > $4
		  and created_by_user_id = $5
		order by id`, workspaceID, scriptID, purpose, afterID, userID)
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

// CallStatuses 批量取同步调用的状态（带工作区条件，查不到的不在结果里）。
func (r *Repo) CallStatuses(ctx context.Context, workspaceID int64, ids []int64) (map[int64]CallStatus, error) {
	out := map[int64]CallStatus{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := r.pool.Query(ctx, `select id, status, coalesce(error_message, '')
		from script_llm_call where workspace_id = $1 and id = any($2::bigint[])`, workspaceID, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var c CallStatus
		if err := rows.Scan(&c.ID, &c.Status, &c.ErrorMessage); err != nil {
			return nil, err
		}
		out[c.ID] = c
	}
	return out, rows.Err()
}

// CallStatus 是一次同步调用的状态。
type CallStatus struct {
	ID           int64
	Status       string
	ErrorMessage string
}
