// Package transport 暴露生成任务的用户端接口。
package transport

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	collabdomain "github.com/li904338203/tabtv/backend/internal/collaboration/domain"
	collabtransport "github.com/li904338203/tabtv/backend/internal/collaboration/transport"
	genapp "github.com/li904338203/tabtv/backend/internal/generation/app"
	"github.com/li904338203/tabtv/backend/internal/generation/domain"
	"github.com/li904338203/tabtv/backend/internal/generation/repo"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// HeaderIdempotencyKey 幂等键请求头。
//
// ⚠️ 放在请求头而不是请求体：重试时整个请求体可能被重新序列化
// （字段顺序、空值处理都可能变），而头是调用方显式带上的那一个值。
const HeaderIdempotencyKey = "Idempotency-Key"

type Handler struct {
	svc *genapp.Service
	// memberLevel 解析当前付费主体的会员等级（P11-01 接入）。
	//
	// ⚠️ 不接的话恒为 0 —— 于是 `min_member_level_id` 这道门槛
	// **在受理路径上形同虚设**：报价时会拦（目录域那条路接了），
	// 而直接 POST /generations 照样能跑，白嫖高等级模型。
	memberLevel func(*http.Request) int64
}

func NewHandler(svc *genapp.Service) *Handler {
	return &Handler{svc: svc, memberLevel: func(*http.Request) int64 { return 0 }}
}

// SetMemberLevelResolver 注入会员等级解析器（P11-01 接入）。
func (h *Handler) SetMemberLevelResolver(f func(*http.Request) int64) {
	if f != nil {
		h.memberLevel = f
	}
}

func (h *Handler) Mount(r chi.Router, requireUser, requireWorkspace func(http.Handler) http.Handler,
	userIDOf func(*http.Request) int64) {
	r.Group(func(r chi.Router) {
		r.Use(requireUser)
		r.Use(requireWorkspace)
		// ⚠️ **会扣费的入口显式标 generate**（P12-03）。默认闸按方法推断出的是
		// edit，对 viewer 已经够用；标出来是为了让"哪些接口会花用户的钱"
		// 在代码里一眼可见，也让共享授权（view/edit/generate/export）
		// 将来能精确表达到这一层。
		r.With(collabtransport.RequireAction(collabdomain.ActionGenerate)).
			Post("/generations", h.create(userIDOf))
		// 重试＝新建一条 generation、重新校验重新扣费，与发起同一档。
		r.With(collabtransport.RequireAction(collabdomain.ActionGenerate)).
			Post("/generations/{generationID}/retry", h.retry(userIDOf))
		r.Get("/generations", h.list)
		r.Get("/generations/{generationID}", h.get)
		// 取消是**停掉**一个已经在跑的任务，不花钱 —— 按默认的 edit 即可。
		r.Post("/generations/{generationID}/cancel", h.cancel)
		r.Get("/generations/{generationID}/outputs", h.outputs)
		r.Get("/generations/{generationID}/outputs/{idx}", h.output)
	})
}

