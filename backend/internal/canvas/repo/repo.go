// Package repo 是项目/文件夹/模板的数据访问层。
//
// ⚠️ 每条查询都带 `workspace_id` 条件，**不是**只靠中间件校验过成员资格就直接按 id 查：
// 请求头里的工作区 ID 是环境权限，用户可以带着自己工作区的 ID 去访问别人的资源 ID
// （CD-04 的第 2 条）。这一层是那条规则的最后一道闸。
package repo

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrNotFound  = errors.New("资源不存在")
	ErrDuplicate = errors.New("名称重复")
)

type Repo struct{ pool *pgxpool.Pool }

func New(pool *pgxpool.Pool) *Repo { return &Repo{pool: pool} }

type Project struct {
	ID          int64
	WorkspaceID int64
	CreatedBy   int64
	Name        string
	// CoverMedia 是**用户显式设置**的封面（可能为 0）。
	// ⚠️ 它会被读回去写（改名时登记媒体引用、复制项目时带给副本），
	// 所以**不能**换成展示封面 —— 那样改一次名就给一张用户没选过的图登记引用，
	// 复制出来的副本会拿源项目的产出图当显式封面。展示用的是 DisplayCover。
	CoverMedia int64
	// DisplayCover 是**展示用**的封面：显式封面 ?? 自己最近一张产出图 ?? 继承封面（0 = 没有）。
	// 只读、由 `project_display_cover()` 算出（迁移 00030/00031），**绝不写回**任何列。
	DisplayCover int64
	// InheritedCover 是**复制时**从源项目继承来的封面，只由 Copy 写一次（迁移 00031）。
	// ⚠️ 副本按设计不拥有源项目的生成记录（CV-03 / GAP-16），没有这一列的话，
	//    副本的展示封面永远算不出来 —— 画布里明明有图，卡片上却是占位。
	// ⚠️ 它**不是**显式封面：不钉死（副本自己出了图就换）、不算引用（不挡删图）。
	InheritedCover int64
	FolderID       int64
	CanvasData     json.RawMessage
	Version        int
	Status         string
	DeletedAt      *time.Time
	CreatedAt      time.Time
	UpdatedAt      time.Time
	// Origin 是项目从哪个入口建的（MKD-3，迁移 00058）：空 = 未记录（历史项目与非营销入口），
	// 目前只有 OriginMarketing 一个取值。⚠️ 只在建项目时写一次，任何更新路径都不写它。
	Origin string
	// Agent 是项目最近活跃的那个 Agent 会话（AGT-45），nil = 从没开过会话（普通画布项目）。
	// ⚠️ 只有**列表**（Page / AgentSummaries）会填它；Get / Update 等单项查询不填，
	// 不能拿它为 nil 推断「这不是 Agent 项目」。
	Agent *AgentBrief
}

// AgentBrief 是项目卡片上要的那一点会话信息：点卡片进哪个会话、副标题写什么。
type AgentBrief struct {
	SessionID    int64
	Title        string
	State        string
	LastActiveAt time.Time
}

// ProjectKind 是列表的类型筛选（AGT-45）。判据是「有没有任何 agent_session」——
// 服务端查询时算，**不落列、不打标记**：打标记的话建会话那条路径漏写一次，
// 这个项目就永远显示成画布项目，而没有任何报错。
type ProjectKind string

const (
	KindAll    ProjectKind = ""
	KindCanvas ProjectKind = "canvas"
	KindAgent  ProjectKind = "agent"
	// KindMarketing 按 project.origin 落列判定（MKD-3）—— 与 KindAgent 的「按会话现算」**并存**：
	// 营销项目建时必带会话，所以它同时出现在 agent 下、天然不出现在 canvas 下。
	// ⚠️ 不要把 Agent 的判定也改成落列（AGT-45 的理由仍然成立）。
	KindMarketing ProjectKind = "marketing"
)

// OriginMarketing 是 project.origin 唯一允许的非空值（ck_project_origin）。
const OriginMarketing = "marketing"

