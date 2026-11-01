package transport

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
)

// 只读管理员（只有 agent:read）：读接口放行、全部写接口在权限闸上 403。
//
// 桩中间件按「当前管理员持有的权限」放行或拒绝，放行的请求在闸后截住（418），不走到 handler ——
// 这里钉住的是**每条路由挂的是哪个权限位**：漏挂一条写路由的话，只读管理员绕开前端就能
// 上线一条营销选项，而界面上看不出任何异常。12 条路由逐条列出，少注册一条也会红。
func TestAdminRoutes_读写权限位(t *testing.T) {
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

	reads := [][2]string{
		{http.MethodGet, "/admin/marketing/options"},
		{http.MethodGet, "/admin/marketing/options/1"},
		{http.MethodGet, "/admin/marketing/options/1/versions"},
		{http.MethodGet, "/admin/marketing/categories"},
	}
	writes := [][2]string{
		{http.MethodPost, "/admin/marketing/options"},
		{http.MethodPut, "/admin/marketing/options/1"},
		{http.MethodPost, "/admin/marketing/options/1/enable"},
		{http.MethodPost, "/admin/marketing/options/1/disable"},
		{http.MethodPut, "/admin/marketing/options/sort"},
		{http.MethodPost, "/admin/marketing/categories"},
		{http.MethodPut, "/admin/marketing/categories/1"},
		{http.MethodPut, "/admin/marketing/categories/sort"},
	}
	check := func(c [2]string, want int) {
		t.Helper()
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(c[0], c[1], nil))
		if rec.Code != want {
			t.Errorf("%s %s 状态 %d，期望 %d", c[0], c[1], rec.Code, want)
		}
	}
	for _, c := range reads {
		check(c, http.StatusTeapot)
	}
	for _, c := range writes {
		check(c, http.StatusForbidden)
	}
	// 没有 DELETE：选项与分类都只停用。
	check([2]string{http.MethodDelete, "/admin/marketing/options/1"}, http.StatusMethodNotAllowed)

	granted["agent:write"] = true
	for _, c := range writes {
		check(c, http.StatusTeapot)
	}
}

// 营销角色（MKT-T-36）7 条路由逐条钉住权限位：无 agent:read 全 403；只读管理员读放行、写 403；
// 有写权限全放行。没有 DELETE（MKT-F-120，只停用）—— 405 而不是被某条路由接住。
func TestPresetAdminRoutes_权限与无删除(t *testing.T) {
	granted := map[string]bool{}
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

	reads := [][2]string{
		{http.MethodGet, "/admin/marketing/preset-characters"},
		{http.MethodGet, "/admin/marketing/preset-characters/1"},
	}
	writes := [][2]string{
		{http.MethodPost, "/admin/marketing/preset-characters"},
		{http.MethodPut, "/admin/marketing/preset-characters/1"},
		{http.MethodPost, "/admin/marketing/preset-characters/1/enable"},
		{http.MethodPost, "/admin/marketing/preset-characters/1/disable"},
		{http.MethodPut, "/admin/marketing/preset-characters/sort"},
	}
	check := func(c [2]string, want int) {
		t.Helper()
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(c[0], c[1], nil))
		if rec.Code != want {
			t.Errorf("%s %s 状态 %d，期望 %d", c[0], c[1], rec.Code, want)
		}
	}
	for _, c := range append(append([][2]string{}, reads...), writes...) {
		check(c, http.StatusForbidden)
	}
	granted["agent:read"] = true
	for _, c := range reads {
		check(c, http.StatusTeapot)
	}
	for _, c := range writes {
		check(c, http.StatusForbidden)
	}
	granted["agent:write"] = true
	for _, c := range writes {
		check(c, http.StatusTeapot)
	}
	check([2]string{http.MethodDelete, "/admin/marketing/preset-characters/1"}, http.StatusMethodNotAllowed)
}
