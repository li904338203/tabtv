// Package cache 提供 Redis 客户端。
//
// ⚠️ Redis 只做加速：验证码、限流、配置快照缓存、用户运行中计数、事件广播。
// **幂等与唯一性的正确性永远不依赖 Redis**（GO-11）——
// Redis 挂掉只能表现为变慢，绝不能变成重复扣费。
package cache

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/li904338203/tabtv/backend/internal/platform/config"
)

type Client struct {
	*redis.Client
	ns string
}

// Open 建立客户端并立即探活。
func Open(ctx context.Context, cfg config.Redis) (*Client, error) {
	if cfg.Addr == "" {
		return nil, fmt.Errorf("SEEAITV_REDIS_ADDR 未配置")
	}
	rdb := redis.NewClient(&redis.Options{Addr: cfg.Addr, DB: cfg.DB})
	if err := rdb.Ping(ctx).Err(); err != nil {
		_ = rdb.Close()
		return nil, fmt.Errorf("Redis 探活失败: %w", err)
	}
	return &Client{Client: rdb, ns: cfg.Namespace}, nil
}

// Key 给键加上命名空间前缀。
// ⚠️ 一律通过本方法构造键，不要手写字符串拼接：
// 新旧系统若共用一个 Redis 实例，裸键会直接读到对方的数据（INIT-07）。
func (c *Client) Key(parts ...string) string {
	k := c.ns
	for _, p := range parts {
		k += ":" + p
	}
	return k
}

func (c *Client) Name() string { return "redis" }

func (c *Client) Check(ctx context.Context) error { return c.Ping(ctx).Err() }

// PresenceStore 是 collaboration/app.PresenceStore 的 Redis 实现。
//
// ⚠️ 一个资源的全部端放在**同一个 hash** 里（field = clientID），
// 这样"列出谁在线"只要一次 HGETALL。一端一个 key 的话要先 SCAN 再逐个取 ——
// SCAN 在共享实例上是要避开的操作。
//
// 代价是 TTL 只能落在整个 hash 上，单个 field 不会自己过期 ——
// 所以逐条的过期判定放在 domain.MergeOnline 里按 seenAt 算。
// 键上仍然要设 TTL：一个再也没人打开的项目，它的 hash 必须自己消失。
func (c *Client) PresenceTouch(ctx context.Context, key, field, value string,
	ttl time.Duration) error {
	pipe := c.TxPipeline()
	pipe.HSet(ctx, c.Key(key), field, value)
	// ⚠️ 每次心跳都要顺延整个键：只在创建时设 TTL 的话，
	// 一张被连续编辑几小时的画布会在中途整体过期，全体成员同时"消失"。
	pipe.Expire(ctx, c.Key(key), ttl*2)
	_, err := pipe.Exec(ctx)
	return err
}

func (c *Client) PresenceEntries(ctx context.Context, key string) (map[string]string, error) {
	return c.HGetAll(ctx, c.Key(key)).Result()
}

func (c *Client) PresenceDrop(ctx context.Context, key, field string) error {
	return c.HDel(ctx, c.Key(key), field).Err()
}
