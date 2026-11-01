// Package lenientjson 是模型输出的容错 JSON 解析（容错层，不是许可证）。
//
// ⚠️ 它从 `script/domain` 下沉到 platform，是因为第二个调用方出现了：
// 3D 导演台的 AI 搭景（`director/domain.ParseDraft`，docs/agent/director DRD-25）。
// 让导演台去依赖脚本域是反的，而各抄一份的话，两份早晚会不一致**且没有编译错误** ——
// 不一致的具体形态是「同一种脏输出，分镜救得回来、搭景救不回来」（或者反过来），
// 而两边看起来都在工作。同 `platform/pgtext`、`platform/logredact` 的理由。
//
// ⚠️ 这个包**只依赖标准库**，是个工具叶子包：别往里放任何业务规则
// （"0 个镜头算失败""白名单之外的码丢弃"这类判断留在各自的领域包里）。
package lenientjson

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Decode 从模型输出里把 JSON 抠出来并尽力修好，解进 out（同 json.Unmarshal 的 out）。
//
// ⚠️ 这是**容错层，不是许可证**：模型该被要求输出合法 JSON，
// 这里只处理已知的几类脏输出。见过的形态：
//
//   - 外面裹着 ```json ... ``` 代码块；
//   - 正文前后有一段"好的，这是分镜："之类的说明；
//   - 尾随逗号 `[1,2,]`；
//   - **字符串里出现裸引号** —— 输出越长越容易出现。老系统实测
//     `claude_opus_4_8` 因为输出过长在字符串里写出裸引号，
//     16228 字符的整批响应作废、用户白扣了 60 积分。
//
// 最后一条是**样板段落必须由代码拼、不让模型写**的直接原因：
// 让模型每次重写一千多字的固定版式，既会漂移也会把输出撑长。
func Decode(raw string, out any) error {
	candidates := []string{}
	if s := stripFence(raw); s != "" {
		candidates = append(candidates, s)
	}
	if s := sliceOutermost(raw); s != "" {
		candidates = append(candidates, s)
	}
	candidates = append(candidates, strings.TrimSpace(raw))

	var lastErr error
	for _, c := range candidates {
		for _, repaired := range []string{c, dropTrailingCommas(c), escapeInnerQuotes(c)} {
			if repaired == "" {
				continue
			}
			if err := json.Unmarshal([]byte(repaired), out); err == nil {
				return nil
			} else {
				lastErr = err
			}
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("没有可解析的内容")
	}
	// ⚠️ 错误信息要带上**原文片段**：解析失败时用户看到的只有这句话，
	// 不带片段的话运营无从判断是模型跑题了还是我们的提示词写坏了。
	if !strings.ContainsAny(raw, "{[") {
		// 通篇没有一个括号：模型根本没写 JSON（多半是在反问或拒答）。
		// 这时 encoding/json 的错误是 `invalid character 'æ' looking for beginning of value`
		// —— 'æ' 是中文首字节被当成 Latin-1 打出来的，用户看了只会以为是乱码。
		return fmt.Errorf("模型没有按要求输出 JSON，回的是一段文字（原文开头：%s）", head(raw, 120))
	}
	return fmt.Errorf("模型输出无法解析为 JSON: %w（原文开头：%s）", lastErr, head(raw, 120))
}

// stripFence 去掉 ```json 围栏。
func stripFence(s string) string {
	i := strings.Index(s, "```")
	if i < 0 {
		return ""
	}
	rest := s[i+3:]
	// 跳过语言标注那一行。
	if j := strings.IndexByte(rest, '\n'); j >= 0 {
		first := strings.TrimSpace(rest[:j])
		if first == "" || !strings.ContainsAny(first, "{[") {
			rest = rest[j+1:]
		}
	}
	if k := strings.Index(rest, "```"); k >= 0 {
		rest = rest[:k]
	}
	return strings.TrimSpace(rest)
}

// sliceOutermost 取最外层的 {...} 或 [...]。
//
// ⚠️ 按**括号配对**取，不是找第一个 `{` 到最后一个 `}` ——
// 模型常在 JSON 后面又跟一段说明，里面也可能有大括号。
func sliceOutermost(s string) string {
	start := strings.IndexAny(s, "{[")
	if start < 0 {
		return ""
	}
	open := s[start]
	close := byte('}')
	if open == '[' {
		close = ']'
	}
	depth := 0
	inStr := false
	escaped := false
	for i := start; i < len(s); i++ {
		c := s[i]
		switch {
		case escaped:
			escaped = false
		case c == '\\' && inStr:
			escaped = true
		case c == '"':
			inStr = !inStr
		case inStr:
			// 字符串内的括号不计数。
		case c == open:
			depth++
		case c == close:
			depth--
			if depth == 0 {
				return s[start : i+1]
			}
		}
	}
	return ""
}

// dropTrailingCommas 去掉 `,}` `,]` 里的尾随逗号。
func dropTrailingCommas(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	inStr, escaped := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case escaped:
			escaped = false
		case c == '\\' && inStr:
			escaped = true
		case c == '"':
			inStr = !inStr
		}
		if c == ',' && !inStr {
			if j := nextNonSpace(s, i+1); j < len(s) && (s[j] == '}' || s[j] == ']') {
				continue
			}
		}
		b.WriteByte(c)
	}
	return b.String()
}

func nextNonSpace(s string, i int) int {
	for i < len(s) && (s[i] == ' ' || s[i] == '\n' || s[i] == '\t' || s[i] == '\r') {
		i++
	}
	return i
}

// escapeInnerQuotes 把字符串值里的裸引号转义掉。
//
// ⚠️ 只在**值**里动手，且靠「下一个非空白字符是不是 `,` `}` `]` `:`」
// 判断这个引号是不是真的结束符。这是启发式，救不了所有情况 ——
// 救不了时按失败处理并退款，**绝不吞掉**：吞掉的结果是落 0 个镜头
// 却照样扣了钱（老系统的真实 bug）。
func escapeInnerQuotes(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	inStr, escaped := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case escaped:
			escaped = false
			b.WriteByte(c)
			continue
		case c == '\\' && inStr:
			escaped = true
			b.WriteByte(c)
			continue
		case c == '"' && !inStr:
			inStr = true
			b.WriteByte(c)
			continue
		case c == '"' && inStr:
			j := nextNonSpace(s, i+1)
			if j < len(s) && (s[j] == ',' || s[j] == '}' || s[j] == ']' || s[j] == ':') {
				inStr = false
				b.WriteByte(c)
			} else {
				// 字符串还没结束，这个引号是内容的一部分。
				b.WriteString(`\"`)
			}
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

func head(s string, n int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= n {
		return string(r)
	}
	return string(r[:n]) + "…"
}
