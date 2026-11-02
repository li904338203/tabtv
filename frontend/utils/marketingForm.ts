/**
 * 营销选项 / 分类 / 预置角色的编辑规则（TT-T-21，移植自来源系统管理端
 * `utils/marketingOptionForm.ts`、`utils/marketingCharacterForm.ts` 的核心部分）。
 *
 * 服务端保存与启用时还会完整校验一次（`details.issues[]` 点名字段），这里只为边写边提示。
 * ⚠️ 没有删除：历史会话与生成快照按 (code, 版本) 引用选项，下线用停用。
 * ⚠️ 新建一律停用：启用是单独一次操作（启用时服务端校验封面、片段、引用的分类 / 模型 / 技能）。
 * ⚠️ 提示词片段只在服务端拼接，用户端只看得到名称、说明与封面。
 * ⚠️ 参考音频「只存不发」：任何上游请求都不带它（服务端保证），这里只是存。
 */
export type SettingsMarketingKind = "creative" | "hook" | "style";

export const MARKETING_KINDS: Array<{ value: SettingsMarketingKind; label: string }> = [
  { value: "creative", label: "创意" },
  { value: "hook", label: "Hook" },
  { value: "style", label: "风格" }
];
export const marketingKindLabel = (k: string) => MARKETING_KINDS.find(x => x.value === k)?.label ?? k;

/** 输入槽（键只认这三个）。 */
export const MARKETING_SLOTS: Array<{ key: string; label: string }> = [
  { key: "product", label: "商品" },
  { key: "character", label: "角色" },
  { key: "source_video", label: "源视频" }
];

export const MARKETING_OUTPUT_TYPES = [
  { value: "video", label: "视频" },
  { value: "image", label: "图片" }
];

export const OPTION_CODE_RE = /^[a-z][a-z0-9_]{2,63}$/;
export const CATEGORY_CODE_RE = /^[a-z][a-z0-9_]{1,47}$/;
export const MARKETING_MAX_NAME = 24;
export const MARKETING_MAX_DESCRIPTION = 60;
export const MARKETING_MAX_PROMPT = 1000;

/** 封面张数上限：创意 1~3，Hook / 风格 1。下限只在启用时强制。 */
export const marketingCoverLimit = (k: string) => (k === "creative" ? 3 : 1);
/** 推荐模型 / 推荐参数 / 默认技能只给创意。 */
export const marketingCreativeOnly = (k: string) => k === "creative";
/** 依赖出声只给创意与 Hook。 */
export const marketingOptionHasAudio = (k: string) => k !== "style";
export const marketingCharCount = (s: string) => [...s].length;

const LIFECYCLE: Record<string, { label: string; type: "info" | "success" | "warning" }> = {
  not_started: { label: "未开始", type: "info" },
  live: { label: "在投", type: "success" },
  ended: { label: "已结束", type: "warning" },
  disabled: { label: "已停用", type: "info" }
};
/** 投放状态由服务端按 `[开始, 结束)` 现算并下发，前端不拿本地时钟再算一遍。 */
export function marketingLifecycleTag(l: string) {
  return LIFECYCLE[l] ?? { label: l, type: "info" as const };
}

type Row = Record<string, unknown>;
const str = (v: unknown) => (v === undefined || v === null ? "" : String(v));
const strArr = (v: unknown) => (Array.isArray(v) ? v.map(String) : []);
const posInt = (v: unknown): number | null => (typeof v === "number" && Number.isInteger(v) && v > 0 ? v : null);

export interface OptionDraft {
  id: string;
  kind: SettingsMarketingKind;
  code: string;
  name: string;
  categoryCode: string;
  description: string;
  coverMediaIds: string[];
  previewMediaId: string;
  prompt: string;
  outputTypes: string[];
  /** 槽位 → required / optional；不认识的键原样保留（服务端会点名拒绝，而不是被前端悄悄丢掉）。 */
  inputRequirements: Record<string, string>;
  isAudioRequired: boolean;
  recommendedModels: string[];
  aspectRatio: string;
  durationMin: number | null;
  durationMax: number | null;
  /** 推荐参数里表单不认识的键（原样带回，不丢）。 */
  extraParams: Row;
  defaultSkillCode: string;
  tags: string[];
  /** 投放时段：ISO 串，空 = 不限。 */
  startAt: string;
  endAt: string;
  sort: number;
  isEnabled: boolean;
  version: number;
}

