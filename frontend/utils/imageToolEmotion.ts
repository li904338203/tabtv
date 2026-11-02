/**
 * 情绪调节编辑器（docs/agent/imagetools IMGT-17，IMG-F-10~22）专用的纯函数：角色增删改、坐标盘键盘移动、
 * 节点参数还原、源图变化判定、脸部小图裁切区域、能力校验用的选项。
 *
 * 方案（2026-10-31 变更）：**只有手动框选**，没有自动人物识别；服务端把框选区域裁出来发给图片模型（img2img），
 * 再羽化贴回原图 —— 输出就是原图分辨率，所以也没有分辨率选项。
 *
 * 通用的那一部分（情绪格互查、显示 ↔ 原图坐标、只发 touched、角色上限）在 `utils/imageTools.ts`，这里只放
 * 编辑器自己的状态推导，不碰 DOM、不发请求。
 * ⚠️ 导出名一律带 `imageToolEmotion` 前缀并显式 import（Nuxt 自动导入重名时会悄悄顶替，CLAUDE.md §5.28）。
 */
import {
  imageToolRoleLimitReason,
  type ImageToolEditorRole,
  type ImageToolExpressionOptions,
  type ImageToolPixelBox
} from "./imageTools";

/** 坐标盘的取值范围（arousal / warmth 都是 -2..2）。 */
export const IMAGE_TOOL_EMOTION_MIN = -2;
export const IMAGE_TOOL_EMOTION_MAX = 2;

/** 编辑器的完整状态（角色比请求多一个 touched 界面字段）。 */
export type ImageToolEmotionState = {
  count: number;
  roles: ImageToolEditorRole[];
};

function clampAxis(value: number) {
  return Math.min(IMAGE_TOOL_EMOTION_MAX, Math.max(IMAGE_TOOL_EMOTION_MIN, Math.round(value)));
}

/**
 * 坐标盘的方向键（IMG-F-14，可键盘操作）：上 = 更激动、下 = 更平静、左 = 更亲近、右 = 更疏离，
 * Home 回中心「淡然自若」。到边不绕回（绕回会让「再按一下右」从最疏离跳到最亲近）。
 * 不是方向键返回 null（调用方不 preventDefault，Tab 等照常工作）。
 */
export function imageToolEmotionMove(current: { arousal: number; warmth: number }, key: string): { arousal: number; warmth: number } | null {
  switch (key) {
    case "ArrowUp": return { arousal: clampAxis(current.arousal + 1), warmth: current.warmth };
    case "ArrowDown": return { arousal: clampAxis(current.arousal - 1), warmth: current.warmth };
    case "ArrowLeft": return { arousal: current.arousal, warmth: clampAxis(current.warmth + 1) };
    case "ArrowRight": return { arousal: current.arousal, warmth: clampAxis(current.warmth - 1) };
    case "Home": return { arousal: 0, warmth: 0 };
    default: return null;
  }
}

/** 给一个角色设情绪坐标：同时标 touched（IMG-F-15：点过坐标盘才进请求）。找不到角色原样返回。 */
export function imageToolEmotionSetRole(roles: readonly ImageToolEditorRole[], id: string, arousal: number, warmth: number): ImageToolEditorRole[] {
  return roles.map(role => role.id === id ? { ...role, arousal: clampAxis(arousal), warmth: clampAxis(warmth), touched: true } : role);
}

/**
 * 手动框选新增一个角色（IMG-F-12 / F-22）：超上限返回说明、不加；新角色**未调整**（坐标 0,0）。
 * id 用 `manual-` 前缀 + 调用方给的序号（从节点参数还原出来的角色也是这个前缀，序号带时间戳不会撞上）。
 */
