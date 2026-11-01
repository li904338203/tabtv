package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"

	collab "github.com/li904338203/tabtv/backend/internal/collaboration/domain"

	"github.com/li904338203/tabtv/backend/internal/canvas/domain"
	"github.com/li904338203/tabtv/backend/internal/canvas/repo"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// 画布 Agent 的 API 侧服务（AGT-26）要用到的项目能力：经 wiring 适配成 agent 的端口。
// agent 域不读写 project 表（dev/01 T-1），所以这几件事由拥有它的画布域来做。

// AgentProjectKey 把客户端的 Idempotency-Key 换成项目表上的创建幂等键。
//
// ⚠️ 加前缀再哈希：① 与「直接生成」共用 `uk_project_create_idem`，同一个键分别用在两条路上
// 不能互相命中；② 列宽 varchar(64)，客户端的键最长 128。
func AgentProjectKey(clientKey string) string {
	sum := sha256.Sum256([]byte(clientKey))
	return "agent:" + hex.EncodeToString(sum[:16])
}

// EnsureAgentProject 按幂等键建一个空项目；同一个键已建过则返回那一个（replayed=true）。
//
// ⚠️ 与建会话不在同一个事务（项目归画布域、会话归 agent 域）：先建项目、再建会话，
// 后一步失败时客户端带同一个键重试会拿回同一个项目 —— 不会出现「每重试一次多一个空项目」。
//
// origin 是项目来源（MKD-3）：空 = 不写（通用会话、MCP），"marketing" = 营销会话建的项目。
// 只在**新建**时写入；幂等重放返回已有项目、不改它的来源（建后不可改）。
func (s *Service) EnsureAgentProject(ctx context.Context, ws, userID int64, role collab.Role,
	name, clientKey, origin string) (repo.Project, bool, error) {
	if err := requireRole(role, collab.ActionEdit); err != nil {
		return repo.Project{}, false, err
	}
	if origin != "" && origin != repo.OriginMarketing {
		// 调用方是服务端代码而不是用户输入：传错是编程错误，但仍给 400 而不是写一个 CHECK 会拒的值。
		return repo.Project{}, false, httpx.Errf(errcode.InvalidArgument, "origin 非法")
	}
	key := AgentProjectKey(clientKey)
	if p, err := s.repo.FindByCreateIdemKey(ctx, ws, key); err == nil {
		return p, true, nil
	} else if !errors.Is(err, repo.ErrNotFound) {
		return repo.Project{}, false, httpx.Wrap(errcode.Internal, err)
	}
	n := strings.TrimSpace(name)
	if n == "" {
		n = "AI 创作"
	}
	n = domain.QuickProjectName(n)
	if _, err := domain.ValidateProjectName(n); err != nil {
		n = "AI 创作"
	}
	p := repo.Project{ID: s.nextID(), WorkspaceID: ws, CreatedBy: userID, Name: n,
		CanvasData: json.RawMessage(`{}`), Origin: origin}
	if err := s.repo.InsertWithIdemKey(ctx, p, key); err != nil {
		if errors.Is(err, repo.ErrDuplicate) {
			existing, e2 := s.repo.FindByCreateIdemKey(ctx, ws, key)
			if e2 != nil {
				return repo.Project{}, false, httpx.Errf(errcode.Conflict, "并发创建冲突，请重试")
			}
			return existing, true, nil
		}
		return repo.Project{}, false, httpx.Wrap(errcode.Internal, err)
	}
	detail := map[string]any{"name": n}
	if origin != "" {
		detail["origin"] = origin
	}
	s.writeAudit(ctx, userID, "project.agent_create", p.ID, detail)
	return p, false, nil
}

// AgentProjectNode 是画布里一个节点的摘要（引用解析用）。
type AgentProjectNode struct {
	ID    string
	Type  string
	Title string
}

// AgentProject 校验项目属于本工作区且未删除，并返回画布节点摘要（引用校验用）。
//
// 回收站里的项目返回 409/40905：会话会往画布上写东西，不能在一个被删的项目上开工。
func (s *Service) AgentProject(ctx context.Context, ws, projectID int64) (map[string]AgentProjectNode, error) {
	p, err := s.repo.GetWithCanvas(ctx, ws, projectID)
	if err != nil {
		return nil, wrapRepo(err)
	}
	if p.Status != "active" {
		return nil, httpx.Errf(errcode.InvalidStateTransition, "项目在回收站中，请先恢复")
	}
	var doc struct {
		Nodes []struct {
			ID    string `json:"id"`
			Type  string `json:"type"`
			Title string `json:"title"`
		} `json:"nodes"`
	}
	out := map[string]AgentProjectNode{}
	if len(p.CanvasData) > 0 {
		// 画布结构坏了不挡建会话：只是引用不到节点而已。
		_ = json.Unmarshal(p.CanvasData, &doc)
	}
	for _, n := range doc.Nodes {
		if n.ID != "" {
			out[n.ID] = AgentProjectNode{ID: n.ID, Type: n.Type, Title: n.Title}
		}
	}
	return out, nil
}
