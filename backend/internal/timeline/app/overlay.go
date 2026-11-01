package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"os"
	"path/filepath"

	"github.com/li904338203/tabtv/backend/internal/timeline/domain"
	"github.com/li904338203/tabtv/backend/internal/timeline/typeset"
)

// 文字与字幕叠加流（docs/agent/smartcut SCT-14，SCD-28）：把成片时间切成「可见集合不变」的区间，
// 每个区间合成一张整幅透明 PNG，用 concat 分离器按时长拼成一路带透明的流，在水印之前 overlay 一次。
//
// 不再走 SRT + `subtitles` 滤镜：那要 libass，本机与不少部署镜像的 ffmpeg 都没有，于是带字幕的时间线一律被预检拦下；
// 字幕样式也进不了成片（SCB-7）。现在条目用服务端排版（与 POST /smartcut/typeset 同一个函数、同一个缓存键），
// 预览里看到的那张图就是成片里叠的那张。

// subtitleStackGapPct 多条字幕轨上下叠放时，两块之间留的间距（画面高度的百分比）。
const subtitleStackGapPct = 1.0

// buildOverlay 合成叠加流，返回 concat 脚本的路径；没有任何文字与字幕时返回空（argv 里不加这路输入）。
//
// ⚠️ 区间多（几千条字幕约四千个区间）时逐个合成、编码、写文件，不整批留在内存里：
// 一张 1080×1920 的整幅图约 8MB。PNG 按内容哈希去重，同一张图只写一次，脚本里反复引用；
// 所有没字的区间共用一张全透明图。
func (x *Renderer) buildOverlay(ctx context.Context, dir string, snap domain.RenderSnapshot, totalMs int) (string, error) {
	segs := domain.OverlaySegments(snap, totalMs)
	any := false
	for _, s := range segs {
		if len(s.Items) > 0 {
			any = true
			break
		}
	}
	if !any {
		return "", nil
	}
	if x.typeset == nil {
		return "", errors.New("渲染进程没有接上排版器，文字与字幕出不来")
	}
	written := map[string]string{}
	files := make([]string, 0, len(segs))
	durs := make([]int, 0, len(segs))
	for _, seg := range segs {
		img, err := x.composeSegment(ctx, snap, seg)
		if err != nil {
			return "", err
		}
		var buf bytes.Buffer
		enc := png.Encoder{CompressionLevel: png.BestSpeed}
		if err := enc.Encode(&buf, img); err != nil {
			return "", err
		}
		sum := sha256.Sum256(buf.Bytes())
		key := hex.EncodeToString(sum[:8])
		path, ok := written[key]
		if !ok {
			path = filepath.Join(dir, "ovl-"+key+".png")
			if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
				return "", err
			}
			written[key] = path
		}
		files = append(files, path)
		durs = append(durs, seg.EndMs-seg.StartMs)
	}
	list := filepath.Join(dir, "overlay.txt")
	if err := os.WriteFile(list, []byte(domain.OverlayConcatScript(files, durs)), 0o600); err != nil {
		return "", err
	}
	return list, nil
}

// composeSegment 一个区间的整幅透明图：字幕按轨道从下往上叠（第一条在最下方，后面的让出前面各条的高度），
// 文字层按位置框摆。
//
// ⚠️ 叠放靠**整块往上挪**，不改样式里的 bottomPct 重排：条目 PNG 与缓存键要和排版预览接口完全相同
// （SC-AC-17「预览接口与渲染用的条目哈希相同」）。
func (x *Renderer) composeSegment(ctx context.Context, snap domain.RenderSnapshot, seg domain.OverlaySegment) (
	image.Image, error) {
	if len(seg.Items) == 0 {
		return domain.ComposeOverlay(snap.Width, snap.Height, nil), nil
	}
	var placed []domain.PlacedImage
	var subs []*typeset.Result
	var subBottoms []float64
	for _, it := range seg.Items {
		style := objectMap(it.Style)
		req := typeset.Request{Text: it.Text, Kind: it.Kind, Style: style, Width: snap.Width, Height: snap.Height}
		if it.Kind == domain.OverlayText {
			req.Box = objectMap(it.Box)
		}
		res, err := x.typeset.Typeset(ctx, req)
		if err != nil {
			if errors.Is(err, typeset.ErrFontsUnavailable) {
				return nil, fmt.Errorf("排版字体未就绪，文字与字幕出不来（运维：scripts/fetch-fonts.sh）: %w", err)
			}
			return nil, fmt.Errorf("排版「%s」失败: %w", clip(it.Text, 20), err)
		}
		img, err := png.Decode(bytes.NewReader(res.PNG))
		if err != nil {
			return nil, fmt.Errorf("排版结果解不开: %w", err)
		}
		if it.Kind == domain.OverlaySubtitle {
			subs = append(subs, res)
			subBottoms = append(subBottoms, typeset.ParseStyle(typeset.KindSubtitle, style, nil).BottomPct)
		}
		placed = append(placed, domain.PlacedImage{Img: img, X: res.Box.X, Y: res.Box.Y})
	}
	// 字幕叠放：第一条留在它自己的下边距，后面每条往上让出前面各条的高度加间距。
	if len(subs) > 1 {
		heights := make([]float64, len(subs))
		for i, r := range subs {
			heights[i] = r.BoxPct.H
		}
		stacked := domain.StackBottomPct(subBottoms[0], heights, subtitleStackGapPct)
		k := 0
		for i, it := range seg.Items {
			if it.Kind != domain.OverlaySubtitle {
				continue
			}
			if up := stacked[k] - subBottoms[k]; up > 0 {
				placed[i].Y -= int(up * float64(snap.Height) / 100)
			}
			k++
		}
	}
	return domain.ComposeOverlay(snap.Width, snap.Height, placed), nil
}

func objectMap(raw json.RawMessage) map[string]any {
	var m map[string]any
	if len(raw) == 0 || json.Unmarshal(raw, &m) != nil {
		return nil
	}
	return m
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
