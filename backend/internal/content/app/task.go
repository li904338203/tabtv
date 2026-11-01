package app

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/li904338203/tabtv/backend/internal/content/domain"
	"github.com/li904338203/tabtv/backend/internal/content/repo"
	"github.com/li904338203/tabtv/backend/internal/platform/audit"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// ---------------------------------------------------------------- 任务中心

// Tasks 列出统一任务历史（GAP-05）。
func (s *Service) Tasks(ctx context.Context, workspaceID int64, kind, state string,
	limit int, cursorAt *time.Time, cursorID int64) ([]repo.Task, repo.TaskCounts, error) {
	k := domain.TaskKind(kind)
	if kind != "" && !k.Valid() {
		return nil, nil, httpx.Errf(errcode.InvalidArgument, "任务类型非法")
	}
	st, ok := domain.ParseState(state)
	if !ok {
		return nil, nil, httpx.Errf(errcode.InvalidArgument, "任务状态非法")
	}
	list, err := s.repo.ListTasks(ctx, workspaceID, repo.TaskFilter{
		Kind: k, State: st, Limit: limit, CursorAt: cursorAt, CursorID: cursorID,
	})
	if err != nil {
		return nil, nil, httpx.Wrap(errcode.Internal, err)
	}
	// ⚠️ 计数与列表用**同一个 union**（见 repo.CountTasks）：分开写的话
	// 筛选器上的数字与列表内容对不上，而两个数都"没错"。
	counts, err := s.repo.CountTasks(ctx, workspaceID, k)
	if err != nil {
		// 计数失败只让筛选器少几个数字，不该让整张列表打不开。
		s.log.Warn("任务计数失败，已降级", "error", err)
		counts = repo.TaskCounts{}
	}
	return list, counts, nil
}

// ---------------------------------------------------------------- 作品

// SubmitWork 把一次成功的任务提交成作品。
//
// ⚠️ 调用方（画布/时间线域）必须**先校验来源归属与是否成功** ——
// 本域只认 workspaceID 与来源引用，它不知道"这次生成是不是这个人的"。
func (s *Service) SubmitWork(ctx context.Context, w repo.Work) (repo.Work, error) {
	if w.SourceType != "generation" && w.SourceType != "render" {
		return repo.Work{}, httpx.Errf(errcode.InvalidArgument, "来源类型非法")
	}
	title, err := normalizeWorkTitle(w.Title)
	if err != nil {
		return repo.Work{}, err
	}
	w.Title = title
	out, err := s.repo.SubmitWork(ctx, w)
	if err != nil {
		return repo.Work{}, httpx.Wrap(errcode.Internal, err)
	}
	return out, nil
}

func (s *Service) MyWorks(ctx context.Context, workspaceID int64, limit int,
	cursorID int64) ([]repo.Work, error) {
	list, err := s.repo.ListWorksOf(ctx, workspaceID, limit, cursorID)
	if err != nil {
		return nil, httpx.Wrap(errcode.Internal, err)
	}
	return list, nil
}

// ---------------------------------------------------------------- 后台审核

func (s *Service) AdminWorks(ctx context.Context, f repo.AdminWorkFilter) ([]repo.Work, error) {
	list, err := s.repo.ListWorksForAdmin(ctx, f)
	if err != nil {
		return nil, httpx.Wrap(errcode.Internal, err)
	}
	return list, nil
}

// WorkDetail 作品详情 + 审核历史（"后台可追踪发布原因"）。
func (s *Service) WorkDetail(ctx context.Context, id int64) (repo.Work, []repo.ReviewRecord, error) {
	w, err := s.repo.GetWork(ctx, id)
	if errors.Is(err, repo.ErrNotFound) {
		return repo.Work{}, nil, httpx.Err(errcode.NotFound)
	}
	if err != nil {
		return repo.Work{}, nil, httpx.Wrap(errcode.Internal, err)
	}
	history, err := s.repo.ReviewHistory(ctx, id)
	if err != nil {
		// 历史取不到不该让详情打不开 —— 但要告警，因为它正是这个页面的重点。
		s.log.Warn("审核历史取失败", "error", err, "workId", id)
	}
	return w, history, nil
}

