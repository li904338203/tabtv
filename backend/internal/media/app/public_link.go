package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/li904338203/tabtv/backend/internal/media/domain"
	"github.com/li904338203/tabtv/backend/internal/media/repo"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// 公开图片直连对象存储（2026-11-02，对象存储迁到火山 TOS 之后）。
//
// 起因：公开封面（营销选项、技能、精选作品）原先由 API 取原图 → 现场缩略 → 把字节回给浏览器。测试服在海外、
// TOS 在北京，取一次原图（3~4MB）就要 1 秒多，缩略图每次都重做也不存，PNG 缩略图又有三百多 KB ——
// 一张 320 宽的封面首字节 2 秒多、整张 5~6 秒。
//
// 现在：可见性仍由调用方先判（它是「能不能公开」的唯一闸门），之后这里给出一个**短时效的签名地址**，
// 由传输层 302 过去，浏览器直接从对象存储取；缩略图生成一次、存成派生媒体，之后只查库。
//
// ⚠️ 只给 `<img>` / `<video>` 直接加载的匿名公开端点用。用 fetch 读字节的端点（作者看自己的作品）不能跳转：
// 跨源跳转后读响应体要对象存储配 CORS，没配的话前端拿不到图。

const (
	// DerivePublicThumb 公开缩略图的派生种类；derive_key 是宽度（"w320"）。
	DerivePublicThumb = "public_thumb"
	// PurposePublicThumbnail 公开缩略图的用途。单独一个用途：不会混进任何按用途列的列表（素材库、生成历史）。
	PurposePublicThumbnail = "public_thumbnail"

	// publicLinkTTL 签名地址的有效期。⚠️ 它也是「撤下来之后，已经拿到地址的人还能看多久」的窗口：
	// 跳转本身只缓存 5 分钟（与原来的公开缓存一致），这里取 15 分钟 —— 够浏览器把图取完，又不至于太长。
	publicLinkTTL = 15 * time.Minute
	// publicLinkReuse 同一对象在这段时间内复用同一个签名地址：每次都签新地址的话 URL 每次不同，
	// 浏览器的图片缓存永远命中不了。要短于 publicLinkTTL，留出取图的时间。
	publicLinkReuse = 10 * time.Minute
	// publicThumbBuildTimeout 首次生成缩略图（取原图 + 缩放 + 存回）的上限，不跟着请求取消。
	publicThumbBuildTimeout = 2 * time.Minute
)

// ErrNoDirectLink 当前存储签不出可直连的地址（本地开发的 local 存储）：调用方退回原来的输出字节。
var ErrNoDirectLink = errors.New("当前存储不支持直连地址")

type cachedLink struct {
	url     string
	expires time.Time
}

type linkCache struct {
	mu sync.Mutex
	m  map[string]cachedLink
}

func (c *linkCache) get(key string, now time.Time) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	l, ok := c.m[key]
	if !ok || now.After(l.expires) {
		return "", false
	}
	return l.url, true
}

func (c *linkCache) put(key, url string, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil || len(c.m) > 20000 {
		c.m = map[string]cachedLink{} // 粗暴但够用：只是复用签名地址，丢了重签即可
	}
	c.m[key] = cachedLink{url: url, expires: now.Add(publicLinkReuse)}
}

// PublicLink 给一份**已经被调用方判定为公开**的媒体签一个直连地址，不做归属校验（同 OpenPublic）。
//
// cover=true 时取封面（图片就是它自己，视频取 Worker 截的封面帧，其余 404，同 OpenPublicCover）。
// width>0 且是位图时给缩略图（宽度只接受 160/320/640）；否则给原文件（视频播放据此直连，还顺带支持了 Range）。
func (s *Service) PublicLink(ctx context.Context, mediaID int64, width int, cover bool) (string, error) {
	if s.store == nil {
		return "", ErrNoDirectLink
	}
	m, err := s.repo.Get(ctx, mediaID)
	if err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			return "", httpx.Err(errcode.NotFound)
		}
		return "", httpx.Wrap(errcode.Internal, err)
	}
	if cover {
		switch kindOf(m.ContentType) {
		case "image":
		case "video":
			poster, ok, err := s.PosterOf(ctx, m.WorkspaceID, m.ID)
			if err != nil {
				return "", httpx.Wrap(errcode.Internal, err)
			}
			if !ok {
				return "", httpx.Errf(errcode.NotFound, "视频的封面帧还没截好")
			}
			m = poster
		default:
			return "", httpx.Errf(errcode.NotFound, "这类媒体没有封面图")
		}
	}
	if width > 0 && isBitmap(m.ContentType) {
		if !domain.IsAllowedThumbWidth(width) {
			return "", httpx.Errf(errcode.InvalidArgument, "缩略图宽度只接受 160/320/640")
		}
		thumb, err := s.publicThumb(ctx, m, width)
		if err != nil {
			return "", err
		}
		m = thumb
	}
	return s.signPublic(ctx, m.ObjectKey)
}

