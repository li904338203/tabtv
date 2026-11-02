/**
 * 创造页 / Agent 输入框的发送键可用性与模型可用性（AGT-42，设计 02 §4.6）。纯函数。
 *
 * tabtv 没有计价（TT-T-14）：不显示任何积分、价格、余额。报价接口（`POST /generations/estimate`）仍然调用 ——
 * 它与受理走同一条能力校验路径，报价失败的服务端原话就是「这个模型现在不能用」的原因。
 */

export type QuoteState = {
  loading: boolean;
  /** 报价（能力校验）失败的服务端原话（模型不可用、参数不合法……）。 */
  error?: string;
};

export type SendGate = {
  disabled: boolean;
  /** 禁用时悬停/下方显示的原因；可用时为空串。 */
  reason: string;
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
};

/**
 * 发送键能不能点、不能点为什么。理由给**最根本的那一条**：只读排在最前。
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
  return { disabled: false, reason: "" };
}

// ---------------------------------------------------------------- Agent 模式（AGT-29）

/** 偏好浮层里一个模型的样子：不可用的**照样列出**，置灰并写原因。 */
export type ModelOption = {
  code: string;
  name: string;
  available: boolean;
  /** 不可用时的原因（服务端原话）；可用时为空。 */
  reason?: string;
  /** 角标（只来自目录里真有的字段，见 `utils/composerUi.modelBadges`）；没有为空。 */
  badges?: string[];
  /** 原生出声（`/models` 的 `hasNativeAudio`，只对视频模型有意义）；目录没给为 undefined。营销弹层据此置灰依赖出声的 Hook。 */
  hasNativeAudio?: boolean;
};

/** 报价失败的原因 → 模型不可用的说法（原话带出；没有原话时给一句不含糊的）。 */
export function modelUnavailableReason(cause: unknown): string {
  const message = typeof cause === "object" && cause !== null && "message" in cause
    ? String((cause as { message?: unknown }).message ?? "").trim()
    : "";
  return message && message !== "请求失败" ? message : "暂时不可用";
}

/**
 * 推理模型不能用的原因（Agent 发送、一键优化、AI 帮写据此禁用）；能用返回 undefined。
 */
export function reasoningUnavailable(reasoning: ModelOption | undefined): string | undefined {
  if (!reasoning) return "还没有可用的推理模型";
  if (!reasoning.available) return `${reasoning.name}：${reasoning.reason || "暂时不可用"}`;
  return undefined;
}

/** 一键优化 / AI 帮写的改写用途（`POST /agent/prompt-optimize` 的 `purpose`，MKD-29）。 */
export type OptimizePurpose = "general" | "character_appearance";

/**
 * 一键优化的请求体。⚠️ 不传 `purpose` 时与改动前**逐字一致**（`{text, modelCode?}`）。
 */
export function optimizeRequestBody(text: string, modelCode?: string, purpose?: OptimizePurpose): { text: string; modelCode?: string; purpose?: OptimizePurpose } {
  return { text, ...(modelCode ? { modelCode } : {}), ...(purpose ? { purpose } : {}) };
}

/**
 * 模型目录给视频模型做可用性报价时用的时长（秒）。必须落在各模型允许的时长范围里（Seedance 4~30、Grok 3~30、可灵 3 起），
 * 报价入口在受理前就校验它 —— 报 1 秒会让所有视频模型都显示「不可用」。
 */
export const QUOTE_VIDEO_SECONDS = 5;
