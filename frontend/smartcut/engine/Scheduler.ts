/**
 * 预览调度（docs/agent/smartcut SCT-21，dev/03 §6）：给定时间线与成片时刻，算出此刻该画什么、该响什么、该叠什么字。
 * **纯计算**，不碰 DOM —— 播放引擎（`PlaybackEngine`）每帧问它一次，单测直接覆盖全部规则。
 *
 * 规则照服务端渲染（`backend/internal/timeline/domain/render.go` / `render_audio.go` / `overlay.go`），
 * 预览与成片的差别只能出在「近似」上（转场一律按淡入淡出、不做响度归一与人声闪避），不能出在时间上：
 * - **成片时间**一律由 `smartcutLayout` 算（画面轨按顺序首尾相接、转场重叠扣掉；跟随片段的字幕按锚点重算）。
 *   不读片段上存的 `startMs`：本地编辑之后、保存回来之前，那个值可能是旧的。
 * - **画面**：区间左闭右开 `[start, end)`；转场重叠区里前后两段同时在，后一段按进度 0→1 盖在前一段上；
 *   第一段的转入 = 从黑场淡入，最后一段的转出 = 淡出到黑场。其它转场类型（溶解、滑动、擦除）一律按淡入淡出近似。
 * - **声音**：音频轨片段（轨道静音或音量 0 不响）与画面轨里有声的视频原声（画面轨静音不响、图片没有声音）。
 *   增益 = trunc(片段音量 × 轨道音量 / 100) / 100 × 淡入淡出包络（与服务端的整数换算一致）；
 *   画面原声在转场处前一段淡出、后一段淡入（与用户设的淡入淡出取较大者），淡入 + 淡出夹在片段长度之内。
 * - **文字与字幕**：跳过隐藏的轨道与被裁掉（hidden）的字幕；字幕在前（按可见字幕轨的顺序，第一条在最下），
 *   再到文字层；字幕样式 = 轨道统一样式 + 条目样式（条目优先，嵌套对象递归合并，null 不覆盖）。
 *
 * ⚠️ 音频轨的「隐藏」照服务端：渲染只看静音，不看隐藏。预览若让隐藏的音频轨不出声，就会出现「预览里没声、成片里有声」。
 */
import type { components } from "@tabtv/api-client";
import { smartcutLayout, smartcutTransitionActive, type SmartcutLayout } from "~/utils/smartcutLayout";

type Timeline = components["schemas"]["Timeline"];
type TimelineClip = components["schemas"]["TimelineClip"];
type TimelineTrack = components["schemas"]["TimelineTrack"];
type ClipTransform = components["schemas"]["ClipTransform"];

export type ScheduleMediaKind = "video" | "image" | "audio";

/**
 * 素材信息（时间线上没有这些字段）：由调用方从预览素材 / 素材条里查到后注入。
 * 未知时：画面轨按视频、且按**有声**处理（多建一个静音的音频元素，好过漏掉一段原声）。
 */
export type ScheduleMediaInfo = { kind?: ScheduleMediaKind; hasAudio?: boolean; durationMs?: number; width?: number; height?: number };
export type ScheduleMediaInfoFn = (mediaId: string) => ScheduleMediaInfo | undefined;

/** 画面轨的一段（成片时间已算好）。 */
export type ScheduleVideoSegment = {
  /** 在画面轨里的序号（0 起）。 */
  index: number;
  clipId: string;
  mediaId: string;
  kind: "video" | "image";
  startMs: number;
  /** 不含：`[startMs, endMs)`。 */
  endMs: number;
  durationMs: number;
  sourceInMs: number;
  sourceOutMs: number;
  /** 百分比，100 = 原速。 */
  speed: number;
  /** 与上一段的转场重叠（第一段为 0）。 */
  overlapMs: number;
  /** 第一段的「从黑场淡入」时长（夹在片段长度之内）。 */
  fadeFromBlackMs: number;
  /** 最后一段的「淡出到黑场」时长（夹在片段长度之内）。 */
  fadeToBlackMs: number;
  /** 原样的变换（换算交给 `smartcutTransformCss`）。 */
  transform: ClipTransform | null;
};