export function imageToolEmotionAddRole(
  roles: readonly ImageToolEditorRole[],
  box: ImageToolPixelBox,
  id: string,
  maxRoles?: number
): { roles: ImageToolEditorRole[]; error: string | null; id?: string } {
  const error = imageToolRoleLimitReason(roles.length, maxRoles);
  if (error) return { roles: [...roles], error };
  const roleId = id.startsWith("manual-") ? id : `manual-${id}`;
  return { roles: [...roles, { id: roleId, box: { ...box }, arousal: 0, warmth: 0, touched: false }], error: null, id: roleId };
}

/** 删掉一个角色（框错了时用，IMG-F-12）。 */
export function imageToolEmotionRemoveRole(roles: readonly ImageToolEditorRole[], id: string): ImageToolEditorRole[] {
  return roles.filter(role => role.id !== id);
}

/** 按从左到右的顺序排好的角色（角色条的显示顺序与「角色N」的编号一致）。 */
export function imageToolEmotionOrdered(roles: readonly ImageToolEditorRole[]): ImageToolEditorRole[] {
  return [...roles].sort((a, b) => a.box.x - b.box.x || a.box.y - b.box.y);
}

/**
 * 删掉或新增后该选中谁：原来选中的还在就不动，否则选从左到右的第一个；一个都没有返回空串。
 */
export function imageToolEmotionPickSelected(roles: readonly ImageToolEditorRole[], selectedId: string): string {
  if (selectedId && roles.some(role => role.id === selectedId)) return selectedId;
  return imageToolEmotionOrdered(roles)[0]?.id ?? "";
}

/**
 * 节点上存的 `params.toolOptions` → 编辑器状态（node 模式，IMG-F-21：子节点的面板带着上次的选择）。
 *
 * 存下来的只有**被调整过**的角色（请求形状，只发 touched），还原后都是已调整的手动框。
 * 老节点里残留的 `resolution` 忽略（输出就是原图分辨率）。
 * 坏值（非数字、越界）夹到范围内或丢弃，不让一条脏数据把面板打不开。
 */
export function imageToolEmotionRestore(options: unknown): ImageToolEmotionState {
  const value = (options && typeof options === "object" ? options : {}) as Partial<ImageToolExpressionOptions> & { roles?: unknown };
  const count = imageToolEmotionCount(value.count);
  const roles: ImageToolEditorRole[] = [];
  for (const raw of Array.isArray(value.roles) ? value.roles : []) {
    const role = raw as { id?: unknown; box?: Partial<ImageToolPixelBox>; arousal?: unknown; warmth?: unknown };
    const box = role?.box;
    if (!box || ![box.x, box.y, box.w, box.h].every(item => typeof item === "number" && Number.isFinite(item))) continue;
    roles.push({
      id: typeof role.id === "string" && role.id ? role.id : `manual-${roles.length + 1}`,
      box: { x: box.x!, y: box.y!, w: box.w!, h: box.h! },
      arousal: clampAxis(Number(role.arousal) || 0),
      warmth: clampAxis(Number(role.warmth) || 0),
      touched: true
    });
  }
  return { count, roles };
}

/** 张数夹到 1~4（IMG-F-17）。 */
export function imageToolEmotionCount(value: unknown): number {
  const count = Math.round(Number(value));
  return Number.isFinite(count) ? Math.min(4, Math.max(1, count)) : 1;
}

/**
 * 源图变了没有（IMG-F-21）：节点上记的源图（`toolOptions.sourceMediaId`，缺省退回 `params.toolSourceMediaId`）
 * 与当前上游产物不同 → 框作废。任一侧未知时**不判变化**（拿不准就不清掉用户的框）。
 */
export function imageToolEmotionSourceChanged(params: Record<string, unknown> | undefined, currentMediaId: string | undefined): boolean {
  const options = params?.toolOptions as { sourceMediaId?: unknown } | undefined;
  const stored = typeof options?.sourceMediaId === "string" && options.sourceMediaId
    ? options.sourceMediaId
    : typeof params?.toolSourceMediaId === "string" ? params.toolSourceMediaId : "";
  return Boolean(stored && currentMediaId && stored !== currentMediaId);
}

