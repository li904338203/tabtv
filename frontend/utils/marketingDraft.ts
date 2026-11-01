/**
 * 营销输入框的草稿（MKT-T-14，营销 dev/03 §3.6；MKT-F-03、MKT-UX-11）。纯函数 + 存取。
 *
 * 用途有两个，走同一份数据：
 * - **刷新不丢**：输入一变就（500ms 防抖）写一次；进页面时读回来。
 * - **未登录点输入框 → 去登录 → 回来恢复**：跳登录前写一次，登录回到 `/marketing` 时读回来。
 *
 * 存什么：正文、胶囊（code + name + auto）、`suppressAutoSkill`、上一次的预填原文、所选选项的输入要求、
 * **已完成**的槽位项（商品 / 角色 / 资产 / 媒体 ID、顺序即数组顺序、显示名、商品张数）、模型偏好草稿。
 *
 * ⚠️ **结构版本 v2**（MKT-T-38，MKD-38）：商品改从商品库选。读到 v1（或没有版本号）的旧草稿时，
 *    丢弃商品槽的媒体项、商品名称卖点、角色槽里上传的人物图，**保留**正文、胶囊、脚本资产角色与源视频，
 *    并以 `droppedLegacy` 告诉输入框提示一次（MKT-F-133）—— 静默丢掉的话用户刷新后发现商品没了而不知道为什么。
 *
 * ⚠️ 键与创造页**分开**（`seeaitv:marketing-draft` vs `create-draft`）：共用的话从创造页登录回来
 *    会恢复出一组营销胶囊，反过来营销页会恢复出创造页的模式。
 * ⚠️ **上传中 / 失败的不进草稿**：`File` 存不进 sessionStorage，只记条数，恢复时说清「有 N 张图片未上传完成，请重新添加」——
 *    悄悄丢掉的话用户以为传上去了，到发送时才发现少了几张。
 * ⚠️ 只存 ID，不存任何 URL / 本地预览地址（GO-27）；非数字 ID 一律丢弃。恢复时媒体归属由调用方再校验一次。
 * ⚠️ 损坏 / 过期 / 形状不对的一律当没有，不抛错（读不出草稿不该让营销页打不开）。
 */
import type { AgentPrefsValue, PrefKind } from "./agentPrefs";
import type { ComposerChip, ComposerChipDim } from "./composerKeys";
import type { MarketingComposerState, PickDim, SlotKey } from "./marketingComposer";
import { SLOT_CODE_MAX, SLOT_KEYS, slotItemAccepted, type MarketingSlotsValue, type SlotItem, type SlotItemKind } from "./marketingSlots";

export const MARKETING_DRAFT_KEY = "seeaitv:marketing-draft";
export const MARKETING_DRAFT_TTL_MS = 24 * 60 * 60 * 1000;
/** 写入防抖（MKT-UX-11）。 */
export const MARKETING_DRAFT_DEBOUNCE_MS = 500;
/** 正文上限与消息上限一致（服务端 8000 字）。 */
const MAX_TEXT = 8000;
/** 草稿结构版本（v1 = 商品媒体图 + 名称卖点；v2 = 商品库 / 角色库引用）。 */
export const MARKETING_DRAFT_VERSION = 2;
/** 旧草稿被迁移时的提示（MKT-F-133）。 */
export const MARKETING_LEGACY_DRAFT_TEXT = "输入框已改为从商品库 / 角色库选择，请重新选择";
const CHIP_DIMS: readonly ComposerChipDim[] = ["skill", "creative", "hook", "style"];
const PICK_DIMS: readonly PickDim[] = ["creative", "hook", "style"];
const PREF_KINDS: readonly PrefKind[] = ["video", "image", "reasoning"];

/** 草稿里的一个槽位项：只有上传完成、有服务端 ID 的。 */
export type MarketingDraftItem = {
  kind: SlotItemKind;
  id: string;
  label: string;
  thumbMediaId?: string;
  hint?: string;
  imageCount?: number;
};

