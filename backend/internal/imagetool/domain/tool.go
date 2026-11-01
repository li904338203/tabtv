// Package domain 是图片节点工具条的纯计算（docs/agent/imagetools IMGD-1，2026-10-31 方案变更后见 dev/01 R-5）：
// 工具码、选项校验、把界面上的结构化选项翻译成**提示词 + 图生图的常规参数**、情绪调节的裁切与贴回。
//
// 2026-10-31 起所有工具都用现有图片大模型走图生图（img2img）：不接专用模型，翻译结果只有提示词与
// aspect_ratio / quality 这类通用参数，任何图片协议都认得。
//
// ⚠️ 只依赖标准库（与 golang.org/x/image）：不碰库、不发网络。受理（校验 + 冻进快照）、报价、整图计划器
// 三处共用这一份 —— 各写一份的话，「界面上说多少钱 / 发出去的是什么」早晚对不上。
package domain

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
)

// 工具码（`params.tool`）。它们不是操作码：工具子节点的操作一律是图生图（OpImg2Img）。
const (
	ToolMultiAngle = "multi_angle"
	ToolRelight    = "relight"
	ToolExpression = "expression_edit"
	ToolUpscale    = "upscale"
)

// OpImg2Img 工具子节点的操作码：所有工具都是「原图 + 服务端拼的提示词」的一次图生图。
const OpImg2Img = "img2img"

// 参数键。`output` / `toolCrop` 只能由服务端写（IMGD-2）：客户端传来的同名键一律丢弃。
const (
	ParamTool        = "tool"
	ParamToolOptions = "toolOptions"
	ParamOutput      = "output"
	ParamPrompt      = "prompt"
	// ParamToolCrop 情绪调节的裁切区域（原图像素）：Worker 按它裁出局部发给上游，再贴回原图。
	ParamToolCrop = "toolCrop"
)

// ServerOwnedKeys 是只能由服务端写的 params 键（`upstream` 是旧方案的键，仍一律丢弃）。
var ServerOwnedKeys = []string{"upstream", ParamOutput, ParamToolCrop}

var tools = map[string]bool{ToolMultiAngle: true, ToolRelight: true, ToolExpression: true, ToolUpscale: true}

// IsTool 判定一个码是不是工具码。
func IsTool(code string) bool { return tools[code] }

// ToolOf 取节点参数里的工具码；不是工具节点返回空串。
func ToolOf(params map[string]any) string {
	if t, _ := params[ParamTool].(string); IsTool(t) {
		return t
	}
	return ""
}

// OperationOf 按节点参数定操作码（IMGD-3）：工具节点一律图生图，否则返回空串（交给常规判定）。
//
// ⚠️ 受理与整图计划器都只认它，不信客户端报的 operation。
func OperationOf(params map[string]any) string {
	if ToolOf(params) != "" {
		return OpImg2Img
	}
	return ""
}

// ToolInfo 是工具的展示信息（`/image-tools`、`/canvas/config` 用）。
type ToolInfo struct {
	Code string `json:"code"`
	Name string `json:"name"`
	// Spawn 是派生子节点的默认标题。
	Spawn string `json:"spawn"`
}

// Tools 列出四个工具（顺序即工具条上的顺序）。
func Tools() []ToolInfo {
	return []ToolInfo{
		{Code: ToolExpression, Name: "情绪调节", Spawn: "情绪调节"},
		{Code: ToolMultiAngle, Name: "多角度", Spawn: "多角度"},
		{Code: ToolRelight, Name: "打光", Spawn: "打光"},
		{Code: ToolUpscale, Name: "高清", Spawn: "高清"},
	}
}

// SourceFacts 是源图的事实（受理侧查好传进来）。宽高未知为 0。
type SourceFacts struct {
	MediaID int64
	Width   int
	Height  int
}

// Prepared 是一次工具调用翻译好的结果，原样冻进任务快照。
type Prepared struct {
	Tool string
	// Options 规范化之后的选项（回写 params.toolOptions：界面回显与快照都用它）。
	Options map[string]any
	// Prompt 服务端拼好的提示词（写进 params.prompt）。
	Prompt string
	// Params 合进 params 的通用图生图参数（aspect_ratio / quality）。
	Params map[string]any
	// Count 产出张数。
	Count int
	// Crop 情绪调节的裁切区域（原图像素）；其余工具为 nil。
	Crop *Box
}

