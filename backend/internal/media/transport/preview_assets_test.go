package transport

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	mediaapp "github.com/li904338203/tabtv/backend/internal/media/app"
	"github.com/li904338203/tabtv/backend/internal/media/domain"
)

// 预览素材挂在媒体的工作区路由组里（要登录 + 工作区上下文），不是公开路由。
func TestPreviewAssets_挂在工作区路由组(t *testing.T) {
	deny := func(http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })
	}
	r := chi.NewRouter()
	NewHandler(nil, func(*http.Request) int64 { return 0 }, func(*http.Request) int64 { return 0 }).
		Mount(r, deny, deny, deny)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/media/123/preview-assets", nil))
	if rec.Code != http.StatusTeapot {
		t.Fatalf("应先过登录与工作区中间件：%d", rec.Code)
	}
}

// 形状与契约一致：四个字段缺失时是 null（不是缺键），ID 一律是字符串。
func TestPreviewAssetsResp_形状(t *testing.T) {
	empty, _ := json.Marshal(previewAssetsResp(mediaapp.PreviewAssets{MediaID: 9007199254740993,
		Status: map[string]string{"preview_proxy": "queued", "peaks": "none"}}))
	for _, want := range []string{`"mediaId":"9007199254740993"`, `"preview":null`, `"filmstrip":null`,
		`"peaks":null`, `"beats":null`, `"preview_proxy":"queued"`} {
		if !strings.Contains(string(empty), want) {
			t.Fatalf("缺少 %s：%s", want, empty)
		}
	}
	full, _ := json.Marshal(previewAssetsResp(mediaapp.PreviewAssets{MediaID: 1, Status: map[string]string{},
		Preview:   &mediaapp.PreviewProxy{MediaID: 9007199254740995, Width: 854, Height: 480, DurationMs: 3200},
		Filmstrip: &mediaapp.Filmstrip{FrameW: 160, FrameH: 90, IntervalMs: 1000, Cols: 10, Count: 12, Sheets: mediaapp.IDList{9007199254740997}},
		Peaks:     &mediaapp.Peaks{IntervalMs: 10, Values: "", NoAudio: true},
		Beats:     &mediaapp.Beats{BeatResult: domain.BeatResult{BPM: 120, BeatsMs: []int{0, 500}, DownbeatsMs: []int{0}, DownbeatApprox: true}},
	}))
	for _, want := range []string{`"preview":{"durationMs":3200,"height":480,"mediaId":"9007199254740995","width":854}`,
		`"sheets":["9007199254740997"]`, `"frameW":160`, `"noAudio":true`, `"bpm":120`, `"beatsMs":[0,500]`} {
		if !strings.Contains(string(full), want) {
			t.Fatalf("缺少 %s：%s", want, full)
		}
	}
}
