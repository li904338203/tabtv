package app

import (
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
