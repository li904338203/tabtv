/**
 * 图片预设编辑的纯规则（TT-T-20）。来源系统管理端对预设只读，这里的写规则与服务端
 * `catalog/app/preset_admin.go` 的 `validatePreset` 逐条对应（服务端保存时再验一次，以它为准）。
 *
 * ⚠️ 配方提示词只在服务端拼进生成请求（`/canvas/config` 不下发它）；设置页能看能改，但预览也走服务端
 *    （`POST /admin/image-presets/preview`）—— 「没选的维度按 default 出词」这条规则只有一份。
 * ⚠️ 没有删除：历史节点里存着 presetCode，下线用停用。
 */
export type PresetGroup = "sheet" | "tuning" | "storyboard" | "space" | "tool";

/** 分组按**工作顺序**不按字典序：先定角色/场景设定，再调质感，再排分镜，最后空间机位；加工工具不进选择器。 */
export const PRESET_GROUPS: Array<{ value: PresetGroup; label: string }> = [
  { value: "sheet", label: "设定图" },
  { value: "tuning", label: "质感调节" },
  { value: "storyboard", label: "分镜叙事" },
  { value: "space", label: "空间与机位" },
  { value: "tool", label: "加工工具（不进选择器）" }
];

export const PRESET_SPECS: Array<{ value: string; label: string }> = [
  { value: "sheet", label: "设定图" },
  { value: "sequence", label: "序列" },
  { value: "variation", label: "变体" },
  { value: "retouch", label: "修饰" },
  { value: "panorama", label: "全景" }
];

export const PRESET_INPUT_MODES: Array<{ value: string; label: string }> = [
  { value: "image_required", label: "必须有图" },
  { value: "text_required", label: "必须有描述" },
  { value: "either", label: "图或描述其一" }
];

export const PRESET_CODE_RE = /^[a-z][a-z0-9_]{1,63}$/;
export const PRESET_NAME_MAX = 64;
export const PRESET_DESC_MAX = 300;

type Row = Record<string, unknown>;

export interface PresetDraft {
  code: string;
  name: string;
  presetGroup: string;
  spec: string;
  inputMode: string;
  icon: string;
  operation: string;
  prompt: string;
  /** 参数默认（JSON 对象文本）。 */
  paramsText: string;
  /** 调节维度（JSON 数组文本，空 = 没有维度）。 */
  dimensionsText: string;
  description: string;
  isNew: boolean;
  sort: number;
  isEnabled: boolean;
  isBuiltin: boolean;
  isUserModified: boolean;
}

const str = (v: unknown) => (v === undefined || v === null ? "" : String(v));
const pretty = (v: unknown, empty: string) => (v === undefined || v === null ? empty : JSON.stringify(v, null, 2));

export function presetDraft(row: Row | null, nextSort = 10): PresetDraft {
  const dims = row?.dimensions;
  return {
    code: str(row?.code),
    name: str(row?.name),
    presetGroup: str(row?.presetGroup) || "tuning",
    spec: str(row?.spec) || "retouch",
    inputMode: str(row?.inputMode) || "either",
    icon: str(row?.icon),
    operation: str(row?.operation),
    prompt: str(row?.prompt),
    paramsText: pretty(row?.params, "{}"),
    dimensionsText: Array.isArray(dims) && dims.length ? JSON.stringify(dims, null, 2) : "",
    description: str(row?.description),
    isNew: row?.isNew === true,
    sort: typeof row?.sort === "number" ? (row.sort as number) : nextSort,
    isEnabled: row ? row.isEnabled === true : true,
    isBuiltin: row?.isBuiltin === true,
    isUserModified: row?.isUserModified === true
  };
}

/** 解析参数默认：必须是 JSON 对象；空串按 `{}`。 */
export function parsePresetParams(text: string): { value: Row; error: string } {
  if (!text.trim()) return { value: {}, error: "" };
  try {
    const v = JSON.parse(text);
    if (typeof v !== "object" || v === null || Array.isArray(v)) return { value: {}, error: "参数默认必须是 JSON 对象" };
    return { value: v as Row, error: "" };
  } catch {
    return { value: {}, error: "参数默认不是合法的 JSON" };
  }
}

export interface PresetDimensionDraft { code: string; name?: string; default?: string; options: Array<{ value: string; label?: string; prompt?: string }> }

