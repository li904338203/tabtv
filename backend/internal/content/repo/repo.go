// Package repo 是内容域的数据访问层。
package repo

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/li904338203/tabtv/backend/internal/content/domain"
)

var ErrNotFound = errors.New("资源不存在")

type Repo struct {
	pool   *pgxpool.Pool
	nextID func() int64
}

func New(pool *pgxpool.Pool, nextID func() int64) *Repo {
	return &Repo{pool: pool, nextID: nextID}
}

// ---------------------------------------------------------------- Banner

type Banner struct {
	ID            int64
	Position      domain.BannerPosition
	Title         string
	MediaObjectID int64
	CoverMediaID  int64
	LinkURL       string
	Sort          int
	IsEnabled     bool
	StartAt       *time.Time
	EndAt         *time.Time
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

const bannerCols = `id, position, title, coalesce(media_object_id,0),
	coalesce(cover_media_id,0), coalesce(link_url,''), sort, is_enabled,
	start_at, end_at, created_at, updated_at`

func scanBanner(row pgx.Row) (Banner, error) {
	var b Banner
	err := row.Scan(&b.ID, &b.Position, &b.Title, &b.MediaObjectID, &b.CoverMediaID,
		&b.LinkURL, &b.Sort, &b.IsEnabled, &b.StartAt, &b.EndAt, &b.CreatedAt, &b.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Banner{}, ErrNotFound
	}
	return b, err
}

// ActiveBanners 取某个位置**此刻在投**的 Banner。
//
// ⚠️ 排期条件写在 SQL 里而不是取回来再过滤：一个位置历史上可能积累几十条，
// 全取回来只为筛出两条是白跑一趟；更重要的是**"在投"的判定只能有一处** ——
// 这里与 domain.ActiveAt 是同一个式子，改一处必须改两处，
// 所以集成测试用同一批数据同时验两边。
//
// ⚠️ 边界 `[start, end)` 与 domain 一致：`end_at > now` 而不是 `>=`。
func (r *Repo) ActiveBanners(ctx context.Context, pos domain.BannerPosition,
	now time.Time, limit int) ([]Banner, error) {
	rows, err := r.pool.Query(ctx, `
		select `+bannerCols+` from banner
		where position = $1 and is_enabled
		  and (start_at is null or start_at <= $2)
		  and (end_at   is null or end_at   >  $2)
		order by sort, id limit $3`, pos, now, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Banner
	for rows.Next() {
		b, err := scanBanner(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// ListBanners 管理端列表：**不过滤排期**，运营要看得到没生效与已过期的。
//
// ⚠️ 这正是管理端与用户端的分野：用户端看"现在投什么"，管理端看"配了什么"。
// 管理端也按排期过滤的话，运营排好的明天那条会从列表里消失 ——
// 他会以为自己没保存成功，然后再配一条。
func (r *Repo) ListBanners(ctx context.Context, pos string) ([]Banner, error) {
	sql := `select ` + bannerCols + ` from banner`
	args := []any{}
	if pos != "" {
		sql += ` where position = $1`
		args = append(args, pos)
	}
	sql += ` order by position, sort, id`
	rows, err := r.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Banner
	for rows.Next() {
		b, err := scanBanner(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func (r *Repo) InsertBanner(ctx context.Context, b Banner) (Banner, error) {
	b.ID = r.nextID()
	return scanBanner(r.pool.QueryRow(ctx, `
		insert into banner (id, position, title, media_object_id, cover_media_id,
			link_url, sort, is_enabled, start_at, end_at)
		values ($1,$2,$3,nullif($4::bigint,0),nullif($5::bigint,0),
			nullif($6,''),$7,$8,$9,$10)
		returning `+bannerCols,
		b.ID, b.Position, b.Title, b.MediaObjectID, b.CoverMediaID,
		b.LinkURL, b.Sort, b.IsEnabled, b.StartAt, b.EndAt))
}

// UpdateBanner 整条替换（管理端表单一次提交全部字段）。
func (r *Repo) UpdateBanner(ctx context.Context, b Banner) (Banner, error) {
	return scanBanner(r.pool.QueryRow(ctx, `
		update banner set position = $2, title = $3,
			media_object_id = nullif($4::bigint,0), cover_media_id = nullif($5::bigint,0),
			link_url = nullif($6,''), sort = $7, is_enabled = $8,
			start_at = $9, end_at = $10, updated_at = now()
		where id = $1 returning `+bannerCols,
		b.ID, b.Position, b.Title, b.MediaObjectID, b.CoverMediaID,
		b.LinkURL, b.Sort, b.IsEnabled, b.StartAt, b.EndAt))
}

func (r *Repo) DeleteBanner(ctx context.Context, id int64) error {
	tag, err := r.pool.Exec(ctx, `delete from banner where id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (r *Repo) GetBanner(ctx context.Context, id int64) (Banner, error) {
	return scanBanner(r.pool.QueryRow(ctx, `select `+bannerCols+` from banner where id = $1`, id))
}

// ---------------------------------------------------------------- 公告

type Announcement struct {
	ID          int64
	Title       string
	Content     string
	Status      domain.AnnouncementStatus
	LinkURL     string
	PublishedAt *time.Time
	CreatedBy   int64
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

const annCols = `id, title, content, status, coalesce(link_url,''),
	published_at, created_by_admin_id, created_at, updated_at`

func scanAnn(row pgx.Row) (Announcement, error) {
	var a Announcement
	err := row.Scan(&a.ID, &a.Title, &a.Content, &a.Status, &a.LinkURL,
		&a.PublishedAt, &a.CreatedBy, &a.CreatedAt, &a.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Announcement{}, ErrNotFound
	}
	return a, err
}

func (r *Repo) ListAnnouncements(ctx context.Context, status string) ([]Announcement, error) {
	sql := `select ` + annCols + ` from announcement`
	args := []any{}
	if status != "" {
		sql += ` where status = $1`
		args = append(args, status)
	}
	sql += ` order by id desc`
	rows, err := r.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Announcement
	for rows.Next() {
		a, err := scanAnn(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (r *Repo) GetAnnouncement(ctx context.Context, id int64) (Announcement, error) {
	return scanAnn(r.pool.QueryRow(ctx, `select `+annCols+` from announcement where id = $1`, id))
}

// InsertAnnouncement 新建。⚠️ **一律落草稿**：发布是单独一次显式操作。
func (r *Repo) InsertAnnouncement(ctx context.Context, a Announcement) (Announcement, error) {
	a.ID = r.nextID()
	return scanAnn(r.pool.QueryRow(ctx, `
		insert into announcement (id, title, content, status, link_url, created_by_admin_id)
		values ($1,$2,$3,'draft',nullif($4,''),$5)
		returning `+annCols, a.ID, a.Title, a.Content, a.LinkURL, a.CreatedBy))
}

// UpdateAnnouncement 改内容。**不碰 status** —— 发布/下线各有自己的路径。
func (r *Repo) UpdateAnnouncement(ctx context.Context, a Announcement) (Announcement, error) {
	return scanAnn(r.pool.QueryRow(ctx, `
		update announcement set title = $2, content = $3,
			link_url = nullif($4,''), updated_at = now()
		where id = $1 returning `+annCols, a.ID, a.Title, a.Content, a.LinkURL))
}

// SetAnnouncementStatus 用 CAS 推进状态。
//
// ⚠️ 带 `status = $3`（期望的当前状态）：两个运营同时点发布时只有一个成功，
// 另一个收到"当前状态不允许"。不带的话第二次点会把 `published_at` 刷新一遍 ——
// 公告在消息中心里按发布时间排序，于是它会莫名其妙地跳到最前面。
func (r *Repo) SetAnnouncementStatus(ctx context.Context, id int64,
	from, to domain.AnnouncementStatus, publish bool) (Announcement, error) {
	sql := `update announcement set status = $2, updated_at = now()`
	if publish {
		sql += `, published_at = now()`
	}
	sql += ` where id = $1 and status = $3 returning ` + annCols
	return scanAnn(r.pool.QueryRow(ctx, sql, id, to, from))
}

func (r *Repo) DeleteAnnouncement(ctx context.Context, id int64) error {
	tag, err := r.pool.Exec(ctx, `delete from announcement where id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
