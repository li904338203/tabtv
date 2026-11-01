/**
 * 「MCP 访问」页的访问密钥部分（MCPT-11F）—— 表单、文案与角色提示的纯函数。
 *
 * ⚠️ 这里**没有任何一个函数接收或返回密钥明文**：明文只在创建响应里出现一次，
 *    只活在弹层组件的内存里（关闭即丢）。表单 → 请求体这一步也不可能把它带进去。
 * ⚠️ 前端的角色提示只是**说明**，不是安全边界：有效权限 = 密钥 scope ∩ 调用当时的成员角色，
 *    由服务端每次调用实时算（MCP-F-11）。
 */
import type { components } from "@tabtv/api-client";
import type { WorkspaceRole } from "~/utils/agentState";
import { errorCode } from "~/utils/asyncState";

export type McpScope = components["schemas"]["McpScope"];
export type McpToolset = components["schemas"]["McpToolset"];
export type McpAccessKey = components["schemas"]["McpAccessKey"];
export type McpAccessKeyCreate = components["schemas"]["McpAccessKeyCreate"];
export type McpToolsetInfo = components["schemas"]["McpToolsetInfo"];

/** scope 的展示顺序与中文名（与服务端 `domain.Scopes()` 同序）。 */
export const SCOPE_OPTIONS: ReadonlyArray<{ value: McpScope; label: string; hint: string }> = [
  { value: "seeaitv:view", label: "查看", hint: "列项目、读画布、查模型与价格、查任务与产物" },
  { value: "seeaitv:edit", label: "编辑", hint: "建项目、写画布与项目文档" },
  { value: "seeaitv:generate", label: "生成", hint: "发起生成（花该工作区的积分）" },
  { value: "seeaitv:export", label: "导出", hint: "渲染成片、批量导出" },
];

/** 工具集的中文名（与服务端 `domain.toolsetDefs` 同序）。 */
export const TOOLSET_LABELS: Record<McpToolset, string> = {
  context: "当前身份与凭据",
  projects: "项目",
  canvas: "画布与项目文档",
  catalog: "模型与图片工具目录",
  generation: "报价、生成、任务与媒体",
  script: "脚本线",
  workflow: "整图运行",
  timeline: "时间线与成片",
  agent: "委托站内 Agent",
};

const TOOLSET_ORDER = Object.keys(TOOLSET_LABELS) as McpToolset[];

/** 服务端没给可用性时的兜底：默认开启集（与 `domain.DefaultToolsets()` 一致）。 */
export const DEFAULT_TOOLSETS: readonly McpToolset[] = ["context", "projects", "canvas", "catalog", "generation"];

const VIEW: McpScope = "seeaitv:view";

/** 有效期选项。默认 90 天（MCP-PARAM-05，**设计建议值**）。 */
export type ExpiryChoice = "90" | "30" | "never";
export const EXPIRY_OPTIONS: ReadonlyArray<{ value: ExpiryChoice; label: string }> = [
  { value: "90", label: "90 天" },
  { value: "30", label: "30 天" },
  { value: "never", label: "永久" },
];

export type KeyForm = {
  workspaceId: string;
  clientName: string;
  scopes: McpScope[];
  toolsets: McpToolset[];
  expiry: ExpiryChoice;
  /** 花钱需在 seeAitv 页面确认（MCPT-13 页面审批已接通，MCPT-14 放开）。默认关：花钱在客户端里确认。 */
  requirePageConfirm: boolean;
};

export const MAX_CLIENT_NAME = 100;

/** 一个工具集选项：不可用的照样列出并说明（隐藏的话用户会以为不存在）。 */
export type ToolsetOption = { name: McpToolset; label: string; selectable: boolean; hint: string };

/**
 * 工具集选项。`available=false`（注册表里此刻没有这个工具集的工具）标「后续开放」且不可选 ——
 * 授予了也什么都用不了。服务端没给可用性（`infos` 为空）时只放开默认开启集。
 */
