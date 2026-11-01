package app

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// 两级限流（MCPT-06，MCP-F-75，开发 01 §9）：「用户 × 客户端」与「工作区」，Redis 固定窗口（每分钟）。
//
// 访问密钥的「客户端」就是它的 grant（一把密钥一个 grant）；OAuth（M3）同样以 grant 为单位。
// 阈值是 MCP-PARAM-01 的**设计建议值**（60 / 300），不是已批准的政策；0 = 不限。
//
// ⚠️ **Redis 出错时放行并告警**，与验证码限流（identity/verify，出错即拒）的取舍相反：
// 验证码限流挡的是短信费与撞库，宁可拒；这里的限流是**保护不是正确性**（GO-11）——
// 为一次 Redis 抖动让所有外部客户端的调用全部失败，代价远大于短时间不限流。

// RateWindow 是固定窗口长度。键按「Unix 分钟」切，窗口边界对所有 API 实例一致。
const RateWindow = time.Minute

// 被哪一级拦下（返回给客户端，让他知道是自己太快还是整个团队太快）。
const (
	RateScopeUserClient = "user_client"
	RateScopeWorkspace  = "workspace"
)

// RateDecision 是一次限流判定。
type RateDecision struct {
	Allowed bool
	// RetryAfter 是到窗口结束还要等多少秒（1~60），只在被拒时有意义。
	RetryAfter int
	// Scope / Limit：被哪一级、按多少的上限拦下。
	Scope string
	Limit int
}

// rateScript 原子地检查两个计数、都没超才各加一。
//
// ⚠️ **先查后加，被拒的那次不计数**：先加后查的话，一个把自己「用户 × 客户端」额度打满还在猛刷的
// 客户端，每一次被拒都还在往**工作区**的计数上加 —— 同工作区的其他人会被他连带限流，而他们一次都没超。
// limit 为 0 的那一级既不查也不加（不限）。首次 INCR 时设过期，键随窗口自行消失。
//
// 返回 {0,0} 放行；{1|2, 当前计数} 被第 1 / 2 级拦下。
const rateScript = `
local lim = {tonumber(ARGV[1]), tonumber(ARGV[2])}
for i = 1, 2 do
  if lim[i] > 0 then
    local n = tonumber(redis.call('GET', KEYS[i]) or '0')
    if n >= lim[i] then return {i, n} end
  end
end
for i = 1, 2 do
  if lim[i] > 0 then
    if redis.call('INCR', KEYS[i]) == 1 then redis.call('EXPIRE', KEYS[i], ARGV[3]) end
  end
end
return {0, 0}
`

// Limiter 是两级限流器。nil 或两级都为 0 时一律放行。
type Limiter struct {
	rdb        redis.Scripter
	key        func(parts ...string) string
	userClient int
	workspace  int
	log        *slog.Logger
	now        func() time.Time

	mu         sync.Mutex
	lastWarn   time.Time
	suppressed int
}

// NewLimiter 构造限流器。key 必须是 cache.Client.Key（带命名空间，INIT-07）。
func NewLimiter(rdb redis.Scripter, key func(parts ...string) string, userClient, workspace int,
	log *slog.Logger) *Limiter {
	if log == nil {
		log = slog.Default()
	}
	return &Limiter{rdb: rdb, key: key, userClient: max(userClient, 0), workspace: max(workspace, 0),
		log: log, now: time.Now}
}

// SetClock 替换时钟（测试用：窗口切换不靠 sleep 卡边界）。
func (l *Limiter) SetClock(now func() time.Time) { l.now = now }

