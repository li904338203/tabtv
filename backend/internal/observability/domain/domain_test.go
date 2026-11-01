package domain

import (
	"testing"
	"time"
)

func TestShouldLog_探活与WS不记(t *testing.T) {
	// ⚠️ 探活按秒级被拉，一天几万条 —— 真正要查的那几条会淹没在里面。
	for _, p := range []string{"/healthz", "/readyz", "/metrics", "/openapi.yaml"} {
		if ShouldLog(p) {
			t.Fatalf("%s 不该记日志", p)
		}
	}
	// ⚠️ WS 的理由不同：握手之后的帧**根本不过 middleware**，
	// 只记一条握手日志会让人以为"这个连接只发了一次请求"。
	if ShouldLog("/api/v1/ws/events") {
		t.Fatal("WebSocket 不该记日志")
	}
	for _, p := range []string{"/api/v1/generations", "/api/v1/admin/tasks"} {
		if !ShouldLog(p) {
			t.Fatalf("%s 应该记日志", p)
		}
	}
}

func TestShouldCaptureBody_只缓存请求体且跳过上传(t *testing.T) {
	if ShouldCaptureBody("GET", "application/json") {
		t.Fatal("GET 没有请求体")
	}
	if !ShouldCaptureBody("POST", "application/json") {
		t.Fatal("POST 的 JSON 体应该缓存")
	}
	// ⚠️ 上传走 multipart，body 就是文件本身 —— 缓存它等于把每次上传的
	// 文件在内存里复制一遍，而日志里只会留下一段无意义的二进制摘要。
	if ShouldCaptureBody("POST", "multipart/form-data; boundary=x") {
		t.Fatal("上传不该缓存请求体")
	}
	if ShouldCaptureBody("PUT", "application/octet-stream") {
		t.Fatal("二进制流不该缓存请求体")
	}
}

// ⚠️ 分母不含 cancelled：取消是用户行为不是故障。并进去的话，用户多取消
// 几次会让成功率看起来在下降，**而系统一切正常**。
func TestSuccessRate_取消不进分母且无数据回负一(t *testing.T) {
	if got := SuccessRate(8, 2); got != 0.8 {
		t.Fatalf("成功率应为 0.8，得到 %v", got)
	}
	// ⚠️ 0% 与"还没有数据"是两件事，而看板上它们长得一模一样 ——
	// 一个是"全挂了"，一个是"今天没人用"。
	if got := SuccessRate(0, 0); got != -1 {
		t.Fatalf("无数据应回 -1（调用方显示「—」），得到 %v", got)
	}
	if got := SuccessRate(0, 5); got != 0 {
		t.Fatalf("全失败应回 0，得到 %v", got)
	}
}

func TestRangeNormalize_缺省补齐与倒置交换(t *testing.T) {
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)

	// 全空 → 最近 7 天。
	r := Range{}.Normalize(now)
	if !r.To.Equal(now) || r.From != now.AddDate(0, 0, -7) {
		t.Fatalf("默认应是最近 7 天：%+v", r)
	}

	// ⚠️ 倒置**交换而不是报错**：这是一次输入笔误，为它把整个看板挡掉不值。
	a := now.AddDate(0, 0, -3)
	r = Range{From: now, To: a}.Normalize(now)
	if !r.From.Equal(a) || !r.To.Equal(now) {
		t.Fatalf("倒置应被交换：%+v", r)
	}

	// ⚠️ 跨度上限：不设的话一次 from=1970 会全表扫，而那条请求看起来
	// 完全正常。
	r = Range{From: time.Unix(0, 0), To: now}.Normalize(now)
	if r.To.Sub(r.From) > 370*24*time.Hour {
		t.Fatalf("跨度应被夹到 370 天内：%v", r.To.Sub(r.From))
	}
	// 370 而不是 365 —— 要让"看整整一年"成立。
	oneYear := Range{From: now.AddDate(-1, 0, 0), To: now}
	if got := oneYear.Normalize(now); !got.From.Equal(oneYear.From) {
		t.Fatalf("整一年不该被夹：%+v", got)
	}
}

// 指标名就是对外契约，改名等于改契约 —— 这条把名字钉住。
func TestMetricNames_不得随意改名(t *testing.T) {
	want := map[Metric]string{
		MetricActiveUsers:         "active_users",
		MetricCreditsConsumed:     "credits_consumed",
		MetricCreditsRefunded:     "credits_refunded",
		MetricGenerationCancelled: "generation_cancelled",
		MetricRevenueCents:        "revenue_cents",
		MetricProviderCalls:       "provider_calls",
	}
	for m, s := range want {
		if string(m) != s {
			t.Fatalf("指标名变了：%s != %s", m, s)
		}
	}
}
