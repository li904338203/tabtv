package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/draw"
	_ "image/jpeg" // 源图可能是 JPEG
	"image/png"
	"io"

	genapp "github.com/li904338203/tabtv/backend/internal/generation/app"
	imagetooldomain "github.com/li904338203/tabtv/backend/internal/imagetool/domain"
	"github.com/li904338203/tabtv/backend/internal/provider/protocol"
)

// 取回产物之后、归档之前的工具后处理（docs/agent/imagetools IMGD-7，dev/01 R-2）。
//
// 目前只有情绪调节要做（R-5）：发给上游的是原图里框选人物所在的那一块（cropToolInput），回来的是这一块的
// 编辑结果；这里把它缩回裁切区域的尺寸、放回原图对应位置，再只把选中人物的框（外扩、羽化）贴回原图 ——
// 框外像素逐像素等于原图（IMGK-9），输出就是原图分辨率。放在协议层之后：协议不认识工具（IMGD-5）。
//
// ⚠️ 贴回失败按**任务失败**处理（调用方据此退款），不是「成功但没贴」——
// 后者会把一张连旁人都被改了表情的图当成功交给用户，而他付的正是「只改选中的人」的钱。

// finishTool 按 params.tool 分派后处理；不是需要后处理的工具原样返回。
func (p *HTTPProvider) finishTool(ctx context.Context, job genapp.Job, outs []genapp.ProviderOutput) (
	[]genapp.ProviderOutput, error) {
	var params map[string]any
	if json.Unmarshal(job.Params, &params) != nil || imagetooldomain.ToolOf(params) != imagetooldomain.ToolExpression {
		return outs, nil
	}
	out, err := p.compositeExpression(ctx, job, params, outs)
	if err != nil {
		closeOutputs(outs)
		return nil, fmt.Errorf("情绪调节贴回原图失败：%w", err)
	}
	return out, nil
}

func (p *HTTPProvider) compositeExpression(ctx context.Context, job genapp.Job, params map[string]any,
	outs []genapp.ProviderOutput) ([]genapp.ProviderOutput, error) {
	var opts imagetooldomain.ExpressionOptions
	raw, _ := json.Marshal(params[imagetooldomain.ParamToolOptions])
	if err := json.Unmarshal(raw, &opts); err != nil || len(opts.Roles) == 0 {
		return nil, fmt.Errorf("快照里没有人物框")
	}
	crop, ok := cropOf(params)
	if !ok {
		return nil, fmt.Errorf("快照里没有裁切区域")
	}
	srcID := toolSourceMediaID(job.Inputs)
	if srcID == 0 {
		return nil, fmt.Errorf("快照里没有源图")
	}
	src, err := p.decodeMedia(ctx, srcID)
	if err != nil {
		return nil, err
	}
	base := imagetooldomain.Resize(src, src.Bounds().Dx(), src.Bounds().Dy())
	boxes := make([]imagetooldomain.Box, 0, len(opts.Roles))
	for _, r := range opts.Roles {
		boxes = append(boxes, r.Box)
	}

	result := make([]genapp.ProviderOutput, 0, len(outs))
	for i, o := range outs {
		if o.Type != "image" || o.Body == nil {
			result = append(result, o)
			continue
		}
		edited, _, err := image.Decode(o.Body)
		closeOutput(o)
		if err != nil {
			closeOutputs(outs[i+1:])
			return nil, fmt.Errorf("第 %d 张产物解不开：%w", i, err)
		}
		// 编辑结果缩回裁切区域、放回原图的那个位置，得到一张「只有这一块变了」的整图，再按人物框贴回。
		full := imagetooldomain.Resize(base, base.Bounds().Dx(), base.Bounds().Dy())
		patch := imagetooldomain.Resize(edited, crop.W, crop.H)
		draw.Draw(full, image.Rect(crop.X, crop.Y, crop.X+crop.W, crop.Y+crop.H), patch, image.Point{}, draw.Src)
		var buf bytes.Buffer
		if err := png.Encode(&buf, imagetooldomain.Composite(base, full, boxes)); err != nil {
			closeOutputs(outs[i+1:])
			return nil, err
		}
		result = append(result, genapp.ProviderOutput{Type: "image", MIME: "image/png", Size: int64(buf.Len()),
			Body: bytes.NewReader(buf.Bytes()), Meta: o.Meta})
	}
	return result, nil
}

