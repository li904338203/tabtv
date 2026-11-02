/**
 * AI 剪辑的文案（docs/agent/smartcut SC-F-80~89、SC-UX-09、SCD-43）：结果摘要、42217 的出路选项、
 * 冲突与远端更新的说法。纯函数，界面原样显示。
 *
 * ⚠️ 冲突与远端更新按**字段**分流（`bySelf` / `editorKind` / `editorName`），不匹配 message 文案（CLAUDE.md §5.16）。
 *    来源比画布多两种：`ai`（AI 剪辑写入）与 `sync`（连线即入轨的素材同步）。机器写入带的是用户本人的 ID 与名字，
 *    所以**先认来源再认名字**：说成「林澈 修改了…」会让他以为是自己（或同名同事）改的。
 * ⚠️ 42217 的出路要能直接点（SC-UX-09），不能只给一句报错：素材不够 → 允许放慢 / 允许重复 / 去补镜；
 *    要卡点没配乐 → 去连音频节点；解说没配音 → 去连音频节点；缺识别结果 → 先识别（带报价）。
 */
import { ErrorCode } from "@tabtv/api-client";
import type { components } from "@tabtv/api-client";
import { errorCode } from "./asyncState";
import { smartcutRetryAfterText } from "./smartcutRender";
import { smartcutFormatTime } from "./smartcutTimeline";

type SmartcutVariant = components["schemas"]["SmartcutVariant"];
type SmartcutPlanResult = components["schemas"]["SmartcutPlanResult"];
type SmartcutPlanRequest = components["schemas"]["SmartcutPlanRequest"];
type SmartcutPlanMode = SmartcutPlanRequest["mode"];
type SmartcutSyncResult = components["schemas"]["SmartcutSyncResult"];
type SmartcutTimeline = components["schemas"]["Timeline"];
type SmartcutInputItem = components["schemas"]["SmartcutInput"];

/** 一次 AI 调用是哪一种：剪一版、追加指令、切换方案。 */
export type SmartcutPlanKind = "plan" | "edit" | "variant";

// ---------------------------------------------------------------- 请求

const PLAN_MODES: ReadonlySet<string> = new Set(["mix", "board", "talk", "narr"]);
const PLAN_ASPECTS: ReadonlySet<string> = new Set(["16:9", "9:16", "1:1", "4:3", "3:4", "21:9"]);

function intOf(value: unknown): number | undefined {
  const raw = value && typeof value === "object" && !Array.isArray(value) ? (value as { value?: unknown }).value : value;
  const n = typeof raw === "number" ? raw : typeof raw === "string" && raw.trim() !== "" ? Number(raw) : Number.NaN;
  return Number.isFinite(n) ? Math.round(n) : undefined;
}

/**
 * 节点参数 → AI 剪辑的请求（不含 `baseVersion`，由 store 在 flush 之后补上）。参数配置驱动（`smartcut_mode`、`prompt`、
 * `aspect_ratio`、`resolution`、`target_duration`、`plan_variants`），认不出的值回落默认，不把坏值发给服务端：
 * - 画幅「跟随素材」在节点上存的是 `custom`，请求里叫 `follow`；
 * - 目标时长「自动」= 0；自定义控件可能存成 `{value}`；上限 30 分钟（SC-PARAM-21）。
 */
export function smartcutPlanRequestOf(params: Record<string, unknown> | null | undefined): Omit<SmartcutPlanRequest, "baseVersion"> {
  const p = params ?? {};
  const mode = typeof p.smartcut_mode === "string" && PLAN_MODES.has(p.smartcut_mode) ? p.smartcut_mode as SmartcutPlanMode : "mix";
  const instruction = typeof p.prompt === "string" ? p.prompt.trim() : "";
  const spec: NonNullable<SmartcutPlanRequest["spec"]> = { targetSec: Math.min(1800, Math.max(0, intOf(p.target_duration) ?? 0)) };
  if (p.aspect_ratio === "custom" || p.aspect_ratio === "follow") spec.aspectRatio = "follow";
  else if (typeof p.aspect_ratio === "string" && PLAN_ASPECTS.has(p.aspect_ratio)) spec.aspectRatio = p.aspect_ratio as NonNullable<typeof spec.aspectRatio>;
  if (p.resolution === "720p" || p.resolution === "1080p") spec.resolution = p.resolution;
  const variants = Math.min(3, Math.max(1, intOf(p.plan_variants) ?? 1));
  return { mode, ...(instruction ? { instruction: instruction.slice(0, 2000) } : {}), spec, variants };
}

