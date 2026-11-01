package typeset

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// maxRequestBytes：500 个字加一份样式，远到不了 64 KB；再大的请求体只会是别有用心。
const maxRequestBytes = 64 << 10

// 响应头（契约 typesetSmartcutText）。
const (
	HeaderBox     = "X-Typeset-Box"
	HeaderMissing = "X-Typeset-Missing"
)

// Handler 是 `POST /smartcut/typeset`（SCD-28）的处理器。权限（view）与工作区上下文由挂载方的
// 路由闸门负责：这个接口只读、不扣费、不落库。
//
// 请求体是契约的 TypesetRequest（text / width / height / style / kind），另收一个可选的 `box`
// （文字层的 xPct / yPct / anchor）：文字层的位置不在 style 里，不带它的话文字层一律按画面正中排，
// 预览里的位置就和成片对不上。
//
// 响应 image/png；`X-Typeset-Box` 是 PNG 在画面里的位置 `x,y,w,h`（像素，非透明区域的包围框）；
// `X-Typeset-Missing` 是画成替代字形的字符，百分号编码、不加分隔 ——
// 前端 `decodeURIComponent` 之后按码点拆开（`Array.from`）。
func Handler(t *Typesetter) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Text   *string        `json:"text"`
			Width  *int           `json:"width"`
			Height *int           `json:"height"`
			Kind   string         `json:"kind"`
			Style  map[string]any `json:"style"`
			Box    map[string]any `json:"box"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBytes)).Decode(&body); err != nil {
			var tooBig *http.MaxBytesError
			if errors.As(err, &tooBig) {
				httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, fmt.Sprintf("请求体超过 %d KB", maxRequestBytes>>10)))
				return
			}
			httpx.Fail(w, r, httpx.Errf(errcode.InvalidJSON, "请求体不是合法 JSON"))
			return
		}
		for _, f := range []struct {
			path    string
			missing bool
		}{{"text", body.Text == nil}, {"width", body.Width == nil}, {"height", body.Height == nil}} {
			if f.missing {
				failField(w, r, &FieldError{Path: f.path, Msg: "必填"})
				return
			}
		}
		req := Request{
			Text: *body.Text, Kind: body.Kind, Style: body.Style, Box: body.Box,
			Width: *body.Width, Height: *body.Height,
		}
		// 接口上严格校验、指出是哪一项（SC-AC 的 40001）；排版本身对样式只夹回不报错，
		// 那是给库里存着的样式用的（见 ParseStyle）。
		if err := ValidateRequest(req); err != nil {
			writeError(w, r, err)
			return
		}
		res, err := t.Typeset(r.Context(), req)
		if err != nil {
			writeError(w, r, err)
			return
		}
		h := w.Header()
		h.Set("Content-Type", "image/png")
		h.Set(HeaderBox, fmt.Sprintf("%d,%d,%d,%d", res.Box.X, res.Box.Y, res.Box.W, res.Box.H))
		h.Set(HeaderMissing, url.PathEscape(strings.Join(res.Missing, "")))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(res.PNG)
	}
}

func failField(w http.ResponseWriter, r *http.Request, fe *FieldError) {
	httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, fe.Error()).WithDetails(map[string]any{"path": fe.Path}))
}

func writeError(w http.ResponseWriter, r *http.Request, err error) {
	var fe *FieldError
	switch {
	case errors.As(err, &fe):
		failField(w, r, fe)
	case errors.Is(err, ErrFontsUnavailable):
		// 503 而不是 500：是部署没装字体（make fonts / SEEAITV_TYPESET_FONT_DIR），不是代码坏了。
		be := httpx.Wrap(errcode.DependencyUnavailable, err)
		be.Message = "排版字体未就绪"
		httpx.Fail(w, r, be)
	case errors.Is(err, context.Canceled):
		// 客户端已经走了（停手后又改了字，前端取消了上一次请求），没有人在等这个响应。
	default:
		httpx.Fail(w, r, err)
	}
}
