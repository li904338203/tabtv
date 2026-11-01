package transport

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/li904338203/tabtv/backend/internal/canvas/app"
	collabdomain "github.com/li904338203/tabtv/backend/internal/collaboration/domain"
	collabtransport "github.com/li904338203/tabtv/backend/internal/collaboration/transport"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// QuickHandler 暴露创造页「直接生成」（AGT-42）：`POST /quick-generations`。
//
// ⚠️ 单独一个 handler 而不是挂进 Handler：它要会员等级解析器（与生成受理**同一个**，
// 少接一处的话那条路上的会员门槛形同虚设，§5.11），而项目 CRUD 用不到它。
type QuickHandler struct {
	svc         *app.Service
	userIDs     func(r *http.Request) int64
	memberLevel func(*http.Request) int64
}

func NewQuickHandler(svc *app.Service, userIDs func(r *http.Request) int64) *QuickHandler {
	return &QuickHandler{svc: svc, userIDs: userIDs, memberLevel: func(*http.Request) int64 { return 0 }}
}

// SetMemberLevelResolver 注入会员等级解析器（与 generation/transport 同一个）。
func (h *QuickHandler) SetMemberLevelResolver(f func(*http.Request) int64) {
	if f != nil {
		h.memberLevel = f
	}
}

// Mount 挂载路由。requireWorkspace 用组装根的 wsGuard（默认闸：POST → edit）。
//
// ⚠️ 会扣费，显式标 generate（§5.17）：只读成员直接打这个接口回 403。
func (h *QuickHandler) Mount(r chi.Router, requireUser, requireWorkspace func(http.Handler) http.Handler) {
	r.Group(func(r chi.Router) {
		r.Use(requireUser)
		r.Use(requireWorkspace)
		r.With(collabtransport.RequireAction(collabdomain.ActionGenerate)).
			Post("/quick-generations", h.create)
	})
}

type quickReq struct {
	Mode      string          `json:"mode"`
	ModelCode string          `json:"modelCode"`
	Operation string          `json:"operation"`
	Text      string          `json:"text"`
	Params    json.RawMessage `json:"params"`
	Inputs    []struct {
		Kind        string `json:"kind"`
		ReferenceID string `json:"referenceId"`
		Order       int    `json:"order"`
		Name        string `json:"name"`
		// URL 只为了**认出来并拒绝**：客户端给地址等于自己声明「我有权限」（GO-27）。
		URL string `json:"url"`
	} `json:"inputs"`
}

type quickErr struct {
	Code    int    `json:"code"`
	Name    string `json:"name,omitempty"`
	Message string `json:"message"`
}

type quickResp struct {
	ProjectID    string    `json:"projectId"`
	NodeID       string    `json:"nodeId"`
	GenerationID *string   `json:"generationId,omitempty"`
	Accepted     bool      `json:"accepted"`
	Replayed     bool      `json:"replayed"`
	CreditsCost  *int      `json:"creditsCost,omitempty"`
	Error        *quickErr `json:"error,omitempty"`
}

// HeaderIdempotencyKey 与生成受理同一个头。
const HeaderIdempotencyKey = "Idempotency-Key"

func (h *QuickHandler) create(w http.ResponseWriter, r *http.Request) {
	ws, ok := collabtransport.WorkspaceFrom(r.Context())
	if !ok {
		httpx.Fail(w, r, httpx.Err(errcode.PermissionDenied))
		return
	}
	var req quickReq
	if !decode(w, r, &req) {
		return
	}
	refs := make([]app.QuickRef, 0, len(req.Inputs))
	for _, in := range req.Inputs {
		if strings.TrimSpace(in.URL) != "" {
			httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, "附件只能按 referenceId 引用，不接受地址"))
			return
		}
		if in.Kind != "" && in.Kind != "reference" {
			httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, "直接生成的附件只支持上传的参考文件"))
			return
		}
		id, err := strconv.ParseInt(in.ReferenceID, 10, 64)
		if err != nil || id <= 0 {
			httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, "referenceId 非法"))
			return
		}
		refs = append(refs, app.QuickRef{ReferenceID: id, Order: in.Order, Name: in.Name})
	}

	res, err := h.svc.QuickGenerate(r.Context(), app.QuickInput{
		WorkspaceID: ws.WorkspaceID, PayerAccountID: ws.PayerAccountID,
		UserID: h.userIDs(r), Role: ws.Role, MemberLevelID: h.memberLevel(r),
		IdempotencyKey: r.Header.Get(HeaderIdempotencyKey),
		Mode:           req.Mode, ModelCode: req.ModelCode, Operation: req.Operation,
		Text: req.Text, Params: req.Params, Refs: refs,
	})
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	out := quickResp{
		ProjectID: idStr(res.ProjectID), NodeID: res.NodeID,
		Accepted: res.Accepted, Replayed: res.Replayed,
	}
	if res.GenerationID != 0 {
		s := idStr(res.GenerationID)
		out.GenerationID = &s
	}
	if res.Accepted && !res.Replayed {
		c := res.Credits
		out.CreditsCost = &c
	}
	if !res.Accepted && res.Error != "" {
		out.Error = &quickErr{Code: int(res.ErrorCode), Name: res.ErrorCode.Name(), Message: res.Error}
	}
	// 新建 201；重放 200（同生成受理：客户端据此分辨是不是重试命中了）。
	// ⚠️ 受理失败**仍是 201**：项目和节点确实建了，失败原因在 error 里、也写在节点上。
	status := http.StatusCreated
	if res.Replayed {
		status = http.StatusOK
	}
	httpx.JSON(w, status, out)
}
