import type { components } from "@tabtv/api-client";
import type { SmartcutEditResult } from "./smartcutContent";
import { smartcutPlayLen } from "./smartcutLayout";
import { smartcutSecondsText } from "./smartcutPlan";

/**
 * 片段级生成的界面规则（docs/agent/smartcut SCT-40，dev/03 §5.6）：片段重拍、补镜、去字幕、生成配乐、生成音效。纯函数：
 * 对话框、片段面板、时间线与测试共用。
 *
 * ⚠️ 价格只用服务端的报价（dryRun）：前端按单价自己乘出来的与服务端按区间向上取整到秒算的不是一回事。
 * ⚠️ 不可用的工具照样显示并说明原因，原因给**最根本的那一条**（只读排在「这一段不合适」之前）。
 * ⚠️ 回填结果以服务端为准：受理之后等 `timeline.updated` 重取时间线，前端不自己替换片段（SCR-13）。
 */

type ModelChoice = components["schemas"]["SmartcutModelChoice"];
type Quote = components["schemas"]["SmartcutQuote"];
type Generation = components["schemas"]["SmartcutGeneration"];
type Origin = components["schemas"]["SmartcutClipOrigin"];
type Timeline = components["schemas"]["Timeline"];
type TimelineClip = components["schemas"]["TimelineClip"];
type TimelineTrack = components["schemas"]["TimelineTrack"];

export type SmartcutClipTool = "regenerate" | "fill" | "desubtitle" | "music" | "sfx";

/**
 * 打开一个片段级工具的确认框（片段面板、工具条、生成记录的「重试」都发它，剪辑台外壳负责打开）。
 * `prompt` / `modelName` 是重拍的提示词与原模型名；`afterClipId` 是补镜的位置（空串 = 最前面）。
 */
export type SmartcutClipToolOpen = { tool: SmartcutClipTool; clipId?: string; afterClipId?: string; prompt?: string; modelName?: string };

/** 生成记录里的产物拖进时间线时 dataTransfer 里的类型（生成记录与时间线共用）。 */
export const SMARTCUT_GENERATION_DRAG = "application/x-smartcut-generation";

export const smartcutClipToolLabels: Record<SmartcutClipTool, string> = {
  regenerate: "重新生成", fill: "补镜", desubtitle: "去字幕", music: "生成配乐", sfx: "生成音效"
};

/** 生成记录里的种类（契约 SmartcutGeneration.kind）。 */
export const smartcutGenerationKindLabels: Record<Generation["kind"], string> = {
  asr: "语音识别", regen: "片段重拍", fill: "补镜", desubtitle: "去字幕", music: "配乐", sfx: "音效"
};

/** 配乐的时长范围（契约 `/music` 的 durationSec）与空时间线的缺省时长（与服务端一致）。 */
export const smartcutMusicLimits = { minSec: 3, maxSec: 300, defaultSec: 30 } as const;

/** 判断一个片段级工具能不能用的上下文。 */
export type SmartcutClipToolContext = {
  editable: boolean;
  disabledReason: string;
  /** 片段所在的轨道与素材种类；不针对某一段的工具（补镜、配乐）不给。 */
  trackKind?: TimelineTrack["kind"];
  mediaKind?: "video" | "image" | "audio";
  /** 这一段正在被片段级生成替换。 */
  pending?: boolean;
  /** 「生成信息」（重拍要看它能不能重拍）；还没取回来时为 undefined。 */
  origin?: Pick<Origin, "kind" | "canRegenerate" | "reason"> | null;
  /** 片段取用的长度（素材时间，毫秒）。 */
  clipMs?: number;
  /** 所选模型（音效要看它的单次范围）；`models` 是这个工具可选的模型清单。 */
  model?: Pick<ModelChoice, "name" | "minSec" | "maxSec"> | null;
  models?: readonly unknown[];
};

/**
 * 工具能不能用：空串 = 能用，否则是给用户看的原因。顺序：只读 → 这一段正在生成 → 工具自己的条件 → 没有可用的模型。
 */
