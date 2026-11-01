package domain

import (
	"encoding/json"
	"fmt"
)

// 转场（NEW-09）与片段属性（NEW-08）。
//
// ⚠️ **接缝的转场只记在后一段的 `transition_in` 上**。
// 两段各记一半（A 的 transitionOut + B 的 transitionIn）看起来更对称，
// 但用户改了 A 的转出而 B 的转入还是旧的时，渲染该听谁的没人说得清 ——
// 而这种不一致在时间线界面上完全看不出来。
//
// 于是两个字段的语义是：
//   - `transition_in`  在第一段上＝**从黑场淡入**；在其余段上＝**与上一段的衔接**
//   - `transition_out` **只在最后一段**有意义＝**淡出到黑场**
//
// 中间段的 `transition_out` 一律忽略（不是报错：用户从别处复制过来的片段
// 可能带着它，为此拒掉一次保存太重了）。

// 转场类型。故意只有四种 —— 每多一种就要在渲染侧多一条 filter 链，
// 而"基础转场"（NEW-09 的原文）指的就是这几种。
const (
	TransitionNone      = "none"
	TransitionFade      = "fade"       // 淡入淡出（叠化）
	TransitionDissolve  = "dissolve"   // 溶解
	TransitionSlideLeft = "slide_left" // 左滑
	TransitionWipe      = "wipe"       // 划像
)

// MinTransitionMs / MaxTransitionMs 转场时长的上下限。
//
// ⚠️ 下限不是洁癖：比两帧还短的转场在 30fps 下根本看不出来，
// 而它仍然会让渲染多走一条 filter 链、多花一份时间。
const (
	MinTransitionMs = 100
	MaxTransitionMs = 3000
)

// Transition 是一次转场。
type Transition struct {
	Kind       string `json:"kind"`
	DurationMs int    `json:"durationMs"`
}

// IsActive 有没有真的设转场。
func (t Transition) IsActive() bool {
	return t.Kind != "" && t.Kind != TransitionNone && t.DurationMs > 0
}

var transitionKinds = map[string]bool{
	TransitionNone: true, TransitionFade: true, TransitionDissolve: true,
	TransitionSlideLeft: true, TransitionWipe: true,
}

// ValidateTransition 校验一次转场。
//
// `prevLen` 是上一段的时长（第一段传 0），`selfLen` 是这一段的时长。
//
// ⚠️ **转场会"吃掉"两段各一半的时间**：500ms 的叠化意味着两段重叠 500ms。
// 时长超过任一侧的一半时，那一侧会被整段吃光 —— 渲染出来直接少一个镜头，
// 而时间线上那一段还好端端地画着。
func ValidateTransition(t Transition, prevLen, selfLen int) error {
	if !t.IsActive() {
		return nil
	}
	if !transitionKinds[t.Kind] {
		return fmt.Errorf("不支持的转场类型 %s", t.Kind)
	}
	if t.DurationMs < MinTransitionMs || t.DurationMs > MaxTransitionMs {
		return fmt.Errorf("转场时长只支持 %d~%d 毫秒，当前 %d",
			MinTransitionMs, MaxTransitionMs, t.DurationMs)
	}
	if t.DurationMs*2 > selfLen {
		return fmt.Errorf("转场 %d 毫秒超过本段 %d 毫秒的一半，会把这一段吃光",
			t.DurationMs, selfLen)
	}
	// prevLen == 0 表示这是第一段（从黑场淡入），没有上一段可吃。
	if prevLen > 0 && t.DurationMs*2 > prevLen {
		return fmt.Errorf("转场 %d 毫秒超过上一段 %d 毫秒的一半，会把上一段吃光",
			t.DurationMs, prevLen)
	}
	return nil
}

// EffectiveDuration 算上转场之后整条轨道有多长。
//
// ⚠️ 每个**生效的**转场都让总时长少一个转场时长（两段重叠那么久）。
// 不算这一笔的话，界面上显示的总时长与渲染出来的成片对不上 ——
// 而用户是按界面上那个数字去配乐、配字幕的。
//
// ⚠️ 第一段的转入是"从黑场淡入"，**不重叠**，所以不减。
// 最后一段的转出同理。
//
// 与 Sequence 的落点、渲染的 offset 同一个实现（layout.go 的 placeVideo）。
func EffectiveDuration(clips []Clip) int {
	_, end := VideoPlacement(clips)
	return end
}

// ParseTransition 从 jsonb 读一次转场。空值与坏值都当"没有转场"。
//
// ⚠️ 坏值不报错：这一列是 jsonb，历史数据与手工改库都可能留下奇怪的东西，
// 而"有一段的转场字段坏了"不该让整条时间线打不开。
func ParseTransition(raw json.RawMessage) Transition {
	if len(raw) == 0 {
		return Transition{}
	}
	var t Transition
	if err := json.Unmarshal(raw, &t); err != nil {
		return Transition{}
	}
	if !transitionKinds[t.Kind] {
		return Transition{}
	}
	return t
}

// ---------------------------------------------------------------- 片段属性

// ClipProps 是一次片段属性修改。指针为 nil 表示"这一项不动"。
//
// ⚠️ 用指针而不是零值：`volume: 0` 是**静音**，是一个正当的取值。
// 用零值当"没传"的话，用户永远调不到静音 —— 他点了没反应，
// 而界面上那个滑块确实滑到了 0（03 §5「不要让 Go 零值悄悄覆盖语义」）。
type ClipProps struct {
	Speed        *int
	Volume       *int
	FadeInMs     *int
	FadeOutMs    *int
	TransitionIn *Transition
}

// ApplyProps 把修改应用到一段上，只动传了的那些项。
func ApplyProps(c Clip, p ClipProps) Clip {
	next := c
	if p.Speed != nil {
		next.Speed = *p.Speed
	}
	if p.Volume != nil {
		next.Volume = *p.Volume
	}
	if p.FadeInMs != nil {
		next.FadeInMs = *p.FadeInMs
	}
	if p.FadeOutMs != nil {
		next.FadeOutMs = *p.FadeOutMs
	}
	if p.TransitionIn != nil {
		next.TransitionIn = *p.TransitionIn
	}
	next.DurationMs = next.PlayLen()
	return next
}
