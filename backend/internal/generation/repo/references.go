package repo

import "context"

// MediaReferences 回答「这个媒体还是不是某次生成的产物」（P14-03）。
//
// ⚠️ 实现 `media/app.ReferenceChecker`，是**计算式**的删除保护。
// 产物在归档时也会往 `media_usage` 登记一条，但那是**登记式**的 ——
// 归档路径将来要是多出一条分支而没跟着登记，保护就悄悄失效了。
// 这一条当场查 `generation_output`，与"有没有人记得登记"无关。
//
// 产物被删掉的后果比素材更重：作品墙、任务详情、时间线装配都按 mediaId 取它，
// 而用户在素材库里根本看不到产物（那张列表只列 upload_reference），
// 所以他不会意识到自己删掉的是一件作品的唯一副本。
func (r *Repo) MediaReferences(ctx context.Context, mediaID int64) ([]int64, error) {
	rows, err := r.pool.Query(ctx, `
		select generation_id from generation_output where media_object_id = $1`, mediaID)
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