export function smartcutClipToolReason(tool: SmartcutClipTool, ctx: SmartcutClipToolContext): string {
  if (!ctx.editable) return ctx.disabledReason || "现在不能编辑";
  if (ctx.pending && (tool === "regenerate" || tool === "desubtitle")) return "这一段正在生成，等它完成再试";
  switch (tool) {
    case "regenerate":
      if (ctx.trackKind !== "video") return "只有画面轨上的片段可以重新生成";
      if (ctx.origin === undefined) return "正在读取这一段的生成信息…";
      if (!ctx.origin || ctx.origin.kind !== "generation") {
        return ctx.origin?.reason || "这是上传的素材，没有生成信息：可以去字幕或补镜";
      }
      if (!ctx.origin.canRegenerate) return ctx.origin.reason || "这一段不能重新生成";
      return "";
    case "desubtitle":
      if (ctx.trackKind !== "video") return "只有画面轨上的视频片段可以去字幕";
      if (ctx.mediaKind !== "video") return "图片没有字幕可去：只有视频片段可以去字幕";
      break;
    case "sfx": {
      if (ctx.trackKind !== "video") return "按画面生成音效要选画面轨上的视频片段";
      if (ctx.mediaKind !== "video") return "图片没有画面变化：只有视频片段可以生成音效";
      const range = smartcutSfxRangeReason(ctx.clipMs ?? 0, ctx.model);
      if (range) return range;
      break;
    }
  }
  if (ctx.models && !ctx.models.length) return `没有可用的${tool === "fill" ? "视频" : ""}模型，请联系管理员配置`;
  return "";
}

/** 音效的片段长度（毫秒）在不在所选模型的单次范围里（按毫秒判，与服务端一致）；在范围里为空串。 */
export function smartcutSfxRangeReason(clipMs: number, model: Pick<ModelChoice, "name" | "minSec" | "maxSec"> | null | undefined): string {
  if (!model) return "";
  const lo = model.minSec ?? 0;
  const hi = model.maxSec ?? 0;
  if ((lo > 0 && clipMs < lo * 1000) || (hi > 0 && clipMs > hi * 1000)) {
    return `「${model.name}」只收 ${lo}~${hi} 秒的片段，这一段取用了 ${(clipMs / 1000).toFixed(1)} 秒：先把它裁到这个范围里`;
  }
  return "";
}

/** 片段取用的长度（素材时间，毫秒）：去字幕与音效按它计价（按区间向上取整到秒是服务端的事）。 */
export function smartcutClipSourceMs(clip: Pick<TimelineClip, "sourceInMs" | "sourceOutMs">): number {
  return Math.max(0, clip.sourceOutMs - clip.sourceInMs);
}

/**
 * 配乐的时长（与服务端 musicWantMs 同一条规则，dev/01 R-20 #8）：想要的长度缺省取「从播放头到成片结尾」（播放头在 0 时
 * 就是成片时长；播放头已在结尾或之后时取成片时长；空时间线按 30 秒），夹到 3~300 秒，再按所选模型的单次范围截。
 * - `sec` 生成与计费的秒数（向上取整）；`landMs` 放进时间线时的长度（模型最短比想要的长时按最短生成、计费，
 *   落下去按毫秒裁到想要的长度 —— 正好收在成片结尾）；
 * - 只是给用户看的说明 —— 计费秒数以服务端报价里的 `durationSec` 为准。
 */
export function smartcutMusicDuration(
  effectiveMs: number,
  atMs: number,
  wantSec: number | null | undefined,
  model: Pick<ModelChoice, "name" | "minSec" | "maxSec"> | null | undefined
): { sec: number; wantMs: number; landMs: number; note: string } {
  const { minSec, maxSec, defaultSec } = smartcutMusicLimits;
  let wantMs = wantSec && wantSec > 0 ? Math.round(wantSec) * 1000 : effectiveMs - atMs;
  if (!(wantMs > 0)) wantMs = effectiveMs;
  if (!(wantMs > 0)) wantMs = defaultSec * 1000;
  wantMs = Math.min(Math.max(wantMs, minSec * 1000), maxSec * 1000);
  const want = Math.ceil(wantMs / 1000);
  let sec = want;
  const lo = model?.minSec ?? 0;
  const hi = model?.maxSec ?? 0;
  if (lo > 0) sec = Math.max(sec, lo);
  if (hi > 0) sec = Math.min(sec, hi);
  const landMs = Math.min(wantMs, sec * 1000);
  let note = "";
  if (sec > want) note = `「${model?.name ?? "这个模型"}」单次最短 ${lo} 秒：按 ${sec} 秒生成并计费，放进时间线时裁到 ${smartcutSecondsText(wantMs)} 秒`;
  else if (sec < want) note = `「${model?.name ?? "这个模型"}」单次最长 ${hi} 秒：按 ${sec} 秒生成`;
  return { sec, wantMs, landMs, note };
}

