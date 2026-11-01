// Package app 编排协作域用例。
package app

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"log/slog"
	"time"

	"github.com/li904338203/tabtv/backend/internal/collaboration/domain"
	"github.com/li904338203/tabtv/backend/internal/collaboration/repo"
	"github.com/li904338203/tabtv/backend/internal/platform/audit"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// SubscriptionRevoker 由 WebSocket hub 实现。
// 成员移除后必须同步撤销 API、媒体与**事件订阅**三处（NEW-18、FULL-06）。
//
// ⚠️ 两个方法**不能混用**（P12-03）：
//   - `RevokeUserTopic` 针对某一个人（移除成员、撤销共享给某人）；
//   - `RevokeTopic` 针对所有人（工作区归档）。
//
// 拿后者去做"移除某人"的话，整个团队的实时订阅会同时断掉 —— 他们什么都没做错，
// 界面上只会看到"订阅已撤销"然后再也收不到生成进度。这个错误**只在多人同时
// 在线时才暴露**，单人测试永远看不到。
type SubscriptionRevoker interface {
	RevokeUserTopic(userID int64, topic, reason string)
	RevokeTopic(topic, reason string)
}

type Service struct {
	repo    *repo.Repo
	nextID  func() int64
	revoker SubscriptionRevoker
	audit   *audit.Logger
	log     *slog.Logger

	// providers / importers 是共享资源的可插拔出入口（见 share.go）。
	providers map[domain.ResourceType]ResourceProvider
	importers map[domain.ResourceType]ResourceImporter
	// presence 是"此刻谁在看"，只存 Redis（见 presence.go）。为 nil 时整体降级。
	presence PresenceStore
}

func New(r *repo.Repo, nextID func() int64) *Service {
	return &Service{repo: r, nextID: nextID, log: slog.Default()}
}

func (s *Service) SetRevoker(r SubscriptionRevoker) { s.revoker = r }

func (s *Service) SetAudit(a *audit.Logger) { s.audit = a }

func (s *Service) SetLogger(l *slog.Logger) { s.log = l }

// writeAudit 写审计。审计失败只告警不阻断主流程（同 audit.Logger 的约定）。
func (s *Service) writeAudit(ctx context.Context, e audit.Entry) {
	if s.audit == nil {
		return
	}
	s.audit.Write(ctx, e)
}

// InvitationTTL 邀请有效期。
// 不设永久有效：泄漏的链接不该一直能用。
const InvitationTTL = 7 * 24 * time.Hour

func (s *Service) List(ctx context.Context, userID int64) ([]repo.Workspace, error) {
	ws, err := s.repo.ListForUser(ctx, userID)
	if err != nil {
		return nil, httpx.Wrap(errcode.Internal, err)
	}
	return ws, nil
}

// Membership 供中间件判定，是 CD-04 第 1 条校验。
func (s *Service) Membership(ctx context.Context, workspaceID, userID int64) (domain.Role, error) {
	role, _, err := s.MembershipWithPayer(ctx, workspaceID, userID)
	return role, err
}

// MembershipWithPayer 校验成员资格并回带计费主体，供工作区中间件一次装好上下文。
func (s *Service) MembershipWithPayer(ctx context.Context, workspaceID, userID int64) (
	domain.Role, int64, error) {
	role, payer, err := s.repo.MembershipWithPayer(ctx, workspaceID, userID)
	if err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			// 不区分"工作区不存在"与"你不是成员"：
			// 区分二者等于提供了一个工作区枚举接口。
			return "", 0, httpx.Err(errcode.NotWorkspaceMember)
		}
		return "", 0, httpx.Wrap(errcode.Internal, err)
	}
	return role, payer, nil
}

func (s *Service) CreateTeam(ctx context.Context, userID int64, name string) (repo.Workspace, error) {
	if err := domain.ValidateName(name); err != nil {
		return repo.Workspace{}, httpx.Errf(errcode.InvalidArgument, err.Error())
	}
	w, err := s.repo.CreateTeam(ctx, [3]int64{s.nextID(), s.nextID(), s.nextID()}, name, userID)
	if err != nil {
		return repo.Workspace{}, httpx.Wrap(errcode.Internal, err)
	}
	return w, nil
}

func (s *Service) Rename(ctx context.Context, workspaceID int64, actor domain.Role, name string) error {
	if !actor.Can(domain.ActionManage) {
		return httpx.Err(errcode.InsufficientRole)
	}
	if err := domain.ValidateName(name); err != nil {
		return httpx.Errf(errcode.InvalidArgument, err.Error())
	}
	if err := s.repo.Rename(ctx, workspaceID, name); err != nil {
		return httpx.Wrap(errcode.Internal, err)
	}
	return nil
}

