package typeset

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveFontDir_随包资源优先于源码树(t *testing.T) {
	root := t.TempDir()
	fonts := filepath.Join(root, "fonts")
	if err := os.MkdirAll(fonts, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fonts, FileRegular), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TABTV_RESOURCES", root)
	t.Setenv(EnvFontDir, "")
	if got := ResolveFontDir(""); got != fonts {
		t.Fatalf("应取随包的 %s，得到 %s", fonts, got)
	}
}