// ⚠️ 跨域只读：canvas 域在这里**直接读 agent 域的 agent_session 表**。
// 这是有意的取舍（与内容域任务中心四张表 union、首页最近项目直接查 project 同理）：
// 类型筛选必须和游标分页**在同一条 SQL 里**完成 —— 先分页再按会话过滤会让一页
// 不满（甚至空页而 hasMore=true），分开查再在内存里拼则会在翻页时漏项。
// 只读、只取四列、只按 project_id 走 idx_agent_session_project，不写、不依赖 agent 域代码。
// 「最近活跃的会话」的规则**只有这一处**：列表（Page）与首页（AgentSummaries）共用。
const agentLateral = `left join lateral (
		select s.id as agent_session_id, s.title as agent_title,
		       s.state as agent_state, s.last_active_at as agent_last_active_at
		from agent_session s
		where s.project_id = p.id
		order by s.last_active_at desc, s.id desc
		limit 1) a on true`

// ⚠️ 子查询的列都带 agent_ 前缀：projectCols 是不带表前缀的列名，lateral 里再出一个 id
// 的话 `id` 就有歧义，整条列表查询报错。
const agentCols = `coalesce(a.agent_session_id, 0), coalesce(a.agent_title, ''),
	coalesce(a.agent_state, ''), a.agent_last_active_at`

// agentDest 返回扫描目标与一个收尾函数（把扫描结果装回 Project.Agent）。
func agentDest() ([]any, func() *AgentBrief) {
	var (
		id    int64
		title string
		state string
		at    *time.Time
	)
	return []any{&id, &title, &state, &at}, func() *AgentBrief {
		if id == 0 {
			return nil
		}
		b := &AgentBrief{SessionID: id, Title: title, State: state}
		if at != nil {
			b.LastActiveAt = *at
		}
		return b
	}
}

type Folder struct {
	ID           int64
	Name         string
	Sort         int
	ProjectCount int
	CreatedAt    time.Time
}

type Template struct {
	Code        string
	Name        string
	Description string
	CanvasData  json.RawMessage
	Sort        int
}

// 列表查询**不取 canvas_data**：一屏 20 个项目，每个画布 JSON 几百 KB 到几 MB，
// 全取回来只为显示名字和封面，是把内存和带宽白白烧掉。
//
// ⚠️ 展示封面走 `project_display_cover()`（迁移 00030），**不在这里另写一段子查询**：
// 首页「最近项目」（content 域）读的是同一条规则，两边各写一段的话早晚会不一致，
// 而表现只是「首页和全部项目页的封面对不上」。
const projectCols = `id, workspace_id, created_by_user_id, name,
	coalesce(cover_media_id,0), coalesce(folder_id,0), version, status,
	deleted_at, created_at, updated_at,
	project_display_cover(id, cover_media_id, inherited_cover_media_id),
	coalesce(origin, '')`

// extra 是 projectCols 之后追加的列（如 canvas_data）的扫描目标。
func scanProject(row pgx.Row, extra ...any) (Project, error) {
	var p Project
	dest := []any{&p.ID, &p.WorkspaceID, &p.CreatedBy, &p.Name, &p.CoverMedia,
		&p.FolderID, &p.Version, &p.Status, &p.DeletedAt, &p.CreatedAt, &p.UpdatedAt,
		&p.DisplayCover, &p.Origin}
	err := row.Scan(append(dest, extra...)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return Project{}, ErrNotFound
	}
	return p, err
}

func (r *Repo) Insert(ctx context.Context, p Project) error {
	return insertProject(ctx, r.pool, p)
}

// execer 是 pool 与 tx 的公共子集：建项目的那条 SQL 只写一份，
// 复制项目（要和复制文档同一个事务，AGT-09）与普通新建共用。
type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

func insertProject(ctx context.Context, db execer, p Project) error {
	canvas := p.CanvasData
	if len(canvas) == 0 {
		canvas = json.RawMessage(`{}`)
	}
	_, err := db.Exec(ctx, `
		insert into project (id, workspace_id, created_by_user_id, name,
			cover_media_id, folder_id, canvas_data, status, inherited_cover_media_id)
		values ($1, $2, $3, $4, nullif($5::bigint,0), nullif($6::bigint,0), $7, 'active',
			nullif($8::bigint,0))`,
		p.ID, p.WorkspaceID, p.CreatedBy, p.Name, p.CoverMedia, p.FolderID, canvas,
		p.InheritedCover)
	return err
}

