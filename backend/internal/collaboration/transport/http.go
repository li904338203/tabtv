// Package transport 暴露协作域 HTTP 接口，并提供工作区上下文中间件。
package transport

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/li904338203/tabtv/backend/internal/collaboration/app"
	"github.com/li904338203/tabtv/backend/internal/collaboration/domain"
	"github.com/li904338203/tabtv/backend/internal/collaboration/repo"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// HeaderWorkspaceID 是工作区作用域请求头（CD-04）。
const HeaderWorkspaceID = "X-Workspace-Id"

type wsCtxKey struct{}

// WorkspaceContext 是中间件放进 context 的工作区上下文。
type WorkspaceContext struct {
	WorkspaceID int64
	Role        domain.Role
	// PayerAccountID 是计费主体。放进上下文而不是让账本自己再查一遍：
	// 成员资格校验本来就要连 workspace 表，顺手带回来即可。
	PayerAccountID int64
}

type Handler struct {
	svc     *app.Service
	userIDs func(ctx context.Context) (int64, bool)
}

// NewHandler 需要一个从 context 取当前用户 ID 的函数，
// 由身份域提供 —— 协作域不该知道 JWT claims 的结构。
func NewHandler(svc *app.Service, userIDs func(ctx context.Context) (int64, bool)) *Handler {
	return &Handler{svc: svc, userIDs: userIDs}
}

// Mount 挂载路由。requireUser 由身份域提供。
func (h *Handler) Mount(r chi.Router, requireUser func(http.Handler) http.Handler) {
	r.Route("/workspaces", func(r chi.Router) {
		r.Use(requireUser)

		// 不需要工作区上下文的接口。
		r.Get("/", h.list)
		r.Post("/", h.createTeam)
		r.Post("/invitations/accept", h.acceptInvitation)
		// 「我收到的邀请」跨工作区，按当前账号的联系方式反查，
		// 自然不能挂在某个工作区作用域下。
		r.Get("/invitations/mine", h.myInvitations)

		// 以下需要 X-Workspace-Id 且校验成员资格。
		r.Group(func(r chi.Router) {
			r.Use(h.RequireWorkspace)
			r.Patch("/current", h.rename)
			r.Delete("/current", h.archive)
			r.Post("/current/transfer", h.transfer)
			r.Get("/current/members", h.members)
			r.Delete("/current/members/{userID}", h.removeMember)
			r.Put("/current/members/{userID}/role", h.setRole)
			r.Get("/current/invitations", h.listInvitations)
			r.Post("/current/invitations", h.invite)
			r.Delete("/current/invitations/{invitationID}", h.revokeInvitation)
			// 授予方视角的共享管理：共享出去的是**本工作区**的资源。
			r.Get("/current/shares", h.listGranted)
			r.Post("/current/shares", h.grantShare)
			r.Delete("/current/shares/{shareID}", h.revokeShare)
			// 导入是往**当前**工作区里写，所以在这一组而不是 /shares 下。
			r.Post("/current/shares/import", h.importShared)
			// 在线状态（NEW-19）。心跳与列表合成一个接口：
			// 客户端本来就要定期发心跳，再单独轮询一次列表等于把请求翻倍。
			r.Post("/current/presence", h.heartbeat)
			r.Post("/current/presence/leave", h.leave)
			r.Get("/current/presence/{resourceType}/{resourceID}", h.online)
		})
	})
}

// RequireWorkspace 是 CD-04 的落地。
//
// ⚠️ 服务端**两条都要校验**：
//  1. 调用者是该工作区的 active 成员（本中间件）；
//  2. 被访问资源的 workspace_id 与请求头一致（各业务域查询里带上）。
//
// 只做第 1 条是不够的：请求头是环境权限（ambient authority），
// 用户可以带着自己工作区的 ID 去访问别人工作区的资源 ID。
func (h *Handler) RequireWorkspace(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw := r.Header.Get(HeaderWorkspaceID)
		if raw == "" {
			httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, "缺少 "+HeaderWorkspaceID+" 请求头"))
			return
		}
		wsID, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || wsID <= 0 {
			httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, HeaderWorkspaceID+" 非法"))
			return
		}
		userID, ok := h.userIDs(r.Context())
		if !ok {
			httpx.Fail(w, r, httpx.Err(errcode.Unauthenticated))
			return
		}
		role, payer, err := h.svc.MembershipWithPayer(r.Context(), wsID, userID)
		if err != nil {
			httpx.Fail(w, r, err)
			return
		}
		ctx := context.WithValue(r.Context(), wsCtxKey{},
			WorkspaceContext{WorkspaceID: wsID, Role: role, PayerAccountID: payer})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// WorkspaceFrom 取工作区上下文。业务域用它拿 workspaceID 与角色。