export function toolsetOptions(infos?: readonly McpToolsetInfo[] | null): ToolsetOption[] {
  const avail = new Map<McpToolset, boolean>();
  if (infos?.length) for (const i of infos) avail.set(i.name, i.available);
  return TOOLSET_ORDER.map((name) => {
    const selectable = infos?.length ? avail.get(name) === true : DEFAULT_TOOLSETS.includes(name);
    return { name, label: TOOLSET_LABELS[name], selectable, hint: selectable ? "" : "后续开放" };
  });
}

/** 新建表单的初值：view 必选、工具集取「默认开启 ∩ 可用」、有效期 90 天。 */
export function defaultKeyForm(workspaceId: string, infos?: readonly McpToolsetInfo[] | null): KeyForm {
  const selectable = new Set(toolsetOptions(infos).filter((o) => o.selectable).map((o) => o.name));
  const defaults = infos?.length ? infos.filter((i) => i.defaultOn).map((i) => i.name) : [...DEFAULT_TOOLSETS];
  return {
    workspaceId,
    clientName: "",
    scopes: [VIEW],
    toolsets: TOOLSET_ORDER.filter((n) => defaults.includes(n) && selectable.has(n)),
    expiry: "90",
    requirePageConfirm: false,
  };
}

/** 勾 / 取消一个 scope。⚠️ view 必选且不可取消（能写不能读自相矛盾，服务端也会补上）。结果按固定顺序。 */
export function toggleScope(scopes: readonly McpScope[], scope: McpScope, on: boolean): McpScope[] {
  const set = new Set(scopes);
  if (on) set.add(scope);
  else if (scope !== VIEW) set.delete(scope);
  set.add(VIEW);
  return SCOPE_OPTIONS.map((o) => o.value).filter((v) => set.has(v));
}

export function isScopeLocked(scope: McpScope): boolean {
  return scope === VIEW;
}

/** 勾 / 取消一个工具集；不可选的勾不上。结果按定义顺序。 */
export function toggleToolset(
  toolsets: readonly McpToolset[],
  name: McpToolset,
  on: boolean,
  infos?: readonly McpToolsetInfo[] | null,
): McpToolset[] {
  const set = new Set(toolsets);
  const option = toolsetOptions(infos).find((o) => o.name === name);
  if (on && option?.selectable) set.add(name);
  if (!on) set.delete(name);
  return TOOLSET_ORDER.filter((n) => set.has(n));
}

/** 表单校验：返回第一条问题；没问题返回 null。 */
export function validateKeyForm(form: KeyForm): string | null {
  if (!form.workspaceId) return "请选择工作区";
  const name = form.clientName.trim();
  if (!name) return "请填写名称（用来认出是哪个客户端，如「我的 Claude Code」）";
  if ([...name].length > MAX_CLIENT_NAME) return `名称最多 ${MAX_CLIENT_NAME} 个字`;
  if (!form.toolsets.length) return "至少选择一个工具集";
  return null;
}

/**
 * 表单 → 请求体。
 *
 * 「花钱需在 seeAitv 页面确认」按表单取（MCPT-13 页面审批已接通：开了它的凭据每次花积分都要用户本人
 * 在 /mcp/approve 批准；MCPT-14 放开）。建好之后也能在列表里随时切换（关闭要二次确认）。
 */
export function keyFormToRequest(form: KeyForm): McpAccessKeyCreate {
  const base = {
    workspaceId: form.workspaceId,
    clientName: form.clientName.trim(),
    scopes: toggleScope(form.scopes, VIEW, true),
    toolsets: [...form.toolsets],
    requirePageConfirm: form.requirePageConfirm === true,
  };
  return form.expiry === "never"
    ? { ...base, neverExpires: true }
    : { ...base, neverExpires: false, expiresInDays: Number(form.expiry) };
}

export function scopeLabel(scope: string): string {
  return SCOPE_OPTIONS.find((o) => o.value === scope)?.label ?? scope;
}

export function scopesText(scopes: readonly string[]): string {
  return scopes.map(scopeLabel).join("、");
}

export function toolsetLabel(name: string): string {
  return TOOLSET_LABELS[name as McpToolset] ?? name;
}

export function toolsetsText(toolsets: readonly string[]): string {
  return toolsets.map(toolsetLabel).join("、");
}

/** 列表里的前缀显示：`sk_mcp_xxxxxxxx…`（只是用来认出它，不是密钥）。 */
export function keyPrefixText(prefix: string): string {
  return `sk_mcp_${prefix}…`;
}

