// Package verify 实现验证码的发送、消费与限流。
//
// ⚠️ 修 GAP-25：旧实现把「读取 → 比较 → 删除」分成三步，
// 并发下同一个验证码可能被用两次（注册两个账号、或同时改密+登录）。
// 本实现用 Lua 脚本把校验与删除合成**一次原子操作**。
package verify

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

type Purpose string

const (
	PurposeRegister      Purpose = "register"
	PurposeLogin         Purpose = "login"
	PurposeResetPassword Purpose = "reset_password"
)

type Channel string

const (
	ChannelEmail Channel = "email"
	ChannelPhone Channel = "phone"
)

var (
	ErrTooFrequent   = errors.New("发送过于频繁")
	ErrCodeInvalid   = errors.New("验证码错误或已失效")
	ErrTooManyFailed = errors.New("错误次数过多，请重新获取")
	ErrQuotaExceeded = errors.New("今日发送次数已达上限")
)

type Config struct {
	// TTL 是验证码有效期。
	TTL time.Duration
	// ResendInterval 两次发送之间的最小间隔。
	ResendInterval time.Duration
	// MaxAttempts 同一个验证码允许的错误次数。
	// ⚠️ 旧实现没有上限，6 位数字码可被暴力穷举。
	MaxAttempts int
	// MaxSendPerHour 单个目标每小时最多发几条，防短信轰炸与成本失控。
	MaxSendPerHour int
}

func DefaultConfig() Config {
	return Config{
		TTL:            5 * time.Minute,
		ResendInterval: 60 * time.Second,
		MaxAttempts:    5,
		MaxSendPerHour: 10,
	}
}

type Service struct {
	rdb    redis.Scripter
	keys   KeyBuilder
	cfg    Config
	nowFn  func() time.Time
	codeFn func() (string, error)
}

// KeyBuilder 由 platform/cache 提供，保证带命名空间前缀。
type KeyBuilder interface {
	Key(parts ...string) string
}

func New(rdb redis.Scripter, keys KeyBuilder, cfg Config) *Service {
	return &Service{rdb: rdb, keys: keys, cfg: cfg, nowFn: time.Now, codeFn: randomCode}
}

// consumeScript 原子地校验并消费验证码。
//
// 返回：1=成功 0=码不匹配（已累加失败次数） -1=不存在或已过期 -2=失败次数超限（已删除）
//
// 必须是脚本而不是 WATCH/MULTI：Redis 事务在冲突时是**重试**语义，
// 而这里需要的是「谁先到谁用掉」的互斥语义。
const consumeScript = `
local stored = redis.call('GET', KEYS[1])
if not stored then return -1 end
local attempts = tonumber(redis.call('GET', KEYS[2]) or '0')
if attempts >= tonumber(ARGV[2]) then
  redis.call('DEL', KEYS[1], KEYS[2])
  return -2
end
if stored ~= ARGV[1] then
  redis.call('INCR', KEYS[2])
  redis.call('EXPIRE', KEYS[2], ARGV[3])
  return 0
end
redis.call('DEL', KEYS[1], KEYS[2])
return 1
`

// sendScript 原子地检查发送间隔与小时配额，通过则写入验证码。
//
// 返回：1=成功 0=发送过于频繁 -1=超出小时配额
const sendScript = `
local cooldown = tonumber(ARGV[3])
if cooldown > 0 and redis.call('EXISTS', KEYS[3]) == 1 then return 0 end
local quota = tonumber(ARGV[4])
if quota > 0 then
  local hourly = tonumber(redis.call('GET', KEYS[4]) or '0')
  if hourly >= quota then return -1 end
end
redis.call('SET', KEYS[1], ARGV[1], 'EX', ARGV[2])
-- 重发必须清掉旧的失败计数，否则用户"重新获取"后仍被旧计数卡住。
redis.call('DEL', KEYS[2])
-- ⚠️ cooldown 为 0 表示不限间隔。不能无条件 SET ... EX 0 ——
-- Redis 会因非法过期时间直接报错，等于把发送功能整个弄挂。
if cooldown > 0 then
  redis.call('SET', KEYS[3], '1', 'EX', cooldown)
end
if quota > 0 then
  redis.call('INCR', KEYS[4])
  redis.call('EXPIRE', KEYS[4], 3600)
end
return 1
`

// Issue 生成并登记一个验证码，返回明文供发送渠道使用。
// 限流与写码在同一次脚本里完成，避免"检查通过但写入失败"的空窗。
func (s *Service) Issue(ctx context.Context, ch Channel, target string, p Purpose) (string, error) {
	if s.cfg.TTL <= 0 {
		return "", errors.New("验证码 TTL 配置非法（必须大于 0）")
	}
	code, err := s.codeFn()
	if err != nil {
		return "", fmt.Errorf("生成验证码失败: %w", err)
	}
	res, err := s.rdb.Eval(ctx, sendScript,
		[]string{s.codeKey(ch, target, p), s.attemptKey(ch, target, p),
			s.cooldownKey(ch, target, p), s.hourlyKey(ch, target)},
		code,
		strconv.Itoa(int(s.cfg.TTL.Seconds())),
		strconv.Itoa(int(s.cfg.ResendInterval.Seconds())),
		strconv.Itoa(s.cfg.MaxSendPerHour),
	).Result()
	if err != nil {
		return "", fmt.Errorf("登记验证码失败: %w", err)
	}
	switch toInt(res) {
	case 1:
		return code, nil
	case 0:
		return "", ErrTooFrequent
	case -1:
		return "", ErrQuotaExceeded
	default:
		return "", fmt.Errorf("验证码登记返回未知结果: %v", res)
	}
}

// Consume 校验并**一次性消费**验证码。成功后该码立即失效。
func (s *Service) Consume(ctx context.Context, ch Channel, target string, p Purpose, code string) error {
	res, err := s.rdb.Eval(ctx, consumeScript,
		[]string{s.codeKey(ch, target, p), s.attemptKey(ch, target, p)},
		code,
		strconv.Itoa(s.cfg.MaxAttempts),
		strconv.Itoa(int(s.cfg.TTL.Seconds())),
	).Result()
	if err != nil {
		return fmt.Errorf("校验验证码失败: %w", err)
	}
	switch toInt(res) {
	case 1:
		return nil
	case -2:
		return ErrTooManyFailed
	default:
		// 码错与码不存在返回**同一个错误**：
		// 区分二者等于告诉攻击者"这个号码存在且刚发过码"。
		return ErrCodeInvalid
	}
}

func (s *Service) codeKey(ch Channel, target string, p Purpose) string {
	return s.keys.Key("verify", string(ch), string(p), target)
}
func (s *Service) attemptKey(ch Channel, target string, p Purpose) string {
	return s.keys.Key("verify", "attempt", string(ch), string(p), target)
}
func (s *Service) cooldownKey(ch Channel, target string, p Purpose) string {
	return s.keys.Key("verify", "cooldown", string(ch), string(p), target)
}
func (s *Service) hourlyKey(ch Channel, target string) string {
	return s.keys.Key("verify", "hourly", string(ch), target)
}

func randomCode() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(1000000))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%06d", n.Int64()), nil
}

func toInt(v any) int64 {
	switch t := v.(type) {
	case int64:
		return t
	case int:
		return int64(t)
	default:
		return -99
	}
}
