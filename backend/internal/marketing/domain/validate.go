package domain

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Level 是问题的级别：error 拦截保存，warn 只提示。
type Level string

const (
	LevelError Level = "error"
	LevelWarn  Level = "warn"
)

// IssueCode 是问题的机器可读分类。前端按它（而不是文案）决定怎么呈现 ——
// 文案改一个字匹配就失效。
type IssueCode string

const (
	IssueRequired     IssueCode = "required"
	IssueTooLong      IssueCode = "too_long"
	IssueBadFormat    IssueCode = "bad_format"
	IssueBadValue     IssueCode = "bad_value"
	IssueBadWindow    IssueCode = "bad_window"
	IssueKindOnly     IssueCode = "kind_only"
	IssueCoverCount   IssueCode = "cover_count"
	IssueDuplicate    IssueCode = "duplicate"
	IssueTooMany      IssueCode = "too_many"
	IssueBannedWord   IssueCode = "banned_word"
	IssueMissingNote  IssueCode = "missing_disclaimer"
	IssueCautionWord  IssueCode = "caution_word"
	IssueUnknownField IssueCode = "unknown_field"
)

// Issue 是一条校验问题。Field 用请求体里的字段路径（camelCase，如 `inputSlots[0].key`），
// 管理端据此把问题挂到对应的表单项上。
type Issue struct {
	// Slot 只在槽位填充校验里有值（ValidateSlotFill），对应接口错误的 `details.slot`。
	Slot    string    `json:"slot,omitempty"`
	Field   string    `json:"field"`
	Code    IssueCode `json:"code"`
	Message string    `json:"message"`
	Level   Level     `json:"level"`
}

// HasError 判断问题里有没有拦截级的。
func HasError(issues []Issue) bool {
	for _, i := range issues {
		if i.Level == LevelError {
			return true
		}
	}
	return false
}

func errIssue(field string, code IssueCode, msg string) Issue {
	return Issue{Field: field, Code: code, Message: msg, Level: LevelError}
}

func warnIssue(field string, code IssueCode, msg string) Issue {
	return Issue{Field: field, Code: code, Message: msg, Level: LevelWarn}
}

func tooLong(field, label string, got, max int) Issue {
	return errIssue(field, IssueTooLong, fmt.Sprintf("%s最多 %d 字，当前 %d 字", label, max, got))
}

// ValidateOption 校验一条选项能不能保存（MKT-F-73）。
//
// ⚠️ **一次报完**，不是遇到第一个就返回：运营改一个存一次、再报下一个，
// 十个问题要来回十趟。
//
// 不在这里做的：口播文案（CheckTalkCopy，与技能保存共用一个实现）、
// 推荐模型 / 默认技能的存在性（要查目录，属 app 层）、封面媒体的来源（要查媒体域）。
func ValidateOption(o Option) []Issue {
	var out []Issue

	if !o.Kind.Valid() {
		out = append(out, errIssue("kind", IssueBadValue, "类型只能是 creative / hook / style"))
	}
	if !ValidCode(o.Code) {
		out = append(out, errIssue("code", IssueBadFormat, "code 须为小写字母开头的小写字母、数字、下划线，共 3~64 位"))
	}

	if RuneLen(trim(o.Name)) == 0 {
		out = append(out, errIssue("name", IssueRequired, "名称不能为空"))
	} else if n := RuneLen(o.Name); n > MaxOptionName {
		out = append(out, tooLong("name", "名称", n, MaxOptionName))
	}
	if trim(o.CategoryCode) == "" {
		out = append(out, errIssue("categoryCode", IssueRequired, "请选择分类"))
	}
	if n := RuneLen(o.Description); n > MaxOptionDescription {
		out = append(out, tooLong("description", "说明", n, MaxOptionDescription))
	}
	if n := RuneLen(o.Prompt); n > MaxOptionPrompt {
		out = append(out, tooLong("prompt", "提示词片段", n, MaxOptionPrompt))
	} else if o.IsEnabled && trim(o.Prompt) == "" {
		// 停用的草稿可以先空着；启用一个空片段等于「选了等于没选」（GAP-01 同构）。
		out = append(out, errIssue("prompt", IssueRequired, "启用前须填写提示词片段"))
	}

	out = append(out, validateWindow(o.StartAt, o.EndAt)...)
	out = append(out, validateOutputTypes(o.OutputTypes)...)
	out = append(out, validateRequirements(o.InputRequirements)...)
	out = append(out, validateKindOnly(o)...)
	out = append(out, validateCovers(o)...)
	return out
}

// validateWindow：投放时段 `[start, end)`，倒置与零长度都拒（`<` 而非 `<=`）。
// ⚠️ 存进去的话它永远投不出去，而管理端列表里看到的是一条「已启用」的创意。
func validateWindow(start, end *time.Time) []Issue {
	if start == nil || end == nil || start.Before(*end) {
		return nil
	}
	msg := "结束时间必须晚于开始时间（当前开始晚于结束，这个时段永远投不出去）"
	if start.Equal(*end) {
		msg = "结束时间必须晚于开始时间（当前两者相同，时段长度为零，永远投不出去）"
	}
	return []Issue{errIssue("endAt", IssueBadWindow, msg)}
}

