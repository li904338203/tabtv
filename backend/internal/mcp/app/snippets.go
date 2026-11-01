package app

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/li904338203/tabtv/backend/internal/mcp/domain"
)

// 客户端配置片段（MCPT-11F，MCP-F-08 的配置片段部分；设计 03 §12）。
//
// ⚠️ 片段放服务端而不是前端：各客户端的配置格式变得快，改片段不该要求发一次前端版本；
// URL 也只有服务端知道（SEEAITV_MCP_PUBLIC_URL）。
//
// ⚠️ **片段里绝不出现任何真实密钥**：密钥位置一律是 KeyPlaceholder。用户刚建完密钥时，
// 由前端在**内存里**把占位符换成明文供复制 —— 服务端这一侧根本拿不到明文（库里只有哈希）。
//
// ⚠️ 格式**以 MCPT-15 兼容矩阵的实测为准**（docs/records/MCP-客户端兼容矩阵.md，2026-10-18）：Note 里只写
// 观察到的行为；没测到的写明「未实测」，不冒充已验证。

// KeyPlaceholder 是片段里密钥的占位符。前端只替换这一个字符串。
const KeyPlaceholder = "<YOUR_MCP_KEY>"

// URLPlaceholder 是平台没有配置 SEEAITV_MCP_PUBLIC_URL 时片段里地址的占位符。
// ⚠️ 不从请求头猜地址：开发代理、反向代理都会改写 Host，猜出来的地址看着像真的、填进客户端却连不上。
const URLPlaceholder = "<SEEAITV_MCP_URL>"

// 片段格式。
const (
	SnippetJSON  = "json"
	SnippetTOML  = "toml"
	SnippetShell = "shell"
)

// serverName 是写进各客户端配置里的服务器名。
const serverName = "seeaitv"

// ClientSnippet 是一个客户端的配置片段。
type ClientSnippet struct {
	Client   string // 稳定的客户端码（前端排序、页签 key 用）
	Label    string
	Format   string // json | toml | shell
	Location string // 放到哪里（文件路径或「在终端执行」）
	Content  string
	Note     string // 已知限制与「待实测」说明（以上四项是密钥模式的片段，与 Modes 里 key 那一项相同，为已有调用方保留）
	// Modes 是这个客户端支持的接入方式（MCPT-11F 改版，`/mcp` 接入指南页）：oauth 一键接入 / key 密钥。
	// ⚠️ 某种方式**根本不适用**的客户端不出现那一项（stdio 桥接没有 oauth）；适用但不支持 / 未核实的给
	// Supported=false 并在 Note 说明原因，页面据此提示改用密钥。
	Modes []ClientMode
}

// 接入方式。
const (
	ModeOAuth = "oauth" // 一键接入：客户端走 OAuth 打开浏览器授权，配置里不带任何密钥
	ModeKey   = "key"   // 访问密钥：配置里是 KeyPlaceholder
)

// ClientMode 是一个客户端在某种接入方式下的片段与可直接发给 Agent 的话术。
type ClientMode struct {
	Mode      string // oauth | key
	Supported bool
	Format    string // json | toml | shell；不支持时为空
	Location  string
	Content   string
	// AgentPrompt 是用户复制后直接粘给自己的 Agent 的中文话术（含安装命令 / 配置）。
	// ⚠️ 与 Content 一样只含占位符，不含任何密钥；不支持时为空。
	AgentPrompt string
	Note        string // 说明与限制；客户端侧没有实测过的写「待实测」
}

// ToolsetInfo 是一个工具集在当前注册表里的可用性。
type ToolsetInfo struct {
	Name      string
	DefaultOn bool
	// Available 表示注册表里**此刻真有**这个工具集的工具。没有工具的工具集授予了也什么都用不了，
	// 页面据此标「后续开放」并不可选（按 api/mcp/tools.json 当时实际有的工具集）。
	Available bool
}

// ClientSnippetsResult 是 GET /me/mcp/client-snippets 的内容。
type ClientSnippetsResult struct {
	MCPURL         string
	URLConfigured  bool
	KeyPlaceholder string
	Toolsets       []ToolsetInfo
	Items          []ClientSnippet
}

// SnippetClients 是片段覆盖的客户端（定义顺序即页签顺序；设计 03 §12 的目标客户端 + stdio 桥接）。
func SnippetClients() []string {
	return []string{"claude_code", "codex", "cursor", "opencode", "workbuddy", "gemini_cli", "stdio_bridge"}
}