// ---------------------------------------------------------------- 阶段

export type SmartcutStageStep = { key: string; label: string; state: "done" | "current" | "todo" };

const PLAN_STEPS: Array<{ key: string; label: string; stages: string[] }> = [
  { key: "sync", label: "同步素材", stages: ["sync"] },
  { key: "understand", label: "理解素材", stages: ["understand"] },
  { key: "plan", label: "规划", stages: ["plan"] },
  { key: "compile", label: "装配", stages: ["compile", "apply"] }
];

/**
 * 阶段条（SC-F-80）：同步素材 → 理解素材 → 规划 → 装配。认不出的阶段按刚开始（第一步进行中）显示，
 * `done` 全部完成。阶段只来自服务端（`smartcut.updated` 或摘要的 `activeCall.stage`），前端不按耗时猜。
 */
export function smartcutPlanSteps(stage: string | null | undefined): SmartcutStageStep[] {
  const at = stage === "done" ? PLAN_STEPS.length : Math.max(0, PLAN_STEPS.findIndex(step => step.stages.includes(stage ?? "")));
  return PLAN_STEPS.map((step, i) => ({ key: step.key, label: step.label, state: i < at ? "done" : i === at ? "current" : "todo" }));
}

// ---------------------------------------------------------------- 结果摘要

/** 兜底方案说明（dev/01 §4.3「兜底」）：按模式说清这一版是怎么来的。 */
export function smartcutFallbackNote(mode?: SmartcutPlanMode): string {
  const how = mode === "talk"
    ? "删掉了超过 0.6 秒的停顿"
    : mode === "narr"
      ? "按句均分了画面"
      : "按素材编号顺序拼接，并按目标时长从尾部截掉";
  return `AI 的方案没能用上，这一版是${how}（兜底），可以改要求再试一次`;
}

/** 一套方案的一句话：用了哪几段、成片多长。 */
export function smartcutVariantSummary(variant: SmartcutVariant): string {
  if (variant.status === "failed") return `${smartcutVariantLabel(variant.index)} 没有生成：${variant.error?.trim() || "未知原因"}`;
  const clips = variant.usedClips.length
    ? `用了 ${variant.usedClips.length} 段素材（${variant.usedClips.join("、")}）`
    : "没有用到素材";
  return `${clips} · 成片 ${smartcutFormatTime(variant.totalMs)}`;
}

export type SmartcutPlanView = {
  title: string;
  /** 写进时间线的那一套的摘要；一套都没写进去时为空。 */
  summary: string;
  warnings: string[];
  /** 用了兜底方案时的说明；否则为空。 */
  fallbackNote: string;
  /** 追加指令改了哪几处（人话，服务端给）。 */
  changes: string[];
  /** 每段为什么这样用（模型给的理由，原样转述）。 */
  reasons: Array<{ clip: string; reason: string }>;
  /** 模型的取舍说明与编译附注；兜底时服务端的固定说明已由 `fallbackNote` 说过，不重复。 */
  notes: string[];
  /** 素材不够目标时长时的出路（按 `shortage` 字段，不匹配警告文案）。 */
  shortage: SmartcutShortageView | null;
  /** 多方案时的切换列表（只有一套时为空）。 */
  variants: Array<{ index: number; label: string; summary: string; applied: boolean; failed: boolean }>;
};

/** 兜底时服务端写在 notes 第一条的固定说明（dev/01 R-8）：界面另有按模式的说法，这一句不重复显示。 */
const SERVER_FALLBACK_NOTE = "AI 没能给出方案，已按规则剪好";

/** 方案的字母标签（SC-F-112：A / B / C）。 */
export function smartcutVariantLabel(index: number): string {
  return `方案 ${String.fromCharCode(65 + Math.max(0, Math.min(25, index)))}`;
}

