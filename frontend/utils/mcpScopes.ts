/**
 * OAuth 同意页 `/mcp/authorize`（MCPT-13，MCP-F-05 / MCP-AC-14）—— 勾选 → scope / 工具集、角色不足提示、
 * 花钱说明、客户端来源说明的纯函数。
 *
 * ⚠️ 前端的角色提示只是**说明**，不是安全边界：有效权限 = 授权的 scope ∩ 调用当时的成员角色，
 *    由服务端每次调用实时算（MCP-F-11）。这里标出「勾了也不会生效」，是为了不让用户以为他授予了什么。
 * ⚠️ 回跳地址**不在这里**：同意页只把服务端 `POST /mcp/consent` 回的 `redirectTo` 交给 `location.assign`，
 *    从不读 URL 参数拼地址（回跳只用服务端给的，见页面注释）。
 */
import type { components } from "@tabtv/api-client";
import type { WorkspaceRole } from "~/utils/agentState";
import { errorCode } from "~/utils/asyncState";
import {
  DEFAULT_TOOLSETS,
  SCOPE_OPTIONS,
  TOOLSET_LABELS,
  scopesText,
  type McpScope,
  type McpToolset,
  type McpToolsetInfo,
} from "~/utils/mcpKeys";

export type McpConsentInfo = components["schemas"]["McpConsentInfo"];
export type McpConsentSubmit = components["schemas"]["McpConsentSubmit"];
export type McpConsentResult = components["schemas"]["McpConsentResult"];

/** 同意页上选工作区要用的那几个字段（来自 `stores/workspace.ts` 的 list）。 */
export type ConsentWorkspace = { id: string; name: string; kind: "personal" | "team"; role: WorkspaceRole };

export type ConsentForm = {
  workspaceId: string;
  scopes: McpScope[];
  toolsets: McpToolset[];
  requirePageConfirm: boolean;
};

const VIEW: McpScope = "seeaitv:view";
const GENERATE: McpScope = "seeaitv:generate";
const TOOLSET_ORDER = Object.keys(TOOLSET_LABELS) as McpToolset[];

/** 角色能做的动作（与后端 `collaboration/domain` 角色矩阵的 view/edit/generate/export 四列一致）。 */
export function roleAllowsScope(role: WorkspaceRole | undefined, scope: McpScope): boolean {
  if (scope === VIEW) return Boolean(role);
  return role === "owner" || role === "admin" || role === "editor";
}

export const ROLE_LABELS: Record<WorkspaceRole, string> = {
  owner: "所有者",
  admin: "管理员",
  editor: "编辑",
  viewer: "只读",
};

export function roleLabel(role: WorkspaceRole | undefined): string {
  return role ? ROLE_LABELS[role] ?? role : "未知角色";
}

/**
 * 勾了某个 scope 在该工作区「不会生效」时的说明；会生效返回 null。
 * ⚠️ 只读成员勾了也照样能勾（服务端按 scope ∩ 角色算），这里只是如实说。
 */
export function scopeRoleHint(role: WorkspaceRole | undefined, scope: McpScope): string | null {
  if (roleAllowsScope(role, scope)) return null;
  return role === "viewer" ? "你在该工作区是只读，勾选也不会生效" : "你在该工作区的角色不足，勾选也不会生效";
}

/** 勾选了但不会生效的那几项（按固定顺序）。 */
export function ineffectiveScopes(role: WorkspaceRole | undefined, scopes: readonly McpScope[]): McpScope[] {
  return SCOPE_OPTIONS.map((o) => o.value).filter((s) => scopes.includes(s) && !roleAllowsScope(role, s));
}

/** 一条 scope 选项：只列客户端申请了的（勾选必须 ⊆ 申请的，服务端也会拒）。 */
export type ConsentScopeOption = { value: McpScope; label: string; hint: string; locked: boolean; roleHint: string | null };

export function consentScopeOptions(requested: readonly McpScope[], role: WorkspaceRole | undefined): ConsentScopeOption[] {
  const asked = new Set<McpScope>([...requested, VIEW]);
  return SCOPE_OPTIONS.filter((o) => asked.has(o.value)).map((o) => ({
    value: o.value,
    label: o.label,
    hint: o.hint,
    locked: o.value === VIEW,
    roleHint: scopeRoleHint(role, o.value),
  }));
}

