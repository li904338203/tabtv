package app

import (
	"context"
	"time"

	"github.com/li904338203/tabtv/backend/internal/generation/domain"
	"github.com/li904338203/tabtv/backend/internal/generation/repo"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// 后台运维补偿（P13-03；GAP-07）。
//
// ⚠️ 补偿**不是取消**。取消的语义是"用户决定不要了"，补偿是"平台判定这条
// 任务已经死了"。混用的话：成功率指标里会多出一批不存在的用户取消，
// 而用户在自己的列表里看到一条他从没取消过的"已取消"。
// 所以这里走 `failed` + 专用错误码，用户看到的是"失败（平台已退款）"。
//
// ⚠️ 补偿**绝不绕过账本**：退款走与正常失败完全相同的 `Refunder`，
// 按原消费批次回补、由账本的唯一索引保证只成功一次。后台不存在
// "直接把积分加回去"的路径 —— 那会让逐批明细与流水对不上
// （同 CD-56 里人工调账为什么分加/减两条路）。

// OpsTaskFacts 是运维补偿判定所需的最小事实。
type OpsTaskFacts struct {
	Finished   bool
	DeadlineAt *time.Time
	// Credits 是这次任务扣掉的积分；0 表示没有可退的。
	Credits int
}

// DescribeForOps 取一条任务的补偿判定事实。**不带工作区条件**。
func (s *Service) DescribeForOps(ctx context.Context, id int64) (OpsTaskFacts, error) {
	g, err := s.repo.GetForOps(ctx, id)
	if err != nil {
		return OpsTaskFacts{}, httpx.Err(errcode.NotFound)
	}
	return OpsTaskFacts{
		Finished:   domain.IsTerminal(g.Status),
		DeadlineAt: g.DeadlineAt,
		Credits:    g.CreditsCost,
	}, nil
}

// CompensateFail 由运维把一条卡住的任务判失败并退款。
//
// 返回「本次是否真的改了状态」。⚠️ 调用方必须看这个返回值：
// 两个运维同时点、或者任务刚好在这一刻跑完了，`Finish` 的 CAS 会拦下来 ——
// 不看的话就会给一条已经成功的任务退款，而用户既拿到了产物又拿回了钱。
func (s *Service) CompensateFail(ctx context.Context, id int64, reason string) (bool, error) {
	g, err := s.repo.GetForOps(ctx, id)
	if err != nil {
		return false, httpx.Err(errcode.NotFound)
	}
	msg := "运维补偿：" + reason
	changed, err := s.repo.Finish(ctx, repo.FinishInput{
		ID: id, Status: domain.StatusFailed,
		// ⚠️ 专用错误码：统计与排障都要能把"平台补偿"与真实的上游失败分开。
		ErrorCode:    domain.ErrorCodeOpsCompensated,
		ErrorMessage: msg,
	})
	if err != nil {
		return false, httpx.Wrap(errcode.Internal, err)
	}
	if !changed {
		return false, nil
	}
	// 组子项与整图运行都要跟着推进 —— 漏掉的话子项永远停在 queued、
	// 整图运行永远停在 running，而那条任务在运维台里已经是 failed 了。
	if s.groupBinder != nil && g.GroupItemID != 0 {
		if err := s.groupBinder.BindGroupItem(ctx, id, domain.StatusFailed, reason); err != nil {
			s.logWarn(ctx, "补偿后回写组子项失败，子项可能停在 queued", id, err)
		}
	}
	if s.runAdvancer != nil {
		if err := s.runAdvancer.AdvanceByGeneration(ctx, id,
			domain.StatusFailed, domain.ErrorCodeOpsCompensated, reason); err != nil {
			s.logWarn(ctx, "补偿后推进整图运行失败，运行可能停在 running", id, err)
		}
	}
	bindSmartcut(ctx, s.smartcut, s.log, g, domain.StatusFailed, nil, msg)
	if s.refunder != nil && g.CreditsCost > 0 {
		if err := s.refunder.Refund(ctx, g.PayerAccountID, "generation", itoa(id),
			msg); err != nil {
			// ⚠️ 退款失败只告警**不回滚判失败**：任务已经不跑了，把它拉回
			// running 更糟（它本来就是卡住的）。退款的一次性由账本唯一索引
			// 兜底，失败的由对账补 —— 而运维台上那条记录留着，看得见。
			s.logWarn(ctx, "补偿退款失败，需对账补偿", id, err)
		}
	}
	// 用户的画布可能还开着、还在等这条任务：推一条终态事件，让它当场落定而不是一直转圈。
	publishUpdate(ctx, s.events, s.log, g, domain.StatusFailed, nil, domain.ErrorCodeOpsCompensated, msg)
	return true, nil
}

// RetryForOps 由运维代发起一次重试。
//
// ⚠️ **走与用户完全相同的 `Retry`** —— 重新校验、重新报价、**重新扣费**。
// 后台没有"免费重试"这条路：重试是一次真实的上游调用，平台替用户吃掉
// 成本是一个**商业决定**，不能由一个运维按钮隐式做出（GO-04 同一条）。
// 上一次失败的钱已经退回去了，所以这次扣费在账面上是自洽的。
//
// ⚠️ 归属三要素取**原任务的**（工作区 / 付费主体 / 发起人），不是运维自己的：
// 记成运维的话，这笔消费会从用户的账单里消失，而用量审计里会多出一个
// 从来不生成内容的"用户"。
func (s *Service) RetryForOps(ctx context.Context, id int64, idempotencyKey string) (
	AcceptResult, error) {
	g, err := s.repo.GetForOps(ctx, id)
	if err != nil {
		return AcceptResult{}, httpx.Err(errcode.NotFound)
	}
	return s.Retry(ctx, g.WorkspaceID, g.PayerAccountID, g.CreatedByUserID, id, idempotencyKey)
}
