package app

import (
	"context"
	"errors"
	"time"

	"github.com/li904338203/tabtv/backend/internal/content/domain"
	"github.com/li904338203/tabtv/backend/internal/content/repo"
	"github.com/li904338203/tabtv/backend/internal/platform/audit"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// ---------------------------------------------------------------- Banner 运营

// BannerInput 是一次 Banner 新建/编辑。
type BannerInput struct {
	ID            int64
	Position      string
	Title         string
	MediaObjectID int64
	CoverMediaID  int64
	LinkURL       string
	Sort          int
	IsEnabled     bool
	StartAt       *time.Time
	EndAt         *time.Time
}

func (s *Service) validateBanner(in BannerInput) (repo.Banner, error) {
	pos := domain.BannerPosition(in.Position)
	if !pos.Valid() {
		return repo.Banner{}, httpx.Errf(errcode.InvalidArgument, domain.ErrBadPosition.Error())
	}
	if err := domain.ValidateBannerTitle(in.Title); err != nil {
		return repo.Banner{}, httpx.Errf(errcode.InvalidArgument, err.Error())
	}
	if err := domain.ValidateLink(in.LinkURL); err != nil {
		return repo.Banner{}, httpx.Errf(errcode.InvalidArgument, err.Error())
	}
	sched := domain.BannerSchedule{StartAt: in.StartAt, EndAt: in.EndAt}
	if err := domain.ValidateSchedule(sched); err != nil {
		// ⚠️ 倒置或零长度的窗口要**在保存时**拒掉：存进去的话它永远投不出去，
		// 而运营在列表里看到的是一条"已启用"的 Banner，找不出哪里不对。
		return repo.Banner{}, httpx.Errf(errcode.InvalidArgument, err.Error())
	}
	return repo.Banner{
		ID: in.ID, Position: pos, Title: in.Title,
		MediaObjectID: in.MediaObjectID, CoverMediaID: in.CoverMediaID,
		LinkURL: in.LinkURL, Sort: in.Sort, IsEnabled: in.IsEnabled,
		StartAt: in.StartAt, EndAt: in.EndAt,
	}, nil
}

func (s *Service) ListBanners(ctx context.Context, pos string) ([]repo.Banner, error) {
	list, err := s.repo.ListBanners(ctx, pos)
	if err != nil {
		return nil, httpx.Wrap(errcode.Internal, err)
	}
	return list, nil
}

func (s *Service) CreateBanner(ctx context.Context, adminID int64, in BannerInput) (
	repo.Banner, error) {
	b, err := s.validateBanner(in)
	if err != nil {
		return repo.Banner{}, err
	}
	out, err := s.repo.InsertBanner(ctx, b)
	if err != nil {
		return repo.Banner{}, httpx.Wrap(errcode.Internal, err)
	}
	s.writeAudit(ctx, audit.Entry{
		ActorType: audit.ActorAdmin, ActorID: adminID,
		Action: "banner.create", ObjectType: "banner", ObjectID: out.ID,
		After: map[string]any{"position": in.Position, "title": in.Title,
			"enabled": in.IsEnabled},
	})
	return out, nil
}

func (s *Service) UpdateBanner(ctx context.Context, adminID int64, in BannerInput) (
	repo.Banner, error) {
	b, err := s.validateBanner(in)
	if err != nil {
		return repo.Banner{}, err
	}
	before, err := s.repo.GetBanner(ctx, in.ID)
	if errors.Is(err, repo.ErrNotFound) {
		return repo.Banner{}, httpx.Err(errcode.NotFound)
	}
	out, err := s.repo.UpdateBanner(ctx, b)
	if errors.Is(err, repo.ErrNotFound) {
		return repo.Banner{}, httpx.Err(errcode.NotFound)
	}
	if err != nil {
		return repo.Banner{}, httpx.Wrap(errcode.Internal, err)
	}
	s.writeAudit(ctx, audit.Entry{
		ActorType: audit.ActorAdmin, ActorID: adminID,
		Action: "banner.update", ObjectType: "banner", ObjectID: out.ID,
		Before: map[string]any{"title": before.Title, "enabled": before.IsEnabled},
		After:  map[string]any{"title": out.Title, "enabled": out.IsEnabled},
	})
	return out, nil
}

func (s *Service) DeleteBanner(ctx context.Context, adminID, id int64) error {
	if err := s.repo.DeleteBanner(ctx, id); err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			return httpx.Err(errcode.NotFound)
		}
		return httpx.Wrap(errcode.Internal, err)
	}
	s.writeAudit(ctx, audit.Entry{
		ActorType: audit.ActorAdmin, ActorID: adminID,
		Action: "banner.delete", ObjectType: "banner", ObjectID: id,
	})
	return nil
}

// ---------------------------------------------------------------- 公告运营

type AnnouncementInput struct {
	ID      int64
	Title   string
	Content string
	LinkURL string
}

func (s *Service) validateAnnouncement(in AnnouncementInput) error {
	if err := domain.ValidateBannerTitle(in.Title); err != nil {
		return httpx.Errf(errcode.InvalidArgument, err.Error())
	}
	if err := domain.ValidateAnnouncementContent(in.Content); err != nil {
		return httpx.Errf(errcode.InvalidArgument, err.Error())
	}
	if err := domain.ValidateLink(in.LinkURL); err != nil {
		return httpx.Errf(errcode.InvalidArgument, err.Error())
	}
	return nil
}

