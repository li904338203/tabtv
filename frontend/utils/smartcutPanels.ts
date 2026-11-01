/**
 * 剪辑台右侧面板的编辑计算（docs/agent/smartcut SCT-22）：片段设置、转场、备选切换、文字层、字幕条目与字幕轨。
 * 面板在「应用」时调一次这里的函数，再 `store.commit`（一次应用 = 一步撤销）。
 *
 * 规则与保存前校验（`smartcutValidateContent`，与 Go 同一份）一致，但这里**先拦**并说清上限是多少：
 * 「淡入 + 淡出不能超过片段长度 2.4 秒」比保存时一句「tracks[0].clips[3].fadeOutMs 不合法」有用得多。
 *
 * ⚠️ 不碰 DOM；导出带 `smartcut` 前缀（Nuxt 自动导入 `utils/` 的全部导出，重名时悄悄顶替）。
 */
import type { components } from "@tabtv/api-client";
import { smartcutContentLimits, type SmartcutEditResult } from "./smartcutContent";
import { smartcutFindClip } from "./smartcutEdit";
import { smartcutPlayLen, smartcutTransitionActive } from "./smartcutLayout";
import { smartcutMaxTransitionMs } from "./smartcutTimeline";

type Timeline = components["schemas"]["Timeline"];
type TimelineClip = components["schemas"]["TimelineClip"];
type TimelineTrack = components["schemas"]["TimelineTrack"];
type Transition = components["schemas"]["Transition"];
type ClipAlternate = components["schemas"]["ClipAlternate"];
type TimelineText = components["schemas"]["TimelineText"];
type TimelineSubtitle = components["schemas"]["TimelineSubtitle"];

const fail = (reason: string): { ok: false; reason: string } => ({ ok: false, reason });
const sec = (ms: number) => `${Math.round(ms / 100) / 10} 秒`;
const MIN_TRANSITION_MS = 100;
const MAX_TRANSITION_MS = 3000;
const MIN_SPAN_MS = 100;

function replaceClip(t: Timeline, clipId: string, next: TimelineClip): Timeline {
  return { ...t, tracks: t.tracks.map(track => ({ ...track, clips: (track.clips ?? []).map(clip => (clip.id === clipId ? next : clip)) })) };
}

// ---------------------------------------------------------------- 转场

/**
 * 这一段的转入能设多长（毫秒）：画面轨上不超过本段与上一段各自的一半、最长 3 秒；第一段的转入是「从黑场淡入」，
 * 只看本段。其它轨的片段没有转场（返回 0）。
 */
export function smartcutTransitionLimitMs(t: Timeline, clipId: string): number {
  const ref = smartcutFindClip(t, clipId);
  if (!ref || ref.track.kind !== "video") return 0;
  return smartcutMaxTransitionMs(ref.track.clips.map(clip => ({ id: clip.id, durationMs: smartcutPlayLen(clip) })), clipId);
}

/** 最后一段的「淡出到黑场」能设多长：不超过本段的一半、最长 3 秒；不是最后一段时为 0（中间段的转出不生效）。 */
export function smartcutFadeOutLimitMs(t: Timeline, clipId: string): number {
  const ref = smartcutFindClip(t, clipId);
  if (!ref || ref.track.kind !== "video" || ref.index !== ref.track.clips.length - 1) return 0;
  return Math.min(MAX_TRANSITION_MS, Math.floor(smartcutPlayLen(ref.clip) / 2));
}

function checkTransition(t: Transition | null | undefined, limit: number, label: string): string {
  if (!t || !smartcutTransitionActive(t)) return "";
  if (t.durationMs < MIN_TRANSITION_MS) return `${label}至少 ${MIN_TRANSITION_MS} 毫秒`;
  if (limit < MIN_TRANSITION_MS) return `片段太短，放不下${label}`;
  if (t.durationMs > limit) return `${label}最长 ${sec(limit)}（不超过相邻两段较短者的一半）`;
  return "";
}

// ---------------------------------------------------------------- 片段设置

export type SmartcutClipPatch = {
  volume?: number;
  fadeInMs?: number;
  fadeOutMs?: number;
  /** null = 去掉转入。 */
  transitionIn?: Transition | null;
  /** null = 去掉淡出到黑场（只在最后一段上有意义）。 */
  transitionOut?: Transition | null;
};

/**
 * 应用片段设置（一次「应用」= 一步撤销）。音量 0~200；声音淡入 + 淡出不超过片段长度；转场不超过上限 ——
 * 不合法时整次不改，说清上限是多少。什么都没变时原样返回。
 */
