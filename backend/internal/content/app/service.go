// Package app 编排内容域用例：首页聚合、Banner、公告、消息中心、精选作品。
package app

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/li904338203/tabtv/backend/internal/content/domain"
	"github.com/li904338203/tabtv/backend/internal/content/repo"
	"github.com/li904338203/tabtv/backend/internal/platform/audit"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

type Service struct {
	repo   *repo.Repo
	audit  *audit.Logger
	log    *slog.Logger
	agents ProjectAgents
}

// ProjectAgent 是首页最近项目卡片上的 Agent 会话摘要（AGT-45）。
type ProjectAgent struct {
	SessionID    int64
	Title        string
	State        string
	LastActiveAt time.Time
}

// ProjectAgents 是「项目最近活跃的 Agent 会话」的端口，由画布域实现（cmd/api 里接）。
//
// ⚠️ 内容域**不自己读 agent_session**：「最近活跃的会话」这条规则在画布域的列表查询里
// 已经有一份（全部项目页 / AI 视频页用的就是它），首页再写一份的话两边早晚会点进
// 不同的会话。首页只有 8 条、不分页，事后按 ID 批量补一次不会有游标漏项的问题。
// 只返回有会话的项目。
type ProjectAgents interface {
	ProjectAgents(ctx context.Context, workspaceID int64, projectIDs []int64) (map[int64]ProjectAgent, error)
}

// SetProjectAgents 注入端口；不注入时首页项目一律按普通画布项目显示。
func (s *Service) SetProjectAgents(p ProjectAgents) { s.agents = p }

func New(r *repo.Repo) *Service {
	return &Service{repo: r, log: slog.Default()}
}

func (s *Service) SetAudit(a *audit.Logger) { s.audit = a }
func (s *Service) SetLogger(l *slog.Logger) { s.log = l }

func (s *Service) writeAudit(ctx context.Context, e audit.Entry) {
	if s.audit != nil {
		s.audit.Write(ctx, e)
	}
}

// ---------------------------------------------------------------- 首页聚合

// Home 是首页一次取齐的内容。
//
// ⚠️ **一个请求取齐**而不是让前端打三个：首屏三块（Banner / 最近项目 /
// 精选作品）总是一起出现，分成三个请求的话页面会分三次抖动，
// 而其中任意一个慢都会让首屏看起来是坏的。
type Home struct {
	Banners []repo.Banner
	Recent  []repo.RecentProject
	// RecentAgents 按项目 ID 给出最近活跃的 Agent 会话；没有会话的项目不在里面。
	RecentAgents map[int64]ProjectAgent
	Showcase     []repo.ShowcaseItem
}

// HomeFor 聚合首页。
//
// ⚠️ **任何一块失败都不让整个首页失败**：Banner 取不到就少一块，
// 精选取不到就少一块 —— 首页是用户进门的第一眼，为一块运营内容
// 给他一张错误页不值。失败只告警。
func (s *Service) HomeFor(ctx context.Context, workspaceID int64,
	pos domain.BannerPosition, now time.Time) Home {
	var h Home
	if b, err := s.repo.ActiveBanners(ctx, pos, now, 10); err != nil {
		s.log.Warn("首页 Banner 取失败，已降级", "error", err, "position", pos)
	} else {
		h.Banners = b
	}
	if workspaceID > 0 {
		if p, err := s.repo.RecentProjects(ctx, workspaceID, 8); err != nil {
			s.log.Warn("首页最近项目取失败，已降级", "error", err)
		} else {
			h.Recent = p
			h.RecentAgents = s.recentAgents(ctx, workspaceID, p)
		}
	}
	if w, err := s.repo.ListShowcase(ctx, 12, 0, domain.ShowcaseKindAll); err != nil {
		s.log.Warn("首页精选取失败，已降级", "error", err)
	} else {
		h.Showcase = w
	}
	return h
}

// recentAgents 给最近项目补上 Agent 会话。
//
// ⚠️ 失败**只降级不报错**（与首页其他块同一条）：降级的表现是 Agent 项目暂时显示成
// 普通卡片、点进去是画布 —— 会话都还在，比整个首页报错好。
func (s *Service) recentAgents(ctx context.Context, workspaceID int64,
	recent []repo.RecentProject) map[int64]ProjectAgent {
	if s.agents == nil || len(recent) == 0 {
		return nil
	}
	ids := make([]int64, 0, len(recent))
	for _, p := range recent {
		ids = append(ids, p.ID)
	}
	m, err := s.agents.ProjectAgents(ctx, workspaceID, ids)
	if err != nil {
		s.log.Warn("首页最近项目的 Agent 会话取失败，已降级", "error", err)
		return nil
	}
	return m
}

