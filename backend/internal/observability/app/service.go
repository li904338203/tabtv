// Package app 编排可观测用例：请求日志检索、统计口径、日志清理（P13-04）。
//
// ⚠️ 这一域**只读不写业务数据**。它唯一写的是 `api_request_log`（自己的表），
// 而那一次写**失败也不能影响主流程** —— 一条日志写不进去就让请求失败，
// 等于把观测设施变成单点故障。
package app

import (
	"context"
	"log/slog"
	"time"

	"github.com/li904338203/tabtv/backend/internal/observability/domain"
	"github.com/li904338203/tabtv/backend/internal/observability/repo"
	"github.com/li904338203/tabtv/backend/internal/provider/runcfg"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// ProviderLogReader 由 Provider 域实现：读它自己那张日志表。
//
// ⚠️ 端口而不是在这里直接查 `provider_request_log`：那张表归 Provider 域
// （写在那儿），跨域直接读对方的表结构是本仓库明确禁止的 ——
// 一旦它改了列，编译不会报错，而日志页会在运行时整页打不开。
//
// 两边共用 `provider/runcfg` 里的类型（只依赖标准库的叶子包），
// 所以**不需要任何转换适配器** —— 而两份转换早晚会不一致。
type ProviderLogReader interface {
	ListLogs(ctx context.Context, f runcfg.LogFilter) ([]runcfg.LogRecord, error)
	CleanupBefore(ctx context.Context, before time.Time, limit int) (int64, error)
}

type Service struct {
	repo     *repo.Repo
	provider ProviderLogReader
	log      *slog.Logger
}

func New(r *repo.Repo) *Service {
	return &Service{repo: r, log: slog.Default()}
}

func (s *Service) SetLogger(l *slog.Logger)            { s.log = l }
func (s *Service) SetProviderLogs(p ProviderLogReader) { s.provider = p }

// WriteAPILog 落一条 API 请求日志。**失败只告警**。
func (s *Service) WriteAPILog(ctx context.Context, l repo.APILog) {
	if err := s.repo.WriteAPILog(ctx, l); err != nil {
		// ⚠️ 用 Warn 不用 Error：日志写不进去是运维问题，不是用户请求失败。
		// 记成 Error 的话，一次数据库抖动会在告警里刷出一屏"错误"，
		// 而真正失败的那些请求淹没在里面。
		s.log.Warn("写 API 请求日志失败", "traceId", l.TraceID, "error", err)
	}
}

// ---------------------------------------------------------------- 检索

func (s *Service) ListAPILogs(ctx context.Context, f repo.APILogFilter) ([]repo.APILog, error) {
	list, err := s.repo.ListAPILogs(ctx, f)
	if err != nil {
		return nil, httpx.Wrap(errcode.Internal, err)
	}
	return list, nil
}

func (s *Service) ListProviderLogs(ctx context.Context, f runcfg.LogFilter) (
	[]runcfg.LogRecord, error) {
	if s.provider == nil {
		return nil, httpx.Errf(errcode.Internal, "Provider 日志未接入")
	}
	list, err := s.provider.ListLogs(ctx, f)
	if err != nil {
		return nil, httpx.Wrap(errcode.Internal, err)
	}
	return list, nil
}

// ---------------------------------------------------------------- 统计

// Stats 是看板一次取齐的内容。
type Stats struct {
	Range    domain.Range
	Totals   repo.Totals
	Provider []repo.ProviderBreakdown
	// GenerationSuccessRate 为 **-1 表示"还没有数据"**，不是 0%。
	// 调用方据此显示"—" —— 两者在看板上长得一模一样，
	// 一个是"全挂了"，一个是"今天没人用"。
	GenerationSuccessRate float64
	RenderSuccessRate     float64
}

// Overview 取看板汇总。
func (s *Service) Overview(ctx context.Context, rg domain.Range, now time.Time) (Stats, error) {
	rg = rg.Normalize(now)
	t, err := s.repo.Overview(ctx, rg)
	if err != nil {
		return Stats{}, httpx.Wrap(errcode.Internal, err)
	}
	byProvider, err := s.repo.ByProvider(ctx, rg)
	if err != nil {
		return Stats{}, httpx.Wrap(errcode.Internal, err)
	}
	return Stats{
		Range: rg, Totals: t, Provider: byProvider,
		GenerationSuccessRate: domain.SuccessRate(t.GenerationSuccess, t.GenerationFailed),
		// 渲染没有 cancelled 之外的第三种终态，口径与生成一致。
		RenderSuccessRate: domain.SuccessRate(t.RenderSuccess, t.Renders-t.RenderSuccess),
	}, nil
}

// Series 取一个指标的按天序列。
//
// ⚠️ 未知指标回**空序列而不是报错**：前端可能比后端新（配置/版本先后），
// 为一个不认识的指标把整个看板挡掉不值。
func (s *Service) Series(ctx context.Context, m domain.Metric, rg domain.Range,
	now time.Time) ([]repo.SeriesPoint, domain.Range, error) {
	rg = rg.Normalize(now)
	pts, err := s.repo.Series(ctx, m, rg)
	if err != nil {
		return nil, rg, httpx.Wrap(errcode.Internal, err)
	}
	if pts == nil {
		pts = []repo.SeriesPoint{}
	}
	return pts, rg, nil
}

// ---------------------------------------------------------------- 清理

// Cleanup 删两张日志表里过期的行，返回各删了多少。
//
// ⚠️ `api_request_log` 与 `provider_request_log` 是**唯二会无限增长的表**
// （每次请求 / 每次上游调用各一条，还带着请求体）。到 P13-04 之前
// **两张都没有任何清理接线** —— Provider 那张的 `CleanupBefore` 写好了
// 却从没被调用过，而这类"写了没接"只会在磁盘满的那天被发现。
//
// ⚠️ 单次有上限、间隔要密：一次删几十万行会长时间持锁，把正在写日志的
// 线程堵住 —— 而那时表现为**所有接口都变慢**，看起来完全不像日志的问题。
func (s *Service) Cleanup(ctx context.Context, retention time.Duration, batch int,
	now time.Time) (api, provider int64) {
	before := now.Add(-retention)
	n, err := s.repo.CleanupAPILogsBefore(ctx, before, batch)
	if err != nil {
		s.log.Warn("清理 API 请求日志失败", "error", err)
	}
	api = n
	if s.provider != nil {
		m, err := s.provider.CleanupBefore(ctx, before, batch)
		if err != nil {
			s.log.Warn("清理 Provider 请求日志失败", "error", err)
		}
		provider = m
	}
	return api, provider
}