/** 结果卡（`PlanResultCard`）要显示的内容。`kind = edit` 时标题说「追加修改」。 */
export function smartcutPlanView(result: SmartcutPlanResult, mode?: SmartcutPlanMode, kind: SmartcutPlanKind = "plan"): SmartcutPlanView {
  const applied = result.variants.find(variant => variant.index === result.appliedVariant);
  const usable = result.variants.some(variant => variant.status !== "failed");
  const what = kind === "edit" ? "追加修改" : "AI 剪辑";
  const title = !applied
    ? usable ? `${what}出了方案，还没写进时间线` : `${what}没有可用的方案`
    : applied.usedFallback ? `${what}完成（兜底方案）` : `${what}完成`;
  return {
    title,
    summary: applied ? smartcutVariantSummary(applied) : "",
    warnings: applied?.warnings ?? [],
    fallbackNote: applied?.usedFallback ? smartcutFallbackNote(mode) : "",
    changes: result.changes ?? [],
    reasons: (applied?.reasons ?? []).filter((r): r is { clip: string; reason: string } => Boolean(r?.clip && r?.reason?.trim())),
    notes: (applied?.notes ?? []).filter(note => note.trim() && note.trim() !== SERVER_FALLBACK_NOTE),
    shortage: smartcutShortage(applied, mode),
    variants: result.variants.length > 1
      ? result.variants.map(variant => ({
        index: variant.index,
        label: smartcutVariantLabel(variant.index),
        summary: smartcutVariantSummary(variant),
        applied: variant.index === result.appliedVariant,
        failed: variant.status === "failed"
      }))
      : []
  };
}

/** 毫秒 → 「14.3」「20」（四舍五入到 0.1 秒，整秒不写 .0；与服务端警告里的写法一致）。 */
export function smartcutSecondsText(ms: number): string {
  const tenths = Math.round(Math.max(0, ms) / 100);
  return tenths % 10 === 0 ? String(tenths / 10) : (tenths / 10).toFixed(1);
}

export type SmartcutShortageView = { text: string; options: Array<{ action: SmartcutUnsuitableAction; label: string }> };

/**
 * 素材不够（SC-F-92）：「素材共 X 秒，不足目标 Y 秒」与三个出路。**只看 `shortage` 字段**：
 * 警告文案改一个字匹配就失效，而失效的表现是出路按钮悄悄消失。解说成片的目标是配音时长。
 */
export function smartcutShortage(variant: SmartcutVariant | null | undefined, mode?: SmartcutPlanMode): SmartcutShortageView | null {
  const s = variant?.shortage;
  if (!s || !(s.targetMs > 0)) return null;
  const text = mode === "narr"
    ? `画面共 ${smartcutSecondsText(s.totalMs)} 秒，不足配音 ${smartcutSecondsText(s.targetMs)} 秒`
    : `素材共 ${smartcutSecondsText(s.totalMs)} 秒，不足目标 ${smartcutSecondsText(s.targetMs)} 秒`;
  return {
    text,
    options: [
      { action: "allowSlow", label: "允许放慢（最低 0.75×）" },
      { action: "allowRepeat", label: "允许重复使用片段" },
      { action: "fill", label: "去补镜" }
    ]
  };
}

// ---------------------------------------------------------------- 42217 的出路（SC-UX-09）

/**
 * 42217 `details.reason`（契约 `api/errors.yaml`）：没有画面素材、没有装配出镜头、要卡点没配乐、解说没配音、缺识别结果。
 * `not_enough` 是早先的写法（素材不够现在由结果里的 `shortage` 说），仍然认得。
 */
export type SmartcutUnsuitableReason = "no_picture" | "no_shots" | "no_music" | "no_voice" | "asr_missing" | "not_enough";

export type SmartcutUnsuitableAction = "allowSlow" | "allowRepeat" | "fill" | "connectAudio" | "connectPicture" | "noBeat" | "transcribe";

export type SmartcutUnsuitable = {
  reason: string;
  text: string;
  options: Array<{ action: SmartcutUnsuitableAction; label: string }>;
  /** 缺识别结果时「先识别语音」打开的识别框的缺省值：服务端报价用的模型、要不要保留口癖（口播要）、报价（SCT-36）。 */
  transcribe?: { modelCode?: string; keepFillers?: boolean };
};