function pad(n: number): string {
  return String(n).padStart(2, "0");
}

/** 本地时间 `YYYY-MM-DD`。 */
export function formatDate(iso: string): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso;
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}`;
}

/** 本地时间 `YYYY-MM-DD HH:mm`。 */
export function formatDateTime(iso: string): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso;
  return `${formatDate(iso)} ${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

/**
 * 到期文案。⚠️ 过期与否**只看服务端给的 `status`**（服务端按自己的时钟算），
 * 不拿 `expiresAt` 跟本机时钟比 —— 本机时钟偏几分钟，这一列就会和实际能不能用对不上。
 */
export function expiryText(key: Pick<McpAccessKey, "status" | "expiresAt">): string {
  if (key.status === "revoked") return "已撤销";
  if (key.status === "expired") return "已过期";
  if (!key.expiresAt) return "永久";
  return `${formatDate(key.expiresAt)} 到期`;
}

export function statusText(status: McpAccessKey["status"]): string {
  return { active: "有效", expired: "已过期", revoked: "已撤销" }[status] ?? status;
}

export function lastUsedText(lastUsedAt: string | null | undefined): string {
  return lastUsedAt ? formatDateTime(lastUsedAt) : "从未使用";
}

/** 角色能做的动作（与后端 `collaboration/domain.matrix` 的 view/edit/generate/export 四列一致）。 */
function roleAllows(role: WorkspaceRole | undefined, scope: McpScope): boolean {
  if (scope === VIEW) return Boolean(role);
  return role === "owner" || role === "admin" || role === "editor";
}

/**
 * 角色不足时的如实说明（MCP-F-11：权限 = scope ∩ 当前角色）。**不禁止创建**，只说清哪些不会生效。
 * 没有不生效的项时返回 null。
 */
export function roleScopeNotice(role: WorkspaceRole | undefined, scopes: readonly McpScope[]): string | null {
  const ineffective = scopes.filter((s) => s !== VIEW && !roleAllows(role, s));
  if (!ineffective.length) return null;
  const who = role === "viewer" ? "你在该工作区是只读" : "你在该工作区的角色不足";
  return `${who}，${scopesText(ineffective)}权限不会生效（实际权限 = 密钥勾选 ∩ 你在工作区的角色，每次调用实时计算）`;
}

/** 选了 generate 时的醒目提示。 */
export function generateNotice(scopes: readonly McpScope[]): string | null {
  return scopes.includes("seeaitv:generate") ? "会消耗该工作区的积分，每次都会请你确认费用" : null;
}

/** 撤销确认框的正文：写清后果。 */
export function revokeConfirmText(key: Pick<McpAccessKey, "clientName" | "prefix">): string {
  return `撤销「${key.clientName}」（${keyPrefixText(key.prefix)}）后，使用此密钥的客户端下一次调用即失效，且无法恢复；需要时请重新创建。`;
}

/** 加载失败的文案与是否给重试（四态里的「错 / 断网 / 403」，空态由页面按列表判断）。 */
export function mcpLoadError(cause: unknown, online: boolean): { text: string; retry: boolean } {
  // ⚠️ 断网要说成断网：说「加载失败」的话用户会反复点重试，而该做的是看自己的网络。
  if (!online) return { text: "网络已断开，恢复后点重试", retry: true };
  const status = typeof cause === "object" && cause !== null && "statusCode" in cause
    ? Number((cause as { statusCode?: unknown }).statusCode)
    : undefined;
  // ⚠️ 403 不给重试（重试多少次都一样）。
  if (status === 403) return { text: "没有权限查看 MCP 访问设置", retry: false };
  // MCP 总开关关闭时 /me/mcp/* 不挂路由 → 404。
  if (status === 404) return { text: "平台暂未开放 MCP 访问", retry: false };
  const message = typeof cause === "object" && cause !== null && "message" in cause
    ? String((cause as { message?: unknown }).message ?? "")
    : "";
  const code = errorCode(cause);
  return { text: message ? `加载失败：${message}${code ? `（${code}）` : ""}` : "加载失败", retry: true };
}
