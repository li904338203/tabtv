//go:build integration

// 验证码服务的真库测试（GAP-25）。
// 原子性**必须**用真实 Redis 证明：mock 不会暴露竞态。
package integration

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/li904338203/tabtv/backend/internal/identity/verify"
	"github.com/li904338203/tabtv/backend/internal/platform/testsupport"
)

type keys struct{}

func (keys) Key(parts ...string) string {
	k := "seeaitv"
	for _, p := range parts {
		k += ":" + p
	}
	return k
}

func startRedis(t *testing.T) *redis.Client {
	t.Helper()
	// 每个包共用一个 Redis 容器，用例之间靠 FLUSHALL 隔离。
	// ⚠️ 每个用例各起一个容器时，映射端口在高负载下会撞上别的服务 ——
	// 实测出现过 Redis 客户端连到 HTTP 服务的报错（见 testsupport 包注释）。
	addr := testsupport.RedisAddr(t)
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	if err := rdb.FlushAll(context.Background()).Err(); err != nil {
		t.Fatalf("清空 Redis 失败: %v", err)
	}
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb
}

func newSvc(t *testing.T, mutate func(*verify.Config)) (*verify.Service, *redis.Client) {
	rdb := startRedis(t)
	cfg := verify.DefaultConfig()
	if mutate != nil {
		mutate(&cfg)
	}
	return verify.New(rdb, keys{}, cfg), rdb
}

// ⚠️ 本测试是 GAP-25 的核心证据：
// 20 个 goroutine 拿同一个验证码并发消费，**只能有一个成功**。
// 旧实现「读取→比较→删除」三步分离，这里必然会出现多个成功。
func TestIntegration_并发消费只有一个成功(t *testing.T) {
	svc, _ := newSvc(t, nil)
	ctx := context.Background()

	code, err := svc.Issue(ctx, verify.ChannelPhone, "13800000000", verify.PurposeLogin)
	if err != nil {
		t.Fatal(err)
	}

	const n = 20
	var wg sync.WaitGroup
	results := make([]error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // 尽量让它们同时起跑
			results[i] = svc.Consume(ctx, verify.ChannelPhone, "13800000000", verify.PurposeLogin, code)
		}(i)
	}
	close(start)
	wg.Wait()

	success := 0
	for _, err := range results {
		if err == nil {
			success++
		}
	}
	if success != 1 {
		t.Fatalf("并发消费同一验证码应只有 1 次成功，实际 %d 次 —— 存在竞态", success)
	}
}

// 消费成功后立即失效，不能二次使用。
func TestIntegration_一次性消费(t *testing.T) {
	svc, _ := newSvc(t, nil)
	ctx := context.Background()
	code, _ := svc.Issue(ctx, verify.ChannelEmail, "a@b.com", verify.PurposeRegister)

	if err := svc.Consume(ctx, verify.ChannelEmail, "a@b.com", verify.PurposeRegister, code); err != nil {
		t.Fatalf("首次消费应成功: %v", err)
	}
	if err := svc.Consume(ctx, verify.ChannelEmail, "a@b.com", verify.PurposeRegister, code); err == nil {
		t.Fatal("同一验证码不应被二次使用")
	}
}

// 错误次数必须有上限，否则 6 位数字码可被暴力穷举。
func TestIntegration_错误次数超限即作废(t *testing.T) {
	svc, _ := newSvc(t, func(c *verify.Config) { c.MaxAttempts = 3 })
	ctx := context.Background()
	code, _ := svc.Issue(ctx, verify.ChannelPhone, "13900000000", verify.PurposeLogin)

	for i := 0; i < 3; i++ {
		if err := svc.Consume(ctx, verify.ChannelPhone, "13900000000", verify.PurposeLogin, "000000"); err != verify.ErrCodeInvalid {
			t.Fatalf("第 %d 次错误码应返回 ErrCodeInvalid，实际 %v", i+1, err)
		}
	}
	// 超限后即便拿对的码也不再接受 —— 否则穷举者只要撞对就赢了。
	if err := svc.Consume(ctx, verify.ChannelPhone, "13900000000", verify.PurposeLogin, code); err == nil {
		t.Fatal("错误次数超限后，正确验证码也不应再被接受")
	}
}

// 码错与码不存在必须返回同一个错误：
// 区分二者等于告诉攻击者"这个号码存在且刚发过码"。
func TestIntegration_错误信息不泄漏存在性(t *testing.T) {
	svc, _ := newSvc(t, nil)
	ctx := context.Background()

	errNotExist := svc.Consume(ctx, verify.ChannelPhone, "13700000000", verify.PurposeLogin, "123456")
	_, _ = svc.Issue(ctx, verify.ChannelPhone, "13700000001", verify.PurposeLogin)
	errWrong := svc.Consume(ctx, verify.ChannelPhone, "13700000001", verify.PurposeLogin, "999999")

	if errNotExist != errWrong {
		t.Fatalf("「码不存在」与「码错误」应返回同一错误，实际 %v vs %v", errNotExist, errWrong)
	}
}

