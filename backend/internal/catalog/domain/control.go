// Package domain 定义画布配置的控件、参数合并与连线规则。
//
// 配置驱动画布：前端遍历配置按 control 查表渲染，**不写死表单**；
// 遇未知 control/code → 忽略 + 告警，不崩溃（配置可以比前端新）。
//
// 本包的职责是保证「后端下发的配置前端一定能渲染」——
// 控件配置在**写入时**就要校验，而不是等前端拿到再崩。
package domain

import (
	"encoding/json"
	"fmt"
	"strings"
)

type ControlType string

const (
	ControlSelect    ControlType = "select"
	ControlSegmented ControlType = "segmented"
	ControlSlider    ControlType = "slider"
	ControlGrid      ControlType = "grid"
	ControlTagGroup  ControlType = "tag_group"
	ControlSwitch    ControlType = "switch"
	ControlDialog    ControlType = "dialog"
	ControlButton    ControlType = "button"
	ControlUpload    ControlType = "upload"
	ControlInline    ControlType = "inline"
	ControlText      ControlType = "text"
)

var allControls = map[ControlType]bool{
	ControlSelect: true, ControlSegmented: true, ControlSlider: true,
	ControlGrid: true, ControlTagGroup: true, ControlSwitch: true,
	ControlDialog: true, ControlButton: true, ControlUpload: true,
	ControlInline: true, ControlText: true,
}

// Option 是选项类控件的一项。
type Option struct {
	Value string `json:"value"`
	Label string `json:"label"`
	Icon  string `json:"icon,omitempty"`
	// DomainFrom 表示选项走懒加载（模型/风格/音色这类大列表不内联下发）。
	DomainFrom string `json:"domainFrom,omitempty"`
}

// ControlConfig 是控件配置的统一形状。
// 不同 control 用到的字段不同，由 ValidateControlConfig 按类型校验。
type ControlConfig struct {
	Options    []Option `json:"options,omitempty"`
	DomainFrom string   `json:"domainFrom,omitempty"`
	Min        *float64 `json:"min,omitempty"`
	Max        *float64 `json:"max,omitempty"`
	Step       *float64 `json:"step,omitempty"`
	Unit       string   `json:"unit,omitempty"`
	Columns    *int     `json:"columns,omitempty"`
	// MaxCount / MaxSize 是上传类控件的上限。
	// ⚠️ 这两个值是**配置不是代码**：把张数写死在前端，
	// 调整上限就得发版，等于把配置驱动作废。
	MaxCount     *int     `json:"maxCount,omitempty"`
	MaxSizeBytes *int64   `json:"maxSize,omitempty"`
	AcceptMIME   []string `json:"accept,omitempty"`
	Placeholder  string   `json:"placeholder,omitempty"`
	MaxLength    *int     `json:"maxLength,omitempty"`
	Multiple     bool     `json:"multiple,omitempty"`
}

// ValidateControlType 校验控件类型是否已知。
func ValidateControlType(t string) error {
	if !allControls[ControlType(t)] {
		return fmt.Errorf("未知控件类型: %q", t)
	}
	return nil
}

// ValidateControlConfig 按控件类型校验配置形状。
//
// ⚠️ 这是 P03-01 出口「动态参数 schema 与后端校验一致」的实现：
// 配置在**写入时**就要保证前端能渲染，否则运营保存成功、用户端却白屏，
// 而错误要等用户反馈才发现。
func ValidateControlConfig(ct string, raw []byte) error {
	if err := ValidateControlType(ct); err != nil {
		return err
	}
	var c ControlConfig
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &c); err != nil {
			return fmt.Errorf("控件配置不是合法 JSON: %w", err)
		}
	}

	switch ControlType(ct) {
	case ControlSelect, ControlSegmented, ControlGrid, ControlTagGroup:
		// 选项类：要么内联 options，要么声明 domainFrom 懒加载，二选一必须有。
		// 都没有的话前端会渲染出一个空下拉，用户以为功能坏了。
		if len(c.Options) == 0 && c.DomainFrom == "" {
			return fmt.Errorf("%s 控件必须提供 options 或 domainFrom", ct)
		}
		if err := validateOptions(c.Options); err != nil {
			return err
		}
		if ControlType(ct) == ControlGrid && c.Columns != nil && *c.Columns < 1 {
			return fmt.Errorf("grid 的 columns 必须 ≥ 1")
		}

	case ControlSlider:
		if c.Min == nil || c.Max == nil {
			return fmt.Errorf("slider 必须提供 min 与 max")
		}
		if *c.Min >= *c.Max {
			return fmt.Errorf("slider 的 min(%v) 必须小于 max(%v)", *c.Min, *c.Max)
		}
		if c.Step != nil {
			if *c.Step <= 0 {
				return fmt.Errorf("slider 的 step 必须大于 0")
			}
			if *c.Step > *c.Max-*c.Min {
				return fmt.Errorf("slider 的 step 大于取值区间，滑块将无法移动")
			}
		}

	case ControlUpload:
		if c.MaxCount != nil && *c.MaxCount < 1 {
			return fmt.Errorf("upload 的 maxCount 必须 ≥ 1")
		}
		if c.MaxSizeBytes != nil && *c.MaxSizeBytes <= 0 {
			return fmt.Errorf("upload 的 maxSize 必须大于 0")
		}
		if len(c.AcceptMIME) == 0 {
			// 不限类型等于允许上传任意文件，服务端虽会再校验文件头，
			// 但前端不给约束会让用户传完才被拒。
			return fmt.Errorf("upload 必须声明 accept（允许的 MIME）")
		}

	case ControlText:
		if c.MaxLength != nil && *c.MaxLength < 1 {
			return fmt.Errorf("text 的 maxLength 必须 ≥ 1")
		}

	case ControlSwitch, ControlDialog, ControlButton, ControlInline:
		// 这几类无额外必填配置。
	}
	return nil
}

func validateOptions(opts []Option) error {
	seen := make(map[string]bool, len(opts))
	for i, o := range opts {
		if strings.TrimSpace(o.Value) == "" {
			return fmt.Errorf("第 %d 个选项缺少 value", i+1)
		}
		if strings.TrimSpace(o.Label) == "" {
			return fmt.Errorf("选项 %q 缺少 label", o.Value)
		}
		if seen[o.Value] {
			// 重复 value 会让前端选中态错乱：选 A 高亮 B。
			return fmt.Errorf("选项 value 重复: %q", o.Value)
		}
		seen[o.Value] = true
	}
	return nil
}

// MergeConfig 按优先级合并控件配置：
//
//	param_def.config < node_param.config_override < model_param_support.override
//
// 后者**逐字段覆盖**前者，不是整体替换 —— 整体替换会让节点覆盖一个
// maxCount 时把 accept 也一并清空。
func MergeConfig(layers ...[]byte) ([]byte, error) {
	merged := map[string]any{}
	for _, raw := range layers {
		if len(raw) == 0 {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, fmt.Errorf("配置层不是合法 JSON 对象: %w", err)
		}
		for k, v := range m {
			merged[k] = v
		}
	}
	return json.Marshal(merged)
}
