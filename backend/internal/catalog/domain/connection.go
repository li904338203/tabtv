package domain

import (
	"fmt"
	"sort"
)

// NodeKind 节点种类。
const (
	KindGenerator = "generator"
	KindMaterial  = "material"
	KindComposite = "composite"
)

// Input 是一条运行时输入（已按 edge.order 排好）。
type Input struct {
	// Order 从 1 起连续且唯一。它决定首尾帧与参考图锚定顺序，不可丢失。
	Order      int
	SourceType string // text | image | audio | video | style | effect
}

// MediaRoleOf 是「节点类型 → 媒体角色」的**唯一**映射（3D 导演台 DRT-05，DRD-20）。
//
// image / video / audio 原样返回；导演台（director）的产出是截图，按 image 算；其余（text、素材、脚本…）回空串。
// ⚠️ 视频模式校验按源**节点类型**查媒体，不经这一层的话「图生视频不接受 director 输入」；
// 整图的 roleOf、前端的视频模式判断都要与它同口径，不要各写一份（连线时能连、执行时不认）。
func MediaRoleOf(nodeType string) string {
	switch nodeType {
	case "image", "video", "audio":
		return nodeType
	case "director":
		return "image"
	case "smartcut":
		// 智能剪辑交给下游的是导出的成片（docs/agent/smartcut SCT-09）。
		return "video"
	}
	return ""
}

// Rule 是一条连线规则。
type Rule struct {
	SourceType       string
	TargetType       string
	IsAllowed        bool
	DefaultOperation string
}

// RuleSet 是连线规则的查表集合。
type RuleSet map[string]Rule

func NewRuleSet(rules []Rule) RuleSet {
	m := make(RuleSet, len(rules))
	for _, r := range rules {
		m[r.SourceType+"→"+r.TargetType] = r
	}
	return m
}

func (rs RuleSet) Lookup(src, dst string) (Rule, bool) {
	r, ok := rs[src+"→"+dst]
	return r, ok
}

// 视频模式对输入的约束（01 §3）。
// ⚠️ 这是**画布语义**层。某个具体型号真能收什么由 model_input_capability
// 另行判定（GAP-22）——两层覆盖范围本就不同，不能互相替代。
type videoModeSpec struct {
	// mediaMin/mediaMax 是媒体类输入（image/audio/video）的数量区间。
	mediaMin, mediaMax int
	allowedMedia       map[string]bool
	desc               string
}

var videoModes = map[string]videoModeSpec{
	"text2video": {0, 0, map[string]bool{}, "无外部输入或全部为 text"},
	"img2video":  {1, 1, map[string]bool{"image": true}, "恰好 1 张图片"},
	// 首尾帧：顺序即语义，第 1 张是首帧、第 2 张是尾帧。
	"first_last_frame": {2, 2, map[string]bool{"image": true}, "恰好 2 张图片（顺序为首帧、尾帧）"},
	// 全能参考收参考视频（DRK-12）：画布语义层放开，具体型号由能力矩阵把关。
	"ref2video":   {1, 8, map[string]bool{"image": true, "audio": true, "video": true}, "至少 1 个 image/audio/video 参考"},
	"video2video": {1, 1, map[string]bool{"video": true}, "恰好 1 个视频"},
}

// ValidateInputOrder 校验 order 从 1 起连续且唯一。
//
// ⚠️ order 不只是显示顺序：首尾帧靠它区分首/尾，参考图靠它与提示词里的
// 编号对应。断号或重号会让生成结果与用户预期错位，而且**不会报错**。
func ValidateInputOrder(inputs []Input) error {
	if len(inputs) == 0 {
		return nil
	}
	orders := make([]int, len(inputs))
	for i, in := range inputs {
		orders[i] = in.Order
	}
	sort.Ints(orders)
	for i, o := range orders {
		if o != i+1 {
			return fmt.Errorf("输入 order 必须从 1 起连续且唯一，实际为 %v", orders)
		}
	}
	return nil
}

// ValidateConnections 校验一组输入能否连到目标节点。
func (rs RuleSet) ValidateConnections(targetType string, inputs []Input) error {
	if err := ValidateInputOrder(inputs); err != nil {
		return err
	}
	for _, in := range inputs {
		r, ok := rs.Lookup(in.SourceType, targetType)
		if !ok || !r.IsAllowed {
			return fmt.Errorf("不允许从 %s 连到 %s", in.SourceType, targetType)
		}
	}
	return nil
}

