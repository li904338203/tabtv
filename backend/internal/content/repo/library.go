package repo

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ---------------------------------------------------------------- 我的资产（迁移 00074）

// ErrConflict 是唯一约束冲突（文件夹重名等）。
var ErrConflict = errors.New("名称已存在")

// ErrNotSavable 是这项产物不能存进资产（不是本工作区的、还没成功、没有这一项）。
var ErrNotSavable = errors.New("这项生成结果不能保存")

// LibraryItem 是「我的资产」里的一项：一次生成的某一项产物。
type LibraryItem struct {
	ID           int64
	FolderID     int64 // 0 = 顶层
	GenerationID int64
	OutputIdx    int
	Kind         string
	Title        string
	MediaID      int64
	// PosterMediaID 视频的封面帧（Worker 截的，CD-83），还没截出来时为 0。
	PosterMediaID int64
	Text          string
	ProjectID     int64
	ProjectName   string
	NodeID        string
	Tags          []string
	Rating        int
	CreatorName   string
	ModelCode     string
	ModelName     string
	Prompt        string
	GeneratedAt   time.Time
	SavedAt       time.Time
}

// LibraryFilter 限定一次资产查询。列表与计数共用同一套条件（libraryWhere）。
type LibraryFilter struct {
	WorkspaceID int64
	// FolderID：nil = 不限文件夹；指向 0 = 只要顶层；其余 = 只要这个文件夹里的。
	FolderID     *int64
	Tags         []string
	Query        string
	Kind         string
	MinRating    int
	GenerationID int64
	Limit        int
	CursorID     int64
}

func libraryWhere(f LibraryFilter, args []any) (string, []any) {
	add := func(v any) string {
		args = append(args, v)
		return "$" + strconv.Itoa(len(args))
	}
	sql := ` where i.workspace_id = ` + add(f.WorkspaceID) + ` and i.deleted_at is null`
	if f.FolderID != nil {
		if *f.FolderID == 0 {
			sql += ` and i.folder_id is null`
		} else {
			sql += ` and i.folder_id = ` + add(*f.FolderID)
		}
	}
	if len(f.Tags) > 0 {
		// 选了几个标签就要同时带着这几个（交集）：筛选是「缩小范围」，选得越多越少
		sql += ` and i.tags @> ` + add(f.Tags) + `::text[]`
	}
	if q := strings.TrimSpace(f.Query); q != "" {
		p := add("%" + escapeLike(q) + "%")
		sql += ` and (i.title ilike ` + p + ` or coalesce(i.text_content, '') ilike ` + p +
			` or coalesce(g.params_snapshot->>'prompt', '') ilike ` + p + `)`
	}
	if f.Kind != "" {
		sql += ` and i.kind = ` + add(f.Kind)
	}
	if f.MinRating > 0 {
		sql += ` and coalesce(gr.rating, 0) >= ` + add(f.MinRating)
	}
	if f.GenerationID > 0 {
		sql += ` and i.generation_id = ` + add(f.GenerationID)
	}
	return sql, args
}

func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// libraryFrom 资产 + 来源生成（left join：项目进回收站、模型下线、生成被清理，保存过的资产照样在）。
const libraryFrom = `
	from library_item i
	left join generation g on g.id = i.generation_id
	left join generation_rating gr on gr.generation_id = i.generation_id
	left join project p on p.id = i.project_id
	left join model m on m.id = g.model_id
	left join app_user u on u.id = g.created_by_user_id`

const libraryCols = `i.id, coalesce(i.folder_id, 0), i.generation_id, i.output_idx, i.kind, i.title,
	coalesce(i.media_object_id, 0),
	coalesce((select pm.id from media_object pm where pm.source_media_id = i.media_object_id
	          and pm.derive_kind = 'video_poster' and pm.status = 'active' order by pm.id desc limit 1), 0),
	coalesce(i.text_content, ''), coalesce(i.project_id, 0), coalesce(p.name, ''), coalesce(i.canvas_node_id, ''),
	i.tags, coalesce(gr.rating, 0), coalesce(u.nickname, ''), coalesce(m.code, ''), coalesce(m.name, ''),
	coalesce(g.params_snapshot->>'prompt', ''), coalesce(g.created_at, i.created_at), i.created_at`

func scanLibraryItem(row pgx.Row) (LibraryItem, error) {
	var it LibraryItem
	err := row.Scan(&it.ID, &it.FolderID, &it.GenerationID, &it.OutputIdx, &it.Kind, &it.Title,
		&it.MediaID, &it.PosterMediaID, &it.Text, &it.ProjectID, &it.ProjectName, &it.NodeID,
		&it.Tags, &it.Rating, &it.CreatorName, &it.ModelCode, &it.ModelName, &it.Prompt,
		&it.GeneratedAt, &it.SavedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return LibraryItem{}, ErrNotFound
	}
	if it.Tags == nil {
		it.Tags = []string{}
	}
	return it, err
}

