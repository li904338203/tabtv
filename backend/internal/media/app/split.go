package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	_ "image/gif"  // 注册解码器：源图可能是 GIF
	_ "image/jpeg" // 同上
	"image/png"
	"io"

	"github.com/li904338203/tabtv/backend/internal/media/domain"
	"github.com/li904338203/tabtv/backend/internal/media/repo"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// 宫格切分（NEW-14）。
//
// ⚠️ 切分**不走生成链路**：没有模型、没有上游、没有费用 ——
// 它就是一次本地像素搬运。塞进 generation 的话会凭空多出一条
// 永远 0 积分、永远同步完成的"任务"，把任务列表和对账都搅乱。
//
// ⚠️ 切出来的每一块都是**独立媒体对象**，purpose 与上传参考相同，
// 因此立刻就能作为下一次生成的输入（NEW-14 要求"能继续引用"）。

// DeriveGridTile 是宫格切块的派生种类。
const DeriveGridTile = "grid_tile"

// maxSplitSourceBytes 允许进内存做切分的源图上限。
//
// ⚠️ 切分**必须**把整张图解进内存（要按坐标裁剪），这与上传走临时文件
// 是两回事。一张 8000×8000 的 PNG 解出来是 256MB 的像素缓冲，
// 并发几路就把进程撑爆 —— 所以这里要有一道自己的闸。
const maxSplitSourceBytes = 64 << 20

type SplitInput struct {
	WorkspaceID   int64
	UserID        int64
	SourceMediaID int64
	Rows          int
	Cols          int
}

// SplitTile 是返回给前端的一块。
type SplitTile struct {
	MediaID int64
	Index   int
	Row     int
	Col     int
	Width   int
	Height  int
	Label   string
}

type tileMeta struct {
	Rows int `json:"rows"`
	Cols int `json:"cols"`
	Row  int `json:"row"`
	Col  int `json:"col"`
	// 像素矩形，便于将来在原图上高亮某一块。
	X int `json:"x"`
	Y int `json:"y"`
	W int `json:"w"`
	H int `json:"h"`
}

// SplitGrid 把一张图切成 rows×cols 块，每块落成独立媒体对象。
//
// ⚠️ **幂等**：同一张源图、同一种格数重复调用返回已有的那一批，
// 不会越切越多。用户手抖点两下、网络重试都会走到这里。
func (s *Service) SplitGrid(ctx context.Context, in SplitInput) ([]SplitTile, error) {
	spec := domain.GridSpec{Rows: in.Rows, Cols: in.Cols}
	if err := spec.Validate(); err != nil {
		return nil, httpx.Errf(errcode.InvalidArgument, err.Error())
	}
	if s.store == nil {
		return nil, httpx.Errf(errcode.Internal, "对象存储未配置")
	}

	src, err := s.repo.GetScoped(ctx, in.WorkspaceID, in.SourceMediaID)
	if err != nil {
		// 不区分"不存在"与"不是你的"：区分开等于提供一个按 id 探测的接口。
		return nil, httpx.Err(errcode.NotFound)
	}
	key := gridKey(spec)

	// 幂等：已经切过就把那一批原样返回。
	if existing, err := s.repo.ListDerived(ctx, in.WorkspaceID, src.ID, DeriveGridTile, key); err == nil &&
		len(existing) == spec.Rows*spec.Cols {
		return toTiles(existing, spec), nil
	}

	if !splittable(src.ContentType) {
		return nil, httpx.Errf(errcode.UnsupportedMediaType,
			fmt.Sprintf("暂不支持切分 %s（可切分 PNG / JPEG / GIF）", src.ContentType))
	}
	if src.SizeBytes > maxSplitSourceBytes {
		return nil, httpx.Errf(errcode.PayloadTooLarge,
			fmt.Sprintf("源图超过 %d MB，暂不支持切分", maxSplitSourceBytes>>20))
	}

	body, _, err := s.Open(ctx, src.ID)
	if err != nil {
		return nil, err
	}
	raw, err := io.ReadAll(io.LimitReader(body, maxSplitSourceBytes+1))
	_ = body.Close()
	if err != nil {
		return nil, httpx.Wrap(errcode.Internal, err)
	}
	if int64(len(raw)) > maxSplitSourceBytes {
		return nil, httpx.Errf(errcode.PayloadTooLarge, "源图过大，暂不支持切分")
	}

	img, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		// ⚠️ 解不出来要**明说**，不能像缩略图那样"原样返回"：
		// 缩略图解不了还能显示原图，切分解不了就是什么都没有 ——
		// 静默成功会让用户盯着一个空的结果区等下去。
		return nil, httpx.Errf(errcode.UnsupportedMediaType, "无法解码这张图片，切分失败")
	}
	b := img.Bounds()
	tiles, err := domain.SplitGrid(b.Dx(), b.Dy(), spec)
	if err != nil {
		return nil, httpx.Errf(errcode.InvalidArgument, err.Error())
	}

	// 切块统一编码成 PNG。
	//
	// ⚠️ 用无损格式而不是跟随源格式：切块是要拿去当参考图继续生成的，
	// 而 JPEG 每存一次就掉一层画质 —— 切九块等于给九张参考图各加一层压缩噪点。
	// 代价是体积变大，但这些块的生命周期通常很短。
	out := make([]SplitTile, 0, len(tiles))
	for _, t := range tiles {
		sub := cropTo(img, t.Rect)
		var buf bytes.Buffer
		if err := png.Encode(&buf, sub); err != nil {
			return nil, httpx.Wrap(errcode.Internal, err)
		}
		meta, _ := json.Marshal(tileMeta{
			Rows: spec.Rows, Cols: spec.Cols, Row: t.Row, Col: t.Col,
			X: t.Rect.Min.X, Y: t.Rect.Min.Y, W: t.Rect.Dx(), H: t.Rect.Dy(),
		})

		up, err := s.Upload(ctx, UploadInput{
			WorkspaceID: in.WorkspaceID, UserID: in.UserID,
			// ⚠️ 与上传参考同一个 purpose：切块要能立刻出现在"我的素材"里
			// 并作为下一次生成的输入。另起一个 purpose 的话，
			// 每一处按 purpose 过滤的地方都要再加一个分支才认得它。
			Purpose:      "upload_reference",
			DeclaredMIME: "image/png", Size: int64(buf.Len()),
			Body: bytes.NewReader(buf.Bytes()), ExpectKind: domain.KindImage,
			Derive: &DeriveInfo{
				SourceMediaID: src.ID, Kind: DeriveGridTile,
				Key: key, Index: t.Index, Meta: meta,
			},
		})
		if err != nil {
			return nil, err
		}
		out = append(out, SplitTile{
			MediaID: up.ID, Index: t.Index, Row: t.Row, Col: t.Col,
			Width: t.Rect.Dx(), Height: t.Rect.Dy(),
			Label: domain.TileLabel(t, spec),
		})
	}
	return out, nil
}

