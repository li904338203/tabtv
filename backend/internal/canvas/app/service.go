// Package app 编排项目/文件夹/回收站用例。
//
// 分层：transport → app → domain / repo。角色判定一律走协作域的 Can()，
// 本包不自己比较角色字符串（散落的判断迟早漏一处）。
package app

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/jackc/pgx/v5"
	"log/slog"
	"strconv"
	"time"

	"github.com/li904338203/tabtv/backend/internal/canvas/domain"
	"github.com/li904338203/tabtv/backend/internal/canvas/repo"
	collab "github.com/li904338203/tabtv/backend/internal/collaboration/domain"
	director "github.com/li904338203/tabtv/backend/internal/director/domain"
	"github.com/li904338203/tabtv/backend/internal/platform/audit"
	smartcut "github.com/li904338203/tabtv/backend/internal/smartcut/domain"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// DefaultRetention 回收站保留期。
//
// ⚠️ 这是**设计建议值**（PARAM 未定稿的容量类参数同理），不是用户已批准的政策；
// 可由 SEEAITV_TRASH_RETENTION 覆盖。
const DefaultRetention = 30 * 24 * time.Hour

type Service struct {
	repo      *repo.Repo
	nextID    func() int64
	audit     *audit.Logger
	log       *slog.Logger
	retention time.Duration
	// rebind 是生成域（P05）接入的产物重绑定器；为 nil 时复制只解绑不重绑。
	rebind domain.OutputRebinder
	// media 由媒体域注入：项目被彻底删除时解除它持有的媒体引用，
	// 解除后那些文件才可能被回收队列按「无引用 + 超保留期」收走（GAP-17）。
	media MediaReleaser
	// activeTasks 由生成域（P05）注入，用来在彻底删除前拦住还有在途任务的项目。
	// 为 nil 时不拦 —— 此刻系统里还不存在任务，拦不拦都一样；
	// 但接缝必须现在就留出来，否则 P05 会去 canvas 域里直接查 generation 表。
	activeTasks ActiveTaskCounter
	// 输入解析用的三个查询端口（P04-05）。都为 nil 时解析会明确判"未就绪"，
	// 而不是放行 —— 查不到就放行等于把别人的资源当成已授权资产。
	outputs     OutputLookup
	mediaLookup MediaLookup
	materials   MaterialLookup
	// quick 是创造页直接生成的报价/余额/受理端口（AGT-42，见 quick.go）。
	quick QuickAccepter
	// events 推送 canvas.ops_applied（AGT-10，见 events.go）。为 nil 时不推。
	events EventPublisher
	// directorMedia 是 3D 导演台的媒体上传与软删（DRT-03，见 director_media.go），由媒体域实现。
	directorMedia  DirectorMediaStore
	directorLimits DirectorUploadLimits
	// timelines 项目复制 / 清理时带上智能剪辑的时间线（SCT-07，见 timelines.go）。
	timelines ProjectTimelines
}

// MediaReleaser 解除某业务对象持有的媒体引用。
//
// ⚠️ 只解除引用，**不直接删文件**：同一个媒体可能被别的项目引用着
// （复制项目、跨项目复用资产），删文件是回收队列按"没人再引用"判断的事。
type MediaReleaser interface {
	ReleaseUsage(ctx context.Context, usageType string, refID int64) (int64, error)
	RegisterUsage(ctx context.Context, mediaID int64, usageType string, refID int64) error
}

// usageProjectCover 与 media/app.UsageProjectCover 是同一个值。
// ⚠️ 这里刻意重写一遍字面量而不是 import 媒体域的常量：
// 领域之间只通过上面那个小接口耦合，为一个字符串把整个包拉进来不值得。
// 代价是两处必须一致 —— 拼错的后果是"删除时查不到引用"，于是封面被回收、项目图裂。
const usageProjectCover = "project_cover"

// ActiveTaskCounter 统计项目上还有多少在途任务（queued/running）。
type ActiveTaskCounter interface {
	CountActive(ctx context.Context, projectID int64) (int, error)
}

func New(r *repo.Repo, nextID func() int64) *Service {
	return &Service{repo: r, nextID: nextID, retention: DefaultRetention}
}