func WorkspaceFrom(ctx context.Context) (WorkspaceContext, bool) {
	c, ok := ctx.Value(wsCtxKey{}).(WorkspaceContext)
	return c, ok
}

// ---- handlers ----

type workspaceResp struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Kind           string `json:"kind"`
	Role           string `json:"role"`
	PayerAccountID string `json:"payerAccountId"`
}

func toResp(w repo.Workspace) workspaceResp {
	return workspaceResp{
		ID: itoa(w.ID), Name: w.Name, Kind: string(w.Kind),
		Role: string(w.Role), PayerAccountID: itoa(w.PayerAccountID),
	}
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	userID, _ := h.userIDs(r.Context())
	ws, err := h.svc.List(r.Context(), userID)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	out := make([]workspaceResp, 0, len(ws))
	for _, x := range ws {
		out = append(out, toResp(x))
	}
	httpx.JSON(w, http.StatusOK, out)
}

func (h *Handler) createTeam(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if !decode(w, r, &req) {
		return
	}
	userID, _ := h.userIDs(r.Context())
	ws, err := h.svc.CreateTeam(r.Context(), userID, req.Name)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, toResp(ws))
}

func (h *Handler) rename(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if !decode(w, r, &req) {
		return
	}
	wc, _ := WorkspaceFrom(r.Context())
	if err := h.svc.Rename(r.Context(), wc.WorkspaceID, wc.Role, req.Name); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) members(w http.ResponseWriter, r *http.Request) {
	wc, _ := WorkspaceFrom(r.Context())
	ms, err := h.svc.Members(r.Context(), wc.WorkspaceID)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	type memberResp struct {
		UserID   string `json:"userId"`
		Nickname string `json:"nickname"`
		Role     string `json:"role"`
		JoinedAt string `json:"joinedAt"`
	}
	out := make([]memberResp, 0, len(ms))
	for _, m := range ms {
		out = append(out, memberResp{
			UserID: itoa(m.UserID), Nickname: m.Nickname,
			Role: string(m.Role), JoinedAt: m.JoinedAt.UTC().Format(timeLayout),
		})
	}
	httpx.JSON(w, http.StatusOK, out)
}

func (h *Handler) transfer(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ToUserID string `json:"toUserId"`
	}
	if !decode(w, r, &req) {
		return
	}
	to, ok := parseID(w, r, req.ToUserID, "toUserId")
	if !ok {
		return
	}
	wc, _ := WorkspaceFrom(r.Context())
	actorID, _ := h.userIDs(r.Context())
	ws, err := h.svc.Describe(r.Context(), wc.WorkspaceID)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	if err := h.svc.Transfer(r.Context(), wc.WorkspaceID, ws.Kind, wc.Role, actorID, to); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) archive(w http.ResponseWriter, r *http.Request) {
	wc, _ := WorkspaceFrom(r.Context())
	actorID, _ := h.userIDs(r.Context())
	ws, err := h.svc.Describe(r.Context(), wc.WorkspaceID)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	res, err := h.svc.Archive(r.Context(), wc.WorkspaceID, ws.Kind, wc.Role, actorID)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	// 回条数而不是 204：归档会顺带作废共享与邀请，
	// 用户有权知道这次操作实际影响了多少对外授权。
	httpx.JSON(w, http.StatusOK, map[string]int64{
		"revokedShares": res.RevokedShares, "revokedInvitations": res.RevokedInvitation,
	})
}

func (h *Handler) myInvitations(w http.ResponseWriter, r *http.Request) {
	userID, _ := h.userIDs(r.Context())
	list, err := h.svc.MyInvitations(r.Context(), userID)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	type myInvResp struct {
		ID            string `json:"id"`
		WorkspaceID   string `json:"workspaceId"`
		WorkspaceName string `json:"workspaceName"`
		Role          string `json:"role"`
		InvitedBy     string `json:"invitedBy"`
		ExpiresAt     string `json:"expiresAt"`
	}
	out := make([]myInvResp, 0, len(list))
	for _, i := range list {
		out = append(out, myInvResp{
			ID: itoa(i.ID), WorkspaceID: itoa(i.WorkspaceID), WorkspaceName: i.WorkspaceName,
			Role: string(i.Role), InvitedBy: i.InvitedBy,
			ExpiresAt: i.ExpiresAt.UTC().Format(timeLayout),
		})
	}
	httpx.JSON(w, http.StatusOK, out)
}

