// Package pgtext 把任意字节变成**能存进 PostgreSQL 文本/jsonb 列**的文本。
//
// ⚠️ 这不是洁癖，是一次真实故障的修复：上游可以回任意字节
// （TTS 的音频、被网关拦截的二进制错误页），而 PG 的 text 列拒绝 `0x00`
// （`invalid byte sequence for encoding "UTF8"`）、jsonb 拒绝 NUL 的转义形式
// （`unsupported Unicode escape sequence`）。
//
// ⚠️ 连这段注释本身都踩过一次：初稿把那个转义写成了**真的 NUL 字节**，
// 于是 `go build` 报 `unexpected NUL in input` 并指向**导入这个包的文件**
// （`redact.go:10`），看着像 import 写错了。写这类注释时别去"举例"控制字符。
//
// 后者是**会卡死任务的**那一个：产物落库时 raw_result 写不进去 →
// 整条事务回滚 → 执行器判可重试 → 一直重试到撞死线，最后报
// `deadline_exceeded`。而上游其实早就成功了，日志里只有一句看不懂的
// SQLSTATE 22P05 —— 排查时根本想不到是"响应体里有个 0 字节"。
//
// ⚠️ 这个包**只依赖标准库**，是个类型/工具叶子包。它从
// `provider/transport` 下沉到这里，是因为第二个调用方出现了
// （死信落库，P13-03）—— 让运维域去依赖 Provider 适配层是反的，
// 而各抄一份的话两份早晚会不一致**且没有编译错误**。
package pgtext

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// Safe 丢掉 NUL 与其他 C0 控制字符（保留 \t \n \r），
// 非法 UTF-8 序列替换成 U+FFFD。
//
// **不做 base64**：这里的目的是留个能看的摘要给排查用，不是保真存档。
func Safe(s string) string {
	if utf8.ValidString(s) && !strings.ContainsRune(s, 0) && !hasControl(s) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	dropped := 0
	for _, r := range s {
		switch {
		case r == utf8.RuneError:
			// range 对非法字节回 RuneError，写进去即可（已是合法码点）。
			b.WriteRune(r)
		case r == 0 || (r < 0x20 && r != '\t' && r != '\n' && r != '\r') || r == 0x7f:
			dropped++
		default:
			b.WriteRune(r)
		}
	}
	out := b.String()
	if dropped > 0 {
		out += fmt.Sprintf("[已丢弃 %d 个不可存储的控制字符]", dropped)
	}
	return out
}

// Fit 把文本变成能存进 `varchar(maxRunes)` 列的样子：先 Safe，再按**字符**（PG 的 varchar 按字符计）截到上限，
// 截掉的话末尾带「…」（算在上限之内）。
//
// ⚠️ 起因（CD-82）：退款流水的原因写的是上游的报错原文，一段 323 字的 JSON 撞上 `credit_transaction.reason varchar(255)`，
// 整笔退款回滚 —— 而退款失败只留一行日志，6 个失败任务的 288 积分就这样没退回去。
// 存不下的说明文字要截，不能让它把它所属的那件事（这里是退款）一起拖垮。
func Fit(s string, maxRunes int) string {
	s = Safe(s)
	if maxRunes <= 0 || utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	runes := []rune(s)
	if maxRunes == 1 {
		return string(runes[:1])
	}
	return string(runes[:maxRunes-1]) + "…"
}

func hasControl(s string) bool {
	for _, r := range s {
		if r == 0 || (r < 0x20 && r != '\t' && r != '\n' && r != '\r') || r == 0x7f {
			return true
		}
	}
	return false
}
