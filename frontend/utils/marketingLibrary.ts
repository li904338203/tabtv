/**
 * 营销商品库（MKT-T-38，营销 dev/05 §4.4、§5；MKT-F-92/93/96/97、MKT-UX-14/15/17/18）。纯函数。
 *
 * 商品选择弹窗（`ProductPickerDialog.vue`）与新增 / 编辑弹窗（`ProductEditorDialog.vue`）只调用这里：
 * 列表查询参数、图片条的增删排与主图、码点计数、保存前校验、请求体、冲突 / 配额 / 删除文案。
 * 角色库的部分（MKT-T-39）在文件后半段。
 *
 * ⚠️ 几条容易做错的：
 * - **上限与长度一律取 `/marketing/config`**（`maxProductImages`、`textLimits`、`allowedImageMimes`），
 *   这里的常量只是配置缺失时的兜底 —— 写死的话改配置后「界面允许、提交被拒」（MKD-34）。
 * - **按码点计长度**（emoji 算 1）：`String.length` 按 UTF-16 码元，与服务端（Go 按 rune）不一致。
 * - **主图就是排第一的那张**，不是单独存的标记；排序与「设为主图」复用 `moveSlotItem`（同一个语义）。
 * - **格式只是预检**：服务端按上传时嗅探出的 content_type 再判（MKD-35），这里挡掉的是明显不收的（GIF / BMP）。
 * - ⚠️ 导出名一律 `marketing` 前缀：Nuxt 自动导入 `utils/` 的全部导出，撞名会悄悄顶替（CLAUDE.md §5.28）。
 */
import type { components } from "@tabtv/api-client";
import { moveSlotItem, type SlotItem } from "./marketingSlots";

export type MarketingProduct = components["schemas"]["MarketingProduct"];
export type MarketingProductSummary = components["schemas"]["MarketingProductSummary"];
export type MarketingProductInput = components["schemas"]["MarketingProductInput"];
type MarketingConfig = components["schemas"]["MarketingConfig"];

// ---------------------------------------------------------------- 配置兜底

/** 配置缺失时的兜底（与服务端默认值一致，dev/05 MKD-34）。 */
export const MARKETING_LIBRARY_FALLBACK = {
  maxProductImages: 10,
  productQuota: 500,
  productName: 30,
  sellingPoints: 300,
  productDescription: 300,
  allowedImageMimes: ["image/png", "image/jpeg", "image/webp"] as readonly string[]
};

export type MarketingProductLimits = {
  maxImages: number;
  quota: number;
  name: number;
  sellingPoints: number;
  description: number;
  mimes: readonly string[];
};

const positive = (value: unknown, fallback: number) => (typeof value === "number" && value > 0 ? value : fallback);

/** 从 `/marketing/config` 取商品的上限；缺字段（旧后端）时回落兜底值。 */
export function marketingProductLimits(config: Partial<MarketingConfig> | null | undefined): MarketingProductLimits {
  const f = MARKETING_LIBRARY_FALLBACK;
  return {
    maxImages: positive(config?.maxProductImages, f.maxProductImages),
    quota: positive(config?.quota?.products, f.productQuota),
    name: positive(config?.textLimits?.productName, f.productName),
    sellingPoints: positive(config?.textLimits?.sellingPoints, f.sellingPoints),
    description: positive(config?.textLimits?.productDescription, f.productDescription),
    mimes: config?.allowedImageMimes?.length ? config.allowedImageMimes : f.allowedImageMimes
  };
}

// ---------------------------------------------------------------- 列表查询

export type MarketingLibrarySort = "updated" | "created" | "name";

export const MARKETING_LIBRARY_SORTS: ReadonlyArray<{ value: MarketingLibrarySort; label: string }> = [
  { value: "updated", label: "最近更新" },
  { value: "created", label: "最近创建" },
  { value: "name", label: "名称" }
];

/** 搜索框防抖（MKT-F-92：服务端搜索，不在浏览器里筛）。 */
export const MARKETING_SEARCH_DEBOUNCE_MS = 300;
export const MARKETING_LIBRARY_PAGE_SIZE = 24;

/**
 * 列表查询参数：空搜索不带 `q`、默认排序不带 `sort`、第一页不带 `cursor`。
 * ⚠️ 游标与排序绑定（dev/05 R-1 #7）：换排序 / 换搜索必须从第一页开始，调用方 `reset()` 即可。
 */
export function marketingLibraryQuery(input: { q?: string; sort?: MarketingLibrarySort; cursor?: string; limit?: number }): Record<string, string> {
  const out: Record<string, string> = {};
  const q = (input.q ?? "").trim();
  if (q) out.q = q;
  if (input.sort && input.sort !== "updated") out.sort = input.sort;
  if (input.cursor) out.cursor = input.cursor;
  out.limit = String(input.limit && input.limit > 0 ? input.limit : MARKETING_LIBRARY_PAGE_SIZE);
  return out;
}

/** 空列表的说法：真没有 vs 搜索没结果（MKT-F-92）。 */
export function marketingProductEmptyText(q: string): string {
  return q.trim() ? "没有匹配的商品" : "暂无商品";
}