/** 一个音源：音频轨的片段，或画面轨视频的原声。 */
export type ScheduleAudioSource = {
  /** 稳定键：同一片段同一个键（引擎据此复用 `<audio>`）。 */
  key: string;
  origin: "video" | "audio";
  trackId: string;
  clipId: string;
  mediaId: string;
  startMs: number;
  endMs: number;
  sourceInMs: number;
  sourceOutMs: number;
  speed: number;
  /** 轨道音量 × 片段音量（0~4），不含包络。 */
  baseGain: number;
  /** 夹过的淡入淡出（成片毫秒）。 */
  fadeInMs: number;
  fadeOutMs: number;
};

/** 一条可见的文字或字幕。 */
export type ScheduleOverlayItem = {
  /** `subtitle:{id}` / `text:{id}`。 */
  key: string;
  kind: "subtitle" | "text";
  id: string;
  trackId: string;
  /** 在可见的同类轨道里的顺序（从 1 起）：字幕第一条轨在最下。 */
  trackOrder: number;
  startMs: number;
  endMs: number;
  text: string;
  /** 字幕：轨道统一样式 + 条目样式；文字：条目样式。键按字典序（同样的样式序列化出同样的串，排版缓存键才稳定）。 */
  style: Record<string, unknown> | null;
  /** 文字层的位置框（字幕为 null）。 */
  box: Record<string, unknown> | null;
  /** 内容签名（文字 + 样式 + 位置框）：同一条被编辑过时签名会变，引擎据此判断叠加层要不要刷新。 */
  sig: string;
};

/** 一条时间线预先算好的调度表（每次时间线变化重建一次，之后每帧查询都很便宜）。 */
export type SchedulePlan = {
  width: number;
  height: number;
  totalMs: number;
  /** 成片长度：时刻 ≥ 它即播完。 */
  effectiveMs: number;
  /** 画面轨的各段（按顺序，起点非降）。时长不大于 0 的段不进表（它永远不可见）。 */
  video: ScheduleVideoSegment[];
  /** 全部音源（按起点排序）。 */
  audio: ScheduleAudioSource[];
  /** 全部文字与字幕（已按服务端的叠放顺序排好）。 */
  overlays: ScheduleOverlayItem[];
  layout: SmartcutLayout;
};

/** 此刻在画的一段：素材内时刻（毫秒，可带小数）与透明度。 */
export type ScheduleVisible = { segment: ScheduleVideoSegment; sourceMs: number; opacity: number };

/** 此刻在响的一个音源：素材内时刻与增益（含包络）。 */
export type ScheduleAudible = { source: ScheduleAudioSource; sourceMs: number; gain: number };

export type ScheduleFrame = {
  ms: number;
  ended: boolean;
  /** 下层在前：后一段盖在前一段上。 */
  video: ScheduleVisible[];
  audio: ScheduleAudible[];
  overlays: ScheduleOverlayItem[];
};

type JsonMap = Record<string, unknown>;

function isMap(v: unknown): v is JsonMap {
  return typeof v === "object" && v !== null && !Array.isArray(v);
}

function intOr(v: unknown, fallback: number): number {
  return typeof v === "number" && Number.isFinite(v) ? Math.trunc(v) : fallback;
}

function speedOf(clip: Pick<TimelineClip, "speed">): number {
  return clip.speed && clip.speed > 0 ? clip.speed : 100;
}

/** 淡入 + 淡出夹在片段长度之内（与 Go 的 `clampFades` 一致：超了 afade 会把整段压成静音）。 */
function clampFades(fadeIn: number, fadeOut: number, playLen: number): [number, number] {
  const i = Math.max(0, Math.min(fadeIn, playLen));
  const o = Math.max(0, Math.min(fadeOut, playLen - i));
  return [i, o];
}

/** Go 的 `unicode.IsSpace`（`strings.TrimSpace` 用它）：比 JS 的 `\s` 多 U+0085、少 U+FEFF。 */
function isGoSpace(cp: number): boolean {
  return (cp >= 0x09 && cp <= 0x0d) || cp === 0x20 || cp === 0x85 || cp === 0xa0 || cp === 0x1680
    || (cp >= 0x2000 && cp <= 0x200a) || cp === 0x2028 || cp === 0x2029 || cp === 0x202f || cp === 0x205f || cp === 0x3000;
}

/** Go 的 `strings.TrimSpace(s) == ""`（空白的字幕不出现在成片里，预览也不画）。 */
function blank(s: string): boolean {
  for (const ch of s) {
    if (!isGoSpace(ch.codePointAt(0)!)) return false;
  }
  return true;
}