func (h *Handler) create(userIDOf func(*http.Request) int64) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ws, ok := collabtransport.WorkspaceFrom(r.Context())
		if !ok {
			httpx.Fail(w, r, httpx.Err(errcode.PermissionDenied))
			return
		}
		var req struct {
			ProjectID     string          `json:"projectId"`
			NodeID        string          `json:"nodeId"`
			OperationCode string          `json:"operation"`
			ModelCode     string          `json:"modelCode"`
			Count         int             `json:"count"`
			DurationSec   int             `json:"durationSec"`
			Text          string          `json:"text"`
			Params        json.RawMessage `json:"params"`
			Inputs        json.RawMessage `json:"inputs"`
			RetryOfID     string          `json:"retryOfId"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, "请求体不是合法 JSON"))
			return
		}
		projectID, _ := strconv.ParseInt(req.ProjectID, 10, 64)
		retryOf, _ := strconv.ParseInt(req.RetryOfID, 10, 64)

		res, err := h.svc.Accept(r.Context(), genapp.AcceptInput{
			WorkspaceID: ws.WorkspaceID, PayerAccountID: ws.PayerAccountID,
			UserID: userIDOf(r), ProjectID: projectID, NodeID: req.NodeID,
			OperationCode: req.OperationCode, ModelCode: req.ModelCode,
			IdempotencyKey: r.Header.Get(HeaderIdempotencyKey),
			Count:          req.Count, DurationSec: req.DurationSec, Text: req.Text,
			Params: req.Params, DeclaredInputs: req.Inputs, RetryOfID: retryOf,
			MemberLevelID: h.memberLevel(r),
		})
		if err != nil {
			httpx.Fail(w, r, err)
			return
		}
		// 重放返回 200 而不是 201：201 的语义是"刚刚创建了资源"，
		// 而这次什么都没创建 —— 客户端据此能分辨自己是不是重试命中了。
		status := http.StatusCreated
		if res.Replayed {
			status = http.StatusOK
		}
		httpx.JSON(w, status, toResp(res.Generation))
	}
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	ws, ok := collabtransport.WorkspaceFrom(r.Context())
	if !ok {
		httpx.Fail(w, r, httpx.Err(errcode.PermissionDenied))
		return
	}
	id, ok2 := pathID(w, r)
	if !ok2 {
		return
	}
	g, err := h.svc.Get(r.Context(), ws.WorkspaceID, id)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, toResp(g))
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	ws, ok := collabtransport.WorkspaceFrom(r.Context())
	if !ok {
		httpx.Fail(w, r, httpx.Err(errcode.PermissionDenied))
		return
	}
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	var cursor, projectID int64
	if c := q.Get("cursor"); c != "" {
		cursor, _ = strconv.ParseInt(c, 10, 64)
	}
	if p := q.Get("projectId"); p != "" {
		projectID, _ = strconv.ParseInt(p, 10, 64)
	}
	rows, hasMore, err := h.svc.List(r.Context(), ws.WorkspaceID, projectID, limit, cursor)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	items := make([]generationResp, 0, len(rows))
	for _, g := range rows {
		items = append(items, toResp(g))
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

func (h *Handler) cancel(w http.ResponseWriter, r *http.Request) {
	ws, ok := collabtransport.WorkspaceFrom(r.Context())
	if !ok {
		httpx.Fail(w, r, httpx.Err(errcode.PermissionDenied))
		return
	}
	id, ok2 := pathID(w, r)
	if !ok2 {
		return
	}
	var req struct {
		Reason string `json:"reason"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if err := h.svc.Cancel(r.Context(), ws.WorkspaceID, id, req.Reason); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// retry 重试失败的任务：**新建一行**，不是把原记录改回 queued。
func (h *Handler) retry(userIDOf func(*http.Request) int64) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ws, ok := collabtransport.WorkspaceFrom(r.Context())
		if !ok {
			httpx.Fail(w, r, httpx.Err(errcode.PermissionDenied))
			return
		}
		id, ok2 := pathID(w, r)
		if !ok2 {
			return
		}
		res, err := h.svc.Retry(r.Context(), ws.WorkspaceID, ws.PayerAccountID,
			userIDOf(r), id, r.Header.Get(HeaderIdempotencyKey))
		if err != nil {
			httpx.Fail(w, r, err)
			return
		}
		status := http.StatusCreated
		if res.Replayed {
			status = http.StatusOK
		}
		httpx.JSON(w, status, toResp(res.Generation))
	}
}