func (s *Service) ListAnnouncements(ctx context.Context, status string) (
	[]repo.Announcement, error) {
	if status != "" && !domain.AnnouncementStatus(status).Valid() {
		return nil, httpx.Errf(errcode.InvalidArgument, domain.ErrBadStatus.Error())
	}
	list, err := s.repo.ListAnnouncements(ctx, status)
	if err != nil {
		return nil, httpx.Wrap(errcode.Internal, err)
	}
	return list, nil
}

// CreateAnnouncement 新建。⚠️ **一律落草稿**：一次误保存不该等于给全站误发。
func (s *Service) CreateAnnouncement(ctx context.Context, adminID int64,
	in AnnouncementInput) (repo.Announcement, error) {
	if err := s.validateAnnouncement(in); err != nil {
		return repo.Announcement{}, err
	}
	out, err := s.repo.InsertAnnouncement(ctx, repo.Announcement{
		Title: in.Title, Content: in.Content, LinkURL: in.LinkURL, CreatedBy: adminID,
	})
	if err != nil {
		return repo.Announcement{}, httpx.Wrap(errcode.Internal, err)
	}
	s.writeAudit(ctx, audit.Entry{
		ActorType: audit.ActorAdmin, ActorID: adminID,
		Action: "announcement.create", ObjectType: "announcement", ObjectID: out.ID,
		After: map[string]any{"title": in.Title},
	})
	return out, nil
}

// UpdateAnnouncement 改内容。⚠️ **不碰 status**：发布是单独一次显式操作。
func (s *Service) UpdateAnnouncement(ctx context.Context, adminID int64,
	in AnnouncementInput) (repo.Announcement, error) {
	if err := s.validateAnnouncement(in); err != nil {
		return repo.Announcement{}, err
	}
	out, err := s.repo.UpdateAnnouncement(ctx, repo.Announcement{
		ID: in.ID, Title: in.Title, Content: in.Content, LinkURL: in.LinkURL,
	})
	if errors.Is(err, repo.ErrNotFound) {
		return repo.Announcement{}, httpx.Err(errcode.NotFound)
	}
	if err != nil {
		return repo.Announcement{}, httpx.Wrap(errcode.Internal, err)
	}
	s.writeAudit(ctx, audit.Entry{
		ActorType: audit.ActorAdmin, ActorID: adminID,
		Action: "announcement.update", ObjectType: "announcement", ObjectID: out.ID,
		After: map[string]any{"title": in.Title},
	})
	return out, nil
}

// PublishAnnouncement 发布 —— **单独一条路径**。
//
// ⚠️ 混进 update 的话，运营改个错字保存一下就等于给全站重新发了一次公告，
// 而这个动作**不可撤销**（已经推给所有人了）。
func (s *Service) PublishAnnouncement(ctx context.Context, adminID, id int64) (
	repo.Announcement, error) {
	return s.transition(ctx, adminID, id, domain.StatusDraft, domain.StatusPublished,
		true, "announcement.publish")
}

// OfflineAnnouncement 下线。下线之后它**立刻**从所有人的消息中心消失
// （状态写在查询条件里，不需要去删每个人的那一行 —— 本来也没有那一行）。
func (s *Service) OfflineAnnouncement(ctx context.Context, adminID, id int64) (
	repo.Announcement, error) {
	return s.transition(ctx, adminID, id, domain.StatusPublished, domain.StatusOffline,
		false, "announcement.offline")
}

func (s *Service) transition(ctx context.Context, adminID, id int64,
	from, to domain.AnnouncementStatus, publish bool, action string) (
	repo.Announcement, error) {
	cur, err := s.repo.GetAnnouncement(ctx, id)
	if errors.Is(err, repo.ErrNotFound) {
		return repo.Announcement{}, httpx.Err(errcode.NotFound)
	}
	if err != nil {
		return repo.Announcement{}, httpx.Wrap(errcode.Internal, err)
	}
	// 先用 domain 判一次，给出说得清的文案；CAS 再兜一次并发。
	var check error
	if publish {
		check = domain.CanPublish(cur.Status)
	} else {
		check = domain.CanOffline(cur.Status)
	}
	if check != nil {
		return repo.Announcement{}, httpx.Errf(errcode.InvalidStateTransition, check.Error())
	}
	out, err := s.repo.SetAnnouncementStatus(ctx, id, from, to, publish)
	if errors.Is(err, repo.ErrNotFound) {
		// CAS 没命中 = 期间被别人改过。
		return repo.Announcement{}, httpx.Errf(errcode.Conflict,
			"公告状态已被其他操作改变，请刷新后重试")
	}
	if err != nil {
		return repo.Announcement{}, httpx.Wrap(errcode.Internal, err)
	}
	s.writeAudit(ctx, audit.Entry{
		ActorType: audit.ActorAdmin, ActorID: adminID,
		Action: action, ObjectType: "announcement", ObjectID: id,
		Before: map[string]any{"status": string(cur.Status)},
		After:  map[string]any{"status": string(to)},
	})
	return out, nil
}

func (s *Service) DeleteAnnouncement(ctx context.Context, adminID, id int64) error {
	if err := s.repo.DeleteAnnouncement(ctx, id); err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			return httpx.Err(errcode.NotFound)
		}
		return httpx.Wrap(errcode.Internal, err)
	}
	s.writeAudit(ctx, audit.Entry{
		ActorType: audit.ActorAdmin, ActorID: adminID,
		Action: "announcement.delete", ObjectType: "announcement", ObjectID: id,
	})
	return nil
}