func (s *Service) SetAudit(a *audit.Logger)            { s.audit = a }
func (s *Service) SetLogger(l *slog.Logger)            { s.log = l }
func (s *Service) SetRetention(d time.Duration)        { s.retention = d }
func (s *Service) SetRebinder(r domain.OutputRebinder) { s.rebind = r }
func (s *Service) SetActiveTasks(c ActiveTaskCounter)  { s.activeTasks = c }
func (s *Service) SetMedia(m MediaReleaser)            { s.media = m }
func (s *Service) Retention() time.Duration            { return s.retention }

// requireRole 统一的角色闸。
func requireRole(role collab.Role, action collab.Action) error {
	if !role.Can(action) {
		return httpx.Errf(errcode.PermissionDenied, "当前角色无权执行该操作")
	}
	return nil
}

func wrapRepo(err error) error {
	switch {
	case errors.Is(err, repo.ErrNotFound):
		return httpx.Err(errcode.NotFound)
	case errors.Is(err, repo.ErrDuplicate):
		return httpx.Errf(errcode.DuplicateName, "同名文件夹已存在")
	case err != nil:
		return httpx.Wrap(errcode.Internal, err)
	}
	return nil
}

// ---------------------------------------------------------------- 项目

type CreateInput struct {
	Name         string
	FolderID     int64
	TemplateCode string
}

func (s *Service) Create(ctx context.Context, ws, userID int64, role collab.Role,
	in CreateInput) (repo.Project, error) {
	if err := requireRole(role, collab.ActionEdit); err != nil {
		return repo.Project{}, err
	}
	name, err := domain.ValidateProjectName(in.Name)
	if err != nil {
		return repo.Project{}, httpx.Errf(errcode.InvalidArgument, err.Error())
	}
	if err := s.checkFolder(ctx, ws, in.FolderID); err != nil {
		return repo.Project{}, err
	}

	canvas := json.RawMessage(`{}`)
	if in.TemplateCode != "" {
		tpl, err := s.repo.GetTemplate(ctx, in.TemplateCode)
		if err != nil {
			return repo.Project{}, httpx.Errf(errcode.NotFound, "模板不存在或已停用: "+in.TemplateCode)
		}
		canvas = tpl.CanvasData
	}

	p := repo.Project{
		ID: s.nextID(), WorkspaceID: ws, CreatedBy: userID,
		Name: name, FolderID: in.FolderID, CanvasData: canvas,
	}
	if err := s.repo.Insert(ctx, p); err != nil {
		return repo.Project{}, httpx.Wrap(errcode.Internal, err)
	}
	return s.repo.Get(ctx, ws, p.ID)
}

func (s *Service) Get(ctx context.Context, ws, id int64, role collab.Role) (repo.Project, error) {
	if err := requireRole(role, collab.ActionView); err != nil {
		return repo.Project{}, err
	}
	p, err := s.repo.GetWithCanvas(ctx, ws, id)
	return p, wrapRepo(err)
}

type ListInput struct {
	FolderID int64
	NoFolder bool
	Keyword  string
	Limit    int
	Cursor   string
	Trash    bool
	// Kind 类型筛选（AGT-45），已由 transport 按白名单解析。
	Kind repo.ProjectKind
}

type Page struct {
	Items      []repo.Project
	NextCursor string
	HasMore    bool
}

func (s *Service) List(ctx context.Context, ws int64, role collab.Role, in ListInput) (Page, error) {
	if err := requireRole(role, collab.ActionView); err != nil {
		return Page{}, err
	}
	limit := in.Limit
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	q := repo.PageQuery{
		WorkspaceID: ws, FolderID: in.FolderID, NoFolder: in.NoFolder,
		Keyword: in.Keyword, Trash: in.Trash, Kind: in.Kind,
		// 多查一条用来判断 hasMore：用 count(*) 再查一次会在翻页时
		// 因为数据变动而与本页结果不自洽，而且多一次全表条件扫描。
		Limit: limit + 1,
	}
	if in.Cursor != "" {
		t, id, err := decodeCursor(in.Cursor)
		if err != nil {
			return Page{}, httpx.Errf(errcode.InvalidArgument, "cursor 非法")
		}
		q.CursorTime, q.CursorID = t, id
	}
	rows, err := s.repo.Page(ctx, q)
	if err != nil {
		return Page{}, httpx.Wrap(errcode.Internal, err)
	}

	out := Page{Items: rows}
	if len(rows) > limit {
		out.Items = rows[:limit]
		out.HasMore = true
		last := out.Items[len(out.Items)-1]
		ts := last.UpdatedAt
		if in.Trash && last.DeletedAt != nil {
			ts = *last.DeletedAt
		}
		out.NextCursor = encodeCursor(ts, last.ID)
	}
	return out, nil
}

