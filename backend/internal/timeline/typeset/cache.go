package typeset

import (
	"container/list"
	"sync"
)

// DefaultCacheBytes 是排版结果缓存的上限（dev/01 §4.5：API 进程内 LRU 64 MB）。
const DefaultCacheBytes int64 = 64 << 20

// lru 是按**字节**计量的最近最少使用缓存。
//
// 按条数计量挡不住内存：一行字幕的 PNG 几 KB，一段满屏的标题能到几百 KB，
// 同样「最多 N 条」在两种情况下差两个数量级。
type lru struct {
	mu    sync.Mutex
	limit int64
	size  int64
	ll    *list.List // 前面是最近用过的
	items map[string]*list.Element
}

type lruEntry struct {
	key  string
	res  *Result
	size int64
}

func newLRU(limit int64) *lru {
	if limit <= 0 {
		limit = DefaultCacheBytes
	}
	return &lru{limit: limit, ll: list.New(), items: map[string]*list.Element{}}
}

// entrySize 估一条结果占的内存：PNG 字节为主，其余（行、缺字、键）按实际长度加一点结构开销。
func entrySize(key string, r *Result) int64 {
	n := int64(len(key) + len(r.PNG) + len(r.Key) + 256)
	for _, l := range r.Lines {
		n += int64(len(l) + 16)
	}
	for _, m := range r.Missing {
		n += int64(len(m) + 16)
	}
	return n
}

// get 取一条并把它挪到最前（最近用过）。返回的结果与缓存共用，调用方不得修改。
func (c *lru) get(key string) (*Result, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[key]
	if !ok {
		return nil, false
	}
	c.ll.MoveToFront(el)
	return el.Value.(*lruEntry).res, true
}

// put 放一条；超过上限时从最久没用的开始淘汰。比整个上限还大的一条不缓存。
func (c *lru) put(key string, r *Result) {
	size := entrySize(key, r)
	if size > c.limit {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		e := el.Value.(*lruEntry)
		c.size += size - e.size
		e.res, e.size = r, size
		c.ll.MoveToFront(el)
	} else {
		c.items[key] = c.ll.PushFront(&lruEntry{key: key, res: r, size: size})
		c.size += size
	}
	for c.size > c.limit {
		el := c.ll.Back()
		e := el.Value.(*lruEntry)
		c.ll.Remove(el)
		delete(c.items, e.key)
		c.size -= e.size
	}
}

// stats 返回条数与占用字节（测试与监控用）。
func (c *lru) stats() (n int, bytes int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ll.Len(), c.size
}
