package domain

import (
	"errors"
	"strings"
	"testing"
)

func TestIdempotencyKey_轮次不同键就不同(t *testing.T) {
	// ⚠️ 轮次按**调用次数**算（含失败）。只按产出物算的话，一次失败
	// 什么都没改变，重试会算出同一个键、命中那条 failed 记录，
	// 于是永远 40902 —— 用户再也点不动「重新生成」。
	k1 := IdempotencyKey(PurposeStoryboard, 12345, 1, "gpt-x")
	k2 := IdempotencyKey(PurposeStoryboard, 12345, 2, "gpt-x")
	if k1 == k2 {
		t.Fatalf("不同轮次必须产出不同的键: %s", k1)
	}
	// 同一轮次同一参数则稳定 —— 这才是幂等要保的。
	if k1 != IdempotencyKey(PurposeStoryboard, 12345, 1, "gpt-x") {
		t.Fatal("同一轮次的键必须稳定")
	}
}

func TestIdempotencyKey_可变部分先哈希(t *testing.T) {
	// ⚠️ 列宽 varchar(128)。可变部分可能是一串实体名或镜头 ID，
	// 六个类型名拼出来就超了。
	long := strings.Repeat("超长的可变部分", 100)
	key := IdempotencyKey(PurposeCompose, 999, 7, long)
	if n := len([]rune(key)); n > MaxIdempotencyKeyRunes {
		t.Fatalf("键长 %d 超过列宽 %d: %s", n, MaxIdempotencyKeyRunes, key)
	}
	// 不同的可变部分要产出不同的键，否则哈希就白做了。
	other := IdempotencyKey(PurposeCompose, 999, 7, long+"x")
	if key == other {
		t.Fatal("不同的可变部分应产出不同的键")
	}
}

func TestIdempotencyKey_不同脚本不同用途都不撞(t *testing.T) {
	seen := map[string]bool{}
	for _, k := range []string{
		IdempotencyKey(PurposeStoryboard, 1, 1, ""),
		IdempotencyKey(PurposeStoryboard, 2, 1, ""),
		IdempotencyKey(PurposeCompose, 1, 1, ""),
	} {
		if seen[k] {
			t.Fatalf("键撞了: %s", k)
		}
		seen[k] = true
	}
}

func TestDecideSettle_解析失败必须退款(t *testing.T) {
	// ⚠️ 这是脚本域最贵的一条规则。老系统的顺序是「调模型 → 结算 → 解析」，
	// 于是分镜 JSON 解析失败时已经扣了 60 积分、库里落 0 个镜头 ——
	// 用户看到一个空脚本和一笔说不清的消费。
	d := DecideSettle(nil, errors.New("裸引号"))
	if d.Settle {
		t.Fatal("解析失败绝不能结算")
	}
	if !d.Refund {
		t.Fatal("解析失败必须退款：用户拿到的是一个空脚本")
	}
	if d.Status != LLMFailed {
		t.Fatalf("状态应为 failed: %s", d.Status)
	}
	if !strings.Contains(d.Reason, "裸引号") {
		t.Fatalf("原因要带上模型说了什么: %s", d.Reason)
	}
}

func TestDecideSettle_调用失败也退款(t *testing.T) {
	d := DecideSettle(errors.New("上游 502"), nil)
	if d.Settle || !d.Refund || d.Status != LLMFailed {
		t.Fatalf("调用失败应退款并标 failed: %+v", d)
	}
}

func TestDecideSettle_两步都过才结算(t *testing.T) {
	d := DecideSettle(nil, nil)
	if !d.Settle {
		t.Fatal("调用与解析都成功才该结算")
	}
	if d.Refund {
		t.Fatal("成功不该退款")
	}
	if d.Status != LLMSuccess {
		t.Fatalf("状态应为 success: %s", d.Status)
	}
}

func TestDecideSettle_调用失败优先于解析(t *testing.T) {
	// 调用都没成功，解析错误是派生的，原因要指向根因。
	d := DecideSettle(errors.New("连接超时"), errors.New("空响应"))
	if !strings.Contains(d.Reason, "连接超时") {
		t.Fatalf("原因应指向根因: %s", d.Reason)
	}
}