export function optionDraft(row: Row | null, kind: SettingsMarketingKind = "creative", nextSort = 10): OptionDraft {
  const params = { ...((row?.recommendedParams as Row | undefined) ?? {}) };
  const aspect = params.aspectRatio;
  const dur = Array.isArray(params.durationSec) ? params.durationSec : [];
  delete params.aspectRatio;
  delete params.durationSec;
  return {
    id: str(row?.id),
    kind: (row?.kind as SettingsMarketingKind) ?? kind,
    code: str(row?.code),
    name: str(row?.name),
    categoryCode: str(row?.categoryCode),
    description: str(row?.description),
    coverMediaIds: strArr(row?.coverMediaIds),
    previewMediaId: str(row?.previewMediaId),
    prompt: str(row?.prompt),
    outputTypes: row ? strArr(row.outputTypes) : ["video"],
    inputRequirements: { ...((row?.inputRequirements as Record<string, string> | undefined) ?? {}) },
    isAudioRequired: row?.isAudioRequired === true,
    recommendedModels: strArr(row?.recommendedModels),
    aspectRatio: typeof aspect === "string" ? aspect : "",
    durationMin: posInt(dur[0]),
    durationMax: posInt(dur[1]),
    extraParams: params,
    defaultSkillCode: str(row?.defaultSkillCode),
    tags: strArr(row?.tags),
    startAt: str(row?.startAt),
    endAt: str(row?.endAt),
    sort: typeof row?.sort === "number" ? (row.sort as number) : nextSort,
    isEnabled: row?.isEnabled === true,
    version: typeof row?.version === "number" ? (row.version as number) : 0
  };
}

/** 推荐参数 → 请求体（只有创意有）。空对象表示不推荐。 */
export function recommendedParamsOf(d: OptionDraft): Row {
  const out: Row = { ...d.extraParams };
  const aspect = (d.aspectRatio ?? "").trim();
  if (aspect) out.aspectRatio = aspect;
  const min = posInt(d.durationMin);
  const max = posInt(d.durationMax);
  if (min !== null && max !== null && max !== min) out.durationSec = [min, max];
  else if (min !== null) out.durationSec = [min];
  else if (max !== null) out.durationSec = [max];
  return out;
}

/** 表单 → 请求体。新建带 kind + code（建后不可改），修改带 version（乐观锁）。 */
export function optionPayload(d: OptionDraft, mode: "create" | "update"): Row {
  const body: Row = {
    name: d.name.trim(),
    categoryCode: d.categoryCode,
    description: d.description.trim(),
    coverMediaIds: [...d.coverMediaIds],
    previewMediaId: d.previewMediaId || null,
    // 片段原样提交（不 trim）：回读要与录入的逐字一致
    prompt: d.prompt,
    outputTypes: [...d.outputTypes],
    inputRequirements: { ...d.inputRequirements },
    tags: d.tags.map(t => t.trim()).filter(Boolean),
    startAt: d.startAt || null,
    endAt: d.endAt || null,
    sort: d.sort
  };
  if (marketingOptionHasAudio(d.kind)) body.isAudioRequired = d.isAudioRequired;
  if (marketingCreativeOnly(d.kind)) {
    body.recommendedModels = [...d.recommendedModels];
    body.recommendedParams = recommendedParamsOf(d);
    body.defaultSkillCode = d.defaultSkillCode || null;
  }
  if (mode === "create") {
    body.kind = d.kind;
    body.code = d.code.trim();
  } else {
    body.version = d.version;
  }
  return body;
}

