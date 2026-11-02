package app

import (
	"context"
	"encoding/json"
	"strconv"
	"sync"
	"time"
)

// ConfigCache 缓存配置快照（进程内，TT-T-06）。
//
// 键含 revision：写配置即 bump revision，下次读自然落到新键（CD-14），不需要主动失效。
// 存 JSON 字节而不是对象本身：调用方拿到的是独立副本，改了不会污染缓存。
type ConfigCache struct {
	ttl time.Duration
	mu  sync.Mutex
	m   map[string]cacheEntry
}

type cacheEntry struct {
	raw     []byte
	expires time.Time
}

// NewConfigCache 构造缓存。
func NewConfigCache(ttl time.Duration) *ConfigCache {
	return &ConfigCache{ttl: ttl, m: map[string]cacheEntry{}}
}

func (c *ConfigCache) cacheKey(scope string, revision int64) string {
	return scope + ":" + strconv.FormatInt(revision, 10)
}

// Get 读缓存；没有或已过期返回 false。
func (c *ConfigCache) Get(_ context.Context, scope string, revision int64, dst any) bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	e, ok := c.m[c.cacheKey(scope, revision)]
	c.mu.Unlock()
	if !ok || time.Now().After(e.expires) {
		return false
	}
	return json.Unmarshal(e.raw, dst) == nil
}

// Set 写缓存。同时清掉同一 scope 的旧 revision（它们再也不会被读到）。
func (c *ConfigCache) Set(_ context.Context, scope string, revision int64, v any) {
	if c == nil {
		return
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return
	}
	key := c.cacheKey(scope, revision)
	c.mu.Lock()
	defer c.mu.Unlock()
	for k := range c.m {
		if len(k) > len(scope) && k[:len(scope)+1] == scope+":" && k != key {
			delete(c.m, k)
		}
	}
	c.m[key] = cacheEntry{raw: raw, expires: time.Now().Add(c.ttl)}
}
