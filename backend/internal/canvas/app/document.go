package app

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/li904338203/tabtv/backend/internal/canvas/domain"
	"github.com/li904338203/tabtv/backend/internal/canvas/repo"
	collab "github.com/li904338203/tabtv/backend/internal/collaboration/domain"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// ---------------------------------------------------------------- 项目文档（AGT-09）
//
// 文档归 canvas 域：它从属于 project，而 project 归这里。阶段 B 的 agent 经端口
// （dev 01 §3 的 Documents）调用下面这几个方法，不直接查表。
//
// 角色：读要 view、写要 edit。路由层的 GuardByMethod 已经按 HTTP 方法挡了一道，
// 这里再判一次是因为 Agent 端口不经过路由 —— 它带着会话发起人的角色进来。

// DocumentActor 是写文档的人：真人（UserID）、Agent（AgentSessionID 非 0）或外部 MCP 客户端
// （McpGrantID 非 0，MCPT-11）。后两者 UserID 都是「代谁写的」那个人。
//
// McpGrantID 只决定来源记 mcp（00053）：哪个授权写的不落在文档表上（canvas 域不读 mcp_* 表）。
type DocumentActor struct {
	UserID         int64
	AgentSessionID int64
	McpGrantID     int64
}

func (a DocumentActor) source() string {
	switch {
	case a.AgentSessionID != 0:
		return "agent"
	case a.McpGrantID != 0:
		return "mcp"
	}
	return "user"
}

// CreateDocumentInput 新建文档的输入。
type CreateDocumentInput struct {
	Path    string
	Content string
}

func (s *Service) ListDocuments(ctx context.Context, ws, projectID int64, role collab.Role) (
	[]repo.DocumentSummary, error) {
	if err := requireRole(role, collab.ActionView); err != nil {
		return nil, err
	}
	if err := s.projectReadable(ctx, ws, projectID); err != nil {
		return nil, err
	}
	list, err := s.repo.ListDocuments(ctx, ws, projectID)
	return list, wrapRepo(err)
}

func (s *Service) GetDocument(ctx context.Context, ws, projectID, docID int64, role collab.Role) (
	repo.Document, error) {
	if err := requireRole(role, collab.ActionView); err != nil {
		return repo.Document{}, err
	}
	if err := s.projectReadable(ctx, ws, projectID); err != nil {
		return repo.Document{}, err
	}
	d, err := s.repo.GetDocument(ctx, ws, projectID, docID)
	return d, wrapDocRepo(err)
}

// DocumentByPath 按路径取（Agent 读 /plan.md、/.memory.md 用）。
func (s *Service) DocumentByPath(ctx context.Context, ws, projectID int64, role collab.Role,
	path string) (repo.Document, error) {
	if err := requireRole(role, collab.ActionView); err != nil {
		return repo.Document{}, err
	}
	clean, err := domain.ValidateDocPath(path)
	if err != nil {
		return repo.Document{}, httpx.Errf(errcode.InvalidArgument, err.Error())
	}
	if err := s.projectReadable(ctx, ws, projectID); err != nil {
		return repo.Document{}, err
	}
	d, err := s.repo.GetDocumentByPath(ctx, ws, projectID, clean)
	return d, wrapDocRepo(err)
}

func (s *Service) CreateDocument(ctx context.Context, ws, projectID int64, actor DocumentActor,
	role collab.Role, in CreateDocumentInput) (repo.Document, error) {
	if err := requireRole(role, collab.ActionEdit); err != nil {
		return repo.Document{}, err
	}
	path, err := domain.ValidateDocPath(in.Path)
	if err != nil {
		return repo.Document{}, httpx.Errf(errcode.InvalidArgument, err.Error())
	}
	if err := docContentErr(in.Content); err != nil {
		return repo.Document{}, err
	}
	if err := s.projectWritable(ctx, ws, projectID); err != nil {
		return repo.Document{}, err
	}
	d, err := s.repo.CreateDocument(ctx, repo.Document{
		ID: s.nextID(), WorkspaceID: ws, ProjectID: projectID, Path: path, Content: in.Content,
		Source: actor.source(), EditorID: actor.UserID, AgentSessionID: actor.AgentSessionID,
	})
	if errors.Is(err, repo.ErrDuplicate) {
		return repo.Document{}, httpx.Errf(errcode.DuplicateName, "该项目里已有同路径的文档: "+path)
	}
	return d, wrapDocRepo(err)
}