/** 服务端给的出路码（`details.options`）→ 界面的动作。认不出的码丢掉（不给一个点了不知道做什么的按钮）。 */
const OPTION_ACTIONS: Record<string, SmartcutUnsuitableAction> = {
  connect_audio: "connectAudio",
  connect_picture: "connectPicture",
  ignore_beat: "noBeat",
  transcribe: "transcribe",
  allow_slow: "allowSlow",
  allow_repeat: "allowRepeat",
  fill_shot: "fill"
};

function missingText(missing: unknown): string {
  if (Array.isArray(missing)) {
    const items = missing.filter((item): item is string => typeof item === "string" && item.trim() !== "");
    return items.length ? `（缺：${items.join("、")}）` : "";
  }
  return typeof missing === "string" && missing.trim() ? `（缺：${missing.trim()}）` : "";
}

function optionLabel(action: SmartcutUnsuitableAction, reason: string): string {
  switch (action) {
    case "allowSlow": return "允许放慢（最低 0.75×）";
    case "allowRepeat": return "允许重复使用片段";
    case "fill": return "去补镜";
    case "connectAudio": return reason === "no_voice" ? "去连音频节点（配音）" : "去连音频节点（配乐）";
    case "connectPicture": return "去连视频或图片节点";
    case "noBeat": return "不卡点，直接剪";
    case "transcribe": return "先识别语音";
  }
}

/** 每种原因的说明与服务端没给出路时的默认出路。 */
const REASONS: Record<string, { text: string; options: SmartcutUnsuitableAction[] }> = {
  not_enough: { text: "素材不够目标时长", options: ["allowSlow", "allowRepeat", "fill"] },
  no_music: { text: "要卡点，但没有配乐", options: ["connectAudio", "noBeat"] },
  no_voice: { text: "解说成片需要配音", options: ["connectAudio"] },
  asr_missing: { text: "需要先识别语音", options: ["transcribe"] },
  no_picture: { text: "没有可用的画面素材", options: ["connectPicture"] },
  no_shots: { text: "没有装配出可用的镜头", options: ["fill"] }
};

/**
 * 42217 详情 → 说明与可点的出路（SC-UX-09）。出路以服务端的 `details.options` 为准（码表见 errors.yaml），
 * 没给时按原因给默认的；认不出的原因、又没有出路的，只给说明、不给按钮（给错按钮比不给更糟）。
 */
export function smartcutUnsuitable(details: unknown, message = ""): SmartcutUnsuitable {
  const raw = details && typeof details === "object" && !Array.isArray(details) ? details as Record<string, unknown> : {};
  const reason = typeof raw.reason === "string" ? raw.reason : "";
  const missing = missingText(raw.missing);
  const quote = raw.quote && typeof raw.quote === "object" ? raw.quote as { modelCode?: unknown; keepFillers?: unknown } : undefined;
  const known = REASONS[reason];
  const fromServer = Array.isArray(raw.options)
    ? raw.options.map(code => (typeof code === "string" ? OPTION_ACTIONS[code] : undefined)).filter((a): a is SmartcutUnsuitableAction => Boolean(a))
    : [];
  const actions = [...new Set(fromServer.length ? fromServer : known?.options ?? [])];
  // 缺识别结果时服务端的说明逐份说清了原因（「编号 1：识别时去掉了口癖」），比只列编号有用 —— 用它。
  const text = reason === "asr_missing" && message.trim() ? message.trim() : `${known?.text ?? "当前素材不满足这个剪辑模式"}${missing}`;
  return {
    reason,
    text,
    options: actions.map(action => ({ action, label: optionLabel(action, reason) })),
    ...(reason === "asr_missing"
      ? {
          transcribe: {
            ...(typeof quote?.modelCode === "string" && quote.modelCode ? { modelCode: quote.modelCode } : {}),
            ...(typeof quote?.keepFillers === "boolean" ? { keepFillers: quote.keepFillers } : {})
          }
        }
      : {})
  };
}

// ---------------------------------------------------------------- 冲突与远端更新

