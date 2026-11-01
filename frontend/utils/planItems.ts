/**
 * 规划确认卡的营销条目：选图展示、换图与 `itemOverrides`（MKT-T-24，营销 dev/03 §3.1 / §8；
 * MKT-F-48 ②、MKT-F-76、MKT-AC-26 ①⑤、MKT-AC-29）。纯函数。
 *
 * 营销会话的计划项（MKT-T-20 `AgentPlanItem`）比通用的多：`key`、`variant` / `variantLabel`、
 * `productIndexes` / `productMediaIds`（选用的商品图，序号从 1 起）、`productTotal`（会话商品图总数）、
 * `imageLimit`（本项最多能用几张 = 模型 × 操作上限 − 角色图占用；0 = 该操作不收图）、`picks`。
 *
 * ⚠️ 几条容易做错的：
 * - **金额一律来自服务端报价**（MK-10）：换了图之后不在页面上估价，提交时服务端重报价；
 *   合计变了回 409 并把新报价写回卡片，由用户再确认一次（不静默批准）。
 * - **换图上限是 `imageLimit`，超了拒绝不截断**（与服务端 `ValidateSubmission` 同一条）：
 *   悄悄截掉的话用户以为用了 5 张，实际发出去 3 张。
 * - **只提交真的改过的条目**：没改的也带上虽然无害，但会让「换过图」在服务端变成每一次批准都成立。
 * - **`notices` 置顶**：口播类的如实提示由服务端追加（不靠 Agent 写），要在条目之前被看到。
 * - 通用会话的卡片没有这些字段 —— `marketingPlanItem` 对它返回 undefined，卡片渲染与改动前一致。
 * - ⚠️ 导出名不与 Vue / Nuxt 自动导入撞名（Nuxt 自动导入 `utils/` 的全部导出）。
 */
import type { components } from "@tabtv/api-client";

export type AgentPlanItem = components["schemas"]["AgentPlanItem"];
export type AgentPlanItemOverride = components["schemas"]["AgentPlanItemOverride"];
export type AgentPlanPick = components["schemas"]["AgentPlanPick"];

const PICK_LABEL: Record<AgentPlanPick["kind"], string> = { creative: "创意", hook: "Hook", style: "风格" };

function obj(value: unknown): Record<string, unknown> {
  return value && typeof value === "object" && !Array.isArray(value) ? value as Record<string, unknown> : {};
}

function intList(value: unknown): number[] {
  return Array.isArray(value) ? value.filter((n): n is number => Number.isInteger(n)) : [];
}

function strList(value: unknown): string[] {
  return Array.isArray(value) ? value.filter((s): s is string => typeof s === "string") : [];
}

function nonNegInt(value: unknown): number | undefined {
  return typeof value === "number" && Number.isInteger(value) && value >= 0 ? value : undefined;
}

/** 卡片上营销条目的展示数据（金额不在这里，仍由 `planCardView` 给）。 */
export type MarketingPlanItemView = {
  key: string;
  /** 「变体 2 · 痛点开场」；没有变体为空。 */
  variantText: string;
  /** 「创意：xx · Hook：xx · 风格：xx」；没有为空。 */
  picksText: string;
  /** 服务端选定的商品图序号（从 1 起）。 */
  productIndexes: number[];
  /** 与 `productIndexes` 同序的媒体 ID。 */
  productMediaIds: string[];
  productTotal: number;
  /** undefined = 这一项不涉及商品图（不显示选图、不能换）。 */
  imageLimit?: number;
  /** 能不能「更换」：有图可选、上限 > 0、这一项没被拦下。 */
  canChange: boolean;
  /** 素材裂变条目的说明（走哪条路、为什么、本项花多少；MKT-T-28 服务端写）；不是裂变条目时没有这个字段。 */
  fissionNote?: string;
};

/** 服务端没写说明时按路线兜底（只说走哪条，不编金额）。 */
const FISSION_FALLBACK: Record<string, string> = {
  video2video: "视频生视频：以源视频为唯一输入",
  regenerate: "重新生成：所选模型不能以视频为输入，按商品图与选项重新生成，不参考源视频画面"
};