// ListGridTiles 列出已切好的块（前端刷新后要能拿回来）。
func (s *Service) ListGridTiles(ctx context.Context, workspaceID, sourceID int64,
	rows, cols int) ([]SplitTile, error) {
	spec := domain.GridSpec{Rows: rows, Cols: cols}
	if err := spec.Validate(); err != nil {
		return nil, httpx.Errf(errcode.InvalidArgument, err.Error())
	}
	list, err := s.repo.ListDerived(ctx, workspaceID, sourceID, DeriveGridTile, gridKey(spec))
	if err != nil {
		return nil, httpx.Wrap(errcode.Internal, err)
	}
	return toTiles(list, spec), nil
}

func toTiles(list []repo.MediaObject, spec domain.GridSpec) []SplitTile {
	out := make([]SplitTile, 0, len(list))
	for _, m := range list {
		var meta tileMeta
		_ = json.Unmarshal(m.DeriveMeta, &meta)
		out = append(out, SplitTile{
			MediaID: m.ID, Index: m.DeriveIndex, Row: meta.Row, Col: meta.Col,
			Width: m.Width, Height: m.Height,
			Label: domain.TileLabel(domain.Tile{
				Index: m.DeriveIndex, Row: meta.Row, Col: meta.Col,
			}, spec),
		})
	}
	return out
}

func gridKey(g domain.GridSpec) string {
	return fmt.Sprintf("%dx%d", g.Rows, g.Cols)
}

func splittable(mime string) bool {
	// 与 domain.ImageSize 能认的格式保持一致：标准库有解码器的那三种。
	// WebP 没有解码器，明确拒绝而不是静默失败。
	return mime == "image/png" || mime == "image/jpeg" || mime == "image/gif"
}

// cropTo 按矩形裁出子图。
//
// ⚠️ `SubImage` 返回的是**共享底层像素的视图**，它的 Bounds 仍带着原图坐标。
// 直接拿去编码，多数解码器读出来的宽高是对的，但坐标原点不是 (0,0) ——
// 少数工具会因此把图画歪。拷到一张新图上，原点归零，行为确定。
func cropTo(src image.Image, r image.Rectangle) image.Image {
	if sub, ok := src.(interface {
		SubImage(image.Rectangle) image.Image
	}); ok {
		cropped := sub.SubImage(r)
		dst := image.NewNRGBA(image.Rect(0, 0, r.Dx(), r.Dy()))
		drawInto(dst, cropped, r.Min)
		return dst
	}
	dst := image.NewNRGBA(image.Rect(0, 0, r.Dx(), r.Dy()))
	drawInto(dst, src, r.Min)
	return dst
}

func drawInto(dst *image.NRGBA, src image.Image, origin image.Point) {
	b := dst.Bounds()
	for y := 0; y < b.Dy(); y++ {
		for x := 0; x < b.Dx(); x++ {
			dst.Set(x, y, src.At(origin.X+x, origin.Y+y))
		}
	}
}
