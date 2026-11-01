package repo

import "context"

// NodeLatest 是某个画布节点**最近一次**任务（任意状态）的摘要（AGT-23 read_canvas）。
//
// OutputMediaID / OutputType 是第一个产物（idx 最小）；没有产物时为 0 / 空串。
type NodeLatest struct {
	NodeID        string
	GenerationID  int64
	Status        string
	ErrorCode     string
	OutputMediaID int64
	OutputType    string
}

// LatestByNode 取项目内每个节点最近一次任务（**任意状态**）与它的第一个产物。
//
// ⚠️ 与 LatestSuccessByNode 是两个问题：那个回答「有没有可复用的产出」（只取 success），
// 这个回答「这个节点现在怎么样了」—— 最近一次失败了、还在跑，都是 Agent 需要知道的事实。
// 只取 success 的话，一个刚失败的节点会显示成上一次的成功结果，Agent 会以为不用重跑。
//
// ⚠️ 带 workspace 条件（CD-04）：调用方传的 projectID 已在本工作区校验过，这里仍不放宽。
// ⚠️ 产物用 **left join**：排队中 / 失败的任务没有产物，内连接会让这些行整条消失。
// 最近一次按 created_at（受理顺序）而不是 finished_at：还在跑的任务没有 finished_at。
func (r *Repo) LatestByNode(ctx context.Context, workspaceID, projectID int64) ([]NodeLatest, error) {
	rows, err := r.pool.Query(ctx, `
		select l.node_id, l.id, l.status, l.error_code,
		       coalesce(o.media_object_id, 0), coalesce(o.output_type, '')
		from (
			select distinct on (g.node_id) g.node_id, g.id, g.status, coalesce(g.error_code, '') as error_code
			from generation g
			where g.workspace_id = $1 and g.project_id = $2 and g.node_id is not null
			order by g.node_id, g.created_at desc, g.id desc
		) l
		left join lateral (
			select go.media_object_id, go.output_type from generation_output go
			where go.generation_id = l.id order by go.idx limit 1
		) o on true
		order by l.node_id`, workspaceID, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []NodeLatest
	for rows.Next() {
		var n NodeLatest
		if err := rows.Scan(&n.NodeID, &n.GenerationID, &n.Status, &n.ErrorCode,
			&n.OutputMediaID, &n.OutputType); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}
