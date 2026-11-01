package domain

import (
	"bytes"
	"encoding/json"
	"math"
	"testing"
	"time"

	gendomain "github.com/li904338203/tabtv/backend/internal/generation/domain"
)

// MCPT-10 卡片的三条指纹用例（规范化本身的完整表驱动在 fingerprint_test.go）。

func TestFingerprint_键序与数字写法不影响(t *testing.T) {
	a := Fingerprint(QuoteToolGenerate, json.RawMessage(`{"modelCode":"m","durationSec":5,"inputs":{"image":1,"text":1}}`))
	for _, s := range []string{
		`{"inputs":{"text":1,"image":1},"durationSec":5.0,"modelCode":"m"}`,
		`{"durationSec":5e0,"modelCode":"m","inputs":{"image":1.0,"text":1}}`,
	} {
		if !bytes.Equal(a, Fingerprint(QuoteToolGenerate, json.RawMessage(s))) {
			t.Errorf("%s 应与基准同指纹", s)
		}
	}
}

func TestFingerprint_去掉确认字段(t *testing.T) {
	a := Fingerprint(QuoteToolGenerate, json.RawMessage(`{"modelCode":"m","count":1}`))
	b := Fingerprint(QuoteToolGenerate, json.RawMessage(`{"confirmQuoteId":"123","modelCode":"m","count":1,"confirmCredits":30}`))
	if !bytes.Equal(a, b) {
		t.Fatal("带确认字段的重试必须与第一次同指纹")
	}
}

func TestFingerprint_null与缺失不同(t *testing.T) {
	a := Fingerprint(QuoteToolGenerate, json.RawMessage(`{"modelCode":"m","text":null}`))
	b := Fingerprint(QuoteToolGenerate, json.RawMessage(`{"modelCode":"m"}`))
	if bytes.Equal(a, b) {
		t.Fatal("null 与缺失不等价")
	}
	// 字符串原样：前后空格也是内容（不 trim）。
	c := Fingerprint(QuoteToolGenerate, json.RawMessage(`{"text":" 猫"}`))
	d := Fingerprint(QuoteToolGenerate, json.RawMessage(`{"text":"猫"}`))
	if bytes.Equal(c, d) {
		t.Fatal("字符串不得 trim")
	}
}

