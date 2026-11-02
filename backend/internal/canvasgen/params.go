package canvasgen

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

// 「画布上生成」写进节点的参数要用**画布的参数编码**（AGT-24 缺陷修复，2026-10-16；
// MCPT-08 自 agent/tools/generate_params.go 原样搬入，Agent 与 MCP 共用）。
//
// ⚠️ 节点 params 的键就是参数面板读的键（`param_def.code`，视频模式例外：`video_mode` 在节点上存为
// `mode`，同前端 `utils/paramLayout.paramKey`）。模型惯用驼峰（`aspectRatio`）、时长又只作为受理参数
// `durationSec` 传，节点上于是没有面板认得的键 —— 面板回落默认值（16:9 / 5 秒），与实际 9:16 / 15 秒
// 不符，用户从节点重跑就生成了错误规格，而任务本身一切正常。
//
// 编码以目录为准（本包的 `NodeParams` 端口，即 node_param 绑定）；目录取不到时退回下面的副本。
// 归一只做「把能对上画布编码的键改成画布编码」，其余键原样保留（GO-35，配置可以比代码新）。

// fallbackNodeParamCodes 是 `internal/platform/seed/param.go` 绑定表的**副本**（节点上的键，已把
// video_mode 换成 mode）。只在目录取不到时用；以 seed / 后台配置为准，改 seed 时顺手同步
// （副本的位置：`internal/canvasgen/params.go`，MCPT-08 前在 `agent/tools/generate_params.go`）。
var fallbackNodeParamCodes = map[string][]string{
	"text":  {"prompt", "model"},
	"image": {"prompt", "preset", "reference", "model", "quality", "aspect_ratio", "count", "style_ref", "negative_prompt", "seed"},
	"video": {"mode", "prompt", "model", "duration", "resolution", "aspect_ratio", "effect_ref", "seed"},
	"audio": {"prompt", "voice", "model", "speed"},
	// 3D 导演台（DRT-05）：AI 搭景面板的描述与参考图。
	"director": {"prompt", "reference"},
}

// paramAliases 是驼峰转下划线推不出来的别名（老前端的 `ratio`、受理参数名 `durationSec`）。
var paramAliases = map[string]string{
	"ratio":        "aspect_ratio",
	"durationSec":  "duration",
	"duration_sec": "duration",
	"videoMode":    "mode",
	"video_mode":   "mode",
}

// nodeParamKey 与前端 paramKey 同一条规则：参数编码 → 节点上的键。
func nodeParamKey(code string) string {
	if code == "video_mode" {
		return "mode"
	}
	return code
}

// canvasParamKeys 把目录给的编码换成节点上的键集合；目录为空时退回副本。
// nodeType 为空（改已有节点却读不到它的类型）时取所有类型的并集。
func canvasParamKeys(nodeType string, codes []string) map[string]bool {
	keys := map[string]bool{}
	for _, c := range codes {
		if c = strings.TrimSpace(c); c != "" {
			keys[nodeParamKey(c)] = true
		}
	}
	if len(keys) > 0 {
		return keys
	}
	if nodeType != "" {
		for _, c := range fallbackNodeParamCodes[nodeType] {
			keys[c] = true
		}
		return keys
	}
	for _, list := range fallbackNodeParamCodes {
		for _, c := range list {
			keys[c] = true
		}
	}
	return keys
}