func validateOutputTypes(types []string) []Issue {
	if len(types) == 0 {
		return []Issue{errIssue("outputTypes", IssueRequired, "至少选一种适用产出（video / image）")}
	}
	var out []Issue
	seen := map[string]bool{}
	for i, t := range types {
		field := fmt.Sprintf("outputTypes[%d]", i)
		switch {
		case t != OutputVideo && t != OutputImage:
			out = append(out, errIssue(field, IssueBadValue, fmt.Sprintf("适用产出只能是 video / image，不认识「%s」", t)))
		case seen[t]:
			out = append(out, errIssue(field, IssueDuplicate, fmt.Sprintf("适用产出「%s」重复", t)))
		}
		seen[t] = true
	}
	return out
}

func validateRequirements(req map[string]string) []Issue {
	var out []Issue
	for _, k := range sortedKeys(req) {
		field := "inputRequirements." + k
		if !ValidSlotKey(k) {
			out = append(out, errIssue(field, IssueBadValue, fmt.Sprintf("输入要求只能针对 product / character / source_video，不认识「%s」", k)))
			continue
		}
		if v := req[k]; v != RequirementRequired && v != RequirementOptional {
			out = append(out, errIssue(field, IssueBadValue, fmt.Sprintf("输入要求只能是 required / optional，不认识「%s」", v)))
		}
	}
	return out
}

// validateKindOnly：推荐模型 / 推荐参数 / 默认技能只给创意（MKT-D-31~33），
// 依赖出声只给 Hook 与创意（MKT-D-30）。
//
// ⚠️ 填在别的类型上不报错的话，它们就成了「存了没人读」的字段 —— 运营以为给某个
// Hook 配了推荐模型，而前台从来不看。
func validateKindOnly(o Option) []Issue {
	var out []Issue
	if o.Kind != KindCreative {
		if len(o.RecommendedModels) > 0 {
			out = append(out, errIssue("recommendedModels", IssueKindOnly, "推荐模型只能配在创意上"))
		}
		if !emptyJSONObject(o.RecommendedParams) {
			out = append(out, errIssue("recommendedParams", IssueKindOnly, "推荐参数只能配在创意上"))
		}
		if trim(o.DefaultSkillCode) != "" {
			out = append(out, errIssue("defaultSkillCode", IssueKindOnly, "默认技能只能配在创意上"))
		}
	}
	if o.Kind == KindStyle && o.IsAudioRequired {
		out = append(out, errIssue("isAudioRequired", IssueKindOnly, "「依赖出声」只能配在创意或 Hook 上"))
	}
	if o.Kind == KindCreative && !emptyJSONObject(o.RecommendedParams) {
		if _, err := ParseRecommendedParams(o.RecommendedParams); err != nil {
			out = append(out, errIssue("recommendedParams", IssueBadFormat, err.Error()))
		}
	}
	return out
}

// validateCovers：创意 1~3 张、Hook / 风格 1 张（MKT-D-26）。
// 下限**只在启用时强制**：草稿可以先存文字、封面后补；上限任何时候都拦。
func validateCovers(o Option) []Issue {
	max := MaxOtherCovers
	if o.Kind == KindCreative {
		max = MaxCreativeCovers
	}
	n := len(o.CoverMediaIDs)
	switch {
	case n > max:
		return []Issue{errIssue("coverMediaIds", IssueCoverCount, fmt.Sprintf("%s封面最多 %d 张，当前 %d 张", o.Kind.Label(), max, n))}
	case o.IsEnabled && n == 0:
		return []Issue{errIssue("coverMediaIds", IssueCoverCount, fmt.Sprintf("启用前须上传封面（%s 1~%d 张）", o.Kind.Label(), max))}
	}
	return nil
}

// RecommendedParams 是创意的推荐参数（MKT-D-32）：画幅与时长（秒，可为区间）。
type RecommendedParams struct {
	AspectRatio string `json:"aspectRatio,omitempty"`
	DurationSec []int  `json:"durationSec,omitempty"`
}

// ParseRecommendedParams 解析推荐参数。时长是 `[n]` 或 `[min, max]`，都要为正且不倒置。
func ParseRecommendedParams(raw json.RawMessage) (RecommendedParams, error) {
	var p RecommendedParams
	if emptyJSONObject(raw) {
		return p, nil
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return p, fmt.Errorf("推荐参数不是合法的对象：%v", err)
	}
	switch len(p.DurationSec) {
	case 0:
	case 1:
		if p.DurationSec[0] <= 0 {
			return p, fmt.Errorf("推荐时长必须为正数")
		}
	case 2:
		if p.DurationSec[0] <= 0 || p.DurationSec[1] < p.DurationSec[0] {
			return p, fmt.Errorf("推荐时长区间必须为正且下限不大于上限")
		}
	default:
		return p, fmt.Errorf("推荐时长只能是一个数或 [下限, 上限]")
	}
	return p, nil
}

func emptyJSONObject(raw json.RawMessage) bool {
	s := bytes.TrimSpace(raw)
	return len(s) == 0 || bytes.Equal(s, []byte("null")) || bytes.Equal(s, []byte("{}"))
}

func trim(s string) string { return strings.TrimSpace(s) }

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