// FieldError 是选项校验失败：Path 指到具体字段（如 `toolOptions.roles[2].box`）。
type FieldError struct {
	Path string
	Msg  string
}

func (e *FieldError) Error() string { return e.Path + "：" + e.Msg }

func fieldErr(path, format string, args ...any) error {
	return &FieldError{Path: "toolOptions." + path, Msg: fmt.Sprintf(format, args...)}
}

// Prepare 校验并翻译一次工具调用。raw 是客户端给的 `params.toolOptions`（任意 JSON 形状）。
func Prepare(tool string, raw any, src SourceFacts) (Prepared, error) {
	b, err := json.Marshal(raw)
	if err != nil {
		return Prepared{}, fieldErr("", "不是合法的 JSON")
	}
	if string(b) == "null" {
		b = []byte("{}")
	}
	var p Prepared
	switch tool {
	case ToolMultiAngle:
		var o MultiAngleOptions
		if err := strictDecode(b, &o); err != nil {
			return Prepared{}, err
		}
		p, err = o.prepare()
	case ToolRelight:
		var o RelightOptions
		if err := strictDecode(b, &o); err != nil {
			return Prepared{}, err
		}
		p, err = o.prepare()
	case ToolExpression:
		var o ExpressionOptions
		if err := strictDecode(b, &o); err != nil {
			return Prepared{}, err
		}
		return o.prepare(src)
	case ToolUpscale:
		var o UpscaleOptions
		if err := strictDecode(b, &o); err != nil {
			return Prepared{}, err
		}
		p, err = o.prepare()
	default:
		return Prepared{}, &FieldError{Path: "tool", Msg: "未知工具 " + tool}
	}
	if err != nil {
		return Prepared{}, err
	}
	p.Params = imageParams(src.Width, src.Height)
	return p, nil
}

// CountOf 只算张数（报价用：不需要源图尺寸）。选项坏了按 1 张，交给受理去报错。
func CountOf(tool string, raw any) int {
	if tool != ToolExpression {
		return 1
	}
	m, _ := raw.(map[string]any)
	if c, ok := m["count"].(float64); ok && c >= 1 && c <= MaxCount {
		return int(c)
	}
	return 1
}

// strictDecode 未知字段报错：键名写错时静默忽略的话请求 200 而什么都没生效（CLAUDE.md §5.10 末条）。
func strictDecode(b []byte, v any) error {
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		// 报错要指到具体字段、说中文：前端按 details.path 定位，json 包的英文原文用户看不懂。
		msg := err.Error()
		if i := strings.Index(msg, `unknown field "`); i >= 0 {
			name := strings.TrimSuffix(msg[i+len(`unknown field "`):], `"`)
			return fieldErr(name, "不认识的选项 %s", name)
		}
		var te *json.UnmarshalTypeError
		if errors.As(err, &te) && te.Field != "" {
			return fieldErr(te.Field, "类型不对（应为 %s）", te.Type.String())
		}
		return &FieldError{Path: "toolOptions", Msg: "选项格式不对"}
	}
	return nil
}

func toMap(v any) map[string]any {
	b, _ := json.Marshal(v)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	return m
}

// StandardRatios 是通用的画幅取值（各图片协议都认得的写法）。
var StandardRatios = []string{"1:1", "4:3", "3:4", "3:2", "2:3", "16:9", "9:16", "21:9"}

// NearestRatio 按宽高给出最接近的标准画幅（按比值的对数距离）；尺寸未知返回空串（交给模型缺省）。
func NearestRatio(w, h int) string {
	if w <= 0 || h <= 0 {
		return ""
	}
	target := math.Log(float64(w) / float64(h))
	best, bestD := "", math.Inf(1)
	for _, r := range StandardRatios {
		var a, b float64
		_, _ = fmt.Sscanf(r, "%f:%f", &a, &b)
		if d := math.Abs(math.Log(a/b) - target); d < bestD {
			best, bestD = r, d
		}
	}
	return best
}

// imageParams 工具出图的通用参数：画幅跟着源图（或裁切区域）、画质取高。
func imageParams(w, h int) map[string]any {
	out := map[string]any{"quality": "high"}
	if r := NearestRatio(w, h); r != "" {
		out["aspect_ratio"] = r
	}
	return out
}

// PlanEstimate 给整图计划器的预估张数。
func PlanEstimate(params map[string]any) int {
	return CountOf(ToolOf(params), params[ParamToolOptions])
}
