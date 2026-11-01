/**
 * 创造页输入框的价格展示与发送键可用性（AGT-42，设计 02 §4.6、§8；开发 03 §9）。纯函数。
 *
 * ⚠️ **页面不自己算价**：金额一律来自 `POST /generations/estimate`（与受理同一条报价路径），
 *    这里只负责「怎么说」。前端按「单价 × 数量」自己估的话，和服务端按定价目录算出来的不是一回事。
 */

export type PriceUnit = "per_time" | "per_image" | "per_second" | "per_kchar" | string;

export type QuoteState = {
  loading: boolean;
  /** 服务端报的总价；未知（还没报、报价失败）为 undefined。 */
  credits?: number;
  unit?: PriceUnit;
  /** 有折扣时的原价（服务端给了才显示，前端不推算）。 */
  originalCredits?: number;
  discountReason?: string;
  /** 报价失败的服务端原话（模型不可用、需要会员……）。 */
  error?: string;
  count?: number;
  durationSec?: number;
};

export type PriceView = {
  /** 发送键左边那颗：`✦60` / `报价中` / `—`。 */
  label: string;
  /** 明细里的算式（单价 × 数量）；拆不出单价时只说总价。 */
  formula: string;
  /** 原价划线（只在服务端给了原价且高于现价时）。 */
  original?: string;
  discountReason?: string;
  /** 报价失败时显示在价格处的原因。 */
  error?: string;
};

const UNIT_WORDS: Record<string, string> = {
  per_image: "张",
  per_second: "秒",
  per_time: "次"
};

export function priceView(state: QuoteState): PriceView {
  if (state.error) return { label: "—", formula: "", error: state.error };
  if (state.credits === undefined) return { label: state.loading ? "报价中" : "—", formula: "" };
  const total = state.credits;
  const view: PriceView = { label: `✦${total}`, formula: formulaOf(state) };
  if (state.originalCredits !== undefined && state.originalCredits > total) {
    view.original = `✦${state.originalCredits}`;
    if (state.discountReason) view.discountReason = state.discountReason;
  }
  return view;
}

/**
 * 「单价 × 数量」。⚠️ 单价由总价**反推**，除不尽时不硬拆（写出一个带小数的单价，用户会以为算错了），
 * 只说总价。按字数计价的说清是按字数。
 */
function formulaOf(state: QuoteState): string {
  const total = state.credits ?? 0;
  const unit = state.unit ?? "";
  if (unit === "per_kchar") return `按字数计费，共 ✦${total}`;
  const qty = unit === "per_second" ? state.durationSec ?? 5 : state.count ?? 1;
  const word = UNIT_WORDS[unit];
  if (!word || qty <= 1 || total % qty !== 0) return `共 ✦${total}`;
  return `✦${total / qty}/${word} × ${qty} ${word} = ✦${total}`;
}

export type SendGate = {
  disabled: boolean;
  /** 禁用时悬停/下方显示的原因；可用时为空串。 */
  reason: string;
  /** 余额不足时还差多少（给「去充值」用）。 */
  shortfall?: number;
};

export type SendGateInput = {
  readOnly: boolean;
  sending: boolean;
  uploading: boolean;
  hasContent: boolean;
  hasModel: boolean;
  /** 必填参数缺失的说明（如音频没选音色）；没有缺失为空。 */
  missingParam?: string;
  /** 当前模式没有任何可用模型时的原因（如「暂无可用的音频模型」）。 */
  modeUnavailable?: string;
  quote: QuoteState;
  /** 余额；不知道（还没取到）为 undefined —— **不猜**。 */
  balance?: number;
};

/**
 * 发送键能不能点、不能点为什么。
 *
 * ⚠️ 理由给**最根本的那一条**（§5.18）：只读排在「余额不足」之前 —— 否则只读成员会去充值，
 *    充完发现还是点不动，而真正的原因从头到尾都是他没有权限。
 */
export function sendGate(input: SendGateInput): SendGate {
  if (input.readOnly) return { disabled: true, reason: "你在该工作区是只读，无法发起创作" };
  if (input.sending) return { disabled: true, reason: "正在发送" };
  if (input.modeUnavailable) return { disabled: true, reason: input.modeUnavailable };
  if (input.uploading) return { disabled: true, reason: "附件上传中，完成后再发送" };
  if (!input.hasContent) return { disabled: true, reason: "先写下想生成的内容" };
  if (!input.hasModel) return { disabled: true, reason: "请先选择模型" };
  if (input.missingParam) return { disabled: true, reason: input.missingParam };
  if (input.quote.error) return { disabled: true, reason: input.quote.error };
  if (input.quote.credits === undefined) {
    return { disabled: true, reason: input.quote.loading ? "正在报价" : "还没有报价" };
  }
  if (input.balance !== undefined && input.balance < input.quote.credits) {
    const shortfall = input.quote.credits - input.balance;
    return { disabled: true, reason: `积分不足，还差 ✦${shortfall}`, shortfall };
  }
  return { disabled: false, reason: "" };
}