/** 确认框里的花费说明。`blocked` 非空时确认按钮置灰（原因原样给用户）。 */
export function smartcutClipQuoteText(quote: Quote | null | undefined, modelName: string): {
  cost: string;
  detail: string;
  provider: string;
  refund: string;
  blocked: string;
} {
  const provider = `会发给「${modelName || "所选模型"}」处理`;
  const refund = "生成失败或取消会按原批次全额退款";
  if (!quote) return { cost: "", detail: "", provider, refund, blocked: "" };
  const blocked = (quote.blockers ?? []).map(b => b.message).filter(Boolean).join("；");
  if (blocked || quote.items === 0) {
    return { cost: "", detail: "", provider, refund, blocked: blocked || "现在不能生成" };
  }
  let detail = "";
  if (quote.unit === "per_second" && typeof quote.unitPrice === "number") {
    detail = `按 ${quote.durationSec ?? "?"} 秒计费（✦${quote.unitPrice}/秒）`;
  } else if (typeof quote.unitPrice === "number" && quote.unit) {
    detail = quote.unit === "per_time" || quote.unit === "per_image" ? `按次计费（✦${quote.unitPrice}/次）` : `单价 ✦${quote.unitPrice}`;
  }
  return { cost: `✦${quote.credits}`, detail, provider, refund, blocked: "" };
}

const TERMINAL = new Set<Generation["status"]>(["success", "failed", "cancelled"]);

/**
 * 实时事件 `generation.update` 按 `generationId` 认领：只更新已经在记录里的那几条（智能剪辑名下的任务不挂画布节点，
 * 画布的生成 store 认领不到，见 SCT-40 背景第 1 条）。已经到终态的不会被晚到的非终态事件改回去。
 * 返回更新后的那一条；不是自己的、或者没有变化时为 null。
 */
export function smartcutClaimGenerationFrame(
  known: Readonly<Record<string, Generation>>,
  frame: Record<string, unknown>
): Generation | null {
  if (frame?.type !== "generation.update") return null;
  const id = typeof frame.generationId === "string" ? frame.generationId : "";
  const current = id ? known[id] : undefined;
  if (!current) return null;
  const status = frame.status as Generation["status"];
  if (!["queued", "running", "success", "failed", "cancelled"].includes(status)) return null;
  if (TERMINAL.has(current.status) && !TERMINAL.has(status)) return null;
  const progress = typeof frame.progress === "number" ? frame.progress : current.progress;
  const errorOf = frame.error as { message?: unknown } | string | null | undefined;
  const error = typeof errorOf === "string" ? errorOf : typeof errorOf?.message === "string" ? errorOf.message : current.error;
  if (status === current.status && progress === current.progress && error === current.error) return null;
  const next: Generation = { ...current, status, progress: status === "success" ? 100 : progress };
  if (error) next.error = error;
  return next;
}

/** 每个片段最近一次片段级生成（按受理时间）：失败标记与重试入口按它判 —— 失败之后又成功了的不再标失败。 */
export function smartcutLatestByClip(generations: Readonly<Record<string, Generation>>): Map<string, Generation> {
  const out = new Map<string, Generation>();
  for (const g of Object.values(generations)) {
    if (!g.clipId || g.kind === "asr") continue;
    const prev = out.get(g.clipId);
    if (!prev || g.createdAt > prev.createdAt) out.set(g.clipId, g);
  }
  return out;
}

/** 最近一次生成失败了的片段（时间线上标「失败」）。只看替换这一段的工具（重拍、去字幕）：音效失败不让画面标失败。 */
export function smartcutFailedClipIds(generations: Readonly<Record<string, Generation>>): Set<string> {
  const ids = new Set<string>();
  for (const [clipId, g] of smartcutLatestByClip(generations)) {
    if (g.status === "failed" && (g.kind === "regen" || g.kind === "desubtitle")) ids.add(clipId);
  }
  return ids;
}

/** 生成记录里一条的状态说明。 */
export function smartcutGenerationStatusText(g: Pick<Generation, "status" | "progress" | "landed" | "outputMediaId" | "error" | "kind">): string {
  switch (g.status) {
    case "queued":
      return "排队中";
    case "running":
      return g.progress > 0 ? `生成中 ${g.progress}%` : "生成中";
    case "failed":
      return g.error ? `失败：${g.error}` : "失败（已退款）";
    case "cancelled":
      return "已取消（已退款）";
  }
  if (g.kind === "asr") return "已完成";
  if (g.landed) return "已放进时间线";
  return g.outputMediaId ? "已完成：原来的位置已经不在了，可以拖进时间线" : "已完成";
}

/** 生成记录里的产物要放进哪类轨道：视频进画面轨，配乐 / 音效进对应用途的音频轨；识别没有媒体产物。 */
export function smartcutGenerationOutputKind(kind: Generation["kind"]): { kind: "video" | "audio"; role?: "music" | "sfx" } | null {
  switch (kind) {
    case "regen":
    case "fill":
    case "desubtitle":
      return { kind: "video" };
    case "music":
      return { kind: "audio", role: "music" };
    case "sfx":
      return { kind: "audio", role: "sfx" };
  }
  return null;
}

