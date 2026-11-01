package domain

import (
	"bytes"
	"strconv"
	"time"
)

// 花钱确认的纯判定（MCPT-10，MT-7，开发 01 §5）：基线（confirmQuoteId + 回显 confirmCredits）与
// elicitation 两条路径**只是拿到用户同意的方式不同，判定只有这一份**。
//
// ⚠️ 本文件不碰库、不取当前时间（now 由调用方传）、不调报价：重新报价的结果以 requoted 传进来。

// ProofVia 是确认凭证的来源。
type ProofVia string

const (
	// ProofBaseline：模型带 confirmQuoteId + confirmCredits 重调（全兼容基线，MCP-F-42）。
	ProofBaseline ProofVia = "baseline"
	// ProofElicitation：用户在 elicitation 表单里点了确认（MCP-F-43）；Credits 由服务端从 quote 取。
	ProofElicitation ProofVia = "elicitation"
	// ProofPage：凭据开了「花钱需在 seeAitv 页面确认」（MCP-F-44，MCPT-13）。客户端带来的凭证（回显金额 /
	// elicitation 回答）只用来指出是哪一张 quote，**同意本身只认 approved_at**：用户本人在审批页上点了批准。
	ProofPage ProofVia = "page"
)

// Proof 是一次「用户同意了」的凭证。
type Proof struct {
	QuoteID int64
	// Credits 是回显的金额（基线下必须等于报价金额：用户在工具调用审批框里看到的就是这个数）。
	Credits int
	Via     ProofVia
}

// Quote 是判定需要的 mcp_quote 事实（repo.Quote 的投影）。
type Quote struct {
	ID          int64
	GrantID     int64
	Fingerprint []byte
	Credits     int
	ExpiresAt   time.Time
	// Used：used_at 非空（被受理用掉，或被作废 —— 后者 VoidedReason 非空）。
	Used bool
	// VoidedReason 非空表示这张报价被作废（price_changed）而不是被用掉。
	VoidedReason string
	// Approved：approved_at 非空（用户在审批页上批准了，MCPT-13）。
	Approved bool
	// Rejected：rejected_at 非空（用户在审批页上拒绝了）。拒绝的报价**无论哪种凭证都无效**。
	Rejected bool
}

// DecisionKind 是判定结论。
type DecisionKind string

const (
	// DecideNeedConfirm：没有确认凭证 —— 报价并签发 quote，请用户确认。
	DecideNeedConfirm DecisionKind = "need_confirm"
	// DecideInvalid：凭证无效（Reason 说明哪一种），不受理。
	DecideInvalid DecisionKind = "invalid"
	// DecideReplay：quote 已被用掉 —— 用**同一幂等键**再调受理，只会拿到原任务，不新扣费。
	DecideReplay DecisionKind = "replay"
	// DecidePriceChanged：确认之后价格变了 —— 作废旧 quote、签发新报价，不受理（MCP-F-41）。
	DecidePriceChanged DecisionKind = "price_changed"
	// DecideAwaitApproval：页面确认模式下用户还没在审批页上批准 —— 不受理、不扣费，quote 保持未用。
	DecideAwaitApproval DecisionKind = "await_approval"
	// DecideProceed：放行。
	DecideProceed DecisionKind = "proceed"
)

// InvalidReason 是 MCP_QUOTE_INVALID 的 details.reason。
type InvalidReason string

const (
	// ReasonNotFound：quote 不存在**或不属于本授权**（同一错误，不泄露别人的 quote 是否存在）。
	ReasonNotFound InvalidReason = "not_found"
	// ReasonFingerprintMismatch：参数与报价时不同（改了一个字也算）。
	ReasonFingerprintMismatch InvalidReason = "fingerprint_mismatch"
	// ReasonAmountMismatch：回显的 confirmCredits 不等于报价金额。
	ReasonAmountMismatch InvalidReason = "amount_mismatch"
	// ReasonVoided：这张报价已因价格变化作废（要用随作废一起签发的新 quote）。
	ReasonVoided InvalidReason = "voided"
	// ReasonExpired：过期（MCP-PARAM-02，默认 10 分钟）。
	ReasonExpired InvalidReason = "expired"
	// ReasonRejected：用户在审批页上拒绝了这张报价（MCPT-13）。
	ReasonRejected InvalidReason = "rejected"
)