// ValidateVideoMode 按视频模式校验输入。
//
// style/effect 是修饰输入，**不计入媒体数量**（01 §3），
// 但连线本身仍必须被规则允许。
func ValidateVideoMode(mode string, inputs []Input) error {
	spec, ok := videoModes[mode]
	if !ok {
		return fmt.Errorf("未知视频模式: %q", mode)
	}
	if err := ValidateInputOrder(inputs); err != nil {
		return err
	}

	media := 0
	for _, in := range inputs {
		switch in.SourceType {
		case "style", "effect":
			continue // 修饰输入不计数
		case "text":
			if mode != "text2video" && mode != "ref2video" {
				return fmt.Errorf("%s 模式不接受 text 输入", mode)
			}
			continue
		}
		// 先归一成媒体角色（导演台截图算 image）再判，错误文案仍写原节点类型。
		role := MediaRoleOf(in.SourceType)
		if role == "" {
			role = in.SourceType
		}
		if !spec.allowedMedia[role] {
			return fmt.Errorf("%s 模式不接受 %s 输入（%s）", mode, in.SourceType, spec.desc)
		}
		media++
	}
	if media < spec.mediaMin || media > spec.mediaMax {
		return fmt.Errorf("%s 模式需要 %s，实际 %d 个媒体输入", mode, spec.desc, media)
	}
	return nil
}

// ResolveOperation 判定本次生成的 operation。
//
// 优先级（03 §2.6）：
//  1. 视频节点以选中的 **mode 为准**；
//  2. 其他节点用 connection_rule.default_operation（**仅默认建议**）。
//
// ⚠️ 不能靠 operation 名称前缀猜输出类型（06 §3.1），
// 也不能在多义时让 default_operation 覆盖 mode。
func (rs RuleSet) ResolveOperation(targetType, mode string, inputs []Input) (string, error) {
	if targetType == "video" {
		if mode == "" {
			return "", fmt.Errorf("视频节点必须指定 mode")
		}
		if _, ok := videoModes[mode]; !ok {
			return "", fmt.Errorf("未知视频模式: %q", mode)
		}
		return mode, nil
	}
	// 非视频节点：取第一条有 default_operation 的规则。
	for _, in := range inputs {
		if r, ok := rs.Lookup(in.SourceType, targetType); ok && r.DefaultOperation != "" {
			return r.DefaultOperation, nil
		}
	}
	return "", fmt.Errorf("无法判定 operation：目标 %s 的输入未匹配到默认操作", targetType)
}

// ConnectionVerdict 是连线预校验的结论（POST /canvas/connections/validate）。
type ConnectionVerdict struct {
	Valid     bool
	Operation string
	Reason    string
}

// CheckConnection 判断一组输入能否连到目标节点，并给出默认操作建议。
//
// ⚠️ 非视频目标判定不出默认操作，只是「没有建议操作」，**不是**「连不上」：
//   - 导演台（composite）本来就没有生成操作，`image → director` 是合法连线（3D 导演台 DRT-05）；
//   - 风格节点单独连图片节点时，图片节点的操作由它自己的描述决定（文生图），规则同样允许。
//
// 早先这两种都回 `valid:false`，只是因为前端从不看 valid（DRB-1）才没人发现；DRT-16 修了前端之后，
// 它们会被当成非法连线拦掉。视频目标不同：模式是必选的，判定不出就是真的不合法。
func (rs RuleSet) CheckConnection(targetType, mode string, inputs []Input) ConnectionVerdict {
	if err := rs.ValidateConnections(targetType, inputs); err != nil {
		return ConnectionVerdict{Reason: err.Error()}
	}
	if targetType == "video" {
		if err := ValidateVideoMode(mode, inputs); err != nil {
			return ConnectionVerdict{Reason: err.Error()}
		}
	}
	op, err := rs.ResolveOperation(targetType, mode, inputs)
	if err != nil {
		if targetType == "video" {
			return ConnectionVerdict{Reason: err.Error()}
		}
		return ConnectionVerdict{Valid: true}
	}
	return ConnectionVerdict{Valid: true, Operation: op}
}

// VideoModes 返回全部已知视频模式，供管理端与契约使用。
func VideoModes() []string {
	out := make([]string, 0, len(videoModes))
	for m := range videoModes {
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}