type UpdateInput struct {
	Name     *string
	FolderID **int64
	CoverID  **int64
}

func (s *Service) Update(ctx context.Context, ws, id int64, role collab.Role,
	in UpdateInput) (repo.Project, error) {
	if err := requireRole(role, collab.ActionEdit); err != nil {
		return repo.Project{}, err
	}
	if in.Name == nil && in.FolderID == nil && in.CoverID == nil {
		return repo.Project{}, httpx.Errf(errcode.InvalidArgument, "没有要更新的字段")
	}
	if in.Name != nil {
		name, err := domain.ValidateProjectName(*in.Name)
		if err != nil {
			return repo.Project{}, httpx.Errf(errcode.InvalidArgument, err.Error())
		}
		in.Name = &name
	}
	if in.FolderID != nil && *in.FolderID != nil {
		if err := s.checkFolder(ctx, ws, **in.FolderID); err != nil {
			return repo.Project{}, err
		}
	}
	// 换封面前先看旧的是哪张：旧封面的引用要解除，否则它永远被算作"还在用"，
	// 回收队列就再也收不走它。
	var oldCover int64
	if in.CoverID != nil {
		if cur, err := s.repo.Get(ctx, ws, id); err == nil {
			oldCover = cur.CoverMedia
		}
	}

	p, err := s.repo.UpdateMeta(ctx, ws, id, in.Name, in.FolderID, in.CoverID)
	if err != nil {
		return p, wrapRepo(err)
	}

	// ⚠️ 引用登记放在更新**之后**：更新可能因为项目已删而失败，
	// 先登记就会留下一条指向不存在项目的引用，把媒体永久钉住。
	if s.media != nil && in.CoverID != nil {
		if oldCover != 0 && oldCover != p.CoverMedia {
			if _, err := s.media.ReleaseUsage(ctx, usageProjectCover, id); err != nil {
				s.logWarn(ctx, "解除旧封面引用失败", id, err)
			}
		}
		if p.CoverMedia != 0 {
			if err := s.media.RegisterUsage(ctx, p.CoverMedia, usageProjectCover, id); err != nil {
				// 登记失败不回滚改封面：封面已经换了，而引用没登记上
				// 最坏是这张图被回收队列收走 —— 比让用户改不了封面轻。
				s.logWarn(ctx, "登记封面引用失败", id, err)
			}
		}
	}
	return p, nil
}

// SaveCanvasResult 是保存成功的结果。
type SaveCanvasResult struct {
	Version   int
	UpdatedAt time.Time
}