// Describe 取工作区自身信息（转让/归档要先知道它是不是个人工作区）。
// 调用点都在 RequireWorkspace 之后，成员资格已经校验过。
func (s *Service) Describe(ctx context.Context, workspaceID int64) (repo.Workspace, error) {
	w, err := s.repo.Get(ctx, workspaceID)
	if err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			return repo.Workspace{}, httpx.Err(errcode.NotFound)
		}
		return repo.Workspace{}, httpx.Wrap(errcode.Internal, err)
	}
	return w, nil
}

func (s *Service) Members(ctx context.Context, workspaceID int64) ([]repo.Member, error) {
	m, err := s.repo.ListMembers(ctx, workspaceID)
	if err != nil {
		return nil, httpx.Wrap(errcode.Internal, err)
	}
	return m, nil
}

type InviteInput struct {
	TargetKind  string
	TargetValue string
	Role        domain.Role
}

type InviteResult struct {
	InvitationID int64
	// Token 是**明文**，只在创建时返回一次。库里只存哈希。
	Token     string
	ExpiresAt time.Time
	// ReplacedPending 是本次被顶掉的旧邀请数。
	//
	// ⚠️ 必须回给调用方并在界面上说出来：不说的话，用户会以为之前发出去的
	// 那个码还能用，而它在这一刻已经失效了 —— 同事点开收到的是"邀请码无效"，
	// 两边都不知道发生了什么。
	ReplacedPending int
}

func (s *Service) Invite(ctx context.Context, workspaceID int64, actor domain.Role, invitedBy int64, in InviteInput) (InviteResult, error) {
	if err := domain.CanAssign(actor, in.Role); err != nil {
		return InviteResult{}, mapAssignErr(err)
	}
	if in.TargetKind != "email" && in.TargetKind != "phone" {
		return InviteResult{}, httpx.Errf(errcode.InvalidArgument, "targetKind 取值非法")
	}
	if in.TargetValue == "" {
		return InviteResult{}, httpx.Errf(errcode.InvalidArgument, "请填写受邀人账号")
	}

	// ⚠️ 已经是成员的人不能再被邀请。邀请码一旦被接受，AcceptInvitation 的
	// on conflict 分支会把他的角色**改成邀请里写的那个** —— 于是"邀请同事"
	// 这个动作会悄悄把一个 admin 降成 viewer，而操作者完全没有这个意思。
	// 改角色有专门的入口。
	already, err := s.repo.MemberExistsByContact(ctx, workspaceID, in.TargetKind, in.TargetValue)
	if err != nil {
		return InviteResult{}, httpx.Wrap(errcode.Internal, err)
	}
	if already {
		return InviteResult{}, httpx.Errf(errcode.Conflict, domain.ErrAlreadyMember.Error())
	}

	// 顶掉同目标的旧邀请。这一步在建新邀请**之前**跑，不是可选的优化：
	// uk_invite_pending 会让第二次邀请撞唯一索引，而 PG 的 23505 映射到
	// 业务上就是「名称已存在」—— 一句和当前操作完全对不上号的错误。
	replaced, err := s.repo.RevokePendingForTarget(ctx, workspaceID, in.TargetKind, in.TargetValue)
	if err != nil {
		return InviteResult{}, httpx.Wrap(errcode.Internal, err)
	}

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return InviteResult{}, httpx.Wrap(errcode.Internal, err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	expires := time.Now().Add(InvitationTTL)

	id := s.nextID()
	if err := s.repo.CreateInvitation(ctx, id, repo.Invitation{
		WorkspaceID: workspaceID, TargetKind: in.TargetKind,
		TargetValue: in.TargetValue, Role: in.Role, ExpiresAt: expires,
	}, hashToken(token), invitedBy); err != nil {
		return InviteResult{}, httpx.Wrap(errcode.Internal, err)
	}
	s.writeAudit(ctx, audit.Entry{
		ActorType: audit.ActorUser, ActorID: invitedBy, WorkspaceID: workspaceID,
		Action: "workspace.invite", ObjectType: "workspace_invitation", ObjectID: id,
		// ⚠️ 审计里**不记 token**（连哈希都不记）：审计表是给运营查的，
		// 记了等于把邀请码变成"有后台只读权限就能拿到"的东西。
		After: map[string]any{"targetKind": in.TargetKind, "role": string(in.Role)},
	})
	return InviteResult{InvitationID: id, Token: token,
		ExpiresAt: expires, ReplacedPending: replaced}, nil
}

// MyInvitations 列出发给当前用户的待处理邀请。
//
// ⚠️ 不回 token（见 repo.MyInvitation 的说明）。这个列表的作用是
// 「让用户知道有人邀请过他、去哪个邮箱/短信里找那个码」，不是替代邀请码本身。
func (s *Service) MyInvitations(ctx context.Context, userID int64) ([]repo.MyInvitation, error) {
	email, phone, err := s.repo.UserContact(ctx, userID)
	if err != nil {
		return nil, httpx.Wrap(errcode.Internal, err)
	}
	list, err := s.repo.ListInvitationsFor(ctx, email, phone)
	if err != nil {
		return nil, httpx.Wrap(errcode.Internal, err)
	}
	return list, nil
}

// Transfer 把工作区转让给另一个成员。
func (s *Service) Transfer(ctx context.Context, workspaceID int64, kind domain.Kind,
	actor domain.Role, actorID, toUserID int64) error {
	if err := domain.CanTransfer(actor, kind); err != nil {
		return mapTransferErr(err)
	}
	if toUserID == actorID {
		return httpx.Errf(errcode.InvalidArgument, domain.ErrTransferToSelf.Error())
	}
	if err := s.repo.TransferOwner(ctx, workspaceID, actorID, toUserID); err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			return httpx.Errf(errcode.NotFound, domain.ErrTargetNotMember.Error())
		}
		return httpx.Wrap(errcode.Internal, err)
	}
	s.writeAudit(ctx, audit.Entry{
		ActorType: audit.ActorUser, ActorID: actorID, WorkspaceID: workspaceID,
		Action: "workspace.transfer", ObjectType: "workspace", ObjectID: workspaceID,
		Before: map[string]any{"ownerUserId": itoa(actorID)},
		After:  map[string]any{"ownerUserId": itoa(toUserID)},
	})
	return nil
}