/** 工具集选项。服务端给了可用性（`/me/mcp/client-snippets` 的 toolsets）时不可用的标「后续开放」不可选；没给时全部可选。 */
export type ConsentToolsetOption = { name: McpToolset; label: string; selectable: boolean; hint: string };

export function consentToolsetOptions(infos?: readonly McpToolsetInfo[] | null): ConsentToolsetOption[] {
  const avail = new Map<McpToolset, boolean>();
  if (infos?.length) for (const i of infos) avail.set(i.name, i.available);
  return TOOLSET_ORDER.map((name) => {
    const selectable = infos?.length ? avail.get(name) === true : true;
    return { name, label: TOOLSET_LABELS[name], selectable, hint: selectable ? "" : "后续开放" };
  });
}

/**
 * 默认勾选的工具集：默认开启集（context / projects / canvas / catalog / generation）；
 * script / workflow / timeline / agent 默认不勾。客户端连的是 `/mcp/x/{toolset}` 时把那一个也勾上
 * （路径已经收窄到它，授权里没有它的话这个客户端什么都用不了）。不可选的不勾。
 */
export function defaultConsentToolsets(info: Pick<McpConsentInfo, "toolset">, infos?: readonly McpToolsetInfo[] | null): McpToolset[] {
  const selectable = new Set(consentToolsetOptions(infos).filter((o) => o.selectable).map((o) => o.name));
  const want = new Set<McpToolset>(DEFAULT_TOOLSETS);
  if (info.toolset && (TOOLSET_ORDER as string[]).includes(info.toolset)) want.add(info.toolset as McpToolset);
  return TOOLSET_ORDER.filter((n) => want.has(n) && selectable.has(n));
}

/** 初始表单：工作区取当前工作区（不在列表里时取第一个）；scope 取客户端申请的全部（补 view）；「页面确认」默认关。 */
export function initialConsentForm(
  info: Pick<McpConsentInfo, "requestedScopes" | "toolset">,
  workspaces: readonly Pick<ConsentWorkspace, "id">[],
  currentId: string,
  infos?: readonly McpToolsetInfo[] | null,
): ConsentForm {
  const workspaceId = workspaces.some((w) => w.id === currentId) ? currentId : workspaces[0]?.id ?? "";
  return {
    workspaceId,
    scopes: toggleConsentScope(info.requestedScopes, VIEW, true),
    toolsets: defaultConsentToolsets(info, infos),
    requirePageConfirm: false,
  };
}

/** 勾 / 取消一个 scope。view 必选不可取消；结果按固定顺序。 */
export function toggleConsentScope(scopes: readonly McpScope[], scope: McpScope, on: boolean): McpScope[] {
  const set = new Set(scopes);
  if (on) set.add(scope);
  else if (scope !== VIEW) set.delete(scope);
  set.add(VIEW);
  return SCOPE_OPTIONS.map((o) => o.value).filter((v) => set.has(v));
}

/** 勾 / 取消一个工具集；不可选的勾不上。结果按定义顺序。 */
export function toggleConsentToolset(
  toolsets: readonly McpToolset[],
  name: McpToolset,
  on: boolean,
  infos?: readonly McpToolsetInfo[] | null,
): McpToolset[] {
  const set = new Set(toolsets);
  const option = consentToolsetOptions(infos).find((o) => o.name === name);
  if (on && option?.selectable) set.add(name);
  if (!on) set.delete(name);
  return TOOLSET_ORDER.filter((n) => set.has(n));
}

/** 提交前的校验：返回第一条问题，没问题返回 null。 */
export function validateConsentForm(form: ConsentForm): string | null {
  if (!form.workspaceId) return "请选择要授权的工作区";
  if (!form.toolsets.length) return "至少选择一个工具集";
  return null;
}

