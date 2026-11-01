package repo

import (
	"context"
)

// TaskSnapshot 是给画布 Agent 扫描循环（AGT-25）看的一次任务快照：
// 状态 + 失败原因 + 全部产物 + 账本上「扣过没有 / 退过没有」。
//
// ⚠️ 退款事实只认账本（credit_transaction），不认 generation.credits_cost：
// 后者是报价，失败退款之后不会变（CD-60 同一条）。
type TaskSnapshot struct {
	ID           int64
	Status       string
	ErrorCode    string
	ErrorMessage string
	CreditsCost  int
	// Consumed：账本上有这次任务的消费流水。
	Consumed bool
	// Refunded：账本上有这次任务的退款流水。
	Refunded bool
	// Outputs 只在成功时取（全部产物，按 idx 升序）。
	Outputs []Output
}

// TaskSnapshots 批量取任务快照（避免扫描循环逐个查 —— N+1）。
//
// ⚠️ 带 workspace_id 条件：步骤上的 generation_id 来自 Agent 自己的受理，
// 但归属仍按工作区卡一道（CD-04），查不到的任务调用方按「不存在」处理。
func (r *Repo) TaskSnapshots(ctx context.Context, workspaceID int64, ids []int64) ([]TaskSnapshot, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := r.pool.Query(ctx, `
		select g.id, g.status, coalesce(g.error_code, ''), coalesce(g.error_message, ''), g.credits_cost,
		       exists (select 1 from credit_transaction t
		               where t.ref_type = 'generation' and t.ref_id = g.id::text and t.type = 'consume'),
		       exists (select 1 from credit_transaction t
		               where t.ref_type = 'generation' and t.ref_id = g.id::text and t.type = 'refund')
		from generation g
		where g.workspace_id = $1 and g.id = any($2::bigint[])
		order by g.id`, workspaceID, ids)
	if err != nil {
		return nil, err
	}
	var out []TaskSnapshot
	index := map[int64]int{}
	var successIDs []int64
	for rows.Next() {
		var s TaskSnapshot
		if err := rows.Scan(&s.ID, &s.Status, &s.ErrorCode, &s.ErrorMessage, &s.CreditsCost,
			&s.Consumed, &s.Refunded); err != nil {
			rows.Close()
			return nil, err
		}
		index[s.ID] = len(out)
		out = append(out, s)
		if s.Status == "success" {
			successIDs = append(successIDs, s.ID)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(successIDs) == 0 {
		return out, nil
	}
	orows, err := r.pool.Query(ctx, `
		select generation_id, idx, output_type, coalesce(media_object_id, 0), coalesce(text_content, ''), meta
		from generation_output where generation_id = any($1::bigint[])
		order by generation_id, idx`, successIDs)
	if err != nil {
		return nil, err
	}
	defer orows.Close()
	for orows.Next() {
		var gid int64
		var o Output
		if err := orows.Scan(&gid, &o.Idx, &o.OutputType, &o.MediaObjectID, &o.TextContent, &o.Meta); err != nil {
			return nil, err
		}
		if i, ok := index[gid]; ok {
			out[i].Outputs = append(out[i].Outputs, o)
		}
	}
	return out, orows.Err()
}