// ListLibraryItems 按保存先后倒序（id desc）列出资产。
func (r *Repo) ListLibraryItems(ctx context.Context, f LibraryFilter) ([]LibraryItem, error) {
	where, args := libraryWhere(f, nil)
	if f.CursorID > 0 {
		args = append(args, f.CursorID)
		where += ` and i.id < $` + strconv.Itoa(len(args))
	}
	args = append(args, f.Limit)
	rows, err := r.pool.Query(ctx, `select `+libraryCols+libraryFrom+where+`
		order by i.id desc limit $`+strconv.Itoa(len(args)), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]LibraryItem, 0, f.Limit)
	for rows.Next() {
		it, err := scanLibraryItem(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// CountLibraryItems 与 ListLibraryItems 同一套条件（不含游标）。
func (r *Repo) CountLibraryItems(ctx context.Context, f LibraryFilter) (int, error) {
	where, args := libraryWhere(f, nil)
	var n int
	err := r.pool.QueryRow(ctx, `select count(*)`+libraryFrom+where, args...).Scan(&n)
	return n, err
}

// GetLibraryItem 取一项（本工作区、未移出）。
func (r *Repo) GetLibraryItem(ctx context.Context, workspaceID, id int64) (LibraryItem, error) {
	return scanLibraryItem(r.pool.QueryRow(ctx, `select `+libraryCols+libraryFrom+`
		where i.workspace_id = $1 and i.id = $2 and i.deleted_at is null`, workspaceID, id))
}

// SaveLibraryItemInput 保存一项产物。产物本身（类型、媒体、项目、节点）由服务端按生成取，不信客户端。
type SaveLibraryItemInput struct {
	WorkspaceID  int64
	UserID       int64
	GenerationID int64
	OutputIdx    int
	Title        string
	// Text 文本产物保存那一刻卡片上显示的内容（可能是用户改过的）；为空时取那次生成的原文。
	Text     string
	FolderID int64
	Tags     []string
}

// SaveLibraryItem 保存（幂等）：同一项产物在工作区里已经存着就原样返回那一项，不报错也不改它。
// 返回 (资产 ID, 是否新建)。
func (r *Repo) SaveLibraryItem(ctx context.Context, in SaveLibraryItemInput) (int64, bool, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return 0, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var kind, outText string
	var mediaID, projectID int64
	var nodeID string
	err = tx.QueryRow(ctx, `
		select coalesce(o.output_type, g.result_type), coalesce(o.media_object_id, 0), coalesce(o.text_content, ''),
		       g.project_id, coalesce(g.node_id, '')
		  from generation g
		  join generation_output o on o.generation_id = g.id and o.idx = $3
		 where g.id = $2 and g.workspace_id = $1 and g.status = 'success'`,
		in.WorkspaceID, in.GenerationID, in.OutputIdx).Scan(&kind, &mediaID, &outText, &projectID, &nodeID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, ErrNotSavable
	}
	if err != nil {
		return 0, false, err
	}
	switch kind {
	case "text":
		mediaID = 0
		if strings.TrimSpace(in.Text) != "" {
			outText = in.Text
		}
	case "image", "video", "audio":
		if mediaID == 0 {
			return 0, false, ErrNotSavable
		}
		outText = ""
	default:
		return 0, false, ErrNotSavable
	}
	var folder any
	if in.FolderID > 0 {
		var ok bool
		if err := tx.QueryRow(ctx, `select exists(select 1 from library_folder where id = $1 and workspace_id = $2)`,
			in.FolderID, in.WorkspaceID).Scan(&ok); err != nil {
			return 0, false, err
		}
		if !ok {
			return 0, false, ErrNotFound
		}
		folder = in.FolderID
	}
	tags := in.Tags
	if tags == nil {
		tags = []string{}
	}
	var id int64
	err = tx.QueryRow(ctx, `
		insert into library_item (id, workspace_id, folder_id, generation_id, output_idx, kind, title,
		                          media_object_id, text_content, project_id, canvas_node_id, tags, created_by_user_id)
		values ($1, $2, $3, $4, $5, $6, $7, nullif($8::bigint, 0), nullif($9, ''), nullif($10::bigint, 0), nullif($11, ''), $12, nullif($13::bigint, 0))
		on conflict (workspace_id, generation_id, output_idx) where deleted_at is null do nothing
		returning id`,
		r.nextID(), in.WorkspaceID, folder, in.GenerationID, in.OutputIdx, kind, in.Title,
		mediaID, outText, projectID, nodeID, tags, in.UserID).Scan(&id)
	created := true
	if errors.Is(err, pgx.ErrNoRows) {
		created = false
		err = tx.QueryRow(ctx, `select id from library_item
			where workspace_id = $1 and generation_id = $2 and output_idx = $3 and deleted_at is null`,
			in.WorkspaceID, in.GenerationID, in.OutputIdx).Scan(&id)
	}
	if err != nil {
		return 0, false, err
	}
	return id, created, tx.Commit(ctx)
}

// UpdateLibraryItemInput 改一项：各字段为 nil 表示不改；FolderID 指向 0 表示挪到顶层。
type UpdateLibraryItemInput struct {
	Title    *string
	FolderID *int64
	Tags     []string
	SetTags  bool
}

func (r *Repo) UpdateLibraryItem(ctx context.Context, workspaceID, id int64, in UpdateLibraryItemInput) error {
	if in.FolderID != nil && *in.FolderID > 0 {
		if err := r.folderExists(ctx, workspaceID, *in.FolderID); err != nil {
			return err
		}
	}
	tags := in.Tags
	if tags == nil {
		tags = []string{}
	}
	var folder any
	if in.FolderID != nil && *in.FolderID > 0 {
		folder = *in.FolderID
	}
	tag, err := r.pool.Exec(ctx, `
		update library_item set
		  title = coalesce($3, title),
		  folder_id = case when $4 then $5::bigint else folder_id end,
		  tags = case when $6 then $7::text[] else tags end,
		  updated_at = now()
		where workspace_id = $1 and id = $2 and deleted_at is null`,
		workspaceID, id, in.Title, in.FolderID != nil, folder, in.SetTags, tags)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// MoveLibraryItems 批量挪到文件夹（0 = 顶层）。返回实际挪动的条数。
func (r *Repo) MoveLibraryItems(ctx context.Context, workspaceID int64, ids []int64, folderID int64) (int, error) {
	if folderID > 0 {
		if err := r.folderExists(ctx, workspaceID, folderID); err != nil {
			return 0, err
		}
	}
	var folder any
	if folderID > 0 {
		folder = folderID
	}
	tag, err := r.pool.Exec(ctx, `update library_item set folder_id = $3, updated_at = now()
		where workspace_id = $1 and id = any($2) and deleted_at is null`, workspaceID, ids, folder)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

// RemoveLibraryItems 批量移出资产（软删：生成结果本身不动，节点上的产出照常显示）。
func (r *Repo) RemoveLibraryItems(ctx context.Context, workspaceID int64, ids []int64) (int, error) {
	tag, err := r.pool.Exec(ctx, `update library_item set deleted_at = now(), updated_at = now()
		where workspace_id = $1 and id = any($2) and deleted_at is null`, workspaceID, ids)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

// ---------------------------------------------------------------- 文件夹

type LibraryFolder struct {
	ID        int64
	Name      string
	ItemCount int
	CreatedAt time.Time
}

func (r *Repo) folderExists(ctx context.Context, workspaceID, id int64) error {
	var ok bool
	if err := r.pool.QueryRow(ctx, `select exists(select 1 from library_folder where id = $1 and workspace_id = $2)`,
		id, workspaceID).Scan(&ok); err != nil {
		return err
	}
	if !ok {
		return ErrNotFound
	}
	return nil
}

// ListLibraryFolders 按名称排序（文件夹靠名字辨认），带未移出的资产数。
func (r *Repo) ListLibraryFolders(ctx context.Context, workspaceID int64) ([]LibraryFolder, error) {
	rows, err := r.pool.Query(ctx, `
		select f.id, f.name,
		       (select count(*)::int from library_item i where i.folder_id = f.id and i.deleted_at is null),
		       f.created_at
		  from library_folder f where f.workspace_id = $1 order by lower(f.name), f.id`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []LibraryFolder{}
	for rows.Next() {
		var f LibraryFolder
		if err := rows.Scan(&f.ID, &f.Name, &f.ItemCount, &f.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

func (r *Repo) CreateLibraryFolder(ctx context.Context, workspaceID, userID int64, name string) (LibraryFolder, error) {
	f := LibraryFolder{ID: r.nextID(), Name: name}
	err := r.pool.QueryRow(ctx, `insert into library_folder (id, workspace_id, name, created_by_user_id)
		values ($1, $2, $3, nullif($4::bigint, 0)) returning created_at`, f.ID, workspaceID, name, userID).Scan(&f.CreatedAt)
	if isUniqueViolation(err) {
		return LibraryFolder{}, ErrConflict
	}
	return f, err
}

func (r *Repo) RenameLibraryFolder(ctx context.Context, workspaceID, id int64, name string) error {
	tag, err := r.pool.Exec(ctx, `update library_folder set name = $3, updated_at = now() where workspace_id = $1 and id = $2`,
		workspaceID, id, name)
	if isUniqueViolation(err) {
		return ErrConflict
	}
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteLibraryFolder 删文件夹：里面的资产先退回顶层（一个事务），资产不跟着删。
func (r *Repo) DeleteLibraryFolder(ctx context.Context, workspaceID, id int64) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `update library_item set folder_id = null, updated_at = now()
		where workspace_id = $1 and folder_id = $2`, workspaceID, id); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `delete from library_folder where workspace_id = $1 and id = $2`, workspaceID, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return tx.Commit(ctx)
}

// ---------------------------------------------------------------- 标签

// ListLibraryTags 工作区里「新建标签」建出来的（预置的在 app 层）。
func (r *Repo) ListLibraryTags(ctx context.Context, workspaceID int64) ([]string, error) {
	rows, err := r.pool.Query(ctx, `select name from library_tag where workspace_id = $1 order by created_at, id`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// CreateLibraryTag 幂等：已有同名的不报错。
func (r *Repo) CreateLibraryTag(ctx context.Context, workspaceID int64, name string) error {
	_, err := r.pool.Exec(ctx, `insert into library_tag (id, workspace_id, name) values ($1, $2, $3)
		on conflict (workspace_id, name) do nothing`, r.nextID(), workspaceID, name)
	return err
}

// DeleteLibraryTag 删自定义标签，并从资产上摘掉它（一个事务）。
func (r *Repo) DeleteLibraryTag(ctx context.Context, workspaceID int64, name string) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, err := tx.Exec(ctx, `delete from library_tag where workspace_id = $1 and name = $2`, workspaceID, name)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	if _, err := tx.Exec(ctx, `update library_item set tags = array_remove(tags, $2), updated_at = now()
		where workspace_id = $1 and $2 = any(tags)`, workspaceID, name); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ---------------------------------------------------------------- 评级

// SetGenerationRating 给一次生成打分（1~5，0 = 清除）。生成必须属于本工作区且已成功。
func (r *Repo) SetGenerationRating(ctx context.Context, workspaceID, generationID, userID int64, rating int) error {
	if rating == 0 {
		_, err := r.pool.Exec(ctx, `delete from generation_rating where generation_id = $1 and workspace_id = $2`,
			generationID, workspaceID)
		return err
	}
	tag, err := r.pool.Exec(ctx, `
		insert into generation_rating (generation_id, workspace_id, rating, rated_by_user_id)
		select g.id, g.workspace_id, $3, nullif($4::bigint, 0) from generation g
		 where g.id = $1 and g.workspace_id = $2 and g.status = 'success'
		on conflict (generation_id) do update set rating = excluded.rating,
		  rated_by_user_id = excluded.rated_by_user_id, updated_at = now()`,
		generationID, workspaceID, rating, userID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotSavable
	}
	return nil
}

// GenerationMarks 一批生成的评分与「保存过的产物序号」（右键菜单显示用）。
type GenerationMarks struct {
	Rating     int
	SavedItems map[int]int64 // 产物序号 → 资产 ID
}

func (r *Repo) GenerationMarks(ctx context.Context, workspaceID int64, ids []int64) (map[int64]GenerationMarks, error) {
	out := map[int64]GenerationMarks{}
	get := func(id int64) GenerationMarks {
		m, ok := out[id]
		if !ok {
			m = GenerationMarks{SavedItems: map[int]int64{}}
		}
		return m
	}
	rows, err := r.pool.Query(ctx, `select generation_id, rating from generation_rating
		where workspace_id = $1 and generation_id = any($2)`, workspaceID, ids)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id int64
		var rating int
		if err := rows.Scan(&id, &rating); err != nil {
			rows.Close()
			return nil, err
		}
		m := get(id)
		m.Rating = rating
		out[id] = m
	}
	rows.Close()
	rows, err = r.pool.Query(ctx, `select generation_id, output_idx, id from library_item
		where workspace_id = $1 and generation_id = any($2) and deleted_at is null`, workspaceID, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var genID, itemID int64
		var idx int
		if err := rows.Scan(&genID, &idx, &itemID); err != nil {
			return nil, err
		}
		m := get(genID)
		m.SavedItems[idx] = itemID
		out[genID] = m
	}
	return out, rows.Err()
}