// Allow 判定本次调用能否放行；每放行一次，两级计数各加一。
func (l *Limiter) Allow(ctx context.Context, p Principal) RateDecision {
	if l == nil || l.rdb == nil || (l.userClient == 0 && l.workspace == 0) {
		return RateDecision{Allowed: true}
	}
	now := l.now()
	minute := strconv.FormatInt(now.Unix()/int64(RateWindow/time.Second), 10)
	keys := []string{
		l.key("mcp", "rl", "u", strconv.FormatInt(p.UserID, 10), strconv.FormatInt(p.GrantID, 10), minute),
		l.key("mcp", "rl", "w", strconv.FormatInt(p.WorkspaceID, 10), minute),
	}
	res, err := l.rdb.Eval(ctx, rateScript, keys, l.userClient, l.workspace, int(RateWindow/time.Second)).Int64Slice()
	if err != nil || len(res) != 2 {
		if err == nil {
			err = fmt.Errorf("限流脚本返回 %v", res)
		}
		l.warn(p, err)
		return RateDecision{Allowed: true}
	}
	switch res[0] {
	case 0:
		return RateDecision{Allowed: true}
	case 1:
		return RateDecision{RetryAfter: retryAfter(now), Scope: RateScopeUserClient, Limit: l.userClient}
	default:
		return RateDecision{RetryAfter: retryAfter(now), Scope: RateScopeWorkspace, Limit: l.workspace}
	}
}

// retryAfter 是到当前分钟结束的秒数（1~60）。按窗口边界算而不是键的 TTL：
// 键在窗口内第一次调用时才设过期，TTL 会比真实的窗口剩余时间长。
func retryAfter(now time.Time) int {
	w := int64(RateWindow / time.Second)
	return int(w - now.Unix()%w)
}

// warn 记 Redis 故障：每分钟至多一条（Redis 挂着时每次调用都打一行会把日志淹没），带上被压掉的条数。
func (l *Limiter) warn(p Principal, err error) {
	l.mu.Lock()
	now := l.now()
	if !l.lastWarn.IsZero() && now.Sub(l.lastWarn) < time.Minute {
		l.suppressed++
		l.mu.Unlock()
		return
	}
	n := l.suppressed
	l.lastWarn, l.suppressed = now, 0
	l.mu.Unlock()
	l.log.Warn("MCP 限流不可用，本次放行（限流是保护不是正确性）", "error", err,
		"grantId", p.GrantID, "workspaceId", p.WorkspaceID, "suppressedSinceLastWarn", n)
}

// fixedScript 是单键固定窗口：已有计数 ≥ 上限即拒（不计数），否则 INCR 并在首次设过期。
const fixedScript = `
local n = tonumber(redis.call('GET', KEYS[1]) or '0')
if n >= tonumber(ARGV[1]) then return {1, n} end
if redis.call('INCR', KEYS[1]) == 1 then redis.call('EXPIRE', KEYS[1], ARGV[2]) end
return {0, 0}
`

// AllowFixed 是单键、任意窗口的固定窗口限流（OAuth 动态注册按 IP 限流用，MCPT-12）。
// 与 Allow 同一套取舍：Redis 出错放行并告警；limit <= 0 不限；被拒的那次不计数。
// parts 是键的各段（会经 cache.Client.Key 加命名空间），窗口编号追加在末尾。
func (l *Limiter) AllowFixed(ctx context.Context, parts []string, limit int, window time.Duration) RateDecision {
	if l == nil || l.rdb == nil || limit <= 0 || window < time.Second {
		return RateDecision{Allowed: true}
	}
	now := l.now()
	w := int64(window / time.Second)
	key := l.key(append(append([]string{}, parts...), strconv.FormatInt(now.Unix()/w, 10))...)
	res, err := l.rdb.Eval(ctx, fixedScript, []string{key}, limit, w).Int64Slice()
	if err != nil || len(res) != 2 {
		if err == nil {
			err = fmt.Errorf("限流脚本返回 %v", res)
		}
		l.warn(Principal{}, err)
		return RateDecision{Allowed: true}
	}
	if res[0] == 0 {
		return RateDecision{Allowed: true}
	}
	return RateDecision{RetryAfter: int(w - now.Unix()%w), Scope: "ip", Limit: limit}
}
