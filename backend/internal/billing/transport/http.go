// Package transport 暴露积分账本的用户端接口。
package transport

import (
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	billingapp "github.com/li904338203/tabtv/backend/internal/billing/app"
	collabtransport "github.com/li904338203/tabtv/backend/internal/collaboration/transport"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

type Handler struct {
	svc *billingapp.Service
	// userID 取当前用户（下单要记"谁下的单"）。
	userID func(*http.Request) int64
	// devPayOn 开发支付开关，见 SetDevPay。
	devPayOn bool
	// payGw 支付域端口（P11-02）。没注入时 /pay 与 /sync 一律 404。
	payGw PayGateway
}

func NewHandler(svc *billingapp.Service, userID func(*http.Request) int64) *Handler {
	if userID == nil {
		userID = func(*http.Request) int64 { return 0 }
	}
	return &Handler{svc: svc, userID: userID}
}

func (h *Handler) Mount(r chi.Router, requireUser, requireWorkspace func(http.Handler) http.Handler) {
	r.Group(func(r chi.Router) {
		r.Use(requireUser)
		r.Use(requireWorkspace)
		r.Get("/credits", h.credits)
		r.Get("/credits/transactions", h.transactions)
	})
}

func (h *Handler) workspace(w http.ResponseWriter, r *http.Request) (int64, bool) {
	ws, ok := collabtransport.WorkspaceFrom(r.Context())
	if !ok {
		httpx.Fail(w, r, httpx.Err(errcode.PermissionDenied))
		return 0, false
	}
	// 个人工作区的计费主体就挂在工作区上；协作域已在中间件里校验过成员资格。
	return ws.PayerAccountID, true
}

func (h *Handler) credits(w http.ResponseWriter, r *http.Request) {
	payer, ok := h.workspace(w, r)
	if !ok {
		return
	}
	balance, err := h.svc.Balance(r.Context(), payer)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	grants, err := h.svc.Grants(r.Context(), payer)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	type grantResp struct {
		ID        string  `json:"id"`
		Source    string  `json:"source"`
		Amount    int     `json:"amount"`
		Remaining int     `json:"remaining"`
		ExpireAt  *string `json:"expireAt"`
	}
	items := make([]grantResp, 0, len(grants))
	for _, g := range grants {
		gr := grantResp{ID: strconv.FormatInt(g.ID, 10), Source: g.Source,
			Amount: g.Amount, Remaining: g.Remaining}
		if g.ExpireAt != nil {
			s := g.ExpireAt.UTC().Format(time.RFC3339)
			gr.ExpireAt = &s
		}
		items = append(items, gr)
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"balance": balance, "grants": items})
}

func (h *Handler) transactions(w http.ResponseWriter, r *http.Request) {
	payer, ok := h.workspace(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	var cursor int64
	if c := q.Get("cursor"); c != "" {
		cursor, _ = strconv.ParseInt(c, 10, 64)
	}
	rows, hasMore, err := h.svc.Transactions(r.Context(), payer, limit, cursor)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	type item struct {
		ID           string `json:"id"`
		Type         string `json:"type"`
		Change       int    `json:"change"`
		BalanceAfter int    `json:"balanceAfter"`
		RefType      string `json:"refType,omitempty"`
		RefID        string `json:"refId,omitempty"`
		Reason       string `json:"reason,omitempty"`
		CreatedAt    string `json:"createdAt"`
		// 展示用的关联（2026-11-01，见 repo.ListTransactions）：关不上就不出现这几个字段。
		ProjectID      string `json:"projectId,omitempty"`
		ProjectName    string `json:"projectName,omitempty"`
		ProjectDeleted bool   `json:"projectDeleted,omitempty"`
		OperationCode  string `json:"operationCode,omitempty"`
		OperationName  string `json:"operationName,omitempty"`
		ModelName      string `json:"modelName,omitempty"`
		RefTitle       string `json:"refTitle,omitempty"`
	}
	items := make([]item, 0, len(rows))
	for _, t := range rows {
		it := item{
			ID: strconv.FormatInt(t.ID, 10), Type: t.Type, Change: t.Change,
			BalanceAfter: t.BalanceAfter, RefType: t.RefType, RefID: t.RefID,
			Reason: t.Reason, CreatedAt: t.CreatedAt.UTC().Format(time.RFC3339),
			ProjectName: t.ProjectName, ProjectDeleted: t.ProjectDeleted,
			OperationCode: t.OperationCode, OperationName: t.OperationName,
			ModelName: t.ModelName, RefTitle: t.RefTitle,
		}
		if t.ProjectID != 0 {
			it.ProjectID = strconv.FormatInt(t.ProjectID, 10)
		}
		items = append(items, it)
	}
	var next *string
	if hasMore && len(items) > 0 {
		s := items[len(items)-1].ID
		next = &s
	}
	httpx.JSON(w, http.StatusOK, map[string]any{
		"items": items, "nextCursor": next, "hasMore": hasMore,
	})
}
