/**
 * 当前角色能不能编辑 3D 导演台的场景（DR-F-156）：界面与 store 共用这一个判断。
 *
 * ⚠️ 只是调用 `roleAllowsScope(role, 编辑)`：那是现有前端里唯一一份与后端角色矩阵逐列一致的判断
 * （CLAUDE.md §5.17「角色矩阵只有一份」），不另写 `role === "viewer"`。拿不到角色时按不能编辑处理。
 * ⚠️ 单独成一个小模块，不放在 `directorScene.ts`：画布上的来源角标也要用它，而画布页与懒加载的编辑器共用的模块
 *    会整个落进画布页的首屏 chunk（连同编辑器才用的场景校验）—— 包体积检查（DRT-09）在批次 4 因此超限。
 */
import type { WorkspaceRole } from "./agentState";
import { roleAllowsScope } from "./mcpScopes";

export function directorCanEdit(role: WorkspaceRole | null | undefined): boolean {
  return roleAllowsScope(role ?? undefined, "seeaitv:edit");
}
