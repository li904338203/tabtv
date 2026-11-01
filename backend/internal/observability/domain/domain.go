// Package domain 是可观测域的纯规则（P13-04；AD-07、GAP-24）。
//
// 两类东西在这里：**什么该记进请求日志**，以及**每个统计指标到底是什么**。
// 后者尤其重要 —— GAP-24 的原话是「不把旧 SQL 名称直接当业务定义」：
// 一个叫 `active_users` 的列如果实际统计的是"今天登录过的人"，那它回答的
// 就不是"有多少人在用"这个问题，而看板上没有任何地方写着这件事。
package domain

import (
	"strings"
	"time"
)

// ---------------------------------------------------------------- 请求日志

// skipPrefixes 是**不记日志**的路径前缀。
//
// ⚠️ 探活与静态契约会把日志表刷满：`/healthz` 按秒级被拉，一天几万条，
// 而它们没有任何排障价值 —— 真正要查的那几条会淹没在里面。
//
// ⚠️ `/ws` 也在这里，但理由不同：**WebSocket 握手之后的帧根本不过
// middleware**，只记一条握手日志会让人以为"这个连接只发了一次请求"。
var skipPrefixes = []string{
	"/healthz", "/readyz", "/livez", "/metrics",
	"/openapi", "/docs", "/swagger",
	"/api/v1/ws", "/ws",
}

// ShouldLog 判定一条请求要不要记。
func ShouldLog(path string) bool {
	for _, p := range skipPrefixes {
		if strings.HasPrefix(path, p) {
			return false
		}
	}
	return true
}

// bodyMethods 是**会带请求体**的方法。
//
// ⚠️ 只缓存请求体，**不缓存响应体**：产出图和音视频直接吐字节，
// 缓存等于在内存里再复制一份 —— 一次几百 MB 的成片下载就能把 API 打爆。
// Provider 那一侧的响应体有单独的日志（体积可控且必须留）。
var bodyMethods = map[string]bool{"POST": true, "PUT": true, "PATCH": true, "DELETE": true}

// ShouldCaptureBody 判定要不要缓存请求体。
func ShouldCaptureBody(method, contentType string) bool {
	if !bodyMethods[strings.ToUpper(method)] {
		return false
	}
	// ⚠️ 上传走 multipart，body 就是文件本身 —— 缓存它等于把每次上传的
	// 文件在内存里复制一遍，而日志里只会留下一段无意义的二进制摘要。
	ct := strings.ToLower(contentType)
	if strings.HasPrefix(ct, "multipart/") || strings.HasPrefix(ct, "application/octet-stream") {
		return false
	}
	return true
}

// MaxCapturedBody 单条请求体的缓存上限（字节）。
//
// ⚠️ 与 `logredact.MaxBodyChars`（入库截断）是两件事：这一条管的是**内存**
// —— 一张内联 base64 参考图就是几 MB，不设上限的话并发几路就把进程撑爆，
// 而那时日志表里其实只存得下 4000 字符。
const MaxCapturedBody = 64 << 10

// Failed 判定一条请求算不算失败。
//
// ⚠️ **不能只看 http_status**：本项目的约定是 HTTP 状态码与业务码一致
// （CD-01），但 `bizCode` 仍单列 —— 有它才能按**错误类型**统计
// （"40402 资源不存在" 与 "40302 无权访问" 都是 404/403，运维要分开看）。
func Failed(httpStatus int) bool { return httpStatus >= 400 }

// ---------------------------------------------------------------- 统计口径

// Metric 是一个统计指标。取值即对外的 key，改名等于改契约。
type Metric string

