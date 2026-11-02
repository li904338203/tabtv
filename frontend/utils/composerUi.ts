/**
 * 统一输入框的展示用纯函数（输入框改版：大圆角卡片 + 三组工具栏 + 两个浮层）。
 *
 * 只管「长什么样」：文案拆分、画幅图标尺寸、模型角标与说明行、模式下拉的标签与说明。
 * 数据仍然来自原处（`agentPrefs` store），这里不取数。tabtv 不计价（TT-T-14），不显示价格与会员角标。
 */
import { prefModel, type AgentPrefsValue } from "~/utils/agentPrefs";
import type { ModelOption } from "~/utils/composerPrice";
import type { ComposerMode } from "~/utils/quickGenerate";

/**
 * 画幅分段按钮里的小矩形尺寸：长边固定为 `max`，短边按比例（至少 4px，免得 21:9 缩成一条线）。
 * 解析不出来（如「智能」「auto」）给正方形 —— 图标只是示意，不能因为值不规范就不显示。
 */
export function ratioIconBox(value: string, max = 14): { width: number; height: number } {
  const match = /^\s*(\d+(?:\.\d+)?)\s*[:：xX×]\s*(\d+(?:\.\d+)?)\s*$/.exec(value);
  const w = match ? Number(match[1]) : 0;
  const h = match ? Number(match[2]) : 0;
  if (!(w > 0 && h > 0)) return { width: max, height: max };
  const min = 4;
  if (w >= h) return { width: max, height: Math.max(min, Math.round((max * h) / w)) };
  return { width: Math.max(min, Math.round((max * w) / h)), height: max };
}

/**
 * 「模型偏好」胶囊的两段文案：前段是所选视频模型名（未选为「智能匹配模型」），后段固定「创作偏好」。
 * 拆开是为了窄宽度下只收起后段，模型名始终看得见。所选模型不在目录里时显示编码而不是空白。
 */
export function prefTriggerParts(
  prefs: AgentPrefsValue | null | undefined,
  videoOptions: Array<{ code: string; name: string }>
): { model: string; tail: string; custom: boolean } {
  const code = prefModel(prefs, "video");
  if (!code) return { model: "智能匹配模型", tail: "创作偏好", custom: false };
  return { model: videoOptions.find(option => option.code === code)?.name ?? code, tail: "创作偏好", custom: true };
}

/**
 * 创造页直接生成的同一个胶囊：前段是当前模式所选的模型名，没选（模型还没取到 / 当前模式没有模型）
 * 时为「自定义模型」；后段固定「创作偏好」。所选模型不在目录里时显示编码而不是空白。
 */
export function directTriggerParts(
  modelCode: string | undefined,
  options: Array<{ code: string; name: string }>
): { model: string; tail: string; custom: boolean } {
  if (!modelCode) return { model: "自定义模型", tail: "创作偏好", custom: false };
  return { model: options.find(option => option.code === modelCode)?.name || modelCode, tail: "创作偏好", custom: true };
}

/**
 * 模型角标：**只显示数据里真有的**。来源系统唯一的角标是会员门槛；tabtv 没有会员（TT-T-14），
 * 所以目前没有角标 —— 不编 NEW / 限时优惠。保留函数是为了角标位置与数据流不变。
 */
export function modelBadges(_model: { minMemberLevelId?: string | null }): string[] {
  return [];
}

/**
 * 模型下面那行灰字：不可用写原因（服务端原话）；可用时没有说明（tabtv 不显示单价，目录里也没有说明字段）。
 */
export function modelOptionNote(option: ModelOption, _perTurn = false): string {
  if (!option.available) return `不可用：${option.reason || "暂时不可用"}`;
  return "";
}

/** 模式下拉每一项的小标签与说明（选中项下方那行灰字）。没有标签的不放。 */
export const MODE_META: Record<ComposerMode, { tag?: string; description: string }> = {
  agent: { tag: "分钟级成片", description: "描述你的想法，Agent 帮你完善创意并自动调用合适的模型" },
  video: { description: "一句话或参考图，直接生成一段视频" },
  image: { description: "文字或参考图，直接生成图片" },
  audio: { description: "选择音色，把文字转成语音" }
};