// snakeCase：`aspectRatio` → `aspect_ratio`；已是下划线的原样返回。
func snakeCase(s string) string {
	var b strings.Builder
	for i, r := range s {
		if unicode.IsUpper(r) {
			if i > 0 {
				b.WriteByte('_')
			}
			b.WriteRune(unicode.ToLower(r))
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// NormalizeGenerateParams 把 generate 的参数归一成画布节点的形状（纯函数，输入 map 不被修改）。
//
//   - 能对上画布编码的键改名（别名 → 驼峰转下划线），画布编码本身已给时**以它为准**并丢掉别名；
//   - 时长：`durationSec`（受理参数）与 `params.duration` 统一 —— 两者都给时以 durationSec 为准
//     （计价按它算，节点上的必须与扣的钱一致）；只给了 params.duration 时反过来补 durationSec，
//     否则计价按默认 5 秒、上游按 15 秒，扣的积分和实际时长对不上。节点上存**字符串**（控件选项值是 "15"）。
//   - 张数（count）同理，只在该节点类型有 count 参数时处理。
//   - 视频节点 `mode` = 操作码（与「直接生成」一致，视频的操作由 mode 决定）。
func NormalizeGenerateParams(nodeType, operationCode string, params map[string]any, codes []string,
	durationSec, count int) (map[string]any, int, int) {
	keys := canvasParamKeys(nodeType, codes)
	out := make(map[string]any, len(params)+2)
	for k, v := range params {
		out[k] = v
	}
	names := make([]string, 0, len(params))
	for k := range params {
		names = append(names, k)
	}
	sort.Strings(names) // 两个别名指向同一编码时结果要确定
	for _, k := range names {
		canonical, ok := paramAliases[k]
		if !ok {
			canonical = snakeCase(k)
		}
		if canonical == k || !keys[canonical] {
			continue
		}
		if _, exists := out[canonical]; !exists {
			out[canonical] = out[k]
		}
		delete(out, k)
	}

	durationSec = syncIntParam(out, keys, "duration", durationSec)
	count = syncIntParam(out, keys, "count", count)
	if nodeType == "video" && operationCode != "" {
		out["mode"] = operationCode
	}
	return out, durationSec, count
}

// FillNodeDefaults 给**新建**节点补上调用方没给的参数默认值（纯函数，输入 map 不被修改）。
//
// ⚠️ 与参数面板挂载时补默认值是同一条规则（ParamPanel：节点上没有这个键才补，值取 GET /canvas/config 的
// defaultValue）。Agent / MCP 建的节点原先不补：受理参数里没有画幅，上游按它自己的默认走 —— 有的型号
// 不支持那个默认，直接 400（会话 107398945801502720）；有的则悄悄出了它自己的默认规格，而用户打开节点时
// 面板补上的是 16:9，节点上显示的与实际发出去的对不上，从节点重跑又是另一种规格。
//
//   - 只补**缺的键**：键已按画布编码归一，aspectRatio 之类的别名不会被默认值顶掉；
//   - 只给**新建节点**补（调用方判断）：已有节点上的值是用户设的，补默认值会在 merge patch 里把它盖掉；
//   - 时长 / 张数是补上来的就同步回受理值（计价按它算，节点上的必须与扣的钱一致）。
func FillNodeDefaults(params map[string]any, defaults map[string]string, durationSec, count int) (
	map[string]any, int, int) {
	if len(defaults) == 0 {
		return params, durationSec, count
	}
	out := make(map[string]any, len(params)+len(defaults))
	for k, v := range params {
		out[k] = v
	}
	for code, v := range defaults {
		key := nodeParamKey(code)
		if strings.TrimSpace(v) == "" {
			continue
		}
		if _, ok := out[key]; ok {
			continue
		}
		out[key] = v
	}
	if _, ok := defaults["duration"]; ok && durationSec <= 0 {
		if n, ok := positiveInt(out["duration"]); ok {
			durationSec = n
		}
	}
	if _, ok := defaults["count"]; ok && count <= 0 {
		if n, ok := positiveInt(out["count"]); ok {
			count = n
		}
	}
	return out, durationSec, count
}

// OverlayNodeParams 是在**已有节点**上生成时的受理参数：节点上当前的参数打底，这次给的覆盖（纯函数，输入不被修改）。
//
// ⚠️ 与前端「从节点生成」同一份语义（ParamPanel 发的是节点的全部 params，时长 / 张数也从 params 取）。
// 原先只发这次给的：Agent 在自己的节点上重跑（追加额度后带同一个 nodeId 再调一次）而没再写画幅时，
// 节点上明明是 9:16，发出去的却没有画幅 —— 有的上游直接 400，有的悄悄出了它自己的默认规格。
// 这次没给时长 / 张数时取节点上的（只认该类节点有的参数）：不取的话按默认 5 秒计价、上游却按节点上的 15 秒出片。
func OverlayNodeParams(node, given map[string]any, keys map[string]bool, durationSec, count int) (
	map[string]any, int, int) {
	if len(node) == 0 {
		return given, durationSec, count
	}
	out := make(map[string]any, len(node)+len(given))
	for k, v := range node {
		out[k] = v
	}
	for k, v := range given {
		out[k] = v
	}
	if keys["duration"] && durationSec <= 0 {
		if n, ok := positiveInt(out["duration"]); ok {
			durationSec = n
		}
	}
	if keys["count"] && count <= 0 {
		if n, ok := positiveInt(out["count"]); ok {
			count = n
		}
	}
	return out, durationSec, count
}

// ParamsByNode 从 canvasData 取出各节点当前的 params（节点 ID → params）；解不开返回 nil。
// 两个读画布端口（Agent 与 MCP）共用，不各解析一份。
func ParamsByNode(canvasData json.RawMessage) map[string]map[string]any {
	var doc struct {
		Nodes []struct {
			ID     string         `json:"id"`
			Params map[string]any `json:"params"`
		} `json:"nodes"`
	}
	if len(canvasData) == 0 || json.Unmarshal(canvasData, &doc) != nil {
		return nil
	}
	out := make(map[string]map[string]any, len(doc.Nodes))
	for _, n := range doc.Nodes {
		if n.ID != "" && n.Params != nil {
			out[n.ID] = n.Params
		}
	}
	return out
}

// EdgesOf 解析画布数据里的连线（两端与 order）。解析不了返回 nil；缺两端的跳过。
func EdgesOf(canvasData json.RawMessage) []CanvasEdge {
	var doc struct {
		Edges []struct {
			Source string  `json:"source"`
			Target string  `json:"target"`
			Order  float64 `json:"order"`
		} `json:"edges"`
	}
	if len(canvasData) == 0 || json.Unmarshal(canvasData, &doc) != nil {
		return nil
	}
	out := make([]CanvasEdge, 0, len(doc.Edges))
	for _, e := range doc.Edges {
		if e.Source != "" && e.Target != "" {
			out = append(out, CanvasEdge{Source: e.Source, Target: e.Target, Order: int(e.Order)})
		}
	}
	return out
}

// syncIntParam 让受理参数（整数）与节点参数（字符串）一致，返回最终的受理值。
func syncIntParam(params map[string]any, keys map[string]bool, key string, arg int) int {
	if !keys[key] {
		return arg
	}
	if arg > 0 {
		params[key] = strconv.Itoa(arg)
		return arg
	}
	n, ok := positiveInt(params[key])
	if !ok {
		return arg
	}
	params[key] = strconv.Itoa(n)
	return n
}

func positiveInt(v any) (int, bool) {
	var s string
	switch x := v.(type) {
	case json.Number:
		s = x.String()
	case string:
		s = strings.TrimSpace(x)
	case float64:
		s = strconv.FormatFloat(x, 'f', -1, 64)
	case int:
		s = strconv.Itoa(x)
	default:
		return 0, false
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		// "15.0" 这类写法也认；带小数的不猜。
		f, e := strconv.ParseFloat(s, 64)
		if e != nil || f != float64(int(f)) {
			return 0, false
		}
		n = int(f)
	}
	return n, n > 0
}

// BuildNodeParams 把模型 / 描述 / 视频模式写进节点参数，与前端节点同形（model / prompt；
// 音频的描述键是 text；视频的操作由 mode 决定 —— 同「直接生成」）。
//
// 自 agent/tools.ParseGenerateArgs 原样抽出（MCPT-08）：**就地修改并返回 params**（为 nil 时新建）。
// 之后还要过一次 NormalizeGenerateParams 才是最终写进节点的形状。
func BuildNodeParams(nodeType, modelCode, operationCode, prompt string, params map[string]any) map[string]any {
	if params == nil {
		params = map[string]any{}
	}
	params["model"] = modelCode
	if p := strings.TrimSpace(prompt); p != "" {
		key := "prompt"
		if nodeType == "audio" {
			key = "text"
		}
		params[key] = p
	}
	if nodeType == "video" {
		params["mode"] = operationCode
	}
	return params
}