// SaveCanvas 乐观锁保存画布。
//
// ⚠️ 冲突**不是错误处理的边角**，而是这条链路的主要设计：两个标签页、
// 两台设备、一次网络重试都会走到这里。返回 40902 并带上服务端当前版本，
// 客户端据此保留本地草稿并让用户选择重载或放弃，**不得静默覆盖**（AC-03、FLOW-09）。
func (s *Service) SaveCanvas(ctx context.Context, ws, id, userID int64, role collab.Role,
	version int, data json.RawMessage) (SaveCanvasResult, error) {
	if err := requireRole(role, collab.ActionEdit); err != nil {
		return SaveCanvasResult{}, err
	}
	if version < 1 {
		// 不接受 0：那是"我没读过服务端版本"的意思，等于要求无条件覆盖。
		return SaveCanvasResult{}, httpx.Errf(errcode.InvalidArgument,
			"version 必须来自服务端，不能为 0")
	}
	if err := domain.ValidateCanvas(data); err != nil {
		return SaveCanvasResult{}, httpx.Errf(errcode.InvalidArgument, err.Error())
	}

	st, err := s.repo.SaveCanvas(ctx, ws, id, version, userID, data)
	switch {
	case errors.Is(err, repo.ErrVersionConflict):
		return SaveCanvasResult{}, httpx.Errf(errcode.VersionConflict,
			conflictMessage(st, userID)).WithDetails(map[string]any{
			"currentVersion":   st.Version,
			"currentUpdatedAt": st.UpdatedAt.UTC().Format(time.RFC3339),
			// ⚠️ "是谁改的"是这条错误里最有用的一项（NEW-19）：
			// 自己的另一个标签页可以直接覆盖，同事则要先问一句。
			"currentEditorId":   strconv.FormatInt(st.EditorID, 10),
			"currentEditorName": st.EditorName,
			"bySelf":            BySelf(st, userID),
			// AGT-10：中间那次改动是 Agent 的画布操作还是真人整份保存。
			// 前者前端可以补取批次、重放后自动重存（用户无感）；后者必须让用户选。
			// MCPT-09：mcp 与 agent 同样是机器写入，前端同样补取重放后自动重存。
			"editorKind":     st.EditorKind,
			"agentSessionId": agentSessionIDString(st),
			"mcpGrantId":     idOrEmpty(st.McpGrantID),
		})
	case errors.Is(err, repo.ErrNotFound):
		return SaveCanvasResult{}, httpx.Err(errcode.NotFound)
	case err != nil:
		return SaveCanvasResult{}, httpx.Wrap(errcode.Internal, err)
	}
	return SaveCanvasResult{Version: st.Version, UpdatedAt: st.UpdatedAt}, nil
}

// ApplyTemplate 给**空画布**套用一个模板（空画布上的四张快捷生成卡片）。
//
// 老系统的语义原样保留（`POST /projects/{id}/template`）：只允许空画布、带版本号。
// ⚠️ 「是不是空的」与「写进去」之间要是被别人改过，必须是冲突而不是覆盖 ——
//
//	所以写入复用保存画布的**乐观锁**那条路：检查读的是版本 V 的画布，写入要求仍是 V。
//	分开做（先查空、再无条件写）的话，同事刚画上去的节点会被一张模板静默盖掉。
//
// ⚠️ 模板内容只有一份（`project_template` 表，新建项目时选模板用的也是它）。
//
//	在前端再写一份模板画布的话，两边早晚会不一样。
func (s *Service) ApplyTemplate(ctx context.Context, ws, id, userID int64, role collab.Role,
	templateCode string, version int) (repo.Project, error) {
	if err := requireRole(role, collab.ActionEdit); err != nil {
		return repo.Project{}, err
	}
	if version < 1 {
		return repo.Project{}, httpx.Errf(errcode.InvalidArgument, "version 必须来自服务端，不能为 0")
	}
	tpl, err := s.repo.GetTemplate(ctx, templateCode)
	if err != nil {
		return repo.Project{}, httpx.Errf(errcode.NotFound, "模板不存在或已停用: "+templateCode)
	}
	cur, err := s.repo.GetWithCanvas(ctx, ws, id)
	if err != nil {
		return repo.Project{}, wrapRepo(err)
	}
	if cur.Status != "active" {
		return repo.Project{}, httpx.Errf(errcode.InvalidStateTransition, "回收站中的项目不能套用模板，请先恢复")
	}
	if !domain.CanvasIsEmpty(cur.CanvasData) {
		return repo.Project{}, httpx.Errf(errcode.InvalidStateTransition, "仅空画布可以套用模板")
	}
	if _, err := s.SaveCanvas(ctx, ws, id, userID, role, version, tpl.CanvasData); err != nil {
		return repo.Project{}, err
	}
	s.writeAudit(ctx, userID, "project.apply_template", id, map[string]any{"templateCode": templateCode})
	return wrapProject(s.repo.GetWithCanvas(ctx, ws, id))
}

