// Package transport 暴露内容域 HTTP 接口。
package transport

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	collabtransport "github.com/li904338203/tabtv/backend/internal/collaboration/transport"
	"github.com/li904338203/tabtv/backend/internal/content/app"
	"github.com/li904338203/tabtv/backend/internal/content/domain"
	"github.com/li904338203/tabtv/backend/internal/content/repo"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// MediaServer 由媒体域实现：把一份媒体按 ID 吐给客户端。
//
// ⚠️ 端口定义在这里而不是让内容域去 import 媒体域的 service：
// 内容域要的只是"把这个 ID 的图发出去"，它不该知道对象存储、缩略图、
// Range 请求那一套。**谁定义端口谁拥有契约**。
type MediaServer interface {
	// ServePublic 输出媒体字节。**不做归属校验** —— 调用方已经按
	// 作品的公开可见性判过了（这是这个方法与受保护下载端点唯一的区别）。
	ServePublic(w http.ResponseWriter, r *http.Request, mediaID int64, width int)
	// ServePublicCover 输出封面图：图片是它的缩略图，视频是第一帧，其余 404。同样不做归属校验。
	ServePublicCover(w http.ResponseWriter, r *http.Request, mediaID int64, width int)
	// RedirectPublic 同 ServePublic，但 302 到对象存储的短时效签名地址、不经 API 转发字节（2026-11-02）。
	// 只给 <img> / <video> 直接加载的匿名公开端点用。
	RedirectPublic(w http.ResponseWriter, r *http.Request, mediaID int64, width int)
	// RedirectPublicCover 是 ServePublicCover 的跳转版。
	RedirectPublicCover(w http.ResponseWriter, r *http.Request, mediaID int64, width int)
}

type Handler struct {
	svc     *app.Service
	userIDs func(ctx context.Context) (int64, bool)
	media   MediaServer
	sources WorkSourceGuard
}

func NewHandler(svc *app.Service, userIDs func(ctx context.Context) (int64, bool)) *Handler {
	return &Handler{svc: svc, userIDs: userIDs}
}

func (h *Handler) SetMedia(m MediaServer) { h.media = m }

// MountPublic 挂载**不需要登录**的内容接口。
//
// ⚠️ 精选墙与公开取图对未登录访客也要可用 —— 那是首页的门面。
// 挂在 requireUser 后面的话，没登录的人看到的是一张空白首页，
// 而这正是他决定要不要注册的那一眼。
func (h *Handler) MountPublic(r chi.Router) {
	r.Get("/showcase", h.showcase)
	// ⚠️ 公开取图**另开一条路**，不复用 `/generations/{id}/outputs/{i}`：
	// 那个端点按 workspace 卡归属，作者之外的人（含访客）一律取不到 ——
	// 首页那一栏会全是破图。可见性由「是否加精」划定，判定在 SQL 里，
	// 取消加精或下架后**立刻**取不到。
	r.Get("/showcase/{workID}/media", h.showcaseMedia)
	// 封面图（2026-11-01）：作品墙的视频卡显示第一帧，悬停才取视频本身 —— 此前取封面就把整段视频下完了。
	r.Get("/showcase/{workID}/cover", h.showcaseCover)
}

// MountUser 挂载需要登录的接口（首页聚合、消息中心）。
//
// ⚠️ 首页要 `wsCtx` 而不是带默认闸的 `guard`：它是 GET，默认闸给的是 view，
// 本来就够 —— 但首页在**没有工作区上下文**时也要能出（访客/刚注册），
// 所以"最近项目"那一块单独按有没有工作区来决定。
func (h *Handler) MountUser(r chi.Router, requireUser, wsCtx func(http.Handler) http.Handler) {
	r.Group(func(r chi.Router) {
		r.Use(requireUser)
		// 消息中心不属于任何工作区：个人通知按 userId、公告是全站的。
		r.Get("/notifications", h.messages)
		r.Get("/notifications/unread-count", h.unread)
		r.Post("/notifications/read", h.markRead)
		r.Post("/notifications/read-all", h.markAllRead)
	})
	r.Group(func(r chi.Router) {
		// 首页带工作区上下文时才给"最近项目"。
		r.Use(requireUser, wsCtx, collabtransport.RequireAction("view"))
		r.Get("/home", h.home)
	})
}

// ---------------------------------------------------------------- 首页

