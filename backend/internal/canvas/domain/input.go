package domain

import (
	"fmt"
	"sort"
)

// 输入解析：把画布上的连线与选择，变成一份**可信、有序、类型兼容**的输入清单。
//
// ⚠️ 这是 GO-27 的落点：服务端只认**可信资源 ID**（generationId / referenceId /
// materialCode / voiceCode），**绝不把客户端给的任意 URL 或 objectKey 当作已授权资产**。
// 客户端能给的只有"指哪个"，不能给"内容在哪"。
//
// ⚠️ 顺序是语义的一部分：order 决定首尾帧的首/尾与参考图的锚定顺序，
// 在解析、排序、落快照的任何一步丢掉它，产出就会是另一个东西（GO-37）。

// InputKind 是输入的来源类型。
type InputKind string

const (
	InputText       InputKind = "text"       // 上游文本节点的文字
	InputGeneration InputKind = "generation" // 上游节点已完成的产出
	InputReference  InputKind = "reference"  // 用户上传的参考文件
	InputMaterial   InputKind = "material"   // 风格/特效：只贡献提示词，不算媒体
	InputVoice      InputKind = "voice"      // 音色：音频节点专用
)

// DeclaredInput 是客户端声明的一条输入。**只有 ID，没有 URL**。
type DeclaredInput struct {
	// Role 显式角色（目前只有 `mask`）。空表示按 Kind 推断。
	Role string
	Kind InputKind
	// Order 来自画布连线；同一目标内必须唯一。
	Order int
	// SourceNodeID 上游节点（generation/text 输入必填），用于报错时指名道姓。
	SourceNodeID string
	// 以下按 Kind 取其一。
	Text         string
	GenerationID int64
	ReferenceID  int64
	MaterialCode string
	MaterialType string // style / effect
	VoiceCode    string
	// RangeMs 是只用素材的哪一段：`[起点, 终点]`（毫秒，左闭右开），nil = 整段（docs/agent/smartcut SCD-34）。
	// 只对视频 / 音频有效：去字幕、按画面配音效只把片段用到的那一段发出去，并按这一段计价。
	RangeMs []int
}

// ResolvedInput 是解析后的可信输入，落进 generation.inputs_snapshot。
//
// ⚠️ 快照一旦写入**不再变更**：后台此后改模型/预设/素材/参数定义，
// 都不得改变这次任务的输入解释（06 §3.1）。
type ResolvedInput struct {
	// Width / Height 图片像素尺寸（0 = 未知），蒙版校验要用（NEW-13）。
	Width  int `json:"width,omitempty"`
	Height int `json:"height,omitempty"`
	// Role 是这条输入扮演的角色。默认按 Kind 推断；
	// `mask` 是显式角色 —— 同样是一张图，它说的是"改哪一块"而不是"画什么"。
	Role         string    `json:"role,omitempty"`
	Kind         InputKind `json:"kind"`
	Order        int       `json:"order"`
	SourceNodeID string    `json:"sourceNodeId,omitempty"`
	Text         string    `json:"text,omitempty"`
	// MediaID 是服务端解析出来的媒体对象，**不是客户端给的**。
	MediaID      int64  `json:"mediaId,omitempty"`
	MediaKind    string `json:"mediaKind,omitempty"` // image / audio / video
	GenerationID int64  `json:"generationId,omitempty"`
	ReferenceID  int64  `json:"referenceId,omitempty"`
	MaterialCode string `json:"materialCode,omitempty"`
	VoiceCode    string `json:"voiceCode,omitempty"`
	// RangeMs 是区间（见 DeclaredInput.RangeMs），校验过才进快照；Worker 发给上游前按它截出这一段。
	// ⚠️ omitempty：没有区间的输入快照与改动前逐字相同。
	RangeMs []int `json:"rangeMs,omitempty"`
	// Purpose 是参考文件的媒体用途（3D 导演台的截图 / 预演据此识别，DRD-18）。
	// ⚠️ **不进快照**（`json:"-"`）：所有带参考文件的生成都会经过这里，写进去的话每一条快照都多一个字段，
	// 与改动前逐字不等。它只在受理时用来汇总 DirectorKinds。
	Purpose string `json:"-"`
}