/** 卡上的张数（「共 3 张」）。 */
export function marketingProductCountText(imageCount: number): string {
  return `共 ${Math.max(0, imageCount)} 张`;
}

/** 确认卡 / 换图器选图区的标题（MKT-F-99）；旧会话没有商品名时为空串。 */
export function marketingProductTitle(name: string | null | undefined): string {
  const flat = (name ?? "").trim();
  return flat ? `商品：${flat}` : "";
}

/** 选择弹窗「添加」能不能点：选择相对打开时有变化才可点（MKT-UX-14）。 */
export function marketingPickChanged(initialId: string, selectedId: string): boolean {
  return initialId !== selectedId;
}

// ---------------------------------------------------------------- 文本

/** 码点数（emoji 算 1，与服务端 Go 按 rune 计一致）。 */
export function marketingTextLength(text: string): number {
  return Array.from(text).length;
}

/** 字段计数与超长提示：「12 / 30」「商品名称最多 30 字（当前 31 字）」。 */
export function marketingTextCheck(text: string, limit: number, label: string): { length: number; counter: string; error: string } {
  const length = marketingTextLength(text);
  return {
    length,
    counter: `${length} / ${limit}`,
    error: length > limit ? `${label}最多 ${limit} 字（当前 ${length} 字）` : ""
  };
}

// ---------------------------------------------------------------- 图片条

export type MarketingImageItem = {
  localId: string;
  /** 上传完成后的媒体 ID；已有的图直接带。 */
  mediaId?: string;
  status: "uploading" | "failed" | "done";
  name: string;
  error?: string;
};

const MIME_LABEL: Record<string, string> = { "image/png": "PNG", "image/jpeg": "JPG", "image/webp": "WEBP" };

