package desktop

import (
	"context"
	"net/http"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/li904338203/tabtv/backend/internal/platform/buildinfo"
	"github.com/li904338203/tabtv/backend/internal/platform/ffbin"
	"github.com/li904338203/tabtv/backend/internal/platform/migrate"
	"github.com/li904338203/tabtv/backend/internal/timeline/typeset"
	"github.com/li904338203/tabtv/backend/migrations"
)

// 环境自检（TT-F-35，TT-T-22）。每一项给「状态 + 现值 + 处置建议」：
// 只说「ffmpeg 不可用」的话，用户不知道下一步该做什么。
// Linux 播放依赖（TT-T-28）后续追加到 checks 里。

// Check 是一项自检结果。
type Check struct {
	Key    string `json:"key"`
	Label  string `json:"label"`
	Status string `json:"status"` // ok / warn / fail
	Value  string `json:"value"`
	Advice string `json:"advice,omitempty"`
}

func mountDiagnostics(r chi.Router, pool *pgxpool.Pool) {
	r.Get("/settings/diagnostics", func(w http.ResponseWriter, req *http.Request) {
		ctx, cancel := context.WithTimeout(req.Context(), 20*time.Second)
		defer cancel()
		writeJSON(w, map[string]any{"version": buildinfo.Version, "checks": runDiagnostics(ctx, pool)})
	})
}

func runDiagnostics(ctx context.Context, pool *pgxpool.Pool) []Check {
	out := []Check{dbCheck(ctx, pool)}
	out = append(out, binCheck(ctx, "ffmpeg", "ffmpeg", "渲染成片、视频封面、剪辑台预览都依赖它"))
	out = append(out, binCheck(ctx, "ffprobe", "ffprobe", "读取音视频时长与音轨"))
	if ffbin.Available("ffmpeg") {
		out = append(out, encoderCheck())
	}
	out = append(out, fontCheck())
	return out
}

func dbCheck(ctx context.Context, pool *pgxpool.Pool) Check {
	c := Check{Key: "database", Label: "内嵌数据库"}
	var ver string
	if err := pool.QueryRow(ctx, `show server_version`).Scan(&ver); err != nil {
		c.Status, c.Value, c.Advice = "fail", err.Error(), "重启应用；仍失败时打开日志目录查看 postgres.log"
		return c
	}
	applied, _ := migrate.AppliedVersion(ctx, pool)
	expected, _ := migrate.ExpectedVersion(migrations.FS, migrations.Dir)
	c.Status, c.Value = "ok", "PostgreSQL "+ver+" · 结构版本 "+itoa(applied)
	if applied < expected {
		c.Status, c.Advice = "fail", "数据库结构落后于应用版本，重启应用会自动升级"
	}
	return c
}

func binCheck(ctx context.Context, name, label, why string) Check {
	c := Check{Key: name, Label: label}
	path := ffbin.Path(name)
	if !ffbin.Available(name) {
		c.Status, c.Value = "fail", "找不到"
		c.Advice = why + "。安装包应已附带；如被安全软件删除请重新安装，或在「常规」里指定系统中的 " + name + " 路径"
		return c
	}
	out, err := exec.CommandContext(ctx, path, "-version").Output()
	if err != nil {
		c.Status, c.Value, c.Advice = "fail", err.Error(), "程序存在但无法运行（可能被系统拦截）"
		return c
	}
	first := strings.SplitN(string(out), "\n", 2)[0]
	c.Status, c.Value = "ok", first+" · "+path
	return c
}

func fontCheck() Check {
	c := Check{Key: "fonts", Label: "字幕字体"}
	dir := typeset.ResolveFontDir("")
	if _, err := typeset.LoadFonts(dir); err != nil {
		c.Status, c.Value, c.Advice = "fail", err.Error(), "带字幕或文字的成片会渲染失败；请重新安装应用"
		return c
	}
	c.Status, c.Value = "ok", filepath.Clean(dir)
	return c
}

// encoderCheck 报告选中的 H.264 编码器与被跳过的候选（TT-T-27）。
// ⚠️ 跳过原因要原样给：「nvenc 试编码失败：No capable devices found」能让用户知道是驱动问题，
// 一句「不可用」不能。
func encoderCheck() Check {
	c := Check{Key: "h264_encoder", Label: "H.264 编码器"}
	sel := ffbin.H264()
	var skipped []string
	for _, s := range sel.Skipped {
		if s.Reason == "当前 ffmpeg 没有编入" {
			continue // 随包构建本来就没有 x264 等，列出来只是噪声
		}
		skipped = append(skipped, s.Name+"："+s.Reason)
	}
	if sel.Encoder.Name == "" {
		c.Status, c.Value = "fail", "没有可用的编码器"
		c.Advice = "无法渲染成片与剪辑预览；请重新安装应用，或在「常规」里指定一个带 H.264 编码器的 ffmpeg"
		if len(skipped) > 0 {
			c.Value += "（" + strings.Join(skipped, "；") + "）"
		}
		return c
	}
	c.Status, c.Value = "ok", sel.Encoder.Name
	if len(skipped) > 0 {
		c.Value += " · 已跳过 " + strings.Join(skipped, "；")
	}
	if sel.Encoder.Name == "libopenh264" {
		c.Status = "warn"
		c.Advice = "没有可用的硬件编码器，使用软件编码 openh264：成片能出，但渲染较慢"
	}
	return c
}
