package updater

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeReleases 是假的 Releases 服务：/latest 回 release，/files/<name> 回文件内容。
func fakeReleases(t *testing.T, status int, files map[string][]byte, sums string) (*httptest.Server, *int) {
	t.Helper()
	hits := 0
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if !strings.HasPrefix(r.Header.Get("User-Agent"), "tabtv/") {
			t.Errorf("User-Agent 应只带版本号：%q", r.Header.Get("User-Agent"))
		}
		switch {
		case r.URL.Path == "/latest":
			if status != http.StatusOK {
				w.WriteHeader(status)
				return
			}
			rel := Release{Tag: "v0.2.0", Body: "## 更新\n- 修复", HTMLURL: srv.URL + "/page"}
			for name := range files {
				rel.Assets = append(rel.Assets, Asset{Name: name, URL: srv.URL + "/files/" + name})
			}
			if sums != "" {
				rel.Assets = append(rel.Assets, Asset{Name: "SHA256SUMS", URL: srv.URL + "/files/SHA256SUMS"})
			}
			_ = json.NewEncoder(w).Encode(rel)
		case r.URL.Path == "/files/SHA256SUMS":
			_, _ = w.Write([]byte(sums))
		case strings.HasPrefix(r.URL.Path, "/files/"):
			_, _ = w.Write(files[strings.TrimPrefix(r.URL.Path, "/files/")])
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func sumOf(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

const asset = "tabtv-0.2.0-linux-amd64.AppImage"

func TestLatest_有新版与私有仓库(t *testing.T) {
	srv, _ := fakeReleases(t, http.StatusOK, map[string][]byte{asset: []byte("x")}, "")
	c := &Client{Feed: srv.URL + "/latest", Current: "0.1.0"}
	r, err := c.Latest(context.Background())
	if err != nil || r.Tag != "v0.2.0" || !Newer(c.Current, r.Tag) {
		t.Fatalf("应发现新版：%+v %v", r, err)
	}
	srv404, _ := fakeReleases(t, http.StatusNotFound, nil, "")
	if _, err := (&Client{Feed: srv404.URL + "/latest", Current: "0.1.0"}).Latest(context.Background()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("404 应为无法检查：%v", err)
	}
	srv500, _ := fakeReleases(t, http.StatusInternalServerError, nil, "")
	if _, err := (&Client{Feed: srv500.URL + "/latest"}).Latest(context.Background()); err == nil || errors.Is(err, ErrUnavailable) {
		t.Fatalf("500 应是普通错误：%v", err)
	}
}

func TestLatest_超时(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	defer slow.Close()
	c := &Client{Feed: slow.URL, HTTP: &http.Client{Timeout: 200 * time.Millisecond}}
	if _, err := c.Latest(context.Background()); err == nil {
		t.Fatal("超时应报错")
	}
}

func TestDownload_校验通过与篡改(t *testing.T) {
	payload := []byte("appimage-bytes")
	good := sumOf(payload) + "  " + asset + "\n"
	srv, _ := fakeReleases(t, http.StatusOK, map[string][]byte{asset: payload}, good)
	c := &Client{Feed: srv.URL + "/latest", Current: "0.1.0"}
	r, _ := c.Latest(context.Background())
	dir := t.TempDir()
	path, err := c.Download(context.Background(), r, asset, dir)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); string(b) != string(payload) || filepath.Base(path) != asset {
		t.Fatalf("内容或文件名不对：%s", path)
	}

	// 篡改一个字节：校验失败、不留任何文件。
	bad := []byte("appimage-byteS")
	srv2, _ := fakeReleases(t, http.StatusOK, map[string][]byte{asset: bad}, good)
	c2 := &Client{Feed: srv2.URL + "/latest", Current: "0.1.0"}
	r2, _ := c2.Latest(context.Background())
	dir2 := t.TempDir()
	if _, err := c2.Download(context.Background(), r2, asset, dir2); err == nil || !strings.Contains(err.Error(), "校验失败") {
		t.Fatalf("篡改后应校验失败：%v", err)
	}
	if left, _ := os.ReadDir(dir2); len(left) != 0 {
		t.Fatalf("失败后不应留下文件：%v", left)
	}
}

func TestDownload_缺校验文件或条目就放弃(t *testing.T) {
	payload := []byte("p")
	srv, _ := fakeReleases(t, http.StatusOK, map[string][]byte{asset: payload}, "")
	c := &Client{Feed: srv.URL + "/latest"}
	r, _ := c.Latest(context.Background())
	if _, err := c.Download(context.Background(), r, asset, t.TempDir()); err == nil {
		t.Fatal("没有 SHA256SUMS 应放弃")
	}
	other := sumOf(payload) + "  other-file\n"
	srv2, _ := fakeReleases(t, http.StatusOK, map[string][]byte{asset: payload}, other)
	c2 := &Client{Feed: srv2.URL + "/latest"}
	r2, _ := c2.Latest(context.Background())
	if _, err := c2.Download(context.Background(), r2, asset, t.TempDir()); err == nil {
		t.Fatal("SHA256SUMS 缺条目应放弃")
	}
	if _, err := c2.Download(context.Background(), r2, "no-such-asset", t.TempDir()); err == nil {
		t.Fatal("没有本平台包应报错")
	}
}
