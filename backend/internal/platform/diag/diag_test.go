package diag

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestInstallCommand_按发行版(t *testing.T) {
	cases := []struct{ release, want string }{
		{"NAME=\"Ubuntu\"\nID=ubuntu\nID_LIKE=debian\n", "sudo apt install gstreamer1.0-libav"},
		{"ID=debian\n", "sudo apt install gstreamer1.0-libav"},
		{"ID=fedora\n", "sudo dnf install gstreamer1-libav"},
		{"ID=arch\n", "sudo pacman -S gst-libav"},
		// 自己认不出、靠 ID_LIKE 归到亲缘发行版。
		{"ID=linuxmint\nID_LIKE=\"ubuntu debian\"\n", "sudo apt install gstreamer1.0-libav"},
		{"ID=\"opensuse-tumbleweed\"\nID_LIKE=\"opensuse suse\"\n", "sudo zypper install"},
		{"ID=nixos\n", "包管理器"},
		{"", "包管理器"},
	}
	for _, c := range cases {
		if got := InstallCommand(ParseOSRelease(c.release)); !strings.Contains(got, c.want) {
			t.Errorf("%q → %q，应包含 %q", c.release, got, c.want)
		}
	}
}

func TestVideoDecoder_三种结论(t *testing.T) {
	ubuntu := "ID=ubuntu\n"
	ok := func(context.Context, string, ...string) error { return nil }
	missing := func(context.Context, string, ...string) error { return errors.New("exit status 1") }
	noTool := func(context.Context, string, ...string) error { return exec.ErrNotFound }
	if r := VideoDecoder(context.Background(), ubuntu, ok); r.Status != "ok" {
		t.Fatalf("有插件：%+v", r)
	}
	if r := VideoDecoder(context.Background(), ubuntu, missing); r.Status != "fail" || !strings.Contains(r.Advice, "apt install") {
		t.Fatalf("缺插件应失败并给 apt 命令：%+v", r)
	}
	// 没有检测工具 ≠ 缺插件：说无法判断，不报失败。
	if r := VideoDecoder(context.Background(), ubuntu, noTool); r.Status != "warn" || !strings.Contains(r.Value, "无法判断") {
		t.Fatalf("没有 gst-inspect 应为无法判断：%+v", r)
	}
}

func TestVideoDecoder_超时不卡住(t *testing.T) {
	slow := func(ctx context.Context, _ string, _ ...string) error { <-ctx.Done(); return ctx.Err() }
	start := time.Now()
	r := VideoDecoder(context.Background(), "ID=arch\n", slow)
	if r.Status != "warn" || time.Since(start) > 5*time.Second {
		t.Fatalf("应在 3 秒左右以无法判断返回：%+v %v", r, time.Since(start))
	}
}