// ---------------------------------------------------------------- 精选与公开媒体

// Showcase 公开墙；kind 为空 = 全部（类型已由 transport 按白名单解析过）。
func (s *Service) Showcase(ctx context.Context, limit int, cursor int64,
	kind domain.ShowcaseKind) ([]repo.ShowcaseItem, error) {
	items, err := s.repo.ListShowcase(ctx, limit, cursor, kind)
	if err != nil {
		return nil, httpx.Wrap(errcode.Internal, err)
	}
	return items, nil
}

// PublicMediaOf 取公开作品的媒体 ID，供**不鉴权**的取图端点使用。
//
// ⚠️ 可见性判定在 SQL 里（`repo.publicWhere`），所以取消加精或下架之后
// **立刻**取不到；查不到与不可见回同一个错误（区分开等于提供一个探测接口）。
func (s *Service) PublicMediaOf(ctx context.Context, workID int64) (int64, error) {
	id, err := s.repo.PublicMediaOf(ctx, workID)
	if errors.Is(err, repo.ErrNotFound) {
		return 0, httpx.Err(errcode.NotFound)
	}
	if err != nil {
		return 0, httpx.Wrap(errcode.Internal, err)
	}
	return id, nil
}

// OwnMediaOf 取自己工作区那件作品的媒体 ID，供**需要登录**的取图端点使用。
//
// ⚠️ 作者要能看到自己刚提交、还没过审的那一件；公开墙上的人不能。
// 归属由传进来的 workspaceID 限定（调用方已经过了工作区闸）。
func (s *Service) OwnMediaOf(ctx context.Context, workID, workspaceID int64) (int64, error) {
	id, err := s.repo.OwnMediaOf(ctx, workID, workspaceID)
	if errors.Is(err, repo.ErrNotFound) {
		return 0, httpx.Err(errcode.NotFound)
	}
	if err != nil {
		return 0, httpx.Wrap(errcode.Internal, err)
	}
	return id, nil
}

// ---------------------------------------------------------------- 消息中心

func (s *Service) Messages(ctx context.Context, userID int64, limit int,
	cursorAt *time.Time, cursorID int64) ([]repo.Message, error) {
	list, err := s.repo.ListMessages(ctx, userID, limit, cursorAt, cursorID)
	if err != nil {
		return nil, httpx.Wrap(errcode.Internal, err)
	}
	return list, nil
}

func (s *Service) UnreadCount(ctx context.Context, userID int64) (int, error) {
	n, err := s.repo.UnreadCount(ctx, userID)
	if err != nil {
		return 0, httpx.Wrap(errcode.Internal, err)
	}
	return n, nil
}

// MarkRead 标记已读。source 必填（见 repo.MarkRead 的说明）。
func (s *Service) MarkRead(ctx context.Context, userID, id int64, source string) error {
	src, err := domain.ParseSource(source)
	if err != nil {
		return httpx.Errf(errcode.InvalidArgument, err.Error())
	}
	if err := s.repo.MarkRead(ctx, userID, id, src); err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			return httpx.Err(errcode.NotFound)
		}
		return httpx.Wrap(errcode.Internal, err)
	}
	return nil
}

func (s *Service) MarkAllRead(ctx context.Context, userID int64) (int, error) {
	n, err := s.repo.MarkAllRead(ctx, userID)
	if err != nil {
		return 0, httpx.Wrap(errcode.Internal, err)
	}
	return n, nil
}

// Notify 发一条个人通知。供生成域/审核链路调用（见 transport 的端口说明）。
//
// ⚠️ **吞掉自身异常**：通知绝不能牵连主流程。在 Worker 那侧抛异常就是消息
// nack → 整条生成链路重跑，为一条通知重跑一次生成不值。
func (s *Service) Notify(ctx context.Context, userID int64, n repo.Message) {
	ok, err := s.repo.InsertNotification(ctx, n, userID)
	if err != nil {
		s.log.Warn("通知写入失败（不影响主流程）", "error", err, "type", n.Type)
		return
	}
	if !ok {
		s.log.Debug("通知已存在，跳过", "type", n.Type, "refId", n.RefID)
	}
}
