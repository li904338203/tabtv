/**
 * 营销页发送：建营销会话的请求体与失败映射（MKT-T-24，营销 dev/03 §3.5；MKT-F-40/41、MKT-UX-09）。纯函数。
 *
 * 一次 `POST /agent/sessions`（带 `Idempotency-Key`）建项目 + 营销会话 + 首条消息：
 * `{createProject:{name}, scene:"marketing", message:{text, refs}, marketing:{picks}, skillCode?, prefs?}`。
 * 商品槽引用商品库商品 `{kind:"marketing_product", id, slot:"product", order:1}`；**不再发 `marketing.product`**
 * （服务端出现即 40001 `legacy_shape`，MKT-T-37 / MKD-27），名称卖点描述由服务端从商品库展开成快照。
 *
 * ⚠️ 几条容易做错的：
 * - **项目名回落顺序**：正文前 20 字 → 所选商品的名称 → 「营销创作」。只选了商品、正文为空是营销页的常态，
 *   沿用创造页的「未命名创作」的话，「我的项目」里会是一排分不出来的同名项目。
 * - **refs 先槽位、后普通引用**：槽位引用带 `slot` / `order`（序列化器同一个 `serializeRefs`），
 *   普通附件与 `@` 引用只带 `{kind,id}`；绝不带缩略图或 URL（GO-27）。
 * - **picks 只有 code**：版本由服务端钉住（MKT-T-17），未选的维度给 null。
 * - **失败只映射、不改输入**：40001 `details.slot` 标槽位、42215 标胶囊，语气按 `details.userFixable` 分类，
 *   **不匹配文案**（CD-48）；断网说断网。
 * - ⚠️ 导出名不与 Vue / Nuxt 自动导入撞名（Nuxt 自动导入 `utils/` 的全部导出，dev/03 R-1 #1）。
 */
import type { components } from "@tabtv/api-client";
import type { ComposerChip } from "./composerKeys";
import { serializeRefs, type SerializedRef } from "./composerRefs";
import type { PickDim, SlotDecl, SlotKey } from "./marketingComposer";
import { marketingSlotRefs, SLOT_KEYS, type MarketingSlotsValue } from "./marketingSlots";
import { serverOptionBlock, type ServerOptionBlock } from "./marketingOptions";
import { apiFailure } from "./agentPanelBridge";
import { quickErrorText } from "./quickGenerate";

export type MarketingSessionRequest = components["schemas"]["AgentCreateSessionRequest"];

/** 项目名上限（与创造页同一条：取前 20 字，按码点截）。 */
export const MARKETING_PROJECT_NAME_MAX = 20;
/** 正文与商品名称都为空时的项目名。 */
export const MARKETING_PROJECT_FALLBACK = "营销创作";

function head(text: string, max: number): string {
  const flat = text.replace(/\s+/g, " ").trim();
  return Array.from(flat).slice(0, max).join("").trim();
}

/** 新项目的名字：正文前 20 字 || 商品名称（同样截 20 字）|| 「营销创作」。 */
export function marketingProjectName(text: string, productName: string): string {
  return head(text, MARKETING_PROJECT_NAME_MAX) || head(productName, MARKETING_PROJECT_NAME_MAX) || MARKETING_PROJECT_FALLBACK;
}

const PICK_ORDER: readonly PickDim[] = ["creative", "hook", "style"];

/** 三个维度各自选中的 code；未选为 null。 */
export function marketingPicksOf(chips: readonly ComposerChip[]): Record<PickDim, string | null> {
  const out = { creative: null, hook: null, style: null } as Record<PickDim, string | null>;
  for (const dim of PICK_ORDER) out[dim] = chips.find(chip => chip.dim === dim)?.code || null;
  return out;
}

export type MarketingRequestInput = {
  text: string;
  /** 四维胶囊（技能 → `skillCode`，其余 → `picks`）。 */
  chips: readonly ComposerChip[];
  /** 槽位内容（`MarketingSlots` 的 v-model）。 */
  slots: MarketingSlotsValue;
  /** 当前槽位声明（只序列化声明里有的槽）。 */
  decls: readonly SlotDecl[];
  /** 普通附件与 `@` 引用（已序列化成 `{kind,id}`）。 */
  refs?: readonly SerializedRef[];
  /** 生效的偏好（含解析出的推理模型）；空对象不带。 */
  prefs?: Record<string, unknown>;
};