func (h *Handler) outputs(w http.ResponseWriter, r *http.Request) {
	ws, ok := collabtransport.WorkspaceFrom(r.Context())
	if !ok {
		httpx.Fail(w, r, httpx.Err(errcode.PermissionDenied))
		return
	}
	id, ok2 := pathID(w, r)
	if !ok2 {
		return
	}
	list, err := h.svc.Outputs(r.Context(), ws.WorkspaceID, id)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	type item struct {
		Index   int    `json:"index"`
		Type    string `json:"type"`
		MediaID string `json:"mediaId,omitempty"`
		Text    string `json:"text,omitempty"`
		// URL 是**受保护端点**的地址，不是对象存储直链（CD-16）。
		URL string `json:"url,omitempty"`
	}
	// ⚠️ 返回**全部产物**，不隐式只给 outputs[0]（GO-40）：
	// 多产物模型一次出四张图，只显示第一张等于把另外三张白白扣掉的积分丢了。
	out := make([]item, 0, len(list))
	for _, o := range list {
		it := item{Index: o.Idx, Type: o.OutputType, Text: o.TextContent}
		if o.MediaObjectID != 0 {
			it.MediaID = strconv.FormatInt(o.MediaObjectID, 10)
			// ⚠️ url 指向**真正出字节的那个端点**（媒体域），不指向
			// `/generations/{id}/outputs/{i}` —— 后者回的是元信息 JSON。
			// 指错的话，前端把它交给 useAuthenticatedMedia 会拿到一段 JSON
			// 当作图片去渲染，界面上只是一张裂图，谁也看不出原因。
			it.URL = domain.OutputMediaURL(o.MediaObjectID)
		}
		out = append(out, it)
	}
	httpx.JSON(w, http.StatusOK, out)
}

// output 受保护地读取单个产物。
//
// ⚠️ 这里只回媒体 ID 与类型，真正的字节流走媒体域的受保护端点 ——
// 产物的归属校验（workspace）在这里做过一次，媒体域再做一次，
// 两处都查得到才放行。
func (h *Handler) output(w http.ResponseWriter, r *http.Request) {
	ws, ok := collabtransport.WorkspaceFrom(r.Context())
	if !ok {
		httpx.Fail(w, r, httpx.Err(errcode.PermissionDenied))
		return
	}
	id, ok2 := pathID(w, r)
	if !ok2 {
		return
	}
	idx, err := strconv.Atoi(chi.URLParam(r, "idx"))
	if err != nil || idx < 0 {
		httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, "产物序号非法"))
		return
	}
	o, err := h.svc.Output(r.Context(), ws.WorkspaceID, id, idx)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	if o.OutputType == "text" {
		httpx.JSON(w, http.StatusOK, map[string]any{"type": "text", "text": o.TextContent})
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{
		"type":    o.OutputType,
		"mediaId": strconv.FormatInt(o.MediaObjectID, 10),
		// 前端用 useAuthenticatedMedia 取这个地址（不会带 Authorization 头的
		// <img src> 直接引用一律 401）。
		"mediaUrl": "/api/v1/media/" + strconv.FormatInt(o.MediaObjectID, 10),
	})
}

func pathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, "generationID"), 10, 64)
	if err != nil || id <= 0 {
		httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, "generationID 非法"))
		return 0, false
	}
	return id, true
}

type generationResp struct {
	ID          string  `json:"id"`
	ProjectID   string  `json:"projectId"`
	NodeID      string  `json:"nodeId,omitempty"`
	Status      string  `json:"status"`
	Progress    int     `json:"progress"`
	ResultType  string  `json:"resultType"`
	CreditsCost int     `json:"creditsCost"`
	RetryOfID   string  `json:"retryOfId,omitempty"`
	ErrorCode   string  `json:"errorCode,omitempty"`
	ErrorMsg    string  `json:"errorMessage,omitempty"`
	DeadlineAt  *string `json:"deadlineAt"`
	CreatedAt   string  `json:"createdAt"`
	UpdatedAt   string  `json:"updatedAt"`
}

func toResp(g repo.Generation) generationResp {
	out := generationResp{
		ID: strconv.FormatInt(g.ID, 10), ProjectID: strconv.FormatInt(g.ProjectID, 10),
		NodeID: g.NodeID, Status: g.Status, Progress: g.Progress,
		ResultType: g.ResultType, CreditsCost: g.CreditsCost,
		ErrorCode: g.ErrorCode, ErrorMsg: g.ErrorMessage,
		CreatedAt: g.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt: g.UpdatedAt.UTC().Format(time.RFC3339),
	}
	if g.RetryOfID != 0 {
		out.RetryOfID = strconv.FormatInt(g.RetryOfID, 10)
	}
	if g.DeadlineAt != nil {
		s := g.DeadlineAt.UTC().Format(time.RFC3339)
		out.DeadlineAt = &s
	}
	return out
}