export function smartcutPatchClip(t: Timeline, clipId: string, patch: SmartcutClipPatch): SmartcutEditResult<Timeline> {
  const ref = smartcutFindClip(t, clipId);
  if (!ref) return fail("片段不存在");
  const clip = ref.clip;
  const next: TimelineClip = { ...clip };
  if (patch.volume !== undefined) {
    if (!Number.isInteger(patch.volume) || patch.volume < 0 || patch.volume > 200) return fail("音量只支持 0~200");
    next.volume = patch.volume;
  }
  if (patch.fadeInMs !== undefined) next.fadeInMs = Math.max(0, Math.round(patch.fadeInMs));
  if (patch.fadeOutMs !== undefined) next.fadeOutMs = Math.max(0, Math.round(patch.fadeOutMs));
  const len = smartcutPlayLen(next);
  if ((next.fadeInMs ?? 0) + (next.fadeOutMs ?? 0) > len) return fail(`声音淡入与淡出合计不能超过片段长度 ${sec(len)}`);
  if (patch.transitionIn !== undefined) {
    if (patch.transitionIn && smartcutTransitionActive(patch.transitionIn)) {
      const reason = checkTransition(patch.transitionIn, smartcutTransitionLimitMs(t, clipId), ref.index === 0 && ref.track.kind === "video" ? "从黑场淡入" : "转场");
      if (reason) return fail(reason);
      next.transitionIn = { kind: patch.transitionIn.kind, durationMs: Math.round(patch.transitionIn.durationMs) };
    } else {
      delete next.transitionIn;
    }
  }
  if (patch.transitionOut !== undefined) {
    if (patch.transitionOut && smartcutTransitionActive(patch.transitionOut)) {
      const limit = smartcutFadeOutLimitMs(t, clipId);
      if (!limit) return fail("只有画面轨的最后一段能淡出到黑场");
      const reason = checkTransition(patch.transitionOut, limit, "淡出到黑场");
      if (reason) return fail(reason.replace("（不超过相邻两段较短者的一半）", "（不超过这一段的一半）"));
      next.transitionOut = { kind: "fade", durationMs: Math.round(patch.transitionOut.durationMs) };
    } else {
      delete next.transitionOut;
    }
  }
  if (JSON.stringify(next) === JSON.stringify(clip)) return { ok: true, value: t };
  next.origin = "user";
  return { ok: true, value: replaceClip(t, clipId, next) };
}

// ---------------------------------------------------------------- 备选（原片 / 重拍 / 去字幕）

/**
 * 切到第 `index` 个备选（SCD-35）：备选的素材与取用区间成为片段的内容，当前的那份进备选列表（超过 10 个丢最早的）。
 * 当前那份是什么来历片段上没记，按规则推：切回原片时当前的是最近一次生成的结果（取列表里最近一个非原片的种类，
 * 没有时用 `currentKind`）；切到生成结果时当前的就是原片。
 */
export function smartcutSwitchAlternate(t: Timeline, clipId: string, index: number, currentKind: ClipAlternate["kind"] = "regen"): SmartcutEditResult<Timeline> {
  const ref = smartcutFindClip(t, clipId);
  if (!ref) return fail("片段不存在");
  const list = ref.clip.alternates ?? [];
  const chosen = list[index];
  if (!chosen) return fail("这个备选不存在");
  const derived = [...list].reverse().find(a => a.kind !== "original")?.kind;
  const kind: ClipAlternate["kind"] = chosen.kind === "original" ? derived ?? currentKind : "original";
  const current: ClipAlternate = { mediaId: ref.clip.mediaId, sourceInMs: ref.clip.sourceInMs, sourceOutMs: ref.clip.sourceOutMs, kind };
  const rest = list.filter((_, i) => i !== index);
  const alternates = [...rest, current].slice(-smartcutContentLimits.alternates);
  const next: TimelineClip = {
    ...ref.clip, mediaId: chosen.mediaId, sourceInMs: chosen.sourceInMs, sourceOutMs: chosen.sourceOutMs, alternates, origin: "user"
  };
  delete next.mediaUrl;
  return { ok: true, value: replaceClip(t, clipId, next) };
}

// ---------------------------------------------------------------- 文字层

function textTracks(t: Timeline): TimelineTrack[] {
  return t.tracks.filter(track => track.kind === "text");
}

function overlaps(spans: ReadonlyArray<{ id: string; startMs: number; endMs: number }>, candidate: { id: string; startMs: number; endMs: number }): boolean {
  return spans.some(s => s.id !== candidate.id && candidate.startMs < s.endMs && s.startMs < candidate.endMs);
}

