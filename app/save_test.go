package app

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSanitizeName(t *testing.T) {
	for in, want := range map[string]string{
		"a.png": "a.png", "../../etc/passwd": "passwd", `..\..\x.mp4`: "x.mp4", "": "tabtv-download",
	} {
		if got := sanitizeName(in); got != want {
			t.Errorf("%q → %q，期望 %q", in, got, want)
		}
	}
}

func TestWriteFile_原子写入(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "out.txt")
	if err := writeFile(p, strings.NewReader("hello")); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if string(b) != "hello" {
		t.Fatalf("内容 %q", b)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("不应留下临时文件：%v", entries)
	}
}

func TestSaveMiddleware_先收内容再弹窗_空内容拒绝(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "cfg.json")
	asked := 0
	h := saveMiddleware(func(name string) (string, error) {
		asked++
		if name != "cfg.json" {
			t.Errorf("默认文件名 %q", name)
		}
		return target, nil
	})(http.NotFoundHandler())

	// 0 字节：报错，不弹窗、不写文件。
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, SavePath+"?name=cfg.json", strings.NewReader("")))
	if rec.Code != http.StatusBadRequest || asked != 0 {
		t.Fatalf("空内容应 400 且不弹窗：%d %s asked=%d", rec.Code, rec.Body, asked)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatal("空内容不应写出文件")
	}

	// 正常内容：写到选中的位置，内容一致。
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, SavePath+"?name=cfg.json", strings.NewReader(`{"a":1}`)))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"saved":true`) {
		t.Fatalf("保存失败：%d %s", rec.Code, rec.Body)
	}
	if b, _ := os.ReadFile(target); string(b) != `{"a":1}` {
		t.Fatalf("内容 %q", b)
	}

	// 取消：不写文件。
	h2 := saveMiddleware(func(string) (string, error) { return "", nil })(http.NotFoundHandler())
	rec = httptest.NewRecorder()
	h2.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, SavePath+"?name=x.json", strings.NewReader("x")))
	if !strings.Contains(rec.Body.String(), `"saved":false`) {
		t.Fatalf("取消应 saved=false：%s", rec.Body)
	}
}
