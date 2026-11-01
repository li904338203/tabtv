package repo

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/li904338203/tabtv/backend/internal/content/domain"
)

// 作品提交与审核（P13-02；AD-09）。

// Work 是一件作品。
type Work struct {
	ID            int64
	WorkspaceID   int64
	SourceType    string // generation / render
	SourceID      int64
	MediaObjectID int64
	Title         string
	ReviewStatus  string
	IsFeatured    bool
	IsOffline     bool
	SubmittedAt   *time.Time
	ReviewReason  string
	CreatedAt     time.Time
	// AuthorUserID 只在后台查询里填（从来源任务反查），用于发通知。
	AuthorUserID int64
}

// workCols 用于 **select**（带表别名 w）。
const workCols = `w.id, w.workspace_id, w.source_type, w.source_id,
	coalesce(w.media_object_id,0), coalesce(w.title,''), w.review_status,
	w.is_featured, w.is_offline, w.submitted_at, coalesce(w.review_reason,''),
	w.created_at`

// workColsBare 用于 **returning**。
//
// ⚠️ PG 的 `returning` 子句里**不认表别名** —— 写 `returning w.id` 会报
// `missing FROM-clause entry for table "w"`，而那条错误指向别名、
// 看着像 from 子句写错了（实测踩过）。两份清单必须保持同样的列序，
// 因为它们共用同一个 scan。
const workColsBare = `id, workspace_id, source_type, source_id,
	coalesce(media_object_id,0), coalesce(title,''), review_status,
	is_featured, is_offline, submitted_at, coalesce(review_reason,''),
	created_at`

func scanWork(row pgx.Row) (Work, error) {
	var w Work
	err := row.Scan(&w.ID, &w.WorkspaceID, &w.SourceType, &w.SourceID,
		&w.MediaObjectID, &w.Title, &w.ReviewStatus, &w.IsFeatured, &w.IsOffline,
		&w.SubmittedAt, &w.ReviewReason, &w.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Work{}, ErrNotFound
	}
	return w, err
}

// SubmitWork 把一次成功的生成提交成作品。
//
// ⚠️ **幂等靠 `uk_work_source(source_type, source_id)`**：同一次生成重复提交
// 回的是**已有的那一件**而不是报错 —— 用户点两下提交不该看到"失败"，
// 而重复建的话同一张图会在审核队列里出现两次，运营得审两遍。
//
// ⚠️ **一律落 `pending`**：自动通过等于没有审核。"什么算违规"是一条
// 需要人定义的策略（已登记 PARAM-09），在它定稿之前，
// 唯一诚实的默认值是"都要人看一眼"。
func (r *Repo) SubmitWork(ctx context.Context, w Work) (Work, error) {
	row := r.pool.QueryRow(ctx, `
		insert into work (id, workspace_id, source_type, source_id, media_object_id,
			title, review_status, submitted_at)
		values ($1,$2,$3,$4,nullif($5::bigint,0),nullif($6,''),'pending',now())
		on conflict (source_type, source_id) do update
		  -- 重复提交只刷新标题与提交时间，**不重置审核状态** ——
		  -- 否则用户重新提交一次就能把"已驳回"洗回"待审"，绕过审核。
		  set title = coalesce(nullif(excluded.title,''), work.title),
		      submitted_at = now()
		returning `+workColsBare,
		r.nextID(), w.WorkspaceID, w.SourceType, w.SourceID, w.MediaObjectID, w.Title)
	return scanWorkAliased(row)
}

// scanWorkAliased 与 scanWork 相同，只是 returning 里没有表别名。
func scanWorkAliased(row pgx.Row) (Work, error) { return scanWork(row) }

// ListWorksOf 列出某工作区的作品（用户端"我的作品"）。
func (r *Repo) ListWorksOf(ctx context.Context, workspaceID int64, limit int,
	cursorID int64) ([]Work, error) {
	args := []any{workspaceID}
	cond := ""
	if cursorID > 0 {
		args = append(args, cursorID)
		cond = " and w.id < $2"
	}
	args = append(args, limit)
	return r.queryWorks(ctx, `select `+workCols+` from work w
		where w.workspace_id = $1`+cond+`
		order by w.id desc limit $`+itoa(len(args)), args)
}

// AdminWorkFilter 限定后台审核队列。
type AdminWorkFilter struct {
	// Status 为空表示不筛选；运营最常用的是 pending。
	Status   string
	Featured *bool
	Offline  *bool
	Limit    int
	CursorID int64
}

