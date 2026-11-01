package domain

import (
	"reflect"
	"strings"
	"testing"
)

func TestCalcCredits_按次按张(t *testing.T) {
	got, err := CalcCredits(QuoteInput{Unit: UnitPerTime, Credits: 10, Count: 3})
	if err != nil || got != 30 {
		t.Errorf("per_time: got=%d err=%v，期望 30", got, err)
	}
	got, _ = CalcCredits(QuoteInput{Unit: UnitPerImage, Credits: 5, Count: 4})
	if got != 20 {
		t.Errorf("per_image: got=%d，期望 20", got)
	}
	// count 缺省为 1
	got, _ = CalcCredits(QuoteInput{Unit: UnitPerTime, Credits: 7})
	if got != 7 {
		t.Errorf("count 缺省应为 1，got=%d", got)
	}
}

// ⚠️ per_second 不乘 count。旧实现曾乘过，
// 导致一次多产出请求按 N 倍时长计费。
func TestCalcCredits_按秒不乘count(t *testing.T) {
	got, _ := CalcCredits(QuoteInput{Unit: UnitPerSecond, Credits: 2, DurationSec: 10, Count: 4})
	if got != 20 {
		t.Errorf("per_second 应为 2×10=20（不乘 count），实际 %d", got)
	}
	// 缺省 5 秒
	got, _ = CalcCredits(QuoteInput{Unit: UnitPerSecond, Credits: 3})
	if got != 15 {
		t.Errorf("per_second 缺省应按 5 秒，实际 %d", got)
	}
}

// ⚠️ 最容易错的一条：per_kchar 按 **Unicode 码点**不按字节。
// Go 的 len(s) 是字节数，中文一个字 3 字节 —— 用它算中文用户被多收 3 倍。
func TestCalcCredits_按千字用码点(t *testing.T) {
	// 1000 个中文字 = 1000 码点 = 3000 字节。
	text := strings.Repeat("字", 1000)
	got, _ := CalcCredits(QuoteInput{Unit: UnitPerKChar, Credits: 6, Text: text})
	if got != 6 {
		t.Fatalf("1000 个中文字应算 1 个千字单位（6 积分），实际 %d —— 若为 18 则是按字节算的", got)
	}

	// 1001 码点 → 向上取整成 2 个单位
	got, _ = CalcCredits(QuoteInput{Unit: UnitPerKChar, Credits: 6, Text: strings.Repeat("字", 1001)})
	if got != 12 {
		t.Errorf("1001 字应向上取整为 2 单位，实际 %d", got)
	}

	// 空文本也至少 1 个单位，避免 0 积分调用。
	got, _ = CalcCredits(QuoteInput{Unit: UnitPerKChar, Credits: 6, Text: ""})
	if got != 6 {
		t.Errorf("空文本应至少计 1 单位，实际 %d", got)
	}

	// per_kchar 同样不乘 count
	got, _ = CalcCredits(QuoteInput{Unit: UnitPerKChar, Credits: 6, Text: "短", Count: 5})
	if got != 6 {
		t.Errorf("per_kchar 不应乘 count，实际 %d", got)
	}
}

func TestCalcCredits_非法输入(t *testing.T) {
	if _, err := CalcCredits(QuoteInput{Unit: "per_token", Credits: 1}); err == nil {
		t.Error("未知单位应被拒绝")
	}
	if _, err := CalcCredits(QuoteInput{Unit: UnitPerTime, Credits: -1}); err == nil {
		t.Error("负单价应被拒绝")
	}
	// 溢出保护：积分是整数，乘法前必须查（GO-19）。
	if _, err := CalcCredits(QuoteInput{Unit: UnitPerSecond, Credits: 1 << 20, DurationSec: 1 << 20}); err == nil {
		t.Error("溢出应被拒绝")
	}
}

// 操作级估时优先：同一文本模型做纯文本润色十几秒、做图片反推要几分钟，
// 只有模型级 est 会让视觉理解请求撞上按短耗时算的超时墙。
func TestResolveEstSeconds(t *testing.T) {
	n := func(v int) *int { return &v }
	if got := ResolveEstSeconds(n(180), 20); got != 180 {
		t.Errorf("操作级应优先，实际 %d", got)
	}
	if got := ResolveEstSeconds(nil, 45); got != 45 {
		t.Errorf("无操作级应回落模型级，实际 %d", got)
	}
	if got := ResolveEstSeconds(n(0), 45); got != 45 {
		t.Errorf("操作级为 0 应回落模型级，实际 %d", got)
	}
	if got := ResolveEstSeconds(nil, 0); got != 30 {
		t.Errorf("都没有应回落 30，实际 %d", got)
	}
}

