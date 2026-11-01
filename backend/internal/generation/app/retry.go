package app

import (
	"context"
	"encoding/json"

	"github.com/li904338203/tabtv/backend/internal/generation/domain"
	"github.com/li904338203/tabtv/backend/internal/generation/repo"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// Retry 重试一个失败的任务。
//
// ⚠️ **重试 = 新建一行**（填 `retry_of_id`），不是把原记录改回 queued（GO-17）。
// 改回去的话：原任务的失败记录消失了（用户看不到「它失败过」），
// 那次已经退掉的款与新的扣费对不上，而且 `attempt` 的语义也乱了。
//
// ⚠️ **重新校验、重新报价、重新扣费**：价格、能力、余额都可能在这期间变了。
// 直接复用旧快照会让用户按一周前的价格享受一次生成。
func (s *Service) Retry(ctx context.Context, ws, payerID, userID, id int64,
	idempotencyKey string) (AcceptResult, error) {
	orig, err := s.repo.Get(ctx, ws, id)
	if err != nil {
		return AcceptResult{}, httpx.Err(errcode.NotFound)
	}
	// 只允许对失败的任务重试：成功的重试等于再花一次钱却没有新的诉求，
	// 进行中的重试会产生两个并行任务。
	if orig.Status != domain.StatusFailed {
		return AcceptResult{}, httpx.Errf(errcode.InvalidStateTransition,
			"只有失败的任务可以重试，当前状态："+statusLabel(orig.Status))
	}

	snap := parseSnapshot(orig.ParamsSnapshot)
	return s.Accept(ctx, AcceptInput{
		WorkspaceID: ws, PayerAccountID: payerID, UserID: userID,
		ProjectID: orig.ProjectID, NodeID: orig.NodeID,
		OperationCode: snap.OperationCode, ModelCode: snap.ModelCode,
		IdempotencyKey: idempotencyKey,
		Count:          orig.RequestCount,
		// 参数取原任务的用户参数部分；快照会在受理时按**当前**配置重新生成。
		Params: userParams(orig.ParamsSnapshot),
		// ⚠️ 输入要**还原成声明形态**，不能把解析后的快照原样发回去：
		// 快照里的 ID 是 int64，而受理入口认的是 string（ID 对外一律
		// string，防 JS 精度丢失）—— 直接传的话第一个数字 ID 就让
		// `json.Unmarshal` 失败，整条重试报「inputs 不是合法的输入数组」，
		// 而那句话指向"客户端传错了参数"，可这次请求里根本没有客户端传的
		// inputs。这个缺陷对**任何带输入的任务**都成立，只是纯文生任务
		// 没有输入、恰好绕过了它（P13-03 的后台受控重试第一次踩到）。
		DeclaredInputs: domain.RedeclareInputs(orig.InputsSnapshot),
		RetryOfID:      orig.ID,
		// ⚠️ 来源照抄（MKT-T-35）：营销「生成角色」不属于任何项目，丢了来源的话重试会以
		// project_id = 0 走常规解析而被拒（404），或成功后被当成作品登记进审核队列。
		Origin: snap.Origin,
		// 智能剪辑名下的任务：回写目标照抄（重试成功也要把识别结果写回缓存、把产物放回时间线）。
		SmartcutTarget: orig.SmartcutTarget,
	})
}

func parseSnapshot(params json.RawMessage) domain.Snapshot {
	var tree struct {
		Snapshot domain.Snapshot `json:"_snapshot"`
	}
	_ = json.Unmarshal(params, &tree)
	return tree.Snapshot
}

// UserParams 剥掉 `_snapshot`，只留用户参数（智能剪辑的片段重拍以原任务为模板时用，同 Retry）。
func UserParams(params json.RawMessage) json.RawMessage { return userParams(params) }

// userParams 剥掉 `_snapshot`，只留用户参数。
func userParams(params json.RawMessage) json.RawMessage {
	var tree map[string]json.RawMessage
	if err := json.Unmarshal(params, &tree); err != nil {
		return nil
	}
	delete(tree, "_snapshot")
	out, err := json.Marshal(tree)
	if err != nil {
		return nil
	}
	return out
}

// 编译期保证 repo 引用不被优化掉（Retry 用到了 repo.Generation 的字段）。
var _ = repo.Generation{}