// Decision 是一次判定的结果。
type Decision struct {
	Kind   DecisionKind
	Reason InvalidReason // 只在 DecideInvalid 时有值
}

// NotRequoted 表示「还没重新报价」：第 7 条（价格变化）跳过。
// 调用方先用它判一次，只有结论是 Proceed 才去重新报价、再判一次 —— 凭证本身无效或要重放时
// 不该为它付一次报价查询，更不该因为「现在报不出价」（模型下线）让重放拿不回原任务。
const NotRequoted = -1

// DecideConfirm 按序判定（任务卡 MCPT-10 的表，另加一行「已作废」）：
//
//	1 无 proof                         → NeedConfirm
//	2 quote 不存在 / 不属于本授权       → Invalid(not_found)
//	3 指纹不符                          → Invalid(fingerprint_mismatch)
//	4 confirmCredits != q.credits       → Invalid(amount_mismatch)
//	4' 已作废（price_changed）          → Invalid(voided)
//	4'' 已拒绝（审批页，MCPT-13）       → Invalid(rejected)
//	5 已用                              → Replay
//	6 已过期                            → Invalid(expired)
//	6' 页面确认且未批准                 → AwaitApproval
//	7 重新报价 != q.credits              → PriceChanged
//	8 其余                              → Proceed
//
// ⚠️ 4' 必须排在 5 之前：作废也写 used_at（CAS 防并发），若当成「已用 → 重放」，同一幂等键下
// 查不到任务，受理会**按新价正常受理一次**——用户确认的是旧价，这等于绕过了确认。
// ⚠️ 5 排在 6 之前：用掉的 quote 过期之后重放仍要拿回原任务（客户端第一次的响应丢了，十几分钟后才重试）。
// ⚠️ 4” 不看凭证来源：拒绝之后用户关掉了「页面确认」，客户端拿着同一个 quoteId 回显金额也不能把它复活。
// ⚠️ 6' 只看 approved_at：页面确认模式下客户端的回显金额与 elicitation 回答都不算同意（MCP-AC-15）——
// 客户端被设成自动批准工具时，那两样是模型自己就能给出来的。
func DecideConfirm(q *Quote, proof *Proof, fp []byte, grantID int64, requoted int, now time.Time) Decision {
	switch {
	case proof == nil:
		return Decision{Kind: DecideNeedConfirm}
	case q == nil || q.GrantID != grantID || q.ID != proof.QuoteID:
		return Decision{Kind: DecideInvalid, Reason: ReasonNotFound}
	case !bytes.Equal(q.Fingerprint, fp):
		return Decision{Kind: DecideInvalid, Reason: ReasonFingerprintMismatch}
	case proof.Credits != q.Credits:
		return Decision{Kind: DecideInvalid, Reason: ReasonAmountMismatch}
	case q.VoidedReason != "":
		return Decision{Kind: DecideInvalid, Reason: ReasonVoided}
	case q.Rejected:
		return Decision{Kind: DecideInvalid, Reason: ReasonRejected}
	case q.Used:
		return Decision{Kind: DecideReplay}
	case !now.Before(q.ExpiresAt):
		return Decision{Kind: DecideInvalid, Reason: ReasonExpired}
	case proof.Via == ProofPage && !q.Approved:
		return Decision{Kind: DecideAwaitApproval}
	case requoted != NotRequoted && requoted != q.Credits:
		return Decision{Kind: DecidePriceChanged}
	}
	return Decision{Kind: DecideProceed}
}

// IdemKey 是花钱受理的幂等键：`mcp:{grantId}:{quoteId}`（MCP-F-48）。
//
// 基线与 elicitation 包着同一个 quoteId，两条路径落到同一个键 —— 同一次确认无论从哪条路进来、
// 重试多少次，受理只会产生一个任务。字符集 [0-9a-z:] ⊂ 受理要求的 [A-Za-z0-9-_:.]，
// 长度至多 4+19+1+19 = 43 ≤ 128；同时用作建节点那一批画布操作的 batch_id（varchar(64)），
// link 那一批再加 `:link` 也只有 48。
func IdemKey(grantID, quoteID int64) string {
	return "mcp:" + strconv.FormatInt(grantID, 10) + ":" + strconv.FormatInt(quoteID, 10)
}
