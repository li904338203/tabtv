package transport

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/li904338203/tabtv/backend/internal/collaboration/app"
	"github.com/li904338203/tabtv/backend/internal/collaboration/domain"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// MountShares 挂载**受让方视角**的共享路由。
//
// ⚠️ 这一组刻意不套 RequireWorkspace。受让方不是源工作区的成员，
// 带不出 X-Workspace-Id，也不该被要求带 —— 让他先"切换到别人的工作区"
// 才能看共享给他的东西，等于把共享退化成了临时成员资格，
// 而那正是 NEW-17/NEW-18 要分开的两件事。授权的唯一来源是 resource_share。
//
// 授予方视角（谁共享给了谁、撤销）在 Mount 里，是工作区作用域的。
func (h *Handler) MountShares(r chi.Router, requireUser func(http.Handler) http.Handler) {
	r.Route("/shares", func(r chi.Router) {
		r.Use(requireUser)
		r.Get("/received", h.listReceived)
		r.Get("/received/{resourceType}/{resourceID}", h.openShared)
	})
}

type shareResp struct {
	ID           string   `json:"id"`
	ResourceType string   `json:"resourceType"`
	ResourceID   string   `json:"resourceId"`
	ResourceName string   `json:"resourceName"`
	GranteeType  string   `json:"granteeType"`
	GranteeID    string   `json:"granteeId"`
	GranteeName  string   `json:"granteeName"`
	Permissions  []string `json:"permissions"`
	CreatedAt    string   `json:"createdAt"`
}

func permStrings(p domain.PermSet) []string {
	acts := p.List()
	out := make([]string, 0, len(acts))
	for _, a := range acts {
		out = append(out, string(a))
	}
	return out
}

func (h *Handler) listGranted(w http.ResponseWriter, r *http.Request) {
	wc, _ := WorkspaceFrom(r.Context())
	items, err := h.svc.ListGranted(r.Context(), wc.WorkspaceID, wc.Role)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	out := make([]shareResp, 0, len(items))
	for _, it := range items {
		out = append(out, shareResp{
			ID: itoa(it.ID), ResourceType: string(it.Resource),
			ResourceID: itoa(it.ResourceID), ResourceName: it.ResourceName,
			GranteeType: string(it.GranteeType), GranteeID: itoa(it.GranteeID),
			GranteeName: it.GranteeName, Permissions: permStrings(it.Permissions),
			CreatedAt: it.CreatedAt.UTC().Format(timeLayout),
		})
	}
	httpx.JSON(w, http.StatusOK, out)
}

func (h *Handler) grantShare(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ResourceType string   `json:"resourceType"`
		ResourceID   string   `json:"resourceId"`
		GranteeType  string   `json:"granteeType"`
		GranteeID    string   `json:"granteeId"`
		Permissions  []string `json:"permissions"`
	}
	if !decode(w, r, &req) {
		return
	}
	resourceID, ok := parseID(w, r, req.ResourceID, "resourceId")
	if !ok {
		return
	}
	granteeID, ok := parseID(w, r, req.GranteeID, "granteeId")
	if !ok {
		return
	}
	wc, _ := WorkspaceFrom(r.Context())
	userID, _ := h.userIDs(r.Context())
	sh, err := h.svc.Grant(r.Context(), wc.WorkspaceID, wc.Role, userID, app.GrantInput{
		Resource: domain.ResourceType(req.ResourceType), ResourceID: resourceID,
		GranteeType: domain.GranteeType(req.GranteeType), GranteeID: granteeID,
		Permissions: req.Permissions,
	})
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, shareResp{
		ID: itoa(sh.ID), ResourceType: string(sh.Resource), ResourceID: itoa(sh.ResourceID),
		ResourceName: sh.ResourceName,
		GranteeType:  string(sh.GranteeType), GranteeID: itoa(sh.GranteeID),
		GranteeName: sh.GranteeName, Permissions: permStrings(sh.Permissions),
		CreatedAt: sh.CreatedAt.UTC().Format(timeLayout),
	})
}

func (h *Handler) revokeShare(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "shareID")
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	wc, _ := WorkspaceFrom(r.Context())
	userID, _ := h.userIDs(r.Context())
	if err := h.svc.Revoke(r.Context(), wc.WorkspaceID, id, wc.Role, userID); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type receivedResp struct {
	ID                 string   `json:"id"`
	ResourceType       string   `json:"resourceType"`
	ResourceID         string   `json:"resourceId"`
	ResourceName       string   `json:"resourceName"`
	OwnerWorkspaceName string   `json:"ownerWorkspaceName"`
	Permissions        []string `json:"permissions"`
	// Gone 与"被撤销"是两件事：撤销后这一行根本不会出现，
	// Gone 表示对方把东西删了。合成一个的话用户会怪错人。
	Gone       bool   `json:"gone"`
	Importable bool   `json:"importable"`
	CreatedAt  string `json:"createdAt"`
}

