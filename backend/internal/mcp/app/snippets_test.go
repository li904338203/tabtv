package app

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/li904338203/tabtv/backend/internal/mcp/domain"
)

func TestSnippets_使用公网地址且不含密钥(t *testing.T) {
	const url = "https://mcp.example.com/mcp"
	res := ClientSnippets(url, nil)
	if !res.URLConfigured || res.MCPURL != url || res.KeyPlaceholder != KeyPlaceholder {
		t.Fatalf("结果头部不对：%+v", res)
	}
	for _, it := range res.Items {
		if !strings.Contains(it.Content, url) {
			t.Errorf("%s 片段里没有公网地址：\n%s", it.Client, it.Content)
		}
		if !strings.Contains(it.Content, KeyPlaceholder) {
			t.Errorf("%s 片段里没有密钥占位符（前端无处替换）：\n%s", it.Client, it.Content)
		}
		// ⚠️ 不能出现任何形似真实密钥的东西，也不能出现被 HTML 转义掉的占位符。
		if strings.Contains(it.Content, "sk_mcp_") || strings.Contains(it.Content, `\`+"u003c") {
			t.Errorf("%s 片段含密钥形态或被转义的占位符：\n%s", it.Client, it.Content)
		}
		if strings.Contains(it.Content, URLPlaceholder) {
			t.Errorf("%s 已配置地址却仍有地址占位符", it.Client)
		}
		if it.Format == SnippetJSON && !json.Valid([]byte(it.Content)) {
			t.Errorf("%s 的 JSON 片段不合法：\n%s", it.Client, it.Content)
		}
	}
}

func TestSnippets_未配置地址时用占位符(t *testing.T) {
	res := ClientSnippets("  ", nil)
	if res.URLConfigured || res.MCPURL != URLPlaceholder {
		t.Fatalf("未配置地址应如实标出：%+v", res)
	}
	for _, it := range res.Items {
		if !strings.Contains(it.Content, URLPlaceholder) {
			t.Errorf("%s 片段里没有地址占位符", it.Client)
		}
	}
}

func TestSnippets_覆盖全部目标客户端(t *testing.T) {
	res := ClientSnippets("https://x.example/mcp", nil)
	var got []string
	for _, it := range res.Items {
		got = append(got, it.Client)
		if it.Label == "" || it.Location == "" || it.Note == "" {
			t.Errorf("%s 缺 label / location / note", it.Client)
		}
		if !slices.Contains([]string{SnippetJSON, SnippetTOML, SnippetShell}, it.Format) {
			t.Errorf("%s 格式非法：%s", it.Client, it.Format)
		}
	}
	if !slices.Equal(got, SnippetClients()) {
		t.Fatalf("客户端清单 = %v，期望 %v", got, SnippetClients())
	}
	// 设计 03 §12 的目标客户端 + stdio 桥接一个都不能少。
	for _, want := range []string{"claude_code", "codex", "cursor", "opencode", "workbuddy", "gemini_cli", "stdio_bridge"} {
		if !slices.Contains(got, want) {
			t.Errorf("缺少客户端 %s", want)
		}
	}
}

func TestToolsetAvailability_按注册表实际有的工具集(t *testing.T) {
	tools := []*Tool{{Name: "a", Toolset: domain.ToolsetCanvas}, {Name: "b", Toolset: domain.ToolsetContext}}
	got := ToolsetAvailability(tools)
	if len(got) != len(domain.Toolsets()) {
		t.Fatalf("应列出全部工具集，得到 %d", len(got))
	}
	for _, ts := range got {
		wantAvail := ts.Name == domain.ToolsetCanvas || ts.Name == domain.ToolsetContext
		if ts.Available != wantAvail {
			t.Errorf("%s available=%v", ts.Name, ts.Available)
		}
		if ts.DefaultOn != slices.Contains(domain.DefaultToolsets(), ts.Name) {
			t.Errorf("%s defaultOn=%v", ts.Name, ts.DefaultOn)
		}
	}
}

// MCPT-11B：stdio 桥接片段用自研 mcp-bridge，密钥只在 env、绝不进 args（命令行在 ps 里可见），也不再是浮动的 @latest。
func TestSnippets_stdio桥接密钥只走环境变量(t *testing.T) {
	const url = "https://mcp.example.com/mcp"
	var it *ClientSnippet
	res := ClientSnippets(url, nil)
	for i := range res.Items {
		if res.Items[i].Client == "stdio_bridge" {
			it = &res.Items[i]
		}
	}
	if it == nil {
		t.Fatal("缺少 stdio_bridge 片段")
	}
	var doc struct {
		MCPServers map[string]struct {
			Command string            `json:"command"`
			Args    []string          `json:"args"`
			Env     map[string]string `json:"env"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(it.Content), &doc); err != nil {
		t.Fatalf("片段不是合法 JSON：%v\n%s", err, it.Content)
	}
	s, ok := doc.MCPServers[serverName]
	if !ok {
		t.Fatalf("片段里没有 %s：%s", serverName, it.Content)
	}
	if s.Command != "mcp-bridge" || !slices.Equal(s.Args, []string{"--url", url}) {
		t.Errorf("command / args 不对：%q %q", s.Command, s.Args)
	}
	if s.Env["SEEAITV_MCP_KEY"] != KeyPlaceholder || len(s.Env) != 1 {
		t.Errorf("密钥应只在 env.SEEAITV_MCP_KEY：%v", s.Env)
	}
	for _, a := range s.Args {
		if strings.Contains(a, KeyPlaceholder) || strings.Contains(strings.ToLower(a), "bearer") {
			t.Errorf("args 里出现了密钥位置：%q", a)
		}
	}
	for _, bad := range []string{"mcp-remote", "@latest", "npx"} {
		if strings.Contains(it.Content, bad) || strings.Contains(it.Note, bad) {
			t.Errorf("片段 / 说明里不该再出现 %q", bad)
		}
	}
}

// MCPT-15：片段与兼容矩阵（docs/records/MCP-客户端兼容矩阵.md，2026-10-18）里**实际用过**的配置逐客户端一致 ——
// 键名、传输类型、密钥放哪里。改片段时若与实测形状不同，这条会红：先去矩阵里补实测，再改这里。
// Note 里不再出现「待实测」：测过的写观察到的行为，没测的写「未实测」。
func TestSnippets_与兼容矩阵实测的配置一致(t *testing.T) {
	const u = "https://mcp.example.com/mcp"
	res := ClientSnippets(u, nil)
	by := map[string]ClientSnippet{}
	for _, it := range res.Items {
		by[it.Client] = it
		if strings.Contains(it.Note, "待 MCPT-15") || strings.Contains(it.Note, "待实测") {
			t.Errorf("%s 的说明还写着待实测：%s", it.Client, it.Note)
		}
	}
	auth := "Bearer " + KeyPlaceholder
	dig := func(client string, path ...string) any {
		var v any
		if err := json.Unmarshal([]byte(by[client].Content), &v); err != nil {
			t.Fatalf("%s 片段不是 JSON：%v", client, err)
		}
		for _, k := range path {
			m, _ := v.(map[string]any)
			v = m[k]
		}
		return v
	}
	cases := []struct {
		client string
		path   []string
		want   any
	}{
		// Cursor（cursor-agent 2026.10.01）：项目内 .cursor/mcp.json，url + headers。
		{"cursor", []string{"mcpServers", "seeaitv", "url"}, u},
		{"cursor", []string{"mcpServers", "seeaitv", "headers", "Authorization"}, auth},
		// OpenCode 1.18.35：type remote + url + enabled + headers。
		{"opencode", []string{"mcp", "seeaitv", "type"}, "remote"},
		{"opencode", []string{"mcp", "seeaitv", "url"}, u},
		{"opencode", []string{"mcp", "seeaitv", "enabled"}, true},
		{"opencode", []string{"mcp", "seeaitv", "headers", "Authorization"}, auth},
		// Gemini CLI 0.63.0：httpUrl（Streamable HTTP）+ headers。
		{"gemini_cli", []string{"mcpServers", "seeaitv", "httpUrl"}, u},
		{"gemini_cli", []string{"mcpServers", "seeaitv", "headers", "Authorization"}, auth},
		// stdio 桥接（Claude Code 2.1.283 拉起）：command + --url，密钥只在 env。
		{"stdio_bridge", []string{"mcpServers", "seeaitv", "command"}, "mcp-bridge"},
		{"stdio_bridge", []string{"mcpServers", "seeaitv", "env", "SEEAITV_MCP_KEY"}, KeyPlaceholder},
	}
	for _, c := range cases {
		if got := dig(c.client, c.path...); got != c.want {
			t.Errorf("%s %v = %v，实测用的是 %v", c.client, c.path, got, c.want)
		}
	}
	// Codex CLI 0.162.0：-c mcp_servers.<名>.url / bearer_token_env_var 实测可用，片段是同一组键。
	for _, s := range []string{"[mcp_servers.seeaitv]", `url = "` + u + `"`, `bearer_token_env_var = "SEEAITV_MCP_KEY"`} {
		if !strings.Contains(by["codex"].Content, s) {
			t.Errorf("codex 片段缺 %q：%s", s, by["codex"].Content)
		}
	}
	// Claude Code 2.1.283：实测的是等价的 JSON（type http + headers.Authorization）。
	for _, s := range []string{"--transport http", "Authorization: " + auth, u} {
		if !strings.Contains(by["claude_code"].Content, s) {
			t.Errorf("claude_code 片段缺 %q：%s", s, by["claude_code"].Content)
		}
	}
	if !strings.Contains(by["workbuddy"].Note, "未实测") {
		t.Errorf("WorkBuddy 没测过，说明里要写明：%s", by["workbuddy"].Note)
	}
}

// MCPT-11F 改版（`/mcp` 接入指南）：每个客户端给两种接入方式，各带一段可直接发给 Agent 的话术。
func TestSnippets_两种接入方式与话术(t *testing.T) {
	const u = "https://mcp.example.com/mcp"
	res := ClientSnippets(u, nil)
	for _, it := range res.Items {
		modes := map[string]ClientMode{}
		var order []string
		for _, m := range it.Modes {
			modes[m.Mode] = m
			order = append(order, m.Mode)
		}
		key, ok := modes[ModeKey]
		if !ok || !key.Supported {
			t.Fatalf("%s 缺少可用的密钥方式：%+v", it.Client, it.Modes)
		}
		// 密钥方式就是已实测的那份片段，顶层字段与它逐字相同（已有调用方继续读顶层）。
		if key.Content != it.Content || key.Format != it.Format || key.Location != it.Location || key.Note != it.Note {
			t.Errorf("%s 的 key 方式与顶层片段不一致", it.Client)
		}
		if !strings.Contains(key.AgentPrompt, KeyPlaceholder) {
			t.Errorf("%s 的密钥话术里没有占位符（用户无处替换）：\n%s", it.Client, key.AgentPrompt)
		}
		if _, has := modes[ModeOAuth]; has && order[0] != ModeOAuth {
			t.Errorf("%s 的方式顺序应为 oauth 在前：%v", it.Client, order)
		}
		for _, m := range it.Modes {
			for _, bad := range []string{"sk_mcp_", `\` + "u003c"} {
				if strings.Contains(m.AgentPrompt, bad) || strings.Contains(m.Content, bad) {
					t.Errorf("%s/%s 含 %q", it.Client, m.Mode, bad)
				}
			}
			if !m.Supported {
				if m.Content != "" || m.AgentPrompt != "" || m.Note == "" {
					t.Errorf("%s/%s 不支持时应只有说明：%+v", it.Client, m.Mode, m)
				}
				continue
			}
			if !strings.HasPrefix(m.AgentPrompt, promptIntro) || !strings.Contains(m.AgentPrompt, "whoami") ||
				!strings.HasSuffix(m.AgentPrompt, promptOutro) {
				t.Errorf("%s/%s 话术缺开头 / whoami 验证 / 先报价的收尾：\n%s", it.Client, m.Mode, m.AgentPrompt)
			}
			if !strings.Contains(m.AgentPrompt, u) {
				t.Errorf("%s/%s 话术里没有地址", it.Client, m.Mode)
			}
			if m.Format == SnippetJSON && !json.Valid([]byte(m.Content)) {
				t.Errorf("%s/%s 的 JSON 不合法：\n%s", it.Client, m.Mode, m.Content)
			}
		}
		if oauth, has := modes[ModeOAuth]; has {
			// ⚠️ 一键接入的命令 / 配置 / 话术里绝不能出现密钥位置。
			for _, bad := range []string{KeyPlaceholder, "Authorization", "Bearer", "SEEAITV_MCP_KEY"} {
				if strings.Contains(oauth.Content, bad) || strings.Contains(oauth.AgentPrompt, bad) {
					t.Errorf("%s 的 oauth 方式出现了 %q", it.Client, bad)
				}
			}
			// 客户端侧 OAuth 一次都没实测过，说明里必须写明，不冒充已验证。
			if !strings.Contains(oauth.Note, "待实测") {
				t.Errorf("%s 的 oauth 说明没写待实测：%s", it.Client, oauth.Note)
			}
		}
	}
}

func TestSnippets_各客户端的一键接入写法(t *testing.T) {
	const u = "https://mcp.example.com/mcp"
	res := ClientSnippets(u, nil)
	oauth := map[string]*ClientMode{}
	for _, it := range res.Items {
		for i := range it.Modes {
			if it.Modes[i].Mode == ModeOAuth {
				oauth[it.Client] = &it.Modes[i]
			}
		}
	}
	if oauth["stdio_bridge"] != nil {
		t.Error("stdio 桥接只走密钥，不应有 oauth 方式")
	}
	if w := oauth["workbuddy"]; w == nil || w.Supported {
		t.Errorf("WorkBuddy 未核实 OAuth，应给出不支持并说明：%+v", w)
	}
	if got := oauth["claude_code"].Content; got != "claude mcp add --transport http --scope user seeaitv '"+u+"'" {
		t.Errorf("claude_code oauth 命令 = %q", got)
	}
	if !strings.Contains(oauth["codex"].Content, "codex mcp login seeaitv") {
		t.Errorf("codex oauth 缺 login：%s", oauth["codex"].Content)
	}
	for _, c := range []string{"claude_code", "codex", "cursor", "opencode", "gemini_cli"} {
		if m := oauth[c]; m == nil || !m.Supported {
			t.Errorf("%s 应给出一键接入", c)
		}
	}
	if !strings.Contains(oauth["gemini_cli"].Content, `"httpUrl"`) || !strings.Contains(oauth["opencode"].Content, `"remote"`) {
		t.Error("Gemini 用 httpUrl、OpenCode 用 type remote（与密钥方式实测的键名一致）")
	}
}
