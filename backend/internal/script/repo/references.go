package repo

import "context"

// MediaReferences 回答「这个媒体还被哪些脚本资产挂着」（P14-03）。
//
// ⚠️ 实现 `media/app.ReferenceChecker`，是**计算式**的删除保护：
// 当场查表，不依赖任何人在写入时记得去登记 `media_usage`。
//
// ⚠️ **不按工作区过滤**：这个查询要回答的是"还有没有人在用"，
// 按工作区过滤会漏掉引用，而漏掉的后果是把还在用的文件删了。
// 它只回 ID 不回内容，所以不构成跨租户信息泄漏。
//
// ⚠️ **不按 status 过滤**：`script_asset.status` 的取值是
// pending/generating/ready/failed，**没有一个代表"这个资产不要了"** ——
// 挂着 media 的资产无论什么状态都还在用户的资产列表里。
// 按状态筛等于凭空发明一个"已弃用"的语义，而那会让某些状态下的资产
// 挡不住删除 —— 表现为"有的图删得掉、有的删不掉"，而用户看不出规律。
func (r *Repo) MediaReferences(ctx context.Context, mediaID int64) ([]int64, error) {
	rows, err := r.pool.Query(ctx, `
		select id from script_asset where media_object_id = $1`, mediaID)
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
