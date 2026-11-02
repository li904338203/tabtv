/**
 * 创造页「直接生成」的纯函数（AGT-42，设计 02 §4/§7，开发 03 §7）。
 *
 * 模式 → 节点类型 / 推荐操作码、请求体组装、上次模式的本地记忆 —— 都不碰 store，单测见同名 test。
 *
 * ⚠️ 操作码由服务端核对（`canvas/domain.OperationFor`）：视频把按张数推断出的操作显式发过去
 *    （服务端核对与张数对得上），图片/音频不带 —— 服务端按附件张数自己定。
 */
import type { JsonObject } from "~/types/canvas";

export type ComposerMode = "agent" | "video" | "image" | "audio";
export type QuickMode = Exclude<ComposerMode, "agent">;

export const QUICK_MODES: readonly QuickMode[] = ["video", "image", "audio"];

/** 模式下拉的文案。 */
export const MODE_LABELS: Record<ComposerMode, string> = {
  agent: "Agent 模式",
  video: "视频生成",
  image: "图片生成",
  audio: "音频生成"
};

/** 每个模式的占位文案（设计 02 §4.1：随模式变化）。 */
export const MODE_PLACEHOLDERS: Record<QuickMode, string> = {
  video: "描述你想生成的视频，比如：雨夜的霓虹街头，镜头缓慢推进，赛博朋克风",
  image: "描述你想生成的画面，比如：水墨风格的江南小镇，清晨薄雾",
  audio: "输入需要朗读的文本"
};

export function isQuickMode(value: unknown): value is QuickMode {
  return typeof value === "string" && (QUICK_MODES as readonly string[]).includes(value);
}

/** 模式对应的画布节点类型（三者同名；单独一个函数，是为了将来不再一一对应时只改这里）。 */
export function modeNodeType(mode: QuickMode): string {
  return mode;
}

/**
 * 创造页视频模式会用到的三种操作。⚠️ **首尾帧（`first_last_frame`）不在里面**：创造页不再给操作下拉，
 * 而「2 张图」既可能是首尾帧也可能是两张参考，靠张数猜不出用户的意图 —— 一律按全能参考处理，
 * 要首尾帧去画布节点里显式选。`video2video` 要视频附件，`＋` 只收图片。
 */
export const QUICK_VIDEO_OPERATIONS = ["text2video", "img2video", "ref2video"] as const;

/**
 * 视频模式按参考图张数推断操作（创造页没有操作下拉）：
 *   0 张 → text2video；1 张 → img2video；2 张及以上 → ref2video。
 *
 * ⚠️ 推断出的操作**原样**用于报价与受理，所选模型不支持时由报价报出原因（价格处 / 发送键），
 *    **不静默换成别的操作** —— 悄悄换的话用户以为在做图生视频，实际跑的是另一种。
 */
export function quickVideoOperation(imageCount: number): (typeof QUICK_VIDEO_OPERATIONS)[number] {
  const count = Number.isFinite(imageCount) ? Math.max(0, Math.floor(imageCount)) : 0;
  if (count === 0) return "text2video";
  if (count === 1) return "img2video";
  return "ref2video";
}

/**
 * 实际的操作码（报价与受理同用）：
 *   视频：按参考图张数推断（见 `quickVideoOperation`）
 *   图片：无图 text2img、有图 img2img
 *   音频：text2audio
 */
export function quickOperation(mode: QuickMode, imageCount: number): string {
  if (mode === "image") return imageCount > 0 ? "img2img" : "text2img";
  if (mode === "audio") return "text2audio";
  return quickVideoOperation(imageCount);
}

const OPERATION_LABELS: Record<string, string> = {
  text2video: "文生视频",
  img2video: "图生视频",
  ref2video: "全能参考",
  text2img: "文生图",
  img2img: "图生图",
  text2audio: "文本转语音"
};

/** 偏好浮层底部那行：现在按哪种操作生成、为什么（没有下拉了，得让用户看得见）。 */
export function quickOperationNote(mode: QuickMode, imageCount: number): string {
  const operation = quickOperation(mode, imageCount);
  const label = OPERATION_LABELS[operation] ?? operation;
  if (mode === "audio") return `当前：${label}`;
  const count = Math.max(0, Math.floor(imageCount) || 0);
  return `当前：${label}（${count ? `${count} 张参考图` : "无参考图"}，按参考图自动判定）`;
}

/** 一个已上传完成的附件（只有 ID，GO-27）。 */
export type QuickAttachment = {
  referenceId: string;
  name?: string;
};

export type QuickRequest = {
  mode: QuickMode;
  modelCode: string;
  operation?: string;
  text: string;
  params: JsonObject;
  inputs: Array<{ kind: "reference"; referenceId: string; order: number; name?: string }>;
};

/** 参数里不该由客户端发的键：附件由 inputs 表达、正文由 text 表达、模型由 modelCode 表达。 */
const STRIPPED_PARAM_KEYS = ["reference", "prompt", "text", "model", "mode", "_snapshot"];

/**
 * 组装 `POST /quick-generations` 的请求体。
 *
 * ⚠️ 附件**只带 referenceId**，不带任何 URL（GO-27）：服务端只认它自己查得到的资源。
 * ⚠️ 视频把按张数推断的操作放进 `operation`，服务端核对与附件张数是否对得上；
 *    图片/音频不带，服务端按附件定 —— 两边各算一遍会在边界上不一致。
 */