/** 键按字典序重排（递归）：同样的样式永远序列化成同样的串。 */
function sortKeys(v: unknown): unknown {
  if (Array.isArray(v)) return v.map(sortKeys);
  if (!isMap(v)) return v;
  const out: JsonMap = {};
  for (const k of Object.keys(v).sort()) out[k] = sortKeys(v[k]);
  return out;
}

function mergeInto(dst: JsonMap, src: JsonMap): void {
  for (const [k, v] of Object.entries(src)) {
    if (v === null || v === undefined) continue;
    if (isMap(v)) {
      const cp: JsonMap = isMap(dst[k]) ? { ...(dst[k] as JsonMap) } : {};
      mergeInto(cp, v);
      dst[k] = cp;
      continue;
    }
    dst[k] = v;
  }
}

/**
 * 样式合并（与 Go 的 `MergeStyle` 一致）：从前往后逐层覆盖；两边都是对象时递归合并；src 里的 null 跳过；
 * 不是对象的层整层跳过；一层对象都没有时回 null。不改动传进来的层。
 */
export function mergeOverlayStyle(...layers: unknown[]): Record<string, unknown> | null {
  let merged: JsonMap | null = null;
  for (const layer of layers) {
    if (!isMap(layer)) continue;
    merged ??= {};
    mergeInto(merged, layer);
  }
  return merged ? (sortKeys(merged) as JsonMap) : null;
}

/** 字幕缺省下边距（画面高度的百分比，与服务端排版 `defaultStyle` 同值）与范围。 */
export const SUBTITLE_DEFAULT_BOTTOM_PCT = 8;
const SUBTITLE_BOTTOM_RANGE = [0, 50] as const;
/** 多条字幕同时有字时，两块之间的间距（画面高度的百分比，与服务端 `subtitleStackGapPct` 同值）。 */
export const SUBTITLE_STACK_GAP_PCT = 1;

/** 一条字幕的下边距：样式里有合法数字就夹到 0~50，否则按缺省 8（与服务端 `ParseStyle` 一致）。 */
export function subtitleBottomPct(style: unknown): number {
  const v = isMap(style) ? style.bottomPct : undefined;
  if (typeof v !== "number" || !Number.isFinite(v)) return SUBTITLE_DEFAULT_BOTTOM_PCT;
  return Math.max(SUBTITLE_BOTTOM_RANGE[0], Math.min(SUBTITLE_BOTTOM_RANGE[1], v));
}

/**
 * 同一时刻有多条字幕时每条要往上挪多少（画面高度的百分比，≥ 0）。与服务端 `composeSegment` 同一算法：
 * 第一条留在自己的下边距；后面每条的目标下边距 = 第一条的下边距 + 前面各条实际排出来的高度 + 间距（没字的不占位），
 * 只往上挪、不往下挪。`bottoms` 与 `heights` 按叠加顺序（`ScheduleOverlayItem` 的字幕部分）一一对应。
 */
export function stackSubtitleShifts(bottoms: readonly number[], heights: readonly number[], gapPct = SUBTITLE_STACK_GAP_PCT): number[] {
  if (bottoms.length <= 1) return bottoms.map(() => 0);
  let at = bottoms[0]!;
  return bottoms.map((bottom, i) => {
    const target = at;
    const h = heights[i] ?? 0;
    if (h > 0) at += h + gapPct;
    return Math.max(0, target - bottom);
  });
}

// ---------------------------------------------------------------- 建表

function emptyPlan(width = 0, height = 0): SchedulePlan {
  return {
    width, height, totalMs: 0, effectiveMs: 0, video: [], audio: [], overlays: [],
    layout: { clips: {}, subtitles: {}, texts: {}, totalMs: 0, effectiveMs: 0 }
  };
}

