import { applyImagePreset, asObject, presetCodeOf, type PresetLike } from "./canvas";
import type { JsonObject } from "~/types/canvas";

/**
 * 节点参数面板的版式：哪个参数放顶部一行、哪个收进「尺寸」弹窗、哪个在底部栏、按什么顺序。
 *
 * ⚠️ 画布节点（`ParamPanel`）与组子项（`GroupImageNodePanel`）**共用这一份**。
 *    各写一份的话，同一个参数在画布上收进了弹窗、在组子项里还平铺着 —— 两边各自看起来都"没错"。
 *
 * 版式照老系统（已评审定稿，老库 `node_param` 的 scope/sort）：
 *   图片：顶部「参考」；底部「模型 → 尺寸（画质 · 比例 · 数量）→ 图片预设」
 *   视频：顶部「参考 · 标记 · 特效 · 角色库 · 运镜」；底部「模型 → 生成模式 → 尺寸（清晰度 · 比例 · 时长）」
 *
 * 数据来自新后端：分区看 `group`（`main` / `format` / `advanced`），「规格」组就是老系统的「尺寸」弹窗。
 */
export type LayoutParam = {
  code?: string;
  group?: string;
  sort?: number;
  config?: unknown;
};

export type ParamLayout<T> = {
  /** 顶部一行（参考上传等）。 */
  top: T[];
  /** 底部栏，已按老系统的顺序排好。 */
  bottom: T[];
  /** 收进「尺寸」弹窗的「规格」项。 */
  size: T[];
  /** 「展开参数」里的高级项（反向提示词、随机种子）。 */
  advanced: T[];
  /** 尺寸弹窗插在这个参数之前；空串表示放在底部栏末尾。 */
  sizeBefore: string;
};

/**
 * 参数在画布 `params` 里的**键**。
 *
 * ⚠️ 视频模式在新后端的参数代号是 `video_mode`，而前端判断「这个视频节点走什么操作」
 *    （`operationForNode`、连线时的模式推断、生成前的输入校验……十来处）读的都是
 *    **`params.mode`**，模板与老画布数据里写的也是它。模式下拉写 `video_mode` 的话，
 *    **手动切换模式写进去了却不生效** —— 操作仍按 `mode` 算，而连线时自动设置模式的那条路
 *    不受影响，所以不容易发现。映射只在这一处。
 */
export function paramKey(code: string | undefined): string {
  const value = code ?? "";
  return value === "video_mode" ? "mode" : value;
}

/** ⚠️ 老后端这个参数叫 `mode`，新后端叫 **`video_mode`** —— 两个都认。 */
export function isVideoModeCode(code: string | undefined): boolean {
  return code === "mode" || code === "video_mode";
}

/**
 * 后端**没有任何读取方**的参数，不渲染。
 *
 * ⚠️ 风格/特效进提示词只有一条路：连一个素材节点（输入 `kind=material`，
 *    服务端按素材码拼词，见 `canvas/app/resolve.go`）。`style_ref` / `effect_ref`
 *    写进 params 之后**没有任何代码读它** —— 渲染出来就是「选了等于没选」（GAP-01 同形），
 *    用户选了风格、扣了积分，出来的图与风格毫无关系。老系统图片节点顶部那个「风格」按钮
 *    也是空动作（`openStylePicker` 没有处理函数）。
 */
export const UNREAD_PARAM_CODES: ReadonlySet<string> = new Set(["style_ref", "effect_ref"]);

const TOP_CODES: Record<string, ReadonlySet<string>> = {
  image: new Set(["reference"]),
  video: new Set(["reference", "mark", "effectLib", "roleLib", "camera"])
};

/** 在这类节点上没有落点的参数（运镜只对视频有意义）。 */
const HIDDEN_CODES: Record<string, ReadonlySet<string>> = {
  image: new Set(["camera"])
};

/** 只有这两类节点有「尺寸」弹窗（老系统同）；音频的语速等仍在底部栏。 */
const SIZE_DIALOG_NODES: ReadonlySet<string> = new Set(["image", "video"]);

/**
 * 参数分区。⚠️ 老后端用 `scope`（`top`/`inline`），新后端用 **`group`** ——
 * 读错字段不报错，`group` 恒为 undefined，全部参数都会平铺到底部栏上。
 * 配置可以比前端新：没有 `group` 时按 `config.advanced` 兜底，再退回 `main`。
 */
export function paramGroupOf(item: LayoutParam): string {
  if (item.group) return item.group;
  return asObject(item.config).advanced ? "advanced" : "main";
}

/**
 * 底部栏的顺序照老系统：**模型 → 生成模式 → 尺寸弹窗 → 图片预设 → 其余**。
 *
 * 后端的 `sort` 管的是参数本身的次序（图片预设排在模型前面），底部栏的版式是老系统定稿的 ——
 * 两件事分开。为了排版去改参数的 sort，会连带影响别处按 sort 渲染的地方。
 */
function bottomRank(code: string): number {
  if (code === "model") return 0;
  if (isVideoModeCode(code)) return 1;
  return 3; // 尺寸弹窗占 2
}