func (h *Handler) listReceived(w http.ResponseWriter, r *http.Request) {
	userID, _ := h.userIDs(r.Context())
	items, err := h.svc.ListReceived(r.Context(), userID)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	out := make([]receivedResp, 0, len(items))
	for _, it := range items {
		out = append(out, receivedResp{
			ID: itoa(it.ID), ResourceType: string(it.Resource),
			ResourceID: itoa(it.ResourceID), ResourceName: it.ResourceName,
			OwnerWorkspaceName: it.OwnerWorkspaceName,
			Permissions:        permStrings(it.Permissions),
			Gone:               it.Gone, Importable: it.Importable,
			CreatedAt: it.CreatedAt.UTC().Format(timeLayout),
		})
	}
	httpx.JSON(w, http.StatusOK, out)
}

func (h *Handler) openShared(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "resourceID")
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	userID, _ := h.userIDs(r.Context())
	view, perms, err := h.svc.OpenShared(r.Context(), userID,
		domain.ResourceType(chi.URLParam(r, "resourceType")), id)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, struct {
		ID          string          `json:"id"`
		Name        string          `json:"name"`
		Kind        string          `json:"kind"`
		UpdatedAt   string          `json:"updatedAt"`
		Permissions []string        `json:"permissions"`
		Detail      json.RawMessage `json:"detail,omitempty"`
	}{
		ID: itoa(view.ID), Name: view.Name, Kind: view.Kind,
		UpdatedAt:   view.UpdatedAt.UTC().Format(timeLayout),
		Permissions: permStrings(perms), Detail: view.Detail,
	})
}

// importShared 把共享资源复制进**当前**工作区，因此挂在工作区作用域下。
func (h *Handler) importShared(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ResourceType string `json:"resourceType"`
		ResourceID   string `json:"resourceId"`
	}
	if !decode(w, r, &req) {
		return
	}
	resourceID, ok := parseID(w, r, req.ResourceID, "resourceId")
	if !ok {
		return
	}
	wc, _ := WorkspaceFrom(r.Context())
	userID, _ := h.userIDs(r.Context())
	newID, err := h.svc.ImportShared(r.Context(), userID, wc.WorkspaceID, wc.Role,
		domain.ResourceType(req.ResourceType), resourceID)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, map[string]string{"resourceId": itoa(newID)})
}

func parseID(w http.ResponseWriter, r *http.Request, raw, name string) (int64, bool) {
	id, err := parseInt64(raw)
	if err != nil || id <= 0 {
		httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, name+" 非法"))
		return 0, false
	}
	return id, true
}

// ---------------------------------------------------------------- 在线状态

type onlineResp struct {
	UserID   string `json:"userId"`
	Nickname string `json:"nickname"`
	Clients  int    `json:"clients"`
	SeenAt   string `json:"seenAt"`
	IsSelf   bool   `json:"isSelf"`
}

// heartbeat 续期并回带在线列表。
//
// ⚠️ 挂在工作区作用域下：presence 只在成员之间可见。虽然它不泄漏资源内容，
// 但"谁在什么时候看了哪个项目"本身就是团队内部信息。
func (h *Handler) heartbeat(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ResourceType string `json:"resourceType"`
		ResourceID   string `json:"resourceId"`
		ClientID     string `json:"clientId"`
	}
	if !decode(w, r, &req) {
		return
	}
	resourceID, ok := parseID(w, r, req.ResourceID, "resourceId")
	if !ok {
		return
	}
	wc, _ := WorkspaceFrom(r.Context())
	userID, _ := h.userIDs(r.Context())
	list, err := h.svc.Heartbeat(r.Context(), app.PresenceInput{
		WorkspaceID: wc.WorkspaceID, ResourceType: req.ResourceType,
		// ⚠️ 昵称**不从请求体收**，由服务端按 userID 查 ——
		// 让客户端自己报的话，任何成员都能在成员条上显示成别人的名字，
		// 而 presence 是团队内可见的，冒充一个同事名字的成本为零（GO-27 同一条）。
		ResourceID: resourceID, UserID: userID, ClientID: req.ClientID,
	})
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	writeOnline(w, list)
}

// leave 主动离开。回 204 且**从不报错** —— 它是优化不是正确性
// （不调也会在 TTL 之后自然消失）。
func (h *Handler) leave(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ResourceType string `json:"resourceType"`
		ResourceID   string `json:"resourceId"`
		ClientID     string `json:"clientId"`
	}
	if !decode(w, r, &req) {
		return
	}
	id, _ := parseInt64(req.ResourceID)
	wc, _ := WorkspaceFrom(r.Context())
	userID, _ := h.userIDs(r.Context())
	h.svc.Leave(r.Context(), app.PresenceInput{
		WorkspaceID: wc.WorkspaceID, ResourceType: req.ResourceType,
		ResourceID: id, UserID: userID, ClientID: req.ClientID,
	})
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) online(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "resourceID")
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	wc, _ := WorkspaceFrom(r.Context())
	userID, _ := h.userIDs(r.Context())
	list, err := h.svc.Online(r.Context(), wc.WorkspaceID,
		chi.URLParam(r, "resourceType"), id, userID)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	writeOnline(w, list)
}

func writeOnline(w http.ResponseWriter, list []domain.OnlineUser) {
	out := make([]onlineResp, 0, len(list))
	for _, u := range list {
		out = append(out, onlineResp{
			UserID: itoa(u.UserID), Nickname: u.Nickname, Clients: u.Clients,
			SeenAt: u.SeenAt.UTC().Format(timeLayout), IsSelf: u.IsSelf,
		})
	}
	httpx.JSON(w, http.StatusOK, out)
}
