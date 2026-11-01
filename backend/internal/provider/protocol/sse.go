package protocol

import "strings"

// feedSSELines 把新到的字节并进累积缓冲，再把其中**完整的行**逐行交给 fn。
//
// ⚠️ 半包：网络按字节切块，一个 `data:` 行可能跨两次读取到达。只处理最后一个
// 换行之前的部分，剩下的留在缓冲里等下一块补齐。行首尾空白（含 `\r\n` 的 `\r`）
// 一律去掉；空行也照样交出去 —— 它是 SSE 的事件分隔符，按事件分派的解析器要靠它
// 清掉上一个 `event:` 名。
//
// fn 返回 true 表示不必再往下读（例如遇到 `[DONE]`），本块剩余的行被丢弃，
// 函数随之返回 true。这与 ParseSSE 一直以来的行为一致。
func feedSSELines(buf *strings.Builder, chunk []byte, fn func(line string) (stop bool)) bool {
	buf.Write(chunk)
	s := buf.String()
	idx := strings.LastIndex(s, "\n")
	if idx < 0 {
		return false
	}
	complete, rest := s[:idx+1], s[idx+1:]
	buf.Reset()
	buf.WriteString(rest)

	lines := strings.Split(complete, "\n")
	// Split 在末尾换行之后会多出一个空串，它不是一条真实的空行。
	lines = lines[:len(lines)-1]
	for _, line := range lines {
		if fn(strings.TrimSpace(line)) {
			return true
		}
	}
	return false
}

// sseField 把一行拆成「字段名 + 值」。注释行（以 `:` 开头）与空行返回 ok=false。
func sseField(line string) (field, value string, ok bool) {
	if line == "" || strings.HasPrefix(line, ":") {
		return "", "", false
	}
	i := strings.Index(line, ":")
	if i < 0 {
		return line, "", true
	}
	return line[:i], strings.TrimSpace(line[i+1:]), true
}

// sseEventReader 在切行之上记住当前的 `event:` 名，按「事件名 + 一行 data」回调。
//
// 每个 `data:` 行单独回调（两家的每个事件都只有一行 data），这与 ParseSSE 的处理粒度
// 一致；事件名在空行处清掉，避免把上一个事件的名字套到下一个只有 data 的事件上。
type sseEventReader struct {
	buf   strings.Builder
	event string
}

func (r *sseEventReader) feed(chunk []byte, fn func(event, data string) (stop bool)) bool {
	return feedSSELines(&r.buf, chunk, func(line string) bool {
		if line == "" {
			r.event = ""
			return false
		}
		field, value, ok := sseField(line)
		if !ok {
			return false
		}
		switch field {
		case "event":
			r.event = value
		case "data":
			return fn(r.event, value)
		}
		return false
	})
}
