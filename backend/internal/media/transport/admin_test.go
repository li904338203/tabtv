package transport

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
)

func mountForTest(t *testing.T) (chi.Router, *bool) {
	t.Helper()
	var admin bool
	requireAdmin := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { admin = true; next.ServeHTTP(w, r) })
	}
	deny := func(http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })
	}
	r := chi.NewRouter()
	// svc 为 nil：下面每条用例都必须在碰到服务之前就被拦下，碰到了就会 panic。
	NewHandler(nil, func(*http.Request) int64 { return 0 }, func(*http.Request) int64 { return 1 }).
		Mount(r, deny, requireAdmin, deny)
	return r, &admin
}

// 管理端读取平台素材的路由必须存在且经过管理员鉴权。
// ⚠️ 起因：此前管理端只有上传没有读取 —— 传上去的 Banner 图、水印图在后台一张都看不到；
// 而用户端的 /media/{id} 对 admin token 是 401，管理端收到 401 会清会话。
func TestAdminMedia_读取路由经过管理员鉴权(t *testing.T) {
	r, admin := mountForTest(t)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/media/abc", nil))
	if !*admin {
		t.Fatalf("GET /admin/media/{id} 没有经过管理员鉴权（状态 %d）", rec.Code)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("非法 ID 应回 400，实际 %d", rec.Code)
	}
}

// 管理端上传只收平台级用途。用户侧用途（upload_reference 等）落在工作区 0 上没有意义，
// 不在入口拦的话会撞上表的 CHECK 约束 —— 表现为 500，看着像服务端坏了。
func TestAdminMedia_上传拒绝用户侧用途(t *testing.T) {
	r, _ := mountForTest(t)
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", "a.png")
	_, _ = fw.Write([]byte("\x89PNG\r\n\x1a\n"))
	_ = mw.WriteField("purpose", "upload_reference")
	_ = mw.Close()
	req := httptest.NewRequest(http.MethodPost, "/admin/media", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("用户侧用途应回 400，实际 %d：%s", rec.Code, rec.Body.String())
	}
}
