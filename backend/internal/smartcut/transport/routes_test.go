package transport

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

// 这些路由都挂在「登录 + 工作区上下文 + 默认动作闸」之后（GET → view，其余 → edit）：
// 同步会写时间线（连线即入轨），必须落在 edit；素材条只读。
func TestRoutes_都过登录与工作区闸(t *testing.T) {
	var seen []string
	guard := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			seen = append(seen, r.Method+" "+r.URL.Path)
			w.WriteHeader(http.StatusTeapot)
		})
	}
	pass := func(next http.Handler) http.Handler { return next }
	r := chi.NewRouter()
	NewHandler(nil).Mount(r, pass, guard)
	for _, c := range []struct{ method, path string }{
		{http.MethodGet, "/projects/1/smartcuts"},
		{http.MethodPost, "/projects/1/smartcuts"},
		{http.MethodPost, "/projects/1/smartcuts/2/sync"},
		{http.MethodGet, "/projects/1/smartcuts/2/inputs"},
		// 语音识别（SCT-33）：在默认闸之外还标了 generate（按模型收费），这里只核对它同样先过工作区闸。
		{http.MethodPost, "/projects/1/smartcuts/2/transcribe"},
		// 字幕翻译（SCT-36）：免费，落在默认的 edit。
		{http.MethodPost, "/projects/1/smartcuts/2/translate"},
		// 片段级生成（SCT-37）：重拍与补镜另标 generate；生成记录与生成信息只读。
		{http.MethodPost, "/projects/1/smartcuts/2/clips/3/regenerate"},
		{http.MethodPost, "/projects/1/smartcuts/2/fill"},
		{http.MethodGet, "/projects/1/smartcuts/2/generations"},
		{http.MethodGet, "/projects/1/smartcuts/2/clips/3/origin"},
		// 去字幕（SCT-38）、生成配乐与音效（SCT-39）：按模型收费，另标 generate。
		{http.MethodPost, "/projects/1/smartcuts/2/clips/3/desubtitle"},
		{http.MethodPost, "/projects/1/smartcuts/2/music"},
		{http.MethodPost, "/projects/1/smartcuts/2/clips/3/sfx"},
	} {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(c.method, c.path, strings.NewReader(`{}`)))
		if rec.Code != http.StatusTeapot {
			t.Errorf("%s %s 应先过工作区闸：%d", c.method, c.path, rec.Code)
		}
	}
	if len(seen) != 13 {
		t.Fatalf("十三个路由都要经过：%v", seen)
	}
}