/**
 * 在播放头处加一条文字层（默认 2 秒、画面正中）：放在第一条文字轨上；那里那段时间已经有字时换到下一条文字轨，
 * 都放不下就新建一条文字轨（最多 4 条）。没有文字轨时也新建。返回新文字的 ID（调用方选中它）。
 */
export function smartcutAddText(t: Timeline, atMs: number, ids: { text: string; track: string }, content = "标题"): SmartcutEditResult<{ timeline: Timeline; id: string }> {
  if ((t.texts ?? []).length >= smartcutContentLimits.textLayers) return fail(`文字层最多 ${smartcutContentLimits.textLayers} 条`);
  const start = Math.max(0, Math.round(atMs));
  const item: TimelineText = { id: ids.text, trackId: "", startMs: start, endMs: start + 2000, text: content, style: {}, box: { xPct: 50, yPct: 50, anchor: "center" } };
  const tracks = textTracks(t);
  const free = tracks.find(track => !overlaps((t.texts ?? []).filter(x => x.trackId === track.id), item));
  if (free) {
    return { ok: true, value: { timeline: { ...t, texts: [...(t.texts ?? []), { ...item, trackId: free.id }] }, id: ids.text } };
  }
  if (tracks.length >= smartcutContentLimits.textTracks) return fail(`文字轨最多 ${smartcutContentLimits.textTracks} 条，这一时刻都已经有字`);
  const track: TimelineTrack = { id: ids.track, kind: "text", idx: t.tracks.length + 1, isMuted: false, volume: 100, clips: [] };
  return { ok: true, value: { timeline: { ...t, tracks: [...t.tracks, track], texts: [...(t.texts ?? []), { ...item, trackId: ids.track }] }, id: ids.text } };
}

export type SmartcutTextPatch = Partial<Pick<TimelineText, "text" | "startMs" | "endMs" | "style" | "box">>;

/** 改一条文字层（内容、起止、样式、位置）。起止至少 100 毫秒、内容不能为空、同一条文字轨不许重叠。 */
export function smartcutPatchText(t: Timeline, id: string, patch: SmartcutTextPatch): SmartcutEditResult<Timeline> {
  const texts = t.texts ?? [];
  const current = texts.find(x => x.id === id);
  if (!current) return fail("文字不存在");
  const next: TimelineText = { ...current, ...patch };
  if (!next.text.trim()) return fail("文字内容不能为空");
  if ([...next.text].length > smartcutContentLimits.textChars) return fail(`文字最多 ${smartcutContentLimits.textChars} 个字`);
  if (!(next.startMs >= 0) || next.endMs - next.startMs < MIN_SPAN_MS) return fail("结束要晚于开始至少 0.1 秒");
  if (overlaps(texts.filter(x => x.trackId === next.trackId), next)) return fail("与同一条文字轨上的文字重叠");
  if (JSON.stringify(next) === JSON.stringify(current)) return { ok: true, value: t };
  return { ok: true, value: { ...t, texts: texts.map(x => (x.id === id ? next : x)) } };
}

export function smartcutRemoveText(t: Timeline, id: string): Timeline {
  const texts = t.texts ?? [];
  return texts.some(x => x.id === id) ? { ...t, texts: texts.filter(x => x.id !== id) } : t;
}

// ---------------------------------------------------------------- 字幕

/** 一条字幕属于哪条字幕轨（旧数据没有 trackId 时归第一条字幕轨，与服务端回填一致）。 */
export function smartcutSubtitleTrackOf(t: Timeline, sub: Pick<TimelineSubtitle, "trackId">): string {
  return sub.trackId ?? t.tracks.find(track => track.kind === "subtitle")?.id ?? "";
}

/** 在播放头处加一条字幕（默认 2 秒、内容「新字幕」—— 空字幕保存时会被拒）。 */
export function smartcutAddSubtitle(t: Timeline, trackId: string, atMs: number, id: string): SmartcutEditResult<Timeline> {
  if (!t.tracks.some(track => track.id === trackId && track.kind === "subtitle")) return fail("字幕轨不存在");
  if ((t.subtitles ?? []).length >= smartcutContentLimits.subtitles) return fail(`字幕最多 ${smartcutContentLimits.subtitles} 条`);
  const start = Math.max(0, Math.round(atMs));
  const sub: TimelineSubtitle = { id, trackId, startMs: start, endMs: start + 2000, text: "新字幕" };
  return { ok: true, value: { ...t, subtitles: [...(t.subtitles ?? []), sub] } };
}

