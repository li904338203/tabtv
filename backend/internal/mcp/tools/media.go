package tools

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"

	collab "github.com/li904338203/tabtv/backend/internal/collaboration/domain"
	"github.com/li904338203/tabtv/backend/internal/mcp/app"
	"github.com/li904338203/tabtv/backend/internal/mcp/domain"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// media.get（MCP-F-54）：元信息 + 短期签名直链（MCP-PARAM-06 建议 1 小时）+ 图片的 320px 缩略图内联。
//
// ⚠️ 缩略图**内联**而不是给地址：受保护的 /media/{id} 端点要用户 JWT，外部客户端拿不到（开发 01 §12 R12）。
// ⚠️ 视频 / 音频**不内联**：base64 后几十 MB，会直接撑爆客户端的上下文；给直链让它自己决定取不取。
// ⚠️ 归属在每一步的 SQL 里校验：别的工作区的媒体与不存在的一样回 NOT_FOUND（不区分，否则就是探测接口）。

// maxInlineBytes 内联缩略图的上限：320px 的 PNG/JPEG 通常几十 KB，超过这个数说明没缩成功（拿到的是原图）。
const maxInlineBytes = 512 << 10

// thumbWidth 是内联缩略图宽度（media 域只接受 160/320/640 三档）。
const thumbWidth = 320

type mediaIn struct {
	MediaID string `json:"mediaId,omitempty" jsonschema:"get：媒体 ID（来自 canvas_read / tasks.get 的产物）"`
	// upload：url 与 base64 二选一。
	URL      string `json:"url,omitempty" jsonschema:"upload：公网可访问的 http/https 地址（图片 / 视频 / 音频；与 base64 二选一）"`
	Base64   string `json:"base64,omitempty" jsonschema:"upload：图片的 base64（解码后不超过 5 MB，只收图片；更大或视频音频请用 url）"`
	FileName string `json:"fileName,omitempty" jsonschema:"upload：文件名（base64 时必填，用于展示）"`
	MimeType string `json:"mimeType,omitempty" jsonschema:"upload：声明的类型（可选，只作校验；实际类型按文件内容判）"`
}

type referenceOut struct {
	ReferenceID string `json:"referenceId" jsonschema:"参考文件 ID：作为 generate 的 inputs[].referenceId（kind=reference）使用"`
	Kind        string `json:"kind" jsonschema:"image / video / audio"`
	ContentType string `json:"contentType"`
	SizeBytes   int64  `json:"sizeBytes"`
	Width       int    `json:"width,omitempty"`
	Height      int    `json:"height,omitempty"`
	DurationMs  int    `json:"durationMs,omitempty"`
}

type mediaInfoOut struct {
	ID          string `json:"id"`
	ContentType string `json:"contentType"`
	SizeBytes   int64  `json:"sizeBytes"`
	Width       int    `json:"width,omitempty"`
	Height      int    `json:"height,omitempty"`
	DurationMs  int    `json:"durationMs,omitempty"`
	Purpose     string `json:"purpose,omitempty"`
}

type mediaOut struct {
	Media        *mediaInfoOut `json:"media,omitempty" jsonschema:"get：媒体元信息"`
	URL          string        `json:"url,omitempty" jsonschema:"get：短期签名直链（到期后重新调用 media.get 取新的）"`
	URLExpiresAt string        `json:"urlExpiresAt,omitempty"`
	// ThumbnailInline 表示本结果附带了一张 image content（320px 缩略图）。
	ThumbnailInline bool   `json:"thumbnailInline"`
	Note            string `json:"note,omitempty"`
	// Reference 是 upload 的结果。
	Reference *referenceOut `json:"reference,omitempty" jsonschema:"upload：上传好的参考文件"`

	images []app.Image
}

// InlineImages 实现 app.ImageCarrier：缩略图走 image content，不进 structuredContent。
func (o mediaOut) InlineImages() []app.Image { return o.images }

func mediaTool(d Deps) app.Tool {
	return app.Define(app.Tool{
		Name:    "media",
		Toolset: domain.ToolsetGeneration,
		Description: "取媒体（生成产物、素材）：get 返回元信息与一个短期有效的下载直链；图片另附 320px 缩略图。" +
			"视频与音频只给直链，不内联内容。upload：把一张参考图 / 一段参考视频或音频放进本工作区，返回 referenceId " +
			"（作为 generate 的 inputs[].referenceId 使用）：url（公网地址，图片 / 视频 / 音频）与 base64（只收图片，" +
			"解码后不超过 5 MB）二选一。不花积分。",
		Methods: []app.Method{{Name: "get", Action: collab.ActionView},
			// 上传要 edit（与站内「上传参考图」同一档）：往工作区里放了一个文件。
			{Name: "upload", Action: collab.ActionEdit, Write: true}},
	}, func(ctx context.Context, method string, in mediaIn) (mediaOut, string, error) {
		p, err := principal(ctx)
		if err != nil {
			return mediaOut{}, "", err
		}
		if method == "upload" {
			return uploadMedia(ctx, d, p, in)
		}
		if d.Media == nil {
			return mediaOut{}, "", unwired("Media")
		}
		id, err := parseID("mediaId", in.MediaID)
		if err != nil {
			return mediaOut{}, "", err
		}
		info, err := d.Media.Info(ctx, p, id)
		if err != nil {
			return mediaOut{}, "", err
		}
		url, exp, err := d.Media.SignedURL(ctx, p, id)
		if err != nil {
			return mediaOut{}, "", err
		}
		out := mediaOut{
			Media: &mediaInfoOut{ID: idStr(info.ID), ContentType: info.ContentType, SizeBytes: info.SizeBytes,
				Width: info.Width, Height: info.Height, DurationMs: info.DurationMs, Purpose: info.Purpose},
			URL: url, URLExpiresAt: ts(exp),
		}
		summary := fmt.Sprintf("媒体 %s（%s，%d 字节），直链有效至 %s", out.Media.ID, info.ContentType, info.SizeBytes,
			out.URLExpiresAt)
		switch {
		case InlineThumbnail(info.ContentType):
			data, mime, err := d.Media.Thumbnail(ctx, p, id)
			switch {
			case err != nil:
				// 缩略图是锦上添花：取不到不让整个调用失败（直链仍然可用）。
				out.Note = "缩略图暂时取不到，请用 url 下载"
			case len(data) > maxInlineBytes || !strings.HasPrefix(mime, "image/"):
				out.Note = "缩略图过大未内联，请用 url 下载"
			default:
				out.images = []app.Image{{MIMEType: mime, Data: data}}
				out.ThumbnailInline = true
				summary += "；附 320px 缩略图"
			}
		case strings.HasPrefix(info.ContentType, "video/"), strings.HasPrefix(info.ContentType, "audio/"):
			out.Note = "视频 / 音频不内联，请用 url 下载"
		default:
			out.Note = "该格式不生成缩略图，请用 url 下载"
		}
		return out, summary, nil
	})
}

