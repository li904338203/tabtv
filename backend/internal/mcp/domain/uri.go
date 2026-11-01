package domain

import "strings"

// MatchURITemplate 按资源 URI 模板解析 URI（MCPT-16）。只支持 RFC 6570 最简的 `{var}` 占位：
// 每个变量匹配一段**非空、不含 `/`** 的串；模板里的其余字符逐字比较。不匹配返回 false。
//
// ⚠️ 不做百分号解码、不认 `{+var}` 之类的扩展：资源里的变量都是 ID（雪花号字符串），
// 放宽的话 `seeaitv://media/1/../2` 这种串会被当成一个变量值传给业务。
func MatchURITemplate(tmpl, uri string) (map[string]string, bool) {
	vars := map[string]string{}
	for tmpl != "" {
		i := strings.IndexByte(tmpl, '{')
		if i < 0 {
			return vars, tmpl == uri
		}
		if !strings.HasPrefix(uri, tmpl[:i]) {
			return nil, false
		}
		uri, tmpl = uri[i:], tmpl[i:]
		j := strings.IndexByte(tmpl, '}')
		if j < 0 {
			return nil, false
		}
		name := tmpl[1:j]
		tmpl = tmpl[j+1:]
		end := strings.IndexByte(uri, '/')
		if end < 0 {
			end = len(uri)
		}
		if end == 0 || name == "" {
			return nil, false
		}
		vars[name], uri = uri[:end], uri[end:]
	}
	return vars, uri == ""
}