func TestCalcDeadlineSec(t *testing.T) {
	if got := CalcDeadlineSec(120); got != 360 {
		t.Errorf("est×3，实际 %d", got)
	}
	// 短任务也要有 30 秒下限，否则网络抖动就会误判超时。
	if got := CalcDeadlineSec(5); got != MinDeadlineSec {
		t.Errorf("应有 30 秒下限，实际 %d", got)
	}
}

// 精确价优先，回落兜底价（model_id 为 null）。
func TestResolvePrice(t *testing.T) {
	prices := []Price{
		{OperationCode: "text2img", ModelCode: "", Credits: 10, Unit: UnitPerImage, Enabled: true},
		{OperationCode: "text2img", ModelCode: "dalle3", Credits: 20, Unit: UnitPerImage, Enabled: true},
		{OperationCode: "text2img", ModelCode: "cheap", Credits: 5, Unit: UnitPerImage, Enabled: false},
	}

	p, ok := ResolvePrice(prices, "text2img", "dalle3")
	if !ok || p.Credits != 20 {
		t.Errorf("应命中精确价 20，实际 %+v ok=%v", p, ok)
	}

	p, ok = ResolvePrice(prices, "text2img", "unknown_model")
	if !ok || p.Credits != 10 {
		t.Errorf("未知模型应回落兜底价 10，实际 %+v", p)
	}

	// 停用的精确价不应被选中，要回落兜底。
	p, ok = ResolvePrice(prices, "text2img", "cheap")
	if !ok || p.Credits != 10 {
		t.Errorf("停用的精确价应被跳过并回落兜底，实际 %+v", p)
	}

	// 既无精确也无兜底 → 无法报价（发布检查要拦的情况）。
	if _, ok := ResolvePrice(prices, "text2video", "any"); ok {
		t.Error("无任何价格时应返回 false")
	}
}

// 停用或删除一条价格之前要知道：哪些在用的模型会因此报不出价（02 §3.3）。
func TestQuoteLosers(t *testing.T) {
	fallback := Price{OperationCode: "text2img", Credits: 10, Unit: UnitPerImage, Enabled: true}
	exactA := Price{OperationCode: "text2img", ModelCode: "a", Credits: 20, Unit: UnitPerImage, Enabled: true}
	offB := Price{OperationCode: "text2img", ModelCode: "b", Credits: 5, Unit: UnitPerImage, Enabled: false}
	live := []LiveBinding{{"a", "text2img"}, {"b", "text2img"}, {"c", "text2img"}, {"a", "img2img"}}

	cases := []struct {
		name   string
		prices []Price
		target Price
		want   []string
	}{
		// 兜底价被 b（精确价停用）与 c（没有精确价）靠着；a 有自己的精确价，不受影响
		{"去掉兜底价", []Price{fallback, exactA, offB}, fallback, []string{"b", "c"}},
		// 还有兜底价托底：去掉 a 的精确价，a 回落兜底，照样报得出价
		{"有兜底时去掉精确价", []Price{fallback, exactA}, exactA, nil},
		// 没有兜底：a 的精确价是它最后一条有效价格
		{"无兜底时去掉精确价", []Price{exactA}, exactA, []string{"a"}},
		// 本来就停用的价格：去掉它什么都不变
		{"去掉已停用的价格", []Price{offB}, offB, nil},
		// 别的操作的价格不牵连这个操作（img2img 没有任何价格，本来就报不出价）
		{"只看同一操作", []Price{exactA}, Price{OperationCode: "img2img", ModelCode: "a", Unit: UnitPerImage}, nil},
	}
	for _, c := range cases {
		got := QuoteLosers(c.prices, live, c.target)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s：得到 %v，应为 %v", c.name, got, c.want)
		}
	}
}

func TestValidatePrice_单价上限(t *testing.T) {
	if err := ValidatePrice(Price{OperationCode: "x", Unit: UnitPerTime, Credits: MaxPriceCredits}); err != nil {
		t.Fatalf("上限本身应通过：%v", err)
	}
	if err := ValidatePrice(Price{OperationCode: "x", Unit: UnitPerTime, Credits: MaxPriceCredits + 1}); err == nil {
		t.Fatal("超过上限应被拒 —— 否则写进 integer 列时是 500")
	}
}

func TestValidatePrice(t *testing.T) {
	if err := ValidatePrice(Price{OperationCode: "text2img", Unit: UnitPerImage, Credits: 10}); err != nil {
		t.Errorf("正常价格应通过: %v", err)
	}
	for _, bad := range []Price{
		{Unit: UnitPerImage, Credits: 10},
		{OperationCode: "x", Unit: "per_token", Credits: 10},
		{OperationCode: "x", Unit: UnitPerImage, Credits: -1},
	} {
		if err := ValidatePrice(bad); err == nil {
			t.Errorf("非法价格应被拒绝: %+v", bad)
		}
	}
}
