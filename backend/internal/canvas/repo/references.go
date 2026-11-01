package repo

import "context"

// MediaReferences 回答「这个媒体还是不是某个项目的封面」（P14-03）。
//
// ⚠️ 实现 `media/app.ReferenceChecker`，是**计算式**的删除保护。
// 封面在设置时会登记 `media_usage`，但那条路径已经出现过一次"改封面忘了
// 先解除旧引用"的形态（service.go 里先 Release 再 Register），
// 而登记式的错误一律表现为"保护静默失效"。
//
// ⚠️ **含回收站里的项目，所以这里不按 status 过滤**：项目是软删的，
// 30 天内可以恢复（CD-56），`status` 只有 active / deleted 两个值。
// 只算 active 的话，用户删了项目、又删了封面图、再恢复项目 ——
// 项目回来了而封面是裂的，**而这三步他每一步都做对了**。
// 项目行真正消失时（回收站清理）封面引用也跟着消失，这里自然就放行了。
func (r *Repo) MediaReferences(ctx context.Context, mediaID int64) ([]int64, error) {
	rows, err := r.pool.Query(ctx, `
		select id from project where cover_media_id = $1`, mediaID)
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
