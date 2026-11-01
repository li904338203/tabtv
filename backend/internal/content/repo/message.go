package repo

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/li904338203/tabtv/backend/internal/content/domain"
)

// 消息中心：个人通知 + 运营公告的合并读取。
//
// ⚠️ **两类消息两张表，故意不合并**：
//   - `notification` 个人通知，一人一行，有明确收件人与业务引用；
//   - `announcement` 运营广播，**全站一行**，已读状态另记在 `announcement_read`。
//
// 公告**不按人扇出**：一条公告若也写成"用户数那么多行"，十万用户就是十万行，
// 而绝大多数人从不点开。代价是消息中心列表要 union 两张表再排序分页 ——
// 这个复杂度换的是一个**不可逆**的容量问题。
//
// ⚠️ **合并必须发生在 SQL 里**。两边各取一页再在内存里拼，**第二页会漏消息**：
// 各取 20 条合出来的 20 条，与真正按时间序的第 1~20 条**不是同一批** ——
// 被挤掉的那些再也不会出现在任何一页上，而用户看到的列表"看起来很正常"。

// Message 是合并之后的一条消息。
type Message struct {
	ID     int64
	Source domain.MessageSource
	Type   string
	Title  string
	// Content 公告是正文，通知是摘要；两者都可能为空。
	Content   string
	LinkURL   string
	RefType   string
	RefID     string
	IsRead    bool
	CreatedAt time.Time
}

