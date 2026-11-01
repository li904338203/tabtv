package transport

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/li904338203/tabtv/backend/internal/marketing/app"
	"github.com/li904338203/tabtv/backend/internal/marketing/repo"
)

// 路由层只测「挂没挂对」：只读成员 403 走真实工作区闸，在 marketing/transport/integration 里测
// （TestLibraryRoutes_只读成员写入403）——绕开真实闸的测试把闸整条删掉照样绿。

type nopProducts struct{}

func (nopProducts) InsertProduct(context.Context, repo.Product, int) (repo.Product, error) {
	return repo.Product{}, nil
}
func (nopProducts) GetProduct(context.Context, int64, int64) (repo.Product, error) {
	return repo.Product{}, repo.ErrNotFound
}
func (nopProducts) UpdateProduct(context.Context, repo.Product, int) (repo.Product, error) {
	return repo.Product{}, repo.ErrNotFound
}
func (nopProducts) SoftDeleteProduct(context.Context, int64, int64, int64) (repo.Product, error) {
	return repo.Product{}, repo.ErrNotFound
}
func (nopProducts) ListProducts(context.Context, repo.ProductFilter) ([]repo.Product, bool, error) {
	return nil, false, nil
}

// libraryRouter 挂公开路由 + 库路由；requireUser / wsGuard 用记录型假中间件。
func libraryRouter(enabled bool) (*chi.Mux, *int) {
	r, _ := newRouter(enabled)
	h := NewHandler(app.NewService(stubStore{}, stubCatalog{}, nil, app.Settings{}), enabled)
	h.SetLibrary(app.NewLibrary(nopProducts{}, nil, nil, app.LibrarySettings{}, func() int64 { return 1 }),
		func(*http.Request) int64 { return 1 })
	hits := 0
	pass := func(n http.Handler) http.Handler { return n }
	guard := func(n http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			hits++
			n.ServeHTTP(w, req)
		})
	}
	h.MountLibrary(r, pass, guard)
	return r, &hits
}

var libraryRoutes = []struct{ method, path string }{
	{http.MethodGet, "/marketing/products"},
	{http.MethodPost, "/marketing/products"},
	{http.MethodGet, "/marketing/products/123"},
	{http.MethodPut, "/marketing/products/123"},
	{http.MethodDelete, "/marketing/products/123"},
}

func TestLibraryRoutes_营销开关关闭404(t *testing.T) {
	r, hits := libraryRouter(false)
	for _, rt := range libraryRoutes {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(rt.method, rt.path, strings.NewReader("{}")))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("开关关闭时 %s %s 应 404，得 %d", rt.method, rt.path, rec.Code)
		}
	}
	if *hits != 0 {
		t.Fatal("开关关闭时不应走到工作区闸")
	}
}

// 库路由（完整路径、独立分组）与公开的 /marketing 子路由共存：两边都能命中，互不吞掉。
func TestLibraryRoutes_与公开路由共存(t *testing.T) {
	r, hits := libraryRouter(true)
	for _, rt := range libraryRoutes {
		before := *hits
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(rt.method, rt.path, strings.NewReader("{}")))
		if *hits != before+1 {
			t.Fatalf("%s %s 应经过工作区闸（命中库路由），得 %d：%s", rt.method, rt.path, rec.Code, rec.Body.String())
		}
		// 假闸没有装工作区上下文：handler 必须拒绝而不是放行（CD-59）。
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s %s 没有工作区上下文应 403，得 %d", rt.method, rt.path, rec.Code)
		}
	}
	for _, p := range publicPaths {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("挂上库路由后公开路由 %s 应仍 200，得 %d", p, rec.Code)
		}
	}
}