func wrapProject(p repo.Project, err error) (repo.Project, error) {
	return p, wrapRepo(err)
}

// conflictMessage 把"是谁"直接写进文案。
//
// ⚠️ 前端仍然会按 `bySelf` / `currentEditorName` 自己组织措辞，这里给的是
// **兜底**：任何一个没接这个字段的客户端（旧版本、脚本、后台）
// 也该看到一句说得清的话，而不是一句放之四海而皆准的"已被其他窗口修改"。
func conflictMessage(st repo.ConflictState, userID int64) string {
	switch {
	case st.EditorKind == repo.EditorAgent && st.EditorName != "":
		return "AI 助手（代 " + st.EditorName + "）修改了这张画布，请重新加载后再保存"
	case st.EditorKind == repo.EditorAgent:
		return "AI 助手修改了这张画布，请重新加载后再保存"
	// ⚠️ 客户端名不在这里：canvas 域不读 mcp_* 表，冲突现场只有授权 ID。
	// 前端按 editorKind=mcp 自动补取重放，补取到的节点上带着 origin.clientName。
	case st.EditorKind == repo.EditorMCP && st.EditorName != "":
		return "外部 AI 客户端（代 " + st.EditorName + "）修改了这张画布，请重新加载后再保存"
	case st.EditorKind == repo.EditorMCP:
		return "外部 AI 客户端修改了这张画布，请重新加载后再保存"
	case st.EditorID != 0 && st.EditorID == userID:
		return "你在另一个窗口修改过这张画布，请重新加载或选择以当前窗口为准"
	case st.EditorName != "":
		return st.EditorName + " 修改了这张画布，请重新加载后再保存"
	default:
		return "画布已被其他窗口修改，请重新加载"
	}
}

// BySelf 最后一次写入是不是当前用户**本人亲手**保存的。
//
// ⚠️ Agent / MCP 的写入不算（AGT-10、MCPT-09）：机器写入时最后编辑者是会话发起人 / 授权所属的
// 用户本人，按 ID 比的话他自己会被告知「你在另一个窗口改过」，进而点「以我为准」——
// 把机器刚加的节点整份覆盖掉。前端对机器写入另有处置（补取重放），看 editorKind。
// ⚠️ 判据是「只有 user 才可能是本人」，不是「不是 agent」：后者在新增第三种机器来源时
// 会悄悄把它算成本人（MCPT-09 之前正是这么写的）。
func BySelf(st repo.ConflictState, userID int64) bool {
	return st.EditorKind == repo.EditorUser && st.EditorID != 0 && st.EditorID == userID
}

func agentSessionIDString(st repo.ConflictState) string {
	return idOrEmpty(st.AgentSessionID)
}

// CanvasState 供离线重连对账：客户端恢复网络后先问一句"服务端现在是第几版"。
//
// ⚠️ 不对账就直接重发的话，离线期间同事改过时用户收到的是一个 409 ——
// 而他刚刚才看到"网络已恢复"，会以为是重连本身出了问题。
func (s *Service) CanvasState(ctx context.Context, ws, id int64, role collab.Role) (
	repo.ConflictState, error) {
	if err := requireRole(role, collab.ActionView); err != nil {
		return repo.ConflictState{}, err
	}
	st, err := s.repo.CanvasState(ctx, ws, id)
	if errors.Is(err, repo.ErrNotFound) {
		return repo.ConflictState{}, httpx.Err(errcode.NotFound)
	}
	if err != nil {
		return repo.ConflictState{}, httpx.Wrap(errcode.Internal, err)
	}
	return st, nil
}

