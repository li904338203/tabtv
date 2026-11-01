/**
 * 「MCP 访问」页的已连接客户端与工作区「MCP 连接」分区（MCPT-14）—— 纯函数。
 *
 * ⚠️ 角色判定只用于**显示不显示分区**：工作区接口要 manage，真正的拦截在服务端
 *    （editor / viewer 直打一样 403）。显示一个必定 403 的分区比不显示更糟。
 * ⚠️ 开关与撤销都是**实时**的：服务端每次调用都重新读，没有生效延迟（文案据此写「下一次调用」）。
 */
import type { components } from "@tabtv/api-client";
import type { WorkspaceRole } from "~/utils/agentState";

export type McpGrant = components["schemas"]["McpGrant"];
export type McpConnection = components["schemas"]["McpConnection"];
export type McpConnectionPage = components["schemas"]["McpConnectionPage"];
export type McpWorkspaceSettings = components["schemas"]["McpWorkspaceSettings"];

/** 只有 owner / admin 看得到「本工作区连接」分区（服务端要 manage）。 */
export function canManageWorkspaceMcp(role: WorkspaceRole | null | undefined): boolean {
  return role === "owner" || role === "admin";
}

/** 「已连接的客户端」只列 OAuth：访问密钥在上面的密钥列表里已经有一行，列两次会让人以为有两个连接。 */
export function oauthGrants(grants: readonly McpGrant[]): McpGrant[] {
  return grants.filter((g) => g.source === "oauth");
}

/** 客户端来源：CIMD 的身份由域名担保；DCR 是客户端自报的名字 —— 要说清「未验证」。 */
export function clientSourceText(g: Pick<McpGrant, "source" | "clientRegistration" | "clientHost" | "keyPrefix">): string {
  if (g.source === "access_key") return g.keyPrefix ? `访问密钥 sk_mcp_${g.keyPrefix}…` : "访问密钥";
  if (g.clientRegistration === "cimd") return g.clientHost ? `已验证来源：${g.clientHost}` : "已验证来源";
  return "未验证的客户端（名称由客户端自报）";
}

/** 连接是谁的：工作区视角给昵称；已离开的成员昵称取不到，如实说出来并留下 ID。 */
export function connectionUserText(c: Pick<McpConnection, "userId" | "userName">): string {
  return c.userName ?? `已离开的成员（${c.userId}）`;
}

export function grantStatusText(status: McpGrant["status"]): string {
  return status === "active" ? "有效" : "已撤销";
}

/** 撤销确认：写清是哪个客户端、后果（下一次调用即失效、要重新授权）。工作区管理员撤销的是别人的连接，要点名。 */
export function grantRevokeConfirmText(
  g: Pick<McpGrant, "clientName" | "source">,
  who?: string,
): string {
  const redo = g.source === "oauth" ? "在客户端里重新连接 seeAitv" : "新建一把访问密钥";
  const owner = who ? `${who}的` : "";
  return `撤销${owner}「${g.clientName}」后，该客户端下一次调用即失效，且无法恢复；需要时${who ? "对方" : ""}要${redo}。`;
}

/**
 * 关闭「花钱需在 seeAitv 页面确认」前的二次确认（MCPT-13 的约定：关闭须二次确认）。
 * 打开不需要确认 —— 打开只会让花钱更难，不会更容易。
 */
export function pageConfirmOffText(g: Pick<McpGrant, "clientName">): string {
  return `关闭后，「${g.clientName}」发起的花钱操作只需在客户端里确认即可，不必再到 seeAitv 页面批准。确定关闭？`;
}

/** 工作区开关的确认文案。关闭不撤销连接，站内功能不受影响。 */
export function workspaceSwitchText(enable: boolean): string {
  return enable
    ? "打开后，本工作区已有的外部客户端连接立即恢复可用。"
    : "关闭后，外部 AI 客户端对本工作区的下一次调用即被拒绝；不会撤销任何连接，重新打开即恢复。站内功能不受影响。";
}

/** 平台关着时工作区开着也没用 —— 如实说出来，不然管理员会以为是自己没打开。 */
export function workspaceSwitchNotice(s: McpWorkspaceSettings | null): string | null {
  if (!s) return null;
  if (!s.platformEnabled) return "平台已暂停 MCP：无论这里开关如何，外部客户端暂时都无法访问";
  if (!s.enabled) return "本工作区的 MCP 已关闭：外部客户端的调用会被拒绝（连接仍保留）";
  return null;
}
