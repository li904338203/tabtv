// Package app 编排积分账本用例。
//
// ⚠️ 账本的正确性**全部落在 repo 的事务与数据库约束里**，本层只做：
// 解析计费主体、把领域错误翻成业务错误码、提供跨域调用的端口。
// 任何"先查再写"的并发控制都不该出现在这里（GO-11）。
package app

import (
	"context"
	"errors"
	"time"

	"log/slog"
	"github.com/li904338203/tabtv/backend/internal/billing/domain"
	"github.com/li904338203/tabtv/backend/internal/billing/repo"

	"github.com/li904338203/tabtv/backend/internal/platform/audit"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// DefaultCompensationValidFor 补偿批次有效期。
//
// ⚠️ **PARAM-02，策略未批准**：这是设计建议值（30 天），不是用户已批准的政策。
// 依据：任务失败常发生在用户不在场时，7 天可能在他下次登录前就过期，补偿等于没发。
// 由配置覆盖，**不得在别处写死**。
const DefaultCompensationValidFor = 30 * 24 * time.Hour

type Service struct {
	repo        *repo.Repo
	compenseTTL time.Duration
	audit       *audit.Logger

	// 退款与对账（P11-03）。
	refundWindow time.Duration
	refundCh     RefundChannel
	reconcileQ   ReconcileQuery
	log          *slog.Logger
}

// SetAudit 注入审计。商品改价要留痕（见 auditCatalog）。
func (s *Service) SetAudit(l *audit.Logger) { s.audit = l }

func New(r *repo.Repo) *Service {
	return &Service{repo: r, compenseTTL: DefaultCompensationValidFor}
}

func (s *Service) SetCompensationValidFor(d time.Duration) { s.compenseTTL = d }

func (s *Service) Balance(ctx context.Context, payerID int64) (int, error) {
	n, err := s.repo.Balance(ctx, payerID)
	if err != nil {
		return 0, httpx.Wrap(errcode.Internal, err)
	}
	return n, nil
}

// Consume 扣费。跨域调用（生成受理、脚本 LLM）走这个端口。
//
// ⚠️ **RefType + RefID 分两列**，不要拼成 "script:{id}:storyboard" 这类字符串：
// 旧系统那样拼既无法索引也无法按来源统计。
func (s *Service) Consume(ctx context.Context, in repo.ConsumeInput) (repo.ConsumeResult, error) {
	res, err := s.repo.Consume(ctx, in)
	switch {
	case errors.Is(err, domain.ErrInsufficient):
		return res, httpx.Errf(errcode.InsufficientCredits, err.Error())
	case errors.Is(err, repo.ErrDuplicate):
		// 幂等命中不是错误的"失败"，但调用方必须能区分 —— 它不该以为自己刚扣了一次。
		return res, httpx.Errf(errcode.Conflict, "该业务已扣过费，未重复扣减")
	case errors.Is(err, domain.ErrAmountInvalid), errors.Is(err, domain.ErrOverflow):
		return res, httpx.Errf(errcode.InvalidArgument, err.Error())
	case err != nil:
		return res, httpx.Wrap(errcode.Internal, err)
	}
	return res, nil
}

// Refund 退款。按原批次回补，过期批次改发补偿批次。
func (s *Service) Refund(ctx context.Context, payerID int64, refType, refID, reason string) (
	repo.RefundResult, error) {
	res, err := s.repo.Refund(ctx, repo.RefundInput{
		PayerID: payerID, RefType: refType, RefID: refID, Reason: reason,
		CompensationValidFor: s.compenseTTL,
	})
	switch {
	case errors.Is(err, repo.ErrDuplicate):
		return res, httpx.Errf(errcode.Conflict, "该业务已退过款，未重复退还")
	case errors.Is(err, repo.ErrNotFound):
		return res, httpx.Errf(errcode.NotFound, "找不到对应的消费记录")
	case err != nil:
		return res, httpx.Wrap(errcode.Internal, err)
	}
	return res, nil
}

// Grant 发放积分。
func (s *Service) Grant(ctx context.Context, in repo.GrantInput) (int64, error) {
	id, err := s.repo.Grant(ctx, in)
	switch {
	case errors.Is(err, repo.ErrDuplicate):
		return 0, httpx.Errf(errcode.Conflict, "该来源已发放过，未重复发放")
	case err != nil:
		return 0, httpx.Wrap(errcode.Internal, err)
	}
	return id, nil
}

func (s *Service) Transactions(ctx context.Context, payerID int64, limit int, cursorID int64) (
	[]repo.Transaction, bool, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := s.repo.ListTransactions(ctx, payerID, limit+1, cursorID)
	if err != nil {
		return nil, false, httpx.Wrap(errcode.Internal, err)
	}
	if len(rows) > limit {
		return rows[:limit], true, nil
	}
	return rows, false, nil
}

// TransactionsByRefs 按业务引用批量取流水（只读，AGT-26 的每轮对账）。
//
// 跨域调用经调用方的端口适配到这里：Agent 不读账本的表（dev/01 T-1）。
func (s *Service) TransactionsByRefs(ctx context.Context, payerID int64, refs []repo.TxRef) (
	[]repo.Transaction, error) {
	rows, err := s.repo.TransactionsByRefs(ctx, payerID, refs)
	if err != nil {
		return nil, httpx.Wrap(errcode.Internal, err)
	}
	return rows, nil
}

// Grants 列出可用批次，让用户看懂"哪些会先过期"。
func (s *Service) Grants(ctx context.Context, payerID int64) ([]repo.GrantRow, error) {
	rows, err := s.repo.ListActiveGrants(ctx, payerID)
	if err != nil {
		return nil, httpx.Wrap(errcode.Internal, err)
	}
	return rows, nil
}

// ---------------------------------------------------------------- 用量审计（P12-04）

// WorkspaceUsage 按操作者汇总一个付费主体的消耗与退回（NEW-20）。
//
// ⚠️ 口径**只能是账本**，不能按各业务表的 `credits_cost` 相加：
// 那一列是报价，任务失败退款之后它不会变 —— 一个失败又退款的任务
// 会被算成花了钱，而两个数都"没错"。
func (s *Service) WorkspaceUsage(ctx context.Context, payerID int64,
	f repo.UsageFilter) ([]repo.MemberUsage, repo.UsageSummary, error) {
	members, sum, err := s.repo.WorkspaceUsage(ctx, payerID, f)
	if err != nil {
		return nil, repo.UsageSummary{}, httpx.Wrap(errcode.Internal, err)
	}
	return members, sum, nil
}

// WorkspaceUsageDetail 列出明细，供"这个人到底花在什么上"追下去。
func (s *Service) WorkspaceUsageDetail(ctx context.Context, payerID, userID int64,
	f repo.UsageFilter, limit int, cursor int64) ([]repo.UsageEntry, error) {
	list, err := s.repo.WorkspaceUsageDetail(ctx, payerID, userID, f, limit, cursor)
	if err != nil {
		return nil, httpx.Wrap(errcode.Internal, err)
	}
	return list, nil
}