// Get 取项目元信息（不含画布数据）。
func (r *Repo) Get(ctx context.Context, workspaceID, id int64) (Project, error) {
	return scanProject(r.pool.QueryRow(ctx,
		`select `+projectCols+` from project where id = $1 and workspace_id = $2`, id, workspaceID))
}

// GetWithCanvas 取项目详情，含画布数据。
//
// ⚠️ version 与 canvas_data 必须**同一条查询**取（AGT-10）：分两次取的话，两次之间
// 落进来一批 Agent 操作，拿到的就是「新画布 + 旧版本号」—— 前端再按这个版本补取重放，
// 同一批节点会被加两遍（实测：并发测试里报「节点 id 重复」）。
func (r *Repo) GetWithCanvas(ctx context.Context, workspaceID, id int64) (Project, error) {
	var canvas json.RawMessage
	p, err := scanProject(r.pool.QueryRow(ctx,
		`select `+projectCols+`, canvas_data from project where id = $1 and workspace_id = $2`,
		id, workspaceID), &canvas)
	p.CanvasData = canvas
	return p, err
}

// PageQuery 是列表查询条件。
type PageQuery struct {
	WorkspaceID int64
	FolderID    int64
	NoFolder    bool // 只看未分类
	Keyword     string
	Limit       int
	// Cursor 是上一页最后一条的排序键。活动列表按 updated_at 倒序，回收站按 deleted_at 倒序。
	CursorTime time.Time
	CursorID   int64
	Trash      bool
	// Kind 类型筛选：空 = 全部；canvas = 没有任何 Agent 会话；agent = 有；
	// marketing = origin 落列为 marketing（MKD-3）。
	Kind ProjectKind
}

// Page 游标分页。
//
// ⚠️ 用 `(时间, id)` 复合游标而不是 offset：边翻页边有项目被更新时，
// offset 会让同一个项目在两页里各出现一次、而另一个从没出现过。
// id 是第二排序键，用来打破同一毫秒的平局 —— 只按时间排会在时间相同时漏行。
func (r *Repo) Page(ctx context.Context, q PageQuery) ([]Project, error) {
	// 表别名 p 是 agentLateral 的约定。
	sql := `select ` + projectCols + `, ` + agentCols +
		` from project p ` + agentLateral + ` where p.workspace_id = $1 and p.status = $2`
	args := []any{q.WorkspaceID, "active"}
	if q.Trash {
		args[1] = "deleted"
	}
	if q.Keyword != "" {
		args = append(args, "%"+q.Keyword+"%")
		sql += ` and p.name ilike $` + strconv.Itoa(len(args))
	}
	switch {
	case q.NoFolder:
		sql += ` and p.folder_id is null`
	case q.FolderID > 0:
		args = append(args, q.FolderID)
		sql += ` and p.folder_id = $` + strconv.Itoa(len(args))
	}
	// 类型筛选用 exists / not exists，**不用** lateral 的结果判空：语义上「有没有会话」
	// 与「最近的那个会话是什么」是两件事，绑在一起的话以后给 lateral 加条件
	// （比如不取已取消的）会悄悄改掉筛选结果。
	switch q.Kind {
	case KindAgent:
		sql += ` and exists (select 1 from agent_session s where s.project_id = p.id)`
	case KindCanvas:
		sql += ` and not exists (select 1 from agent_session s where s.project_id = p.id)`
	case KindMarketing:
		sql += ` and p.origin = 'marketing'`
	}

	order := "p.updated_at"
	if q.Trash {
		order = "p.deleted_at"
	}
	if !q.CursorTime.IsZero() {
		args = append(args, q.CursorTime, q.CursorID)
		sql += ` and (` + order + `, p.id) < ($` + strconv.Itoa(len(args)-1) + `, $` + strconv.Itoa(len(args)) + `)`
	}
	args = append(args, q.Limit)
	sql += ` order by ` + order + ` desc, p.id desc limit $` + strconv.Itoa(len(args))

	rows, err := r.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Project, 0, q.Limit)
	for rows.Next() {
		dest, finish := agentDest()
		p, err := scanProject(rows, dest...)
		if err != nil {
			return nil, err
		}
		p.Agent = finish()
		out = append(out, p)
	}
	return out, rows.Err()
}