// ArchiveResult 回给调用方，让界面能说清"顺带作废了几条共享与邀请"。
type ArchiveResult struct {
	RevokedShares     int64
	RevokedInvitation int64
}

// Archive 归档团队工作区。
func (s *Service) Archive(ctx context.Context, workspaceID int64, kind domain.Kind,
	actor domain.Role, actorID int64) (ArchiveResult, error) {
	if err := domain.CanTransfer(actor, kind); err != nil {
		return ArchiveResult{}, mapTransferErr(err)
	}
	res, err := s.repo.Archive(ctx, workspaceID, actorID)
	if err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			return ArchiveResult{}, httpx.Err(errcode.NotFound)
		}
		return ArchiveResult{}, httpx.Wrap(errcode.Internal, err)
	}
	s.writeAudit(ctx, audit.Entry{
		ActorType: audit.ActorUser, ActorID: actorID, WorkspaceID: workspaceID,
		Action: "workspace.archive", ObjectType: "workspace", ObjectID: workspaceID,
		After: map[string]any{
			"revokedShares": res.RevokedShares, "revokedInvitations": res.RevokedInvitation,
		},
	})
	// 归档后全员立刻失去访问（各查询都带 w.status='active'），
	// 但还连着的 WebSocket 不会自己断 —— 订阅必须显式撤销。
	if s.revoker != nil {
		s.revoker.RevokeTopic(workspaceTopic(workspaceID), "workspace_archived")
	}
	return ArchiveResult{RevokedShares: res.RevokedShares,
		RevokedInvitation: res.RevokedInvitation}, nil
}

func mapTransferErr(err error) error {
	switch {
	case errors.Is(err, domain.ErrNotOwner):
		return httpx.Errf(errcode.InsufficientRole, err.Error())
	default:
		return httpx.Errf(errcode.InvalidArgument, err.Error())
	}
}

func (s *Service) Accept(ctx context.Context, token string, userID int64) (repo.Workspace, error) {
	if token == "" {
		return repo.Workspace{}, httpx.Errf(errcode.InvalidArgument, "缺少邀请码")
	}
	w, err := s.repo.AcceptInvitation(ctx, hashToken(token), userID, s.nextID())
	if err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			// 不存在、已用过、已撤销、已过期，一律同一错误：
			// 区分它们会泄漏邀请码的有效性，便于爆破。
			return repo.Workspace{}, httpx.Errf(errcode.NotFound, "邀请码无效或已失效")
		}
		return repo.Workspace{}, httpx.Wrap(errcode.Internal, err)
	}
	return w, nil
}