// ListMessages 按时间倒序合并两类消息，游标分页。
//
// ⚠️ 游标用 **(createdAt, id)** 而不是单纯的 id：两张表的 id 来自同一个雪花
// 发号器，**但顺序与时间不严格一致**（不同实例的时钟、同毫秒内的序列）。
// 只按 id 翻页的话，跨表的那一处会漏或重。
//
// ⚠️ 公告只取 **published** 的：草稿与已下线的都不该出现在用户面前。
// 下线一条公告之后它必须**立刻**从所有人的列表里消失 —— 所以状态写在
// 查询条件里，而不是靠发布时给每个人塞一行然后再去删。
func (r *Repo) ListMessages(ctx context.Context, userID int64, limit int,
	cursorAt *time.Time, cursorID int64) ([]Message, error) {
	// 两个分支各自先按游标裁剪，再 union、再排序 —— 这样 PG 能各用各的索引。
	rows, err := r.pool.Query(ctx, `
		with merged as (
			select n.id, 'notification' as source, n.type, n.title,
			       coalesce(n.content,'') as content, coalesce(n.link_url,'') as link_url,
			       coalesce(n.ref_type,'') as ref_type, coalesce(n.ref_id,'') as ref_id,
			       (n.read_at is not null) as is_read, n.created_at
			from notification n
			where n.user_id = $1
			union all
			select a.id, 'announcement', 'announcement', a.title,
			       a.content, coalesce(a.link_url,''), '', '',
			       -- 已读状态另记一张表；没有那一行就是未读。
			       (ar.id is not null), coalesce(a.published_at, a.created_at)
			from announcement a
			left join announcement_read ar
			       on ar.announcement_id = a.id and ar.user_id = $1
			where a.status = 'published'
		)
		select id, source, type, title, content, link_url, ref_type, ref_id,
		       is_read, created_at
		from merged
		where ($2::timestamptz is null)
		   or (created_at < $2) or (created_at = $2 and id < $3)
		order by created_at desc, id desc
		limit $4`, userID, cursorAt, cursorID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Message
	for rows.Next() {
		var m Message
		if err := rows.Scan(&m.ID, &m.Source, &m.Type, &m.Title, &m.Content,
			&m.LinkURL, &m.RefType, &m.RefID, &m.IsRead, &m.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// UnreadCount 未读数，供顶栏红点。
//
// ⚠️ 与列表用**同一套条件**（公告只数 published 的）。分开写的话，
// 红点显示有未读而点进去什么都没有 —— 而这种不一致没有任何报错。
func (r *Repo) UnreadCount(ctx context.Context, userID int64) (int, error) {
	var n int
	err := r.pool.QueryRow(ctx, `
		select (
			select count(*) from notification
			where user_id = $1 and read_at is null
		) + (
			select count(*) from announcement a
			where a.status = 'published'
			  and not exists (
			    select 1 from announcement_read ar
			    where ar.announcement_id = a.id and ar.user_id = $1)
		)`, userID).Scan(&n)
	return n, err
}

// MarkRead 标记一条消息已读。
//
// ⚠️ **必须带 source**：两张表的 id 在同一个空间里无从区分。
// 只传 id 的话服务端只能两张表都试一遍 —— 而"试到哪个算哪个"这种行为
// 一旦出错（两表 id 恰好相同）会把别人的公告标成已读，且完全查不出来。
//
// ⚠️ 通知的更新条件带 `user_id`：不带的话任何人都能把别人的通知标成已读。
// 公告的已读是**插入**（`on conflict do nothing`）而不是更新 ——
// 全站一行，不可能去改那一行。
func (r *Repo) MarkRead(ctx context.Context, userID, id int64,
	source domain.MessageSource) error {
	switch source {
	case domain.SourceNotification:
		tag, err := r.pool.Exec(ctx, `
			update notification set read_at = now()
			where id = $1 and user_id = $2 and read_at is null`, id, userID)
		if err != nil {
			return err
		}
		// 0 行有两种可能：不是他的，或者已经读过了。
		// ⚠️ 后者**不是错误** —— 重复标已读是正常的（多个标签页、重试），
		// 报错的话前端得为一个无害的重复操作弹提示。所以只在
		// "这条通知根本不属于他"时才报 404。
		if tag.RowsAffected() == 0 {
			var exists bool
			if err := r.pool.QueryRow(ctx,
				`select exists(select 1 from notification where id = $1 and user_id = $2)`,
				id, userID).Scan(&exists); err != nil {
				return err
			}
			if !exists {
				return ErrNotFound
			}
		}
		return nil

	case domain.SourceAnnouncement:
		var published bool
		err := r.pool.QueryRow(ctx,
			`select status = 'published' from announcement where id = $1`, id).Scan(&published)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if !published {
			// 草稿/已下线的公告用户根本看不到，标它已读只能是构造出来的请求。
			return ErrNotFound
		}
		_, err = r.pool.Exec(ctx, `
			insert into announcement_read (id, announcement_id, user_id)
			values ($1,$2,$3) on conflict (announcement_id, user_id) do nothing`,
			r.nextID(), id, userID)
		return err
	}
	return ErrNotFound
}

// MarkAllRead 全部已读。
//
// ⚠️ 公告那半边是"给所有**当前可见**的公告补一行已读"，
// 而不是"记一个时间戳"。用时间戳的话，之后发布的公告会因为
// "发布时间早于我的已读时间"（运营可以改 published_at）而被误判成已读。
func (r *Repo) MarkAllRead(ctx context.Context, userID int64) (int, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx, `
		update notification set read_at = now()
		where user_id = $1 and read_at is null`, userID)
	if err != nil {
		return 0, err
	}
	n := int(tag.RowsAffected())

	rows, err := tx.Query(ctx, `
		select a.id from announcement a
		where a.status = 'published'
		  and not exists (
		    select 1 from announcement_read ar
		    where ar.announcement_id = a.id and ar.user_id = $1)`, userID)
	if err != nil {
		return 0, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, id := range ids {
		if _, err := tx.Exec(ctx, `
			insert into announcement_read (id, announcement_id, user_id)
			values ($1,$2,$3) on conflict (announcement_id, user_id) do nothing`,
			r.nextID(), id, userID); err != nil {
			return 0, err
		}
	}
	return n + len(ids), tx.Commit(ctx)
}

// InsertNotification 发一条个人通知。
//
// ⚠️ 幂等靠 `uk_notification_ref(user_id, type, ref_type, ref_id)` 的部分唯一索引：
// Worker 重试同一个任务不会发第二条（FLOW-10）。重复时**不报错**，
// 返回 false 表示"这条已经发过了" —— 通知绝不能牵连主流程。
func (r *Repo) InsertNotification(ctx context.Context, n Message, userID int64) (bool, error) {
	tag, err := r.pool.Exec(ctx, `
		insert into notification (id, user_id, type, title, content, ref_type, ref_id, link_url)
		values ($1,$2,$3,$4,nullif($5,''),nullif($6,''),nullif($7,''),nullif($8,''))
		on conflict (user_id, type, ref_type, ref_id) where ref_id is not null
		do nothing`,
		r.nextID(), userID, n.Type, n.Title, n.Content, n.RefType, n.RefID, n.LinkURL)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}