const (
	// MetricActiveUsers 真实活跃用户数。
	//
	// ⚠️ **不是"登录过的人"**。旧系统按 `last_login_at` 算（GAP-24 的原文），
	// 于是一个把页面挂在后台的浏览器每天都算一个活跃用户，而这个数只会涨。
	// 这里的定义是「当天**产生过消费或产出**的人」—— 它回答的才是
	// "有多少人在用"。
	MetricActiveUsers Metric = "active_users"

	// MetricCreditsConsumed 消耗积分。**按账本算**，不按各业务表的
	// `credits_cost` 相加 —— 那一列是**报价**，任务失败退款之后它不会变，
	// 于是一个失败又退款的任务会被算成花了钱（CD-60 同一条）。
	MetricCreditsConsumed Metric = "credits_consumed"

	// MetricCreditsRefunded 退还积分。
	//
	// ⚠️ **与消耗分开给，不合并成净值**。这条规则在本项目已是**第五次**
	// （整图费用对账、后台订单汇总、退款历史、用量审计、这里）：
	// 把退回的悄悄减掉之后，看的人拿这个数去对账本流水会**对不上**，
	// 而他无从知道差额是怎么来的。净值由看的人自己减 —— 只给净值就
	// 再也还原不回去。
	MetricCreditsRefunded Metric = "credits_refunded"

	// MetricGenerations 生成任务数（按发起时间）。
	MetricGenerations Metric = "generations"
	// MetricGenerationSuccess 成功的生成任务数。
	MetricGenerationSuccess Metric = "generation_success"
	// MetricGenerationFailed 失败的生成任务数。
	MetricGenerationFailed Metric = "generation_failed"
	// MetricGenerationCancelled 用户取消的生成任务数。
	//
	// ⚠️ **单列，不并进失败**：取消是用户行为不是故障。并进去的话，
	// 用户多取消几次会让成功率看起来在下降，**而系统一切正常**。
	MetricGenerationCancelled Metric = "generation_cancelled"

	// MetricRenders 渲染任务数。
	MetricRenders Metric = "renders"
	// MetricRenderSuccess 成功的渲染任务数。
	MetricRenderSuccess Metric = "render_success"

	// MetricPaidOrders 已支付订单数。
	MetricPaidOrders Metric = "paid_orders"
	// MetricRevenueCents 已支付金额（分）。
	//
	// ⚠️ **只算已支付的**：把待支付的也算进收入会让报表凭空多出一截
	// （CD-56 同一条）。退款额**单列**，不从收入里减掉。
	MetricRevenueCents Metric = "revenue_cents"
	// MetricRefundedCents 已退款金额（分）。
	MetricRefundedCents Metric = "refunded_cents"

	// MetricProviderCalls 上游调用次数。
	//
	// ⚠️ **这不是"成本"**。真实成本要各 Provider 的单价，而那是
	// **PARAM-04 未定稿**的内容 —— 编一个数出来比没有更糟：
	// 它会被当成真的，而没有人知道它是编的。看板上这一项的标题必须
	// 写"调用次数"，不能写"成本"。
	MetricProviderCalls Metric = "provider_calls"
	// MetricProviderErrors 上游调用失败次数。
	MetricProviderErrors Metric = "provider_errors"
)

// SuccessRate 算成功率。
//
// ⚠️ 分母是 **success + failed**，**不含 cancelled**（用户取消不是故障）、
// 也不含还在跑的（它们还没有结果，算进去会让当天的成功率一整天都偏低，
// 而那只是因为任务还没跑完）。
//
// ⚠️ 分母为 0 时回 **-1 而不是 0**：0% 与"还没有数据"是两件事，
// 而看板上它们长得一模一样 —— 一个是"全挂了"，一个是"今天没人用"。
// 调用方据此显示"—"。
func SuccessRate(success, failed int64) float64 {
	total := success + failed
	if total <= 0 {
		return -1
	}
	return float64(success) / float64(total)
}

// Range 是统计的时间范围，**左闭右开**。
//
// ⚠️ 两端都闭的话，按天切片时同一条记录会同时落进相邻的两天 ——
// 而"总数不等于各天之和"这种错，看的人要对半天才发现。
type Range struct {
	From time.Time
	To   time.Time
}

// DefaultRange 是没传范围时的默认：最近 7 天（到此刻）。
func DefaultRange(now time.Time) Range {
	return Range{From: now.AddDate(0, 0, -7), To: now}
}

// Normalize 校正一个时间范围。
//
// ⚠️ 上下限倒置时**交换而不是报错**：这是一次输入笔误，而为它把整个
// 看板挡掉不值（同后台订单检索里的时间参数当成没传）。
// ⚠️ 跨度上限 **370 天**：不设的话一次 `from=1970` 会全表扫，
// 而那条请求看起来完全正常。370 而不是 365 —— 要让"看整整一年"成立。
func (r Range) Normalize(now time.Time) Range {
	if r.From.IsZero() && r.To.IsZero() {
		return DefaultRange(now)
	}
	if r.To.IsZero() {
		r.To = now
	}
	if r.From.IsZero() {
		r.From = r.To.AddDate(0, 0, -7)
	}
	if r.From.After(r.To) {
		r.From, r.To = r.To, r.From
	}
	if r.To.Sub(r.From) > 370*24*time.Hour {
		r.From = r.To.AddDate(0, 0, -370)
	}
	return r
}