// AgentSummaries 按项目 ID 批量取「最近活跃的 Agent 会话」，供首页最近项目（内容域经端口）用。
// 与 Page 共用同一段 agentLateral —— 两处各写一份「最近」的规则的话，首页与全部项目页
// 早晚会点进不同的会话。只返回有会话的项目；带 workspace 条件（CD-04 第 2 条）。
func (r *Repo) AgentSummaries(ctx context.Context, workspaceID int64, projectIDs []int64) (
	map[int64]AgentBrief, error) {
	out := map[int64]AgentBrief{}
	if len(projectIDs) == 0 {
		return out, nil
	}
	rows, err := r.pool.Query(ctx, `select p.id, `+agentCols+`
		from project p `+agentLateral+`
		where p.workspace_id = $1 and p.id = any($2::bigint[]) and a.agent_session_id is not null`,
		workspaceID, projectIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var pid int64
		dest, finish := agentDest()
		if err := rows.Scan(append([]any{&pid}, dest...)...); err != nil {
			return nil, err
		}
		if b := finish(); b != nil {
			out[pid] = *b
		}
	}
	return out, rows.Err()
}

// UpdateMeta 改名/封面/文件夹。三个字段都是三态：nil = 不改。
//
// ⚠️ **不碰 version** —— version 是画布保存的乐观锁。改个名字就 +1 的话，
// 正在编辑画布的另一个标签页会凭空撞版本冲突、被迫丢弃草稿。
func (r *Repo) UpdateMeta(ctx context.Context, workspaceID, id int64,
	name *string, folderID, coverID **int64) (Project, error) {
	sql := `update project set updated_at = now()`
	args := []any{id, workspaceID}
	if name != nil {
		args = append(args, *name)
		sql += `, name = $` + strconv.Itoa(len(args))
	}
	if folderID != nil {
		args = append(args, *folderID) // *folderID 为 nil 即移出文件夹
		sql += `, folder_id = $` + strconv.Itoa(len(args))
	}
	if coverID != nil {
		args = append(args, *coverID)
		sql += `, cover_media_id = $` + strconv.Itoa(len(args))
	}
	sql += ` where id = $1 and workspace_id = $2 and status = 'active' returning ` + projectCols
	return scanProject(r.pool.QueryRow(ctx, sql, args...))
}

var ErrVersionConflict = errors.New("版本冲突")