/** 一条时间线的调度表。`mediaInfo` 提供素材种类与有没有音轨（时间线上没有这些字段）。 */
export function buildSchedule(timeline: Timeline | null | undefined, mediaInfo?: ScheduleMediaInfoFn): SchedulePlan {
  if (!timeline || !Array.isArray(timeline.tracks)) return emptyPlan(timeline?.width, timeline?.height);
  const layout = smartcutLayout({
    tracks: timeline.tracks,
    subtitles: (timeline.subtitles ?? []).map(s => ({ id: s.id, startMs: s.startMs, endMs: s.endMs, anchor: s.anchor ?? null })),
    texts: timeline.texts ?? []
  });
  const plan: SchedulePlan = {
    width: timeline.width, height: timeline.height, totalMs: layout.totalMs, effectiveMs: layout.effectiveMs,
    video: [], audio: [], overlays: [], layout
  };

  // 画面轨只有一条（保存校验拦着）；多出来的历史数据与服务端一样不播。
  const videoTrack = timeline.tracks.find(t => t.kind === "video");
  if (videoTrack) addVideoTrack(plan, videoTrack, layout, mediaInfo);
  for (const track of timeline.tracks) {
    if (track.kind === "audio") addAudioTrack(plan, track, layout, mediaInfo);
  }
  plan.audio.sort((a, b) => a.startMs - b.startMs || a.key.localeCompare(b.key));
  plan.overlays = buildOverlays(timeline, layout);
  return plan;
}

function addVideoTrack(plan: SchedulePlan, track: TimelineTrack, layout: SmartcutLayout, mediaInfo?: ScheduleMediaInfoFn): void {
  const clips = track.clips ?? [];
  const trackVolume = intOr(track.volume, 100);
  const originAudible = !track.isMuted && trackVolume > 0;
  clips.forEach((clip, i) => {
    const placed = layout.clips[clip.id];
    if (!placed || placed.durationMs <= 0) return;
    const info = mediaInfo?.(clip.mediaId);
    const kind = info?.kind === "image" ? "image" : "video";
    const playLen = placed.durationMs;
    const last = i === clips.length - 1;
    plan.video.push({
      index: i,
      clipId: clip.id,
      mediaId: clip.mediaId,
      kind,
      startMs: placed.startMs,
      endMs: placed.startMs + playLen,
      durationMs: playLen,
      sourceInMs: clip.sourceInMs,
      sourceOutMs: clip.sourceOutMs,
      speed: speedOf(clip),
      overlapMs: placed.overlapMs,
      fadeFromBlackMs: i === 0 && smartcutTransitionActive(clip.transitionIn) ? Math.min(clip.transitionIn!.durationMs, playLen) : 0,
      fadeToBlackMs: last && smartcutTransitionActive(clip.transitionOut) ? Math.min(clip.transitionOut!.durationMs, playLen) : 0,
      transform: (clip.transform ?? null) as ClipTransform | null
    });

    // 画面原声（render_audio.go）：图片没有声音；确知没有音轨的跳过（生成出来的视频十有八九没有音轨）。
    if (!originAudible || kind === "image" || info?.hasAudio === false) return;
    const volume = Math.trunc((intOr(clip.volume, 100) * trackVolume) / 100);
    if (volume <= 0) return;
    let fadeIn = intOr(clip.fadeInMs, 0);
    let fadeOut = intOr(clip.fadeOutMs, 0);
    if (i > 0 && smartcutTransitionActive(clip.transitionIn)) fadeIn = Math.max(fadeIn, clip.transitionIn!.durationMs);
    const next = clips[i + 1];
    if (next && smartcutTransitionActive(next.transitionIn)) fadeOut = Math.max(fadeOut, next.transitionIn!.durationMs);
    const [fi, fo] = clampFades(fadeIn, fadeOut, playLen);
    plan.audio.push({
      key: `${track.id}:${clip.id}`, origin: "video", trackId: track.id, clipId: clip.id, mediaId: clip.mediaId,
      startMs: placed.startMs, endMs: placed.startMs + playLen, sourceInMs: clip.sourceInMs, sourceOutMs: clip.sourceOutMs,
      speed: speedOf(clip), baseGain: volume / 100, fadeInMs: fi, fadeOutMs: fo
    });
  });
}