func (h *Handler) invite(w http.ResponseWriter, r *http.Request) {
	var req struct {
		TargetKind  string `json:"targetKind"`
		TargetValue string `json:"targetValue"`
		Role        string `json:"role"`
	}
	if !decode(w, r, &req) {
		return
	}
	wc, _ := WorkspaceFrom(r.Context())
	userID, _ := h.userIDs(r.Context())
	res, err := h.svc.Invite(r.Context(), wc.WorkspaceID, wc.Role, userID, app.InviteInput{
		TargetKind: req.TargetKind, TargetValue: req.TargetValue, Role: domain.Role(req.Role),
	})
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	// token 明文只在这里返回一次，库里只有哈希。
	httpx.JSON(w, http.StatusCreated, struct {
		InvitationID string `json:"invitationId"`
		Token        string `json:"token"`
		ExpiresAt    string `json:"expiresAt"`
		// 顶掉了几条旧邀请。必须说出来 —— 不说的话用户以为之前发出去的码还能用。
		ReplacedPending int `json:"replacedPending"`
	}{
		InvitationID: itoa(res.InvitationID), Token: res.Token,
		ExpiresAt:       res.ExpiresAt.UTC().Format(timeLayout),
		ReplacedPending: res.ReplacedPending,
	})
}

func (h *Handler) acceptInvitation(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Token string `json:"token"`
	}
	if !decode(w, r, &req) {
		return
	}
	userID, _ := h.userIDs(r.Context())
	ws, err := h.svc.Accept(r.Context(), req.Token, userID)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, toResp(ws))
}

func (h *Handler) listInvitations(w http.ResponseWriter, r *http.Request) {
	wc, _ := WorkspaceFrom(r.Context())
	inv, err := h.svc.Invitations(r.Context(), wc.WorkspaceID, wc.Role)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	type invResp struct {
		ID          string `json:"id"`
		TargetKind  string `json:"targetKind"`
		TargetValue string `json:"targetValue"`
		Role        string `json:"role"`
		ExpiresAt   string `json:"expiresAt"`
	}
	out := make([]invResp, 0, len(inv))
	for _, i := range inv {
		out = append(out, invResp{
			ID: itoa(i.ID), TargetKind: i.TargetKind, TargetValue: i.TargetValue,
			Role: string(i.Role), ExpiresAt: i.ExpiresAt.UTC().Format(timeLayout),
		})
	}
	httpx.JSON(w, http.StatusOK, out)
}

func (h *Handler) revokeInvitation(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "invitationID")
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	wc, _ := WorkspaceFrom(r.Context())
	if err := h.svc.RevokeInvitation(r.Context(), wc.WorkspaceID, id, wc.Role); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) setRole(w http.ResponseWriter, r *http.Request) {
	target, err := pathID(r, "userID")
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	var req struct {
		Role string `json:"role"`
	}
	if !decode(w, r, &req) {
		return
	}
	wc, _ := WorkspaceFrom(r.Context())
	actorID, _ := h.userIDs(r.Context())
	if err := h.svc.SetRole(r.Context(), wc.WorkspaceID, target, wc.Role, actorID, domain.Role(req.Role)); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) removeMember(w http.ResponseWriter, r *http.Request) {
	target, err := pathID(r, "userID")
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	wc, _ := WorkspaceFrom(r.Context())
	actorID, _ := h.userIDs(r.Context())
	if err := h.svc.RemoveMember(r.Context(), wc.WorkspaceID, target, wc.Role, actorID); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(dst); err != nil {
		httpx.Fail(w, r, httpx.Err(errcode.InvalidJSON))
		return false
	}
	return true
}

func pathID(r *http.Request, name string) (int64, error) {
	v := chi.URLParam(r, name)
	id, err := strconv.ParseInt(v, 10, 64)
	if err != nil || id <= 0 {
		return 0, httpx.Errf(errcode.InvalidArgument, "路径参数 "+name+" 非法")
	}
	return id, nil
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }

func parseInt64(s string) (int64, error) { return strconv.ParseInt(s, 10, 64) }

// timeLayout 统一 UTC ISO-8601（GO-33）。
const timeLayout = "2006-01-02T15:04:05Z"