// SaveCanvas 乐观锁保存画布。
//
// ⚠️ 条件里**四个都要带**：id、workspace_id、status='active'、version。
//   - 少了 workspace_id 就能跨工作区改别人的画布；
//   - 少了 status 就能往回收站里的项目写入，而它随时会被清理任务删掉；
//   - 少了 version 就是"最后写入者获胜"，另一个标签页几分钟的工作被静默覆盖（AC-03）。
//
// 0 行更新有两种可能，必须分开回报：版本不一致（冲突，可恢复）与项目不存在/已删（404）。
// 一律报冲突的话，用户会对着一个已经被删掉的项目反复点"重新加载"。
// ⚠️ 冲突时**必须把"是谁改的"一起查出来**（P12-02、NEW-19）：只给版本号的话，
// 用户看到的是「画布已被其他窗口修改」，而"其他窗口"有两种 ——
// 自己的另一个标签页（直接覆盖就行）与同事（覆盖等于抹掉别人的工作）。
// 分不出来他只能凭猜，而猜错的那一半正是"无声覆盖另一成员的修改"。
func (r *Repo) SaveCanvas(ctx context.Context, workspaceID, id int64, version int,
	editorUserID int64, data json.RawMessage) (ConflictState, error) {
	var st ConflictState
	err := r.pool.QueryRow(ctx, `
		update project set canvas_data = $4, version = version + 1, updated_at = now(),
			last_editor_user_id = nullif($5::bigint, 0),
			-- ⚠️ 真人整份保存要把 Agent 会话清掉（AGT-10）：不清的话冲突详情会把
			-- 这次真人保存说成「Agent 改的」，前端据此无感重放、自动重存 ——
			-- 于是同事刚存进去的内容被静默覆盖（NEW-19 要防的那件事）。
			last_editor_agent_session_id = null,
			-- MCP 授权同理（MCPT-09）：不清的话一次真人保存会被说成「外部客户端改的」。
			last_editor_mcp_grant_id = null
		where id = $1 and workspace_id = $2 and status = 'active' and version = $3
		returning version, updated_at`, id, workspaceID, version, data, editorUserID).
		Scan(&st.Version, &st.UpdatedAt)
	if err == nil {
		st.EditorKind, st.EditorID = EditorUser, editorUserID
	}
	if errors.Is(err, pgx.ErrNoRows) {
		// 0 行更新有两种原因，**必须在这一层就分开**：版本不一致（可恢复）
		// 与项目不存在/已删/不是你的。一律报冲突的话，用户会对着一个
		// 已经被删掉的项目反复点「重新加载」。
		//
		// ⚠️ 区分只能靠再查一次，而这一次顺手把"谁改的"也取回来 ——
		// 与其让 app 层为了文案再查第三次，不如在这里一次给全。
		cur, e2 := r.CanvasState(ctx, workspaceID, id)
		if e2 != nil {
			return ConflictState{}, e2 // 含 ErrNotFound
		}
		return cur, ErrVersionConflict
	}
	return st, err
}

// ConflictState 是一次冲突的现场：服务端当前是什么版本、谁改的、什么时候。
type ConflictState struct {
	Version    int
	UpdatedAt  time.Time
	EditorID   int64
	EditorName string
	// EditorKind 最后一次写入来自真人整份保存（user）还是 Agent 的画布操作（agent，AGT-10）。
	// 前端据此判断「中间改动是不是全部来自 Agent」：是就补取批次重放后自动重存，
	// 否则走冲突处理。Agent 写入时 EditorID 是会话发起人（「Agent（代 张三）」）。
	//
	// mcp（MCPT-09）：最后一次写入来自外部 AI 客户端经 MCP 授权的画布操作。EditorID 是授权所属的
	// 用户本人 —— 所以 BySelf 对 mcp 恒为 false，否则前端会说「你在另一个窗口改过」，
	// 用户点「以我为准」就把 MCP 刚建的节点整份覆盖。
	EditorKind     string
	AgentSessionID int64
	McpGrantID     int64
}

// 最后编辑者的种类。
const (
	EditorUser  = "user"
	EditorAgent = "agent"
	EditorMCP   = "mcp"
)

// CanvasState 查当前状态，供冲突详情与离线重连对账使用。
//
// ⚠️ `left join` 不是 `join`：`last_editor_user_id` 为空（P12-02 之前保存的
// 历史项目）或那个人的账号已被清理时，内连接会让整行消失 ——
// 而那时调用方拿到的是 ErrNotFound，界面会说"项目不存在"，
// 一句和"版本冲突"完全不同的话。
func (r *Repo) CanvasState(ctx context.Context, workspaceID, id int64) (ConflictState, error) {
	var st ConflictState
	var editorID *int64
	var editorName *string
	var agentSession, mcpGrant *int64
	err := r.pool.QueryRow(ctx, `
		select p.version, p.updated_at, p.last_editor_user_id, u.nickname,
			p.last_editor_agent_session_id, p.last_editor_mcp_grant_id
		from project p
		left join app_user u on u.id = p.last_editor_user_id
		where p.id = $1 and p.workspace_id = $2 and p.status = 'active'`, id, workspaceID).
		Scan(&st.Version, &st.UpdatedAt, &editorID, &editorName, &agentSession, &mcpGrant)
	if errors.Is(err, pgx.ErrNoRows) {
		return ConflictState{}, ErrNotFound
	}
	st.EditorKind = EditorUser
	switch {
	case agentSession != nil:
		st.EditorKind, st.AgentSessionID = EditorAgent, *agentSession
	case mcpGrant != nil:
		st.EditorKind, st.McpGrantID = EditorMCP, *mcpGrant
	}
	if editorID != nil {
		st.EditorID = *editorID
	}
	if editorName != nil {
		st.EditorName = *editorName
	}
	return st, err
}