export type SmartcutSubtitlePatch = Partial<Pick<TimelineSubtitle, "text" | "startMs" | "endMs">>;

/**
 * 改一条字幕。硬错误（起止颠倒、空内容）拦下这一条；质量问题（过短、过长、重叠）只提示（`smartcutSubtitleIssues`）。
 * ⚠️ 跟随片段的字幕改了起止就**不再跟随**（去掉锚点）：它的起止本来是从片段算出来的，用户手动指定之后再按片段重算
 *    会把他刚改的时间冲掉。只改文字时照旧跟随。
 */
export function smartcutPatchSubtitle(t: Timeline, id: string, patch: SmartcutSubtitlePatch): SmartcutEditResult<Timeline> {
  const subs = t.subtitles ?? [];
  const current = subs.find(s => s.id === id);
  if (!current) return fail("字幕不存在");
  const next: TimelineSubtitle = { ...current, ...patch };
  if (!next.text.trim()) return fail("字幕内容不能为空");
  if ([...next.text].length > smartcutContentLimits.textChars) return fail(`字幕最多 ${smartcutContentLimits.textChars} 个字`);
  if (!(next.startMs >= 0) || next.endMs <= next.startMs) return fail("结束要晚于开始");
  const timeChanged = next.startMs !== current.startMs || next.endMs !== current.endMs;
  if (timeChanged && next.anchor) {
    delete next.anchor;
    next.hidden = false;
  }
  if (JSON.stringify(next) === JSON.stringify(current)) return { ok: true, value: t };
  return { ok: true, value: { ...t, subtitles: subs.map(s => (s.id === id ? next : s)) } };
}

export function smartcutRemoveSubtitles(t: Timeline, ids: readonly string[]): Timeline {
  const set = new Set(ids);
  const subs = t.subtitles ?? [];
  return subs.some(s => set.has(s.id)) ? { ...t, subtitles: subs.filter(s => !set.has(s.id)) } : t;
}

/** 新建一条字幕轨（最多 4 条），放在已有字幕轨之后。 */
export function smartcutAddSubtitleTrack(t: Timeline, id: string, lang?: string): SmartcutEditResult<Timeline> {
  const count = t.tracks.filter(track => track.kind === "subtitle").length;
  if (count >= smartcutContentLimits.subtitleTracks) return fail(`字幕轨最多 ${smartcutContentLimits.subtitleTracks} 条`);
  const track: TimelineTrack = { id, kind: "subtitle", idx: 0, isMuted: false, volume: 100, clips: [], ...(lang ? { lang } : {}) };
  const lastSub = t.tracks.map(tr => tr.kind).lastIndexOf("subtitle");
  const tracks = [...t.tracks];
  tracks.splice(lastSub >= 0 ? lastSub + 1 : tracks.length, 0, track);
  return { ok: true, value: { ...t, tracks } };
}

/**
 * 同类轨道里上移 / 下移一位（字幕轨的顺序决定预览与成片里谁在下面：第一条在最下）。到头时原样返回。
 */
export function smartcutMoveTrack(t: Timeline, trackId: string, delta: -1 | 1): Timeline {
  const index = t.tracks.findIndex(track => track.id === trackId);
  if (index < 0) return t;
  const kind = t.tracks[index]!.kind;
  const sameKind = t.tracks.map((track, i) => ({ track, i })).filter(x => x.track.kind === kind);
  const pos = sameKind.findIndex(x => x.i === index);
  const swap = sameKind[pos + delta];
  if (!swap) return t;
  const tracks = [...t.tracks];
  tracks[index] = swap.track;
  tracks[swap.i] = t.tracks[index]!;
  return { ...t, tracks };
}

/**
 * 轨道统一样式的合并（字幕轨）：`stroke` / `background` 两个子对象按键合并，值为 null 的键删掉（「去掉描边」），
 * 其余键直接覆盖；未知字段原样保留。返回新对象，不改传进来的。
 */
export function smartcutMergeStyle(base: Record<string, unknown> | null | undefined, patch: Record<string, unknown>): Record<string, unknown> {
  const merged: Record<string, unknown> = { ...(base ?? {}) };
  for (const [key, value] of Object.entries(patch)) {
    const prev = merged[key];
    const nested = (key === "stroke" || key === "background") && value && typeof value === "object" && prev && typeof prev === "object" && !Array.isArray(prev);
    merged[key] = nested ? { ...(prev as Record<string, unknown>), ...(value as Record<string, unknown>) } : value;
  }
  return Object.fromEntries(Object.entries(merged).filter(([, value]) => value !== null));
}