export function nodeParamLayout<T extends LayoutParam>(
  nodeType: string,
  params: readonly T[],
  options: { exclude?: readonly string[] } = {}
): ParamLayout<T> {
  const exclude = new Set(options.exclude ?? []);
  const hidden = HIDDEN_CODES[nodeType];
  const top = TOP_CODES[nodeType];
  const layout: Omit<ParamLayout<T>, "sizeBefore"> = { top: [], bottom: [], size: [], advanced: [] };

  for (const item of params) {
    const code = item.code ?? "";
    // prompt 由面板里单独那个 textarea 渲染（control=`text` 本就不在 CONTROL_MAP 里）。
    if (code === "prompt" || code === "text") continue;
    if (UNREAD_PARAM_CODES.has(code) || exclude.has(code) || hidden?.has(code)) continue;
    const group = paramGroupOf(item);
    if (group === "advanced") layout.advanced.push(item);
    else if (top?.has(code)) layout.top.push(item);
    // 带 domainFrom 的（素材库等）是另一类选择器，不进尺寸弹窗。
    else if (SIZE_DIALOG_NODES.has(nodeType) && group === "format" && !asObject(item.config).domainFrom) {
      layout.size.push(item);
    } else layout.bottom.push(item);
  }

  const bySort = (a: T, b: T) => (a.sort ?? 0) - (b.sort ?? 0);
  layout.bottom.sort((a, b) => bottomRank(a.code ?? "") - bottomRank(b.code ?? "") || bySort(a, b));
  layout.size.sort(bySort);
  const after = layout.size.length ? layout.bottom.find(item => bottomRank(item.code ?? "") > 2) : undefined;
  return { ...layout, sizeBefore: after?.code ?? "" };
}

/**
 * 一个正整数参数（数量、时长）。
 *
 * ⚠️ 新后端的选项值**一律是字符串**（`"4"`、`"10"`），而受理请求顶层的 `count` /
 *    `durationSec` 要的是数字。此前按 `typeof === "number"` 判，字符串一律退回默认值 ——
 *    选「4 张」**按 1 张扣费、上游却出 4 张**（上游读的是 `params.count`），
 *    选「10 秒」按 5 秒扣费。两边各自都"没错"，而账对不上。
 */
export function positiveIntParam(value: unknown): number | undefined {
  const parsed = typeof value === "number" ? value : typeof value === "string" ? Number(value.trim()) : Number.NaN;
  if (!Number.isFinite(parsed) || parsed < 1) return undefined;
  return Math.floor(parsed);
}

/**
 * 受理/报价请求顶层的数量与时长 —— 必须与 `params` 里发给上游的是**同一个数**。
 * 服务端按顶层字段计价（`per_image` × count、`per_second` × durationSec），
 * 上游按 `params.count` / `params.duration` 出片。
 */
export function generationQuantities(nodeType: string, params: Record<string, unknown>) {
  const count = positiveIntParam(params.count) ?? 1;
  const durationSec = nodeType === "video" ? positiveIntParam(params.duration) : undefined;
  return durationSec ? { count, durationSec } : { count };
}

/**
 * 按上游类型给新节点补默认参数（DRD-45、DR-F-93）：取目标节点类型每个参数的
 * `config.defaultByUpstream[上游类型]`，写进 params。
 *
 * 只用于**从上游拖出来新建**的节点（`createConnectedNode`）；已有节点连上时不改参数，只给建议条 ——
 * 用户已经选好的东西不该因为多连了一条线就被换掉。
 *
 * - 预设码与模式值都来自配置，代码里一个都不写死（运营换一个配方只改 seed / 后台）；
 * - 键走 `paramKey`：`video_mode` 写到 **`mode`**，写错键的话下拉看着变了，实际不生效；
 * - 图片预设要**整套**落下（预设码 + 预设自带的画幅 + 各维度默认档），与在选择器里选中它一样；
 *   ⚠️ 配置的预设已下线、目录里查不到时跳过：写一个查不到的预设码，服务端按「无预设」处理，选择器上却显示选中了。
 */
/** 一个参数按上游类型配置的默认值（`config.defaultByUpstream[上游类型]`）；没配时为空串。建议条用它说「建议用什么」。 */
export function upstreamDefaultOf(params: readonly LayoutParam[] | undefined, code: string, sourceType: string): string {
  const param = (params ?? []).find(item => item.code === code || paramKey(item.code) === code);
  const value = asObject(asObject(param?.config).defaultByUpstream)[sourceType];
  return typeof value === "string" ? value : "";
}

export function applyDefaultByUpstream(
  params: JsonObject,
  targetType: { params?: readonly LayoutParam[] } | undefined,
  sourceType: string,
  presets: readonly PresetLike[] = []
): JsonObject {
  let result = params;
  for (const param of targetType?.params ?? []) {
    const value = asObject(asObject(param.config).defaultByUpstream)[sourceType];
    if (value === undefined || value === null || value === "") continue;
    if (param.code === "preset") {
      const preset = presets.find(item => presetCodeOf(item) === String(value));
      if (preset) result = applyImagePreset(result, preset);
      continue;
    }
    const key = paramKey(param.code);
    if (key) result = { ...result, [key]: value };
  }
  return result;
}