// SoftDelete 软删。带 status 条件，重复删除返回 0 行而不是刷新 deleted_at ——
// 刷新会让保留期重新开始计算。
func (r *Repo) SoftDelete(ctx context.Context, workspaceID, id int64) error {
	tag, err := r.pool.Exec(ctx, `
		update project set status = 'deleted', deleted_at = now(), updated_at = now()
		where id = $1 and workspace_id = $2 and status = 'active'`, id, workspaceID)
	return affected(tag, err)
}

// Restore 恢复。原文件夹若已被删掉就落回未分类 —— 否则项目会挂在一个查不到的
// folder_id 上，列表里哪个文件夹都看不到它。
func (r *Repo) Restore(ctx context.Context, workspaceID, id int64) (Project, error) {
	return scanProject(r.pool.QueryRow(ctx, `
		update project set status = 'active', deleted_at = null, updated_at = now(),
			folder_id = (select f.id from folder f where f.id = project.folder_id)
		where id = $1 and workspace_id = $2 and status = 'deleted'
		returning `+projectCols, id, workspaceID))
}

// WorkspaceMedia 是某个工作区里的一个媒体：清理导演台记录之后要交给媒体域释放的文件。
type WorkspaceMedia struct {
	WorkspaceID int64
	MediaID     int64
}

// PurgeResult 是彻底删除的结果。
type PurgeResult struct {
	// DirectorMedia 是随项目删掉的导演台媒体记录指向的文件（去重）。调用方在提交之后逐个交给
	// media.Delete：仍被复制出来的项目引用着的会被检查器挡下（正常跳过），其余进入媒体软删（DRD-30）。
	DirectorMedia []WorkspaceMedia
}

// Purge 彻底删除项目行，并在**同一事务**里删掉它的导演台三张表（DRD-30）。
//
// ⚠️ 只删项目自身与从属于它的导演台行。账本流水、已发布作品、媒体对象都不在这里删（GAP-17）：
// 账本必须保持连续可审计，媒体是工作区级资源、可能还被别的项目引用 —— 媒体交给调用方在提交之后
// 走 media.Delete（计算式检查）。项目文档仍留在库里（既有缺陷 DRB-3，单独处理）。
// ⚠️ 先 `for update` 锁住项目行再删导演台行：不锁的话，同时发生的「恢复」会让一个刚恢复的项目
// 丢掉它的场景。
// purgeExtra 在清理事务里做别的域的清理（智能剪辑的时间线，SCT-07）；为 nil 时不做。
type PurgeExtra func(ctx context.Context, tx pgx.Tx, projectIDs []int64) error

func (r *Repo) Purge(ctx context.Context, workspaceID, id int64, extra PurgeExtra) (PurgeResult, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return PurgeResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var locked int64
	err = tx.QueryRow(ctx, `
		select id from project where id = $1 and workspace_id = $2 and status = 'deleted' for update`,
		id, workspaceID).Scan(&locked)
	if errors.Is(err, pgx.ErrNoRows) {
		return PurgeResult{}, ErrNotFound
	}
	if err != nil {
		return PurgeResult{}, err
	}
	media, err := purgeDirectorRows(ctx, tx, []int64{id})
	if err != nil {
		return PurgeResult{}, err
	}
	if extra != nil {
		if err := extra(ctx, tx, []int64{id}); err != nil {
			return PurgeResult{}, err
		}
	}
	tag, err := tx.Exec(ctx,
		`delete from project where id = $1 and workspace_id = $2 and status = 'deleted'`, id, workspaceID)
	if err := affected(tag, err); err != nil {
		return PurgeResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return PurgeResult{}, err
	}
	return PurgeResult{DirectorMedia: media}, nil
}

