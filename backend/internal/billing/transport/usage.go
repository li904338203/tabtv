package transport

import (
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/li904338203/tabtv/backend/internal/billing/repo"
	collabdomain "github.com/li904338203/tabtv/backend/internal/collaboration/domain"
	collabtransport "github.com/li904338203/tabtv/backend/internal/collaboration/transport"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// MountUsage 挂载工作区用量审计（P12-04；NEW-20）。
//
// ⚠️ 要 **manage** 而不是 view：这里回答的是「团队的积分被谁花了」——
// 那是管理动作。editor 能花钱不等于他该看得到同事花了多少
// （在一个按人头计费的团队里，这是个会引起摩擦的信息）。
//
// ⚠️ 显式用 `RequireAction(manage)` 而不是靠默认闸：GET 的默认是 view，
// 而 view 对这组接口太松。默认闸只保证"不会无人看管"，
// **更严的要求必须自己标出来**。
func (h *Handler) MountUsage(r chi.Router, requireUser, wsCtx func(http.Handler) http.Handler) {
	r.Group(func(r chi.Router) {
		r.Use(requireUser, wsCtx, collabtransport.RequireAction(collabdomain.ActionManage))
		r.Get("/workspaces/current/usage", h.usage)
		r.Get("/workspaces/current/usage/entries", h.usageEntries)
	})
}

// parseWindow 解析时间窗。
//
// ⚠️ 解析失败**当成没传**而不是报错（与后台订单检索同一条）：运营/管理员
// 从别处粘来的格式五花八门，为一个格式问题把整张报表挡掉不值 ——
// 少一个条件顶多结果多一些，而报错是什么都看不到。
func parseWindow(r *http.Request) repo.UsageFilter {
	var f repo.UsageFilter
	if v := r.URL.Query().Get("from"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			f.From = &t
		}
	}
	if v := r.URL.Query().Get("to"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			f.To = &t
		}
	}
	return f
}

func (h *Handler) usage(w http.ResponseWriter, r *http.Request) {
	payer, ok := h.workspace(w, r)
	if !ok {
		return
	}
	members, sum, err := h.svc.WorkspaceUsage(r.Context(), payer, parseWindow(r))
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	type memberResp struct {
		UserID   string `json:"userId"`
		Nickname string `json:"nickname"`
		Consumed int    `json:"consumed"`
		Refunded int    `json:"refunded"`
		Tasks    int    `json:"tasks"`
		LastAt   string `json:"lastAt"`
	}
	out := make([]memberResp, 0, len(members))
	for _, m := range members {
		out = append(out, memberResp{
			// ⚠️ userId 为 "0" 表示**没有操作者**的流水（过期扣减、系统调账）。
			// 这一行不能丢：丢掉的话各成员之和对不上总额，
			// 而那个差额没有任何人解释得了。
			UserID: strconv.FormatInt(m.UserID, 10), Nickname: m.Nickname,
			Consumed: m.Consumed, Refunded: m.Refunded, Tasks: m.Tasks,
			LastAt: m.LastAt.UTC().Format(time.RFC3339),
		})
	}
	httpx.JSON(w, http.StatusOK, map[string]any{
		"members": out,
		"summary": map[string]int{
			// ⚠️ 消耗与退回**分开给，不合并成净值**：合并之后用户拿这个数
			// 去对账本流水会对不上，而他无从知道差额是怎么来的。
			"consumed": sum.Consumed, "refunded": sum.Refunded,
			"tasks": sum.Tasks, "members": sum.Members,
		},
	})
}

func (h *Handler) usageEntries(w http.ResponseWriter, r *http.Request) {
	payer, ok := h.workspace(w, r)
	if !ok {
		return
	}
	var userID int64
	if v := r.URL.Query().Get("userId"); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, "userId 非法"))
			return
		}
		userID = id
	}
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 200 {
			limit = n
		}
	}
	var cursor int64
	if v := r.URL.Query().Get("cursor"); v != "" {
		cursor, _ = strconv.ParseInt(v, 10, 64)
	}

	list, err := h.svc.WorkspaceUsageDetail(r.Context(), payer, userID,
		parseWindow(r), limit, cursor)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	type entryResp struct {
		ID        string `json:"id"`
		UserID    string `json:"userId"`
		Nickname  string `json:"nickname"`
		Type      string `json:"type"`
		Change    int    `json:"change"`
		RefType   string `json:"refType"`
		RefID     string `json:"refId"`
		Reason    string `json:"reason"`
		CreatedAt string `json:"createdAt"`
	}
	out := make([]entryResp, 0, len(list))
	for _, e := range list {
		out = append(out, entryResp{
			ID:     strconv.FormatInt(e.TransactionID, 10),
			UserID: strconv.FormatInt(e.UserID, 10), Nickname: e.Nickname,
			Type: e.Type, Change: e.Change, RefType: e.RefType, RefID: e.RefID,
			Reason: e.Reason, CreatedAt: e.CreatedAt.UTC().Format(time.RFC3339),
		})
	}
	next := ""
	if len(out) == limit && len(out) > 0 {
		next = out[len(out)-1].ID
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"items": out, "nextCursor": next})
}