/** 计划项是不是营销条目（带 key）；不是返回 undefined。 */
export function marketingPlanItem(raw: unknown): MarketingPlanItemView | undefined {
  const it = obj(raw);
  const key = typeof it.key === "string" ? it.key : "";
  if (!key) return undefined;
  const variant = nonNegInt(it.variant);
  const label = typeof it.variantLabel === "string" ? it.variantLabel.trim() : "";
  const variantText = [variant ? `变体 ${variant}` : "", label].filter(Boolean).join(" · ");
  const picks = (Array.isArray(it.picks) ? it.picks : []).map(obj)
    .filter(p => typeof p.kind === "string" && typeof p.code === "string")
    .map(p => `${PICK_LABEL[p.kind as AgentPlanPick["kind"]] ?? String(p.kind)}：${typeof p.name === "string" && p.name ? p.name : String(p.code)}`);
  const productTotal = nonNegInt(it.productTotal) ?? 0;
  const imageLimit = nonNegInt(it.imageLimit);
  const blocked = Boolean(it.blockedCode) || Boolean(it.blockedReason);
  const route = typeof it.fissionRoute === "string" ? it.fissionRoute : "";
  const fissionNote = (typeof it.fissionNote === "string" ? it.fissionNote.trim() : "") || FISSION_FALLBACK[route] || "";
  return {
    key,
    variantText,
    picksText: picks.join(" · "),
    productIndexes: intList(it.productIndexes),
    productMediaIds: strList(it.productMediaIds),
    productTotal,
    ...(imageLimit !== undefined ? { imageLimit } : {}),
    canChange: !blocked && imageLimit !== undefined && imageLimit > 0 && productTotal > 0,
    ...(fissionNote ? { fissionNote } : {})
  };
}

/**
 * 「共 X 张，本项用 Y 张（模型上限 L）」（MKT-AC-26 ①）。
 * - 上限为 0：该操作不收图，说清楚而不是显示「本项用 0 张」（看起来像漏选）；
 * - 会话里没有商品图 / 这一项不涉及商品图：空串（不显示这一行）。
 */
export function planItemProductText(input: { productTotal: number; imageLimit?: number; selected: number }): string {
  if (input.imageLimit === undefined || input.productTotal <= 0) return "";
  if (input.imageLimit === 0) return `共 ${input.productTotal} 张，本项不使用商品图（所选模型此操作不收图）`;
  return `共 ${input.productTotal} 张，本项用 ${input.selected} 张（模型上限 ${input.imageLimit}）`;
}

/**
 * 换图选择是否合法（与服务端 `ValidateSubmission` 同一组规则）；合法返回空串。
 * 至少 1 张、不超过上限、序号在 1..总数、不重复。
 */
export function planSelectionError(indexes: readonly number[], limit: number, total: number): string {
  if (!indexes.length) return "至少选 1 张商品图";
  if (indexes.length > limit) return `最多选 ${limit} 张（所选模型的上限）`;
  if (indexes.some(i => !Number.isInteger(i) || i < 1 || i > total)) return `只能从这 ${total} 张商品图里选`;
  if (new Set(indexes).size !== indexes.length) return "同一张图不能选两次";
  return "";
}

/**
 * 点一张图：已选的取消，未选的追加到末尾（选的先后即发给模型的顺序）。
 * 已到上限时**不加**，并给原因 —— 不挤掉最早选的那张（用户不会注意到哪张被挤掉了）。
 */
export function togglePlanSelection(current: readonly number[], index: number, limit: number): { indexes: number[]; reason: string } {
  if (current.includes(index)) return { indexes: current.filter(i => i !== index), reason: "" };
  if (current.length >= limit) return { indexes: [...current], reason: `最多选 ${limit} 张，先取消一张再选` };
  return { indexes: [...current, index], reason: "" };
}

function sameList(a: readonly number[], b: readonly number[]): boolean {
  return a.length === b.length && a.every((v, i) => v === b[i]);
}

/**
 * 组装 `itemOverrides`：只带真的改过的营销条目（顺序也算改过 —— 顺序就是参考图的锚定顺序）。
 * 没有改动返回空数组。键不在卡片上的选择丢弃（卡片被重新报价后旧选择不能带过去）。
 */
export function planItemOverrides(items: readonly unknown[], selections: Readonly<Record<string, readonly number[]>>): AgentPlanItemOverride[] {
  const out: AgentPlanItemOverride[] = [];
  for (const raw of items) {
    const view = marketingPlanItem(raw);
    if (!view) continue;
    const picked = selections[view.key];
    if (!picked || sameList(picked, view.productIndexes)) continue;
    out.push({ key: view.key, productIndexes: [...picked] });
  }
  return out;
}

/** 批准的提交体：有换图时带 `itemOverrides`，否则与通用卡片逐字一致（`{decision:"approve"}`）。 */
export function planApproveSubmission(overrides: readonly AgentPlanItemOverride[]): Record<string, unknown> {
  return overrides.length ? { decision: "approve", itemOverrides: overrides.map(o => ({ key: o.key, productIndexes: [...o.productIndexes] })) } : { decision: "approve" };
}

/** 置顶的如实提示（`payload.notices`）：去空、去重、保持服务端顺序。 */
export function planNotices(payload: unknown): string[] {
  const raw = obj(payload).notices;
  const seen = new Set<string>();
  const out: string[] = [];
  for (const item of strList(raw)) {
    const text = item.trim();
    if (!text || seen.has(text)) continue;
    seen.add(text);
    out.push(text);
  }
  return out;
}