func (h *Handler) home(w http.ResponseWriter, r *http.Request) {
	ws, _ := collabtransport.WorkspaceFrom(r.Context())
	pos := domain.BannerPosition(r.URL.Query().Get("position"))
	if !pos.Valid() {
		// 默认 home。⚠️ 非法值**回落到默认**而不是报错：首页是进门的第一眼，
		// 为一个查询参数给用户一张错误页不值。
		pos = domain.PositionHome
	}
	data := h.svc.HomeFor(r.Context(), ws.WorkspaceID, pos, time.Now())

	banners := make([]map[string]any, 0, len(data.Banners))
	for _, b := range data.Banners {
		banners = append(banners, map[string]any{
			"id": itoa(b.ID), "title": b.Title, "linkUrl": b.LinkURL,
			"mediaObjectId": idOrEmpty(b.MediaObjectID),
			"coverMediaId":  idOrEmpty(b.CoverMediaID),
			"sort":          b.Sort,
		})
	}
	recent := make([]map[string]any, 0, len(data.Recent))
	for _, p := range data.Recent {
		// agent 字段**一定在**：没有会话时为 null（与 GET /projects 的列表项同形，AGT-45）。
		var agent any
		if a, ok := data.RecentAgents[p.ID]; ok {
			agent = map[string]any{
				"sessionId": itoa(a.SessionID), "title": a.Title, "state": a.State,
				"lastActiveAt": a.LastActiveAt.UTC().Format(time.RFC3339),
			}
		}
		recent = append(recent, map[string]any{
			"id": itoa(p.ID), "name": p.Name,
			"coverMediaId":        idOrEmpty(p.CoverMediaID),
			"displayCoverMediaId": idOrEmpty(p.DisplayCoverMediaID),
			"folderId":            idOrEmpty(p.FolderID),
			"updatedAt":           p.UpdatedAt.UTC().Format(time.RFC3339),
			"agent":               agent,
			// origin 一定在（null 也输出，与 Project.origin 同形，MKT-T-32）
			"origin": nullIfEmpty(p.Origin),
		})
	}
	httpx.JSON(w, http.StatusOK, map[string]any{
		"banners":  banners,
		"recent":   recent,
		"showcase": showcaseResp(data.Showcase),
	})
}

// ---------------------------------------------------------------- 精选

func showcaseResp(items []repo.ShowcaseItem) []map[string]any {
	out := make([]map[string]any, 0, len(items))
	for _, it := range items {
		out = append(out, map[string]any{
			"id": itoa(it.ID), "title": it.Title,
			// ⚠️ **不下发 mediaObjectId**：那是受保护端点的 ID，访客拿着它
			// 一律 401，而首页那一栏会全是破图。公开取图走
			// `/showcase/{id}/media` —— 这条路的可见性由加精划定。
			"hasMedia": it.MediaObjectID != 0,
			// 媒体类型由仓储按 content_type 推导（image|video|audio|other），
			// 前端据此选 <img>/<video>/占位，**不自己猜**。
			"type":      string(it.Type),
			"createdAt": it.CreatedAt.UTC().Format(time.RFC3339),
		})
	}
	return out
}

func (h *Handler) showcase(w http.ResponseWriter, r *http.Request) {
	limit := 24
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 100 {
			limit = n
		}
	}
	var cursor int64
	if v := r.URL.Query().Get("cursor"); v != "" {
		cursor, _ = strconv.ParseInt(v, 10, 64)
	}
	// ⚠️ type 非法回 InvalidArgument，**不回落成「全部」**：回落的话筛音频
	// 却出来一堆视频，而调用方看不出是参数写错了还是真的没有音频作品。
	kind, err := domain.ParseShowcaseKind(r.URL.Query().Get("type"))
	if err != nil {
		httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, err.Error()))
		return
	}
	items, err := h.svc.Showcase(r.Context(), limit, cursor, kind)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	next := ""
	if len(items) == limit && len(items) > 0 {
		next = itoa(items[len(items)-1].ID)
	}
	httpx.JSON(w, http.StatusOK, map[string]any{
		"items": showcaseResp(items), "nextCursor": next,
	})
}

func (h *Handler) showcaseMedia(w http.ResponseWriter, r *http.Request) {
	mediaID, width, ok := h.publicShowcaseMedia(w, r)
	if !ok {
		return
	}
	// 公开取图直连对象存储（2026-11-02）：可见性上面已判过，这里 302 到签名地址。
	h.media.RedirectPublic(w, r, mediaID, width)
}

