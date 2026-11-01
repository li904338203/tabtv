/**
 * 页面审批 `/mcp/approve/[quoteId]`（MCPT-13，MCP-F-44 / MCP-AC-15）—— 审批状态 → 文案与按钮可用性的纯函数。
 *
 * ⚠️ 非本人时**什么报价内容都不显示**（客户端名、金额、工作区都不显示）：服务端本来就不回，
 *    这里再保证一次 —— 页面只能从 `approvalScreen` 拿要显示的东西，非本人那一屏里没有这些字段。
 * ⚠️ 前端禁用按钮只是体验：余额不足、已过期、已拒绝的批准，服务端都会按真实状态拒绝。
 */
import type { components } from "@tabtv/api-client";
import { errorCode } from "~/utils/asyncState";

export type McpApproval = components["schemas"]["McpApproval"];
export type ApprovalState = McpApproval["state"];

/** 页面这一屏是什么：数据已到（按状态）/ 非本人 / 不存在 / 连接已撤销 / 加载失败。 */
export type ApprovalScreen =
  | { kind: "approval"; approval: McpApproval; title: string; text: string; tone: Tone; canApprove: boolean;
      canReject: boolean; approveBlocked: string | null }
  | { kind: "not_owner"; title: string; text: string }
  | { kind: "not_found"; title: string; text: string }
  | { kind: "revoked"; title: string; text: string }
  | { kind: "error"; title: string; text: string; retry: boolean };

export type Tone = "pending" | "success" | "danger" | "muted";

export const NOT_OWNER_TEXT = "这条审批属于另一个 seeAitv 账号。请退出后用发起授权的账号登录再打开此链接。";
export const EXPIRED_TEXT = "审批已过期（有效期 10 分钟）。回到客户端让它重新发起，会得到新的报价。";
export const USED_TEXT = "这笔已经执行过了，不会重复扣费。";
export const REJECTED_TEXT = "你已拒绝，这笔不会执行。";
export const APPROVED_TEXT = "已批准，回到客户端让它继续即可。";
export const VOIDED_TEXT = "价格在确认之后变了，这笔报价已作废、没有扣费。回到客户端，它会拿到新的报价与新的审批链接。";

/** 余额不足时批准按钮上的说明（带差额）；够用时返回 null。 */
export function shortfallText(a: Pick<McpApproval, "shortfall" | "credits" | "balance">): string | null {
  if (a.shortfall <= 0) return null;
  return `余额不足，还差 ${a.shortfall} 积分（本次需要 ${a.credits}，当前余额 ${a.balance}）。补足积分后再批准`;
}

/** 按状态给出这一屏的标题、说明与按钮。 */
export function stateScreen(a: McpApproval): Extract<ApprovalScreen, { kind: "approval" }> {
  const base = { kind: "approval" as const, approval: a, canApprove: false, canReject: false, approveBlocked: null };
  switch (a.state) {
    case "pending": {
      const blocked = shortfallText(a);
      return {
        ...base,
        title: "确认这笔花费",
        text: `「${a.clientName}」请求在「${a.workspaceName}」里花费 ${a.credits} 积分。确认之后回到客户端让它继续；不确认就不会执行、不会扣费。`,
        tone: "pending",
        canApprove: !blocked,
        canReject: true,
        approveBlocked: blocked,
      };
    }
    case "approved":
      return { ...base, title: "已批准", text: APPROVED_TEXT, tone: "success" };
    case "used":
      return { ...base, title: "已执行", text: USED_TEXT, tone: "muted" };
    case "rejected":
      return { ...base, title: "已拒绝", text: REJECTED_TEXT, tone: "danger" };
    case "voided":
      return { ...base, title: "报价已作废", text: VOIDED_TEXT, tone: "muted" };
    case "expired":
    default:
      return { ...base, title: "审批已过期", text: EXPIRED_TEXT, tone: "muted" };
  }
}

/**
 * 加载 / 提交失败 → 这一屏。
 * ⚠️ 40309 非本人：只给换账号的说明，不带任何报价内容（服务端也不回）。
 */
export function errorScreen(cause: unknown, online: boolean): ApprovalScreen {
  if (!online) return { kind: "error", title: "网络已断开", text: "网络已断开，恢复后点重试", retry: true };
  const code = errorCode(cause);
  if (code === 40309) return { kind: "not_owner", title: "无法查看这条审批", text: NOT_OWNER_TEXT };
  const status = typeof cause === "object" && cause !== null && "statusCode" in cause
    ? Number((cause as { statusCode?: unknown }).statusCode)
    : undefined;
  if (code === 40401 || status === 404) {
    return { kind: "not_found", title: "审批不存在", text: "链接不完整，或这条审批已被清理。回到客户端让它重新发起。" };
  }
  // 40905：发起这笔花费的连接已被撤销。
  if (code === 40905) {
    return { kind: "revoked", title: "连接已撤销", text: "发起这笔花费的客户端连接已被撤销，这笔不会执行，也不会扣费。" };
  }
  const message = typeof cause === "object" && cause !== null && "message" in cause
    ? String((cause as { message?: unknown }).message ?? "")
    : "";
  return { kind: "error", title: "加载失败", text: message ? `${message}${code ? `（${code}）` : ""}` : "加载失败，请重试", retry: true };
}

/** 付费账户的说法：个人 / 团队。团队要说清「团队管理员看得到是你」。 */
export function payerText(a: Pick<McpApproval, "workspaceKind" | "workspaceName">): string {
  return a.workspaceKind === "team"
    ? `团队「${a.workspaceName}」的积分（团队管理员可在用量审计里看到是你）`
    : `个人空间「${a.workspaceName}」的积分`;
}

/** 确认后剩余的说法：不足时不显示负数，改说差额。 */
export function afterText(a: Pick<McpApproval, "after" | "shortfall">): string {
  return a.shortfall > 0 ? `不足，还差 ${a.shortfall}` : String(a.after);
}

/** 提交批准时撞上余额不足（42201）的提示：带差额（服务端 details.shortfall）。 */
export function approveErrorText(cause: unknown): string | null {
  if (errorCode(cause) !== 42201) return null;
  const details = typeof cause === "object" && cause !== null && "data" in cause
    ? (cause as { data?: { details?: Record<string, unknown> } }).data?.details
    : undefined;
  const shortfall = typeof details?.shortfall === "number" ? details.shortfall : undefined;
  return shortfall ? `余额不足，还差 ${shortfall} 积分，补足后再批准` : "余额不足，补足积分后再批准";
}