/** 写入来源：真人整份保存、AI 剪辑、Agent、外部 AI 客户端、素材同步、系统（片段回填、快照恢复……）。 */
export type SmartcutEditorKind = "user" | "ai" | "agent" | "mcp" | "sync" | "system";

const EDITOR_KINDS: ReadonlySet<string> = new Set(["user", "ai", "agent", "mcp", "sync", "system"]);

/** 冲突详情（`VersionConflictError.details`）；缺字段给空值，不编造。 */
export type SmartcutConflict = {
  currentVersion?: number;
  currentUpdatedAt: string;
  currentEditorName: string;
  bySelf: boolean;
  editorKind?: SmartcutEditorKind;
};

/** 从 `useApi` 抛出的错误里取冲突详情。⚠️ `bySelf` 只认真正的 true：误判成"是我"的话用户会放心地覆盖掉同事的工作。 */
export function smartcutConflictDetails(cause: unknown): SmartcutConflict {
  const details = (cause as { data?: { details?: unknown } } | undefined)?.data?.details;
  const raw = details && typeof details === "object" && !Array.isArray(details) ? details as Record<string, unknown> : {};
  const kind = typeof raw.editorKind === "string" && EDITOR_KINDS.has(raw.editorKind) ? raw.editorKind as SmartcutEditorKind : undefined;
  return {
    ...(typeof raw.currentVersion === "number" ? { currentVersion: raw.currentVersion } : {}),
    currentUpdatedAt: typeof raw.currentUpdatedAt === "string" ? raw.currentUpdatedAt : "",
    currentEditorName: typeof raw.currentEditorName === "string" ? raw.currentEditorName.trim() : "",
    bySelf: raw.bySelf === true,
    ...(kind ? { editorKind: kind } : {})
  };
}

/** 机器来源的叫法；真人与认不出的返回空串（交给名字）。 */
function machineName(kind: string | undefined): string {
  switch (kind) {
    case "agent": return "Agent";
    case "mcp": return "外部 AI 客户端";
    case "ai": return "AI 剪辑";
    case "sync": return "素材同步";
    case "system": return "系统";
    default: return "";
  }
}

/** 拼接时在汉字与字母数字之间补一个空格（「被 Agent 更新」「覆盖 AI 剪辑的修改」），与仓库里其余文案的排版一致。 */
function spaced(...parts: string[]): string {
  return parts.reduce((acc, part) => {
    if (!acc || !part) return acc + part;
    const a = acc[acc.length - 1]!;
    const b = part[0]!;
    const latin = /[A-Za-z0-9]/;
    const han = /[\u4e00-\u9fff]/;
    return (latin.test(a) && han.test(b)) || (han.test(a) && latin.test(b)) ? `${acc} ${part}` : acc + part;
  }, "");
}

/** 冲突条的说明：是谁改的（按来源分流）。 */
export function smartcutConflictText(details: Partial<SmartcutConflict> | null | undefined): string {
  if (details?.bySelf) return "你在另一个窗口修改过这条时间线";
  const machine = machineName(details?.editorKind);
  if (machine) return spaced(machine, "刚修改了这条时间线");
  const name = details?.currentEditorName?.trim();
  return name ? `${name} 修改了这条时间线` : "时间线已被修改";
}

/** 「以我为准」按钮：写明覆盖的是谁。 */
export function smartcutOverwriteLabel(details: Partial<SmartcutConflict> | null | undefined): string {
  if (details?.bySelf) return "以我为准（覆盖我在另一个窗口的修改）";
  const machine = machineName(details?.editorKind);
  if (machine) return spaced("以我为准（覆盖", machine, "的修改）");
  const name = details?.currentEditorName?.trim();
  return name ? `以我为准（覆盖 ${name} 的修改）` : "以我为准（覆盖服务器上的版本）";
}

/** 「采纳对方版本」按钮。 */
export function smartcutAdoptLabel(details: Partial<SmartcutConflict> | null | undefined): string {
  return details?.bySelf ? "采纳另一个窗口的版本" : "采纳对方版本";
}