func (s *Service) signPublic(ctx context.Context, key string) (string, error) {
	now := time.Now()
	if u, ok := s.links.get(key, now); ok {
		return u, nil
	}
	u, err := s.store.PresignGetTTL(ctx, key, publicLinkTTL)
	if err != nil {
		return "", httpx.Wrap(errcode.Internal, err)
	}
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		return "", ErrNoDirectLink
	}
	s.links.put(key, u, now)
	return u, nil
}

// publicThumb 取（没有就生成并存下）一份媒体的公开缩略图。缩不了时返回原图本身（缩略图是优化）。
func (s *Service) publicThumb(ctx context.Context, m repo.MediaObject, width int) (repo.MediaObject, error) {
	key := "w" + strconv.Itoa(width)
	if t, ok, err := s.repo.FindDerived(ctx, m.ID, DerivePublicThumb, key); err != nil {
		return repo.MediaObject{}, httpx.Wrap(errcode.Internal, err)
	} else if ok {
		return t, nil
	}

	// 生成与存储不跟着这一次请求取消（2026-11-02 实测）：服务器离对象存储远时，首次生成可能超过请求的
	// 30 秒上限 —— 跟着取消的话做了一半的活全白做，下一个访客又从头来，永远存不下来。单独给 2 分钟。
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), publicThumbBuildTimeout)
	defer cancel()
	body, thumb, err := s.openWithThumb(ctx, m, width)
	if err != nil {
		return repo.MediaObject{}, err
	}
	data, err := io.ReadAll(body)
	body.Close()
	if err != nil {
		return repo.MediaObject{}, httpx.Wrap(errcode.Internal, err)
	}
	if thumb.ContentType == m.ContentType && int64(len(data)) == m.SizeBytes {
		// 没缩（本来就比目标窄、或解不了）：直接用原图，不存一份一模一样的。
		return m, nil
	}

	ext := ".jpg"
	if thumb.ContentType == "image/png" {
		ext = ".png"
	}
	id := s.nextID()
	objKey := fmt.Sprintf("public_thumb/%s/%d%s", key, id, ext)
	if err := s.store.PutFile(ctx, objKey, bytes.NewReader(data), int64(len(data)), thumb.ContentType); err != nil {
		return repo.MediaObject{}, httpx.Wrap(errcode.Internal, fmt.Errorf("上传缩略图失败: %w", err))
	}
	row := repo.MediaObject{
		ID: id, WorkspaceID: m.WorkspaceID, Bucket: s.store.Bucket(), ObjectKey: objKey,
		ContentType: thumb.ContentType, SizeBytes: int64(len(data)), Purpose: PurposePublicThumbnail,
		CreatedByUserID: m.CreatedByUserID, SourceMediaID: m.ID,
		// ⚠️ derive_index 必须给值：唯一索引 uk_media_derive 含它，PG 里 null 彼此不相等，
		// 留空的话两个同时到达的首次请求会各存一份。
		DeriveKind: DerivePublicThumb, DeriveKey: key, DeriveIndex: 1,
	}
	if err := s.repo.Insert(ctx, row); err != nil {
		// 并发：另一个请求刚存了同一张。用它的，删掉自己这份。
		if t, ok, ferr := s.repo.FindDerived(ctx, m.ID, DerivePublicThumb, key); ferr == nil && ok {
			_ = s.store.Delete(ctx, objKey)
			return t, nil
		}
		return repo.MediaObject{}, httpx.Wrap(errcode.Internal, err)
	}
	return row, nil
}
