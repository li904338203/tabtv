package repo

import (
	"context"
	"strconv"
	"time"
)

// Creation 是「我的作品」里的一项：一次**成功**的生成。
//
// ⚠️ 与 `/works`（P13-02 的「提交去审核的作品」）**不是一回事**。老系统的「我的作品」
// 就是全部成功产出，移植过来的页面照老语义调 `/works`，而用户一件都没提交过 ——
// 于是这一页永远是空的。这里承接的是老页面的语义。
//
// ⚠️ 直接读 generation / project / model / generation_output：与任务中心（CD-62）
// 同一个理由 —— 门户聚合要在一条 SQL 里完成，筛选、计数、翻页才是同一批数据。
type Creation struct {
	ID          int64
	ProjectID   int64
	ProjectName string
	NodeID      string
	ResultType  string
	ModelCode   string
	ModelName   string
	// OutputCount 一次生成可能出好几张（GO-40）：卡片显示首项，同时说出一共几项，
	// 而不是隐式只给 outputs[0]、让另外几张已经扣过费的产物凭空消失。
	OutputCount int
	// 首个产物（按 idx）的类型与媒体 ID。
	CoverType  string
	CoverMedia int64
	CreatedAt  time.Time
}

// CreationTypes 是「创作资产」收录的产出类型 —— **只有图片、视频、音频**，文本不收。
//
// ⚠️ 「全部」的范围与「可以按哪几类筛」**共用这一份**（app 层校验筛选值时引用它）：
//
//	两处各写一份的话，哪天加了一类只改了一处 —— 要么「全部」里有、却筛不出来，
//	要么能筛、「全部」里却没有，而两边各自看都没错。
var CreationTypes = []string{"image", "video", "audio"}

// CreationFilter 限定「我的作品」的一次查询。
type CreationFilter struct {
	WorkspaceID int64
	// ResultType 为空表示「全部」= CreationTypes 里的三类（**不含文本**：
	// 创作资产展示的是画面与声音，一段文字不是「资产」，混进来只会把图挤到后面）。
	ResultType string
	// ProjectID 不为 0 时只要这个项目里的（资产库弹窗「生成历史 · 本项目」）。
	ProjectID int64
	Limit     int
	// CursorID 为 0 表示第一页；否则只取 id 更小的（按 id 倒序翻页）。
	CursorID int64
}

// creationWhere 是列表与计数**共用**的条件。
//
// ⚠️ 两处必须是同一套：分开写的话「共 N 项」和下面列出来的不是同一批 ——
// 而两个数字各自都"没错"（CD-62 任务中心、CD-56 订单汇总踩过同一条）。
func creationWhere(f CreationFilter, args []any) (string, []any) {
	args = append(args, f.WorkspaceID)
	sql := ` where g.workspace_id = $` + strconv.Itoa(len(args)) + ` and g.status = 'success'`
	if f.ProjectID > 0 {
		args = append(args, f.ProjectID)
		sql += ` and g.project_id = $` + strconv.Itoa(len(args))
	}
	if f.ResultType != "" {
		args = append(args, f.ResultType)
		sql += ` and g.result_type = $` + strconv.Itoa(len(args))
	} else {
		// ⚠️ 「全部」也要显式限定类型，不能省成「不筛」：
		//    不筛的话文本产出会混进列表与总数，而页面上并没有给文本的位置。
		args = append(args, CreationTypes)
		sql += ` and g.result_type = any($` + strconv.Itoa(len(args)) + `)`
	}
	return sql, args
}

// ListCreations 按 id 倒序列出成功的生成（索引见迁移 00032）。
func (r *Repo) ListCreations(ctx context.Context, f CreationFilter) ([]Creation, error) {
	where, args := creationWhere(f, nil)
	if f.CursorID > 0 {
		args = append(args, f.CursorID)
		where += ` and g.id < $` + strconv.Itoa(len(args))
	}
	args = append(args, f.Limit)
	rows, err := r.pool.Query(ctx, `
		select g.id, g.project_id, coalesce(p.name, ''), coalesce(g.node_id, ''), g.result_type,
		       coalesce(m.code, ''), coalesce(m.name, ''),
		       (select count(*)::int from generation_output c where c.generation_id = g.id),
		       coalesce(fo.output_type, ''), coalesce(fo.media_object_id, 0),
		       g.created_at
		from generation g
		-- left join：项目进了回收站或被彻底删掉、模型被下线，这些产出仍然是用户
		-- 花钱得来的，不能因此从他的作品里消失（任务中心同一条原则）。
		left join project p on p.id = g.project_id
		left join model m on m.id = g.model_id
		left join lateral (
			select o.output_type, o.media_object_id
			from generation_output o
			where o.generation_id = g.id
			order by o.idx
			limit 1
		) fo on true`+where+`
		order by g.id desc
		limit $`+strconv.Itoa(len(args)), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Creation, 0, f.Limit)
	for rows.Next() {
		var c Creation
		if err := rows.Scan(&c.ID, &c.ProjectID, &c.ProjectName, &c.NodeID, &c.ResultType,
			&c.ModelCode, &c.ModelName, &c.OutputCount, &c.CoverType, &c.CoverMedia,
			&c.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// CountCreations 与 ListCreations 同一套条件（不含游标）。
func (r *Repo) CountCreations(ctx context.Context, f CreationFilter) (int, error) {
	where, args := creationWhere(f, nil)
	var n int
	err := r.pool.QueryRow(ctx, `select count(*)::int from generation g`+where, args...).Scan(&n)
	return n, err
}
