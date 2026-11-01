package repo

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/li904338203/tabtv/backend/internal/content/domain"
)

// 精选作品与受控公开媒体。
//
// ⚠️ **公开边界由"加精"划定**，而不是"审核通过"：审核只回答"有没有违规"，
// 加精才是运营的一次**显式发布决定**。三个条件缺一不可 ——
// `approved ∧ featured ∧ ¬offline`（见 domain.PubliclyVisible）。
//
// ⚠️ **判定只有一处**：列表与取图用**同一个** `where`。分开写的话会出现
// "列表里没有但直链还能打开" —— 取消加精之后那张图还挂在别人的收藏夹里，
// 而运营以为自己已经撤下来了。

// ShowcaseItem 是公开墙上的一件作品。
//
// ⚠️ **不带 workspaceId / sourceId**：公开列表面向任何人（含未登录访客），
// 带上这些等于告诉外面"这件作品属于哪个工作区、对应哪次生成" ——
// 而那两个 ID 是可以拿去撞别的接口的。
type ShowcaseItem struct {
	ID            int64
	Title         string
	MediaObjectID int64
	// Type 由 `media_object.content_type` 推导（domain.KindOfContentType），
	// 无媒体或认不出的是 other。⚠️ 归类只在仓储里做这一次，前端只认这个字段。
	Type      domain.ShowcaseKind
	CreatedAt time.Time
}

// publicWhere 是公开可见性的**唯一** SQL 表达式。
//
// 与 `domain.PubliclyVisible` 是同一个式子；改一处必须改两处，
// 所以集成测试用同一批数据同时验两边。
const publicWhere = ` w.review_status = 'approved' and w.is_featured and not w.is_offline `

// ListShowcase 公开墙，游标分页；kind 非空时按媒体类型筛（AGT-41）。
//
// ⚠️ `left join` 而不是 `join`：没有媒体的公开作品（hasMedia=false）在「全部」下
// 仍要出现，内连接会让它整行消失。
//
// ⚠️ 类型条件与可见性条件用 `and` 连接，可见性**仍只由 publicWhere 给出** ——
// 筛选只能收窄、不能放宽公开边界。前缀来自白名单（domain.ShowcaseKind），
// 且参数化传入，不拼字面量。
func (r *Repo) ListShowcase(ctx context.Context, limit int, cursorID int64,
	kind domain.ShowcaseKind) ([]ShowcaseItem, error) {
	sql := `select w.id, coalesce(w.title,''), coalesce(w.media_object_id,0),
			coalesce(m.content_type,''), w.created_at
		from work w left join media_object m on m.id = w.media_object_id
		where` + publicWhere
	args := []any{}
	if prefix := kind.ContentTypePrefix(); prefix != "" {
		args = append(args, prefix+"%")
		sql += ` and lower(m.content_type) like $` + itoa(len(args))
	}
	if cursorID > 0 {
		args = append(args, cursorID)
		sql += ` and w.id < $` + itoa(len(args))
	}
	args = append(args, limit)
	sql += ` order by w.id desc limit $` + itoa(len(args))

	rows, err := r.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ShowcaseItem
	for rows.Next() {
		var it ShowcaseItem
		var contentType string
		if err := rows.Scan(&it.ID, &it.Title, &it.MediaObjectID, &contentType,
			&it.CreatedAt); err != nil {
			return nil, err
		}
		it.Type = domain.KindOfContentType(contentType)
		out = append(out, it)
	}
	return out, rows.Err()
}

// PublicMediaOf 取一件**公开**作品的媒体 ID。
//
// ⚠️ 可见性条件写在这条查询里，所以"取消加精 / 下架"之后**立刻**取不到 ——
// 不需要任何缓存失效，也不会出现"列表里没有但直链还能打开"。
//
// ⚠️ 查不到与不可见回**同一个错误**：区分开等于提供一个探测接口
// （"这件作品存在但没加精"本身就是信息）。
func (r *Repo) PublicMediaOf(ctx context.Context, workID int64) (int64, error) {
	var mediaID int64
	err := r.pool.QueryRow(ctx, `
		select coalesce(w.media_object_id, 0) from work w
		where w.id = $1 and`+publicWhere, workID).Scan(&mediaID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, err
	}
	if mediaID == 0 {
		// 作品公开但没有媒体（例如纯文本产出）——不是错误，但也没有图可给。
		return 0, ErrNotFound
	}
	return mediaID, nil
}