// 发送间隔限制：防短信轰炸与成本失控。
func TestIntegration_发送间隔限制(t *testing.T) {
	svc, _ := newSvc(t, func(c *verify.Config) { c.ResendInterval = 2 * time.Second })
	ctx := context.Background()

	if _, err := svc.Issue(ctx, verify.ChannelPhone, "13600000000", verify.PurposeLogin); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Issue(ctx, verify.ChannelPhone, "13600000000", verify.PurposeLogin); err != verify.ErrTooFrequent {
		t.Fatalf("间隔内重发应被拒，实际 %v", err)
	}
	// ⚠️ 不要写成"睡 2.1 秒然后断言一定放行"。
	// 冷却期是 Redis 容器里的 TTL，而 sleep 是宿主机的墙钟：
	// colima 这类 VM 在负载高时容器内的时间会走得比宿主机慢，
	// 于是宿主机睡够了、容器里的 key 还没过期 —— 实测这条断言在
	// 容器启动慢的那几次（22s）稳定失败，快的那几次（2.6s）通过。
	// 改成**轮询到期**：断言的仍是"间隔过后能重发"，但不再要求两边时钟严格一致。
	deadline := time.Now().Add(15 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
		if _, lastErr = svc.Issue(ctx, verify.ChannelPhone, "13600000000", verify.PurposeLogin); lastErr == nil {
			return
		}
		if lastErr != verify.ErrTooFrequent {
			t.Fatalf("只应因冷却被拒，实际 %v", lastErr)
		}
	}
	t.Fatalf("间隔过后应允许重发，等待 15s 仍被拒: %v", lastErr)
}

// 小时配额：即便每次都等够间隔，也不能无限发。
func TestIntegration_小时配额(t *testing.T) {
	svc, _ := newSvc(t, func(c *verify.Config) {
		c.ResendInterval = 0
		c.MaxSendPerHour = 3
	})
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, err := svc.Issue(ctx, verify.ChannelPhone, "13500000000", verify.PurposeLogin); err != nil {
			t.Fatalf("第 %d 次发送应成功: %v", i+1, err)
		}
	}
	if _, err := svc.Issue(ctx, verify.ChannelPhone, "13500000000", verify.PurposeLogin); err != verify.ErrQuotaExceeded {
		t.Fatalf("超出小时配额应返回 ErrQuotaExceeded，实际 %v", err)
	}
}

// 重新发送必须清掉旧的失败计数，否则用户"重新获取"后仍被旧计数卡住。
func TestIntegration_重发重置失败计数(t *testing.T) {
	svc, _ := newSvc(t, func(c *verify.Config) {
		c.MaxAttempts = 2
		c.ResendInterval = 0
	})
	ctx := context.Background()
	_, _ = svc.Issue(ctx, verify.ChannelPhone, "13400000000", verify.PurposeLogin)
	_ = svc.Consume(ctx, verify.ChannelPhone, "13400000000", verify.PurposeLogin, "000000")
	_ = svc.Consume(ctx, verify.ChannelPhone, "13400000000", verify.PurposeLogin, "000000")

	fresh, err := svc.Issue(ctx, verify.ChannelPhone, "13400000000", verify.PurposeLogin)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Consume(ctx, verify.ChannelPhone, "13400000000", verify.PurposeLogin, fresh); err != nil {
		t.Fatalf("重发后新码应可用: %v", err)
	}
}

// ResendInterval 配成 0 表示不限间隔，不能因此把发送功能弄挂
// （Redis 的 SET ... EX 0 是非法参数）。
func TestIntegration_零间隔配置可用(t *testing.T) {
	svc, _ := newSvc(t, func(c *verify.Config) { c.ResendInterval = 0; c.MaxSendPerHour = 0 })
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, err := svc.Issue(ctx, verify.ChannelEmail, "z@b.com", verify.PurposeLogin); err != nil {
			t.Fatalf("零间隔零配额下第 %d 次发送应成功: %v", i+1, err)
		}
	}
}

// 不同用途的验证码互不通用：登录码不能拿去改密码。
func TestIntegration_用途隔离(t *testing.T) {
	svc, _ := newSvc(t, nil)
	ctx := context.Background()
	loginCode, _ := svc.Issue(ctx, verify.ChannelPhone, "13300000000", verify.PurposeLogin)

	if err := svc.Consume(ctx, verify.ChannelPhone, "13300000000", verify.PurposeResetPassword, loginCode); err == nil {
		t.Fatal("登录验证码不应能用于重置密码")
	}
}