// UpdateDocument 乐观锁保存正文。
//
// ⚠️ 冲突返回 40902 并带上服务端当前版本与"是谁改的"（同画布保存，NEW-19）：
// 文档节点上的编辑与 Agent 的写入是同一份正文，静默覆盖的话 Agent 刚写好的规划
// 会被用户一次「保存」抹掉，而他根本不知道中间被改过。
func (s *Service) UpdateDocument(ctx context.Context, ws, projectID, docID int64, actor DocumentActor,
	role collab.Role, version int, content string) (repo.Document, error) {
	if err := requireRole(role, collab.ActionEdit); err != nil {
		return repo.Document{}, err
	}
	if version < 1 {
		// 不接受 0：那是"我没读过服务端版本"的意思，等于要求无条件覆盖。
		return repo.Document{}, httpx.Errf(errcode.InvalidArgument, "version 必须来自服务端，不能为 0")
	}
	if err := docContentErr(content); err != nil {
		return repo.Document{}, err
	}
	if err := s.projectWritable(ctx, ws, projectID); err != nil {
		return repo.Document{}, err
	}
	d, err := s.repo.UpdateDocument(ctx, ws, projectID, docID, version, content,
		actor.UserID, actor.AgentSessionID, actor.source())
	if errors.Is(err, repo.ErrVersionConflict) {
		// ⚠️ MCP 的写入也带着用户本人的 ID：只比 EditorID 的话会把「客户端刚写的」说成「你在另一个窗口改的」，
		// 用户点「以我为准」就覆盖掉它（同画布 MCPT-09）。机器发起的保存本身也谈不上「本人另一个窗口」。
		bySelf := d.AgentSessionID == 0 && d.Source != "mcp" && actor.source() == "user" &&
			d.EditorID != 0 && d.EditorID == actor.UserID
		return repo.Document{}, httpx.Errf(errcode.VersionConflict, docConflictMessage(d, bySelf)).
			WithDetails(map[string]any{
				"currentVersion":    d.Version,
				"currentUpdatedAt":  d.UpdatedAt.UTC().Format(time.RFC3339),
				"currentEditorId":   strconv.FormatInt(d.EditorID, 10),
				"currentEditorName": d.EditorName,
				"currentSource":     d.Source,
				"bySelf":            bySelf,
			})
	}
	return d, wrapDocRepo(err)
}

func docConflictMessage(d repo.Document, bySelf bool) string {
	switch {
	case bySelf:
		return "你在另一个窗口修改过这份文档，请载入最新或选择以当前内容为准"
	case d.AgentSessionID != 0:
		return "Agent 刚修改了这份文档，请载入最新后再保存"
	case d.Source == "mcp":
		if d.EditorName != "" {
			return "外部 AI 客户端（代 " + d.EditorName + "）刚修改了这份文档，请载入最新后再保存"
		}
		return "外部 AI 客户端刚修改了这份文档，请载入最新后再保存"
	case d.EditorName != "":
		return d.EditorName + " 修改了这份文档，请载入最新后再保存"
	default:
		return "文档已被修改，请载入最新后再保存"
	}
}

func docContentErr(content string) error {
	if err := domain.ValidateDocContent(content); err != nil {
		if errors.Is(err, domain.ErrDocTooLarge) {
			return httpx.Errf(errcode.PayloadTooLarge, err.Error())
		}
		return httpx.Errf(errcode.InvalidArgument, err.Error())
	}
	return nil
}

// projectReadable 项目属于本工作区即可读（回收站里的也能读：看一眼不会造成任何后果）。
func (s *Service) projectReadable(ctx context.Context, ws, projectID int64) error {
	_, err := s.repo.Get(ctx, ws, projectID)
	return wrapRepo(err)
}

// projectWritable 写要求项目在本工作区且未删除。
//
// ⚠️ 回收站里的项目报 40905 而不是 404：项目明明还在（用户能在回收站里看到它），
// 说"不存在"会让他以为数据丢了。
func (s *Service) projectWritable(ctx context.Context, ws, projectID int64) error {
	p, err := s.repo.Get(ctx, ws, projectID)
	if err != nil {
		return wrapRepo(err)
	}
	if p.Status != "active" {
		return httpx.Errf(errcode.InvalidStateTransition, "回收站中的项目不能修改文档，请先恢复")
	}
	return nil
}

func wrapDocRepo(err error) error {
	if errors.Is(err, repo.ErrNotFound) {
		return httpx.Errf(errcode.NotFound, "文档不存在")
	}
	return wrapRepo(err)
}
