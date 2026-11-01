package app

import "testing"

// 上游回的相对地址按 Provider 的地址补全（RFC 3986），绝对地址原样交给出网策略。
//
// ⚠️ 补全**不放宽任何约束**：`file:` / `data:` 这类绝对地址原样返回，随后照旧被出网策略拒绝；
// `//host/x` 补成另一个源，于是**不会**带上 Provider 的 key（同源才带，GO-31）。
func TestResolveOutputURL(t *testing.T) {
	cases := []struct {
		name, base, raw, want string
	}{
		{"站点根相对（relay.example.com 的 Grok 视频）", "https://relay.example.com/",
			"/v1/videos/3a6c/content", "https://relay.example.com/v1/videos/3a6c/content"},
		{"base 带路径前缀时，站点根相对仍从根算", "https://relay.example/v1/",
			"/v1/videos/x/content", "https://relay.example/v1/videos/x/content"},
		{"目录相对：接在 base 的目录后面", "https://relay.example/v1/",
			"videos/x/content", "https://relay.example/v1/videos/x/content"},
		{"协议相对：沿用 base 的协议、换成另一个主机", "https://relay.example.com/",
			"//cdn.example.com/a.mp4", "https://cdn.example.com/a.mp4"},
		{"绝对地址原样", "https://relay.example.com/",
			"https://cdn.example.com/a.mp4?sig=1", "https://cdn.example.com/a.mp4?sig=1"},
		{"file: 原样（随后被出网策略拒绝）", "https://relay.example.com/",
			"file:///etc/passwd", "file:///etc/passwd"},
		{"data: 原样（随后被出网策略拒绝）", "https://relay.example.com/",
			"data:video/mp4;base64,AAAA", "data:video/mp4;base64,AAAA"},
		{"base 不是绝对地址：不猜，原样", "",
			"/v1/videos/x/content", "/v1/videos/x/content"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := resolveOutputURL(c.base, c.raw); got != c.want {
				t.Fatalf("resolveOutputURL(%q, %q) = %q，应为 %q", c.base, c.raw, got, c.want)
			}
		})
	}
}
