package repo

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/li904338203/tabtv/backend/internal/marketing/domain"
)

// 用户端「只返回在投」的查询（MKT-T-04）。
//
// ⚠️ 与管理端 ListOptions 是**两套口径**（那边看「配了什么」，这边看「现在投什么」），
// 故意不共用一个查询加布尔参数：加一个参数的话，总有一天用户端漏传它，
// 停用 / 已结束的选项就出现在模板墙上。
//
// ⚠️ visiblePredicate 是 domain.Visible 的**同义 SQL 写法**（MKD-13，判定只有一处）：
// 列表、按模型筛选项都走它；详情、封面、Resolve 取行之后走 domain.Visible。
// 两边任何一边改了，`TestListOptions_时段现算不跑作业` 会对不上。
// `now` 由应用层传入，**不用 SQL 的 now()**：测试要能注入时间，且与 domain.Visible
// 用同一个时刻判，避免两次取时钟之间刚好跨过结束时间。

// visiblePredicate 返回在投谓词；now 是已登记的参数占位符（如 $3）。
func visiblePredicate(now string) string {
	return "is_enabled and (start_at is null or start_at <= " + now + ") and (end_at is null or end_at > " + now + ")"
}

// VisibleFilter 是用户端列表的筛选。
type VisibleFilter struct {
	Kind     domain.Kind // 必填
	Category string      // 空 = 全部
	Model    string      // 空 = 不筛；否则只要 recommended_models 含它的
	Q        string      // 名称 / 简介模糊匹配
	Now      time.Time
	// 游标：上一页最后一行的 (sort, id)。CursorID=0 表示第一页。
	CursorSort int
	CursorID   int64
	Limit      int
}

// ListVisibleOptions 列出此刻在投的选项，按 (sort, id) 升序、游标分页。多取一行判断是否还有下一页。
//
// ⚠️ 游标分页而不是 offset：运营随时可能启用 / 停用一条，offset 翻页会漏或重（MKT-F-34）。
func (r *Repo) ListVisibleOptions(ctx context.Context, f VisibleFilter) ([]Option, bool, error) {
	var args []any
	add := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}
	cond := []string{"kind = " + add(string(f.Kind)), visiblePredicate(add(f.Now))}
	if f.Category != "" {
		cond = append(cond, "category_code = "+add(f.Category))
	}
	if f.Model != "" {
		cond = append(cond, "recommended_models @> jsonb_build_array("+add(f.Model)+"::text)")
	}
	if q := strings.TrimSpace(f.Q); q != "" {
		p := add("%" + escapeLike(q) + "%")
		cond = append(cond, "(name ilike "+p+" escape '\\' or description ilike "+p+" escape '\\')")
	}
	if f.CursorID > 0 {
		cond = append(cond, "(sort, id) > ("+add(f.CursorSort)+", "+add(f.CursorID)+")")
	}
	limit := f.Limit
	if limit <= 0 {
		limit = 24
	}
	rows, err := r.pool.Query(ctx, `select `+optionCols+` from marketing_option where `+
		strings.Join(cond, " and ")+` order by sort, id limit `+add(limit+1), args...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var out []Option
	for rows.Next() {
		o, err := scanOption(rows)
		if err != nil {
			return nil, false, err
		}
		out = append(out, o)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	more := len(out) > limit
	if more {
		out = out[:limit]
	}
	return out, more, nil
}

// VisibleRecommendedModels 列出此刻在投的某类选项推荐过的模型 code（去重、按 code 排序）。
//
// 「按模型」筛选项（MKT-F-31、MKT-D-24）由它与「当前可用视频模型」取交集派生，**不手工维护**。
func (r *Repo) VisibleRecommendedModels(ctx context.Context, kind domain.Kind, now time.Time) ([]string, error) {
	rows, err := r.pool.Query(ctx, `
		select distinct m.code
		from marketing_option, jsonb_array_elements_text(recommended_models) as m(code)
		where kind = $1 and `+visiblePredicate("$2")+`
		order by m.code`, string(kind), now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