func (s *Service) Invitations(ctx context.Context, workspaceID int64, actor domain.Role) ([]repo.Invitation, error) {
	if !actor.Can(domain.ActionManage) {
		return nil, httpx.Err(errcode.InsufficientRole)
	}
	inv, err := s.repo.ListInvitations(ctx, workspaceID)
	if err != nil {
		return nil, httpx.Wrap(errcode.Internal, err)
	}
	return inv, nil
}

func (s *Service) RevokeInvitation(ctx context.Context, workspaceID, invitationID int64, actor domain.Role) error {
	if !actor.Can(domain.ActionManage) {
		return httpx.Err(errcode.InsufficientRole)
	}
	if err := s.repo.RevokeInvitation(ctx, workspaceID, invitationID); err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			return httpx.Err(errcode.NotFound)
		}
		return httpx.Wrap(errcode.Internal, err)
	}
	return nil
}

func (s *Service) SetRole(ctx context.Context, workspaceID, targetUserID int64, actor domain.Role, actorID int64, role domain.Role) error {
	if err := domain.CanAssign(actor, role); err != nil {
		return mapAssignErr(err)
	}
	target, err := s.repo.Membership(ctx, workspaceID, targetUserID)
	if err != nil {
		return httpx.Err(errcode.NotFound)
	}
	// 不能改动 owner 的角色，也不能改动与自己同级或更高的成员。
	if target == domain.RoleOwner {
		return httpx.Errf(errcode.InsufficientRole, domain.ErrCannotRemoveOwner.Error())
	}
	if actor != domain.RoleOwner && target.Rank() >= actor.Rank() {
		return httpx.Errf(errcode.InsufficientRole, "不能修改同级或更高成员的角色")
	}
	if err := s.repo.SetRole(ctx, workspaceID, targetUserID, role); err != nil {
		return httpx.Wrap(errcode.Internal, err)
	}
	return nil
}

// RemoveMember 移除成员，并同步撤销其事件订阅。
func (s *Service) RemoveMember(ctx context.Context, workspaceID, targetUserID int64, actor domain.Role, actorID int64) error {
	// 允许自己退出（不需要 manage 权限）。
	selfLeave := actorID == targetUserID
	if !selfLeave && !actor.Can(domain.ActionManage) {
		return httpx.Err(errcode.InsufficientRole)
	}

	target, err := s.repo.Membership(ctx, workspaceID, targetUserID)
	if err != nil {
		return httpx.Err(errcode.NotFound)
	}
	// owner 不能被移除，也不能自行退出 —— 否则工作区会没有归属人。
	if target == domain.RoleOwner {
		return httpx.Errf(errcode.InsufficientRole, domain.ErrCannotRemoveOwner.Error())
	}
	if !selfLeave && actor != domain.RoleOwner && target.Rank() >= actor.Rank() {
		return httpx.Errf(errcode.InsufficientRole, "不能移除同级或更高的成员")
	}

	if err := s.repo.RemoveMember(ctx, workspaceID, targetUserID); err != nil {
		return httpx.Wrap(errcode.Internal, err)
	}

	// ⚠️ 三处撤销必须同步（NEW-18、FULL-06）：
	//   1. **API 鉴权** —— 下次请求即 403（`MembershipWithPayer` 带 status='active'）；
	//   2. **媒体访问** —— 媒体按 workspace_id 卡归属，而取媒体要先过工作区中间件，
	//      所以第 1 条成立时这一条自动成立（E2E 第 150 步钉住了这一点）；
	//   3. **事件订阅** —— 只删成员行而不断订阅的话，被移除的人仍会持续收到
	//      该工作区的生成事件。
	//
	// ⚠️ 用 `RevokeUserTopic` 而**不是** `RevokeTopic`：后者会把所有订阅了这个
	// topic 的连接一起踢掉，于是"移除某人"变成"全团队掉线"。
	if s.revoker != nil {
		s.revoker.RevokeUserTopic(targetUserID, workspaceTopic(workspaceID), "member_removed")
	}
	return nil
}

// mapAssignErr 区分权限问题与参数问题：
// 四态规范要求错误按类别分流，不能混成一句"操作失败"。
func mapAssignErr(err error) error {
	switch {
	case errors.Is(err, domain.ErrNoManage), errors.Is(err, domain.ErrRoleTooHigh):
		return httpx.Errf(errcode.InsufficientRole, err.Error())
	default:
		return httpx.Errf(errcode.InvalidArgument, err.Error())
	}
}

func workspaceTopic(id int64) string {
	return "workspace:" + itoa(id)
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	neg := v < 0
	if neg {
		v = -v
	}
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
