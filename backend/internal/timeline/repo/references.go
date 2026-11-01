package repo

import "context"

// MediaReferences 回答「这个媒体还被哪些时间线片段用着」（P14-03；智能剪辑 SCT-10 改口径）。
//
// ⚠️ 实现 `media/app.ReferenceChecker`，是**计算式**的删除保护：当场查表。
// 片段会被整批替换（删除 + 插入 + 重排都在一个事务里，CD-49），
// 登记式的 `media_usage` 在这种churn 下几乎必然会与实际脱节。
//
// ⚠️ 删掉一段成片正在用的素材，后果是**渲染时才失败**（渲染快照存 mediaId
// 不是 objectKey，CD-51）—— 用户已经排了队、扣了费，才看到"素材不存在"。
// 在删除入口挡住是唯一能在他还知道自己刚删了什么的时候告诉他的时机。
//
// 智能剪辑之后的口径（SCD-36、SCK-18）：
//   - 引用来源：片段的素材 + 片段的**备选**（切回备选时它原样进画面）；
//   - 只算还活着的时间线：`status='editing'`，所属项目还在（回收站里的也算，恢复之后要能用），
//     并且「没有绑定节点（旧时间线、脚本时间线）」或「绑定的节点仍在项目画布上」——
//     节点被删之后它是孤儿时间线，不再保护素材，随保留期回收（SC-F-06）；
//   - **快照不算**：快照只是「可以撤回去的样子」，算进去的话一份素材被 AI 剪辑换掉之后，
//     20 份快照会让它永远删不掉。
func (r *Repo) MediaReferences(ctx context.Context, mediaID int64) ([]int64, error) {
	rows, err := r.pool.Query(ctx, `
		select c.id
		from timeline_clip c
		join timeline_track k on k.id = c.track_id
		join timeline t on t.id = k.timeline_id
		join project p on p.id = t.project_id
		where (c.media_object_id = $1
		       or c.alternates @> jsonb_build_array(jsonb_build_object('mediaId', $1::text)))
		  and t.status = 'editing'
		  and (t.canvas_node_id is null
		       or jsonb_path_exists(p.canvas_data, '$.nodes[*] ? (@.id == $nid)',
		                            jsonb_build_object('nid', t.canvas_node_id)))`, mediaID)
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
