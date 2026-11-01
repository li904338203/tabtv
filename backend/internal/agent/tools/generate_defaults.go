package tools

import (
	"context"
	"strings"

	"github.com/li904338203/tabtv/backend/internal/agent/app"
	"github.com/li904338203/tabtv/backend/internal/agent/domain"
)

// 新建节点的画幅 / 清晰度：模型没给时由服务端补（会话 107398945801502720）。
//
// 那次的 generate 没带画幅：节点上没有，受理参数里也没有。UniArt 的 seedance-2.5 把它当 auto，而该能力集
// 不支持 auto，直接 400；模型下一轮补了 9:16 才通过。节点默认值由 canvasgen 按目录补（与参数面板同一份），
// 但目录默认是 16:9 —— 一支「竖屏 9:16」的带货视频会被悄悄做成横屏，而任务是成功的。所以在那之前，
// 先按本会话里更具体的来源补：
//
//  1. 用户的选择：输入框偏好里选定的、或问答卡里确认过的画幅 / 清晰度（app.VideoSpecState，与「创作偏好」一节同一份）；
//  2. 营销视频条目：所选创意的推荐画幅（「营销输入」里写给模型的那一份）—— 只是推荐，排在用户的选择后面；
//  3. 都没有：交给 canvasgen 按节点默认补。
//
// 只给**新建节点**补、只补**模型没给**的键：已有节点上的值是用户设的；模型给了就以模型为准（用户在对话里
// 改了主意，模型照着改，服务端不该把它顶回去）。时长不在这里补：时长决定扣多少钱，以计划 / 报价为准。

// ratioKeys 是模型可能写画幅的键（canvasgen 归一时都认成 aspect_ratio）。
var ratioKeys = []string{"aspect_ratio", "aspectRatio", "ratio"}

// hasParam：任一键有非空值即算给了；老形状 size.{ratio,clarity} 里的也算（协议同样会读它）。
func hasParam(params map[string]any, nested string, keys ...string) bool {
	for _, k := range keys {
		if nonEmpty(params[k]) {
			return true
		}
	}
	if size, ok := params["size"].(map[string]any); ok && nested != "" {
		return nonEmpty(size[nested])
	}
	return false
}

func nonEmpty(v any) bool {
	if v == nil {
		return false
	}
	if s, ok := v.(string); ok {
		return strings.TrimSpace(s) != ""
	}
	return true
}

// fillGenerateDefaults 按上面的顺序补画幅 / 清晰度（输入 map 不被修改）。creativeRatio 只对视频条目给。
func fillGenerateDefaults(params map[string]any, nodeType, creativeRatio string, spec app.VideoSpecState) map[string]any {
	if nodeType != "video" && nodeType != "image" {
		return params
	}
	out := make(map[string]any, len(params)+2)
	for k, v := range params {
		out[k] = v
	}
	if !hasParam(out, "ratio", ratioKeys...) {
		ratio := spec.Ratio
		if ratio == "" && nodeType == "video" {
			ratio = creativeRatio
		}
		if ratio != "" {
			out["aspect_ratio"] = ratio
		}
	}
	if nodeType == "video" && spec.Resolution != "" && !hasParam(out, "clarity", "resolution") {
		out["resolution"] = spec.Resolution
	}
	return out
}

// creativeRatio 是营销视频条目所选创意的推荐画幅。取不到不算错：由偏好 / 节点默认兜底。
func (t Generate) creativeRatio(ctx context.Context, item domain.PlanItem) string {
	if item.Kind != "video" {
		return ""
	}
	src, ok := t.Plans.(app.CreativeRatioSource)
	if !ok {
		return ""
	}
	for _, p := range item.Picks {
		if p.Kind != "creative" || p.Code == "" {
			continue
		}
		r, err := src.CreativeRatio(ctx, p.Code)
		if err != nil {
			t.logger().Info("取创意的推荐画幅失败，按偏好 / 节点默认补", "creative", p.Code, "err", err)
			return ""
		}
		return strings.TrimSpace(r)
	}
	return ""
}
