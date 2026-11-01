package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// LLM 调用的轮次与幂等键。

// Purpose 是一次同步 LLM 调用的用途。
type Purpose string

const (
	PurposeStoryboard Purpose = "storyboard_gen"
	PurposeCompose    Purpose = "prompt_compose"
)

// MaxIdempotencyKeyRunes 与 `script_llm_call.idempotency_key` 的列宽一致。
const MaxIdempotencyKeyRunes = 128

// IdempotencyKey 生成一次同步 LLM 调用的幂等键。
//
// ⚠️ 两条都是踩出来的：
//
//	① **轮次必须随每次重新生成而变，且失败也要计数**。
//	   轮次取自 `script_llm_call` 的**调用次数**，不是产出物的数量 ——
//	   只按产出物算的话，一次失败什么都没改变，重试会算出同一个键、
//	   命中那条 failed 记录，于是永远 40902，**用户再也点不动「重新生成」**。
//
//	② **可变部分必须先哈希**。列宽 varchar(128)，而可变部分可能是
//	   一串实体名或镜头 ID —— 六个类型名拼出来就超了。
//
// 形如 `storyboard:12345:r3:9f86d081`。
func IdempotencyKey(purpose Purpose, scriptID int64, round int, variant string) string {
	key := fmt.Sprintf("%s:%d:r%d", purpose, scriptID, round)
	if variant != "" {
		sum := sha256.Sum256([]byte(variant))
		key += ":" + hex.EncodeToString(sum[:4])
	}
	if len([]rune(key)) > MaxIdempotencyKeyRunes {
		// 兜底：极端情况下 purpose 变长也不能撑爆列。
		sum := sha256.Sum256([]byte(key))
		key = fmt.Sprintf("%s:%d:%s", purpose, scriptID, hex.EncodeToString(sum[:8]))
	}
	return key
}

// LLMStatus 是同步调用的状态。
type LLMStatus string

const (
	LLMRunning LLMStatus = "running"
	LLMSuccess LLMStatus = "success"
	LLMFailed  LLMStatus = "failed"
)

// SettleDecision 描述一次同步调用该怎么结账。
//
// ⚠️ 这是脚本域最关键的一条规则，单独建模是为了让它有测试可钉：
// **结算必须晚于产出校验**。
//
// 老系统的顺序是「调模型 → settle → 解析」，于是分镜 JSON 解析失败时
// 已经扣了 60 积分、库里落 0 个镜头 —— 用户看到一个空脚本和一笔
// 说不清的消费。而且同步调用的入口方法没有事务可回滚
// （事务内本就禁止远程调用），扣款一旦发生就只能靠退款补。
//
// 正确顺序：`reserve → 调模型 → **先解析** → 解析通过才 settle；
// 解析失败走退款 + 标 failed`。
type SettleDecision struct {
	// Settle 为真表示这次调用产出可用，该记消费。
	Settle bool
	// Refund 为真表示要把预扣的积分退回去。
	Refund bool
	Status LLMStatus
	Reason string
}

// DecideSettle 按「调用是否成功 + 产出是否解析通过」给出结账决定。
func DecideSettle(callErr error, parseErr error) SettleDecision {
	switch {
	case callErr != nil:
		return SettleDecision{Refund: true, Status: LLMFailed,
			Reason: "调用模型失败：" + callErr.Error()}
	case parseErr != nil:
		// ⚠️ 上游其实成功了（我们可能真的被上游计了费），但产出对我们无用。
		// 仍然全额退给用户 —— 让用户为一个空脚本付费是更糟的选择，
		// 上游那笔成本由我们承担并在 provider_request_log 里可查。
		return SettleDecision{Refund: true, Status: LLMFailed,
			Reason: "模型产出无法使用：" + parseErr.Error()}
	}
	return SettleDecision{Settle: true, Status: LLMSuccess}
}
