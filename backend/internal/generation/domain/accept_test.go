package domain

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// 服务端"没有就生成一个"会让每次重试都是新键，重试就变成了重复扣费。
func TestValidateIdempotencyKey_必填(t *testing.T) {
	if err := ValidateIdempotencyKey(""); !errors.Is(err, ErrIdempotencyKey) {
		t.Fatal("空幂等键应被拒")
	}
}

func TestValidateIdempotencyKey_字符集与长度(t *testing.T) {
	ok := []string{
		"abc123", "gen-2026-09-21", "node_1:retry.2",
		strings.Repeat("a", MaxIdempotencyKeyLen),
	}
	for _, k := range ok {
		if err := ValidateIdempotencyKey(k); err != nil {
			t.Errorf("%q 应合法，实际 %v", k[:min(len(k), 20)], err)
		}
	}
	bad := map[string]string{
		"超长":   strings.Repeat("a", MaxIdempotencyKeyLen+1),
		"含空格":  "gen 1",
		"含斜杠":  "gen/1",
		"含中文":  "任务一",
		"含换行":  "gen\n1",
		"含百分号": "gen%2F1",
	}
	for name, k := range bad {
		if err := ValidateIdempotencyKey(k); err == nil {
			t.Errorf("%s 应被拒: %q", name, k)
		}
	}
}

// 不存价格的话，事后调价会让历史任务的费用无法解释。
func TestBuildSnapshot_冻结受理那一刻的事实(t *testing.T) {
	raw, err := BuildSnapshot(Snapshot{
		ModelCode: "gpt_text", ModelID: 123, OperationCode: "text2img",
		ProviderCode: "openai", APIProtocol: "openai_images", ResultType: "image",
		PriceUnit: "per_image", PriceCredits: 5, CreditsCost: 10,
		EstSeconds: 30, PromptSuffix: "水墨风格",
	})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"modelCode", "operationCode", "providerCode", "apiProtocol",
		"resultType", "priceUnit", "priceCredits", "creditsCost", "estSeconds", "promptSuffix"} {
		if _, ok := got[k]; !ok {
			t.Errorf("快照缺少 %s", k)
		}
	}
	// ID 对外序列化为 string，防 JS 精度丢失。
	if _, ok := got["modelId"].(string); !ok {
		t.Errorf("modelId 应序列化为 string，实际 %T", got["modelId"])
	}
}

func TestBuildSnapshot_必填校验(t *testing.T) {
	cases := map[string]Snapshot{
		"缺模型":   {OperationCode: "text2img", ResultType: "image"},
		"缺操作":   {ModelCode: "m", ResultType: "image"},
		"缺结果类型": {ModelCode: "m", OperationCode: "text2img"},
		"费用为负":  {ModelCode: "m", OperationCode: "o", ResultType: "image", CreditsCost: -1},
	}
	for name, s := range cases {
		if _, err := BuildSnapshot(s); !errors.Is(err, ErrSnapshot) {
			t.Errorf("%s 应被拒，实际 %v", name, err)
		}
	}
}

// 死线必须在受理时算好存下来：恢复扫描按当前配置重算，
// 会让调大 est 后一批本该超时的僵尸任务突然"又活了"。
func TestDeadlineAt(t *testing.T) {
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		est  int
		want int // 秒
	}{
		{est: 0, want: MinDeadlineSeconds},  // 没配 est 也要有下限
		{est: 5, want: MinDeadlineSeconds},  // 5×3=15 < 30，取下限
		{est: 10, want: MinDeadlineSeconds}, // 恰好 30
		{est: 30, want: 90},                 // 90 秒
		{est: 200, want: 600},               // 长任务
	}
	for _, c := range cases {
		got := DeadlineAt(now, c.est)
		want := now.Add(time.Duration(c.want) * time.Second)
		if !got.Equal(want) {
			t.Errorf("est=%d 期望死线 %v，实际 %v", c.est, want, got)
		}
	}
}

// 消息在队列里可能待很久，带着快照等于把同一份数据存两处。
func TestOutboxPayload_只带路由信息(t *testing.T) {
	raw, err := json.Marshal(OutboxPayload{
		GenerationID: "123", WorkspaceID: "456", PayerAccountID: "789",
		OperationCode: "text2img", ModelCode: "gpt_image", SchemaVersion: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	for _, forbidden := range []string{"params", "inputs", "snapshot", "credits"} {
		if strings.Contains(s, forbidden) {
			t.Errorf("消息体不该带 %s：Worker 会回库读最新状态", forbidden)
		}
	}
	// ID 一律 string。
	var got map[string]any
	_ = json.Unmarshal(raw, &got)
	if _, ok := got["generationId"].(string); !ok {
		t.Error("generationId 应为 string")
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