func TestDecideConfirm(t *testing.T) {
	now := time.Date(2026, 10, 17, 8, 0, 0, 0, time.UTC)
	fp := bytes.Repeat([]byte{1}, 32)
	base := func() *Quote {
		return &Quote{ID: 9, GrantID: 5, Fingerprint: fp, Credits: 30, ExpiresAt: now.Add(10 * time.Minute)}
	}
	proof := &Proof{QuoteID: 9, Credits: 30, Via: ProofBaseline}
	pageProof := &Proof{QuoteID: 9, Credits: 30, Via: ProofPage}
	cases := []struct {
		name     string
		q        func() *Quote
		proof    *Proof
		fp       []byte
		grant    int64
		requoted int
		now      time.Time
		want     Decision
	}{
		{"缺确认", base, nil, fp, 5, NotRequoted, now, Decision{Kind: DecideNeedConfirm}},
		{"他人 quote", base, proof, fp, 6, NotRequoted, now, Decision{Kind: DecideInvalid, Reason: ReasonNotFound}},
		{"不存在", func() *Quote { return nil }, proof, fp, 5, NotRequoted, now,
			Decision{Kind: DecideInvalid, Reason: ReasonNotFound}},
		{"指纹不符", base, proof, bytes.Repeat([]byte{2}, 32), 5, NotRequoted, now,
			Decision{Kind: DecideInvalid, Reason: ReasonFingerprintMismatch}},
		{"金额不符", base, &Proof{QuoteID: 9, Credits: 29}, fp, 5, NotRequoted, now,
			Decision{Kind: DecideInvalid, Reason: ReasonAmountMismatch}},
		{"已作废不重放", func() *Quote { q := base(); q.Used, q.VoidedReason = true, "price_changed"; return q },
			proof, fp, 5, NotRequoted, now, Decision{Kind: DecideInvalid, Reason: ReasonVoided}},
		{"已用转重放", func() *Quote { q := base(); q.Used = true; return q }, proof, fp, 5, NotRequoted, now,
			Decision{Kind: DecideReplay}},
		{"已用且过期仍重放", func() *Quote { q := base(); q.Used = true; return q }, proof, fp, 5, NotRequoted,
			now.Add(time.Hour), Decision{Kind: DecideReplay}},
		{"过期", base, proof, fp, 5, NotRequoted, now.Add(10 * time.Minute),
			Decision{Kind: DecideInvalid, Reason: ReasonExpired}},
		{"价格变化", base, proof, fp, 5, 31, now, Decision{Kind: DecidePriceChanged}},
		{"放行（未重新报价）", base, proof, fp, 5, NotRequoted, now, Decision{Kind: DecideProceed}},
		{"放行（价格未变）", base, proof, fp, 5, 30, now, Decision{Kind: DecideProceed}},
		{"elicitation 与基线同一判定", base, &Proof{QuoteID: 9, Credits: 30, Via: ProofElicitation}, fp, 5, 30, now,
			Decision{Kind: DecideProceed}},
		// 页面审批（MCPT-13）
		{"已拒绝：基线回显也无效", func() *Quote { q := base(); q.Rejected = true; return q }, proof, fp, 5, NotRequoted,
			now, Decision{Kind: DecideInvalid, Reason: ReasonRejected}},
		{"已拒绝：页面模式无效", func() *Quote { q := base(); q.Rejected = true; return q }, pageProof, fp, 5,
			NotRequoted, now, Decision{Kind: DecideInvalid, Reason: ReasonRejected}},
		{"页面模式：回显正确但未批准", base, pageProof, fp, 5, NotRequoted, now, Decision{Kind: DecideAwaitApproval}},
		{"页面模式：未批准时价格变化也先等批准", base, pageProof, fp, 5, 31, now, Decision{Kind: DecideAwaitApproval}},
		{"页面模式：已批准放行", func() *Quote { q := base(); q.Approved = true; return q }, pageProof, fp, 5, 30, now,
			Decision{Kind: DecideProceed}},
		{"页面模式：已批准后价格变化", func() *Quote { q := base(); q.Approved = true; return q }, pageProof, fp, 5, 31,
			now, Decision{Kind: DecidePriceChanged}},
		{"页面模式：已批准但过期", func() *Quote { q := base(); q.Approved = true; return q }, pageProof, fp, 5,
			NotRequoted, now.Add(10 * time.Minute), Decision{Kind: DecideInvalid, Reason: ReasonExpired}},
		{"页面模式：已用转重放", func() *Quote { q := base(); q.Approved, q.Used = true, true; return q }, pageProof, fp,
			5, NotRequoted, now, Decision{Kind: DecideReplay}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := DecideConfirm(c.q(), c.proof, c.fp, c.grant, c.requoted, c.now); got != c.want {
				t.Fatalf("得到 %+v，期望 %+v", got, c.want)
			}
		})
	}
}

func TestIdemKey_长度与字符集合法(t *testing.T) {
	for _, k := range []string{IdemKey(1, 1), IdemKey(math.MaxInt64, math.MaxInt64)} {
		if err := gendomain.ValidateIdempotencyKey(k); err != nil {
			t.Fatalf("%s：%v", k, err)
		}
		// 同时用作建节点的画布批次（varchar(64)），link 批次再加 ":link"。
		if n := len(k + ":link"); n > 64 {
			t.Fatalf("%s:link 长 %d，超过画布批次列", k, n)
		}
	}
	if IdemKey(5, 9) != "mcp:5:9" {
		t.Fatalf("格式：%s", IdemKey(5, 9))
	}
}
