/**
 * 「MCP 访问」页的客户端配置片段（MCPT-11F）。
 *
 * 片段由服务端渲染（`GET /me/mcp/client-snippets`）：地址取自 `SEEAITV_MCP_PUBLIC_URL`，
 * 密钥位置是占位符。前端**不拼地址、不改格式**，只做两件事：排序、以及刚建完密钥时
 * 在**内存里**把占位符换成明文供复制（结果不进 store / 本地存储 / 日志）。
 *
 * ⚠️ 片段格式以 MCPT-15 兼容矩阵的实测为准；改片段改服务端模板，不用发前端版本。
 */
import type { components } from "@tabtv/api-client";

export type McpClientSnippet = components["schemas"]["McpClientSnippet"];
export type McpClientSnippets = components["schemas"]["McpClientSnippets"];

/** 页签顺序（与服务端 `app.SnippetClients()` 一致）；服务端多给的未知客户端排在最后、保持原序。 */
export const SNIPPET_CLIENT_ORDER: readonly string[] = [
  "claude_code",
  "codex",
  "cursor",
  "opencode",
  "workbuddy",
  "gemini_cli",
  "stdio_bridge",
];

export function orderSnippets(items: readonly McpClientSnippet[]): McpClientSnippet[] {
  const rank = (c: string) => {
    const i = SNIPPET_CLIENT_ORDER.indexOf(c);
    return i < 0 ? SNIPPET_CLIENT_ORDER.length : i;
  };
  return items
    .map((item, index) => ({ item, index }))
    .sort((a, b) => rank(a.item.client) - rank(b.item.client) || a.index - b.index)
    .map((x) => x.item);
}

/**
 * 把片段里的密钥占位符换成明文。没有明文（没刚建密钥）时**原样返回**，保留占位符。
 * ⚠️ 只替换占位符本身，其余内容一个字不动（地址、格式都以服务端为准）。
 */
export function fillSnippet(content: string, placeholder: string, plaintext?: string | null): string {
  if (!plaintext || !placeholder) return content;
  return content.split(placeholder).join(plaintext);
}

/** 排好序并（可选）填入明文的片段列表。 */
export function renderSnippets(res: McpClientSnippets | null | undefined, plaintext?: string | null): McpClientSnippet[] {
  if (!res) return [];
  return orderSnippets(res.items).map((item) => ({
    ...item,
    content: fillSnippet(item.content, res.keyPlaceholder, plaintext),
  }));
}

/** 片段为空时的说明；有片段返回 null。 */
export function snippetsEmptyText(res: McpClientSnippets | null | undefined): string | null {
  if (!res) return "配置片段还没有加载出来";
  return res.items.length ? null : "暂时没有可用的配置片段";
}

/** 平台没配 MCP 对外地址时的如实说明；配了返回 null。 */
export function snippetsUrlNotice(res: McpClientSnippets | null | undefined): string | null {
  if (!res || res.mcpUrlConfigured) return null;
  return `平台尚未配置 MCP 对外地址，片段里的 ${res.mcpUrl} 需要替换为管理员提供的地址`;
}

/** 片段语言的显示名。 */
export function snippetFormatLabel(format: McpClientSnippet["format"]): string {
  return { json: "JSON", toml: "TOML", shell: "命令" }[format] ?? format;
}
