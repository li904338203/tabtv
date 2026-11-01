package domain

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
)

// 视频规格三问：画幅 / 清晰度 / 时长（会话 107419662995685376，2026-10-24 用户要求）。
//
// 用户在输入框的偏好里把这几项选成「智能」（偏好里没有这个字段），规划视频之前 Agent 必须用 ask_user 跟他确认，
// 不能自己定：那次 Agent 自己定了「9:16 · 15 秒 · 720p」写进规划文档，而用户一个都没选过。
//
// ⚠️ 「问过没有」按**固定的题目 id** 认，不匹配题目文字：文字由模型写，改一个字匹配就失效。
const (
	SpecRatio      = "aspect_ratio"
	SpecResolution = "resolution"
	SpecDuration   = "duration"
)

// SpecKeys 是规格三问的固定顺序（提示与报错都按它列）。
var SpecKeys = []string{SpecRatio, SpecResolution, SpecDuration}

// SpecLabel 题目 id 的中文名。
func SpecLabel(key string) string {
	switch key {
	case SpecRatio:
		return "画幅"
	case SpecResolution:
		return "清晰度"
	case SpecDuration:
		return "时长"
	}
	return key
}

// AnsweredQuestions 是一张已提交的问答卡（卡片内容 + 用户的提交）。
type AnsweredQuestions struct {
	Payload    json.RawMessage
	Submission json.RawMessage
}

// SpecAnswers 取问答卡里规格三问的回答：题目 id → 用户回答的原文（所选选项的 label，或自己填的文字）。
//
// cards 按提交时间**从新到旧**；同一题以最新一次回答为准。解不开的卡跳过（不因为一张坏卡把其它回答也丢掉）。
func SpecAnswers(cards []AnsweredQuestions) map[string]string {
	out := map[string]string{}
	for _, c := range cards {
		var p QuestionsPayload
		var s QuestionsSubmission
		if json.Unmarshal(c.Payload, &p) != nil || json.Unmarshal(c.Submission, &s) != nil {
			continue
		}
		byID := make(map[string]Question, len(p.Questions))
		for _, q := range p.Questions {
			byID[q.ID] = q
		}
		for _, a := range s.Answers {
			if !isSpecKey(a.QuestionID) {
				continue
			}
			if _, done := out[a.QuestionID]; done {
				continue
			}
			if v := answerText(byID[a.QuestionID], a); v != "" {
				out[a.QuestionID] = v
			}
		}
	}
	return out
}

func isSpecKey(id string) bool {
	for _, k := range SpecKeys {
		if k == id {
			return true
		}
	}
	return false
}

// answerText 是一题回答的原文：所选选项的 label、「我有别的想法」的文字、文本题的文字。
func answerText(q Question, a Answer) string {
	var parts []string
	for _, id := range a.Selected {
		for _, o := range q.Options {
			if o.ID == id {
				if l := strings.TrimSpace(o.Label); l != "" {
					parts = append(parts, l)
				}
			}
		}
	}
	if a.Other != nil {
		if v := strings.TrimSpace(*a.Other); v != "" {
			parts = append(parts, v)
		}
	}
	if v := strings.TrimSpace(a.Text); v != "" {
		parts = append(parts, v)
	}
	return strings.Join(parts, "；")
}

var (
	ratioPattern      = regexp.MustCompile(`(\d{1,2})\s*[:：]\s*(\d{1,2})`)
	resolutionPattern = regexp.MustCompile(`(480|540|720|1080|1440|2160)\s*p|([248])\s*k`)
	secondsPattern    = regexp.MustCompile(`\d{1,3}`)
)

// NormalizeRatio 从回答里取画幅：「9:16 竖屏（推荐）」→ 9:16；只写了「竖屏 / 横屏 / 方形」按常用画幅认。取不到返回 ""。
func NormalizeRatio(s string) string {
	if m := ratioPattern.FindStringSubmatch(s); m != nil {
		return m[1] + ":" + m[2]
	}
	switch {
	case strings.Contains(s, "竖"):
		return "9:16"
	case strings.Contains(s, "横"):
		return "16:9"
	case strings.Contains(s, "正方") || strings.Contains(s, "方形"):
		return "1:1"
	}
	return ""
}

// NormalizeResolution 从回答里取清晰度：「高清 720P」→ 720p、「4K」→ 4k。只写了「高清」这类词时取不到，返回 ""。
func NormalizeResolution(s string) string {
	m := resolutionPattern.FindStringSubmatch(strings.ToLower(s))
	switch {
	case m == nil:
		return ""
	case m[1] != "":
		return m[1] + "p"
	default:
		return m[2] + "k"
	}
}

// ParseDurationSec 从回答里取时长（秒）：「15 秒（推荐）」→ 15。取第一个数；不在 1~600 之间或没有数时返回 0。
func ParseDurationSec(s string) int {
	n, err := strconv.Atoi(secondsPattern.FindString(s))
	if err != nil || n <= 0 || n > 600 {
		return 0
	}
	return n
}