export function buildQuickRequest(input: {
  mode: QuickMode;
  modelCode: string;
  text: string;
  params: JsonObject;
  attachments: readonly QuickAttachment[];
}): QuickRequest {
  const params: JsonObject = {};
  for (const [key, value] of Object.entries(input.params)) {
    if (STRIPPED_PARAM_KEYS.includes(key) || value === undefined || value === null || value === "") continue;
    params[key] = value;
  }
  const request: QuickRequest = {
    mode: input.mode,
    modelCode: input.modelCode,
    text: input.text.trim(),
    params,
    inputs: input.attachments.map((item, index) => ({
      kind: "reference",
      referenceId: item.referenceId,
      order: index + 1,
      ...(item.name ? { name: item.name } : {})
    }))
  };
  // ⚠️ 视频必须显式带上推断结果：不带的话服务端按张数默认，2 张会落成首尾帧，与报价对不上。
  if (input.mode === "video") request.operation = quickVideoOperation(input.attachments.length);
  return request;
}

/** 报价请求的数量与时长 —— 与服务端 `QuickQuantities`、画布 `generationQuantities` 同一条规则。 */
export function quickQuantities(mode: QuickMode, params: JsonObject): { count: number; durationSec?: number } {
  const count = positiveInt(params.count) ?? 1;
  const durationSec = mode === "video" ? positiveInt(params.duration) : undefined;
  return durationSec ? { count, durationSec } : { count };
}

function positiveInt(value: unknown): number | undefined {
  const parsed = typeof value === "number" ? value : typeof value === "string" ? Number(value.trim()) : Number.NaN;
  if (!Number.isFinite(parsed) || parsed < 1) return undefined;
  return Math.floor(parsed);
}

/** 成功或 `accepted=false` 都跳画布：节点上写着结果，失败了用户在那里直接重试。 */
export function quickCanvasPath(projectId: string): string {
  return `/video/canvas/${projectId}`;
}

// ---------------------------------------------------------------- 上次模式（设计 02 §4.2：记住用户上次的选择）

export const LAST_MODE_KEY = "seeaitv:create-composer-mode";

type StorageLike = Pick<Storage, "getItem" | "setItem">;

export function isComposerMode(value: unknown): value is ComposerMode {
  return value === "agent" || isQuickMode(value);
}

/**
 * 读上次的模式（设计 02 §4.2：默认 Agent，记住用户上次的选择）。脏值 / 读不到时退回默认。
 */
export function readLastMode(storage: Pick<Storage, "getItem"> | undefined, fallback: ComposerMode = "agent"): ComposerMode {
  try {
    const value = storage?.getItem(LAST_MODE_KEY);
    return isComposerMode(value) ? value : fallback;
  } catch {
    return fallback;
  }
}

export function writeLastMode(storage: StorageLike | undefined, mode: ComposerMode) {
  try {
    storage?.setItem(LAST_MODE_KEY, mode);
  } catch {
    // 隐私模式下写不进去：只是下次不记得，不影响使用。
  }
}

// ---------------------------------------------------------------- Agent 模式发送（开发 03 §7）

/** 项目名上限（设计 02 §7：取正文前 20 字）。 */
export const AGENT_PROJECT_NAME_MAX = 20;

/**
 * 新项目的名字：正文前 20 个字（按码点截，不切坏 emoji），空白折成一个空格；
 * 只有附件没有正文时用「未命名创作」。
 */
export function agentProjectName(text: string): string {
  const flat = text.replace(/\s+/g, " ").trim();
  if (!flat) return "未命名创作";
  return Array.from(flat).slice(0, AGENT_PROJECT_NAME_MAX).join("").trim();
}

export type AgentSessionRef = { kind: "node" | "media" | "asset"; id: string };

/** `POST /agent/sessions`（顺带建项目）的请求体。 */
export type AgentCreateRequest = {
  createProject: { name: string };
  message: { text: string; refs: AgentSessionRef[] };
  skillCode?: string;
  prefs?: Record<string, unknown>;
};

/**
 * 组装创造页 Agent 模式的建会话请求：建项目 + 会话 + 首条消息一次完成（服务端按同一个幂等键防重）。
 *
 * ⚠️ 引用只带 `{kind,id}`（GO-27）；技能没选就不带（由 Agent 自行判断）；
 * ⚠️ `prefs` 必须带上推理模型 —— 由调用方解析好再传进来（见 `utils/agentPrefs.resolveReasoningModel`）。
 */
export function buildAgentSessionRequest(input: {
  text: string;
  refs: readonly AgentSessionRef[];
  skillCode?: string;
  prefs?: Record<string, unknown>;
}): AgentCreateRequest {
  const text = input.text.trim();
  return {
    createProject: { name: agentProjectName(text) },
    message: { text, refs: input.refs.map(ref => ({ kind: ref.kind, id: ref.id })) },
    ...(input.skillCode ? { skillCode: input.skillCode } : {}),
    ...(input.prefs && Object.keys(input.prefs).length ? { prefs: input.prefs } : {})
  };
}

/** 建好会话后跳画布：`?session=` 让右侧 Agent 面板自动展开到这个会话（AGT-28）。 */
export function agentCanvasPath(projectId: string, sessionId: string): string {
  return `/video/canvas/${projectId}?session=${encodeURIComponent(sessionId)}`;
}

// ---------------------------------------------------------------- 失败文案

/**
 * 发送失败的文案。⚠️ 断网要说成断网（不说「发送失败」：用户会反复点，而该做的是看网络）；
 * 其余**原样带出服务端那句话**（模型为什么不可用都在里面）。
 */
export function quickErrorText(cause: unknown, online: boolean): string {
  if (!online) return "网络已断开，恢复后再发送";
  const message = typeof cause === "object" && cause !== null && "message" in cause
    ? String((cause as { message?: unknown }).message ?? "")
    : "";
  return message && message !== "请求失败" ? message : "发送没有成功，请稍后重试";
}