const MAX_AUDIO_TRACKS = 6;
const AUDIO_TRACK_NAMES: Record<"music" | "sfx", string> = { music: "配乐", sfx: "音效" };

/**
 * 把生成记录里的一份产物放进时间线（SC-F-136、SC-F-139）：
 * - 视频进画面轨，插在 `atMs` 最近的接缝上（落点由重算给出，后面的段跟着移）；
 * - 音频进这个用途的音频轨，起点 `atMs`：先找一条那段时间空着的同用途轨，再认领没定用途的，都不行就新建（最多 6 条）。
 * 时长未知时不放（猜一个数的话成片长度对不上）。新片段来源记 user。
 */
export function smartcutPlaceGenerated(
  t: Timeline,
  media: { mediaId: string; kind: "video" | "image" | "audio"; durationMs: number; role?: "music" | "sfx" },
  atMs: number,
  ids: { clip: string; track: string }
): SmartcutEditResult<Timeline> {
  const length = media.kind === "image" ? 3000 : Math.round(media.durationMs);
  if (!(length > 0)) return { ok: false, reason: "这份产物的时长还不知道，稍后再试" };
  const at = Math.max(0, Math.round(atMs));
  const base: TimelineClip = {
    id: ids.clip, idx: 0, mediaId: media.mediaId, sourceInMs: 0, sourceOutMs: length, startMs: 0, durationMs: length,
    speed: 100, volume: 100, origin: "user"
  };
  if (media.kind !== "audio") {
    const vi = t.tracks.findIndex(track => track.kind === "video");
    if (vi < 0) return { ok: false, reason: "没有画面轨" };
    const clips = t.tracks[vi]!.clips ?? [];
    const index = clips.filter(c => c.startMs + smartcutPlayLen(c) / 2 < at).length;
    const next = [...clips.slice(0, index), base, ...clips.slice(index)];
    return { ok: true, value: { ...t, tracks: t.tracks.map((tr, i) => (i === vi ? { ...tr, clips: next } : tr)) } };
  }
  const role = media.role ?? "music";
  const clip: TimelineClip = { ...base, startMs: at };
  const free = (track: TimelineTrack) => !(track.clips ?? []).some(c => !c.anchor && c.startMs < at + length && at < c.startMs + smartcutPlayLen(c));
  let ti = t.tracks.findIndex(track => track.kind === "audio" && track.role === role && free(track));
  let tracks = t.tracks;
  if (ti < 0) {
    ti = t.tracks.findIndex(track => track.kind === "audio" && !track.role && free(track));
    if (ti >= 0) tracks = t.tracks.map((tr, i) => (i === ti ? { ...tr, role } : tr));
  }
  if (ti < 0) {
    if (t.tracks.filter(track => track.kind === "audio").length >= MAX_AUDIO_TRACKS) {
      return { ok: false, reason: `音频轨已满（最多 ${MAX_AUDIO_TRACKS} 条），那个位置也没有空着的${AUDIO_TRACK_NAMES[role]}轨` };
    }
    const lastAudio = t.tracks.map(tr => tr.kind).lastIndexOf("audio");
    const track: TimelineTrack = { id: ids.track, kind: "audio", idx: 0, isMuted: false, volume: 100, role, name: AUDIO_TRACK_NAMES[role], clips: [] };
    ti = lastAudio >= 0 ? lastAudio + 1 : 1;
    tracks = [...t.tracks.slice(0, ti), track, ...t.tracks.slice(ti)];
  }
  const clipsAfter = [...(tracks[ti]!.clips ?? []), clip];
  return { ok: true, value: { ...t, tracks: tracks.map((tr, i) => (i === ti ? { ...tr, clips: clipsAfter } : tr)) } };
}

/** 补镜插在哪：播放头所在的那一段之后（播放头在第一段之前或时间线为空时插在最前面）。返回那一段的 ID（空串 = 最前面）。 */
export function smartcutFillAfter(t: Timeline | null | undefined, playheadMs: number): string {
  const clips = t?.tracks.find(track => track.kind === "video")?.clips ?? [];
  let after = "";
  for (const c of clips) {
    if (c.startMs < playheadMs) after = c.id;
  }
  return after;
}

/** 片段级生成的幂等键：一次确认一个键，失败后的「重试」由用户重新确认、换一个新键（服务端按键只建一个任务）。 */
export function smartcutClipToolKey(tool: SmartcutClipTool): string {
  const rand = globalThis.crypto?.randomUUID?.() ?? `${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 12)}`;
  return `smartcut-${tool}-${rand}`;
}
