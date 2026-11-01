package domain

import (
	"encoding/json"
	"fmt"
	"strconv"
)

// ModelParamOverride 把 model_param_support 的一行折成前端合并用的第三层控件配置
// （`param_def.config < node_param.config_override < model_param_support.override`）。
//
// ⚠️ 起因（P15-01）：这张表建好之后一直没有端点读它，前端取不到第三层 —— 画布视频节点永远是
// 通用的「5/10 秒、720P/1080P、三种画幅」：Seedance 2.5 的 4~30 秒、480p、21:9 选不到，
// 只支持 720p 的 2.0 Fast 却能选 1080P（选了要到上游才报错，那时已经扣过费）。
//
// 三列各自的意思：
//   - override：控件配置原样（options / unit / restrictBy …），逐字段覆盖前两层；
//   - allowed_values：型号能收的值。override 没给 options 时据它生成（文案就是值本身）；
//   - default_value：缺省值。override 没给 default 时补上。
//
// ⚠️ default 必须是可选项之一，不是就丢掉：前端切模型时会把 default 原样写进参数，
// 等于替用户选了一个上游不收的值。丢掉之后前端取第一个可选项。
func ModelParamOverride(override, allowed []byte, defaultValue string) (map[string]any, error) {
	out := map[string]any{}
	if len(override) > 0 {
		if err := json.Unmarshal(override, &out); err != nil {
			return nil, fmt.Errorf("override 不是 JSON 对象: %w", err)
		}
		if out == nil { // 列里存的是 JSON null
			out = map[string]any{}
		}
	}
	if _, ok := out["options"]; !ok && len(allowed) > 0 {
		var values []any
		if err := json.Unmarshal(allowed, &values); err != nil {
			return nil, fmt.Errorf("allowed_values 不是 JSON 数组: %w", err)
		}
		if len(values) > 0 {
			options := make([]any, 0, len(values))
			for _, v := range values {
				if s := scalarString(v); s != "" {
					options = append(options, map[string]any{"label": s, "value": s})
				}
			}
			out["options"] = options
		}
	}
	if _, ok := out["default"]; !ok && defaultValue != "" {
		out["default"] = defaultValue
	}
	if d, ok := out["default"]; ok {
		if values, checkable := optionValueSet(out["options"]); checkable && !values[scalarString(d)] {
			delete(out, "default")
		}
	}
	return out, nil
}

// optionValueSet 取可选项的值集合；options 不是数组时 checkable=false（不检查 default）。
// 选项可能是 `{label,value}` 对象，也可能是裸字符串 / 数字（与前端 controlOptions 同一套形状）。
func optionValueSet(options any) (map[string]bool, bool) {
	list, ok := options.([]any)
	if !ok {
		return nil, false
	}
	set := make(map[string]bool, len(list))
	for _, o := range list {
		if m, ok := o.(map[string]any); ok {
			v, has := m["value"]
			if !has {
				v = m["code"]
			}
			set[scalarString(v)] = true
			continue
		}
		set[scalarString(o)] = true
	}
	return set, true
}

// scalarString 把 JSON 标量归一成字符串（"5" 与 5 是同一个值 —— 前端一律按字符串比较）。
func scalarString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(x)
	default:
		return ""
	}
}
