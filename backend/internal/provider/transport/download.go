package transport

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// ErrTooLarge 产物超过允许的大小。
var ErrTooLarge = errors.New("产物超过大小上限")

// Downloaded 是一个已落到临时文件的产物。
//
// ⚠️ 调用方**必须 Close**：Close 会连带删掉临时文件。
// 漏掉的话磁盘上会积一堆谁也不认识的文件，而且每跑一次任务多一个。
type Downloaded struct {
	io.ReadCloser
	MIME string
	Size int64
}

type tempFile struct {
	f *os.File
}

func (t tempFile) Read(p []byte) (int, error) { return t.f.Read(p) }

func (t tempFile) Close() error {
	name := t.f.Name()
	err := t.f.Close()
	// 先删再返回错误：关闭失败也不该把文件留在盘上。
	if rmErr := os.Remove(name); err == nil {
		err = rmErr
	}
	return err
}

// Download 把上游产物拉下来。
//
// ⚠️ **落临时文件而不是内存缓冲**：产物里有几百 MB 的视频，
// 按内存缓冲并发几路就把 Worker 撑爆（老系统的教训）。
// 归档器拿到的是一个流，从临时文件读一遍即可。
//
// ⚠️ 认证由调用方决定要不要带：只有与 Provider **同源**的地址才带
// （用 SameOrigin 判），上游返回的产物地址常常是 CDN 或对象存储签名链接，
// 把 API key 一起发过去等于把密钥泄漏给第三方（GO-31）。
func (c *Client) Download(ctx context.Context, url string, header map[string]string,
	opts Options, maxBytes int64) (Downloaded, error) {
	if err := c.policy.CheckURL(url); err != nil {
		return Downloaded{}, err
	}
	if maxBytes <= 0 {
		maxBytes = DefaultMaxDownloadBytes
	}
	timeout := opts.RequestTimeout
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Downloaded{}, err
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := c.httpClient(opts.ConnectTimeout).Do(req)
	if err != nil {
		return Downloaded{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return Downloaded{}, fmt.Errorf("下载产物失败 HTTP %d: %s", resp.StatusCode, body)
	}

	f, err := os.CreateTemp("", "seeaitv-output-*")
	if err != nil {
		return Downloaded{}, err
	}
	out := tempFile{f: f}
	// 多读 1 字节：读满上限说明后面还有，要判超限而不是悄悄截断
	// —— 截断后的视频文件是坏的，而用户会以为是模型生成坏了。
	n, err := io.Copy(f, io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		_ = out.Close()
		return Downloaded{}, err
	}
	if n > maxBytes {
		_ = out.Close()
		return Downloaded{}, fmt.Errorf("%w: 超过 %d 字节", ErrTooLarge, maxBytes)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		_ = out.Close()
		return Downloaded{}, err
	}
	return Downloaded{ReadCloser: out, MIME: resp.Header.Get("Content-Type"), Size: n}, nil
}

// DefaultMaxDownloadBytes 单个产物的大小上限。
//
// 定在 512MB：成片视频可以很大，但一个远超这个数的"产物"
// 多半是上游把整个页面或错误页返回给了我们。
const DefaultMaxDownloadBytes int64 = 512 << 20
