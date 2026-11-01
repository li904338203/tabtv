package domain

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
)

// 报价（MCPT-05，MCP-F-29）的纯计算部分：合计（查溢出）、差额、是否签发 quoteId。
//
// 参数指纹**不在这里另写**：quote 绑定的指纹与调用审计的 args_fingerprint 是同一个
// Fingerprint（fingerprint.go）—— 各写一份的话「这次调用用的是不是那张报价的参数」就对不上了。

// Quote 绑定的工具名（mcp_quote.tool）。单项报价对应 generate，计划项对应 plan。
const (
	QuoteToolGenerate = "generate"
	QuoteToolPlan     = "plan"
)

// MaxQuoteCredits 是一张报价的金额上限：mcp_quote.credits 是 integer（int32）。
const MaxQuoteCredits = math.MaxInt32

// ErrCreditsOverflow 合计超出上限。
var ErrCreditsOverflow = errors.New("报价合计金额溢出")

// QuoteLine 是一项报价的结果（单项或计划项中的一项）。
//
// Available=false 时 Credits 恒为 0，BlockedCode / BlockedReason 说明原因，
// UserFixable 说明是不是用户自己能解决（升级会员 / 调整参数）—— 调用方按这个**分类**定语气，不匹配文案。
type QuoteLine struct {
	Credits          int    `json:"credits"`
	Unit             string `json:"unit,omitempty"`
	EstimatedSeconds int    `json:"estimatedSeconds,omitempty"`
	Available        bool   `json:"available"`
	BlockedCode      string `json:"blockedCode,omitempty"`
	BlockedReason    string `json:"blockedReason,omitempty"`
	UserFixable      bool   `json:"userFixable"`
}

// SumCredits 合计各项积分。**相加前查溢出**（超过 MaxQuoteCredits 报 ErrCreditsOverflow）：
// 溢出后回绕成一个小数字的话，用户会确认一张「看起来很便宜」的报价。负数是调用方的错误。
func SumCredits(credits []int) (int, error) {
	total := 0
	for i, c := range credits {
		if c < 0 {
			return 0, fmt.Errorf("第 %d 项积分为负：%d", i+1, c)
		}
		if c > MaxQuoteCredits-total {
			return 0, ErrCreditsOverflow
		}
		total += c
	}
	return total, nil
}

// Shortfall 差额 = max(0, total − balance)。余额为负（不应出现）时按差额照算。
func Shortfall(total, balance int) int {
	if total <= balance {
		return 0
	}
	return total - balance
}

// QuoteDecision 是「签不签发 quoteId」的结论。
type QuoteDecision struct {
	Issue bool
	// Reason 不签发时的原因（给模型转述给用户），点名是第几项、为什么。
	Reason string
	// Blocked 是不可用项的下标（从 0 起）。
	Blocked []int
}

// DecideQuote：**有任一项不可用就不签发 quoteId** —— 不能让人确认一个跑不了的计划
// （确认之后受理到那一项必然失败，前面的已经扣了钱）。其余项照常报价，由调用方返回。
// 没有任何项也不签发（没有东西可确认）。
func DecideQuote(lines []QuoteLine) QuoteDecision {
	if len(lines) == 0 {
		return QuoteDecision{Reason: "没有可报价的项"}
	}
	var blocked []int
	var parts []string
	for i, l := range lines {
		if l.Available {
			continue
		}
		blocked = append(blocked, i)
		reason := l.BlockedReason
		if reason == "" {
			reason = l.BlockedCode
		}
		parts = append(parts, fmt.Sprintf("第 %d 项：%s", i+1, reason))
	}
	if len(blocked) == 0 {
		return QuoteDecision{Issue: true}
	}
	return QuoteDecision{Blocked: blocked, Reason: fmt.Sprintf("有 %d 项当前不可用，未签发 quoteId（%s）。"+
		"调整或去掉这些项后重新报价", len(blocked), strings.Join(parts, "；"))}
}

// QuoteRetention 是过期 mcp_quote 行的保留期：过期后再留 30 天（**设计建议值，待定**，MCP-PARAM-02 一并定稿）。
//
// 为什么不是过期即删：generation.idempotency_key 是 `mcp:{grantId}:{quoteId}`，排障时要能从任务
// 反查到「用户确认的是哪张报价、多少钱、什么时候」—— 与调用审计同为 30 天（M-7），两边能对上。
// 用掉的 quote 被删之后再重放会得到 MCP_QUOTE_INVALID(not_found)，那时早已过期，不影响正常重试。
const QuoteRetention = 30 * 24 * time.Hour

// QuoteCleanupBatch 是单次清理的行数上限（与调用审计同一个数）。
const QuoteCleanupBatch = 50000