// PurgeExpired 清理超保留期的回收站项目，返回被删的 ID 与要释放的导演台媒体。
//
// 返回 ID 而不是只返回条数：调用方要据此解除媒体引用、写清理日志。
// 一次限量是为了避免单次删几十万行长时间持锁，把正常的保存请求堵住。
// 导演台三张表与项目在**同一事务**里删（DRD-30）：先 `for update` 选出并锁住这一批项目，
// 再按项目 ID 删导演台行，最后删项目 —— 中途失败整体回滚，不会出现「项目还在、场景没了」。
func (r *Repo) PurgeExpired(ctx context.Context, before time.Time, limit int, extra PurgeExtra) (
	[]int64, []WorkspaceMedia, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	ids, err := scanIDs(tx.Query(ctx, `
		select id from project
		where status = 'deleted' and deleted_at is not null and deleted_at < $1
		order by deleted_at limit $2
		for update`, before, limit))
	if err != nil || len(ids) == 0 {
		return nil, nil, err
	}
	media, err := purgeDirectorRows(ctx, tx, ids)
	if err != nil {
		return nil, nil, err
	}
	if extra != nil {
		if err := extra(ctx, tx, ids); err != nil {
			return nil, nil, err
		}
	}
	deleted, err := scanIDs(tx.Query(ctx, `
		delete from project where id = any($1) and status = 'deleted' returning id`, ids))
	if err != nil {
		return nil, nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, nil, err
	}
	return deleted, media, nil
}

// purgeDirectorRows 在事务里删掉这些项目的导演台三张表，返回媒体记录指向的文件（按工作区去重）。
func purgeDirectorRows(ctx context.Context, tx pgx.Tx, projectIDs []int64) ([]WorkspaceMedia, error) {
	if _, err := tx.Exec(ctx, `delete from director_llm_call where project_id = any($1)`, projectIDs); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `
		delete from director_media where project_id = any($1) returning workspace_id, media_id`, projectIDs)
	if err != nil {
		return nil, err
	}
	seen := map[WorkspaceMedia]bool{}
	var media []WorkspaceMedia
	for rows.Next() {
		var m WorkspaceMedia
		if err := rows.Scan(&m.WorkspaceID, &m.MediaID); err != nil {
			rows.Close()
			return nil, err
		}
		if !seen[m] {
			seen[m] = true
			media = append(media, m)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `delete from director_scene where project_id = any($1)`, projectIDs); err != nil {
		return nil, err
	}
	return media, nil
}

