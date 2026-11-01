package app

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/li904338203/tabtv/backend/internal/canvas/domain"
	"github.com/li904338203/tabtv/backend/internal/canvas/repo"
	collab "github.com/li904338203/tabtv/backend/internal/collaboration/domain"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// 画布操作的应用服务（AGT-10，开发 01 §6.2）。
//
// ⚠️ **没有对用户端开放的 HTTP 入口**：写入方只有阶段 B 的 Agent 工具、MCP 工具（MCPT-09/11，
// actor=mcp）以及开发工具 tools/canvasops。调用方负责鉴权（Agent 会话 / MCP 授权属于这个
// 工作区 / 项目），这里只保证工作区与项目状态条件。
//
// 顺序：行锁事务（幂等 → 应用 → 版本 +1 → 写日志）→ 提交 → 发事件。
// ⚠️ 事件在提交**之后**发：事务里发的话，回滚了的批次也会被前端重放，
// 而那份内容从来没有落过库。

const (
	// DefaultOpLogRetention 操作日志保留期（**设计建议值**，PARAM-05 未定稿）。
	// 补取早于保留期的区间会得到 needsReload=true —— 那时整页重载本来就更划算。
	DefaultOpLogRetention = 30 * 24 * time.Hour
	// MaxOpsSinceBatches 一次补取最多返回的批次数。超过就让前端整页重载：
	// 落后几百批的标签页逐批重放比重新拉一次画布慢得多。
	MaxOpsSinceBatches = 200
	maxBatchIDLen      = 64
)

// ApplyOpsInput 是一次应用的输入。
type ApplyOpsInput struct {
	WorkspaceID int64
	ProjectID   int64
	Actor       domain.Actor
	// BatchID 是这一批的幂等键（同一批重复提交只写一次，返回第一次的结果）。
	// Agent 侧用「会话 + 步骤」之类稳定的值，不要带时间戳。
	BatchID string
	Ops     []domain.Op
}

// ApplyOpsResult 是一次应用的结果。
type ApplyOpsResult struct {
	FromVersion int
	ToVersion   int
	// NodeIDs 是本批新建的节点（含画框）的真实 ID，按创建顺序。
	NodeIDs []string
	// Replayed 表示这一批此前已经应用过，本次没有任何写入。
	Replayed bool
}

// ApplyOps 在最新版本上应用一批画布操作。
//
// 错误：所有权不符 → CANVAS_OP_NOT_OWNED（errors.Is(err, domain.ErrNotOwned) 仍成立，
// Agent 侧据此改为新建节点）；操作非法 / 引用的节点不存在 → INVALID_ARGUMENT；
// 结果画布非法 → INVALID_CANVAS_SCHEMA；项目不存在 / 跨工作区 / 在回收站 → NOT_FOUND。
func (s *Service) ApplyOps(ctx context.Context, in ApplyOpsInput) (ApplyOpsResult, error) {
	if in.BatchID == "" || len(in.BatchID) > maxBatchIDLen {
		return ApplyOpsResult{}, httpx.Errf(errcode.InvalidArgument,
			"batchId 必填且不超过 "+strconv.Itoa(maxBatchIDLen)+" 字符")
	}
	if in.Actor.UserID <= 0 {
		// 最后编辑者要能说出「Agent（代 张三）」：没有发起人的写入无从追溯。
		return ApplyOpsResult{}, httpx.Errf(errcode.InvalidArgument, "画布操作必须带发起人")
	}

	entry, replayed, err := s.repo.ApplyOpsTx(ctx, in.WorkspaceID, in.ProjectID, in.BatchID,
		func(canvas json.RawMessage, _ int) (repo.OpWrite, error) {
			a, err := domain.Apply(canvas, in.Ops, in.Actor, s.newNodeID)
			if err != nil {
				return repo.OpWrite{}, err
			}
			ops, err := json.Marshal(a.Ops)
			if err != nil {
				return repo.OpWrite{}, err
			}
			w := repo.OpWrite{
				LogID: s.nextID(), CanvasData: a.Doc, Ops: ops,
				ActorKind: in.Actor.Kind, UserID: in.Actor.UserID,
			}
			switch in.Actor.Kind {
			case domain.ActorAgent:
				w.AgentSessionID = in.Actor.SessionID
			case domain.ActorMCP:
				w.McpGrantID = in.Actor.GrantID
			}
			return w, nil
		})
	if err != nil {
		return ApplyOpsResult{}, mapOpsError(err)
	}
	if !replayed {
		s.publishOps(ctx, in.WorkspaceID, entry, in.Actor.ClientName)
	}
	return ApplyOpsResult{
		FromVersion: entry.FromVersion, ToVersion: entry.ToVersion,
		NodeIDs: CreatedNodeIDs(entry.Ops), Replayed: replayed,
	}, nil
}