/**
 * 脸部小图的裁切区域（IMGD-29，在**已取到的源图**上裁，不再请求服务端）：
 * - 框是**原图像素**，取到的可能是缩略图 → 按 `loaded / natural` 换算；
 * - 竖长的框多半是半身 / 全身，脸在上部：取宽的 70% 为边长、贴着顶部居中；
 * - 接近方形的框当作脸框：取短边为边长、居中；
 * - 结果夹到图内；边长 < 2px（尺寸未知、框无效）返回 null。
 */
export function imageToolEmotionFaceCrop(
  box: ImageToolPixelBox,
  natural: { width: number; height: number },
  loaded: { width: number; height: number }
): { sx: number; sy: number; side: number } | null {
  if (!(natural.width > 0 && natural.height > 0 && loaded.width > 0 && loaded.height > 0)) return null;
  const ratio = loaded.width / natural.width;
  const tall = box.h > box.w * 1.3;
  const sideNatural = tall ? box.w * 0.7 : Math.min(box.w, box.h);
  const cx = box.x + box.w / 2;
  const top = tall ? box.y + box.h * 0.02 : box.y + (box.h - sideNatural) / 2;
  let side = sideNatural * ratio;
  side = Math.min(side, loaded.width, loaded.height);
  if (!(side >= 2)) return null;
  const sx = Math.min(loaded.width - side, Math.max(0, cx * ratio - side / 2));
  const sy = Math.min(loaded.height - side, Math.max(0, top * ratio));
  return { sx, sy, side };
}

/** 发送前置条件的说明（IMG-F-18）；满足时返回空串。 */
export function imageToolEmotionInputHint(roles: readonly ImageToolEditorRole[]): string {
  if (!roles.length) return "先在画面上框选要调整表情的人";
  return roles.some(role => role.touched) ? "" : "先选一个角色的情绪";
}

/**
 * 能力校验（报价接口）用的选项：只带调整过（touched）的角色，与发送同形。
 * 一个调整过的角色都没有返回 null（不校验 —— 发送按钮此时由「先选一个角色的情绪」禁用）。
 * tabtv 不计价（TT-T-14）：来源系统「没有调整过时借第一个角色的框，只为算出积分显示」的做法删除。
 */
export function imageToolEmotionQuoteOptions(sourceMediaId: string, state: ImageToolEmotionState): ImageToolExpressionOptions | null {
  if (!sourceMediaId || !state.roles.length) return null;
  const touched = state.roles.filter(role => role.touched);
  if (!touched.length) return null;
  const roles = touched.map(role => ({
    id: role.id,
    box: { ...role.box },
    arousal: role.arousal,
    warmth: role.warmth
  }));
  return { sourceMediaId, count: imageToolEmotionCount(state.count), roles };
}

/**
 * 发送 / 写回节点用的选项：保留 touched 字段交给 `imageToolWireOptions` 去过滤 —— 这里只发 touched 的，
 * 存进节点的也只有 touched 的（与请求同形，整图运行按节点参数执行时不会把没调整过的人也改掉）。
 */
export function imageToolEmotionOptions(sourceMediaId: string, state: ImageToolEmotionState): ImageToolExpressionOptions {
  return {
    sourceMediaId,
    count: imageToolEmotionCount(state.count),
    roles: state.roles.filter(role => role.touched).map(role => ({ id: role.id, box: { ...role.box }, arousal: role.arousal, warmth: role.warmth }))
  };
}

/** 示意头像的 SVG 地址（当前产物是程序化生成的 SVG；`imageToolEmotionImage` 给的 webp 取不到时退回它）。 */
export function imageToolEmotionHeadFallback(arousal: number, warmth: number) {
  return `/imagetools/emotion/${clampAxis(arousal)}_${clampAxis(warmth)}.svg`;
}