/** AI 剪辑期间时间线被改过（40902）：说清谁改的，并给「仍然应用」（`force: true`，可撤销）。 */
export function smartcutPlanConflictText(details: Partial<SmartcutConflict> | null | undefined): string {
  return `剪辑期间时间线有改动：${smartcutConflictText(details)}`;
}

export const smartcutForceApplyLabel = "仍然应用（覆盖这些改动，可撤销）";

/** 收到 `timeline.updated` 且自动采纳之后的提示（来源同上；`bySelf` 由 store 按编辑者与本人比出来）。 */
export type SmartcutRemoteNotice = { editorKind?: string; editorName?: string; bySelf?: boolean; reason?: string };

const SYSTEM_REASONS: Record<string, string> = {
  subtitles: "识别出的字幕已写进时间线",
  clip_fill: "片段生成的结果已填进时间线",
  restore: "时间线已恢复到历史版本",
  assemble: "时间线已按分镜装配好"
};

export function smartcutRemoteNoticeText(notice: SmartcutRemoteNotice | null | undefined): string {
  if (!notice) return "";
  if (notice.bySelf) return "你在另一个窗口更新了时间线";
  if (notice.editorKind === "system") return SYSTEM_REASONS[notice.reason ?? ""] ?? "时间线已在别处更新";
  const machine = machineName(notice.editorKind);
  if (machine) return spaced("时间线被", machine, "更新");
  const name = notice.editorName?.trim();
  return name ? `时间线被 ${name} 更新` : "时间线已在别处更新";
}

// ---------------------------------------------------------------- 请求失败

export type SmartcutPlanFailure = {
  text: string;
  /** 40902：时间线刚被改过 —— 调用方转进「仍然应用」流程，而不是只弹一句话。 */
  conflict: boolean;
  /** 40902 时这一次的调用记录：方案已经生成并存下，「仍然应用」用它（`/plan/{callId}/apply` + force）。 */
  callId?: string;
  /** 42901 / 42902 时多少秒后可以再试。 */
  retryAfterSeconds?: number;
  /** 被限流的是哪一条：每小时上限（42901）还是同时进行（42902）。 */
  limit?: "hourly" | "concurrency";
  /** 42217 的出路。 */
  unsuitable?: SmartcutUnsuitable;
};

function detailsOf(cause: unknown): Record<string, unknown> {
  const details = (cause as { data?: { details?: unknown } } | undefined)?.data?.details;
  return details && typeof details === "object" && !Array.isArray(details) ? details as Record<string, unknown> : {};
}

function serverMessage(cause: unknown): string {
  const message = (cause as { message?: unknown } | undefined)?.message;
  return typeof message === "string" ? message.trim() : "";
}

/** 被限流时的说法（SC-AC-35）：说清是哪一条限制，有等待时间时说多久后可以再试（界面每秒刷新，倒数）。 */
export function smartcutLimitText(limit: "hourly" | "concurrency", seconds: number): string {
  if (limit === "concurrency") return seconds > 0 ? `上一次 AI 剪辑还没结束，${smartcutRetryAfterText(seconds)}` : "上一次 AI 剪辑还没结束";
  return `本小时的 AI 剪辑次数用完了，${seconds > 0 ? smartcutRetryAfterText(seconds) : "请稍后再试"}`;
}