/** 解析调节维度：空串 = 没有维度；每维要有 code 与至少一个选项，default 必须是其中一个选项。 */
export function parsePresetDimensions(text: string): { value: PresetDimensionDraft[] | null; error: string } {
  if (!text.trim()) return { value: null, error: "" };
  let raw: unknown;
  try {
    raw = JSON.parse(text);
  } catch {
    return { value: null, error: "调节维度不是合法的 JSON" };
  }
  if (!Array.isArray(raw)) return { value: null, error: "调节维度必须是 JSON 数组" };
  for (const [i, d] of raw.entries()) {
    const dim = d as Row;
    if (!dim || typeof dim !== "object" || !str(dim.code).trim()) return { value: null, error: `第 ${i + 1} 个维度缺少 code` };
    const opts = Array.isArray(dim.options) ? (dim.options as Row[]) : [];
    if (!opts.length) return { value: null, error: `维度「${str(dim.code)}」至少要有一个选项` };
    if (opts.some(o => !o || !str(o.value).trim())) return { value: null, error: `维度「${str(dim.code)}」有选项缺少 value` };
    // ⚠️ default 不在选项里时服务端照样存得进去，但出词时这一维整段消失 —— 与「跳过这一维」是同一个缺陷
    if (dim.default !== undefined && dim.default !== "" && !opts.some(o => str(o.value) === str(dim.default))) {
      return { value: null, error: `维度「${str(dim.code)}」的默认值「${str(dim.default)}」不在选项里` };
    }
  }
  return { value: raw as PresetDimensionDraft[], error: "" };
}

/** 第一条要改的问题；没问题返回空串。 */
export function validatePresetDraft(d: PresetDraft, ctx: { creating: boolean; list: Row[]; operations?: string[] }): string {
  if (ctx.creating) {
    if (!PRESET_CODE_RE.test(d.code.trim())) return "编码要以小写字母开头，只含小写字母、数字、下划线，2~64 位";
    if (ctx.list.some(r => r.code === d.code.trim())) return `编码「${d.code.trim()}」已存在`;
  }
  if (!d.name.trim()) return "名称不能为空";
  if ([...d.name.trim()].length > PRESET_NAME_MAX) return `名称最多 ${PRESET_NAME_MAX} 字`;
  if (!PRESET_GROUPS.some(g => g.value === d.presetGroup)) return "请选择分组";
  if (!PRESET_SPECS.some(g => g.value === d.spec)) return "请选择产出类型";
  if (!PRESET_INPUT_MODES.some(g => g.value === d.inputMode)) return "请选择输入要求";
  if (!d.prompt.trim()) return "配方提示词不能为空";
  if ([...d.description].length > PRESET_DESC_MAX) return `说明最多 ${PRESET_DESC_MAX} 字`;
  if (d.operation && ctx.operations?.length && !ctx.operations.includes(d.operation)) return `操作「${d.operation}」不存在`;
  const p = parsePresetParams(d.paramsText);
  if (p.error) return p.error;
  const dims = parsePresetDimensions(d.dimensionsText);
  if (dims.error) return dims.error;
  return "";
}

/** 表单 → 请求体（先过 validatePresetDraft）。 */
export function presetPayload(d: PresetDraft): Row {
  return {
    code: d.code.trim(),
    name: d.name.trim(),
    presetGroup: d.presetGroup,
    spec: d.spec,
    inputMode: d.inputMode,
    icon: d.icon.trim(),
    operation: d.operation.trim(),
    prompt: d.prompt,
    params: parsePresetParams(d.paramsText).value,
    dimensions: parsePresetDimensions(d.dimensionsText).value,
    description: d.description.trim(),
    isNew: d.isNew,
    sort: d.sort
  };
}

/** 按分组（工作顺序）归组；认不出的分组放最后，不丢掉。 */
export function groupPresets<T extends { presetGroup?: unknown; sort?: unknown }>(rows: readonly T[]): Array<{ group: string; label: string; items: T[] }> {
  const order = PRESET_GROUPS.map(g => g.value as string);
  const groups = new Map<string, T[]>();
  for (const r of rows) {
    const g = str(r.presetGroup);
    if (!groups.has(g)) groups.set(g, []);
    groups.get(g)!.push(r);
  }
  const rank = (g: string) => (order.includes(g) ? order.indexOf(g) : order.length);
  return [...groups.entries()]
    .sort((a, b) => rank(a[0]) - rank(b[0]) || a[0].localeCompare(b[0]))
    .map(([group, items]) => ({
      group,
      label: PRESET_GROUPS.find(g => g.value === group)?.label ?? group,
      items: [...items].sort((a, b) => Number(a.sort ?? 0) - Number(b.sort ?? 0))
    }));
}

/** 导出一条预设：去掉内置 / 修改 / 启用这些本机状态。 */
export function presetExportItem(row: Row): Row {
  const { isBuiltin: _b, isUserModified: _m, isEnabled: _e, ...rest } = row;
  return rest;
}