/** 第一条挡保存的问题；没问题返回空串。封面不足不挡保存（启用时服务端再挡）。 */
export function optionIssue(d: OptionDraft, creating: boolean): string {
  if (creating && !OPTION_CODE_RE.test(d.code.trim())) return "code 须为小写字母开头的小写字母、数字、下划线，共 3~64 位";
  if (!d.name.trim()) return "名称不能为空";
  if (marketingCharCount(d.name) > MARKETING_MAX_NAME) return `名称最多 ${MARKETING_MAX_NAME} 字，当前 ${marketingCharCount(d.name)} 字`;
  if (!d.categoryCode.trim()) return "请选择分类";
  if (marketingCharCount(d.description) > MARKETING_MAX_DESCRIPTION) return `说明最多 ${MARKETING_MAX_DESCRIPTION} 字，当前 ${marketingCharCount(d.description)} 字`;
  if (marketingCharCount(d.prompt) > MARKETING_MAX_PROMPT) return `提示词片段最多 ${MARKETING_MAX_PROMPT} 字，当前 ${marketingCharCount(d.prompt)} 字`;
  if (!d.outputTypes.length) return "至少选一种适用产出（视频 / 图片）";
  if (d.startAt && d.endAt && Date.parse(d.endAt) <= Date.parse(d.startAt)) return "结束时间必须晚于开始时间";
  const max = marketingCoverLimit(d.kind);
  if (d.coverMediaIds.length > max) return `${marketingKindLabel(d.kind)}封面最多 ${max} 张，当前 ${d.coverMediaIds.length} 张`;
  if (marketingCreativeOnly(d.kind) && d.durationMin !== null && d.durationMax !== null && d.durationMax < d.durationMin) {
    return "推荐时长区间的下限不能大于上限";
  }
  return "";
}

/** 不挡保存的提醒。 */
export function optionNotes(d: OptionDraft): string[] {
  const out: string[] = [];
  if (!d.coverMediaIds.length) out.push(`还没有封面：草稿可以保存，启用前须上传（${marketingKindLabel(d.kind)} 1~${marketingCoverLimit(d.kind)} 张）`);
  return out;
}

/** 服务端返回的问题（`details.issues[]`），按「字段：原因」列出。 */
export function marketingIssuesOf(details: Record<string, unknown> | undefined): string[] {
  const raw = details?.issues;
  if (!Array.isArray(raw)) return [];
  return (raw as Row[]).map(x => (x.field ? `${str(x.field)}：${str(x.message)}` : str(x.message)));
}

/**
 * 导出一个选项：只留内容字段。封面 / 预览媒体 ID 只在本机有意义，导出时去掉，
 * 导入后需要重新上传封面（启用时服务端会挡下没封面的）。
 */
export function optionExportItem(row: Row): Row {
  const d = optionDraft(row);
  const body = optionPayload({ ...d, coverMediaIds: [], previewMediaId: "" }, "create");
  delete body.coverMediaIds;
  delete body.previewMediaId;
  return body;
}

/** 导入项 → 保存请求体：覆盖已有时保留它的封面；新建没有封面。 */
export function optionImportPayload(item: Row, current: Row | null): Row {
  const d = optionDraft({ ...item, coverMediaIds: current?.coverMediaIds ?? [], previewMediaId: current?.previewMediaId ?? null,
    version: current?.version }, (item.kind as SettingsMarketingKind) ?? "creative");
  return optionPayload(d, current ? "update" : "create");
}

// ---------------------------------------------------------------- 分类

export interface CategoryDraft { id: string; kind: SettingsMarketingKind; code: string; name: string; sort: number; isEnabled: boolean }

export function categoryIssue(c: CategoryDraft, creating: boolean, list: Row[]): string {
  if (creating) {
    if (!CATEGORY_CODE_RE.test(c.code.trim())) return "分类 code 须为小写字母开头的小写字母、数字、下划线，共 2~48 位";
    if (list.some(r => r.kind === c.kind && r.code === c.code.trim())) return `分类 code「${c.code.trim()}」已存在`;
  }
  if (!c.name.trim()) return "分类名称不能为空";
  if (marketingCharCount(c.name) > MARKETING_MAX_NAME) return `分类名称最多 ${MARKETING_MAX_NAME} 字`;
  return "";
}

