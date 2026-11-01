// Package relay 把参考媒体转存到「公网中转」对象存储，给只收地址、又够不着主存储的上游用
// （marketing dev/01 R-15）。
//
// 起因：UniArt 只收公网地址形态的参考图（R-13），本地 MinIO 签出的 127.0.0.1 它拉不到；而把主存储
// 整体搬到自建服务器上又不行 —— 那台机器出口只有 5Mbps，前端一页十几张原图全部 30 秒超时（R-14）。
// 所以主存储留在本地，只有真要发给这类上游的那几个文件走一趟中转：
//
//   - 同一个对象只传一次：先 Exists，在就直接签名；
//   - 副本放在单独的前缀下（默认 relay/），由存储侧的过期规则清理 —— 过期之后再用到，这里会重传一份。
package relay

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/li904338203/tabtv/backend/internal/media/repo"
)

// Source 是主存储一侧：按媒体 ID 取元数据与字节（媒体域 app.Service 实现）。
type Source interface {
	Get(ctx context.Context, mediaID int64) (repo.MediaObject, error)
	Open(ctx context.Context, mediaID int64) (io.ReadCloser, repo.MediaObject, error)
}

// Store 是中转存储一侧（*objstore.Client 实现）。
type Store interface {
	Exists(ctx context.Context, key string) (bool, error)
	PutFile(ctx context.Context, key string, body io.ReadSeeker, size int64, contentType string) error
	PresignGet(ctx context.Context, key string) (string, error)
}

// Relay 实现 provider/app.MediaRelay。
type Relay struct {
	src    Source
	dst    Store
	prefix string
	logger *slog.Logger
}

func New(src Source, dst Store, prefix string, logger *slog.Logger) *Relay {
	if logger == nil {
		logger = slog.Default()
	}
	return &Relay{src: src, dst: dst, prefix: prefix, logger: logger}
}

// URL 返回媒体在中转存储里的签名地址（有效期沿用主存储的 SignTTL）；那边还没有就先转存一份。
func (r *Relay) URL(ctx context.Context, mediaID int64) (string, string, error) {
	m, err := r.src.Get(ctx, mediaID)
	if err != nil {
		return "", "", fmt.Errorf("取媒体 %d 失败: %w", mediaID, err)
	}
	key := r.prefix + m.ObjectKey
	ok, err := r.dst.Exists(ctx, key)
	if err != nil {
		return "", "", fmt.Errorf("查询中转存储失败: %w", err)
	}
	if !ok {
		if err := r.copy(ctx, mediaID, key); err != nil {
			return "", "", err
		}
	}
	u, err := r.dst.PresignGet(ctx, key)
	if err != nil {
		return "", "", fmt.Errorf("签发中转地址失败: %w", err)
	}
	return u, m.ContentType, nil
}

// copy 先落临时文件再上传：签名要能重读 body，参考视频可能上百 MB，不能进内存。临时文件一定删掉。
func (r *Relay) copy(ctx context.Context, mediaID int64, key string) error {
	body, m, err := r.src.Open(ctx, mediaID)
	if err != nil {
		return fmt.Errorf("读取媒体 %d 失败: %w", mediaID, err)
	}
	defer body.Close()

	tmp, err := os.CreateTemp("", "seeaitv-relay-*")
	if err != nil {
		return fmt.Errorf("建临时文件失败: %w", err)
	}
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
	}()
	size, err := io.Copy(tmp, body)
	if err != nil {
		return fmt.Errorf("读取媒体 %d 失败: %w", mediaID, err)
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("回到临时文件开头失败: %w", err)
	}

	start := time.Now()
	if err := r.dst.PutFile(ctx, key, tmp, size, m.ContentType); err != nil {
		return fmt.Errorf("转存到中转存储失败: %w", err)
	}
	r.logger.Info("参考媒体已转存到公网中转存储", "mediaId", mediaID, "bytes", size,
		"durationMs", time.Since(start).Milliseconds())
	return nil
}