// ---------------------------------------------------------------- Agent 模式（AGT-29，设计 02 §4.5 / §4.6）

/** 「生成费用在确认规划时给出总价」：Agent 模式明细里必有的一句（第一次用就让用户知道确认前不扣生成费）。 */
export const AGENT_GENERATION_NOTE = "生成费用在确认规划时给出总价，确认前不会扣费";

const AGENT_UNIT_WORDS: Record<string, string> = {
  per_image: "张",
  per_second: "秒",
  per_time: "次",
  per_kchar: "千字"
};

/** 偏好浮层里一个模型的样子：不可用的**照样列出**，置灰并写原因。 */
export type ModelOption = {
  code: string;
  name: string;
  available: boolean;
  /** 不可用时的原因（服务端原话，如「需要会员」）；可用时为空。 */
  reason?: string;
  /** 单价（服务端报价）；还没报出来为 undefined。 */
  price?: number;
  unit?: PriceUnit;
  /** 角标（只来自目录里真有的字段，见 `utils/composerUi.modelBadges`）；没有为空。 */
  badges?: string[];
  /** 原生出声（`/models` 的 `hasNativeAudio`，只对视频模型有意义）；目录没给为 undefined。营销弹层据此置灰依赖出声的 Hook。 */
  hasNativeAudio?: boolean;
};

/**
 * 单价文案：`✦4/秒`、`✦2/张`；推理模型按轮（`✦2/轮`）。
 * ⚠️ 价格来自服务端报价（`/generations/estimate` 报 1 个单位），这里不乘不除。
 */
export function unitPriceText(price: number | undefined, unit: PriceUnit | undefined, perTurn = false): string {
  if (price === undefined) return "";
  if (perTurn) return `✦${price}/轮`;
  const word = AGENT_UNIT_WORDS[unit ?? ""];
  return word ? `✦${price}/${word}` : `✦${price}`;
}

/** 报价失败的原因 → 模型不可用的说法（原话带出；没有原话时给一句不含糊的）。 */
export function modelUnavailableReason(cause: unknown): string {
  const message = typeof cause === "object" && cause !== null && "message" in cause
    ? String((cause as { message?: unknown }).message ?? "").trim()
    : "";
  return message && message !== "请求失败" ? message : "暂时不可用";
}

export type AgentPriceInput = {
  /** 所选推理模型。 */
  reasoning?: ModelOption;
  /** 所选视频 / 图片模型；智能匹配时为 undefined。 */
  video?: ModelOption;
  image?: ModelOption;
};

export type AgentPriceView = {
  /** 发送键左边那颗：`✦2/轮` / `—`。 */
  label: string;
  /** 明细逐行。 */
  lines: string[];
  note: string;
  /** 推理模型不可用 / 没选时的原因（发送键据此禁用）。 */
  error?: string;
};

/**
 * 价格明细的可选附加行（营销页，MKT-T-14、MKT-UX-08）。不传 = 与创造页逐字相同（MKT-AC-27）。
 * - `variants`：当前是批量 / 裂变技能。`max` 取 `/marketing/config` 的 `maxVariants`（配置值，不写死 4）；
 *   `count` 是已知的变体数 —— 输入框里还没有规划，通常不知道，**不猜**一个数。
 */
export type AgentPriceExtra = {
  variants?: { max: number; count?: number };
};

/** 批量技能的提示行：变体逐个计费、确认卡逐个列出。 */
export function variantsPriceLine(variants: { max: number; count?: number }): string {
  const max = Math.max(1, Math.floor(variants.max));
  if (variants.count !== undefined && variants.count > 0) {
    return `共 ${Math.floor(variants.count)} 个变体（最多 ${max} 个），确认时逐个列出`;
  }
  return `按变体逐个计费（最多 ${max} 个），确认时逐个列出`;
}