// ---------------------------------------------------------------- 预置角色

export const CHARACTER_NAME_FORBIDDEN = "@<>/\\|:*?()（）";
export const MAX_CHARACTER_NAME = 50;
export const MARKETING_MAX_SUMMARY = 60;
export const MARKETING_MAX_APPEARANCE = 500;
export const MARKETING_MAX_VOICE_TEXT = 500;
export const MARKETING_MAX_IMAGES = 9;

export interface CharacterDraft {
  id: string;
  name: string;
  summary: string;
  imageMediaIds: string[];
  appearance: string;
  voiceText: string;
  /** 空串 = 没有参考音频。 */
  voiceMediaId: string;
  sort: number;
  isEnabled: boolean;
  version: number;
}

export function characterDraft(row: Row | null): CharacterDraft {
  return {
    id: str(row?.id),
    name: str(row?.name),
    summary: str(row?.summary),
    imageMediaIds: strArr(row?.imageMediaIds),
    appearance: str(row?.appearance),
    voiceText: str(row?.voiceText),
    voiceMediaId: str(row?.voiceMediaId),
    sort: typeof row?.sort === "number" ? (row.sort as number) : 0,
    isEnabled: row?.isEnabled === true,
    version: typeof row?.version === "number" ? (row.version as number) : 0
  };
}

/** 表单 → 请求体；音频空串发 null（= 不要音频）。 */
export function characterPayload(d: CharacterDraft, mode: "create" | "update"): Row {
  const body: Row = {
    name: d.name.trim(),
    summary: d.summary.trim(),
    imageMediaIds: [...d.imageMediaIds],
    appearance: d.appearance,
    voiceText: d.voiceText,
    voiceMediaId: d.voiceMediaId || null
  };
  if (mode === "update") body.version = d.version;
  return body;
}

export function characterIssue(d: CharacterDraft): string {
  const name = d.name.trim();
  if (!name) return "名称不能为空";
  if (marketingCharCount(name) > MAX_CHARACTER_NAME) return `名称最多 ${MAX_CHARACTER_NAME} 字`;
  const bad = [...CHARACTER_NAME_FORBIDDEN].find(c => name.includes(c));
  if (bad) return `名称不能包含「${bad}」`;
  if (marketingCharCount(d.summary) > MARKETING_MAX_SUMMARY) return `简介最多 ${MARKETING_MAX_SUMMARY} 字`;
  if (marketingCharCount(d.appearance) > MARKETING_MAX_APPEARANCE) return `外观描述最多 ${MARKETING_MAX_APPEARANCE} 字`;
  if (marketingCharCount(d.voiceText) > MARKETING_MAX_VOICE_TEXT) return `声音描述最多 ${MARKETING_MAX_VOICE_TEXT} 字`;
  if (d.imageMediaIds.length > MARKETING_MAX_IMAGES) return `图片最多 ${MARKETING_MAX_IMAGES} 张`;
  return "";
}

/** 把第 index 张设为主图（第一张）。 */
export function marketingMakePrimary<T>(list: readonly T[], index: number): T[] {
  if (index <= 0 || index >= list.length) return [...list];
  const next = [...list];
  const [it] = next.splice(index, 1);
  return [it!, ...next];
}

/** 导出一个角色：不带媒体（图片 / 参考音频只在本机有意义）。 */
export function characterExportItem(row: Row): Row {
  const d = characterDraft(row);
  return { name: d.name.trim(), summary: d.summary, appearance: d.appearance, voiceText: d.voiceText };
}

/** 导入项 → 保存请求体：覆盖已有时保留它的图片与音频。 */
export function characterImportPayload(item: Row, current: Row | null): Row {
  const d = characterDraft({ ...item, imageMediaIds: current?.imageMediaIds ?? [], voiceMediaId: current?.voiceMediaId ?? null,
    version: current?.version });
  return characterPayload(d, current ? "update" : "create");
}
