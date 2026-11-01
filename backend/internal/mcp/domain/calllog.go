package domain

import "time"

// 调用审计（MCPT-06，MCP-F-70/71）。

// CallLogRetention 是 mcp_call_log 的保留期：30 天（M-7 已定）。Worker 每小时的日志清理按它删。
const CallLogRetention = 30 * 24 * time.Hour

// CallLogCleanupBatch 是单次清理的行数上限（与请求日志同一个数）：一次删几十万行会长时间持锁，
// 把正在写日志的请求堵住 —— 而那时表现为所有接口都变慢。
const CallLogCleanupBatch = 50000

// 调用结果（与 ck_mcp_call_log_result 逐字一致）。
const (
	CallOK           = "ok"
	CallError        = "error"
	CallNeedsConfirm = "needs_confirm"
	// CallDenied：凭据有效但这次调用不被允许（scope ∩ 角色不够、工具集未开启、只读端点写入）。
	CallDenied = "denied"
	// CallRateLimited：被两级限流拦下，工具没有执行。
	CallRateLimited = "rate_limited"
)