/**
 * Agent 模式的价格（设计 02 §4.6）：对话按轮、所选视频 / 图片单价、确认前不扣生成费。
 * ⚠️ 营销页与创造页走同一个函数、同一个 `PricePill`（MK-10）；营销页多出来的只有 `extra` 那一行，没有原价 / 划线价。
 */
export function agentPriceView(input: AgentPriceInput, extra: AgentPriceExtra = {}): AgentPriceView {
  const lines: string[] = [];
  const reasoning = input.reasoning;
  let label = "—";
  let error: string | undefined;
  if (!reasoning) {
    error = "还没有可用的推理模型";
  } else if (!reasoning.available) {
    error = `${reasoning.name}：${reasoning.reason || "暂时不可用"}`;
  } else if (reasoning.price !== undefined) {
    label = unitPriceText(reasoning.price, reasoning.unit, true);
    lines.push(`对话按轮计费 ${label}（${reasoning.name}）`);
  } else {
    lines.push(`对话按轮计费（${reasoning.name}，报价中）`);
  }
  lines.push(mediaLine("视频", input.video));
  lines.push(mediaLine("图片", input.image));
  if (extra.variants) lines.push(variantsPriceLine(extra.variants));
  return { label, lines, note: AGENT_GENERATION_NOTE, ...(error ? { error } : {}) };
}

function mediaLine(kind: string, option: ModelOption | undefined): string {
  if (!option) return `${kind} 智能匹配，确认规划时告诉你用了哪个`;
  if (!option.available) return `${kind} ${option.name}（不可用：${option.reason || "暂时不可用"}）`;
  const price = unitPriceText(option.price, option.unit);
  return price ? `${kind} ${price}（${option.name}）` : `${kind} ${option.name}`;
}

/** 一键优化提示词的价格说明（AGT-43）：点之前就说清花多少，失败退回。 */
export type OptimizePriceView = {
  /** 按钮旁 / 确认浮层里的价格：`✦36`；还没报出来为空串。 */
  label: string;
  /** 确认浮层里的一句话。 */
  text: string;
  /** 推理模型不可用 / 没有时的原因（按钮据此禁用）。 */
  error?: string;
};

/**
 * 优化一次按所选推理模型的**每轮单价**收费（与 Agent 对话同一个报价，服务端 operation=agent_chat）。
 * ⚠️ 价格来自服务端报价，这里不乘不除；没有可用的推理模型时给原因而不是猜一个数。
 */
export function optimizePriceView(reasoning: ModelOption | undefined): OptimizePriceView {
  if (!reasoning) return { label: "", text: "", error: "还没有可用的推理模型" };
  if (!reasoning.available) return { label: "", text: "", error: `${reasoning.name}：${reasoning.reason || "暂时不可用"}` };
  if (reasoning.price === undefined) return { label: "", text: `优化一次按轮计费（${reasoning.name}，报价中）` };
  const label = `✦${reasoning.price}`;
  return {
    label,
    text: `优化一次 ${label}（按轮计费，${reasoning.name}）；失败或没有结果全额退回`
  };
}

/** 一键优化 / AI 帮写的改写用途（`POST /agent/prompt-optimize` 的 `purpose`，MKD-29）。 */
export type OptimizePurpose = "general" | "character_appearance";

/**
 * 一键优化的请求体。⚠️ 不传 `purpose` 时与改动前**逐字一致**（`{text, modelCode?}`）——
 * 创造页的一键优化不受「AI 帮写」影响；AI 帮写只多一个 `purpose`，价格、账本、退款同一条链路（MK-26）。
 */
export function optimizeRequestBody(text: string, modelCode?: string, purpose?: OptimizePurpose): { text: string; modelCode?: string; purpose?: OptimizePurpose } {
  return { text, ...(modelCode ? { modelCode } : {}), ...(purpose ? { purpose } : {}) };
}

/**
 * 模型目录给视频模型报价时用的时长（秒）。必须落在各模型允许的时长范围里（Seedance 4~30、Grok 3~30、可灵 3 起），
 * 报价入口在扣费前就校验它 —— 报 1 秒会让所有视频模型都显示「不可用」。
 */
export const QUOTE_VIDEO_SECONDS = 5;

/** 报价 → 列表上显示的单价：按秒计价的除回每秒（报价是「单价 × 时长」），其余原样。 */
export function quoteUnitPrice(credits: number | undefined, unit: string | undefined, quotedSeconds: number): number | undefined {
  if (typeof credits !== "number") return undefined;
  if (unit === "per_second" && quotedSeconds > 1) return Math.round(credits / quotedSeconds);
  return credits;
}