// InputIssue 是一条解析失败的原因。
//
// ⚠️ 必须**指名道姓**：说"输入不合法"用户不知道去改哪个节点。
type InputIssue struct {
	NodeID  string `json:"nodeId,omitempty"`
	Order   int    `json:"order,omitempty"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

const (
	IssueUpstreamNotReady = "upstream_not_ready"
	IssueTypeMismatch     = "type_mismatch"
	IssueCountMismatch    = "count_mismatch"
	IssueOrderConflict    = "order_conflict"
	IssueNotFound         = "not_found"
	IssueForbidden        = "forbidden"
	// IssueRangeInvalid：输入区间写错了（颠倒、越界、太短，或者给了图片 / 文本）。
	IssueRangeInvalid = "range_invalid"
)

// MinRangeMs 是区间的最短长度：更短的一段截出来不到一帧，上游拿到的是一个空文件（同时间线最短 100ms 的片段）。
const MinRangeMs = 100

// CheckRange 校验一条输入的区间（SCD-34）。kind 是解析出来的媒体大类，durationMs 是素材时长（0 = 未知，放行）。
//
// ⚠️ 时长未知时只校验形状、不校验终点：拒绝等于把探测不出时长的素材整个挡在门外（同时间线「时长未知时放行」）；
// 真越界的话截出来的是素材本身剩下的那一截，计价按区间算 —— 多收的方向，所以时长已知时一定要拦。
func CheckRange(d DeclaredInput, kind string, durationMs int) *InputIssue {
	if d.RangeMs == nil {
		return nil
	}
	issue := func(msg string) *InputIssue {
		return &InputIssue{NodeID: d.SourceNodeID, Order: d.Order, Code: IssueRangeInvalid, Message: msg}
	}
	if kind != "video" && kind != "audio" {
		return issue(fmt.Sprintf("第 %d 个输入：区间只对视频或音频有效", d.Order))
	}
	if len(d.RangeMs) != 2 {
		return issue(fmt.Sprintf("第 %d 个输入：区间要写成 [起点, 终点]（毫秒）", d.Order))
	}
	in, out := d.RangeMs[0], d.RangeMs[1]
	switch {
	case in < 0 || out <= in:
		return issue(fmt.Sprintf("第 %d 个输入：区间的起点要小于终点，且不能为负（%d~%d）", d.Order, in, out))
	case out-in < MinRangeMs:
		return issue(fmt.Sprintf("第 %d 个输入：区间至少 %d 毫秒", d.Order, MinRangeMs))
	case durationMs > 0 && out > durationMs:
		return issue(fmt.Sprintf("第 %d 个输入：区间终点 %d 毫秒超出了素材时长 %d 毫秒", d.Order, out, durationMs))
	}
	return nil
}

// RangeLengthMs 是一组解析后输入里最长的那段区间（毫秒）；没有区间时为 0。
//
// 按区间计价时取最长的那段：去字幕、配音效的产物与输入区间一样长。几段相加不对 —— 两段参考视频
// 出来的仍是一段视频。
func RangeLengthMs(inputs []ResolvedInput) int {
	longest := 0
	for _, in := range inputs {
		if len(in.RangeMs) == 2 && in.RangeMs[1]-in.RangeMs[0] > longest {
			longest = in.RangeMs[1] - in.RangeMs[0]
		}
	}
	return longest
}

// OrderInputs 按 order 排序并校验唯一性。
//
// ⚠️ order 重复必须**报错而不是自动重排**：重复时无从知道用户想要的顺序，
// 自动排一个出来就是替用户做了一个他看不见的决定 —— 首尾帧会反过来。
func OrderInputs(in []DeclaredInput) ([]DeclaredInput, []InputIssue) {
	seen := map[int]string{}
	var issues []InputIssue
	for _, d := range in {
		if d.Kind == InputMaterial || d.Kind == InputVoice {
			continue // 素材与音色不参与媒体排序
		}
		if prev, dup := seen[d.Order]; dup {
			issues = append(issues, InputIssue{
				NodeID: d.SourceNodeID, Order: d.Order, Code: IssueOrderConflict,
				Message: fmt.Sprintf("第 %d 个输入位被 %s 与 %s 同时占用，顺序无法确定",
					d.Order, prev, describe(d)),
			})
			continue
		}
		seen[d.Order] = describe(d)
	}
	out := make([]DeclaredInput, len(in))
	copy(out, in)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Order < out[j].Order })
	return out, issues
}

// describe 给一条输入起个人能看懂的名字。
//
// ⚠️ 上传的参考文件没有上游节点，直接用 SourceNodeID 会拼出
// "第 1 个输入位被  与  同时占用" 这种什么都没说的句子。
func describe(d DeclaredInput) string {
	if d.SourceNodeID != "" {
		return "节点 " + d.SourceNodeID
	}
	switch d.Kind {
	case InputReference:
		return fmt.Sprintf("参考文件 %d", d.ReferenceID)
	case InputGeneration:
		return fmt.Sprintf("产出 %d", d.GenerationID)
	}
	return string(d.Kind)
}

// MediaKindFor 把结果类型映射成媒体大类。
func MediaKindFor(resultType string) string {
	switch resultType {
	case "image", "audio", "video":
		return resultType
	}
	return ""
}

// operationSpec 描述一个 operation 对媒体输入的要求。
//
// ⚠️ 这些数字是**语义**不是限制：first_last_frame 的 2 张图是首帧和尾帧，
// 少一张就不是这个玩法了，多一张也无从决定用哪两张。
var operationSpec = map[string]struct {
	minMedia, maxMedia int      // -1 表示不限
	allowedKinds       []string // 空表示不限
	label              string
}{
	"text2img":   {0, 0, nil, "文生图"},
	"text2video": {0, 0, nil, "文生视频"},
	"text2audio": {0, 0, nil, "文本转语音"},
	"text2text":  {0, 0, nil, "文本生成"},
	// ⚠️ 图生图收**多张**参考图（CD-80）：分镜图要带上这一镜引用的全部资产图（角色 / 场景 / 道具，一镜常有五到八个）。
	// 写成 {1,1} 的话每一张分镜图都在受理时被拒（「最多接受 1 个媒体输入，当前 6 个」）。
	// 具体一个模型最多收几张由它的能力矩阵决定，操作这一层不设上限。
	"img2img":          {1, -1, []string{"image"}, "图生图"},
	"img2prompt":       {1, 1, []string{"image"}, "图片反推"},
	"img2video":        {1, 1, []string{"image"}, "图生视频"},
	"first_last_frame": {2, 2, []string{"image"}, "首尾帧"},
	"video2video":      {1, 1, []string{"video"}, "视频生视频"},
	"video2prompt":     {1, 1, []string{"video"}, "视频反推"},
	// ⚠️ 全能参考收参考视频（3D 导演台 DRK-12，DRD-21）：白模预演视频要能喂给 Seedance 2.5 这类收参考视频的模型。
	// 操作层只说「这个玩法能带视频」，某个模型收不收、收几段由它的能力矩阵判断 —— 矩阵是必配的，
	// 没声明 video 角色的模型在报价阶段就被拦下（42203），不扣费。
	"ref2video": {1, -1, []string{"image", "audio", "video"}, "全能参考"},
	"asset_gen": {0, -1, []string{"image"}, "资产图生成"},
	// ⚠️ 涂抹重绘吃**两张图**：原图 + 蒙版。写成 {1,1} 的话
	// 带蒙版的调用一律报"只接受 1 个媒体输入"，这个工具根本用不了。
	"inpaint":   {1, 2, []string{"image"}, "涂抹重绘"},
	"remove_bg": {1, 1, []string{"image"}, "抠图"},
}

// ValidateForOperation 校验解析后的输入是否满足该 operation。
//
// ⚠️ 这一步**在扣费与入队之前**（GAP-22）：等上游返回 400 才发现不支持，
// 积分已经扣了、任务已经排了队，退款和清理都是额外的补偿路径。
func ValidateForOperation(op string, inputs []ResolvedInput) []InputIssue {
	spec, known := operationSpec[op]
	if !known {
		// 未知 operation 不在这里拦：能力矩阵那一层会拒，
		// 这里瞎猜一个规则反而会把新加的玩法挡在门外。
		return nil
	}

	var media []ResolvedInput
	for _, in := range inputs {
		// 素材、音色、纯文本都不计入媒体数量 —— 素材只是修饰。
		if in.Kind == InputMaterial || in.Kind == InputVoice || in.Kind == InputText {
			continue
		}
		media = append(media, in)
	}

	var issues []InputIssue
	// 蒙版校验先做：它与"几张图"无关，而且报错要具体到尺寸（NEW-13）。
	issues = append(issues, validateMask(inputs)...)
	n := len(media)
	switch {
	case spec.maxMedia == 0 && n > 0:
		issues = append(issues, InputIssue{
			Code:    IssueCountMismatch,
			Message: fmt.Sprintf("%s 不接受媒体输入，当前连了 %d 个", spec.label, n),
		})
	case n < spec.minMedia:
		issues = append(issues, InputIssue{
			Code:    IssueCountMismatch,
			Message: fmt.Sprintf("%s 需要至少 %d 个媒体输入，当前 %d 个", spec.label, spec.minMedia, n),
		})
	case spec.maxMedia > 0 && n > spec.maxMedia:
		issues = append(issues, InputIssue{
			Code:    IssueCountMismatch,
			Message: fmt.Sprintf("%s 最多接受 %d 个媒体输入，当前 %d 个", spec.label, spec.maxMedia, n),
		})
	}

	if len(spec.allowedKinds) > 0 {
		allowed := map[string]bool{}
		for _, k := range spec.allowedKinds {
			allowed[k] = true
		}
		for _, m := range media {
			if m.MediaKind != "" && !allowed[m.MediaKind] {
				issues = append(issues, InputIssue{
					NodeID: m.SourceNodeID, Order: m.Order, Code: IssueTypeMismatch,
					Message: fmt.Sprintf("%s 不接受 %s 类型的输入（第 %d 个输入）",
						spec.label, m.MediaKind, m.Order),
				})
			}
		}
	}
	return issues
}

// OperationLabel 供错误信息使用；未知操作回原码。
func OperationLabel(op string) string {
	if s, ok := operationSpec[op]; ok {
		return s.label
	}
	return op
}

// RoleMask 是蒙版角色：同样是一张图，它说的是"改哪一块"而不是"画什么"。
const RoleMask = "mask"

// validateMask 校验蒙版与原图同尺寸。
//
// ⚠️ 这是**涂抹重绘唯一能对齐的依据**。尺寸对不上的话上游会把蒙版
// 拉伸后贴上去 —— 用户圈的是人脸，改掉的是半张背景，而任务是"成功"的。
//
// ⚠️ 原图取的是**第一张非蒙版的图**（order 最小的那张）：
// 蒙版永远是配角，画什么由主图决定。
//
// ⚠️ 任一侧尺寸未知时**放行**：拿不到数字就没法比较，拒绝等于把一批
// 本来能用的格式（比如 WebP，标准库没有解码器）挡在门外。
func validateMask(inputs []ResolvedInput) []InputIssue {
	var mask *ResolvedInput
	var source *ResolvedInput
	for i := range inputs {
		in := &inputs[i]
		if in.MediaKind != "image" {
			continue
		}
		if in.Role == RoleMask {
			if mask == nil {
				mask = in
			}
			continue
		}
		if source == nil {
			source = in
		}
	}
	if mask == nil {
		return nil
	}
	if source == nil {
		return []InputIssue{{
			Order: mask.Order, Code: IssueCountMismatch,
			Message: "只有蒙版没有原图：蒙版说的是「改哪一块」，得先有一张要改的图",
		}}
	}
	if source.Width <= 0 || source.Height <= 0 ||
		mask.Width <= 0 || mask.Height <= 0 {
		return nil
	}
	if source.Width == mask.Width && source.Height == mask.Height {
		return nil
	}
	return []InputIssue{{
		Order: mask.Order, Code: IssueTypeMismatch,
		Message: fmt.Sprintf(
			"蒙版尺寸与原图不一致：原图 %d×%d，蒙版 %d×%d。请重新在原图上涂抹",
			source.Width, source.Height, mask.Width, mask.Height),
	}}
}