function addAudioTrack(plan: SchedulePlan, track: TimelineTrack, layout: SmartcutLayout, mediaInfo?: ScheduleMediaInfoFn): void {
  const trackVolume = intOr(track.volume, 100);
  if (track.isMuted || trackVolume <= 0) return;
  for (const clip of track.clips ?? []) {
    const placed = layout.clips[clip.id];
    if (!placed || placed.durationMs <= 0) continue;
    // 跟随片段的音效（SC-F-56）：那一段被裁到看不见了就不播（与服务端渲染一样，隐藏不删除）。
    if (placed.follow?.hidden) continue;
    if (mediaInfo?.(clip.mediaId)?.hasAudio === false) continue;
    const volume = Math.trunc((intOr(clip.volume, 100) * trackVolume) / 100);
    if (volume <= 0) continue;
    const playLen = placed.durationMs;
    const [fi, fo] = clampFades(intOr(clip.fadeInMs, 0), intOr(clip.fadeOutMs, 0), playLen);
    // 跟随的按锚点算出来的取用区间与速度播（本地改了画面那一段、还没保存时也跟得上）。
    const window = placed.follow ?? { sourceInMs: clip.sourceInMs, sourceOutMs: clip.sourceOutMs, speed: speedOf(clip) };
    plan.audio.push({
      key: `${track.id}:${clip.id}`, origin: "audio", trackId: track.id, clipId: clip.id, mediaId: clip.mediaId,
      startMs: placed.startMs, endMs: placed.startMs + playLen, sourceInMs: window.sourceInMs, sourceOutMs: window.sourceOutMs,
      speed: window.speed, baseGain: volume / 100, fadeInMs: fi, fadeOutMs: fo
    });
  }
}

type OverlayEntry = { item: ScheduleOverlayItem; rank: number; order: number; trackIdx: number; seq: number };

/** 文字与字幕（与服务端快照 + `OverlaySegments` 同一套筛选与顺序）。 */
function buildOverlays(timeline: Timeline, layout: SmartcutLayout): ScheduleOverlayItem[] {
  const total = layout.effectiveMs;
  const entries: OverlayEntry[] = [];
  const push = (item: Omit<ScheduleOverlayItem, "sig">, rank: number, trackIdx: number) => {
    const startMs = Math.max(item.startMs, 0);
    const endMs = Math.min(item.endMs, total);
    if (endMs <= startMs || blank(item.text)) return;
    const sig = JSON.stringify([item.text, item.style, item.box]);
    entries.push({ item: { ...item, startMs, endMs, sig }, rank, order: item.trackOrder, trackIdx, seq: entries.length });
  };
  let subOrder = 0;
  let subIdx = 0;
  let textOrder = 0;
  for (const track of timeline.tracks) {
    if (track.kind === "subtitle") {
      if (track.isHidden) continue; // 隐藏的字幕轨不进成片（剪辑台里的眼睛开关）
      subOrder += 1;
      const trackIdx = subIdx++;
      for (const sub of timeline.subtitles ?? []) {
        if (sub.trackId !== track.id) continue;
        const placed = layout.subtitles[sub.id];
        if (!placed || placed.hidden) continue; // 跟随的片段把它所在区间裁掉了：隐藏，不删除
        push({
          key: `subtitle:${sub.id}`, kind: "subtitle", id: sub.id, trackId: track.id, trackOrder: subOrder,
          startMs: placed.startMs, endMs: placed.endMs, text: sub.text,
          style: mergeOverlayStyle(track.style, sub.style), box: null
        }, 0, trackIdx);
      }
    } else if (track.kind === "text") {
      if (track.isHidden) continue;
      textOrder += 1;
      for (const text of timeline.texts ?? []) {
        if (text.trackId !== track.id) continue;
        const placed = layout.texts[text.id];
        if (!placed) continue;
        push({
          key: `text:${text.id}`, kind: "text", id: text.id, trackId: track.id, trackOrder: textOrder,
          startMs: placed.startMs, endMs: placed.endMs, text: text.text,
          style: mergeOverlayStyle(text.style), box: isMap(text.box) ? (sortKeys(text.box) as JsonMap) : null
        }, 1, 0);
      }
    }
  }
  entries.sort((a, b) => a.rank - b.rank || a.order - b.order || a.trackIdx - b.trackIdx || a.item.startMs - b.item.startMs || a.seq - b.seq);
  return entries.map(e => e.item);
}

// ---------------------------------------------------------------- 查询

/** 线性包络：淡入段从 0 升到 1，淡出段从 1 降到 0（ffmpeg afade 缺省曲线是线性的）；两段相乘。 */
function envelope(t: number, playLen: number, fadeIn: number, fadeOut: number): number {
  let g = 1;
  if (fadeIn > 0 && t < fadeIn) g *= Math.max(0, t / fadeIn);
  if (fadeOut > 0 && t > playLen - fadeOut) g *= Math.max(0, (playLen - t) / fadeOut);
  return g;
}