// OwnMediaOf 取**自己工作区**那件作品的媒体 ID（不要求公开）。
//
// ⚠️ 这条与 `PublicMediaOf` **必须分开**：作者在"我的作品"里要看到自己
// 刚提交、还没过审的那一件，而公开墙上的人不能。合成一个方法再加个布尔参数
// 的话，总有一天会有人在公开路径上把它传成 true —— 而那一下是把
// 整个审核前的队列开放给了所有人。
//
// ⚠️ 归属条件写在 SQL 里而不是先查再比：先查再比的两步逻辑在重构时
// 很容易被拆开，拆开之后"查到了就给"是个完全正常的写法。
func (r *Repo) OwnMediaOf(ctx context.Context, workID, workspaceID int64) (int64, error) {
	var mediaID int64
	err := r.pool.QueryRow(ctx, `
		select coalesce(media_object_id, 0) from work
		where id = $1 and workspace_id = $2`, workID, workspaceID).Scan(&mediaID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, err
	}
	if mediaID == 0 {
		return 0, ErrNotFound
	}
	return mediaID, nil
}

// RecentProject 是首页"最近项目"的一行。
type RecentProject struct {
	ID           int64
	Name         string
	CoverMediaID int64
	// DisplayCoverMediaID = 显式封面 ?? 最近一张成功产出的图片（0 = 没有）。
	// ⚠️ 与画布域「全部项目」用的是**同一个** SQL 函数 `project_display_cover()`，
	// 不在这里另写一段子查询 —— 两边各写一份，首页和全部项目页的封面早晚对不上。
	DisplayCoverMediaID int64
	// FolderID 所在文件夹（0 = 未分类）。卡片上的「⋯」菜单靠它决定给「移动至文件夹」
	// 还是「移出文件夹」—— 不带的话首页的菜单只能猜，而猜错的那一半是「移不出来」。
	FolderID  int64
	UpdatedAt time.Time
	// Origin 项目来源（MKD-3，空串 = 未记录；`marketing` = 营销会话建的项目）。
	// 首页卡片据此显示「营销」角标（MKT-T-32）—— 不带的话营销项目在首页只显示成普通 Agent 项目，
	// 而在全部项目页显示「营销」，同一个项目两处两种身份。
	Origin string
}

// RecentProjects 取某工作区最近改动的项目。
//
// ⚠️ 直接查 `project`，**不做"产品端口"那套抽象**：本项目只有视频一个产品
// （写作与数字人明确排除，FULL-01）。为不存在的产品建一层接口，
// 换来的是每次读代码都要多跳一次，而抽象掉的那个维度永远只有一个取值。
//
// ⚠️ 带 `status='active'`：回收站里的项目不该出现在"最近"里 ——
// 用户刚删掉它，首页却还推荐他打开。
func (r *Repo) RecentProjects(ctx context.Context, workspaceID int64, limit int) (
	[]RecentProject, error) {
	rows, err := r.pool.Query(ctx, `
		select id, name, coalesce(cover_media_id,0),
		       project_display_cover(id, cover_media_id, inherited_cover_media_id),
		       coalesce(folder_id,0), updated_at, coalesce(origin,'')
		from project
		where workspace_id = $1 and status = 'active'
		order by updated_at desc limit $2`, workspaceID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RecentProject
	for rows.Next() {
		var p RecentProject
		if err := rows.Scan(&p.ID, &p.Name, &p.CoverMediaID, &p.DisplayCoverMediaID,
			&p.FolderID, &p.UpdatedAt, &p.Origin); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	var b [12]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	return string(b[i:])
}