// Review 执行一次审核动作（AD-09）。
//
// 顺序：读当前状态 → domain 校验 → domain 算出新状态 → 一个事务落地
// （改 work + 写 content_review）→ 通知作者。
//
// ⚠️ 通知放在**最后且失败不回滚**：审核已经生效了，为一条通知把它撤回
// 是用一个次要问题制造一个更严重的问题（同 audit.Logger 的约定）。
func (s *Service) Review(ctx context.Context, adminID, workID int64,
	action string, reason string) (repo.Work, error) {
	act := domain.ReviewAction(action)
	cur, err := s.repo.GetWork(ctx, workID)
	if errors.Is(err, repo.ErrNotFound) {
		return repo.Work{}, httpx.Err(errcode.NotFound)
	}
	if err != nil {
		return repo.Work{}, httpx.Wrap(errcode.Internal, err)
	}
	before := domain.WorkState{
		ReviewStatus: cur.ReviewStatus, IsFeatured: cur.IsFeatured, IsOffline: cur.IsOffline,
	}
	if err := domain.ValidateReview(before, act, reason); err != nil {
		code := errcode.InvalidArgument
		if errors.Is(err, domain.ErrAlreadyOffline) ||
			errors.Is(err, domain.ErrFeatureUnapproved) {
			// 这两条是"当前状态不允许"，不是"参数写错了" ——
			// 混成 400 的话运营会去改自己的输入，而输入没有任何问题。
			code = errcode.InvalidStateTransition
		}
		return repo.Work{}, httpx.Errf(code, err.Error())
	}
	after := domain.ApplyReview(before, act)

	out, err := s.repo.ApplyReview(ctx, workID, before, after, act, adminID, reason)
	if errors.Is(err, repo.ErrNotFound) {
		// CAS 落空 = 期间被另一个运营改过。
		return repo.Work{}, httpx.Errf(errcode.Conflict,
			"这件作品的状态已被其他操作改变，请刷新后重试")
	}
	if err != nil {
		return repo.Work{}, httpx.Wrap(errcode.Internal, err)
	}

	s.writeAudit(ctx, auditEntry(adminID, workID, action, before, after, reason))

	// ⚠️ 通知的幂等引用**必须带动作**：先驳回后通过要发两条，
	// 只用 workID 的话第二条会被唯一索引挡掉 —— 作者永远收不到"已通过"。
	if notifyType, ok := domain.NotifyOnReview(act); ok && cur.AuthorUserID > 0 {
		title, body := reviewNotice(act, cur.Title, reason)
		s.Notify(ctx, cur.AuthorUserID, repo.Message{
			Type: notifyType, Title: title, Content: body,
			RefType: "work", RefID: domain.ReviewRefID(itoa(workID), act),
		})
	}
	return out, nil
}

// reviewNotice 组织通知文案。
//
// ⚠️ 驳回与下架**必须把原因带给作者**：只说"你的作品被驳回了"，
// 他既不知道该改什么，也只能来问客服 —— 而那条原因运营本来就填了。
func reviewNotice(act domain.ReviewAction, title, reason string) (string, string) {
	name := title
	if name == "" {
		name = "你的作品"
	}
	switch act {
	case domain.ActionApprove:
		return "作品已通过审核", name + " 已通过审核。"
	case domain.ActionReject:
		return "作品未通过审核", name + " 未通过审核：" + reason
	case domain.ActionOffline:
		return "作品已下架", name + " 已被下架：" + reason
	}
	return "", ""
}

func auditEntry(adminID, workID int64, action string,
	before, after domain.WorkState, reason string) audit.Entry {
	return audit.Entry{
		ActorType: audit.ActorAdmin, ActorID: adminID,
		Action: "work." + action, ObjectType: "work", ObjectID: workID,
		Before: map[string]any{"reviewStatus": before.ReviewStatus,
			"featured": before.IsFeatured, "offline": before.IsOffline},
		After: map[string]any{"reviewStatus": after.ReviewStatus,
			"featured": after.IsFeatured, "offline": after.IsOffline},
		Reason: reason,
	}
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }

// OpsTasks 后台任务运营台检索（P13-03）。
//
// ⚠️ 列表与汇总用**同一套过滤条件**（汇总里不含状态本身，因为它就是按状态
// 分组的）。分开算的话运维看到的"共 132 条失败"与下面列出来的不是同一批 ——
// **而两个数字都"没错"**（CD-56 踩过一次）。
func (s *Service) OpsTasks(ctx context.Context, f repo.OpsTaskFilter, now time.Time) (
	[]repo.OpsTask, repo.OpsSummary, error) {
	list, err := s.repo.ListOpsTasks(ctx, f, now)
	if err != nil {
		return nil, repo.OpsSummary{}, httpx.Wrap(errcode.Internal, err)
	}
	summary, err := s.repo.SummarizeOpsTasks(ctx, f, now)
	if err != nil {
		return nil, repo.OpsSummary{}, httpx.Wrap(errcode.Internal, err)
	}
	return list, summary, nil
}

// workTitleMax 与 work.title 的 varchar(100) 一致（按字符算，PG 的 varchar 也按字符）。
const workTitleMax = 100

// normalizeWorkTitle 去首尾空白；空标题给「未命名作品」（来源校验目前给不出标题，
// 存空串的话审核台与作品墙上就是一行空白）；超长明确报参数错误 ——
// 不拦的话落库失败成 500，用户只看到「服务器错误」。
func normalizeWorkTitle(title string) (string, error) {
	t := strings.TrimSpace(title)
	if t == "" {
		return "未命名作品", nil
	}
	if utf8.RuneCountInString(t) > workTitleMax {
		return "", httpx.Errf(errcode.InvalidArgument, fmt.Sprintf("作品标题最多 %d 个字", workTitleMax))
	}
	return t, nil
}