// ToolsetAvailability 按注册表里实际有的工具算每个工具集是否可用（定义顺序）。
func ToolsetAvailability(tools []*Tool) []ToolsetInfo {
	has := map[string]bool{}
	for _, t := range tools {
		has[t.Toolset] = true
	}
	defaults := map[string]bool{}
	for _, d := range domain.DefaultToolsets() {
		defaults[d] = true
	}
	out := make([]ToolsetInfo, 0, len(domain.Toolsets()))
	for _, n := range domain.Toolsets() {
		out = append(out, ToolsetInfo{Name: n, DefaultOn: defaults[n], Available: has[n]})
	}
	return out
}

// ClientSnippets 渲染各客户端的配置片段。纯函数：入参是公网地址与工具集可用性，不碰库、不读环境变量。
// publicURL 为空时地址位置用 URLPlaceholder，并把 URLConfigured 置 false（页面据此如实说明）。
func ClientSnippets(publicURL string, toolsets []ToolsetInfo) ClientSnippetsResult {
	u := strings.TrimSpace(publicURL)
	configured := u != ""
	if !configured {
		u = URLPlaceholder
	}
	auth := "Bearer " + KeyPlaceholder
	items := []ClientSnippet{
		{
			Client: "claude_code", Label: "Claude Code", Format: SnippetShell, Location: "在终端执行",
			Content: "claude mcp add --transport http --scope user " + serverName + " " + shQuote(u) + " \\\n" +
				"  --header " + shQuote("Authorization: "+auth),
			Note: "已实测（Claude Code CLI 2.1.283）：与这条命令等价的配置（type http + headers.Authorization）七步全通，走 2026-07-28、支持确认表单。" +
				"--scope user 对所有项目生效；Claude Code 桌面 / IDE 客户端是否读同一份用户配置未实测。命令会留在 shell 历史里，介意的话执行后清掉对应历史。",
		},
		{
			Client: "codex", Label: "Codex", Format: SnippetTOML, Location: "~/.codex/config.toml",
			Content: "# 密钥走环境变量，不写进配置文件。先在 shell 配置里加一行：\n" +
				"#   export SEEAITV_MCP_KEY=" + shQuote(KeyPlaceholder) + "\n" +
				"[mcp_servers." + serverName + "]\n" +
				"url = " + jsonStr(u) + "\n" +
				"bearer_token_env_var = \"SEEAITV_MCP_KEY\"",
			Note: "已实测（Codex CLI 0.162.0）：按此配置连接与列工具成功，无需额外开关；生成等工具调用未实测。" +
				"Codex 桌面客户端是否共用 ~/.codex/config.toml 未实测。",
		},
		{
			Client: "cursor", Label: "Cursor", Format: SnippetJSON, Location: "~/.cursor/mcp.json（或项目内 .cursor/mcp.json）",
			Content: jsonDoc(`{"mcpServers":{"` + serverName + `":{"url":` + jsonStr(u) + `,"headers":{"Authorization":` + jsonStr(auth) + `}}}}`),
			Note: "已实测（cursor-agent 2026.10.01，项目内 .cursor/mcp.json）：七步全通。已有 mcp.json 时只把 seeaitv 这一项合并进 mcpServers。" +
				"命令行 -p 模式要加 --force 才放行工具调用，且确认表单一律按拒绝处理（此时用回显金额确认）。",
		},
		{
			Client: "opencode", Label: "OpenCode", Format: SnippetJSON, Location: "~/.config/opencode/opencode.json（或项目内 opencode.json）",
			Content: jsonDoc(`{"$schema":"https://opencode.ai/config.json","mcp":{"` + serverName + `":{"type":"remote","url":` + jsonStr(u) +
				`,"enabled":true,"headers":{"Authorization":` + jsonStr(auth) + `}}}}`),
			Note: "已实测（OpenCode 1.18.35，项目内 opencode.json）：七步全通，不支持确认表单，走回显金额确认。已有配置时只把 mcp.seeaitv 合并进去。",
		},
		{
			Client: "workbuddy", Label: "WorkBuddy", Format: SnippetJSON, Location: "WorkBuddy 的 MCP 设置（JSON 配置）",
			Content: jsonDoc(`{"mcpServers":{"` + serverName + `":{"url":` + jsonStr(u) + `,"headers":{"Authorization":` + jsonStr(auth) + `}}}}`),
			Note:    "未实测（测试机未安装 WorkBuddy）：按通用 mcpServers 格式给出，配置入口与字段名（是否要求声明传输类型）未核实。",
		},
		{
			Client: "gemini_cli", Label: "Gemini CLI", Format: SnippetJSON, Location: "~/.gemini/settings.json（或项目内 .gemini/settings.json）",
			Content: jsonDoc(`{"mcpServers":{"` + serverName + `":{"httpUrl":` + jsonStr(u) + `,"headers":{"Authorization":` + jsonStr(auth) + `}}}}`),
			Note: "已实测（Gemini CLI 0.63.0，项目内 .gemini/settings.json，目录需被信任）：连接成功；工具调用未实测。" +
				"httpUrl 表示 Streamable HTTP（url 是 SSE）。已有 settings.json 时只合并 mcpServers.seeaitv。",
		},
		{
			// MCPT-11B：自研 mcp-bridge（随 seeAitv 同版本发布的单文件），不用 mcp-remote —— 后者在密钥被撤销 / 过期时
			// 会转去走 OAuth 并打开浏览器（判定表 H3，docs/records/MCP-stdio桥接验证.md）。
			// ⚠️ 密钥只在 env 里（命令行在 ps 里对同机其他用户可见）；mcp-bridge 在命令行里看到形似密钥的东西会拒绝启动。
			Client: "stdio_bridge", Label: "stdio 桥接", Format: SnippetJSON, Location: "只支持本地命令（stdio）的客户端的 mcpServers 配置",
			Content: jsonDoc(`{"mcpServers":{"` + serverName + `":{"command":"mcp-bridge","args":["--url",` + jsonStr(u) +
				`],"env":{"SEEAITV_MCP_KEY":` + jsonStr(KeyPlaceholder) + `}}}}`),
			Note: "mcp-bridge 是 seeAitv 随版本发布的单文件桥接（无需 Node），把本地 stdio 原样转发到远程 /mcp；" +
				"从发布包取与服务端同版本的 bin/mcp-bridge，放进 PATH 或把 command 写成绝对路径。" +
				"密钥只放 env 的 SEEAITV_MCP_KEY，不要写进 args。不支持 OAuth：密钥被撤销或过期时调用直接报错，换新密钥即可。" +
				"已实测（Claude Code 2.1.283 以 stdio 方式拉起）：七步全通，确认表单与进度通知都能穿过桥接。",
		},
	}
	for i := range items {
		items[i].Modes = clientModes(u, items[i])
	}
	return ClientSnippetsResult{
		MCPURL: u, URLConfigured: configured, KeyPlaceholder: KeyPlaceholder,
		Toolsets: toolsets, Items: items,
	}
}

