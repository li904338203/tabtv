package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/url"
	"strings"

	genapp "github.com/li904338203/tabtv/backend/internal/generation/app"
	"github.com/li904338203/tabtv/backend/internal/provider/protocol"
	"github.com/li904338203/tabtv/backend/internal/provider/transport"
)

// materialize 把协议解析出的产物变成可归档的形态。
//
// ⚠️ URL 形态的产物**必须在这里取回来**，不能把上游地址存进产物表：
// 那些地址几小时就失效，用户第二天点开就是一片裂图，
// 而那时上游早已把文件清掉，补都补不回来。
//
// ⚠️ 失败时要把已经取回的临时文件关掉（Close 会删文件），
// 否则一次失败就在盘上留下几个没人认识的文件。
func (p *HTTPProvider) materialize(ctx context.Context, cfg RunConfig, resultType string,
	outs []protocol.Output) ([]genapp.ProviderOutput, error) {
	result := make([]genapp.ProviderOutput, 0, len(outs))
	cleanup := func() {
		for _, o := range result {
			if c, ok := o.Body.(io.Closer); ok {
				_ = c.Close()
			}
		}
	}

	for i, o := range outs {
		switch o.Kind {
		case "text":
			result = append(result, genapp.ProviderOutput{Type: "text", Text: o.Value, Meta: o.Meta})

		case "base64":
			data, err := base64.StdEncoding.DecodeString(strings.TrimSpace(o.Value))
			if err != nil {
				cleanup()
				return nil, fmt.Errorf("第 %d 个产物 base64 解码失败: %w", i, err)
			}
			mime := firstNonEmpty(o.MIME, mimeForResult(resultType))
			result = append(result, genapp.ProviderOutput{
				Type: outputType(mime, resultType), MIME: mime,
				Size: int64(len(data)), Body: bytes.NewReader(data), Meta: o.Meta,
			})

		case "url":
			// ⚠️ 上游可能回**相对地址**（relay.example.com 的 Grok 视频回 `/v1/videos/{id}/content`）：
			//    按 Provider 的地址补全。不补的话一律「只允许 http/https」—— 而这时任务在上游**已经成功、
			//    已经计费**，我们这边却判失败退款，用户只看到「下载第 0 个产物失败」。
			ref := resolveOutputURL(cfg.BaseURL, o.Value)
			// ⚠️ 上游说「这个地址要带认证」而它与 Provider 不同源：改走协议给的同源兜底地址；
			// 没有兜底就明确报错 —— **不外发密钥**（GO-31），也不发一个注定 401 的请求。
			if o.AuthRequired && !transport.SameOrigin(cfg.BaseURL, ref) {
				if o.AuthFallback == "" {
					cleanup()
					return nil, fmt.Errorf("第 %d 个产物要求带认证下载，但地址与 Provider 不同源，拒绝外发密钥", i)
				}
				ref = resolveOutputURL(cfg.BaseURL, o.AuthFallback)
			}
			// ⚠️ 只有与 Provider **同源**的地址才带认证：
			// 产物地址常常是 CDN 或对象存储签名链接，
			// 把 API key 一起发过去等于把密钥泄漏给第三方（GO-31）。
			// 补全出来的相对地址天然同源 —— 上游自己的内容端点，要带 key 才取得到。
			header := map[string]string{}
			if transport.SameOrigin(cfg.BaseURL, ref) {
				header = authHeaders(cfg)
			}
			d, err := p.client.Download(ctx, ref, header, transport.Options{
				ConnectTimeout: cfg.ConnectTimeout, RequestTimeout: cfg.RequestTimeout,
			}, p.MaxDownloadBytes)
			if err != nil {
				cleanup()
				return nil, fmt.Errorf("下载第 %d 个产物失败: %w", i, err)
			}
			mime := firstNonEmpty(cleanMIME(d.MIME), o.MIME, mimeForResult(resultType))
			result = append(result, genapp.ProviderOutput{
				Type: outputType(mime, resultType), MIME: mime,
				Size: d.Size, Body: d.ReadCloser, Meta: o.Meta,
			})

		default:
			cleanup()
			return nil, fmt.Errorf("第 %d 个产物形态未知: %q", i, o.Kind)
		}
	}
	return result, nil
}

// resolveOutputURL 把上游回的**相对地址**按 Provider 的地址补全（RFC 3986：`/x` 相对于站点根、`x` 相对于 base 的目录、
// `//host/x` 沿用 base 的协议）。绝对地址原样返回 —— 包括 `file:` / `data:` 这类，照旧交给出网策略拒绝，补全不放宽任何约束。
func resolveOutputURL(baseURL, raw string) string {
	ref, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || ref.IsAbs() {
		return raw
	}
	base, err := url.Parse(baseURL)
	if err != nil || base.Scheme == "" || base.Host == "" {
		return raw
	}
	return base.ResolveReference(ref).String()
}

// outputType 判定产物类型。
//
// ⚠️ 以 MIME 为准、结果类型兜底：结果类型是**我们期望的**，
// MIME 是**上游实际给的**。两者不一致时按实际的存，
// 否则一个回了 jpeg 的"视频"任务会被当成视频存进去，播不出来。
func outputType(mime, resultType string) string {
	switch {
	case strings.HasPrefix(mime, "image/"):
		return "image"
	case strings.HasPrefix(mime, "audio/"):
		return "audio"
	case strings.HasPrefix(mime, "video/"):
		return "video"
	}
	switch resultType {
	case "image", "audio", "video", "text":
		return resultType
	}
	return "image"
}

func mimeForResult(resultType string) string {
	switch resultType {
	case "image":
		return "image/png"
	case "audio":
		return "audio/mpeg"
	case "video":
		return "video/mp4"
	}
	return "application/octet-stream"
}

// cleanMIME 去掉 `; charset=` 之类的参数段。
func cleanMIME(mime string) string {
	if i := strings.Index(mime, ";"); i > 0 {
		mime = mime[:i]
	}
	mime = strings.TrimSpace(mime)
	// 有些 CDN 对未知后缀一律回 octet-stream，用它会把类型判错，
	// 不如当作"没给"，让后面的回落链接手。
	if mime == "application/octet-stream" || mime == "binary/octet-stream" {
		return ""
	}
	return mime
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// decodeDataURI 解 `data:<mime>;base64,<内容>`。
func decodeDataURI(s string) ([]byte, string, error) {
	i := strings.Index(s, ";base64,")
	if i < 0 {
		return nil, "", errNoData
	}
	mime := s[len("data:"):i]
	data, err := base64.StdEncoding.DecodeString(s[i+len(";base64,"):])
	if err != nil {
		return nil, "", err
	}
	return data, mime, nil
}
