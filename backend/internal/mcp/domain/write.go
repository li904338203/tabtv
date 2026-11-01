package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
)

// 写工具的幂等键与批次号（MCPT-11）。
//
// ⚠️ 客户端给的键**一律哈希**后再拼：长度不可控（画布批次列 64 字符、项目创建幂等键列 64 字符），
// 字符集也不可控。前缀带授权 ID：两个客户端（两个授权）用了同一个键不能互相命中 —— 否则 A 的重试
// 会拿回 B 建的项目或 B 写的批次。

// CanvasBatchID 是 canvas_apply 写画布的批次 ID（画布域 uk_canvas_op_batch(project_id, batch_id) 幂等）。
//
//	客户端给了幂等键：mcp:{grant}:k:{sha256(key) 前 16 字节 hex} —— 同键重试只写一次，返回第一次的结果；
//	没给：mcp:{grant}:t:{sha256(fallback) 前 16 字节 hex}，fallback 由调用方给（本次请求的 traceId），
//	每次调用都是新的一批。
//
// 与 generate 的批次（mcp:{grant}:{quoteId}，canvasgen）不会撞：中间多一段 k / t。
// 最长 4 + 19 + 3 + 32 = 58 ≤ 64。
func CanvasBatchID(grantID int64, idemKey, fallback string) string {
	kind, src := "k", idemKey
	if idemKey == "" {
		kind, src = "t", fallback
	}
	return "mcp:" + strconv.FormatInt(grantID, 10) + ":" + kind + ":" + shortHash(src)
}

// ProjectIdemKey 是 projects.create 的项目创建幂等键（project.create_idempotency_key，varchar(64)）。
// 前缀 mcp: 与「直接生成」「Agent 建项目」（agent:）共用唯一索引而互不命中。最长 4 + 19 + 1 + 32 = 56。
func ProjectIdemKey(grantID int64, idemKey string) string {
	return "mcp:" + strconv.FormatInt(grantID, 10) + ":" + shortHash(idemKey)
}

func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:16])
}

// ObjectBatchID 是 script.create 建脚本节点那一批的画布批次（MCPT-16）：mcp:{grant}:o:{sha256 前 12 字节 hex}。
//
// ⚠️ 比 CanvasBatchID 短：「建节点 → 建对象 → 写回」的写回批次是 `{批次}:script` / `{批次}:group`
// （canvasgen.WriteBackBatchID），用 CanvasBatchID 的 58 字符再拼 7 个就超出 canvas_op_log.batch_id 的 64 ——
// 写回失败只告警，症状是节点在画布上一直是空的。最长 4 + 19 + 3 + 24 + 7 = 57。
func ObjectBatchID(grantID int64, idemKey, fallback string) string {
	src := idemKey
	if src == "" {
		src = "t:" + fallback
	}
	sum := sha256.Sum256([]byte(src))
	return "mcp:" + strconv.FormatInt(grantID, 10) + ":o:" + hex.EncodeToString(sum[:12])
}