export type MarketingDraft = {
  text: string;
  lastPrefill: string;
  chips: ComposerChip[];
  suppressAutoSkill: boolean;
  pickRequirements: MarketingComposerState["pickRequirements"];
  slots: Record<SlotKey, MarketingDraftItem[]>;
  /** 存草稿时还没上传完成（上传中 / 失败）的条数。 */
  pendingUploads: number;
  /** 模型偏好草稿（会话还没建，偏好只在本地）；没改过为空对象。 */
  prefs: AgentPrefsValue;
  skillPrefs: MarketingComposerState["skillPrefs"];
  /** 读到的是旧结构的草稿、且确实丢掉了东西（只在读出时为 true；不写回存储）。 */
  droppedLegacy?: boolean;
};

type StorageLike = Pick<Storage, "getItem" | "setItem" | "removeItem">;

const isId = (value: unknown): value is string => typeof value === "string" && /^\d+$/.test(value);
const isPlainObject = (value: unknown): value is Record<string, unknown> => typeof value === "object" && value !== null && !Array.isArray(value);

// ---------------------------------------------------------------- 从输入框状态取草稿

/** 从输入框当前状态生成草稿：只留上传完成的槽位项，其余计数。 */
export function marketingDraftOf(input: {
  state: Pick<MarketingComposerState, "chips" | "lastPrefill" | "suppressAutoSkill" | "pickRequirements" | "skillPrefs">;
  text: string;
  slots: MarketingSlotsValue;
  prefs?: AgentPrefsValue;
}): MarketingDraft {
  let pendingUploads = 0;
  const slots = {} as Record<SlotKey, MarketingDraftItem[]>;
  for (const key of SLOT_KEYS) {
    slots[key] = [];
    for (const item of input.slots.items[key]) {
      if (item.status !== "done" || !isId(item.id)) {
        pendingUploads += 1;
        continue;
      }
      slots[key].push({
        kind: item.kind,
        id: item.id,
        label: item.label,
        ...(isId(item.thumbMediaId) ? { thumbMediaId: item.thumbMediaId } : {}),
        ...(item.hint ? { hint: item.hint } : {}),
        ...(typeof item.imageCount === "number" ? { imageCount: item.imageCount } : {})
      });
    }
  }
  return {
    text: input.text,
    lastPrefill: input.state.lastPrefill,
    chips: input.state.chips.map(chip => ({ dim: chip.dim, code: chip.code, name: chip.name, ...(chip.auto ? { auto: true } : {}) })),
    suppressAutoSkill: input.state.suppressAutoSkill,
    pickRequirements: input.state.pickRequirements,
    slots,
    pendingUploads,
    prefs: input.prefs ?? {},
    skillPrefs: input.state.skillPrefs
  };
}

/** 什么都没填（存它没有意义，且会把「清空了」的意图覆盖掉）。 */
export function isEmptyMarketingDraft(draft: MarketingDraft): boolean {
  return !draft.text.trim()
    && draft.chips.length === 0
    && SLOT_KEYS.every(key => draft.slots[key].length === 0)
    && draft.pendingUploads === 0;
}

// ---------------------------------------------------------------- 编解码

export function encodeMarketingDraft(draft: MarketingDraft, now: number = Date.now()): string {
  return JSON.stringify({
    v: MARKETING_DRAFT_VERSION,
    savedAt: now,
    text: draft.text.slice(0, MAX_TEXT),
    lastPrefill: draft.lastPrefill.slice(0, MAX_TEXT),
    chips: draft.chips,
    suppressAutoSkill: draft.suppressAutoSkill,
    pickRequirements: draft.pickRequirements,
    slots: Object.fromEntries(SLOT_KEYS.map(key => [key, draft.slots[key]
      .filter(item => isId(item.id) && slotItemAccepted(key, item.kind))
      .slice(0, SLOT_CODE_MAX[key])])),
    pendingUploads: draft.pendingUploads,
    prefs: draft.prefs,
    skillPrefs: draft.skillPrefs
  });
}

