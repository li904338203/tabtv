/**
 * 营销输入框的槽位区：商品 / 角色 / 源视频（MKT-T-12 → MKT-T-38 改为方形卡片，营销 dev/05 §4.4、MKD-38；
 * MKT-F-90/91、MKT-UX-13）。纯函数。
 *
 * 槽位组件（`components/agent/composer/marketing/MarketingSlots.vue`）只调用这里：选中 / 移除、上传三态（源视频）、
 * 必填与上限文案、序列化成请求体的 `refs`。组件只负责点击、上传与缩略图；商品 / 角色的选择在弹窗里（输入框挂）。
 *
 * 值结构（dev/05 §4.4）：
 * - 商品槽：`[{kind:"marketing_product", id, label, thumbMediaId, imageCount}]`，**最多 1 个**（`maxProductsPerSession`）；
 * - 角色槽：`[{kind:"marketing_character" | "asset", id, label, thumbMediaId}]`，最多 `maxCharacterRefs` 个；
 * - 源视频槽：上传的媒体或 `@` 来的视频（选择方式不变）。
 * 商品名称 / 卖点不再在输入框里填（商品库提供，MKD-25）。
 *
 * ⚠️ 几条容易做错的：
 * - **上限是配置值不是常量**：取 `/marketing/config` 的 `maxProductsPerSession` / `maxCharacterRefs`，与技能声明的
 *   `maxCount` 取小。前端写死的话，配置一变就「界面允许、提交被拒」（MKD-34）。
 * - **只序列化当前槽认的 kind**：商品槽里残留一项 `media`（旧形状）的话，服务端回 40001 `legacy_shape` ——
 *   这里直接不发，草稿迁移另外丢弃并提示（`marketingDraft`）。
 * - **只序列化完成的项**，`order` 是界面顺序（从 1 起、连续）。
 * - ⚠️ 导出名不与 Vue / Nuxt 自动导入撞名（不叫 `toRefs` / `reorder`）：Nuxt 自动导入 `utils/` 的全部导出，
 *   撞名时构建只打一条 WARN 并悄悄顶替（dev/03 R-1 #1 同一条）。
 */
import { SLOT_NOUN, SLOT_VERB, type MarketingComposerState, type SlotConflict, type SlotDecl, type SlotKey, slotConflicts } from "./marketingComposer";

export type SlotItemStatus = "uploading" | "failed" | "done";

/** 槽位项引用的种类：商品库商品 / 角色库角色 / 脚本角色资产 / 上传的媒体（源视频）。 */
export type SlotItemKind = "media" | "asset" | "marketing_product" | "marketing_character";

/** 槽位里的一项（一个商品 / 一个角色 / 一段源视频）。 */
export type SlotItem = {
  /** 本地键（上传完成前没有服务端 ID）。 */
  localId: string;
  kind: SlotItemKind;
  /** 服务端 ID（商品 ID / 角色 ID / 资产 ID / 媒体 ID）；上传完成前为空。 */
  id?: string;
  status: SlotItemStatus;
  /** 显示名（商品名 / 角色名 / 文件名）。 */
  label: string;
  /** 取缩略图用的媒体 ID（受保护端点，走 useAuthenticatedMedia）；刚上传的用本地 object URL，不填。 */
  thumbMediaId?: string;
  /** image / video（决定显示缩略图还是图标）。 */
  hint?: string;
  /** 商品的图片张数（卡上「共 N 张」）。 */
  imageCount?: number;
  /** 失败原因（原样显示）。 */
  error?: string;
};

export type SlotItems = Record<SlotKey, SlotItem[]>;

export type MarketingSlotsValue = {
  items: SlotItems;
};

/** `/marketing/config` 里与槽位相关的上限（缺失时回落代码上限）。 */
export type MarketingSlotLimits = {
  maxProductsPerSession?: number | null;
  maxCharacterRefs?: number | null;
};

export const SLOT_KEYS: readonly SlotKey[] = ["product", "character", "source_video"];

