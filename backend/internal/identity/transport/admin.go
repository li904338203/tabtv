package transport

import (
	"context"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/li904338203/tabtv/backend/internal/identity/app"
	"github.com/li904338203/tabtv/backend/internal/identity/token"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

type permsKey struct{}

func (h *Handler) MountAdmin(r chi.Router) {
	r.Route("/admin", func(r chi.Router) {
		r.Post("/auth/login", h.adminLogin)
		// 续期不挂鉴权：来的时候 access 多半已经过期了（这正是要续期的原因），凭的是 refresh。
		r.Post("/auth/refresh", h.adminRefresh)

		r.Group(func(r chi.Router) {
			r.Use(h.RequireAdmin)
			r.Get("/auth/me", h.adminMe)
			// 退出要撤销服务端会话：只清前端存储的话，refresh 在服务端还能续期 30 天。
			r.Post("/auth/logout", h.adminLogout)

			// 封禁需要写权限。只读管理员调用这里必须 403（P02-02 负向验收）。
			r.With(h.RequirePermission("user:write")).
				Post("/users/{userID}/ban", h.banUser)
			r.With(h.RequirePermission("user:read")).
				Get("/users/{userID}", h.adminGetUser)
			r.With(h.RequirePermission("user:read")).
				Get("/users", h.searchUsers)

			// RBAC 管理（旧系统缺页面，GAP-06）。
			r.With(h.RequirePermission("rbac:read")).Get("/admins", h.listAdmins)
			r.With(h.RequirePermission("rbac:write")).Post("/admins", h.createAdmin)
			r.With(h.RequirePermission("rbac:write")).Put("/admins/{adminID}/status", h.setAdminStatus)
			r.With(h.RequirePermission("rbac:write")).Put("/admins/{adminID}/roles", h.setAdminRoles)
			r.With(h.RequirePermission("rbac:read")).Get("/roles", h.listRoles)
			r.With(h.RequirePermission("rbac:read")).Get("/permissions", h.listPermissions)
			r.With(h.RequirePermission("rbac:write")).Put("/roles/{roleCode}/permissions", h.setRolePermissions)

			// 认证配置：密钥三态（保留/替换/清除）。
			r.With(h.RequirePermission("auth:read")).Get("/auth-settings", h.getAuthSetting)
			r.With(h.RequirePermission("auth:write")).Put("/auth-settings", h.updateAuthSetting)

			// 业务审计台（GAP-24）。
			r.With(h.RequirePermission("audit:read")).Get("/audit-logs", h.auditLogs)
		})
	})
}

func (h *Handler) adminRefresh(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RefreshToken string `json:"refreshToken"`
	}
	if !decode(w, r, &req) {
		return
	}
	pair, err := h.svc.AdminRefresh(r.Context(), req.RefreshToken, clientIP(r), r.UserAgent())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, pair)
}

func (h *Handler) adminLogout(w http.ResponseWriter, r *http.Request) {
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

func (h *Handler) adminLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !decode(w, r, &req) {
		return
	}
	pair, err := h.svc.AdminLogin(r.Context(),
		app.AdminLoginInput{Username: req.Username, Password: req.Password},
		clientIP(r), r.UserAgent())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, pair)
}

func (h *Handler) adminMe(w http.ResponseWriter, r *http.Request) {
	claims := ClaimsFrom(r.Context())
	id, _ := claims.SubjectID()
	a, err := h.svc.AdminByID(r.Context(), id)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	perms := PermissionsFrom(r.Context())
	codes := make([]string, 0, len(perms))
	for c := range perms {
		codes = append(codes, c)
	}
	httpx.JSON(w, http.StatusOK, map[string]any{
		"id":          itoa(a.ID),
		"username":    a.Username,
		"status":      a.Status,
		"permissions": codes,
	})
}

func (h *Handler) banUser(w http.ResponseWriter, r *http.Request) {
	userID, err := pathID(r, "userID")
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	var req struct {
		Banned bool   `json:"banned"`
		Reason string `json:"reason"`
	}
	if !decode(w, r, &req) {
		return
	}
	// 高影响操作要留原因（02 §4）。
	if req.Banned && req.Reason == "" {
		httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, "封禁必须填写原因"))
		return
	}
	operator, _ := ClaimsFrom(r.Context()).SubjectID()
	if err := h.svc.BanUser(r.Context(), operator, userID, req.Banned, req.Reason); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) adminGetUser(w http.ResponseWriter, r *http.Request) {
	userID, err := pathID(r, "userID")
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	status, err := h.svc.UserStatus(r.Context(), userID)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]string{"id": itoa(userID), "status": status})
}

// RequireAdmin 校验管理员 access token 并装载权限集。
// ⚠️ 只认 KindAdmin：普通用户 token 绝不能进到这里（GO-02）。
func (h *Handler) RequireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw := bearer(r)
		if raw == "" {
			httpx.Fail(w, r, httpx.Err(errcode.Unauthenticated))
			return
		}
		claims, err := h.tokens.Parse(raw, token.KindAdmin, token.TypeAccess)
		if err != nil {
			switch err {
			case token.ErrExpired:
				httpx.Fail(w, r, httpx.Err(errcode.TokenExpired))
			case token.ErrWrongKind:
				// 用户 token 调管理接口会走到这里。
				httpx.Fail(w, r, httpx.Err(errcode.WrongTokenKind))
			default:
				httpx.Fail(w, r, httpx.Err(errcode.Unauthenticated))
			}
			return
		}
		id, err := claims.SubjectID()
		if err != nil {
			httpx.Fail(w, r, httpx.Err(errcode.Unauthenticated))
			return
		}
		a, err := h.svc.AdminByID(r.Context(), id)
		if err != nil {
			httpx.Fail(w, r, err)
			return
		}
		// token_version 比对：改密/停用后旧 token 立即失效。
		if a.TokenVersion != claims.TokenVersion {
			httpx.Fail(w, r, httpx.Err(errcode.TokenRevoked))
			return
		}

		perms, err := h.svc.AdminPermissions(r.Context(), id)
		if err != nil {
			httpx.Fail(w, r, err)
			return
		}
		ctx := context.WithValue(r.Context(), claimsKey{}, claims)
		ctx = context.WithValue(ctx, permsKey{}, perms)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// RequirePermission 判定具体权限码。
//
// ⚠️ 权限由**真实的 role_permission 关联行**决定，没有 super 通配捷径：
// 通配是不可审计的隐式规则（06 §1）。前端隐藏按钮只是体验，这里才是边界。
func (h *Handler) RequirePermission(code string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !PermissionsFrom(r.Context())[code] {
				httpx.Fail(w, r, httpx.Errf(errcode.PermissionDenied, "缺少权限: "+code))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func PermissionsFrom(ctx context.Context) map[string]bool {
	p, _ := ctx.Value(permsKey{}).(map[string]bool)
	return p
}