/** 「支持 PNG/JPG/WEBP，最多 10 张」（格式与张数都取配置）。 */
export function marketingImageHint(limits: Pick<MarketingProductLimits, "mimes" | "maxImages">): string {
  const formats = [...new Set(limits.mimes.map(mime => MIME_LABEL[mime] ?? mime.replace(/^image\//, "").toUpperCase()))];
  return `支持 ${formats.join("/")}，最多 ${limits.maxImages} 张`;
}

/** `<input accept>`。 */
export function marketingImageAccept(mimes: readonly string[]): string {
  return mimes.join(",");
}

/** 文件格式预检（浏览器报的 MIME，服务端会按文件头再判）。 */
export function marketingImageAllowed(mime: string, mimes: readonly string[]): boolean {
  const normalized = mime === "image/jpg" ? "image/jpeg" : mime;
  return mimes.includes(normalized);
}

/** 还能不能加图；满了给「最多 10 张」。 */
export function marketingImageGate(items: readonly MarketingImageItem[], max: number): { ok: boolean; remaining: number; reason: string } {
  const remaining = Math.max(0, max - items.length);
  return remaining > 0 ? { ok: true, remaining, reason: "" } : { ok: false, remaining: 0, reason: `最多 ${max} 张` };
}

/** 一次选多了只加到上限并说清丢了几张。 */
export function marketingAddImages(
  items: readonly MarketingImageItem[],
  incoming: readonly MarketingImageItem[],
  max: number
): { items: MarketingImageItem[]; added: MarketingImageItem[]; notice: string } {
  const room = Math.max(0, max - items.length);
  const added = incoming.slice(0, room);
  const rejected = incoming.length - added.length;
  return {
    items: [...items, ...added],
    added,
    notice: rejected > 0 ? `最多 ${max} 张，有 ${rejected} 张未添加` : ""
  };
}

/** 拖动排序（排第一即主图）。 */
export function marketingMoveImage(items: readonly MarketingImageItem[], from: number, to: number): MarketingImageItem[] {
  return moveSlotItem(items, from, to);
}

/** 设为主图 = 移到第一张。 */
export function marketingSetMainImage(items: readonly MarketingImageItem[], index: number): MarketingImageItem[] {
  return moveSlotItem(items, index, 0);
}

export function marketingRemoveImage(items: readonly MarketingImageItem[], localId: string): MarketingImageItem[] {
  return items.filter(item => item.localId !== localId);
}

/** 按 localId 改一项；已被删的不复活（晚到的上传结果）。 */
export function marketingPatchImage(items: readonly MarketingImageItem[], localId: string, patch: Partial<MarketingImageItem>): MarketingImageItem[] {
  return items.map(item => (item.localId === localId ? { ...item, ...patch } : item));
}

// ---------------------------------------------------------------- 表单

export type MarketingProductForm = {
  images: MarketingImageItem[];
  name: string;
  sellingPoints: string;
  description: string;
};

export function marketingEmptyProductForm(): MarketingProductForm {
  return { images: [], name: "", sellingPoints: "", description: "" };
}

/** 详情 → 表单（编辑时）。 */
export function marketingProductFormOf(product: Pick<MarketingProduct, "imageMediaIds" | "name" | "sellingPoints" | "description">, makeId: () => string): MarketingProductForm {
  return {
    images: product.imageMediaIds.map((id, i) => ({ localId: makeId(), mediaId: id, status: "done", name: `图片 ${i + 1}` })),
    name: product.name,
    sellingPoints: product.sellingPoints ?? "",
    description: product.description ?? ""
  };
}

/**
 * 「保存」能不能点、不能点为什么（MKT-F-93、MKT-AC-32 前端部分）。
 * 顺序：只读 > 缺图 > 上传中 > 上传失败 > 超张数 > 缺名称 > 各字段超长。给第一条（最根本的那一条）。
 */
export function marketingProductFormIssue(form: MarketingProductForm, limits: MarketingProductLimits, readOnly = false): string {
  if (readOnly) return MARKETING_READ_ONLY_TEXT;
  const done = form.images.filter(item => item.status === "done" && item.mediaId);
  const uploading = form.images.filter(item => item.status === "uploading").length;
  const failed = form.images.filter(item => item.status === "failed").length;
  if (!form.images.length) return "请上传商品图";
  if (uploading) return `有 ${uploading} 张图片还在上传`;
  if (failed) return `有 ${failed} 张图片上传失败，请重试或移除`;
  if (done.length > limits.maxImages) return `商品图最多 ${limits.maxImages} 张`;
  if (!form.name.trim()) return "请填写商品名称";
  return marketingTextCheck(form.name.trim(), limits.name, "商品名称").error
    || marketingTextCheck(form.sellingPoints, limits.sellingPoints, "商品卖点").error
    || marketingTextCheck(form.description, limits.description, "商品描述").error;
}

/** 表单 → 请求体（只带上传完成的图，顺序即界面顺序，[0] 为主图）。 */
export function marketingProductInput(form: MarketingProductForm): MarketingProductInput {
  return {
    name: form.name.trim(),
    sellingPoints: form.sellingPoints.trim(),
    description: form.description.trim(),
    imageMediaIds: form.images.filter(item => item.status === "done" && item.mediaId).map(item => item.mediaId!)
  };
}

/** 有没有未保存的改动（关闭时要不要确认一次，MKT-UX-15）。上传中 / 失败的也算改动。 */
export function marketingProductDirty(form: MarketingProductForm, original: MarketingProductForm): boolean {
  if (form.images.some(item => item.status !== "done")) return true;
  const ids = (f: MarketingProductForm) => f.images.map(item => item.mediaId ?? "").join(",");
  return ids(form) !== ids(original)
    || form.name !== original.name
    || form.sellingPoints !== original.sellingPoints
    || form.description !== original.description;
}

// ---------------------------------------------------------------- 文案

export const MARKETING_READ_ONLY_TEXT = "你在该工作区是只读，不能新增、编辑或删除商品";

/** 关闭编辑弹窗前的确认（MKT-UX-15）；不需要确认时返回 ""。 */
export function marketingCloseConfirmText(form: MarketingProductForm, original: MarketingProductForm): string {
  if (form.images.some(item => item.status === "uploading")) return "图片还在上传，关闭后上传会被取消，确定放弃修改？";
  return marketingProductDirty(form, original) ? "有未保存的修改，确定放弃修改？" : "";
}

/** 删除确认（MKT-UX-18 / MKT-F-96）。 */
export function marketingProductDeleteConfirm(name: string): { title: string; message: string } {
  return { title: `删除商品「${name}」？`, message: "删除后从商品库移除；已用它创作的会话不受影响" };
}

/** 被删的商品正被输入框选中时的那一句。 */
export const MARKETING_PRODUCT_REMOVED_TEXT = "已删除的商品已从输入框移除";

/**
 * 配额（MKT-F-97）：游标分页没有总数，只在两种情况下判满 —— 已加载到底且条数达上限，或服务端回过 409 quota。
 * 满了「新增」禁用并说明。
 */
export function marketingProductQuota(input: { loaded: number; finished: boolean; max: number; serverFull?: boolean }): { full: boolean; reason: string } {
  const full = Boolean(input.serverFull) || (input.finished && input.loaded >= input.max);
  return { full, reason: full ? `商品数已达上限（${input.max} 个），删除不用的商品后再新增` : "" };
}

/** 服务端 409 是不是配额满（`details.reason = "quota"`，dev/05 R-1 #2）。 */
export function marketingIsQuotaError(failure: { code?: number; details: Record<string, unknown> }): boolean {
  return failure.details.reason === "quota";
}

/**
 * 版本冲突（40902，CD-58 冲突可解释）：说清是谁改的，按 `bySelf` 分流不匹配文案。
 * 本地输入保留，用户自己点「重新加载」才覆盖。
 */
export function marketingConflictText(details: Record<string, unknown>, noun = "商品"): string {
  const name = typeof details.currentEditorName === "string" ? details.currentEditorName.trim() : "";
  const who = details.bySelf === true
    ? `你在另一个窗口里改过这个${noun}`
    : name ? `「${name}」刚改过这个${noun}` : `这个${noun}已被其他人修改`;
  return `${who}，你的输入还在；重新加载会换成最新内容`;
}

// ================================================================ 角色库（MKT-T-39，dev/05 §4.4、§5；MKT-F-100~112）
//
// 角色选择弹窗（`CharacterPickerDialog.vue`）与新建 / 编辑角色弹窗（`CharacterEditorDialog.vue`）只调用这里。
// ⚠️ 名称禁用字符表取 `/marketing/config.characterNameForbidden`（MKD-36：不另抄一份，抄的话两边迟早差一个字符）。
// ⚠️ 生成角色的价格只来自 dryRun 报价（与 `/generations/estimate` 同一个函数）；这里不乘不除。
// ⚠️ 轮询耗尽 ≠ 失败（GAP-14）：状态文案里那一句不出现「失败」，也不提退款。

export type MarketingCharacter = components["schemas"]["MarketingCharacter"];
export type MarketingCharacterSummary = components["schemas"]["MarketingCharacterSummary"];
export type MarketingCharacterInput = components["schemas"]["MarketingCharacterInput"];
export type MarketingPresetCharacter = components["schemas"]["MarketingPresetCharacter"];
export type MarketingCharacterGenRequest = components["schemas"]["MarketingCharacterGenerateRequest"];
export type MarketingCharacterGenQuote = components["schemas"]["MarketingCharacterGenerateQuote"];
export type MarketingCharacterGenAccepted = components["schemas"]["MarketingCharacterGenerateAccepted"];

/** 配置缺失时的兜底（与服务端设计建议值一致，dev/05 MKD-34 / MKD-36）。 */
export const MARKETING_CHARACTER_FALLBACK = {
  maxImages: 9,
  quota: 200,
  maxRefs: 2,
  genMax: 4,
  name: 50,
  appearance: 500,
  voiceText: 500,
  forbidden: ["@", "<", ">", "/", "\\", "|", ":", "*", "?", "(", ")", "（", "）"] as readonly string[]
};

export type MarketingCharacterLimits = {
  maxImages: number;
  quota: number;
  /** 角色槽上限（输入框里最多几个角色）。 */
  maxRefs: number;
  /** 一次「生成角色」最多几张。 */
  genMax: number;
  name: number;
  appearance: number;
  voiceText: number;
  forbidden: readonly string[];
  mimes: readonly string[];
};

export function marketingCharacterLimits(config: Partial<MarketingConfig> | null | undefined): MarketingCharacterLimits {
  const f = MARKETING_CHARACTER_FALLBACK;
  return {
    maxImages: positive(config?.maxCharacterImages, f.maxImages),
    quota: positive(config?.quota?.characters, f.quota),
    maxRefs: positive(config?.maxCharacterRefs, f.maxRefs),
    genMax: positive(config?.maxCharacterGenCount, f.genMax),
    name: positive(config?.textLimits?.characterName, f.name),
    appearance: positive(config?.textLimits?.appearance, f.appearance),
    voiceText: positive(config?.textLimits?.voiceText, f.voiceText),
    forbidden: config?.characterNameForbidden?.length ? config.characterNameForbidden : f.forbidden,
    mimes: config?.allowedImageMimes?.length ? config.allowedImageMimes : MARKETING_LIBRARY_FALLBACK.allowedImageMimes
  };
}

// ---------------------------------------------------------------- 名称

/** 名称输入框下的说明：「1-50 字符，不支持 @ < > / \ | : * ? ( )」（字符表取配置）。 */
export function marketingCharacterNameHint(limits: Pick<MarketingCharacterLimits, "name" | "forbidden">): string {
  return `1-${limits.name} 字符，不支持 ${limits.forbidden.join(" ")}`;
}

/**
 * 名称即时校验（逐字符，与服务端 `CharacterNameIssue` 同一张字符表）；合法返回 ""。
 * 顺序：空 > 超长 > 禁用字符（点名是哪几个，按出现顺序去重）。
 */
export function marketingCharacterNameIssue(name: string, limits: Pick<MarketingCharacterLimits, "name" | "forbidden">): string {
  const flat = name.trim();
  if (!flat) return "请填写角色名称";
  const length = marketingTextLength(flat);
  if (length > limits.name) return `角色名称最多 ${limits.name} 字（当前 ${length} 字）`;
  const forbidden = new Set(limits.forbidden);
  const hits: string[] = [];
  for (const ch of Array.from(flat)) {
    if (forbidden.has(ch) && !hits.includes(ch)) hits.push(ch);
  }
  return hits.length ? `角色名称不能包含 ${hits.map(ch => `「${ch}」`).join("")}` : "";
}

/**
 * 保存失败里点名到名称的那一条（重名 409 / 40001 的 `issues[field=name]`），没有返回 ""。
 * 输入框据此标红；其余原因走弹窗底部的错误行。
 */
export function marketingCharacterNameFailure(failure: { message: string; details: Record<string, unknown> }, name: string): string {
  const details = failure.details;
  if (details.field === "name" && details.code === "duplicate") return `「${name.trim()}」已有同名角色，换个名字`;
  const issues = Array.isArray(details.issues) ? details.issues as Array<{ field?: unknown; code?: unknown; message?: unknown }> : [];
  const hit = issues.find(issue => issue.field === "name");
  if (!hit) return "";
  if (hit.code === "duplicate") return `「${name.trim()}」已有同名角色，换个名字`;
  return typeof hit.message === "string" && hit.message ? hit.message : failure.message;
}

// ---------------------------------------------------------------- 表单

export type MarketingVoiceItem = {
  localId: string;
  mediaId?: string;
  status: "uploading" | "failed" | "done";
  name: string;
  error?: string;
};

export type MarketingCharacterForm = {
  images: MarketingImageItem[];
  name: string;
  appearance: string;
  styleCode: string;
  voiceText: string;
  /** 参考音频，至多 1 段（单值即上限）。 */
  voice: MarketingVoiceItem | null;
};

export function marketingEmptyCharacterForm(): MarketingCharacterForm {
  return { images: [], name: "", appearance: "", styleCode: "", voiceText: "", voice: null };
}

export function marketingCharacterFormOf(character: Pick<MarketingCharacter, "imageMediaIds" | "name" | "appearance" | "styleCode" | "voiceText" | "voiceMediaId">, makeId: () => string): MarketingCharacterForm {
  return {
    images: character.imageMediaIds.map((id, i) => ({ localId: makeId(), mediaId: id, status: "done", name: `图片 ${i + 1}` })),
    name: character.name,
    appearance: character.appearance ?? "",
    styleCode: character.styleCode ?? "",
    voiceText: character.voiceText ?? "",
    voice: character.voiceMediaId ? { localId: makeId(), mediaId: character.voiceMediaId, status: "done", name: "参考音频" } : null
  };
}

/** 再加一段音频行不行（MKT-F-105：至多 1 段）。 */
export function marketingVoiceGate(voice: MarketingVoiceItem | null): { ok: boolean; reason: string } {
  return voice ? { ok: false, reason: "只能上传 1 段音频，先删除当前这段" } : { ok: true, reason: "" };
}

/** 音频格式预检（服务端按文件头再判，只认 `audio/*`）。 */
export function marketingAudioAllowed(mime: string): boolean {
  return mime.startsWith("audio/");
}

export const MARKETING_CHARACTER_READ_ONLY_TEXT = "你在该工作区是只读，不能新增、编辑或删除角色";

/**
 * 「保存」能不能点、不能点为什么（MKT-F-105、MKT-AC-36 前端部分），给第一条：
 * 只读 > 缺图 > 图片上传中 > 图片失败 > 超张数 > 名称 > 形象超长 > 音色超长 > 音频上传中 / 失败。
 */
export function marketingCharacterFormIssue(form: MarketingCharacterForm, limits: MarketingCharacterLimits, readOnly = false): string {
  if (readOnly) return MARKETING_CHARACTER_READ_ONLY_TEXT;
  const uploading = form.images.filter(item => item.status === "uploading").length;
  const failed = form.images.filter(item => item.status === "failed").length;
  const done = form.images.filter(item => item.status === "done" && item.mediaId).length;
  if (!form.images.length) return "请添加至少 1 张角色图片";
  if (uploading) return `有 ${uploading} 张图片还在上传`;
  if (failed) return `有 ${failed} 张图片上传失败，请重试或移除`;
  if (done > limits.maxImages) return `角色图片最多 ${limits.maxImages} 张`;
  const nameIssue = marketingCharacterNameIssue(form.name, limits);
  if (nameIssue) return nameIssue;
  const textIssue = marketingTextCheck(form.appearance, limits.appearance, "形象描述").error
    || marketingTextCheck(form.voiceText, limits.voiceText, "音色描述").error;
  if (textIssue) return textIssue;
  if (form.voice?.status === "uploading") return "音频还在上传";
  if (form.voice?.status === "failed") return "音频上传失败，请重试或删除";
  return "";
}

/** 表单 → 请求体（只带上传完成的图，[0] 为主图；空的可选字段照样带空串 / null —— 整份更新）。 */
export function marketingCharacterInput(form: MarketingCharacterForm): MarketingCharacterInput {
  return {
    name: form.name.trim(),
    imageMediaIds: form.images.filter(item => item.status === "done" && item.mediaId).map(item => item.mediaId!),
    appearance: form.appearance.trim(),
    styleCode: form.styleCode || null,
    voiceText: form.voiceText.trim(),
    voiceMediaId: form.voice?.status === "done" && form.voice.mediaId ? form.voice.mediaId : null
  };
}

export function marketingCharacterDirty(form: MarketingCharacterForm, original: MarketingCharacterForm): boolean {
  if (form.images.some(item => item.status !== "done") || (form.voice && form.voice.status !== "done")) return true;
  const ids = (f: MarketingCharacterForm) => f.images.map(item => item.mediaId ?? "").join(",");
  return ids(form) !== ids(original)
    || form.name !== original.name
    || form.appearance !== original.appearance
    || form.styleCode !== original.styleCode
    || form.voiceText !== original.voiceText
    || (form.voice?.mediaId ?? "") !== (original.voice?.mediaId ?? "");
}

/**
 * 关闭编辑弹窗前要不要确认（MKT-UX-15）；不需要返回 ""。
 * ⚠️ 生成中 / 还有没加入的候选图：那是已经扣过费的结果，说清它们不会丢（24 小时内再打开可添加）。
 */
export function marketingCharacterCloseConfirmText(
  form: MarketingCharacterForm,
  original: MarketingCharacterForm,
  gen: { generating: boolean; pendingCandidates: number }
): string {
  const parts: string[] = [];
  if (form.images.some(item => item.status === "uploading") || form.voice?.status === "uploading") parts.push("文件还在上传，关闭后上传会被取消");
  else if (marketingCharacterDirty(form, original)) parts.push("有未保存的修改，关闭后会丢失");
  if (gen.generating) parts.push("生成会继续，结果可在下次打开该角色时添加");
  else if (gen.pendingCandidates > 0) parts.push(`还有 ${gen.pendingCandidates} 张生成结果没有加入，24 小时内再次打开该角色仍可添加`);
  return parts.length ? `${parts.join("；")}。确定关闭？` : "";
}

/** 删除确认（MKT-UX-18 同款）。 */
export function marketingCharacterDeleteConfirm(name: string): { title: string; message: string } {
  return { title: `删除角色「${name}」？`, message: "删除后从角色库移除；已用它创作的会话不受影响" };
}

/** 被删的角色正被输入框选中时的那一句。 */
export const MARKETING_CHARACTER_REMOVED_TEXT = "已删除的角色已从输入框移除";

/** 配额（MKT-F-112），判法同商品。满了「新增」与「添加营销角色」禁用并说明。 */
export function marketingCharacterQuota(input: { loaded: number; finished: boolean; max: number; serverFull?: boolean }): { full: boolean; reason: string } {
  const full = Boolean(input.serverFull) || (input.finished && input.loaded >= input.max);
  return { full, reason: full ? `角色数已达上限（${input.max} 个），删除不用的角色后再新增` : "" };
}

/** 音色区的如实说明（MK-25、MKT-F-104）。 */
export const MARKETING_VOICE_NOTE = "音色描述会在生成口播类视频时交给 Agent 参考；上传的音频仅作留存参考，不会发送给生成模型，也不会用于声音克隆";

/** 上传图片处的说明（MKT-PARAM-25）。 */
export const MARKETING_CHARACTER_UPLOAD_NOTE = "请上传非真人形象（卡通、拟人、吉祥物、产品拟人等），不得上传他人肖像";

/** 列表空态（MKT-F-101）。 */
export function marketingCharacterEmptyText(tab: "mine" | "preset", q: string): string {
  if (q.trim()) return tab === "mine" ? "没有匹配的角色" : "没有匹配的营销角色";
  return tab === "mine" ? "我的暂无角色" : "暂无营销角色";
}

// ---------------------------------------------------------------- 选择（弹窗 → 角色卡）

/** 弹窗里选中的一个角色：角色库角色或脚本角色资产（只读分组）。 */
export type MarketingPickedCharacter = {
  kind: "marketing_character" | "asset";
  id: string;
  name: string;
  /** 主图媒体 ID（受保护，走 useAuthenticatedMedia）。 */
  mainMediaId?: string | null;
};

export function marketingPickKey(item: Pick<MarketingPickedCharacter, "kind" | "id">): string {
  return `${item.kind}:${item.id}`;
}

/**
 * 点一张角色卡：已选则取消；没选且还有名额则加上；满了不加并说「最多 2 个角色」（数字取配置）。
 * 打开时保留输入框里已选的（MKT-UX-14），所以上限是**总数**不是剩余数。
 */
export function marketingToggleCharacter(
  selected: readonly MarketingPickedCharacter[],
  item: MarketingPickedCharacter,
  max: number
): { selected: MarketingPickedCharacter[]; notice: string } {
  const key = marketingPickKey(item);
  if (selected.some(entry => marketingPickKey(entry) === key)) return { selected: selected.filter(entry => marketingPickKey(entry) !== key), notice: "" };
  if (selected.length >= max) return { selected: [...selected], notice: `最多 ${max} 个角色` };
  return { selected: [...selected, item], notice: "" };
}

/** 「添加」能不能点：选择相对打开时有变化（集合与顺序都算）。 */
export function marketingCharacterPickChanged(initial: readonly MarketingPickedCharacter[], selected: readonly MarketingPickedCharacter[]): boolean {
  return initial.map(marketingPickKey).join("|") !== selected.map(marketingPickKey).join("|");
}

/** 选中的角色 → 角色槽的值（保持顺序；同一个只放一次）。 */
export function marketingCharacterSlotItems(picked: readonly MarketingPickedCharacter[], makeId: () => string): SlotItem[] {
  const seen = new Set<string>();
  const out: SlotItem[] = [];
  for (const item of picked) {
    const key = marketingPickKey(item);
    if (seen.has(key)) continue;
    seen.add(key);
    out.push({ localId: makeId(), kind: item.kind, id: item.id, status: "done", label: item.name, hint: "image", ...(item.mainMediaId ? { thumbMediaId: item.mainMediaId } : {}) });
  }
  return out;
}

/** 角色槽 → 弹窗打开时的已选（种类不对的旧项不带）。 */
export function marketingPickedFromSlot(items: readonly SlotItem[]): MarketingPickedCharacter[] {
  return items
    .filter(item => item.status === "done" && item.id && (item.kind === "marketing_character" || item.kind === "asset"))
    .map(item => ({ kind: item.kind as MarketingPickedCharacter["kind"], id: item.id!, name: item.label, mainMediaId: item.thumbMediaId ?? null }));
}

/**
 * 角色被编辑（名称 / 主图变了）或删除：输入框里那一项跟着更新 / 移除（MKT-UX-18 同款）。
 * 不在槽里原样返回；`removed` 告诉调用方要不要提示一次。
 */
export function marketingSyncCharacter(
  items: readonly SlotItem[],
  change: { kind: "updated"; character: MarketingPickedCharacter } | { kind: "deleted"; id: string }
): { items: SlotItem[]; changed: boolean; removed: boolean } {
  const id = change.kind === "updated" ? change.character.id : change.id;
  const index = items.findIndex(item => item.kind === "marketing_character" && item.id === id);
  if (index < 0) return { items: [...items], changed: false, removed: false };
  if (change.kind === "deleted") return { items: items.filter((_, i) => i !== index), changed: true, removed: true };
  const current = items[index]!;
  const next: SlotItem = { ...current, label: change.character.name, thumbMediaId: change.character.mainMediaId ?? undefined };
  return { items: items.map((item, i) => (i === index ? next : item)), changed: true, removed: false };
}

/** 营销角色（平台预置）的公开图片地址（不鉴权，直接 `<img>`，MKT-UX-16）。 */
export function marketingPresetImageUrl(apiBase: string, presetId: string, index = 0, width: 160 | 320 | 640 = 320): string {
  return `${apiBase}/marketing/preset-characters/${encodeURIComponent(presetId)}/images/${index}?w=${width}`;
}

/** 来源角标：从营销角色添加来的副本标「营销角色」。 */
export function marketingCharacterSourceLabel(source: string | undefined): string {
  return source === "preset" ? "营销角色" : "";
}

// ---------------------------------------------------------------- 生成角色

export type MarketingCharacterGenInput = {
  modelCode: string;
  description: string;
  styleCode: string;
  referenceMediaIds: readonly string[];
  count: number;
};

/** 数量夹到 1..max（整数）。 */
export function marketingClampGenCount(count: number, max: number): number {
  const n = Number.isFinite(count) ? Math.floor(count) : 1;
  return Math.min(Math.max(1, n), Math.max(1, max));
}

/**
 * 生成角色请求体（dryRun 与真生成同一份，只差 `dryRun`）：空画风 / 无参考图不带字段（服务端「可选」即缺省），
 * 描述去掉两端空白，数量夹到上限。
 */
export function marketingCharacterGenRequest(input: MarketingCharacterGenInput, max: number, dryRun: boolean): MarketingCharacterGenRequest {
  return {
    modelCode: input.modelCode,
    description: input.description.trim(),
    ...(input.styleCode ? { styleCode: input.styleCode } : {}),
    ...(input.referenceMediaIds.length ? { referenceMediaIds: [...input.referenceMediaIds] } : {}),
    count: marketingClampGenCount(input.count, max),
    dryRun
  };
}

/** 报价的缓存键：这几样任一变了就要重报（300ms 防抖）。 */
export function marketingCharacterGenKey(input: MarketingCharacterGenInput, max: number): string {
  return JSON.stringify(marketingCharacterGenRequest(input, max, true));
}

/**
 * 「生成角色」按钮（MKT-F-106、MKT-UX-17）：禁用理由给最根本的一条 ——
 * 只读 > 正在生成 > 缺描述 > 缺模型 > 描述超长 > 参考图上传中 > 校验中 / 校验失败 > 校验被拒。
 * tabtv 不计价（TT-T-14）：dryRun 只用来判这个组合能不能生成，按钮上不显示价格。
 */
export function marketingCharacterGenGate(input: {
  readOnly: boolean;
  generating: boolean;
  description: string;
  appearanceLimit: number;
  modelCode: string;
  uploadingRefs: number;
  quoting: boolean;
  quoteError: string;
  quote: Pick<MarketingCharacterGenQuote, "blocked" | "blockedReason"> | null;
}): { disabled: boolean; reason: string; label: string } {
  const label = "生成角色";
  const no = (reason: string) => ({ disabled: true, reason, label });
  if (input.readOnly) return no("你在该工作区是只读，不能生成角色");
  if (input.generating) return no("正在生成");
  if (!input.description.trim()) return no("先写下角色的形象描述");
  if (!input.modelCode) return no("请选择图片模型");
  const over = marketingTextCheck(input.description, input.appearanceLimit, "形象描述").error;
  if (over) return no(over);
  if (input.uploadingRefs > 0) return no("参考图还在上传");
  if (input.quoting || !input.quote) return no(input.quoteError || "正在报价");
  if (input.quote.blocked) return no(input.quote.blockedReason || "当前组合不能生成");
  return { disabled: false, reason: "", label };
}

export type MarketingGenPhase = "queued" | "running" | "success" | "failed" | "cancelled" | "unknown";

/**
 * 一次生成的状态文案。
 * ⚠️ `unknown`（浏览器轮询耗尽）**不说失败、不提退款**（GAP-14）：任务多半还在跑。
 */
export function marketingGenStatusText(phase: MarketingGenPhase, outputs = 0): string {
  switch (phase) {
    case "queued": return "排队中…";
    case "running": return "生成中…";
    case "success": return outputs > 0 ? `已生成 ${outputs} 张，点选加入角色图片` : "已完成，但没有返回图片";
    case "failed": return "这次没有生成成功";
    case "cancelled": return "已取消";
    default: return "仍在处理中，稍后再打开该角色查看结果";
  }
}

/** 一张候选图（生成产物）。 */
export type MarketingCandidate = { mediaId: string; generationId: string };

/**
 * 候选去重：同一张（mediaId）只出现一次；已经在角色图片里的、用户点过「不要」的不再显示。
 */
export function marketingVisibleCandidates(
  candidates: readonly MarketingCandidate[],
  imageMediaIds: readonly (string | undefined)[],
  dismissed: readonly string[] = []
): MarketingCandidate[] {
  const hidden = new Set([...imageMediaIds.filter((id): id is string => Boolean(id)), ...dismissed]);
  const seen = new Set<string>();
  const out: MarketingCandidate[] = [];
  for (const item of candidates) {
    if (!item.mediaId || hidden.has(item.mediaId) || seen.has(item.mediaId)) continue;
    seen.add(item.mediaId);
    out.push(item);
  }
  return out;
}

/**
 * 候选的本地记录（sessionStorage，24 小时，MKT-UX-15）：只记 generationId（候选从服务端再取），按角色分组
 * （新建中的角色记在 `new`，保存后改记到新角色 ID）。⚠️ 不记在角色行上：候选不是角色的一部分（dev/05 §5）。
 */
export const MARKETING_CHAR_GEN_KEY = "seeaitv:marketing-char-gen";
export const MARKETING_CHAR_GEN_TTL_MS = 24 * 60 * 60 * 1000;

export type MarketingCharGenEntry = { scope: string; generationId: string; createdAt: number; dismissed: string[] };

type StorageLike = Pick<Storage, "getItem" | "setItem" | "removeItem">;

/** 读：坏数据当没有；过期的（24 小时）丢掉。 */
export function marketingReadCharGen(storage: StorageLike, now: number): MarketingCharGenEntry[] {
  let raw: unknown;
  try {
    raw = JSON.parse(storage.getItem(MARKETING_CHAR_GEN_KEY) ?? "[]");
  } catch {
    return [];
  }
  if (!Array.isArray(raw)) return [];
  return raw.filter((item): item is MarketingCharGenEntry => Boolean(item)
    && typeof item.scope === "string" && typeof item.generationId === "string" && typeof item.createdAt === "number"
    && now - item.createdAt < MARKETING_CHAR_GEN_TTL_MS)
    .map(item => ({ ...item, dismissed: Array.isArray(item.dismissed) ? item.dismissed.filter((id: unknown): id is string => typeof id === "string") : [] }));
}

export function marketingWriteCharGen(storage: StorageLike, entries: readonly MarketingCharGenEntry[]): void {
  try {
    if (entries.length) storage.setItem(MARKETING_CHAR_GEN_KEY, JSON.stringify(entries));
    else storage.removeItem(MARKETING_CHAR_GEN_KEY);
  } catch {
    // 存不下（隐私模式 / 满了）：候选只在本次打开期间可见，不影响生成本身。
  }
}

/** 记一次生成（同一个 generationId 只记一次）。 */
export function marketingAddCharGen(entries: readonly MarketingCharGenEntry[], scope: string, generationId: string, now: number): MarketingCharGenEntry[] {
  if (entries.some(item => item.generationId === generationId)) return [...entries];
  return [...entries, { scope, generationId, createdAt: now, dismissed: [] }];
}

/** 新建的角色保存了：`new` 下的记录改记到它名下。 */
export function marketingRescopeCharGen(entries: readonly MarketingCharGenEntry[], from: string, to: string): MarketingCharGenEntry[] {
  return entries.map(item => (item.scope === from ? { ...item, scope: to } : item));
}

/** 用户点了某张候选的「不要」。 */
export function marketingDismissCandidate(entries: readonly MarketingCharGenEntry[], generationId: string, mediaId: string): MarketingCharGenEntry[] {
  return entries.map(item => (item.generationId === generationId && !item.dismissed.includes(mediaId) ? { ...item, dismissed: [...item.dismissed, mediaId] } : item));
}

/** 生成产物里的图片 → 候选（只认图片、有媒体 ID 的；按 index 顺序）。 */
export function marketingCandidatesOf(generationId: string, outputs: ReadonlyArray<{ index?: number; type?: string; mediaId?: string | null }>): MarketingCandidate[] {
  return [...outputs]
    .filter(output => output.type === "image" && output.mediaId)
    .sort((a, b) => (a.index ?? 0) - (b.index ?? 0))
    .map(output => ({ mediaId: String(output.mediaId), generationId }));
}

// ---------------------------------------------------------------- 「我的创作」里的项目名

/**
 * 生成记录所属项目的显示名：生成角色的任务不属于任何项目（`projectId = "0"`，dev/05 MKD-28 / R-4），
 * 显示「营销角色生成」；项目被彻底删除的显示调用方给的兜底。
 */
export function marketingCreationProjectLabel(item: { projectId?: string | number | null; projectName?: string | null }, deletedText: string): string {
  if (item.projectName) return item.projectName;
  return String(item.projectId ?? "") === "0" ? "营销角色生成" : deletedText;
}