function decodeChips(raw: unknown): ComposerChip[] {
  if (!Array.isArray(raw)) return [];
  const seen = new Set<ComposerChipDim>();
  const out: ComposerChip[] = [];
  for (const entry of raw) {
    if (!isPlainObject(entry)) continue;
    const dim = entry.dim as ComposerChipDim;
    if (!CHIP_DIMS.includes(dim) || seen.has(dim)) continue;
    if (typeof entry.code !== "string" || !entry.code || typeof entry.name !== "string") continue;
    seen.add(dim);
    out.push({ dim, code: entry.code, name: entry.name, ...(entry.auto === true ? { auto: true } : {}) });
  }
  return CHIP_DIMS.flatMap(dim => out.filter(chip => chip.dim === dim));
}

function decodeRequirements(raw: unknown): MarketingDraft["pickRequirements"] {
  if (!isPlainObject(raw)) return {};
  const out: MarketingDraft["pickRequirements"] = {};
  for (const dim of PICK_DIMS) {
    const value = raw[dim];
    if (!isPlainObject(value)) continue;
    out[dim] = Object.fromEntries(Object.entries(value).filter(([, level]) => typeof level === "string")) as Record<string, string>;
  }
  return out;
}

/**
 * 解一个槽的项：种类不是这个槽认的一律丢弃并计数（v1 的商品媒体图、上传的人物图都落在这里）。
 * 形状本身坏掉的（非数字 ID 等）不计数 —— 那不是用户放进去的东西。
 */
function decodeItems(raw: unknown, key: SlotKey): { items: MarketingDraftItem[]; dropped: number } {
  if (!Array.isArray(raw)) return { items: [], dropped: 0 };
  const out: MarketingDraftItem[] = [];
  let dropped = 0;
  for (const entry of raw) {
    if (!isPlainObject(entry) || typeof entry.kind !== "string" || !isId(entry.id)) continue;
    if (!slotItemAccepted(key, entry.kind)) {
      dropped += 1;
      continue;
    }
    const count = Number(entry.imageCount);
    out.push({
      kind: entry.kind,
      id: entry.id,
      label: typeof entry.label === "string" ? entry.label : "",
      ...(isId(entry.thumbMediaId) ? { thumbMediaId: entry.thumbMediaId } : {}),
      ...(typeof entry.hint === "string" && entry.hint ? { hint: entry.hint } : {}),
      ...(Number.isInteger(count) && count >= 0 ? { imageCount: count } : {})
    });
  }
  return { items: out.slice(0, SLOT_CODE_MAX[key]), dropped };
}

function decodeSkillPrefs(raw: unknown): MarketingDraft["skillPrefs"] {
  if (!isPlainObject(raw)) return null;
  const out: NonNullable<MarketingDraft["skillPrefs"]> = {};
  for (const kind of PREF_KINDS) {
    const entry = raw[kind];
    if (!isPlainObject(entry) || typeof entry.applied !== "string" || !entry.applied) continue;
    out[kind] = { applied: entry.applied, ...(typeof entry.original === "string" ? { original: entry.original } : {}) };
  }
  return Object.keys(out).length ? out : null;
}

export function decodeMarketingDraft(raw: string | null | undefined, now: number = Date.now()): MarketingDraft | undefined {
  if (!raw) return undefined;
  let value: Record<string, unknown>;
  try {
    const parsed = JSON.parse(raw) as unknown;
    if (!isPlainObject(parsed)) return undefined;
    value = parsed;
  } catch {
    return undefined;
  }
  const savedAt = Number(value.savedAt);
  if (!Number.isFinite(savedAt) || now - savedAt > MARKETING_DRAFT_TTL_MS || savedAt - now > 60_000) return undefined;
  if (typeof value.text !== "string") return undefined;
  const slotsRaw = isPlainObject(value.slots) ? value.slots : {};
  const pending = Number(value.pendingUploads);
  const legacy = value.v !== MARKETING_DRAFT_VERSION;
  const decoded = SLOT_KEYS.map(key => [key, decodeItems(slotsRaw[key], key)] as const);
  // v1 的名称卖点在新输入框里没有落点；有内容才算「丢了东西」（空串不打扰用户）。
  const legacyFields = legacy && [value.productName, value.sellingPoints].some(field => typeof field === "string" && field.trim() !== "");
  // v1 的上传中计数说的是「商品图未上传完成」，新结构里没有意义。
  const droppedLegacy = legacy && (legacyFields || decoded.some(([, entry]) => entry.dropped > 0) || (Number.isInteger(pending) && pending > 0));
  return {
    text: value.text.slice(0, MAX_TEXT),
    lastPrefill: typeof value.lastPrefill === "string" ? value.lastPrefill.slice(0, MAX_TEXT) : "",
    chips: decodeChips(value.chips),
    suppressAutoSkill: value.suppressAutoSkill === true,
    pickRequirements: decodeRequirements(value.pickRequirements),
    slots: Object.fromEntries(decoded.map(([key, entry]) => [key, entry.items])) as MarketingDraft["slots"],
    pendingUploads: !legacy && Number.isInteger(pending) && pending > 0 ? pending : 0,
    prefs: isPlainObject(value.prefs) ? (value.prefs as AgentPrefsValue) : {},
    skillPrefs: decodeSkillPrefs(value.skillPrefs),
    ...(droppedLegacy ? { droppedLegacy: true } : {})
  };
}