/** 片段内成片偏移 → 素材内时刻（带小数，夹在取用区间里）。 */
function sourceAt(startMs: number, sourceInMs: number, sourceOutMs: number, speed: number, ms: number): number {
  const at = sourceInMs + ((ms - startMs) * speed) / 100;
  return Math.max(sourceInMs, Math.min(at, sourceOutMs));
}

/** 此刻在画的各段（下层在前）。 */
function visibleVideo(plan: SchedulePlan, ms: number): ScheduleVisible[] {
  const out: ScheduleVisible[] = [];
  for (const seg of plan.video) {
    if (seg.startMs > ms) break; // 起点非降：后面的都还没开始
    if (ms >= seg.endMs) continue;
    const t = ms - seg.startMs;
    let opacity = 1;
    if (seg.index > 0 && seg.overlapMs > 0 && t < seg.overlapMs) opacity *= t / seg.overlapMs;
    if (seg.fadeFromBlackMs > 0 && t < seg.fadeFromBlackMs) opacity *= t / seg.fadeFromBlackMs;
    if (seg.fadeToBlackMs > 0 && t > seg.durationMs - seg.fadeToBlackMs) opacity *= Math.max(0, (seg.durationMs - t) / seg.fadeToBlackMs);
    out.push({
      segment: seg,
      sourceMs: seg.kind === "image" ? 0 : sourceAt(seg.startMs, seg.sourceInMs, seg.sourceOutMs, seg.speed, ms),
      opacity: Math.max(0, Math.min(1, opacity))
    });
  }
  return out;
}

/** 此刻在响的音源。 */
function audibleAt(plan: SchedulePlan, ms: number): ScheduleAudible[] {
  const out: ScheduleAudible[] = [];
  for (const src of plan.audio) {
    if (src.startMs > ms) break;
    if (ms >= src.endMs) continue;
    const t = ms - src.startMs;
    out.push({
      source: src,
      sourceMs: sourceAt(src.startMs, src.sourceInMs, src.sourceOutMs, src.speed, ms),
      gain: src.baseGain * envelope(t, src.endMs - src.startMs, src.fadeInMs, src.fadeOutMs)
    });
  }
  return out;
}

/** 此刻可见的文字与字幕（顺序即叠放顺序）。 */
export function overlaysAt(plan: SchedulePlan, ms: number): ScheduleOverlayItem[] {
  if (ms < 0 || ms >= plan.effectiveMs) return [];
  return plan.overlays.filter(item => item.startMs <= ms && ms < item.endMs);
}

/**
 * 成片时刻 `ms` 的完整画面：画面、声音、文字。区间一律左闭右开；`ms ≥ effectiveMs` 即播完（什么都不画）。
 * 负数按 0。
 */
export function scheduleAt(plan: SchedulePlan, ms: number): ScheduleFrame {
  const at = Number.isFinite(ms) ? Math.max(0, ms) : 0;
  if (at >= plan.effectiveMs) return { ms: at, ended: true, video: [], audio: [], overlays: [] };
  return { ms: at, ended: false, video: visibleVideo(plan, at), audio: audibleAt(plan, at), overlays: overlaysAt(plan, at) };
}

/** 起点在 `ms` 之后的后面几段画面（预加载用，不含此刻已在画的）。 */
export function upcomingVideo(plan: SchedulePlan, ms: number, count = 2): ScheduleVideoSegment[] {
  const out: ScheduleVideoSegment[] = [];
  for (const seg of plan.video) {
    if (out.length >= count) break;
    if (seg.startMs > ms) out.push(seg);
  }
  return out;
}

/** 起点落在 `(ms, ms + withinMs]` 的音源（提前建好 `<audio>` 并定位到取用起点）。 */
export function upcomingAudio(plan: SchedulePlan, ms: number, withinMs: number): ScheduleAudioSource[] {
  const out: ScheduleAudioSource[] = [];
  for (const src of plan.audio) {
    if (src.startMs > ms + withinMs) break;
    if (src.startMs > ms) out.push(src);
  }
  return out;
}

/** 两组叠加条目是不是同一组（同样的条目、同样的内容签名、同样的顺序）。 */
export function sameOverlays(a: readonly ScheduleOverlayItem[], b: readonly ScheduleOverlayItem[]): boolean {
  if (a.length !== b.length) return false;
  for (let i = 0; i < a.length; i++) {
    if (a[i]!.key !== b[i]!.key || a[i]!.sig !== b[i]!.sig) return false;
  }
  return true;
}