/** 序号 → 媒体 ID（会话商品图 `sceneInput.products`，[0] 为第 1 张）；取不到的丢掉。 */
export function planProductMedia(products: readonly string[], indexes: readonly number[]): string[] {
  return indexes.map(i => products[i - 1]).filter((id): id is string => typeof id === "string" && id.length > 0);
}

// ---------------------------------------------------------------- 变体分组（MKT-T-29，MKT-F-50 / MKT-AC-17）

export type PlanVariantGroup = {
  /** 变体序号；0 = 不属于任何变体的条目（如共用的配音）。 */
  variant: number;
  /** 「变体 2 · 痛点开场」；variant 0 为「其他」。 */
  title: string;
  /** 这一组的条目在 `payload.items` 里的下标（保持服务端顺序）。 */
  indexes: number[];
  /** 本组小计（服务端逐项报价之和；被拦下的条目报价为 0，不计）。 */
  subtotal: number;
};

/**
 * 确认卡按变体分组：**至少两个不同变体**时才分组（一个变体分组只是多一层标题），否则返回空数组、卡片照旧平铺。
 * 组的顺序 = 变体序号升序（与画布上从左到右的排列一致），0 组放最后；组内保持服务端顺序。
 * ⚠️ 小计只是把服务端每一行的报价加起来，**合计仍以服务端 `total` 为准**（不在页面上另算一个合计）。
 */
export function planVariantGroups(items: readonly unknown[]): PlanVariantGroup[] {
  const groups = new Map<number, PlanVariantGroup>();
  items.forEach((raw, index) => {
    const it = obj(raw);
    const variant = nonNegInt(it.variant) ?? 0;
    let group = groups.get(variant);
    if (!group) {
      group = { variant, title: variant ? `变体 ${variant}` : "其他", indexes: [], subtotal: 0 };
      groups.set(variant, group);
    }
    const label = typeof it.variantLabel === "string" ? it.variantLabel.trim() : "";
    if (variant && label && group.title === `变体 ${variant}`) group.title = `变体 ${variant} · ${label}`;
    group.indexes.push(index);
    const blocked = Boolean(it.blockedCode) || Boolean(it.blockedReason);
    group.subtotal += blocked ? 0 : nonNegInt(it.credits) ?? 0;
  });
  const variants = [...groups.keys()].filter(v => v > 0);
  if (variants.length < 2) return [];
  return [...groups.values()].sort((a, b) => (a.variant || Number.MAX_SAFE_INTEGER) - (b.variant || Number.MAX_SAFE_INTEGER));
}

/**
 * 批准失败时该标红哪一条（MKT-T-24 留下）：42215 `details.key` 是被营销选项挡住的条目键；
 * 其余错误不指向具体条目，返回空串。
 */
export function planFailedItemKey(failure: { code?: number; details: Record<string, unknown> }): string {
  if (failure.code !== 42215) return "";
  return typeof failure.details.key === "string" ? failure.details.key : "";
}

// ---------------------------------------------------------------- 执行进度（MKT-T-29，MKT-F-51 / MKT-AC-18）

export type AgentPlanProgress = components["schemas"]["AgentPlanProgress"];
export type AgentPlanItemProgress = components["schemas"]["AgentPlanItemProgress"];

/** 余额不足没提交的那一项（受理时被账本拒）与「因前面余额不足停掉」的码；按码判不按文案。 */
export const PLAN_ERR_INSUFFICIENT = "INSUFFICIENT_CREDITS";
export const PLAN_ERR_STOPPED = "plan_stopped_insufficient_credits";

export type PlanProgressTone = "running" | "partial" | "success" | "failed" | "pending";

export type PlanProgressItemView = {
  key: string;
  /** 「变体 2 · 痛点开场」，没有变体说明时用条目标题。 */
  title: string;
  state: AgentPlanItemProgress["state"];
  /** 「已完成」「生成中」「失败」…… */
  stateText: string;
  /** 没成功的原因（余额不足 / 被停掉 / 生成失败分开说）；成功与进行中为空。 */
  reason: string;
  /** 能不能点「重试」；不能时 `retryBlocked` 说清为什么。 */
  canRetry: boolean;
  retryBlocked: string;
};

export type PlanProgressView = {
  tone: PlanProgressTone;
  /** 「部分成功（2/3）」—— 直接用服务端的 succeeded / total，不自己数。 */
  summary: string;
  /** 「失败 1 · 余额不足停止 1」；没有为空。 */
  detail: string;
  items: PlanProgressItemView[];
};

const STATE_TEXT: Record<AgentPlanItemProgress["state"], string> = {
  pending: "未开始",
  submitted: "生成中",
  success: "已完成",
  failed: "失败",
  stopped: "已停止"
};