/** 代码上限（设计建议值，dev/05 MKD-34）：技能声明与配置只能在它之内取小。 */
export const SLOT_CODE_MAX: Record<SlotKey, number> = { product: 1, character: 2, source_video: 1 };

/** 各槽认的引用种类（与服务端 MKD-26 / MKD-27 一致）。 */
export const SLOT_ITEM_KINDS: Record<SlotKey, readonly SlotItemKind[]> = {
  product: ["marketing_product"],
  character: ["marketing_character", "asset"],
  source_video: ["media", "asset"]
};

/** 量词（「最多 1 个商品」「最多 2 个角色」「最多 1 段源视频」）。 */
const SLOT_UNIT: Record<SlotKey, string> = { product: "个", character: "个", source_video: "段" };

export function emptySlotsValue(): MarketingSlotsValue {
  return { items: { product: [], character: [], source_video: [] } };
}

/** 这一项放得进这个槽吗（种类不对的一律不发、草稿里也不留）。 */
export function slotItemAccepted(key: SlotKey, kind: string): kind is SlotItemKind {
  return (SLOT_ITEM_KINDS[key] as readonly string[]).includes(kind);
}

// ---------------------------------------------------------------- 上限

function configMax(key: SlotKey, limits: MarketingSlotLimits | null | undefined): number {
  const value = key === "product" ? limits?.maxProductsPerSession : key === "character" ? limits?.maxCharacterRefs : null;
  return value && value > 0 ? value : SLOT_CODE_MAX[key];
}

/** 一个槽的条数上限：声明的 `maxCount`（0 / 不传 = 不收紧）与配置取小。 */
export function slotLimit(decl: Pick<SlotDecl, "key" | "maxCount">, limits?: MarketingSlotLimits | null): number {
  const scene = configMax(decl.key, limits);
  return decl.maxCount > 0 ? Math.min(decl.maxCount, scene) : scene;
}

/** 还能不能加；不能时给出「最多 2 个角色」（数字取自配置）。 */
export function slotAddGate(items: readonly SlotItem[], limit: number, key: SlotKey): { ok: boolean; remaining: number; reason: string } {
  const remaining = Math.max(0, limit - items.length);
  return remaining > 0
    ? { ok: true, remaining, reason: "" }
    : { ok: false, remaining: 0, reason: `最多 ${limit} ${SLOT_UNIT[key]}${SLOT_NOUN[key]}` };
}

/**
 * 加入若干项：超出上限的部分不加，返回被挡掉的条数与提示。
 * 同一个服务端 ID 已在槽里的不重复加（`@` 同一个角色两次）。种类不对的不加。
 */
export function addSlotItems(
  items: readonly SlotItem[],
  incoming: readonly SlotItem[],
  limit: number,
  key: SlotKey
): { items: SlotItem[]; rejected: number; notice: string } {
  const next = [...items];
  let rejected = 0;
  for (const item of incoming) {
    if (!slotItemAccepted(key, item.kind)) continue;
    if (item.id && next.some(entry => entry.kind === item.kind && entry.id === item.id)) continue;
    if (next.length >= limit) {
      rejected += 1;
      continue;
    }
    next.push(item);
  }
  const notice = rejected > 0 ? `最多 ${limit} ${SLOT_UNIT[key]}${SLOT_NOUN[key]}，有 ${rejected} ${SLOT_UNIT[key]}未添加` : "";
  return { items: next, rejected, notice };
}

// ---------------------------------------------------------------- 排序 / 删除 / 三态

/**
 * 把 `from` 位置的项移到 `to`（排第一即主图）。越界返回原数组的拷贝（拖到外面松手）。
 * 商品编辑弹窗的图片条也用它（`marketingLibrary`），两处同一个语义。
 */
export function moveSlotItem<T>(items: readonly T[], from: number, to: number): T[] {
  if (from === to || from < 0 || from >= items.length || to < 0 || to >= items.length) return [...items];
  const next = [...items];
  const [moved] = next.splice(from, 1);
  next.splice(to, 0, moved!);
  return next;
}

export function removeSlotItem(items: readonly SlotItem[], localId: string): SlotItem[] {
  return items.filter(item => item.localId !== localId);
}

