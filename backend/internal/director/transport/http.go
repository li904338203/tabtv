// Package transport 暴露 3D 导演台 AI 搭景的 HTTP 接口（docs/agent/director，DRT-19）。
//
// 场景本身的接口在画布域（canvas/transport/director.go）；这里只有搭景与它的可用性。
package transport

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	collabtransport "github.com/li904338203/tabtv/backend/internal/collaboration/transport"
	"github.com/li904338203/tabtv/backend/internal/director/app"
	"github.com/li904338203/tabtv/backend/internal/director/domain"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// Handler 是导演台的 HTTP 处理器。
type Handler struct {
	svc *app.Service
	// userIDs 由身份域注入（本域不该知道 JWT claims 长什么样）。
	userIDs func(ctx context.Context) (int64, bool)
	// memberLevel 与生成受理同一个解析器（cmd/api 的 memberLevelOf）：报价看的会员等级只有一份。
	memberLevel func(*http.Request) int64
}

func NewHandler(svc *app.Service, userIDs func(ctx context.Context) (int64, bool)) *Handler {
	return &Handler{svc: svc, userIDs: userIDs, memberLevel: func(*http.Request) int64 { return 0 }}
}

// SetMemberLevelResolver 接上会员等级解析器。
func (h *Handler) SetMemberLevelResolver(f func(*http.Request) int64) {
	if f != nil {
		h.memberLevel = f
	}
}

// Mount 挂载路由。挂在工作区分组里：默认闸 GET → view，其余 → edit。
//
// ⚠️ 不标 generate（DRD-32）：AI 搭景免费，`generate` 是「会花用户的钱」的标记。
func (h *Handler) Mount(r chi.Router, requireUser, requireWorkspace func(http.Handler) http.Handler) {
	r.Group(func(r chi.Router) {
		r.Use(requireUser)
		r.Group(func(r chi.Router) {
			r.Use(requireWorkspace)
			r.Post("/projects/{projectId}/director-scenes/{sceneId}/build", h.build)
			r.Get("/director/build-config", h.buildConfig)
		})
	})
}

type changesResp struct {
	Added   []string `json:"added"`
	Updated []string `json:"updated"`
	Removed []string `json:"removed"`
}

type proposalResp struct {
	Scene    json.RawMessage `json:"scene"`
	Changes  changesResp     `json:"changes"`
	Defaults []string        `json:"defaults"`
}

type buildResp struct {
	BaseVersion int          `json:"baseVersion"`
	CallID      string       `json:"callId"`
	ModelCode   string       `json:"modelCode"`
	Proposal    proposalResp `json:"proposal"`
}

func toProposal(p domain.Proposal) proposalResp {
	nonNil := func(s []string) []string {
		if s == nil {
			return []string{}
		}
		return s
	}
	return proposalResp{Scene: p.Scene, Defaults: nonNil(p.Defaults),
		Changes: changesResp{Added: nonNil(p.Changes.Added), Updated: nonNil(p.Changes.Updated),
			Removed: nonNil(p.Changes.Removed)}}
}

func (h *Handler) build(w http.ResponseWriter, r *http.Request) {
	ws, ok := collabtransport.WorkspaceFrom(r.Context())
	if !ok {
		httpx.Fail(w, r, httpx.Err(errcode.PermissionDenied))
		return
	}
	uid, ok := h.userIDs(r.Context())
	if !ok {
		httpx.Fail(w, r, httpx.Err(errcode.Unauthenticated))
		return
	}
	pid, ok := pathID(w, r, "projectId")
	if !ok {
		return
	}
	sid, ok := pathID(w, r, "sceneId")
	if !ok {
		return
	}
	var req struct {
		Prompt            string   `json:"prompt"`
		ReferenceMediaIDs []string `json:"referenceMediaIds"`
		SceneVersion      int      `json:"sceneVersion"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, "请求体不是合法 JSON"))
		return
	}
	refs := make([]int64, 0, len(req.ReferenceMediaIDs))
	for i, s := range req.ReferenceMediaIDs {
		id, err := strconv.ParseInt(s, 10, 64)
		if err != nil || id <= 0 {
			httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, "referenceMediaIds 非法").
				WithDetails(map[string]any{"path": "referenceMediaIds[" + strconv.Itoa(i) + "]"}))
			return
		}
		refs = append(refs, id)
	}
	res, err := h.svc.Build(r.Context(), app.BuildInput{
		WorkspaceID: ws.WorkspaceID, ProjectID: pid, SceneID: sid, Role: ws.Role,
		Actor: app.Actor{UserID: uid}, MemberLevelID: h.memberLevel(r),
		Prompt: req.Prompt, ReferenceMediaIDs: refs, SceneVersion: req.SceneVersion,
	})
	if err != nil {
		fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, buildResp{BaseVersion: res.BaseVersion, CallID: strconv.FormatInt(res.CallID, 10),
		ModelCode: res.ModelCode, Proposal: toProposal(res.Proposal)})
}

type buildConfigResp struct {
	Available      bool    `json:"available"`
	Reason         *string `json:"reason"`
	UserFixable    bool    `json:"userFixable"`
	MaxImages      int     `json:"maxImages"`
	MaxPromptChars int     `json:"maxPromptChars"`
	Concurrency    int     `json:"concurrency"`
	PerHour        int     `json:"perHour"`
	UsedThisHour   int     `json:"usedThisHour"`
}

func (h *Handler) buildConfig(w http.ResponseWriter, r *http.Request) {
	uid, ok := h.userIDs(r.Context())
	if !ok {
		httpx.Fail(w, r, httpx.Err(errcode.Unauthenticated))
		return
	}
	c, err := h.svc.BuildConfig(r.Context(), uid, h.memberLevel(r))
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	out := buildConfigResp{Available: c.Available, UserFixable: c.UserFixable, MaxImages: c.MaxImages,
		MaxPromptChars: c.MaxPrompt, Concurrency: c.Concurrency, PerHour: c.PerHour, UsedThisHour: c.UsedThisHour}
	if c.Reason != "" {
		reason := c.Reason
		out.Reason = &reason
	}
	httpx.JSON(w, http.StatusOK, out)
}

// fail 回错误；限流时另设 Retry-After 头（同 MCP 的限流，MCP-I-06）。
func fail(w http.ResponseWriter, r *http.Request, err error) {
	var be *httpx.BizError
	if errors.As(err, &be) && (be.Code == errcode.RateLimited || be.Code == errcode.ConcurrencyLimited) {
		if secs, ok := be.Details["retry_after_seconds"].(int); ok && secs > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(secs))
		}
	}
	httpx.Fail(w, r, err)
}

func pathID(w http.ResponseWriter, r *http.Request, key string) (int64, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, key), 10, 64)
	if err != nil || id <= 0 {
		httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, key+" 非法"))
		return 0, false
	}
	return id, true
}
