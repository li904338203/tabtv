package transport

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
)

// 只读管理员（只有 agent:read）：读接口放行、全部写接口在权限闸上 403。
//
// 桩中间件按「当前管理员持有的权限」放行或拒绝，与 identity.RequirePermission 同语义；
// 放行的请求在闸后截住（418），不走到 handler —— 这里要钉住的是**每条路由挂的是哪个权限位**，
// 漏挂一条写路由的话，只读管理员绕开前端就能改技能，而界面上看不出任何异常。
func TestSkillAdmin_只读管理员403(t *testing.T) {
	granted := map[string]bool{"agent:read": true}
	requireAdmin := func(next http.Handler) http.Handler { return next }
	requirePerm := func(p string) func(http.Handler) http.Handler {
		return func(http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !granted[p] {
					w.WriteHeader(http.StatusForbidden)
					return
				}
				w.WriteHeader(http.StatusTeapot)
			})
		}
	}
	r := chi.NewRouter()
	(&AdminHandler{}).MountAdmin(r, requireAdmin, requirePerm)

	cases := []struct {
		method, path string
		want         int
	}{
		{http.MethodGet, "/admin/agent/tools", http.StatusTeapot},
		{http.MethodGet, "/admin/agent/model-options", http.StatusTeapot},
		{http.MethodGet, "/admin/agent/skills", http.StatusTeapot},
		{http.MethodGet, "/admin/agent/skills/1", http.StatusTeapot},
		{http.MethodGet, "/admin/agent/skills/1/references", http.StatusTeapot},
		{http.MethodPost, "/admin/agent/skills", http.StatusForbidden},
		{http.MethodPut, "/admin/agent/skills/1", http.StatusForbidden},
		{http.MethodPost, "/admin/agent/skills/1/enable", http.StatusForbidden},
		{http.MethodPost, "/admin/agent/skills/1/disable", http.StatusForbidden},
		{http.MethodPost, "/admin/agent/skills/1/references", http.StatusForbidden},
		{http.MethodPut, "/admin/agent/skills/1/references/2", http.StatusForbidden},
		{http.MethodDelete, "/admin/agent/skills/1/references/2?skillVersion=1", http.StatusForbidden},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(c.method, c.path, nil))
		if rec.Code != c.want {
			t.Errorf("%s %s 状态 %d，期望 %d", c.method, c.path, rec.Code, c.want)
		}
	}

	// 有写权限时写接口才过闸。
	granted["agent:write"] = true
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/admin/agent/skills/1", nil))
	if rec.Code != http.StatusTeapot {
		t.Fatalf("有 agent:write 时 PUT 仍被拒（%d）", rec.Code)
	}
}
