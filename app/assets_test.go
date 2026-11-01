package app

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"
)

func TestAPIMiddleware_按前缀分流(t *testing.T) {
	api := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(299) })
	assets := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(298) })
	h := APIMiddleware(api)(assets)
	for path, want := range map[string]int{
		"/api/v1/projects": 299,
		"/api/x":           299,
		"/api":             298, // 精确前缀是 "/api/"
		"/apix":            298,
		"/video/projects":  298,
		"/_nuxt/a.js":      298,
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != want {
			t.Errorf("%s → %d，期望 %d", path, rec.Code, want)
		}
	}
}

func TestSPAFallback(t *testing.T) {
	fsys := fstest.MapFS{"index.html": {Data: []byte("<html>spa</html>")}}
	h := SPAFallback(fsys)
	cases := []struct {
		method, path string
		code         int
	}{
		{http.MethodGet, "/video/projects", 200},
		{http.MethodGet, "/video/canvas/123", 200},
		{http.MethodGet, "/_nuxt/missing.js", 404}, // 带扩展名的找不到就是 404，不能回 HTML
		{http.MethodPost, "/video/projects", 404},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(c.method, c.path, nil))
		if rec.Code != c.code {
			t.Errorf("%s %s → %d，期望 %d", c.method, c.path, rec.Code, c.code)
		}
		if c.code == 200 && rec.Body.String() != "<html>spa</html>" {
			t.Errorf("%s 没有回 index.html", c.path)
		}
	}
}

func TestApp_未就绪时接口回503且重试后切换(t *testing.T) {
	calls := 0
	a := New(func(_ context.Context) (http.Handler, error) {
		calls++
		if calls == 1 {
			return nil, errors.New("数据库起不来")
		}
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }), nil
	}, "/data", "/data/logs")
	a.Startup(context.Background())

	rec := httptest.NewRecorder()
	a.API().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/x", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("启动失败时应回 503，得到 %d", rec.Code)
	}
	if e := a.GetStartupError(); e == nil || e.DataDir != "/data" || e.LogDir != "/data/logs" {
		t.Fatalf("错误详情不对：%+v", e)
	}
	if e := a.RetryStartup(); e != nil {
		t.Fatalf("重试应成功，得到 %+v", e)
	}
	rec = httptest.NewRecorder()
	a.API().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/x", nil))
	if rec.Code != 204 {
		t.Fatalf("重试成功后应切到新路由，得到 %d", rec.Code)
	}
}