// ---- 两种接入方式与 Agent 话术（MCPT-11F 改版：`/mcp` 接入指南页）
//
// ⚠️ OAuth 一键登录在**客户端侧一次都没有实测过**（兼容矩阵 §3.7：服务端只用 curl 验到「401 → PRM → ASM → DCR →
// 跳转同意页」为止，需人工）。所以 oauth 那一项的命令按各客户端现行文档的写法给出，Note 一律写明「待实测」，
// 不冒充已验证；key 那一项就是上面已实测的片段。

// promptIntro / promptOutro 是所有话术共用的开头与收尾。
const (
	promptIntro = "帮我接入 seeAitv MCP，让我可以在这里创作影像。"
	// ⚠️ 收尾要求先报价再生成：话术是用户发给自己的 Agent 的第一句话，花钱前先确认这条规矩从这里立起来。
	promptVerify = "完成后调用 seeaitv 的 whoami 工具确认连接成功，告诉我当前工作区和积分余额。"
	promptOutro  = "之后生成图片或视频会扣积分：每次先用 quote 报价，等我确认金额后再调用 generate。"
	// 密钥模式的额外约束。⚠️ 不写出占位符字面量：用户整段替换占位符时这句话也会被换成明文，读起来自相矛盾。
	promptKeyGuard = "配置里的访问密钥如果还是尖括号占位符，请先停下来向我要密钥，不要猜；密钥不要提交到代码仓库，也不要贴到别处。"
	// oauth 下多家共用的说明。
	oauthUntested  = "待实测：客户端侧的 OAuth 一键登录尚未实测（服务端已验到跳转 seeAitv 授权页）；授权页若报 invalid_scope 或登录走不通，请改用「密钥 · CLI」方式并反馈。"
	oauthNeedsUser = "一键登录需要你在浏览器里登录 seeAitv、选择工作区与权限并点「同意」；配置里不带任何密钥。"
	oauthBrowser   = "浏览器会打开 seeAitv 授权页，由我登录并点「同意」"
)

