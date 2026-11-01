/**
 * 「MCP 接入指南」页（`/mcp`，MCPT-11F 改版）的纯函数：客户端页签、接入方式切换、复制内容。
 *
 * 命令 / 配置 / 话术**全部由服务端给**（`GET /me/mcp/client-snippets` 的 `items[].modes`）：
 * 前端不拼命令、不改格式，只决定显示哪个页签、复制哪一段。
 *
 * ⚠️ 这一页**永远不持有密钥明文**：密钥方式的话术里是占位符，用户自己替换后再发给 Agent。
 *    `guideCopyText` 发现形似明文的内容时拒绝复制 —— 那只可能是哪里出了错。
 */
import type { components } from "@tabtv/api-client";
import { orderSnippets, type McpClientSnippet, type McpClientSnippets } from "./mcpSnippets";

export type McpClientMode = components["schemas"]["McpClientMode"];
export type GuideMode = McpClientMode["mode"];

/** 两段切换（顺序即显示顺序）。 */
export const GUIDE_MODES: readonly { mode: GuideMode; label: string; hint: string }[] = [
  { mode: "oauth", label: "一键接入", hint: "浏览器授权，不需要密钥" },
  { mode: "key", label: "密钥 · CLI", hint: "访问密钥 + 配置 / 命令，含 stdio 桥接" },
];

/** 主视觉两侧的客户端（固定，不等接口：主视觉不该因为接口慢而空着）。 */
export const HERO_CLIENTS: readonly string[] = ["claude_code", "codex", "cursor", "gemini_cli", "opencode", "workbuddy"];

/** 主视觉用的显示名（页签用服务端给的 label）。 */
export const CLIENT_LABELS: Readonly<Record<string, string>> = {
  claude_code: "Claude Code",
  codex: "Codex",
  cursor: "Cursor",
  gemini_cli: "Gemini CLI",
  opencode: "OpenCode",
  workbuddy: "WorkBuddy",
  stdio_bridge: "stdio 桥接",
};

/** 某客户端在某方式下的那一项；不适用（服务端没给这一项）返回 null。 */
export function modeOf(item: McpClientSnippet | null | undefined, mode: GuideMode): McpClientMode | null {
  return item?.modes?.find((m) => m.mode === mode) ?? null;
}

/**
 * 某方式下显示哪些客户端页签：有这一项的才显示（支持与否都显示 —— 不支持的要说清为什么）。
 * 根本不适用的不显示：stdio 桥接只在「密钥 · CLI」下出现。
 */
export function guideClients(res: McpClientSnippets | null | undefined, mode: GuideMode): McpClientSnippet[] {
  if (!res) return [];
  return orderSnippets(res.items).filter((item) => modeOf(item, mode) !== null);
}

/** 当前选中的客户端：选过且在当前方式下可见就保持，否则落到第一个（切换方式时不把人丢到空白页）。 */
export function pickClient(clients: readonly McpClientSnippet[], selected: string | null | undefined): McpClientSnippet | null {
  return clients.find((c) => c.client === selected) ?? clients[0] ?? null;
}

/** 形似访问密钥明文（`sk_mcp_` 后面跟着字符）。 */
const SECRET_LIKE = /sk_mcp_[A-Za-z0-9]/;

/**
 * 复制给 Agent 的内容：服务端给的话术原文。
 * 不支持 / 没有话术 / 内容形似明文时返回 null（按钮置灰，不复制）。
 */
export function guideCopyText(entry: McpClientMode | null | undefined): string | null {
  if (!entry?.supported || !entry.agentPrompt) return null;
  if (SECRET_LIKE.test(entry.agentPrompt)) return null;
  return entry.agentPrompt;
}

/** 当前方式不支持时的说明（优先用服务端的 note）。 */
export function unsupportedText(entry: McpClientMode | null | undefined): string {
  return entry?.note || "该客户端暂不支持一键接入，请改用「密钥 · CLI」方式";
}

/** 图标认不出来时的首字母徽标。 */
export function clientInitial(label: string | null | undefined): string {
  const ch = (label ?? "").trim().charAt(0);
  return ch ? ch.toUpperCase() : "?";
}
