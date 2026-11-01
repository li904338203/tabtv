package transport

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/li904338203/tabtv/backend/internal/mcp/app"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// ApprovalHandler 是页面审批 /mcp/approvals/{quoteId}（MCPT-13，MCP-F-44）。挂在 /api/v1 内，
// **只要登录**（RequireUser），不挂工作区中间件：审批的归属以 quote 所属授权的 user_id 为准，
// 不看 X-Workspace-Id（用户可能正切在别的工作区里打开这条链接）。
type ApprovalHandler struct {
	svc     *app.Approvals
	userIDs func(ctx context.Context) (int64, bool)
}

// NewApprovalHandler 构造。
func NewApprovalHandler(svc *app.Approvals, userIDs func(ctx context.Context) (int64, bool)) *ApprovalHandler {
	return &ApprovalHandler{svc: svc, userIDs: userIDs}
}

// Mount 挂 GET /mcp/approvals/{quoteID}、POST …/approve、POST …/reject。
func (h *ApprovalHandler) Mount(r chi.Router, requireUser func(http.Handler) http.Handler) {
	r.With(requireUser).Get("/mcp/approvals/{quoteID}", h.get)
	r.With(requireUser).Post("/mcp/approvals/{quoteID}/approve", h.approve)
	r.With(requireUser).Post("/mcp/approvals/{quoteID}/reject", h.reject)
}

// approvalItemResp / approvalResp 对应 OpenAPI 的 McpApprovalItem / McpApproval。
type approvalItemResp struct {
	Label   string `json:"label"`
	Credits int    `json:"credits"`
}

type approvalResp struct {
	QuoteID        string             `json:"quoteId"`
	State          string             `json:"state"`
	ClientName     string             `json:"clientName"`
	WorkspaceID    string             `json:"workspaceId"`
	WorkspaceName  string             `json:"workspaceName"`
	WorkspaceKind  string             `json:"workspaceKind"`
	Tool           string             `json:"tool"`
	Summary        string             `json:"summary"`
	Items          []approvalItemResp `json:"items"`
	Credits        int                `json:"credits"`
	PayerAccountID string             `json:"payerAccountId"`
	Balance        int                `json:"balance"`
	After          int                `json:"after"`
	Shortfall      int                `json:"shortfall"`
	ExpiresAt      string             `json:"expiresAt"`
	ApprovedAt     *string            `json:"approvedAt"`
	RejectedAt     *string            `json:"rejectedAt"`
	UsedAt         *string            `json:"usedAt"`
}

func toApproval(v app.ApprovalView) approvalResp {
	items := make([]approvalItemResp, 0, len(v.Items))
	for _, it := range v.Items {
		items = append(items, approvalItemResp{Label: it.Label, Credits: it.Credits})
	}
	return approvalResp{
		QuoteID: idStr(v.QuoteID), State: v.State, ClientName: v.ClientName, WorkspaceID: idStr(v.WorkspaceID),
		WorkspaceName: v.WorkspaceName, WorkspaceKind: v.WorkspaceKind, Tool: v.Tool, Summary: v.Summary,
		Items: items, Credits: v.Credits, PayerAccountID: idStr(v.PayerAccountID), Balance: v.Balance,
		After: v.After, Shortfall: v.Shortfall, ExpiresAt: v.ExpiresAt.UTC().Format(time.RFC3339),
		ApprovedAt: ts(v.ApprovedAt), RejectedAt: ts(v.RejectedAt), UsedAt: ts(v.UsedAt),
	}
}

func (h *ApprovalHandler) get(w http.ResponseWriter, r *http.Request) {
	h.run(w, r, h.svc.Get)
}

func (h *ApprovalHandler) approve(w http.ResponseWriter, r *http.Request) {
	h.run(w, r, h.svc.Approve)
}

func (h *ApprovalHandler) reject(w http.ResponseWriter, r *http.Request) {
	h.run(w, r, h.svc.Reject)
}

func (h *ApprovalHandler) run(w http.ResponseWriter, r *http.Request,
	fn func(ctx context.Context, userID, quoteID int64) (app.ApprovalView, error)) {
	id, err := pathID(r, "quoteID")
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	uid, ok := h.userIDs(r.Context())
	if !ok {
		httpx.Fail(w, r, httpx.Err(errcode.Unauthenticated))
		return
	}
	v, err := fn(r.Context(), uid, id)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, toApproval(v))
}

// updateGrant 是 PATCH /me/mcp/grants/{grantId}（MCPT-13）：目前只有 requirePageConfirm 一个可改字段。
func (h *KeysHandler) updateGrant(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "grantID")
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	var req struct {
		RequirePageConfirm *bool `json:"requirePageConfirm"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		httpx.Fail(w, r, httpx.Err(errcode.InvalidJSON))
		return
	}
	// ⚠️ 指针：缺省不能当成 false —— 那样一个空请求体会悄悄关掉页面确认。
	if req.RequirePageConfirm == nil {
		httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, "requirePageConfirm 必填"))
		return
	}
	uid, _ := h.userIDs(r.Context())
	g, err := h.svc.SetRequirePageConfirm(r.Context(), uid, id, *req.RequirePageConfirm)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, toGrant(g))
}
