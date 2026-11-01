// Package id 生成雪花 ID。
//
// 内部 int64，**对外一律序列化为 string**（GO-34）：
// JS 的 Number 只有 53 位安全整数，bigint 直接传会静默丢精度。
package id

import (
	"fmt"
	"sync"
	"time"
)

// 2026-01-01T00:00:00Z。自定义纪元把可用年限往后推。
const epochMS int64 = 1767225600000

const (
	nodeBits = 10
	seqBits  = 12
	maxNode  = -1 ^ (-1 << nodeBits)
	maxSeq   = -1 ^ (-1 << seqBits)
)

type Generator struct {
	mu     sync.Mutex
	nodeID int64
	lastMS int64
	seq    int64
}

func New(nodeID int64) (*Generator, error) {
	if nodeID < 0 || nodeID > maxNode {
		return nil, fmt.Errorf("节点号 %d 超出范围 [0,%d]", nodeID, maxNode)
	}
	return &Generator{nodeID: nodeID}, nil
}

// Next 返回下一个 ID。同毫秒内序号用尽则自旋到下一毫秒。
func (g *Generator) Next() int64 {
	g.mu.Lock()
	defer g.mu.Unlock()

	now := time.Now().UnixMilli()
	// 时钟回拨：不报错也不生成可能重复的 ID，而是等回拨追平。
	// 回拨通常只有几毫秒（NTP 校正），等待比抛错更实用。
	for now < g.lastMS {
		time.Sleep(time.Duration(g.lastMS-now) * time.Millisecond)
		now = time.Now().UnixMilli()
	}

	if now == g.lastMS {
		g.seq = (g.seq + 1) & maxSeq
		if g.seq == 0 {
			for now <= g.lastMS {
				now = time.Now().UnixMilli()
			}
		}
	} else {
		g.seq = 0
	}
	g.lastMS = now

	return ((now - epochMS) << (nodeBits + seqBits)) | (g.nodeID << seqBits) | g.seq
}