// cropToolInput 情绪调节：把发给上游的源图换成裁切区域那一块（内联 PNG，图片协议都认 data: 地址）。
// 不是情绪调节、或快照里没有裁切区域时不动。
func (p *HTTPProvider) cropToolInput(ctx context.Context, job genapp.Job, pjob *protocol.Job) error {
	var params map[string]any
	if json.Unmarshal(job.Params, &params) != nil || imagetooldomain.ToolOf(params) != imagetooldomain.ToolExpression {
		return nil
	}
	crop, ok := cropOf(params)
	if !ok {
		return fmt.Errorf("快照里没有裁切区域")
	}
	srcID := toolSourceMediaID(job.Inputs)
	if srcID == 0 {
		return fmt.Errorf("快照里没有源图")
	}
	src, err := p.decodeMedia(ctx, srcID)
	if err != nil {
		return err
	}
	b := src.Bounds()
	rect := image.Rect(b.Min.X+crop.X, b.Min.Y+crop.Y, b.Min.X+crop.X+crop.W, b.Min.Y+crop.Y+crop.H).Intersect(b)
	if rect.Empty() {
		return fmt.Errorf("裁切区域在源图之外")
	}
	part := image.NewRGBA(image.Rect(0, 0, rect.Dx(), rect.Dy()))
	draw.Draw(part, part.Bounds(), src, rect.Min, draw.Src)
	var buf bytes.Buffer
	if err := png.Encode(&buf, part); err != nil {
		return err
	}
	uri := "data:image/png;base64," + base64.StdEncoding.EncodeToString(buf.Bytes())
	for i := range pjob.Media {
		if pjob.Media[i].Kind == "image" && pjob.Media[i].Role != "mask" {
			pjob.Media[i].Value, pjob.Media[i].MIME = uri, "image/png"
		}
	}
	for i := range pjob.Images {
		pjob.Images[i] = uri
	}
	return nil
}

// cropOf 取快照里的裁切区域（params.toolCrop）。
func cropOf(params map[string]any) (imagetooldomain.Box, bool) {
	m, _ := params[imagetooldomain.ParamToolCrop].(map[string]any)
	num := func(k string) int { v, _ := m[k].(float64); return int(v) }
	c := imagetooldomain.Box{X: num("x"), Y: num("y"), W: num("w"), H: num("h")}
	return c, c.W > 0 && c.H > 0
}

// decodeMedia 取源图并解码（优先流式打开，回落内联）。
func (p *HTTPProvider) decodeMedia(ctx context.Context, mediaID int64) (image.Image, error) {
	if p.media == nil {
		return nil, fmt.Errorf("媒体解析器未接入")
	}
	var r io.Reader
	if o, ok := p.media.(MediaOpener); ok {
		body, _, err := o.Open(ctx, mediaID)
		if err != nil {
			return nil, fmt.Errorf("取源图 %d 失败：%w", mediaID, err)
		}
		defer body.Close()
		r = body
	} else {
		data, _, err := p.media.Inline(ctx, mediaID)
		if err != nil {
			return nil, fmt.Errorf("取源图 %d 失败：%w", mediaID, err)
		}
		r = bytes.NewReader(data)
	}
	img, _, err := image.Decode(r)
	if err != nil {
		return nil, fmt.Errorf("源图解不开：%w", err)
	}
	return img, nil
}

// toolSourceMediaID 快照里的源图：第一张非蒙版图片。
func toolSourceMediaID(inputs json.RawMessage) int64 {
	var list []struct {
		MediaID   int64  `json:"mediaId"`
		MediaKind string `json:"mediaKind"`
		Role      string `json:"role"`
	}
	_ = json.Unmarshal(inputs, &list)
	for _, in := range list {
		if in.MediaKind == "image" && in.MediaID != 0 && in.Role != "mask" {
			return in.MediaID
		}
	}
	return 0
}

func closeOutput(o genapp.ProviderOutput) {
	if c, ok := o.Body.(io.Closer); ok {
		_ = c.Close()
	}
}

func closeOutputs(outs []genapp.ProviderOutput) {
	for _, o := range outs {
		closeOutput(o)
	}
}
