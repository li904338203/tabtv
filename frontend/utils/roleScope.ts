import type { WorkspaceRole } from "~/utils/agentState";

/**
 * 工作区角色能不能做某类动作（与后端 `collaboration/domain` 角色矩阵的 view / edit / generate / export 一致）。
 *
 * 来源系统把它放在 MCP 同意页的工具文件里（`mcpScopes.ts`）；MCP 删除后（TT-T-13）挪到这里，
 * 画布图片工具条、智能剪辑、导演台的只读判断照常用它。本机用户恒为 owner，判断结果恒为允许。
 */
export type RoleScope = "seeaitv:view" | "seeaitv:edit" | "seeaitv:generate" | "seeaitv:export" | string;

export function roleAllowsScope(role: WorkspaceRole | undefined, scope: RoleScope): boolean {
  if (scope === "seeaitv:view") return Boolean(role);
  return role === "owner" || role === "admin" || role === "editor";
}