/** 「允许」的请求体。⚠️ 回跳地址不在请求里也不在 URL 里：由服务端从签名句柄里取。 */
export function consentAllowBody(req: string, form: ConsentForm): McpConsentSubmit {
  return {
    req,
    decision: "allow",
    workspaceId: form.workspaceId,
    scopes: toggleConsentScope(form.scopes, VIEW, true),
    toolsets: [...form.toolsets],
    requirePageConfirm: form.requirePageConfirm,
  };
}

export function consentDenyBody(req: string): McpConsentSubmit {
  return { req, decision: "deny" };
}

/**
 * 勾了 generate 时的花钱说明（MCP-AC-14）。没勾返回 null。
 * 团队工作区额外说明「消耗的是团队积分、管理员看得到是你」—— 用户最容易误会的是「花的是谁的钱」。
 */
export function spendNotice(
  scopes: readonly McpScope[],
  workspace: Pick<ConsentWorkspace, "name" | "kind"> | null | undefined,
): { text: string; team: string | null } | null {
  if (!scopes.includes(GENERATE)) return null;
  const name = workspace?.name ? `「${workspace.name}」` : "所选工作区";
  const payer = workspace?.kind === "team" ? "团队" : "个人";
  return {
    text: `生成、脚本付费步骤、整图运行、成片渲染会消耗${name}的积分（付费账户：${payer}），每一次都会先请你确认金额`,
    team: workspace?.kind === "team" ? "消耗的是团队积分，团队管理员可在用量审计里看到是你" : null,
  };
}

/** 「花钱需在 seeAitv 页面确认」开关的说明。 */
export const PAGE_CONFIRM_HINT =
  "开启后，客户端里的确认不算数，必须打开 seeAitv 的审批页点确认。客户端设了「自动批准工具」时推荐开启";

/** 角色不足的汇总提示（显示在 scope 列表上方）；都会生效时返回 null。 */
export function consentRoleNotice(role: WorkspaceRole | undefined, scopes: readonly McpScope[]): string | null {
  const bad = ineffectiveScopes(role, scopes);
  if (!bad.length) return null;
  const who = role === "viewer" ? "你在该工作区是只读" : "你在该工作区的角色不足";
  return `${who}，${scopesText(bad)}权限勾选也不会生效（实际权限 = 授权勾选 ∩ 你在工作区的角色，每次调用实时计算）`;
}

/**
 * 客户端来源。CIMD：身份由元数据文档所在域名担保 →「已验证来源」；DCR：名字是客户端自报的 →
 * 醒目提示「未验证的客户端」（攻击者完全可以注册一个叫「Claude」的客户端）。
 */
export function clientSource(client: McpConsentInfo["client"]): { verified: boolean; text: string } {
  if (client.verified && client.host) return { verified: true, text: `已验证来源：${client.host}` };
  return {
    verified: false,
    text: "未验证的客户端：名称由客户端自报，seeAitv 无法确认它是谁。只有在你刚刚亲自发起连接时才继续",
  };
}

/** 同意页加载 / 提交失败的文案。`expired=true` 时不给重试（只能回客户端重新连接）。 */
export function consentError(cause: unknown, online: boolean): { text: string; retry: boolean; expired: boolean } {
  if (!online) return { text: "网络已断开，恢复后点重试", retry: true, expired: false };
  const code = errorCode(cause);
  // 40007 MCP_CONSENT_EXPIRED：句柄无效或过期（10 分钟），两者出路一样。
  if (code === 40007) return { text: "授权请求已过期，请回到客户端重新连接", retry: false, expired: true };
  // 40302 NOT_WORKSPACE_MEMBER：所选工作区里已经不是成员了。
  if (code === 40302) return { text: "你已不是所选工作区的成员，请换一个工作区", retry: false, expired: false };
  const status = typeof cause === "object" && cause !== null && "statusCode" in cause
    ? Number((cause as { statusCode?: unknown }).statusCode)
    : undefined;
  if (status === 404) return { text: "平台暂未开放 MCP 连接", retry: false, expired: false };
  const message = typeof cause === "object" && cause !== null && "message" in cause
    ? String((cause as { message?: unknown }).message ?? "")
    : "";
  return { text: message ? `出错了：${message}${code ? `（${code}）` : ""}` : "出错了，请重试", retry: true, expired: false };
}
