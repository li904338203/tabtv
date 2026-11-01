package app

import (
	"context"
	"time"

	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// BuildConfig 是 `GET /director/build-config` 的结果：前端据此置灰并说明（DR-F-108）。
type BuildConfig struct {
	Available bool
	// Reason / UserFixable 只在不可用时有值。
	Reason       string
	UserFixable  bool
	MaxImages    int
	MaxPrompt    int
	Concurrency  int
	PerHour      int
	UsedThisHour int
}

// BuildConfig 回答「现在能不能 AI 搭景、能带几张参考图、本小时用了几次」。**只读，不插调用记录**。
//
// ⚠️ 可用性与 Build 的第 4 步是**同一个函数**（Catalog.SceneBuildChoice）：各写一份的话，
// 会出现「面板说能用、点下去 42203」或反过来。限流的计数只用于展示，是否放行仍以 Build 那一刻为准。
func (s *Service) BuildConfig(ctx context.Context, userID, memberLevelID int64) (BuildConfig, error) {
	if s.p.Catalog == nil || s.p.Calls == nil {
		return BuildConfig{}, httpx.Errf(errcode.Internal, "AI 搭景链路未接入")
	}
	out := BuildConfig{MaxPrompt: MaxPromptChars, Concurrency: s.cfg.Concurrency, PerHour: s.cfg.PerHour}
	choice, unavailable, err := s.p.Catalog.SceneBuildChoice(ctx, memberLevelID, 0)
	if err != nil {
		return BuildConfig{}, httpx.Wrap(errcode.Internal, err)
	}
	if unavailable != nil {
		out.Reason, out.UserFixable = unavailable.Reason, unavailable.UserFixable
	} else {
		out.Available, out.MaxImages = true, choice.MaxImages
	}
	_, used, err := s.p.Calls.Usage(ctx, userID)
	if err != nil {
		return BuildConfig{}, httpx.Wrap(errcode.Internal, err)
	}
	out.UsedThisHour = used
	return out, nil
}

// RecoverStaleCalls 把「进行中」超过 StaleAfter 的调用判失败，释放它们占着的并发额度（API 的恢复扫描调用）。
// 免费，所以没有退款分支。多个 API 实例同时扫也安全（判失败是 CAS）。
func (s *Service) RecoverStaleCalls(ctx context.Context) (int, error) {
	if s.p.Calls == nil {
		return 0, nil
	}
	total := 0
	for {
		n, err := s.p.Calls.RecoverStale(ctx, s.cfg.StaleAfter, 100)
		total += n
		if err != nil || n < 100 {
			return total, err
		}
	}
}

// CleanupCalls 分批删除超过保留期的调用记录（DR-PARAM-16），返回删了几条。
func (s *Service) CleanupCalls(ctx context.Context) (int, error) {
	if s.p.Calls == nil {
		return 0, nil
	}
	before := s.now().Add(-s.cfg.Retention)
	total := 0
	for {
		n, err := s.p.Calls.DeleteOlderThan(ctx, before, 1000)
		total += n
		if err != nil || n < 1000 {
			return total, err
		}
		// 批间让出：连续大批删除会把锁一直占着，堵住正在写调用记录的搭景请求。
		select {
		case <-ctx.Done():
			return total, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}