/**
 * 组装建营销会话的请求体（dev/03 §3.5）。
 * 普通引用里与槽位同一个 `{kind,id}` 的不重复发（服务端去重键带槽位，一张图在槽里又在附件里会算两条）。
 */
export function buildMarketingSessionRequest(input: MarketingRequestInput): MarketingSessionRequest {
  const text = input.text.trim();
  const slotted = marketingSlotRefs(input.slots, input.decls);
  const productName = input.slots.items.product.find(item => item.kind === "marketing_product")?.label ?? "";
  const inSlots = new Set(slotted.map(ref => `${ref.kind}:${ref.id}`));
  const plain = serializeRefs((input.refs ?? [])
    .filter(ref => !inSlots.has(`${ref.kind}:${ref.id}`))
    .map(ref => ({ kind: ref.kind, id: ref.id, label: "", auto: false })));
  const skillCode = input.chips.find(chip => chip.dim === "skill")?.code;
  return {
    createProject: { name: marketingProjectName(text, productName) },
    scene: "marketing",
    message: { text, refs: [...slotted, ...plain] as MarketingSessionRequest["message"]["refs"] },
    marketing: { picks: marketingPicksOf(input.chips) },
    ...(skillCode ? { skillCode } : {}),
    ...(input.prefs && Object.keys(input.prefs).length ? { prefs: input.prefs } : {})
  };
}

/**
 * 这次发送意图的指纹：内容不变的重试（连点、网络重试）复用同一个幂等键，内容一变换新键。
 * 只看会发出去的东西（槽位只看已完成项的 ID 与顺序）。
 */
export function marketingSendFingerprint(input: MarketingRequestInput): string {
  return JSON.stringify(buildMarketingSessionRequest(input));
}

// ---------------------------------------------------------------- 失败

/** 服务端认出旧形状（MKD-27）时给用户的那一句：输入框是旧页面，刷新即可。 */
export const MARKETING_LEGACY_SHAPE_TEXT = "页面已更新，请刷新后重新选择商品";

export type MarketingSendFailure = {
  /** 输入框下方那一句。 */
  text: string;
  /** 40001 `details.slot`：要标红的槽位。 */
  slot?: SlotKey;
  /** 42215：要标红的胶囊与写进选项弹层的结论。 */
  option?: { dim: PickDim; code: string; block: ServerOptionBlock };
};

function isSlotKey(value: unknown): value is SlotKey {
  return typeof value === "string" && (SLOT_KEYS as readonly string[]).includes(value);
}

function isPickDim(value: unknown): value is PickDim {
  return typeof value === "string" && (PICK_ORDER as readonly string[]).includes(value);
}

/**
 * 建营销会话失败 → 输入框要怎么提示（MKT-AC-04 前端部分）。
 * - 断网：「网络已断开，恢复后再发送」（不标红任何东西 —— 输入本身没错）；
 * - 40001 `details.code = "legacy_shape"`（页面是旧版本、发了旧形状，MKR-15）：「页面已更新，请刷新后重新选择商品」，
 *   带槽位时标那个槽位；
 * - 40001 带 `details.slot`：标那个槽位，原话照说；
 * - 42215：标对应维度的胶囊；语气按 `userFixable`（用户能自己换 → 原话；运营侧的 → 「暂不可用：原话」），
 *   与选项弹层同一个说法（`MarketingOptionPicker`）；
 * - 其余原话照说（`quickErrorText`）。
 */
export function marketingSendFailure(cause: unknown, online: boolean): MarketingSendFailure {
  if (!online) return { text: quickErrorText(cause, false) };
  const failure = apiFailure(cause);
  if (failure.code === 40001 && failure.details.code === "legacy_shape") {
    return { text: MARKETING_LEGACY_SHAPE_TEXT, ...(isSlotKey(failure.details.slot) ? { slot: failure.details.slot } : {}) };
  }
  if (failure.code === 40001 && isSlotKey(failure.details.slot)) {
    return { text: failure.message, slot: failure.details.slot };
  }
  const server = serverOptionBlock(cause);
  if (server && isPickDim(server.kind)) {
    const message = server.block.message || failure.message;
    return {
      text: server.block.userFixable ? message : `暂不可用：${message}`,
      option: { dim: server.kind, code: server.code, block: { ...server.block, message } }
    };
  }
  return { text: quickErrorText(cause, true) };
}