func scanIDs(rows pgx.Rows, err error) ([]int64, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// ---------------------------------------------------------------- 文件夹

func (r *Repo) ListFolders(ctx context.Context, workspaceID int64) ([]Folder, error) {
	// 项目计数只算活动项目：回收站里的还算在文件夹里会让用户以为没删干净。
	rows, err := r.pool.Query(ctx, `
		select f.id, f.name, f.sort, f.created_at,
		       (select count(*) from project p
		         where p.folder_id = f.id and p.status = 'active') as cnt
		from folder f where f.workspace_id = $1
		order by f.sort, f.created_at`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Folder
	for rows.Next() {
		var f Folder
		if err := rows.Scan(&f.ID, &f.Name, &f.Sort, &f.CreatedAt, &f.ProjectCount); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

func (r *Repo) CountFolders(ctx context.Context, workspaceID int64) (int, error) {
	var n int
	err := r.pool.QueryRow(ctx,
		`select count(*) from folder where workspace_id = $1`, workspaceID).Scan(&n)
	return n, err
}

func (r *Repo) InsertFolder(ctx context.Context, id, workspaceID int64, name string) (Folder, error) {
	var f Folder
	err := r.pool.QueryRow(ctx, `
		insert into folder (id, workspace_id, name) values ($1, $2, $3)
		returning id, name, sort, created_at`, id, workspaceID, name).
		Scan(&f.ID, &f.Name, &f.Sort, &f.CreatedAt)
	return f, mapDuplicate(err)
}

func (r *Repo) RenameFolder(ctx context.Context, workspaceID, id int64, name string) (Folder, error) {
	var f Folder
	err := r.pool.QueryRow(ctx, `
		update folder set name = $3, updated_at = now()
		where id = $1 and workspace_id = $2
		returning id, name, sort, created_at`, id, workspaceID, name).
		Scan(&f.ID, &f.Name, &f.Sort, &f.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return f, ErrNotFound
	}
	return f, mapDuplicate(err)
}

// DeleteFolder 删文件夹，夹内项目落回未分类。
//
// ⚠️ 两步必须在同一事务：先删文件夹再置空的话，中间崩溃会留下一批
// 指向不存在文件夹的项目 —— 它们在任何文件夹视图里都查不到，等于消失。
func (r *Repo) DeleteFolder(ctx context.Context, workspaceID, id int64) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx,
		`update project set folder_id = null, updated_at = now()
		 where folder_id = $1 and workspace_id = $2`, id, workspaceID); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx,
		`delete from folder where id = $1 and workspace_id = $2`, id, workspaceID)
	if err := affected(tag, err); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// FolderExists 校验文件夹属于本工作区。移动项目时必须查 ——
// 否则可以把项目移进别人工作区的文件夹 ID，列表里从此消失。
func (r *Repo) FolderExists(ctx context.Context, workspaceID, id int64) (bool, error) {
	var n int
	err := r.pool.QueryRow(ctx,
		`select count(*) from folder where id = $1 and workspace_id = $2`, id, workspaceID).Scan(&n)
	return n > 0, err
}

// ---------------------------------------------------------------- 模板

func (r *Repo) ListTemplates(ctx context.Context) ([]Template, error) {
	rows, err := r.pool.Query(ctx, `
		select code, name, coalesce(description,''), sort
		from project_template where is_enabled = true order by sort, code`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Template
	for rows.Next() {
		var t Template
		if err := rows.Scan(&t.Code, &t.Name, &t.Description, &t.Sort); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (r *Repo) GetTemplate(ctx context.Context, code string) (Template, error) {
	var t Template
	err := r.pool.QueryRow(ctx, `
		select code, name, coalesce(description,''), canvas_data, sort
		from project_template where code = $1 and is_enabled = true`, code).
		Scan(&t.Code, &t.Name, &t.Description, &t.CanvasData, &t.Sort)
	if errors.Is(err, pgx.ErrNoRows) {
		return t, ErrNotFound
	}
	return t, err
}

// ---------------------------------------------------------------- 小工具

func affected(tag pgconn.CommandTag, err error) error {
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func mapDuplicate(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return ErrDuplicate
	}
	return err
}

// GetForShare 按 ID 取项目，**不带 workspace_id 条件**。
//
// ⚠️ 这是本仓储里唯一绕过租户条件的查询，只允许共享域在校验完
// resource_share 授权之后调用（见 collaboration/app.ResourceProvider）。
// 受让方本来就不在这个工作区里，带上 workspace_id 只会永远查不到 ——
// 但也正因为它不带，**绝不能**把它接到任何直接由请求参数驱动的接口上。
// 返回的 workspace_id 由调用方核对归属（共享给谁、由谁授出）。
func (r *Repo) GetForShare(ctx context.Context, id int64) (Project, error) {
	p, err := scanProject(r.pool.QueryRow(ctx,
		`select `+projectCols+` from project where id = $1 and status = 'active'`, id))
	if err != nil {
		return p, err
	}
	err = r.pool.QueryRow(ctx,
		`select coalesce(canvas_data, '{}'::jsonb) from project where id = $1`, id).
		Scan(&p.CanvasData)
	return p, err
}

// MetaForShare 与 GetForShare 同样绕过租户条件，但**不取 canvas_data**。
// 列表场景用它：一屏几十行，每份画布 JSON 几百 KB 到几 MB。
func (r *Repo) MetaForShare(ctx context.Context, id int64) (Project, error) {
	return scanProject(r.pool.QueryRow(ctx,
		`select `+projectCols+` from project where id = $1 and status = 'active'`, id))
}
