package app

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// ConfigCache 缓存配置快照。
//
// ⚠️ 缓存键**含 revision**（CD-14）。写配置即 bump revision，
// 下次读自然落到新键上 —— 漏删缓存不再可能产生脏读。
// 旧系统靠手工删缓存，漏一次前端就拿旧配置最长 30 分钟。
type ConfigCache struct {
	rdb redis.Cmdable
	key func(parts ...string) string
	ttl time.Duration
}

func NewConfigCache(rdb redis.Cmdable, key func(...string) string, ttl time.Duration) *ConfigCache {
	if ttl <= 0 {
		// TTL 只是兜底回收旧 revision 的键，不是失效手段。
		ttl = 30 * time.Minute
	}
	return &ConfigCache{rdb: rdb, key: key, ttl: ttl}
}

func (c *ConfigCache) cacheKey(scope string, revision int64) string {
	return c.key("config", scope, fmt.Sprintf("v%d", revision))
}

// Get 读缓存。未命中或任何错误都返回 false ——
// **缓存只做加速，出错一律回落到数据库**（GO-11 同理）。
func (c *ConfigCache) Get(ctx context.Context, scope string, revision int64, dst any) bool {
	if c == nil || c.rdb == nil {
		return false
	}
	raw, err := c.rdb.Get(ctx, c.cacheKey(scope, revision)).Bytes()
	if err != nil {
		return false
	}
	return json.Unmarshal(raw, dst) == nil
}

// Set 写缓存。失败只影响性能，不影响正确性，因此忽略错误。
func (c *ConfigCache) Set(ctx context.Context, scope string, revision int64, v any) {
	if c == nil || c.rdb == nil {
		return
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return
	}
	_ = c.rdb.Set(ctx, c.cacheKey(scope, revision), raw, c.ttl).Err()
}