// agentPrompt 把步骤拼成一段可直接发给 Agent 的话术。最后一步固定是 whoami 验证。
func agentPrompt(guard string, steps ...string) string {
	var b strings.Builder
	b.WriteString(promptIntro)
	if guard != "" {
		b.WriteString(guard)
	}
	b.WriteString("\n")
	steps = append(steps, promptVerify)
	for i, st := range steps {
		b.WriteString("\n")
		b.WriteString(strconv.Itoa(i+1) + ". " + st)
	}
	b.WriteString("\n\n" + promptOutro)
	return b.String()
}

// codeBlock 把片段包成 Markdown 代码块（Agent 认得出哪里是要原样执行 / 写入的内容）。
func codeBlock(lang, content string) string {
	return "\n```" + lang + "\n" + content + "\n```"
}

// mergeStep 是「打开配置文件并合并」这一步的统一说法。
func mergeStep(file, key, lang, content string) string {
	return "打开 `" + file + "`（不存在就新建），把下面的配置合并进 " + key + "，保留已有的其他服务器：" + codeBlock(lang, content)
}

// clientModes 给出一个客户端的两种接入方式。key 一项复用已实测的片段；oauth 一项见本节开头的说明。
func clientModes(u string, it ClientSnippet) []ClientMode {
	key := ClientMode{
		Mode: ModeKey, Supported: true, Format: it.Format, Location: it.Location, Content: it.Content, Note: it.Note,
	}
	var oauth *ClientMode
	switch it.Client {
	case "claude_code":
		key.AgentPrompt = agentPrompt(promptKeyGuard,
			"在终端执行："+codeBlock("bash", it.Content),
			"执行完提醒我：这条命令带着密钥，会留在 shell 历史里，我可以自行清掉那一条。")
		cmd := "claude mcp add --transport http --scope user " + serverName + " " + shQuote(u)
		oauth = &ClientMode{
			Format: SnippetShell, Location: "在终端执行", Content: cmd,
			AgentPrompt: agentPrompt("",
				"在终端执行："+codeBlock("bash", cmd),
				"告诉我下一步：在 Claude Code 里输入 /mcp，选中 seeaitv 并选择 Authenticate，"+oauthBrowser+"。"),
			Note: oauthNeedsUser + "--scope user 对所有项目生效。" + oauthUntested,
		}
	case "codex":
		key.AgentPrompt = agentPrompt(promptKeyGuard,
			"把访问密钥写成环境变量：在我的 shell 配置（如 ~/.zshrc）里加一行 `export SEEAITV_MCP_KEY="+shQuote(KeyPlaceholder)+"`，并在当前终端执行一次。密钥不要写进 config.toml。",
			"打开 `~/.codex/config.toml`（不存在就新建），追加下面这一段（已有 [mcp_servers.seeaitv] 就替换那一段）："+codeBlock("toml", it.Content))
		cmd := "codex mcp add " + serverName + " --url " + shQuote(u) + "\ncodex mcp login " + serverName
		oauth = &ClientMode{
			Format: SnippetShell, Location: "在终端执行", Content: cmd,
			AgentPrompt: agentPrompt("",
				"在终端执行："+codeBlock("bash", "codex mcp add "+serverName+" --url "+shQuote(u)),
				"再执行 `codex mcp login "+serverName+"`，"+oauthBrowser+"。"),
			Note: oauthNeedsUser + "待实测：codex mcp add --url 与 codex mcp login 未实测（兼容矩阵只测了密钥方式）；" +
				"较早的 Codex 版本可能要先在 config.toml 打开远程 MCP 客户端开关，命令报错时请改用「密钥 · CLI」方式。",
		}
	case "cursor":
		key.AgentPrompt = agentPrompt(promptKeyGuard, mergeStep("~/.cursor/mcp.json", "mcpServers", "json", it.Content))
		conf := jsonDoc(`{"mcpServers":{"` + serverName + `":{"url":` + jsonStr(u) + `}}}`)
		oauth = &ClientMode{
			Format: SnippetJSON, Location: "~/.cursor/mcp.json（或项目内 .cursor/mcp.json）", Content: conf,
			AgentPrompt: agentPrompt("",
				mergeStep("~/.cursor/mcp.json", "mcpServers", "json", conf),
				"告诉我下一步：在 Cursor 的 Settings → MCP 里找到 seeaitv，点 Login（或 Needs login），"+oauthBrowser+"。"),
			Note: oauthNeedsUser + "命令行 cursor-agent 能否完成浏览器登录未核实。" + oauthUntested,
		}
	case "opencode":
		key.AgentPrompt = agentPrompt(promptKeyGuard, mergeStep("~/.config/opencode/opencode.json", "mcp", "json", it.Content))
		conf := jsonDoc(`{"$schema":"https://opencode.ai/config.json","mcp":{"` + serverName + `":{"type":"remote","url":` + jsonStr(u) + `,"enabled":true}}}`)
		oauth = &ClientMode{
			Format: SnippetJSON, Location: "~/.config/opencode/opencode.json（或项目内 opencode.json）", Content: conf,
			AgentPrompt: agentPrompt("",
				mergeStep("~/.config/opencode/opencode.json", "mcp", "json", conf),
				"在终端执行 `opencode mcp auth "+serverName+"`，"+oauthBrowser+"。"),
			Note: oauthNeedsUser + oauthUntested,
		}
	case "gemini_cli":
		key.AgentPrompt = agentPrompt(promptKeyGuard, mergeStep("~/.gemini/settings.json", "mcpServers", "json", it.Content))
		conf := jsonDoc(`{"mcpServers":{"` + serverName + `":{"httpUrl":` + jsonStr(u) + `}}}`)
		oauth = &ClientMode{
			Format: SnippetJSON, Location: "~/.gemini/settings.json（或项目内 .gemini/settings.json）", Content: conf,
			AgentPrompt: agentPrompt("",
				mergeStep("~/.gemini/settings.json", "mcpServers", "json", conf),
				"告诉我下一步：在 Gemini CLI 里输入 /mcp auth seeaitv，"+oauthBrowser+"。"),
			Note: oauthNeedsUser + "项目内配置要求该目录已被 Gemini CLI 信任。" + oauthUntested,
		}
	case "workbuddy":
		key.AgentPrompt = agentPrompt(promptKeyGuard,
			"在 WorkBuddy 的 MCP 设置（JSON 配置）里，把下面这段合并进 mcpServers，保留已有的其他服务器："+codeBlock("json", it.Content))
		oauth = &ClientMode{
			Note: "待实测：WorkBuddy 是否支持 OAuth 一键登录尚未核实（测试机未安装），请先用「密钥 · CLI」方式接入。",
		}
	case "stdio_bridge":
		// ⚠️ 桥接没有 oauth 这一项（设计如此，MCPT-11B）：不是「不支持」，是这条路本来就只走密钥。
		key.AgentPrompt = agentPrompt(promptKeyGuard,
			"确认本机 PATH 里有 seeAitv 发布包里的 mcp-bridge（与服务端同版本的单文件）；没有就停下来问我要，不要从网上下载同名程序。",
			"在你的 MCP 配置里把下面这段合并进 mcpServers（密钥只放 env 的 SEEAITV_MCP_KEY，不要写进 args）："+codeBlock("json", it.Content))
	default:
		key.AgentPrompt = agentPrompt(promptKeyGuard, "按「"+it.Location+"」放入下面的配置："+codeBlock(it.Format, it.Content))
	}
	if oauth == nil {
		return []ClientMode{key}
	}
	oauth.Mode = ModeOAuth
	oauth.Supported = oauth.Content != ""
	return []ClientMode{*oauth, key}
}

// jsonStr 把字符串编码成 JSON 字符串字面量。⚠️ 关掉 HTML 转义：默认会把 `<` 写成反斜杠 u003c，
// 占位符 `<YOUR_MCP_KEY>` 就不再是字面量，前端替换不到它。
func jsonStr(s string) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)
	return strings.TrimSuffix(b.String(), "\n")
}

// jsonDoc 把紧凑 JSON 排成两空格缩进（保留键序 —— 不经 map，否则键会按字典序重排）。
func jsonDoc(compact string) string {
	var b bytes.Buffer
	if err := json.Indent(&b, []byte(compact), "", "  "); err != nil {
		panic("mcp: 片段模板不是合法 JSON: " + err.Error()) // 模板是常量，坏了是代码错误
	}
	return b.String()
}

// shQuote 给 POSIX shell 用的单引号转义。
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
