package desktop

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"net/http"
	"path/filepath"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/li904338203/tabtv/backend/internal/platform/config"
	"github.com/li904338203/tabtv/backend/internal/platform/datadir"
	"github.com/li904338203/tabtv/backend/internal/platform/ffbin"
	"github.com/li904338203/tabtv/backend/internal/platform/objstore"
	"github.com/li904338203/tabtv/backend/internal/platform/settings"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// applySettings 把设置表里的值覆盖到装配用的配置上（TT-T-12）。
// 环境变量（SEEAITV_*）仍可覆盖，但只在设置表没设过该键时生效 —— 用户在界面上改过的以界面为准。
func applySettings(cfg *config.Config, st *settings.Store) {
	cfg.GenerationMaxRunningPerPayer = st.Int(settings.GenerationMaxRunning)
	if st.Bool(settings.ProviderAllowPrivate) {
		cfg.Dev.AllowPrivateNetwork = true
	}
	cfg.LogRetention = time.Duration(st.Int(settings.RequestLogRetention)) * 24 * time.Hour
	// ffmpeg / ffprobe 的路径（设置 → 常规，空 = 用随包的那一份）。
	ffbin.SetOverride("ffmpeg", st.String(settings.FFmpegPath))
	ffbin.SetOverride("ffprobe", st.String(settings.FFprobePath))
	if ep := st.String(settings.S3Endpoint); ep != "" {
		cfg.RelayStorage = config.Storage{
			Type: "s3", Endpoint: ep, Region: st.String(settings.S3Region), Bucket: st.String(settings.S3Bucket),
			AccessKey: st.String(settings.S3AccessKey), SecretKey: st.String(settings.S3SecretKey),
			PathStyle: st.Bool(settings.S3PathStyle),
			SignTTL:   time.Duration(st.Int(settings.S3SignTTLHours)) * time.Hour,
		}
		cfg.RelayPrefix = st.String(settings.S3Prefix)
	}
}

// mountSettingsRoutes：设置页的本机设置接口（TT-T-22）。模型、技能、提示词、营销等仍走原管理端点（dev/01 R-9）。
func mountSettingsRoutes(r chi.Router, st *settings.Store, dirs datadir.Dirs) {
	r.Get("/settings", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"items": st.List(), "dataDir": dirs.Root})
	})
	r.Put("/settings", func(w http.ResponseWriter, req *http.Request) {
		var body map[string]json.RawMessage
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil || len(body) == 0 {
			httpx.Fail(w, req, httpx.Errf(errcode.InvalidArgument, "请求体应为 {键: 值}"))
			return
		}
		restart, err := st.Set(req.Context(), body)
		if err != nil {
			httpx.Fail(w, req, httpx.Errf(errcode.InvalidArgument, err.Error()))
			return
		}
		writeJSON(w, map[string]any{"items": st.List(), "restartRequired": restart})
	})
	// 数据目录各部分占用（设置 → 存储）。
	r.Get("/settings/storage/usage", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{
			"dataDir": dirs.Root,
			"media":   dirSize(dirs.Media), "database": dirSize(dirs.PG),
			"logs": dirSize(dirs.Logs), "backups": dirSize(dirs.Backups),
		})
	})
	// 可选 S3 连接测试：探活 → 上传 1 字节 → 签名 → 删除。用请求体里的配置测（还没保存也能测）。
	r.Post("/settings/storage/test", func(w http.ResponseWriter, req *http.Request) {
		var in struct {
			Endpoint, Region, Bucket, AccessKey, SecretKey, Prefix string
			PathStyle                                              bool
			// KeepSecret：表单里密钥没改（只显示掩码）时用已保存的那一份，不把掩码当密钥（TT-F-28）。
			KeepSecret bool
		}
		if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
			httpx.Fail(w, req, httpx.Errf(errcode.InvalidArgument, "请求体格式不对"))
			return
		}
		if in.KeepSecret {
			in.SecretKey = st.String(settings.S3SecretKey)
		}
		writeJSON(w, testS3(req.Context(), config.Storage{Type: "s3", Endpoint: in.Endpoint, Region: in.Region,
			Bucket: in.Bucket, AccessKey: in.AccessKey, SecretKey: in.SecretKey, PathStyle: in.PathStyle,
			Prefix: in.Prefix, SignTTL: time.Hour}))
	})
}

// testS3 逐步测试，每一步给结论（哪一步断就说哪一步，TT-F-34）。
func testS3(ctx context.Context, cfg config.Storage) map[string]any {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	steps := []map[string]any{}
	add := func(name string, err error) bool {
		item := map[string]any{"step": name, "ok": err == nil}
		if err != nil {
			item["message"] = err.Error()
		}
		steps = append(steps, item)
		return err == nil
	}
	c, err := objstore.New(ctx, cfg)
	if !add("建立客户端", err) {
		return map[string]any{"ok": false, "steps": steps}
	}
	if !add("访问存储桶", c.Check(ctx)) {
		return map[string]any{"ok": false, "steps": steps}
	}
	key := "tabtv-probe/" + time.Now().UTC().Format("20060102T150405")
	if !add("上传测试对象", c.PutFile(ctx, key, bytes.NewReader([]byte("x")), 1, "text/plain")) {
		return map[string]any{"ok": false, "steps": steps}
	}
	_, err = c.PresignGet(ctx, key)
	add("签发下载地址", err)
	add("删除测试对象", c.Delete(ctx, key))
	ok := true
	for _, s := range steps {
		ok = ok && s["ok"].(bool)
	}
	return map[string]any{"ok": ok, "steps": steps}
}

func dirSize(root string) int64 {
	var n int64
	_ = filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() {
			if info, err := d.Info(); err == nil {
				n += info.Size()
			}
		}
		return nil
	})
	return n
}
