package desktop

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/li904338203/tabtv/backend/internal/platform/resources"
)

// 项目包两个端点走真实装配（本机身份、工作区闸、multipart、长超时清单）的冒烟（TT-T-33）。
// 内容层面的往返、Key 不外泄、恶意包与回滚由 internal/projectpkg/integration 覆盖。
func TestBoot_项目包导出再导入(t *testing.T) {
	if _, err := resources.Postgres(); err != nil {
		t.Skipf("没有 PG 二进制：%v", err)
	}
	t.Setenv("TABTV_DATA_DIR", t.TempDir())
	rt, err := Boot(context.Background(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("启动失败：%v", err)
	}
	defer rt.Close(context.Background())

	serve := func(req *http.Request) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		rt.Handler.ServeHTTP(rec, req)
		return rec
	}
	body, _ := json.Marshal(map[string]any{"name": "要导出的项目"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := serve(req)
	var proj map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &proj)
	if rec.Code/100 != 2 || proj["id"] == nil {
		t.Fatalf("建项目 %d %s", rec.Code, rec.Body)
	}

	rec = serve(httptest.NewRequest(http.MethodPost, "/api/v1/projects/"+proj["id"].(string)+"/package", nil))
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "application/zip" ||
		!strings.Contains(rec.Header().Get("Content-Disposition"), "attachment") {
		t.Fatalf("导出 %d %v %s", rec.Code, rec.Header(), rec.Body)
	}
	pkg := rec.Body.Bytes()
	if _, err := zip.NewReader(bytes.NewReader(pkg), int64(len(pkg))); err != nil {
		t.Fatalf("导出的不是合法 zip：%v", err)
	}

	upload := func(content []byte) *httptest.ResponseRecorder {
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		w, _ := mw.CreateFormFile("file", "x.tabtv.zip")
		_, _ = w.Write(content)
		_ = mw.Close()
		req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/import", &buf)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		return serve(req)
	}
	rec = upload(pkg)
	var res map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &res)
	if rec.Code != http.StatusCreated || res["name"] != "要导出的项目（导入）" || res["projectId"] == proj["id"] {
		t.Fatalf("导入 %d %s", rec.Code, rec.Body)
	}
	if _, ok := res["projectId"].(string); !ok {
		t.Errorf("projectId 对外应为字符串：%v", res["projectId"])
	}
	// 不是 zip：400 并说明原因
	if rec = upload([]byte("not a zip")); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "zip") {
		t.Fatalf("坏包应 400：%d %s", rec.Code, rec.Body)
	}
	// 不存在的项目：404
	if rec = serve(httptest.NewRequest(http.MethodPost, "/api/v1/projects/123456789012345678/package", nil)); rec.Code != http.StatusNotFound {
		t.Fatalf("不存在的项目应 404：%d %s", rec.Code, rec.Body)
	}
}
