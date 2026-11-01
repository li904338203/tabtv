package transport

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/li904338203/tabtv/backend/internal/identity/token"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

type claimsKey struct{}

// RequireUser 校验用户 access token。
// ⚠️ 只认 user kind：用户 token 绝不能调用管理接口（GO-02）。
func (h *Handler) RequireUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw := bearer(r)
		if raw == "" {
			httpx.Fail(w, r, httpx.Err(errcode.Unauthenticated))
			return
		}
		claims, err := h.tokens.Parse(raw, token.KindUser, token.TypeAccess)
		if err != nil {
			switch err {
			case token.ErrExpired:
				httpx.Fail(w, r, httpx.Err(errcode.TokenExpired))
			case token.ErrWrongKind:
				httpx.Fail(w, r, httpx.Err(errcode.WrongTokenKind))
			default:
				httpx.Fail(w, r, httpx.Err(errcode.Unauthenticated))
			}
			return
		}

		// ⚠️ 签名有效 ≠ 凭证有效。必须再比对 token_version 与账号状态，
		// 否则封禁/改密之后旧 access token 仍能一路用到过期 —— 这正是 GAP-19。
		// 代价是每个请求多一次主键查询；后续可在 Redis 缓存 token_version
		// 并在封禁/改密时显式失效，但**不能为了省这次查询而不校验**。
		uid, err := claims.SubjectID()
		if err != nil {
			httpx.Fail(w, r, httpx.Err(errcode.Unauthenticated))
			return
		}
		state, err := h.svc.UserAuthState(r.Context(), uid)
		if err != nil {
			httpx.Fail(w, r, err)
			return
		}
		if !state.Active {
			httpx.Fail(w, r, httpx.Err(errcode.AccountBanned))
			return
		}
		if state.TokenVersion != claims.TokenVersion {
			httpx.Fail(w, r, httpx.Err(errcode.TokenRevoked))
			return
		}

		ctx := context.WithValue(r.Context(), claimsKey{}, claims)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func ClaimsFrom(ctx context.Context) *token.Claims {
	c, _ := ctx.Value(claimsKey{}).(*token.Claims)
	return c
}

func bearer(r *http.Request) string {
	v := r.Header.Get("Authorization")
	const p = "Bearer "
	if len(v) > len(p) && strings.EqualFold(v[:len(p)], p) {
		return v[len(p):]
	}
	return ""
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }

func pathID(r *http.Request, name string) (int64, error) {
	v := chi.URLParam(r, name)
	id, err := strconv.ParseInt(v, 10, 64)
	if err != nil || id <= 0 {
		return 0, httpx.Errf(errcode.InvalidArgument, "路径参数 "+name+" 非法")
	}
	return id, nil
}

// UserIDFrom 供其他领域从 context 取当前用户 ID。
// 暴露这个函数而不是 claims 结构：别的域不该知道 JWT 的内部形状。
func UserIDFrom(ctx context.Context) (int64, bool) {
	c := ClaimsFrom(ctx)
	if c == nil {
		return 0, false
	}
	id, err := c.SubjectID()
	return id, err == nil
}