// Duplicate 复制项目。
//
// 复制的是**画布与封面**，不复制 generation 与账本流水（CV-03）：
// 副本不该凭空拥有一份消费记录，账本也不该出现没有真实扣费的行。
func (s *Service) Duplicate(ctx context.Context, ws, userID, id int64, role collab.Role,
	name string) (repo.Project, error) {
	if err := requireRole(role, collab.ActionEdit); err != nil {
		return repo.Project{}, err
	}
	src, err := s.repo.GetWithCanvas(ctx, ws, id)
	if err != nil {
		return repo.Project{}, wrapRepo(err)
	}
	if src.Status != "active" {
		// 回收站里的项目不能复制：复制出来的副本会让用户以为原项目已恢复。
		return repo.Project{}, httpx.Errf(errcode.InvalidArgument, "回收站中的项目不能复制，请先恢复")
	}

	copyName := domain.CopyName(src.Name)
	if name != "" {
		if copyName, err = domain.ValidateProjectName(name); err != nil {
			return repo.Project{}, httpx.Errf(errcode.InvalidArgument, err.Error())
		}
	}
	canvas, stats, err := domain.CopyCanvasData(src.CanvasData, s.rebind)
	if err != nil {
		return repo.Project{}, httpx.Errf(errcode.Internal, err.Error())
	}

	dst := repo.Project{
		ID: s.nextID(), WorkspaceID: ws, CreatedBy: userID, Name: copyName,
		CoverMedia: src.CoverMedia, FolderID: src.FolderID, CanvasData: canvas,
		// ⚠️ 副本不拥有源项目的生成记录（见上面 CopyCanvasData 的两条规则），
		//    展示封面按「自己的产出」永远算不出来。继承的是源项目**此刻显示的**封面
		//    （显式 ?? 产出图 ?? 它自己继承的），所以「副本的副本」也对。
		//    ⚠️ 不写进 CoverMedia：那一列是「用户选的封面」，写进去就把副本钉死在这张图上、
		//    还会让删图保护把它当成引用（迁移 00031 的文件头）。
		InheritedCover: src.DisplayCover,
	}
	// 文档与 3D 导演台场景随项目复制，且与建副本**同一个事务**（AGT-09、DRD-30）：分开做的话中间失败
	// 会留下一个文档 / 导演台节点指向源项目的副本。docId / sceneId 的重绑在拿到新 ID 之后才能做，所以交给回调。
	// 智能剪辑的时间线同理（SCT-07，SC-F-170）：由时间线域在同一个事务里复制，画布里的 timelineId 改指副本。
	reboundDocs, reboundScenes, reboundTimelines := 0, 0, 0
	var copyTimelines repo.CopyHook
	if s.timelines != nil {
		copyTimelines = func(ctx context.Context, tx pgx.Tx) (map[int64]int64, error) {
			return s.timelines.CopyForProjectInTx(ctx, tx, ws, src.ID, dst.ID)
		}
	}
	if err := s.repo.InsertCopy(ctx, dst, src.ID, s.nextID,
		func(ids repo.CopyIDMaps) (json.RawMessage, error) {
			out, n, err := domain.RebindDocIDs(canvas, ids.Docs)
			if err != nil {
				return nil, err
			}
			reboundDocs = n
			out, n, err = director.RebindSceneIDs(out, ids.Scenes)
			if err != nil {
				return nil, err
			}
			reboundScenes = n
			out, n, err = smartcut.RebindTimelineIDs(out, ids.Timelines)
			reboundTimelines = n
			return out, err
		}, copyTimelines); err != nil {
		return repo.Project{}, httpx.Wrap(errcode.Internal, err)
	}
	s.writeAudit(ctx, userID, "project.duplicate", dst.ID, map[string]any{
		"sourceProjectId": itoa(src.ID), "nodes": stats.Nodes,
		"unboundResults": stats.UnboundResults, "reboundDocuments": reboundDocs,
		"reboundDirectorScenes": reboundScenes, "reboundSmartcutTimelines": reboundTimelines,
	})
	return s.repo.Get(ctx, ws, dst.ID)
}

func (s *Service) Delete(ctx context.Context, ws, userID, id int64, role collab.Role) error {
	if err := requireRole(role, collab.ActionEdit); err != nil {
		return err
	}
	if err := wrapRepo(s.repo.SoftDelete(ctx, ws, id)); err != nil {
		return err
	}
	s.writeAudit(ctx, userID, "project.delete", id, nil)
	return nil
}

