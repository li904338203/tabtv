package app

import (
	"encoding/json"
	"errors"

	imagetooldomain "github.com/li904338203/tabtv/backend/internal/imagetool/domain"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// 图片节点工具条在受理侧的一步（docs/agent/imagetools IMGD-2/3/11，R-5）。
//
// ⚠️ 翻译结果（`prompt` / `toolCrop` 等）**只由这里写**，客户端传来的服务端专属键一律丢弃：
// 能直接写裁切区域的话就能让 Worker 裁出任意一块、贴回到任意位置。
// 写进 params 之后随快照冻结：之后改翻译规则不影响已受理的任务（IMG-F-73）。

// toolCodeOf 取工具码；不是工具节点返回空串。
func toolCodeOf(params json.RawMessage) string {
	var m map[string]any
	if len(params) == 0 || json.Unmarshal(params, &m) != nil {
		return ""
	}
	return imagetooldomain.ToolOf(m)
}

// toolStep 校验并翻译一次工具调用，返回翻译结果与改写后的 params。
// inputs 是服务端解析过的输入（可信的媒体 ID 与尺寸），不信客户端报的源图。
//
// 翻译结果写进 params：prompt（服务端拼的提示词）、aspect_ratio / quality（图生图通用参数，覆盖节点上残留的值）、
// toolCrop（情绪调节的裁切区域，Worker 按它裁出局部发给上游、再贴回原图）。
func toolStep(code string, params, inputs json.RawMessage) (imagetooldomain.Prepared, json.RawMessage, error) {
	var m map[string]any
	if err := json.Unmarshal(params, &m); err != nil {
		return imagetooldomain.Prepared{}, nil, httpx.Errf(errcode.InvalidArgument, "params 不是合法的对象")
	}
	for _, k := range imagetooldomain.ServerOwnedKeys {
		delete(m, k)
	}
	// 工具节点不带预设：工具的配方是固定的，留着 preset 的话受理会把预设段也拼进去。
	delete(m, "preset")
	delete(m, "presetOptions")

	src, n := toolSource(inputs)
	if n != 1 {
		return imagetooldomain.Prepared{}, nil, httpx.Errf(errcode.InvalidArgument,
			"这个工具需要恰好一张上游图片").WithDetails(map[string]any{"path": "inputs", "images": n})
	}
	t, err := imagetooldomain.Prepare(code, m[imagetooldomain.ParamToolOptions], src)
	if err != nil {
		return imagetooldomain.Prepared{}, nil, toolError(err)
	}
	m[imagetooldomain.ParamToolOptions] = t.Options
	m[imagetooldomain.ParamPrompt] = t.Prompt
	delete(m, "aspect_ratio")
	for k, v := range t.Params {
		m[k] = v
	}
	m["count"] = t.Count
	if t.Crop != nil {
		m[imagetooldomain.ParamToolCrop] = map[string]any{"x": t.Crop.X, "y": t.Crop.Y, "w": t.Crop.W, "h": t.Crop.H}
	}
	out, err := json.Marshal(m)
	if err != nil {
		return imagetooldomain.Prepared{}, nil, httpx.Wrap(errcode.Internal, err)
	}
	return t, out, nil
}

// toolSource 从解析过的输入里取源图：图片、非蒙版、带媒体 ID 的那一张。返回图片张数（应恰好 1）。
func toolSource(inputs json.RawMessage) (imagetooldomain.SourceFacts, int) {
	var list []struct {
		MediaID   int64  `json:"mediaId"`
		MediaKind string `json:"mediaKind"`
		Role      string `json:"role"`
		Width     int    `json:"width"`
		Height    int    `json:"height"`
	}
	_ = json.Unmarshal(inputs, &list)
	var src imagetooldomain.SourceFacts
	n := 0
	for _, in := range list {
		if in.MediaKind != "image" || in.MediaID == 0 || in.Role == "mask" {
			continue
		}
		n++
		if n == 1 {
			src = imagetooldomain.SourceFacts{MediaID: in.MediaID, Width: in.Width, Height: in.Height}
		}
	}
	return src, n
}

// toolError 把工具包的错误换成业务码：字段错指到路径；源图变了有自己的码，前端据此提示重新框选。
func toolError(err error) error {
	var fe *imagetooldomain.FieldError
	var sc *imagetooldomain.ErrSourceChanged
	switch {
	case errors.As(err, &sc):
		return httpx.Errf(errcode.ToolSourceChanged, sc.Error()).WithDetails(map[string]any{
			"expectedMediaId": itoa(sc.Want), "actualMediaId": itoa(sc.Got)})
	case errors.As(err, &fe):
		return httpx.Errf(errcode.InvalidArgument, fe.Error()).WithDetails(map[string]any{"path": fe.Path})
	}
	return httpx.Errf(errcode.InvalidArgument, err.Error())
}

// stripServerOwned 删掉客户端传来的服务端专属键（upstream / output）。解不开的参数原样返回（后面的校验会报）。
func stripServerOwned(params json.RawMessage) json.RawMessage {
	var m map[string]any
	if len(params) == 0 || json.Unmarshal(params, &m) != nil || m == nil {
		return params
	}
	changed := false
	for _, k := range imagetooldomain.ServerOwnedKeys {
		if _, ok := m[k]; ok {
			delete(m, k)
			changed = true
		}
	}
	if !changed {
		return params
	}
	out, err := json.Marshal(m)
	if err != nil {
		return params
	}
	return out
}
