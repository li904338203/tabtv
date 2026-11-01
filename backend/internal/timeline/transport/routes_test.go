package transport

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
)

// TestRoutes_整份保存挂在默认闸后面（SC-AC-66 后端部分）。
//
// 组装根给 `guard` 的是 RequireWorkspace + GuardByMethod，而 GuardByMethod 让 PUT 要 edit、
// 只读成员（viewer）没有 —— 那一半由 collaboration 的 TestGuardByMethod 钉住。这里钉另一半：
// 新路由确实挂在 `guard` 那一组里（挂到放宽到 view 的渲染组，或者忘了挂中间件，只读成员就能改时间线）。
func TestRoutes_整份保存挂在默认闸后面(t *testing.T) {
	pass := func(next http.Handler) http.Handler { return next }
	denied := false
	guard := func(http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			denied = true
			w.WriteHeader(http.StatusForbidden)
		})
	}
	r := chi.NewRouter()
	NewHandler(nil, func(*http.Request) int64 { return 1 }).Mount(r, pass, guard, pass)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/timelines/1/content", nil))
	if !denied || rec.Code != http.StatusForbidden {
		t.Fatalf("PUT /timelines/{id}/content 应当经过默认闸，实际状态 %d（经过闸：%v）", rec.Code, denied)
	}
}

// 排版接口挂在放宽到 view 的那一组（只读成员打开剪辑台也要看得到字幕与文字），不经过默认闸。
func TestRoutes_排版接口只要查看权限(t *testing.T) {
	pass := func(next http.Handler) http.Handler { return next }
	guarded := false
	guard := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			guarded = true
			next.ServeHTTP(w, r)
		})
	}
	r := chi.NewRouter()
	NewHandler(nil, func(*http.Request) int64 { return 1 }).Mount(r, pass, guard, pass)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/smartcut/typeset", nil))
	if guarded || rec.Code == http.StatusNotFound || rec.Code == http.StatusMethodNotAllowed {
		t.Fatalf("排版接口应当挂在 view 组（不经默认闸），实际状态 %d、经过默认闸 %v", rec.Code, guarded)
	}
}

// 成片下载链接要 export（把内容取出去，SC-AC-27）：在默认闸（GET 只给 view）之外再收紧一道。
// 这里默认闸放行、请求不带工作区上下文：多出来的那道动作闸照样拦下（拿不到上下文一律拒绝）。
func TestRoutes_成片下载另要导出权限(t *testing.T) {
	pass := func(next http.Handler) http.Handler { return next }
	r := chi.NewRouter()
	NewHandler(nil, func(*http.Request) int64 { return 1 }).Mount(r, pass, pass, pass)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/renders/1/download", nil))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("下载链接应另过 export 闸，实际 %d", rec.Code)
	}
}