// ListWorksForAdmin 后台审核队列。
//
// ⚠️ **不带 workspace 条件**：运营要看全站的。归属由 admin 的权限位
// （`review:read`）把关，而不是靠工作区上下文 —— 后台本来就没有工作区上下文。
func (r *Repo) ListWorksForAdmin(ctx context.Context, f AdminWorkFilter) ([]Work, error) {
	args := []any{}
	cond := []string{}
	if f.Status != "" {
		args = append(args, f.Status)
		cond = append(cond, "w.review_status = $"+itoa(len(args)))
	}
	if f.Featured != nil {
		args = append(args, *f.Featured)
		cond = append(cond, "w.is_featured = $"+itoa(len(args)))
	}
	if f.Offline != nil {
		args = append(args, *f.Offline)
		cond = append(cond, "w.is_offline = $"+itoa(len(args)))
	}
	if f.CursorID > 0 {
		args = append(args, f.CursorID)
		cond = append(cond, "w.id < $"+itoa(len(args)))
	}
	where := ""
	if len(cond) > 0 {
		where = " where " + strings.Join(cond, " and ")
	}
	args = append(args, f.Limit)
	return r.queryWorks(ctx, `select `+workCols+` from work w`+where+`
		order by w.id desc limit $`+itoa(len(args)), args)
}

func (r *Repo) queryWorks(ctx context.Context, sql string, args []any) ([]Work, error) {
	rows, err := r.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Work
	for rows.Next() {
		w, err := scanWork(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// GetWork 取一件作品，并**反查作者**（用于发通知）。
//
// ⚠️ 作者要从来源任务反查而不是在 work 上再存一列：那一列会与来源那边
// 不一致（转让、代操作），而"这件作品是谁做的"只有来源任务说了算。
// ⚠️ 两段都用 left join：来源任务被清理之后，作品本身仍要能打开 ——
// 内连接会让整行消失，运营会以为这件作品不存在。
func (r *Repo) GetWork(ctx context.Context, id int64) (Work, error) {
	var w Work
	var author *int64
	err := r.pool.QueryRow(ctx, `
		select `+workCols+`,
		       coalesce(g.created_by_user_id, rj.created_by_user_id) as author
		from work w
		left join generation g  on w.source_type = 'generation' and g.id = w.source_id
		left join render_job rj on w.source_type = 'render'     and rj.id = w.source_id
		where w.id = $1`, id).
		Scan(&w.ID, &w.WorkspaceID, &w.SourceType, &w.SourceID, &w.MediaObjectID,
			&w.Title, &w.ReviewStatus, &w.IsFeatured, &w.IsOffline, &w.SubmittedAt,
			&w.ReviewReason, &w.CreatedAt, &author)
	if errors.Is(err, pgx.ErrNoRows) {
		return Work{}, ErrNotFound
	}
	if author != nil {
		w.AuthorUserID = *author
	}
	return w, err
}

// ApplyReview 落地一次审核动作：改 work + 写 content_review，**一个事务**。
//
// ⚠️ 两件事必须同一个事务：只改 work 不写留痕的话，"这件作品为什么被下架"
// 就永远查不到了 —— 而 AD-09 与 P13 的出口条款要的正是"后台可追踪发布原因"。
// 反过来只写留痕不改 work 的话，运营点了没反应而审计里却记着他点过。
//
// ⚠️ 用 **CAS**（带上读到的那一组状态）：两个运营同时审同一件作品时只有一个
// 成功 —— 不带的话后点的那个会把前一个的结论悄悄覆盖掉，
// 而 content_review 里会留下两条互相矛盾的记录。
func (r *Repo) ApplyReview(ctx context.Context, id int64, before, after domain.WorkState,
	action domain.ReviewAction, adminID int64, reason string) (Work, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return Work{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	row := tx.QueryRow(ctx, `
		update work set review_status = $2, is_featured = $3, is_offline = $4,
			reviewed_by_admin_id = $5, review_reason = nullif($6,''), updated_at = now()
		where id = $1
		  and review_status = $7 and is_featured = $8 and is_offline = $9
		returning `+workColsBare,
		id, after.ReviewStatus, after.IsFeatured, after.IsOffline, adminID, reason,
		before.ReviewStatus, before.IsFeatured, before.IsOffline)
	w, err := scanWork(row)
	if err != nil {
		return Work{}, err // 含 ErrNotFound（CAS 落空）
	}
	if _, err := tx.Exec(ctx, `
		insert into content_review (id, work_id, action, operator_admin_id, reason)
		values ($1,$2,$3,$4,nullif($5,''))`,
		r.nextID(), id, action, adminID, reason); err != nil {
		return Work{}, err
	}
	return w, tx.Commit(ctx)
}

// ReviewRecord 是一条审核留痕。
type ReviewRecord struct {
	ID        int64
	Action    string
	AdminID   int64
	Reason    string
	CreatedAt time.Time
}

// ReviewHistory 列出一件作品的全部审核动作。
//
// ⚠️ **正序**（从旧到新）：排查"它为什么现在是这个状态"时，人是顺着时间读的。
// 倒序的话要从下往上看，而中间任何一次"驳回又通过"都容易看反。
func (r *Repo) ReviewHistory(ctx context.Context, workID int64) ([]ReviewRecord, error) {
	rows, err := r.pool.Query(ctx, `
		select id, action, operator_admin_id, coalesce(reason,''), created_at
		from content_review where work_id = $1 order by id`, workID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ReviewRecord
	for rows.Next() {
		var v ReviewRecord
		if err := rows.Scan(&v.ID, &v.Action, &v.AdminID, &v.Reason, &v.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
