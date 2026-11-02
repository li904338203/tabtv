/**
 * 设置页的「内置 / 已修改」标识（TT-T-19~21，内置版本规则见 TT-T-24）。
 *
 * - 技能与图片预设：行上有 `isBuiltin`（随包内置）与 `isUserModified`（在设置页改过）两个标志。
 *   改过的内置条目**启动时不再被内置内容覆盖**，所以要明确告诉使用者「这一条现在是你的版本」，
 *   并给出「恢复内置版本」的出口。
 * - 提示词：没有覆盖行（或覆盖行已归档）时生效的是代码内置原文；有已发布的覆盖行即「已修改」。
 *   只有草稿没有发布时线上仍是内置那份 —— 说成「已修改」会让人以为改动已经生效。
 */
import type { PromptRow, PromptSpec } from "./promptForm";

export type BuiltinState = "builtin" | "modified" | "custom";

export interface BuiltinBadge {
  state: BuiltinState;
  label: string;
  /** element-plus 的 tag 类型。 */
  type: "info" | "warning" | "success";
  /** 能不能「恢复内置版本」。 */
  restorable: boolean;
}

const BADGES: Record<BuiltinState, BuiltinBadge> = {
  builtin: { state: "builtin", label: "内置", type: "info", restorable: false },
  modified: { state: "modified", label: "内置 · 已修改", type: "warning", restorable: true },
  custom: { state: "custom", label: "自建", type: "success", restorable: false }
};

/** 技能 / 预设的标识。 */
export function builtinBadge(row: { isBuiltin?: unknown; isUserModified?: unknown }): BuiltinBadge {
  if (row.isBuiltin !== true) return BADGES.custom;
  return row.isUserModified === true ? BADGES.modified : BADGES.builtin;
}

/**
 * 「恢复内置版本」按钮的可见性：只有内置条目才有。没改过的内置条目也允许点（幂等），
 * 但列表上只在「已修改」时突出显示。
 */
export function canRestore(row: { isBuiltin?: unknown }): boolean {
  return row.isBuiltin === true;
}

/** 提示词用途的标识：看通用版与各模型专属版里有没有已发布的覆盖行。 */
export function promptBadge(spec: Pick<PromptSpec, "code" | "hasBuiltin">, rows: readonly PromptRow[]): BuiltinBadge {
  const live = rows.filter(r => r.code === spec.code && r.status === "published");
  if (!spec.hasBuiltin) return BADGES.custom;
  return live.length ? BADGES.modified : BADGES.builtin;
}
