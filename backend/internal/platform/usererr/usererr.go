// Package usererr 把上游（模型厂商、中转）的原始报错翻成**给用户看的中文**（2026-10-31 用户要求：
// 上游返回的错误不能直接返回给前端用户）。
//
// 原话只进日志与管理端（generation.error_detail、请求日志）；凡是会出现在用户面前的地方（任务状态、
// 积分流水的原因、站内通知、Agent、MCP）都只放这里给出的文案。
//
// ⚠️ 只依赖标准库：生成域、脚本域、智能剪辑、导演台、Agent 都要用它，放进哪个域都会让别的域反向依赖。
package usererr

import (
	"errors"
	"regexp"
	"strconv"
	"strings"
)

// 给用户看的几类文案。都以「可以怎么办」收尾：只说「失败了」用户不知道下一步做什么。
const (
	ContentPolicy = "内容审核未通过，请调整描述、换一个选项或换一张图后重试"
	RateLimited   = "模型服务繁忙，请稍后重试"
	Unavailable   = "模型服务暂时不可用，请稍后重试"
	Misconfigured = "模型服务暂不可用，请联系运营处理"
	Rejected      = "模型未能处理这次请求，请调整描述或参数后重试"
	Timeout       = "模型响应超时，请稍后重试"
	Network       = "连接模型服务失败，请稍后重试"
	NoOutput      = "模型没有返回结果，请重试"
	Generic       = "生成失败，请稍后重试"
)

var contentMarkers = []string{"内容政策", "内容安全", "内容审核", "content_policy", "content policy", "safety system",
	"safety_violation", "moderation", "nsfw", "违反", "敏感"}

var statusRe = regexp.MustCompile(`(?i)http[^0-9]{0,6}([1-5][0-9]{2})`)

// FromUpstream 把一段上游 / Provider 层的原始报错翻成用户文案。认不出来时给兜底文案，**绝不原样返回**。
func FromUpstream(raw string) string {
	lower := strings.ToLower(raw)
	if lower == "" {
		return Generic
	}
	for _, m := range contentMarkers {
		if strings.Contains(lower, strings.ToLower(m)) {
			return ContentPolicy
		}
	}
	switch {
	case strings.Contains(lower, "timeout") || strings.Contains(lower, "deadline") || strings.Contains(raw, "超时"):
		return Timeout
	case strings.Contains(lower, "connection refused") || strings.Contains(lower, "no such host") ||
		strings.Contains(lower, "connection reset") || strings.Contains(lower, "eof") || strings.Contains(raw, "连接"):
		return Network
	case strings.Contains(raw, "没有返回") || strings.Contains(raw, "未返回") || strings.Contains(raw, "未解析出产物") ||
		strings.Contains(lower, "no output") || strings.Contains(lower, "no image"):
		return NoOutput
	}
	if m := statusRe.FindStringSubmatch(raw); m != nil {
		code, _ := strconv.Atoi(m[1])
		switch {
		case code == 429:
			return RateLimited
		case code == 401 || code == 402 || code == 403 || code == 404:
			return Misconfigured
		case code >= 500:
			return Unavailable
		case code >= 400:
			return Rejected
		}
	}
	return Generic
}

// Error 是给用户看的错误：Error() 只给归类后的文案，Unwrap 保留原错误（调用方照样能 errors.Is / As，
// 例如判 context.Canceled），Detail 给日志与管理端用。
type Error struct {
	Msg string
	Raw error
}

func (e *Error) Error() string { return e.Msg }

func (e *Error) Unwrap() error { return e.Raw }

// Detail 是上游原话（只进日志 / 管理端）。
func (e *Error) Detail() string { return e.Raw.Error() }

// Wrap 把一个上游 / Provider 层的错误包成给用户看的错误；nil 原样返回，已包过的不再包。
//
// ⚠️ 用在**同步**调用的出口（文本模型、Agent 推理）：那些错误会被调用方原样写进脚本调用记录、
// 轮次状态、HTTP 响应，在出口包一次比在每个调用方各翻一遍可靠 —— 漏掉一处不会有编译错误。
func Wrap(err error) error {
	if err == nil {
		return nil
	}
	var ue *Error
	if errors.As(err, &ue) {
		return err
	}
	return &Error{Msg: FromUpstream(err.Error()), Raw: err}
}
