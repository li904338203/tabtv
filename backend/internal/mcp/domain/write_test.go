package domain

import (
	"math"
	"strings"
	"testing"
)

func TestBatchID_不超过64字符(t *testing.T) {
	long := strings.Repeat("键", 500)
	for _, c := range []struct{ key, fb string }{{long, ""}, {"", long}, {"a", ""}, {"", ""}} {
		id := CanvasBatchID(math.MaxInt64, c.key, c.fb)
		if len(id) > 64 || !strings.HasPrefix(id, "mcp:9223372036854775807:") {
			t.Fatalf("批次 ID 超长或前缀不对：%q（%d）", id, len(id))
		}
	}
	if k := ProjectIdemKey(math.MaxInt64, long); len(k) > 64 || !strings.HasPrefix(k, "mcp:") {
		t.Fatalf("项目幂等键超长：%q", k)
	}
	// 同键同授权稳定；换授权、换来源（k / t）都不同 —— 两个客户端用了同一个键不能互相命中。
	if CanvasBatchID(1, "x", "") != CanvasBatchID(1, "x", "trace") {
		t.Fatal("给了幂等键时批次只由键决定")
	}
	if CanvasBatchID(1, "x", "") == CanvasBatchID(2, "x", "") || CanvasBatchID(1, "x", "") == CanvasBatchID(1, "", "x") {
		t.Fatal("不同授权 / 不同来源的批次不能相同")
	}
	if ProjectIdemKey(1, "x") == ProjectIdemKey(2, "x") {
		t.Fatal("不同授权的项目幂等键不能相同")
	}
}

// 多段工具（MCPT-16）的批次：建节点那一批 + 写回后缀（:script / :group，canvasgen.WriteBackBatchID）
// 都要 ≤64（canvas_op_log.batch_id）；花钱方法的批次是确认派生的幂等键 mcp:{grant}:{quote}。
func TestObjectBatchID_写回后缀也不超过64字符(t *testing.T) {
	const maxID = int64(9223372036854775807)
	for _, b := range []string{ObjectBatchID(maxID, strings.Repeat("键", 300), ""), ObjectBatchID(maxID, "", "trace"),
		IdemKey(maxID, maxID)} {
		for _, suffix := range []string{":script", ":group", ":link"} {
			if n := len(b + suffix); n > 64 {
				t.Errorf("%s%s 长 %d", b, suffix, n)
			}
		}
	}
	if ObjectBatchID(1, "a", "") == ObjectBatchID(2, "a", "") || ObjectBatchID(1, "a", "") != ObjectBatchID(1, "a", "x") {
		t.Error("同授权同键同批次、不同授权不同批次")
	}
	if ObjectBatchID(1, "", "t1") == ObjectBatchID(1, "", "t2") {
		t.Error("不带键时每次调用是新的一批")
	}
}