func (s *Service) Restore(ctx context.Context, ws, userID, id int64, role collab.Role) (repo.Project, error) {
	if err := requireRole(role, collab.ActionEdit); err != nil {
		return repo.Project{}, err
	}
	p, err := s.repo.Restore(ctx, ws, id)
	if err != nil {
		return p, wrapRepo(err)
	}
	s.writeAudit(ctx, userID, "project.restore", id, nil)
	return p, nil
}

// Purge 彻底删除。
//
// ⚠️ 需要 manage 权限，比软删更严：软删可恢复，彻底删除不可逆，
// 让每个 editor 都能一键抹掉项目不合理。
func (s *Service) Purge(ctx context.Context, ws, userID, id int64, role collab.Role) error {
	if err := requireRole(role, collab.ActionManage); err != nil {
		return err
	}
	if s.activeTasks != nil {
		n, err := s.activeTasks.CountActive(ctx, id)
		if err != nil {
			return httpx.Wrap(errcode.Internal, err)
		}
		if n > 0 {
			// 静默删除会让 Worker 回写时找不到项目，任务卡在 running 直到超时墙。
			return httpx.Errf(errcode.ResourceInUse,
				"项目上还有 "+strconv.Itoa(n)+" 个进行中的任务，请等待完成或取消")
		}
	}
	purged, err := s.repo.Purge(ctx, ws, id, s.purgeHook())
	if err := wrapRepo(err); err != nil {
		return err
	}
	// 导演台的文件在**提交之后**交给媒体域（DRD-30）：先释放再删的话，中途失败会让还在的项目丢掉文件。
	directorReleased, directorKept := s.releaseDirectorMedia(ctx, purged.DirectorMedia)
	// 引用解除放在删除**之后**：先解除再删，中间失败就会留下一个
	// 还在但已经没有引用的项目，它的封面随时可能被回收队列收走。
	var released int64
	if s.media != nil {
		n, err := s.media.ReleaseUsage(ctx, "project_cover", id)
		if err != nil {
			// 解除失败不回滚删除：项目已经没了，引用留着最多是文件晚点回收，
			// 而为此让删除失败会把用户卡在"删不掉"上。
			s.logWarn(ctx, "解除项目媒体引用失败", id, err)
		}
		released = n
	}
	s.writeAudit(ctx, userID, "project.purge", id, map[string]any{
		"releasedMediaUsages":    released,
		"releasedDirectorMedia":  directorReleased,
		"directorMediaStillUsed": directorKept,
	})
	return nil
}

// releaseDirectorMedia 把随项目删掉的导演台记录指向的文件交给媒体域软删（DRD-30）。返回（软删了几个，仍被引用而保留了几个）。
//
// ⚠️ 走 media.Delete（先跑全部引用检查器）而不是直接改 media_object.status：复制出来的项目还引用着同一份截图，
// 直接改状态会把它一起删掉。被引用（40906）是正常结局 —— 那份文件归副本继续用；其余错误只告警、不回滚清理：
// 项目已经没了，文件晚点回收不影响任何人，而为此让清理失败会把回收站卡住。
// 这些 purpose 是隐藏的（不进「我的素材」），用户删不到它们，不由系统软删就永远不回收。
func (s *Service) releaseDirectorMedia(ctx context.Context, list []repo.WorkspaceMedia) (released, kept int) {
	if s.directorMedia == nil || len(list) == 0 {
		return 0, 0
	}
	for _, m := range list {
		err := s.directorMedia.Delete(ctx, m.WorkspaceID, m.MediaID)
		var be *httpx.BizError
		switch {
		case err == nil:
			released++
		case errors.As(err, &be) && be.Code == errcode.ResourceInUse:
			kept++
		case errors.As(err, &be) && be.Code == errcode.NotFound:
			// 已经不在了（重复清理、或别处先删了）：没有要做的事。
		default:
			if s.log != nil {
				s.log.WarnContext(ctx, "清理后释放导演台文件失败，留待下次处理", "mediaId", m.MediaID, "error", err)
			}
		}
	}
	return released, kept
}

// ---------------------------------------------------------------- 文件夹

func (s *Service) ListFolders(ctx context.Context, ws int64, role collab.Role) ([]repo.Folder, error) {
	if err := requireRole(role, collab.ActionView); err != nil {
		return nil, err
	}
	f, err := s.repo.ListFolders(ctx, ws)
	return f, wrapRepo(err)
}

