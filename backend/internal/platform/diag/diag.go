// Package diag 是只与本机环境有关的自检项（TT-T-28）。
//
// 目前只有一项：Linux 上 WebKitGTK 播放 H.264 要 GStreamer 的 avdec_h264（gst-libav）。
// 缺它时视频区域是黑的、没有任何报错 —— 用户只会觉得「生成的视频坏了」，而产物其实完好。
// macOS（WKWebView）与 Windows（WebView2）自带解码，不受影响。
package diag

import (
	"bufio"
	"context"
	"errors"
	"os/exec"
	"strings"
	"time"
)

// Result 是一项检测结论。Status：ok / warn / fail / na（不适用）。
type Result struct {
	Status string
	Value  string
	Advice string
}

// Runner 执行一条命令，返回退出错误（测试替身用）。命令不存在时返回 exec.ErrNotFound。
type Runner func(ctx context.Context, name string, args ...string) error

func execRunner(ctx context.Context, name string, args ...string) error {
	if _, err := exec.LookPath(name); err != nil {
		return exec.ErrNotFound
	}
	return exec.CommandContext(ctx, name, args...).Run()
}

// VideoDecoder 判断 GStreamer 有没有 avdec_h264（只在 Linux 上有意义，调用方按平台决定是否调用）。
//
// ⚠️ 带 3 秒超时：GStreamer 第一次跑要建插件注册表，可能卡住自检页。
// ⚠️ gst-inspect-1.0 不存在 ≠ 缺插件：有的发行版装了运行库没装工具。这时说「无法判断」并照样给安装命令，
// 不报失败 —— 判成失败会让播放完全正常的人去装一堆不需要的包。
func VideoDecoder(ctx context.Context, osRelease string, run Runner) Result {
	if run == nil {
		run = execRunner
	}
	cmd := InstallCommand(ParseOSRelease(osRelease))
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	err := run(ctx, "gst-inspect-1.0", "avdec_h264")
	switch {
	case err == nil:
		return Result{Status: "ok", Value: "GStreamer avdec_h264 可用"}
	case errors.Is(err, exec.ErrNotFound):
		return Result{Status: "warn", Value: "无法判断（没有 gst-inspect-1.0）",
			Advice: "如果视频播放区域是黑的，请安装 H.264 解码插件：" + cmd}
	case ctx.Err() != nil:
		return Result{Status: "warn", Value: "无法判断（检测超时）",
			Advice: "稍后重新检测；如果视频播放区域是黑的，请安装：" + cmd}
	default:
		return Result{Status: "fail", Value: "缺少 H.264 解码插件（avdec_h264）",
			Advice: "视频能生成但在应用里播放是黑的。请安装后重启应用：" + cmd}
	}
}

// OSRelease 是 /etc/os-release 里用得到的字段。
type OSRelease struct {
	ID     string
	IDLike []string
}

// ParseOSRelease 解析 /etc/os-release 内容（纯函数）。
func ParseOSRelease(content string) OSRelease {
	var r OSRelease
	sc := bufio.NewScanner(strings.NewReader(content))
	for sc.Scan() {
		k, v, ok := strings.Cut(strings.TrimSpace(sc.Text()), "=")
		if !ok {
			continue
		}
		v = strings.ToLower(strings.Trim(strings.TrimSpace(v), `"'`))
		switch k {
		case "ID":
			r.ID = v
		case "ID_LIKE":
			r.IDLike = strings.Fields(v)
		}
	}
	return r
}

// InstallCommand 按发行版给安装命令；本身认不出时按 ID_LIKE 找亲缘发行版（linuxmint → ubuntu → debian）。
func InstallCommand(r OSRelease) string {
	for _, id := range append([]string{r.ID}, r.IDLike...) {
		switch id {
		case "debian", "ubuntu":
			return "sudo apt install gstreamer1.0-libav gstreamer1.0-plugins-good"
		case "fedora", "rhel", "centos":
			return "sudo dnf install gstreamer1-libav（需先启用 RPM Fusion 软件源）"
		case "arch", "manjaro":
			return "sudo pacman -S gst-libav"
		case "opensuse", "suse", "opensuse-leap", "opensuse-tumbleweed":
			return "sudo zypper install gstreamer-plugins-libav"
		}
	}
	return "用系统的包管理器安装 GStreamer 的 libav 插件（包名通常是 gstreamer1.0-libav 或 gst-libav）"
}
