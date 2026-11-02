package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

// SavePath 是前端「另存为」的本机接口（dev/03 TTD-36，TT-UX-5）：
// 请求体就是文件内容（blob 原样 POST 过来，经 AssetServer 流式交给这里，不经 base64、不进 IPC 消息），
// 这里弹系统的另存为对话框，用户选好位置后写盘。
//
// ⚠️ 不能沿用浏览器的 `<a download>`：WebView 里没有下载管理器，点了不是没反应就是直接在窗口里打开文件。
const SavePath = "/api/v1/_desktop/save"

// SaveMiddleware 拦下 SavePath，其余交给 next。ctx 为 Wails 的应用 context（对话框要用）。
func SaveMiddleware(ctx func() context.Context) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != SavePath || r.Method != http.MethodPost {
				next.ServeHTTP(w, r)
				return
			}
			name := sanitizeName(r.URL.Query().Get("name"))
			path, err := wruntime.SaveFileDialog(ctx(), wruntime.SaveDialogOptions{
				DefaultFilename: name, Title: "保存到",
			})
			w.Header().Set("Content-Type", "application/json")
			if err != nil {
				w.WriteHeader(http.StatusInternalServerError)
				_ = json.NewEncoder(w).Encode(map[string]any{"saved": false, "message": err.Error()})
				return
			}
			if path == "" { // 用户取消
				_ = json.NewEncoder(w).Encode(map[string]any{"saved": false})
				return
			}
			if err := writeFile(path, r.Body); err != nil {
				w.WriteHeader(http.StatusInternalServerError)
				_ = json.NewEncoder(w).Encode(map[string]any{"saved": false, "message": err.Error()})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"saved": true, "path": path})
		})
	}
}

// writeFile 先写临时文件再改名：写到一半出错不会留下一个看似完整的半截文件。
func writeFile(path string, body io.Reader) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tabtv-save-*")
	if err != nil {
		return err
	}
	if _, err := io.Copy(tmp, body); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// sanitizeName 只取文件名部分（前端已经做过 safeFilename，这里再防一次路径分隔符）。
func sanitizeName(name string) string {
	name = filepath.Base(strings.ReplaceAll(name, "\\", "/"))
	if name == "." || name == "/" || name == "" {
		return "tabtv-download"
	}
	return name
}