// showcaseCover 输出作品的封面图（图片的缩略图 / 视频的第一帧）：作品墙的视频卡靠它显示画面，
// 悬停才去取视频本身（showcaseMedia）。可见性判定与 showcaseMedia 同一处（PublicMediaOf）。
func (h *Handler) showcaseCover(w http.ResponseWriter, r *http.Request) {
	mediaID, width, ok := h.publicShowcaseMedia(w, r)
	if !ok {
		return
	}
	h.media.RedirectPublicCover(w, r, mediaID, width)
}

// publicShowcaseMedia 判作品可见性并解析宽度：取图与取封面共用，可见性只判一处。
func (h *Handler) publicShowcaseMedia(w http.ResponseWriter, r *http.Request) (int64, int, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, "workID"), 10, 64)
	if err != nil || id <= 0 {
		httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, "workID 非法"))
		return 0, 0, false
	}
	mediaID, err := h.svc.PublicMediaOf(r.Context(), id)
	if err != nil {
		httpx.Fail(w, r, err)
		return 0, 0, false
	}
	if h.media == nil {
		httpx.Fail(w, r, httpx.Errf(errcode.Internal, "媒体服务未接入"))
		return 0, 0, false
	}
	// ⚠️ 列表默认给**缩略图**：公开墙一屏几十张，原图单张动辄几 MB ——
	// 滚几页就把标签页的内存吃光（旧系统实测 472 件约 800MB）。
	// 宽度只接受固定档位（媒体域会再校验一次）。
	width := 320
	if v := r.URL.Query().Get("w"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			width = n
		}
	}
	return mediaID, width, true
}

// ---------------------------------------------------------------- 消息中心

func (h *Handler) messages(w http.ResponseWriter, r *http.Request) {
	userID, _ := h.userIDs(r.Context())
	limit := 20
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 100 {
			limit = n
		}
	}
	// 游标是 (createdAt, id) 两段 —— 两张表的 id 顺序与时间不严格一致。
	var cursorAt *time.Time
	var cursorID int64
	if v := r.URL.Query().Get("cursorAt"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			cursorAt = &t
		}
	}
	if v := r.URL.Query().Get("cursorId"); v != "" {
		cursorID, _ = strconv.ParseInt(v, 10, 64)
	}

	list, err := h.svc.Messages(r.Context(), userID, limit, cursorAt, cursorID)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	items := make([]map[string]any, 0, len(list))
	for _, m := range list {
		items = append(items, map[string]any{
			"id": itoa(m.ID),
			// ⚠️ `source` 必须下发：前端标已读时要原样回传 ——
			// 两张表的 id 在同一个空间里无从区分。
			"source": string(m.Source), "type": m.Type,
			"title": m.Title, "content": m.Content, "linkUrl": m.LinkURL,
			"refType": m.RefType, "refId": m.RefID, "isRead": m.IsRead,
			"createdAt": m.CreatedAt.UTC().Format(time.RFC3339),
		})
	}
	resp := map[string]any{"items": items, "nextCursorAt": "", "nextCursorId": ""}
	if len(list) == limit && len(list) > 0 {
		last := list[len(list)-1]
		resp["nextCursorAt"] = last.CreatedAt.UTC().Format(time.RFC3339)
		resp["nextCursorId"] = itoa(last.ID)
	}
	httpx.JSON(w, http.StatusOK, resp)
}

func (h *Handler) unread(w http.ResponseWriter, r *http.Request) {
	userID, _ := h.userIDs(r.Context())
	n, err := h.svc.UnreadCount(r.Context(), userID)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]int{"unread": n})
}

func (h *Handler) markRead(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID     string `json:"id"`
		Source string `json:"source"`
	}
	if !decode(w, r, &req) {
		return
	}
	id, err := strconv.ParseInt(req.ID, 10, 64)
	if err != nil || id <= 0 {
		httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, "id 非法"))
		return
	}
	userID, _ := h.userIDs(r.Context())
	if err := h.svc.MarkRead(r.Context(), userID, id, req.Source); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) markAllRead(w http.ResponseWriter, r *http.Request) {
	userID, _ := h.userIDs(r.Context())
	n, err := h.svc.MarkAllRead(r.Context(), userID)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]int{"marked": n})
}

// ---------------------------------------------------------------- 工具

func decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(dst); err != nil {
		httpx.Fail(w, r, httpx.Err(errcode.InvalidJSON))
		return false
	}
	return true
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }

func idOrEmpty(v int64) string {
	if v == 0 {
		return ""
	}
	return itoa(v)
}

// nullIfEmpty 把空串输出成 JSON null（「未记录」与空串语义不同，§5.4）。
func nullIfEmpty(v string) any {
	if v == "" {
		return nil
	}
	return v
}
