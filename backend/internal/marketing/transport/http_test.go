package transport

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/li904338203/tabtv/backend/internal/marketing/app"
	"github.com/li904338203/tabtv/backend/internal/marketing/domain"
	"github.com/li904338203/tabtv/backend/internal/marketing/repo"
)

type stubStore struct{ opts []repo.Option }

func (s stubStore) ListVisibleOptions(_ context.Context, f repo.VisibleFilter) ([]repo.Option, bool, error) {
	var out []repo.Option
	for _, o := range s.opts {
		if o.Kind == f.Kind && domain.Visible(o.Option, f.Now) {
			out = append(out, o)
		}
	}
	return out, false, nil
}

func (stubStore) VisibleRecommendedModels(context.Context, domain.Kind, time.Time) ([]string, error) {
	return nil, nil
}

func (s stubStore) GetOptionByCode(_ context.Context, code string) (repo.Option, error) {
	for _, o := range s.opts {
		if o.Code == code {
			return o, nil
		}
	}
	return repo.Option{}, repo.ErrNotFound
}

func (stubStore) GetVersionByCode(context.Context, string, int) (repo.OptionVersion, error) {
	return repo.OptionVersion{}, repo.ErrNotFound
}

func (stubStore) ListCategories(context.Context, domain.Kind) ([]repo.Category, error) {
	return nil, nil
}

type stubCatalog struct{}

func (stubCatalog) UsableModels(context.Context, string) ([]app.ModelRef, error) { return nil, nil }

// stubMedia 记下被要求输出的媒体 ID 与宽度，并照媒体域的样子设缓存头。
type stubMedia struct{ got []int64 }

func (m *stubMedia) ServePublic(w http.ResponseWriter, _ *http.Request, id int64, _ int) {
	m.got = append(m.got, id)
	w.Header().Set("Cache-Control", "public, max-age=300")
	w.WriteHeader(http.StatusOK)
}

// RedirectPublic 公开端点现在走跳转（2026-11-02）；桩照样记下 ID，断言不变。
func (m *stubMedia) RedirectPublic(w http.ResponseWriter, r *http.Request, id int64, width int) {
	m.ServePublic(w, r, id, width)
}

func newRouter(enabled bool) (*chi.Mux, *stubMedia) {
	live := domain.Option{Kind: domain.KindCreative, Code: "live_one", Name: "在投", Prompt: "绝密片段",
		CoverMediaIDs: []int64{77}, IsEnabled: true, Version: 1}
	svc := app.NewService(stubStore{opts: []repo.Option{{ID: 1, Option: live}}}, stubCatalog{}, nil,
		app.Settings{MaxProductImages: 20, MaxVariants: 4, AgentEnabled: true})
	h := NewHandler(svc, enabled)
	m := &stubMedia{}
	h.SetMedia(m)
	r := chi.NewRouter()
	h.MountPublic(r) // ⚠️ 不挂任何鉴权中间件：请求里也不带任何身份
	return r, m
}

var publicPaths = []string{
	"/marketing/config",
	"/marketing/options?kind=creative",
	"/marketing/options/live_one",
	"/marketing/options/live_one/covers/0",
}

func TestPublicRoutes_不需要登录(t *testing.T) {
	r, m := newRouter(true)
	for _, p := range publicPaths {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s 未登录应 200，得 %d：%s", p, rec.Code, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), "绝密片段") || strings.Contains(rec.Body.String(), `"prompt"`) {
			t.Fatalf("%s 响应里出现了片段：%s", p, rec.Body.String())
		}
	}
	if len(m.got) != 1 || m.got[0] != 77 {
		t.Fatalf("封面应按 code+下标解析出媒体 77：%v", m.got)
	}
	// 没有示例视频、下标越界、非法下标、不存在：一律 404。
	for _, p := range []string{"/marketing/options/live_one/preview", "/marketing/options/live_one/covers/1",
		"/marketing/options/live_one/covers/x", "/marketing/options/no_such/covers/0"} {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s 应 404，得 %d", p, rec.Code)
		}
	}
}

func TestPublicRoutes_开关关闭404(t *testing.T) {
	r, m := newRouter(false)
	for _, p := range append(publicPaths, "/marketing/options/live_one/preview") {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("开关关闭时 %s 应 404，得 %d", p, rec.Code)
		}
	}
	if len(m.got) != 0 {
		t.Fatal("开关关闭时不应输出任何媒体")
	}
}

func TestPublicOptionResp_无prompt字段(t *testing.T) {
	for _, typ := range []reflect.Type{reflect.TypeOf(publicOptionResp{}), reflect.TypeOf(recommendedModelResp{})} {
		for i := 0; i < typ.NumField(); i++ {
			f := typ.Field(i)
			tag := strings.ToLower(f.Tag.Get("json") + f.Name)
			if strings.Contains(tag, "prompt") || strings.Contains(tag, "mediaid") {
				t.Fatalf("%s 上不应有字段 %s（MKT-AC-06）", typ.Name(), f.Name)
			}
		}
	}
	// 配置响应：available + 维度 + 三类分类都在（空数组不是 null）。
	r, _ := newRouter(true)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/marketing/config", nil))
	var cfg map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &cfg); err != nil {
		t.Fatal(err)
	}
	data, _ := cfg["data"].(map[string]any)
	if data == nil {
		data = cfg
	}
	cats, _ := data["categories"].(map[string]any)
	for _, k := range []string{"creative", "hook", "style"} {
		if _, ok := cats[k].([]any); !ok {
			t.Fatalf("categories.%s 应为数组：%s", k, rec.Body.String())
		}
	}
	if data["available"] != true || data["maxProductImages"] != float64(20) {
		t.Fatalf("配置响应不对：%s", rec.Body.String())
	}
}