/** 按 localId 改一项（上传完成 / 失败 / 重试）；找不到（已被删）原样返回 —— 晚到的上传结果不能把删掉的项带回来。 */
export function patchSlotItem(items: readonly SlotItem[], localId: string, patch: Partial<SlotItem>): SlotItem[] {
  return items.map(item => (item.localId === localId ? { ...item, ...patch } : item));
}

// ---------------------------------------------------------------- 商品（选择弹窗 → 卡片）

/** 商品弹窗选出来的那一项（列表摘要或详情都行）。 */
export type MarketingPickedProduct = { id: string; name: string; mainMediaId?: string | null; imageCount: number };

/** 商品 → 商品槽的一项。 */
export function marketingProductSlotItem(product: MarketingPickedProduct, localId: string): SlotItem {
  return {
    localId,
    kind: "marketing_product",
    id: product.id,
    status: "done",
    label: product.name,
    hint: "image",
    imageCount: product.imageCount,
    ...(product.mainMediaId ? { thumbMediaId: product.mainMediaId } : {})
  };
}

/** 选中商品（替换掉原来那个 —— 一次创作只选 1 个，MK-22）；传 null 即移除。 */
export function marketingSetProduct(value: MarketingSlotsValue, product: MarketingPickedProduct | null, localId: string): MarketingSlotsValue {
  return { ...value, items: { ...value.items, product: product ? [marketingProductSlotItem(product, localId)] : [] } };
}

/** 当前选中的商品 ID；没选为空串。 */
export function marketingSelectedProductId(value: MarketingSlotsValue): string {
  return value.items.product.find(item => item.kind === "marketing_product" && item.id)?.id ?? "";
}

/**
 * 商品被编辑（名称 / 主图 / 张数变了）：卡片上那一项跟着更新；不是当前选中的原样返回。
 * 被删除：从卡片移除（MKT-UX-18），`removed` 告诉调用方要不要提示一次。
 */
export function marketingSyncProduct(
  value: MarketingSlotsValue,
  change: { kind: "updated"; product: MarketingPickedProduct } | { kind: "deleted"; id: string }
): { value: MarketingSlotsValue; changed: boolean; removed: boolean } {
  const current = value.items.product[0];
  const id = change.kind === "updated" ? change.product.id : change.id;
  if (!current || current.kind !== "marketing_product" || current.id !== id) return { value, changed: false, removed: false };
  if (change.kind === "deleted") return { value: { ...value, items: { ...value.items, product: [] } }, changed: true, removed: true };
  return { value: marketingSetProduct(value, change.product, current.localId), changed: true, removed: false };
}

// ---------------------------------------------------------------- 上报给联动（marketingComposer）

/**
 * 各槽已有内容的条数 → `MarketingComposerState.filled`。
 * 计上传中与已完成的（失败的不算：它不会被发出去，留着它不能让「请选择商品」消失）；种类不对的也不算。
 */
export function slotFilled(value: MarketingSlotsValue): MarketingComposerState["filled"] {
  const out: MarketingComposerState["filled"] = {};
  for (const key of SLOT_KEYS) {
    const count = value.items[key].filter(item => item.status !== "failed" && slotItemAccepted(key, item.kind)).length;
    if (count > 0) out[key] = count;
  }
  return out;
}

export function slotsUploading(value: MarketingSlotsValue): boolean {
  return SLOT_KEYS.some(key => value.items[key].some(item => item.status === "uploading"));
}

/** 有上传失败的项时的提示（发送前要先重试或移除，否则那一项会悄悄不被发出去）。 */
export function slotsFailedText(value: MarketingSlotsValue): string {
  const failed = SLOT_KEYS.reduce((sum, key) => sum + value.items[key].filter(item => item.status === "failed").length, 0);
  return failed > 0 ? `有 ${failed} 项上传失败，请重试或移除` : "";
}

/** 必填槽缺失时的那一句（「请选择商品」）。与 `marketingSendGate` 同源。 */
export function slotRequiredText(key: SlotKey): string {
  return `请${SLOT_VERB[key]}${SLOT_NOUN[key]}`;
}