/** AI 剪辑 / 追加指令 / 方案切换失败时给用户看的话：按业务码分流。认不出的码与断网给兜底，不把英文原文给用户看。 */
export function smartcutPlanErrorText(cause: unknown): SmartcutPlanFailure {
  const retry = Number(detailsOf(cause).retry_after_seconds);
  const retryAfterSeconds = Number.isFinite(retry) && retry > 0 ? Math.ceil(retry) : undefined;
  switch (errorCode(cause)) {
    case ErrorCode.VERSION_CONFLICT: {
      const callId = detailsOf(cause).callId;
      return {
        text: smartcutPlanConflictText(smartcutConflictDetails(cause)),
        conflict: true,
        ...(typeof callId === "string" && /^[0-9]+$/.test(callId) ? { callId } : {})
      };
    }
    case ErrorCode.SMARTCUT_INPUT_UNSUITABLE: {
      const unsuitable = smartcutUnsuitable(detailsOf(cause), serverMessage(cause));
      return { text: unsuitable.text, conflict: false, unsuitable };
    }
    case ErrorCode.SMARTCUT_PLAN_UNAVAILABLE: {
      const reason = typeof detailsOf(cause).reason === "string" ? String(detailsOf(cause).reason).trim() : "";
      return { text: reason ? `AI 剪辑暂不可用：${reason}` : "AI 剪辑暂不可用", conflict: false };
    }
    case ErrorCode.RATE_LIMITED:
      return { text: smartcutLimitText("hourly", retryAfterSeconds ?? 0), conflict: false, limit: "hourly", ...(retryAfterSeconds ? { retryAfterSeconds } : {}) };
    case ErrorCode.CONCURRENCY_LIMITED:
      return { text: smartcutLimitText("concurrency", retryAfterSeconds ?? 0), conflict: false, limit: "concurrency", ...(retryAfterSeconds ? { retryAfterSeconds } : {}) };
    case ErrorCode.PROVIDER_ERROR:
    case ErrorCode.PROVIDER_REJECTED:
      return { text: "模型暂时不可用，请稍后再试", conflict: false };
    case ErrorCode.PROVIDER_TIMEOUT:
      return { text: "模型响应超时，请稍后再试", conflict: false };
    case ErrorCode.INVALID_ARGUMENT:
      return { text: serverMessage(cause) || "参数有误", conflict: false };
    default:
      return { text: "AI 剪辑失败，请检查网络后重试", conflict: false };
  }
}

// ---------------------------------------------------------------- 素材变化与手动改动（SC-F-84、SC-F-150~152）

function videoClips(t: SmartcutTimeline | null | undefined) {
  return t?.tracks.find(track => track.kind === "video")?.clips ?? [];
}

/**
 * 「素材有变化：第 N 段」：画面轨里来源节点的产物变了的片段（素材条的 `changed` 由服务端判，前端只按来源节点对上）。
 * 返回段号（从 1 起）。
 */
export function smartcutChangedClips(t: SmartcutTimeline | null | undefined, inputs: readonly SmartcutInputItem[]): number[] {
  const changed = new Set(inputs.filter(item => item.changed).map(item => item.sourceNodeId));
  if (!changed.size) return [];
  return videoClips(t).flatMap((clip, i) => (clip.sourceNodeId && changed.has(clip.sourceNodeId) ? [i + 1] : []));
}

/** 素材变化条的一句话（段号多时只列前 6 个）。 */
export function smartcutChangedText(positions: readonly number[]): string {
  if (!positions.length) return "素材有变化";
  const head = positions.slice(0, 6).join("、");
  return positions.length > 6 ? `素材有变化：第 ${head} 等 ${positions.length} 段` : `素材有变化：第 ${head} 段`;
}

/** 用户手动改过的片段数（`origin = user`）：重新剪会替换它们，发起前先提示（SC-F-84）。 */
export function smartcutUserEditedCount(t: SmartcutTimeline | null | undefined): number {
  return videoClips(t).filter(clip => clip.origin === "user").length;
}

export function smartcutReplanConfirmText(count: number): string {
  return count > 0 ? `你手动改过 ${count} 处，重新剪会替换它们（可撤销）。` : "";
}

/**
 * 「只替换变了的那几段」的结果（SCT-30）：换了几段；新素材比原来的取用区间短、被截到素材长度的，逐段说出来。
 * 段号按替换之后的时间线算（替换不改片段 ID）。
 */
export function smartcutReplaceResult(res: SmartcutSyncResult, t: SmartcutTimeline | null | undefined): { text: string; details: string[] } {
  const clips = videoClips(t);
  const position = (id: string) => clips.findIndex(clip => clip.id === id) + 1;
  const replaced = res.replacedClipIds?.length ?? 0;
  const details = (res.shortened ?? []).map((item) => {
    const n = position(item.clipId);
    const which = n > 0 ? `第 ${n} 段` : "有一段";
    return `${which}的新素材比原来短，从 ${smartcutSecondsText(item.fromMs)} 秒截到了 ${smartcutSecondsText(item.toMs)} 秒`;
  });
  return { text: replaced ? `已把 ${replaced} 段换成新素材（可撤销）` : "没有需要替换的片段", details };
}