// InlineThumbnail 判断某类媒体是否内联缩略图：只有 media 域能缩的位图（PNG / JPEG）。
// 其余格式（含 WebP / GIF）media 域会原样回原图，可能很大，所以不内联。
func InlineThumbnail(contentType string) bool {
	return contentType == "image/png" || contentType == "image/jpeg"
}

// maxBase64Bytes 是 base64 来源解码后的上限（MCP-F-32 的「小体积」）：再大的话 base64 后的请求体会撞上
// /mcp 的请求体上限（8 MiB），而且模型要把几 MB 的 base64 一字不差地抄进参数里 —— 那种文件请走 url。
const maxBase64Bytes = 5 << 20

// uploadMedia 上传参考文件（MCP-F-32）。url 与 base64 二选一；类型一律按实际文件头判（media 域，GO-27），
// 声明的 mimeType 只作交叉校验。
func uploadMedia(ctx context.Context, d Deps, p app.Principal, in mediaIn) (mediaOut, string, error) {
	src, err := uploadSource(in)
	if err != nil {
		return mediaOut{}, "", err
	}
	if d.Uploader == nil {
		return mediaOut{}, "", unwired("Uploader")
	}
	ref, err := d.Uploader.UploadReference(ctx, p, src)
	if err != nil {
		return mediaOut{}, "", err
	}
	ro := &referenceOut{ReferenceID: idStr(ref.ID), Kind: ref.Kind, ContentType: ref.MIME, SizeBytes: ref.SizeBytes,
		Width: ref.Width, Height: ref.Height, DurationMs: ref.DurationMs}
	return mediaOut{Reference: ro}, fmt.Sprintf("已上传参考文件 %s（%s，%d 字节）：作为 generate 的 inputs[].referenceId 使用",
		ro.ReferenceID, ref.MIME, ref.SizeBytes), nil
}

// uploadSource 校验 upload 的入参：url 与 base64 恰好给一个；base64 解码后 ≤5 MB 且只收图片。
func uploadSource(in mediaIn) (app.UploadSource, error) {
	url, b64 := strings.TrimSpace(in.URL), strings.TrimSpace(in.Base64)
	switch {
	case in.MediaID != "":
		return app.UploadSource{}, httpx.Errf(errcode.InvalidArgument, "upload 不收 mediaId（那是 get 的参数）")
	case url == "" && b64 == "":
		return app.UploadSource{}, httpx.Errf(errcode.InvalidArgument, "upload 需要 url 或 base64（二选一）")
	case url != "" && b64 != "":
		return app.UploadSource{}, httpx.Errf(errcode.InvalidArgument, "url 与 base64 只能给一个")
	}
	if url != "" {
		return app.UploadSource{URL: url, FileName: strings.TrimSpace(in.FileName), DeclaredMIME: in.MimeType}, nil
	}
	name := strings.TrimSpace(in.FileName)
	if name == "" {
		return app.UploadSource{}, httpx.Errf(errcode.InvalidArgument, "base64 上传需要 fileName")
	}
	// 先按编码长度挡一道：解码之前就知道一定超限的，不必先分配几 MB 再拒。
	if base64.StdEncoding.DecodedLen(len(b64)) > maxBase64Bytes+3 {
		return app.UploadSource{}, base64TooLarge()
	}
	if i := strings.Index(b64, ";base64,"); i >= 0 && strings.HasPrefix(b64, "data:") {
		b64 = b64[i+len(";base64,"):] // 容忍 data: URL 前缀
	}
	data, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		if data, err = base64.RawStdEncoding.DecodeString(b64); err != nil {
			return app.UploadSource{}, httpx.Errf(errcode.InvalidArgument, "base64 解码失败：请给标准 base64（可带 data: 前缀）")
		}
	}
	if len(data) > maxBase64Bytes {
		return app.UploadSource{}, base64TooLarge()
	}
	if len(data) == 0 {
		return app.UploadSource{}, httpx.Errf(errcode.InvalidArgument, "base64 解码后为空")
	}
	return app.UploadSource{Data: data, FileName: name, DeclaredMIME: in.MimeType, ImageOnly: true}, nil
}

func base64TooLarge() error {
	return httpx.Errf(errcode.PayloadTooLarge,
		fmt.Sprintf("base64 解码后超过 %d MB：更大的文件请放到公网地址上，用 url 上传", maxBase64Bytes>>20))
}