/** 第一个缺失的必填槽的文案（「请选择商品」）；都满足返回 ""。 */
export function requiredSlotText(decls: readonly SlotDecl[], value: MarketingSlotsValue): string {
  const filled = slotFilled(value);
  const missing = decls.find(decl => decl.required && !(filled[decl.key] ?? 0));
  return missing ? slotRequiredText(missing.key) : "";
}

/**
 * 换技能前判断已有内容放不放得进新声明（供 `applySkill` 的 `needsConfirm`）。
 * 判定本身复用 `marketingComposer.slotConflicts`，这里只把槽位内容换算成条数。
 */
export function slotsCompatible(value: MarketingSlotsValue, next: readonly SlotDecl[]): { ok: boolean; conflicts: SlotConflict[] } {
  const conflicts = slotConflicts(slotFilled(value), next);
  return { ok: conflicts.length === 0, conflicts };
}

/** 清空若干槽（用户确认换技能之后）；返回被清掉的项，组件据此 revoke 本地预览。 */
export function clearSlotItems(value: MarketingSlotsValue, keys: readonly SlotKey[]): { value: MarketingSlotsValue; removed: SlotItem[] } {
  const removed: SlotItem[] = [];
  const items = { ...value.items };
  for (const key of keys) {
    removed.push(...items[key]);
    items[key] = [];
  }
  return { value: { ...value, items }, removed };
}

// ---------------------------------------------------------------- 序列化

/** 槽位引用：`{kind, id, slot, order}`，绝不带缩略图、名称或 URL（GO-27）。 */
export type MarketingSlotRef = { kind: SlotItemKind; id: string; slot: SlotKey; order: number };

/**
 * 槽位 → 请求体 `message.refs` 里的那一段。
 *
 * - 只发**当前声明里有**的槽（换技能后没清的旧槽不发）；
 * - 只发完成、有服务端 ID、且**种类是这个槽认的**项（商品槽只有 `marketing_product`）；
 * - `order` 是界面顺序、每槽从 1 起连续；同一槽里同一个 `{kind,id}` 只发一次。
 */
export function marketingSlotRefs(value: MarketingSlotsValue, decls: readonly SlotDecl[]): MarketingSlotRef[] {
  const refs: MarketingSlotRef[] = [];
  for (const key of SLOT_KEYS) {
    if (!decls.some(decl => decl.key === key)) continue;
    const seen = new Set<string>();
    let order = 0;
    for (const item of value.items[key]) {
      if (item.status !== "done" || !item.id || !slotItemAccepted(key, item.kind)) continue;
      const dedupe = `${item.kind}:${item.id}`;
      if (seen.has(dedupe)) continue;
      seen.add(dedupe);
      order += 1;
      refs.push({ kind: item.kind, id: item.id, slot: key, order });
    }
  }
  return refs;
}

// ---------------------------------------------------------------- 本地预览（源视频上传）

/**
 * 本地预览（object URL）该释放哪些：组件持有的 localId 里，已不在任何槽里的那些（删除 / 清空 / 被替换）。
 * 卸载时组件全部释放，不经过这里。
 */
export function orphanPreviewIds(owned: Iterable<string>, value: MarketingSlotsValue): string[] {
  const present = new Set(SLOT_KEYS.flatMap(key => value.items[key].map(item => item.localId)));
  return [...owned].filter(id => !present.has(id));
}

/** 槽位接受什么文件（`<input accept>`）。 */
export function slotAcceptAttr(decl: Pick<SlotDecl, "accept">): string {
  const out: string[] = [];
  if (decl.accept.includes("image")) out.push("image/*");
  if (decl.accept.includes("video")) out.push("video/*");
  return out.join(",");
}

/** 文件能不能进这个槽（音频进源视频槽的话，渲染出来那段是黑屏而任务"成功"）。 */
export function slotAcceptsFile(decl: Pick<SlotDecl, "accept">, mime: string): boolean {
  if (mime.startsWith("image/")) return decl.accept.includes("image");
  if (mime.startsWith("video/")) return decl.accept.includes("video");
  return false;
}
