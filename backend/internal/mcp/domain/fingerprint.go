package domain

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"strconv"
	"strings"
)

// Fingerprint 是工具参数的指纹：sha256(工具名 + 规范化后的参数 JSON)，32 字节。
//
// 用途有两处，**必须是同一个实现**：调用审计的 args_fingerprint（MCPT-06），
// 与 quote 绑定的参数指纹（MCPT-05 / 10）。各写一份的话「审计里这次调用是不是用了那张 quote 的参数」
// 就对不上了，而两边各自看起来都对（同 workflow/domain.Fingerprint 的教训）。
//
// 规范化：
//   - 对象键排序（客户端两次发来的同一份参数键序可能不同）；
//   - **顶层去掉 `confirm` 开头的字段**：确认信息（confirmQuoteId…）不改变「要做什么」，
//     带着它的重试与第一次必须同指纹；
//   - 数字统一格式（`1`、`1.0`、`1e0` 是同一个数）。超出 int64 的整数按浮点格式化，可能损失精度 ——
//     对外 ID 一律是字符串（R20 ⑤），数字参数只有数量 / 时长这一类小数值；
//   - `null` / 空参数视同 `{}`。
//
// ⚠️ 参数**不是合法 JSON 时对原始字节取哈希**，不返回空：返回空的话两份不同的坏参数会得到同一个指纹。
func Fingerprint(tool string, args json.RawMessage) []byte {
	h := sha256.New()
	h.Write([]byte(tool))
	h.Write([]byte{0})
	h.Write(canonicalArgs(args))
	return h.Sum(nil)
}

func canonicalArgs(args json.RawMessage) []byte {
	trimmed := bytes.TrimSpace(args)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return []byte("{}")
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil || dec.More() {
		return trimmed
	}
	if m, ok := v.(map[string]any); ok {
		for k := range m {
			if strings.HasPrefix(k, "confirm") {
				delete(m, k)
			}
		}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(normalizeNumbers(v)); err != nil {
		return trimmed
	}
	return bytes.TrimRight(buf.Bytes(), "\n")
}

// normalizeNumbers 把 json.Number 统一成同一种写法；map 的键序由 encoding/json 负责排序。
func normalizeNumbers(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			t[k] = normalizeNumbers(val)
		}
		return t
	case []any:
		for i, val := range t {
			t[i] = normalizeNumbers(val)
		}
		return t
	case json.Number:
		if n, err := t.Int64(); err == nil {
			return json.Number(strconv.FormatInt(n, 10))
		}
		if f, err := t.Float64(); err == nil {
			if f == float64(int64(f)) && f >= -9.2e18 && f <= 9.2e18 {
				return json.Number(strconv.FormatInt(int64(f), 10))
			}
			return json.Number(strconv.FormatFloat(f, 'g', -1, 64))
		}
		return t
	}
	return v
}
