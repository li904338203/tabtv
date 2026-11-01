// Package transport 暴露身份域的 HTTP 接口。
//
// handler 保持**薄**：只做绑定、调用、写响应（08 §3）。
// 不出现 SQL、不出现业务分支判断。
package transport

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/li904338203/tabtv/backend/internal/identity/app"
	"github.com/li904338203/tabtv/backend/internal/identity/domain"
	"github.com/li904338203/tabtv/backend/internal/identity/token"
	"github.com/li904338203/tabtv/backend/internal/identity/verify"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

type Handler struct {
	svc    *app.Service
	tokens *token.Service
}

func NewHandler(svc *app.Service, tokens *token.Service) *Handler {
	return &Handler{svc: svc, tokens: tokens}
}

func (h *Handler) Mount(r chi.Router) {
	r.Route("/auth", func(r chi.Router) {
		r.Post("/code", h.sendCode)
		r.Post("/register", h.register)
		r.Post("/login", h.login)
		r.Post("/password/reset", h.resetPassword)
		r.Post("/refresh", h.refresh)
		// 退出与查询自身需要已认证。
		r.Group(func(r chi.Router) {
			r.Use(h.RequireUser)
			r.Post("/logout", h.logout)
			r.Get("/me", h.me)
			r.Put("/profile", h.updateProfile)
		})
	})
}

func decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(dst); err != nil {
		httpx.Fail(w, r, httpx.Err(errcode.InvalidJSON))
		return false
	}
	return true
}

type accountReq struct {
	Phone string `json:"phone"`
	Email string `json:"email"`
}

func (h *Handler) sendCode(w http.ResponseWriter, r *http.Request) {
	var req struct {
		accountReq
		Purpose string `json:"purpose"`
	}
	if !decode(w, r, &req) {
		return
	}
	var p verify.Purpose
	switch req.Purpose {
	case "register":
		p = verify.PurposeRegister
	case "login":
		p = verify.PurposeLogin
	case "reset_password":
		p = verify.PurposeResetPassword
	default:
		httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, "purpose 取值非法"))
		return
	}
	if err := h.svc.SendCode(r.Context(), req.Phone, req.Email, p); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	// 不回显验证码，也不回显账号是否存在。
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) register(w http.ResponseWriter, r *http.Request) {
	var req struct {
		accountReq
		Password string `json:"password"`
		Nickname string `json:"nickname"`
		Code     string `json:"code"`
	}
	if !decode(w, r, &req) {
		return
	}
	pair, err := h.svc.Register(r.Context(), app.RegisterInput{
		Phone: req.Phone, Email: req.Email, Password: req.Password,
		Nickname: req.Nickname, Code: req.Code,
	}, clientIP(r), r.UserAgent())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, pair)
}

func (h *Handler) login(w http.ResponseWriter, r *http.Request) {
	var req struct {
		accountReq
		Password string `json:"password"`
		Code     string `json:"code"`
	}
	if !decode(w, r, &req) {
		return
	}
	pair, err := h.svc.Login(r.Context(), app.LoginInput{
		Phone: req.Phone, Email: req.Email, Password: req.Password, Code: req.Code,
	}, clientIP(r), r.UserAgent())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, pair)
}

func (h *Handler) resetPassword(w http.ResponseWriter, r *http.Request) {
	var req struct {
		accountReq
		Code     string `json:"code"`
		Password string `json:"password"`
	}
	if !decode(w, r, &req) {
		return
	}
	if err := h.svc.ResetPassword(r.Context(), app.ResetInput{
		Phone: req.Phone, Email: req.Email, Code: req.Code, Password: req.Password,
	}); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) refresh(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RefreshToken string `json:"refreshToken"`
	}
	if !decode(w, r, &req) {
		return
	}
	pair, err := h.svc.Refresh(r.Context(), req.RefreshToken, clientIP(r), r.UserAgent())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, pair)
}

func (h *Handler) logout(w http.ResponseWriter, r *http.Request) {
	// access token 的 jti 与 refresh 不同，这里撤销的是当前会话记录。
	claims := ClaimsFrom(r.Context())
	if claims == nil {
		httpx.Fail(w, r, httpx.Err(errcode.Unauthenticated))
		return
	}
	if err := h.svc.Logout(r.Context(), claims.Session()); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type meResp struct {
	ID                 string `json:"id"`
	Nickname           string `json:"nickname"`
	Email              string `json:"email,omitempty"`
	Phone              string `json:"phone,omitempty"`
	Status             string `json:"status"`
	IsAiWatermarkOff   bool   `json:"isAiWatermarkOff"`
	DefaultWorkspaceID string `json:"defaultWorkspaceId"`
}

func (h *Handler) me(w http.ResponseWriter, r *http.Request) {
	claims := ClaimsFrom(r.Context())
	uid, _ := claims.SubjectID()
	u, ws, err := h.svc.Me(r.Context(), uid)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	// ⚠️ 账号一律脱敏返回；ID 序列化为 string（GO-34）。
	httpx.JSON(w, http.StatusOK, meResp{
		ID:                 itoa(u.ID),
		Nickname:           u.Nickname,
		Email:              maskIfSet(u.Email, domain.MaskEmail),
		Phone:              maskIfSet(u.Phone, domain.MaskPhone),
		Status:             string(u.Status),
		IsAiWatermarkOff:   u.IsWatermarkOff,
		DefaultWorkspaceID: itoa(ws),
	})
}

func clientIP(r *http.Request) string {
	if v := r.Header.Get("X-Forwarded-For"); v != "" {
		if i := strings.IndexByte(v, ','); i > 0 {
			return strings.TrimSpace(v[:i])
		}
		return strings.TrimSpace(v)
	}
	return r.RemoteAddr
}

func maskIfSet(s string, f func(string) string) string {
	if s == "" {
		return ""
	}
	return f(s)
}