// ---------------------------------------------------------------- 存取

/** 写草稿；什么都没填时删掉（用户清空了输入框，刷新后不该又冒出来）。写不进去（隐私模式、配额）只是不恢复。 */
export function saveMarketingDraft(storage: StorageLike | undefined, draft: MarketingDraft, now: number = Date.now()) {
  try {
    if (isEmptyMarketingDraft(draft)) storage?.removeItem(MARKETING_DRAFT_KEY);
    else storage?.setItem(MARKETING_DRAFT_KEY, encodeMarketingDraft(draft, now));
  } catch {
    // 忽略。
  }
}

/**
 * 读草稿（**不清**：刷新恢复之后用户什么都没改，再刷新一次也应该还在；之后的输入会防抖覆盖它）。
 * 过期 / 损坏的顺手删掉。
 */
export function readMarketingDraft(storage: StorageLike | undefined, now: number = Date.now()): MarketingDraft | undefined {
  try {
    const raw = storage?.getItem(MARKETING_DRAFT_KEY);
    if (raw === null || raw === undefined) return undefined;
    const draft = decodeMarketingDraft(raw, now);
    if (!draft) storage?.removeItem(MARKETING_DRAFT_KEY);
    return draft;
  } catch {
    return undefined;
  }
}

/** 发送成功后清掉（MKT-T-24 调）。 */
export function clearMarketingDraft(storage: StorageLike | undefined) {
  try {
    storage?.removeItem(MARKETING_DRAFT_KEY);
  } catch {
    // 忽略。
  }
}

// ---------------------------------------------------------------- 恢复

/** 草稿 → 槽位值（全部是已完成项；`makeId` 生成本地键）。 */
export function slotsValueFromDraft(draft: MarketingDraft, makeId: () => string): MarketingSlotsValue {
  const items = Object.fromEntries(SLOT_KEYS.map(key => [key, draft.slots[key].map((item): SlotItem => ({
    localId: makeId(),
    kind: item.kind,
    id: item.id,
    status: "done",
    label: item.label,
    ...(item.thumbMediaId ? { thumbMediaId: item.thumbMediaId } : {}),
    ...(item.hint ? { hint: item.hint } : {}),
    ...(typeof item.imageCount === "number" ? { imageCount: item.imageCount } : {})
  }))])) as MarketingSlotsValue["items"];
  return { items };
}

/** 草稿 → 输入框状态（槽位声明与 `filled` 不在草稿里：声明由调用方按技能重算，条数由槽位组件上报）。 */
export function composerStateFromDraft(base: MarketingComposerState, draft: MarketingDraft): MarketingComposerState {
  return {
    ...base,
    chips: draft.chips,
    text: draft.text,
    lastPrefill: draft.lastPrefill,
    suppressAutoSkill: draft.suppressAutoSkill,
    pickRequirements: draft.pickRequirements,
    skillPrefs: draft.skillPrefs
  };
}

/** 恢复时没能带回来的文件说明；没有为空串。 */
export function pendingUploadsText(count: number): string {
  return count > 0 ? `有 ${count} 个文件未上传完成，请重新添加` : "";
}