func (s *Service) CreateFolder(ctx context.Context, ws int64, role collab.Role, name string) (repo.Folder, error) {
	if err := requireRole(role, collab.ActionEdit); err != nil {
		return repo.Folder{}, err
	}
	clean, err := domain.ValidateFolderName(name)
	if err != nil {
		return repo.Folder{}, httpx.Errf(errcode.InvalidArgument, err.Error())
	}
	n, err := s.repo.CountFolders(ctx, ws)
	if err != nil {
		return repo.Folder{}, httpx.Wrap(errcode.Internal, err)
	}
	if n >= domain.MaxFolders {
		return repo.Folder{}, httpx.Errf(errcode.InvalidArgument,
			"文件夹数量已达上限 "+strconv.Itoa(domain.MaxFolders))
	}
	f, err := s.repo.InsertFolder(ctx, s.nextID(), ws, clean)
	return f, wrapRepo(err)
}

func (s *Service) RenameFolder(ctx context.Context, ws, id int64, role collab.Role, name string) (repo.Folder, error) {
	if err := requireRole(role, collab.ActionEdit); err != nil {
		return repo.Folder{}, err
	}
	clean, err := domain.ValidateFolderName(name)
	if err != nil {
		return repo.Folder{}, httpx.Errf(errcode.InvalidArgument, err.Error())
	}
	f, err := s.repo.RenameFolder(ctx, ws, id, clean)
	return f, wrapRepo(err)
}

func (s *Service) DeleteFolder(ctx context.Context, ws, id int64, role collab.Role) error {
	if err := requireRole(role, collab.ActionEdit); err != nil {
		return err
	}
	return wrapRepo(s.repo.DeleteFolder(ctx, ws, id))
}

func (s *Service) ListTemplates(ctx context.Context) ([]repo.Template, error) {
	t, err := s.repo.ListTemplates(ctx)
	return t, wrapRepo(err)
}

// ---------------------------------------------------------------- 内部

func (s *Service) checkFolder(ctx context.Context, ws, folderID int64) error {
	if folderID == 0 {
		return nil
	}
	ok, err := s.repo.FolderExists(ctx, ws, folderID)
	if err != nil {
		return httpx.Wrap(errcode.Internal, err)
	}
	if !ok {
		return httpx.Errf(errcode.NotFound, "文件夹不存在")
	}
	return nil
}

func (s *Service) logWarn(ctx context.Context, msg string, id int64, err error) {
	if s.log != nil {
		s.log.WarnContext(ctx, msg, "projectId", id, "error", err)
	}
}

func (s *Service) writeAudit(ctx context.Context, userID int64, action string, objectID int64, after any) {
	if s.audit == nil {
		return
	}
	s.audit.Write(ctx, audit.Entry{
		ActorType: audit.ActorUser, ActorID: userID,
		Action: action, ObjectType: "project", ObjectID: objectID, After: after,
	})
}

// ProjectStatus 项目在本工作区的状态（active / deleted），不读画布。给别的域判断「可写 / 可读」用
// （智能剪辑建时间线要求项目可写，分叉来源只要可读，SCT-07）。不存在与不是你的回 404。
func (s *Service) ProjectStatus(ctx context.Context, ws, id int64) (string, error) {
	p, err := s.repo.Get(ctx, ws, id)
	if err != nil {
		return "", wrapRepo(err)
	}
	return p.Status, nil
}

// SavedCanvas 已保存的画布原文（智能剪辑按它解析素材条，SCT-25）。不判角色：调用方的路由已经过了动作闸。
// 回收站里的项目也能读；不在本工作区回 404。
//
// ⚠️ 以库里的为准，不收客户端报的画布（SCK-16）：还没保存的连线不算数，前端连线后先保存再同步。
func (s *Service) SavedCanvas(ctx context.Context, ws, id int64) (json.RawMessage, error) {
	p, err := s.repo.GetWithCanvas(ctx, ws, id)
	if err != nil {
		return nil, wrapRepo(err)
	}
	return p.CanvasData, nil
}