function progressItemTitle(item: AgentPlanItemProgress): string {
  const variant = nonNegInt(item.variant);
  const label = (item.variantLabel ?? "").trim();
  if (variant) return [`变体 ${variant}`, label || item.label].filter(Boolean).join(" · ");
  return item.label || item.key;
}

/** 失败原因：余额不足（这一项被账本拒）、被停掉（前面一项余额不足）、生成失败（上游）三种分开说。 */
function progressReason(item: AgentPlanItemProgress): string {
  if (item.state === "stopped" || item.errorCode === PLAN_ERR_STOPPED) return "前面有一项余额不足，本项已停止：没有提交，也没有扣费";
  if (item.state !== "failed") return "";
  if (item.errorCode === PLAN_ERR_INSUFFICIENT) return "余额不足，没有提交，也没有扣费；充值后可重试";
  return "生成没有成功，扣除的积分会退回（以本轮账单为准）";
}

/**
 * 批量执行进度的展示（「部分成功（2/3）」不摊平）。返回 undefined = 不显示：
 * 没有进度、只有一项（单项失败已有步骤行上的「重试这一步」），或这一项还都没开始。
 *
 * 重试可用性（只读服务端给的状态，**不在前端判断要不要停后续**）：
 * - `failed`：可重试（余额不足的那一项要先充值，文案里说清）；
 * - `stopped`：只要还有「余额不足」的那一项没解决（它仍是 failed + INSUFFICIENT_CREDITS），就先重试那一项 ——
 *   直接重试被停掉的会再次被停掉；那一项解决之后才可以单独重试；
 * - 成功 / 进行中 / 未开始：不给重试；
 * - `blockedReason`（只读成员、Agent 正忙等，与输入框同一判定）非空时全部不可点，并说那一句。
 */
export function planProgressView(progress: AgentPlanProgress | null | undefined, blockedReason = ""): PlanProgressView | undefined {
  if (!progress || !Array.isArray(progress.items) || progress.total < 2) return undefined;
  if (progress.status === "pending") return undefined;
  const ofTotal = `（${progress.succeeded}/${progress.total}）`;
  const SUMMARY: Record<AgentPlanProgress["status"], [PlanProgressTone, string]> = {
    running: ["running", `执行中，已完成 ${progress.succeeded}/${progress.total}`],
    partial_success: ["partial", `部分成功${ofTotal}`],
    success: ["success", `全部成功${ofTotal}`],
    failed: ["failed", `全部未成功${ofTotal}`],
    pending: ["pending", "尚未开始"]
  };
  const [tone, summary] = SUMMARY[progress.status] ?? ["failed", `未知状态${ofTotal}`];
  const insufficient = progress.items.find(item => item.state === "failed" && item.errorCode === PLAN_ERR_INSUFFICIENT);
  const failedUpstream = progress.items.filter(item => item.state === "failed" && item.errorCode !== PLAN_ERR_INSUFFICIENT).length;
  const failedCredits = progress.items.filter(item => item.state === "failed" && item.errorCode === PLAN_ERR_INSUFFICIENT).length;
  const detail = [
    failedUpstream ? `失败 ${failedUpstream}` : "",
    failedCredits ? `余额不足 ${failedCredits}` : "",
    progress.stopped ? `余额不足停止 ${progress.stopped}` : "",
    progress.running ? `生成中 ${progress.running}` : ""
  ].filter(Boolean).join(" · ");
  const items = progress.items.map((item): PlanProgressItemView => {
    let canRetry = item.state === "failed" || item.state === "stopped";
    let retryBlocked = "";
    if (canRetry && item.state === "stopped" && insufficient) {
      canRetry = false;
      retryBlocked = `先充值并重试「${progressItemTitle(insufficient)}」`;
    }
    if (canRetry && blockedReason) {
      canRetry = false;
      retryBlocked = blockedReason;
    }
    return {
      key: item.key,
      title: progressItemTitle(item),
      state: item.state,
      stateText: item.state === "failed" && item.errorCode === PLAN_ERR_INSUFFICIENT ? "余额不足" : STATE_TEXT[item.state] ?? item.state,
      reason: progressReason(item),
      canRetry,
      retryBlocked
    };
  });
  return { tone, summary, detail, items };
}

/**
 * 「重试」发给 Agent 的那句话：交给 Agent 重新报价、走额度检查，服务端建**新步骤、新幂等键**；
 * 点名条目键，避免 Agent 把已经成功的变体也重跑一遍（那也是在花钱）。
 */
export function planRetryMessage(item: Pick<PlanProgressItemView, "key" | "title">): string {
  return `请只重试计划里没有成功的「${item.title}」（条目 ${item.key}），已经成功的变体不要重做`;
}