// newNodeID 是服务端分配的节点 / 连线 ID：`a_` + base36(雪花号)。
// 与前端的 `n_…` / `e_…` 不冲突；同批重放靠 batch_id 幂等返回已记录的 ID，不靠确定性派生。
func (s *Service) newNodeID() string {
	return "a_" + strconv.FormatInt(s.nextID(), 36)
}

// CreatedNodeIDs 从已解析的操作里取出新建节点（含画框）的 ID。
// 幂等重放时用它从日志里还原第一次的结果。
func CreatedNodeIDs(raw json.RawMessage) []string {
	var ops []domain.Op
	if err := json.Unmarshal(raw, &ops); err != nil {
		return nil
	}
	ids := []string{}
	for _, op := range ops {
		var obj json.RawMessage
		switch op.Kind {
		case domain.OpAddNode:
			obj = op.Node
		case domain.OpAddFrame:
			obj = op.Frame
		default:
			continue
		}
		var n struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(obj, &n) == nil && n.ID != "" {
			ids = append(ids, n.ID)
		}
	}
	return ids
}

func mapOpsError(err error) error {
	switch {
	case errors.Is(err, domain.ErrNotOwned):
		return bizWith(errcode.CanvasOpNotOwned, err)
	case errors.Is(err, domain.ErrNodeNotFound), errors.Is(err, domain.ErrOpInvalid):
		return bizWith(errcode.InvalidArgument, err)
	case errors.Is(err, domain.ErrCanvasInvalid):
		return bizWith(errcode.InvalidCanvasSchema, err)
	case errors.Is(err, repo.ErrNotFound):
		return httpx.Err(errcode.NotFound)
	case errors.Is(err, repo.ErrDuplicate):
		// 同一批次的并发提交被唯一索引兜住（正常路径在行锁之后就查到了，走不到这里）。
		return httpx.Wrap(errcode.Conflict, err)
	default:
		return httpx.Wrap(errcode.Internal, err)
	}
}

// bizWith 构造业务错误：文案用领域错误的原文（说得清第几条操作、哪个节点），
// 同时保留 cause，调用方仍可 errors.Is 到领域哨兵错误。
func bizWith(code errcode.Code, cause error) error {
	be := httpx.Wrap(code, cause)
	be.Message = cause.Error()
	return be
}

// OpsSinceResult 是补取结果。
type OpsSinceResult struct {
	CurrentVersion int
	// NeedsReload=true 时 Batches 为空：区间里有真人整份保存、批次已过保留期被清理、
	// 或落后太多 —— 前端应整页重载，而不是重放一个不完整的序列。
	NeedsReload bool
	Batches     []repo.OpLogEntry
}

// OpsSince 返回 sinceVersion 之后的操作批次，供打开着的前端补取重放（AGT-11）。
func (s *Service) OpsSince(ctx context.Context, ws, projectID int64, role collab.Role,
	since int) (OpsSinceResult, error) {
	if err := requireRole(role, collab.ActionView); err != nil {
		return OpsSinceResult{}, err
	}
	if since < 0 {
		return OpsSinceResult{}, httpx.Errf(errcode.InvalidArgument, "sinceVersion 不能为负")
	}
	current, batches, err := s.repo.OpsSince(ctx, ws, projectID, since, MaxOpsSinceBatches+1)
	if err != nil {
		return OpsSinceResult{}, wrapRepo(err)
	}
	if NeedsReload(since, current, batches) {
		return OpsSinceResult{CurrentVersion: current, NeedsReload: true, Batches: []repo.OpLogEntry{}}, nil
	}
	if batches == nil {
		batches = []repo.OpLogEntry{}
	}
	return OpsSinceResult{CurrentVersion: current, Batches: batches}, nil
}

// NeedsReload 判断 (since, current] 是否被批次**首尾相接地铺满**。
//
// ⚠️ 判据是「链条」而不是「条数」：中间夹一次真人整份保存的话那一版没有日志行，
// 链条在那里断开；批次过了保留期被清掉也一样。客户端版本比服务端还新
// （库被回退过）同样只能整页重载。
func NeedsReload(since, current int, batches []repo.OpLogEntry) bool {
	if since > current {
		return true
	}
	if len(batches) > MaxOpsSinceBatches {
		return true
	}
	expect := since
	for _, b := range batches {
		if b.FromVersion != expect || b.ToVersion != expect+1 {
			return true
		}
		expect = b.ToVersion
	}
	return expect != current
}

// CleanupOpLog 删掉超过保留期的操作日志（Worker 每小时调一次，单次 batch 行）。
func (s *Service) CleanupOpLog(ctx context.Context, retention time.Duration, batch int,
	now time.Time) (int64, error) {
	if retention <= 0 {
		retention = DefaultOpLogRetention
	}
	return s.repo.CleanupOpLog(ctx, now.Add(-retention), batch)
}
