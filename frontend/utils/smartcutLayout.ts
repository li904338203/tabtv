/**
 * 成片时间换算（docs/agent/smartcut SCK-15、SC-F-56；与 Go 的 `timeline/domain.Layout` 同一规则，SCT-04）。
 *
 * 规则：
 * - **画面轨**：`start[0] = 0`，`start[i] = start[i-1] + dur[i-1] − 转场[i]`。只扣**生效的**、i>0 的转入：
 *   第一段的转入是「从黑场淡入」，不与任何东西重叠。`dur` = 取用区间按变速换算后的播放长度。
 * - **音频 / 字幕 / 文字轨**：起点取存储值（成片时间），不首尾相接。
 * - **跟随片段的音频**（音频轨上带 `anchor` 的片段：按画面生成的音效）：与那一段的取用区间取交集 `[lo, hi)`，
 *   `start = 片段起点 + (lo − sourceIn) × 100 / speed`；音频取 `[lo − srcStart, hi − srcStart]`、速度跟着片段；
 *   交集不足 {@link smartcutMinClipMs} 时隐藏（按存储值留在原处）。锚点失效时按存储值、不隐藏（与 Go 的 `FollowClip` 一致）。
 * - **跟随片段的字幕**（带 `anchor`）：取素材区间与片段当前取用区间的交集 `[lo, hi)`，
 *   `start = 片段起点 + (lo − sourceIn) × 100 / speed`、`end` 同理；交集为空或**不到原区间的一半**时标「隐藏」
 *   （不删，起止回存储值，区间恢复后重新出现 —— 大半句已被裁掉的字幕留在剩下的一小截上只会一闪而过）；
 *   锚定的片段已不在时间线里、或锚点本身不成立（`srcEnd ≤ srcStart`）时按存储值、不隐藏。
 * - `totalMs` = 画面轨各段播放长度之和（不算重叠），`effectiveMs` = 成片总长（算上重叠，不小于 0）。
 *
 * ⚠️ 整数运算照 Go 的写法（截断取整），否则前后端在变速那一档差 1 毫秒。两边由 `api/fixtures/smartcut/layout/` 绊住
 *    （规则全文见 `api/fixtures/smartcut/README.md`「成片时间」一节），改规则时 fixture、Go、这里一起改。
 * ⚠️ 前端算的只用于显示与播放，保存后以服务端返回的时间线为准（dev/03 §11）。
 */
import type { components } from "@tabtv/api-client";

type Transition = components["schemas"]["Transition"];

/** 片段最短 100 毫秒（与 Go 的 `MinClipMs` 同一个数：更短的在 concat 里会被丢掉）。 */
export const smartcutMinClipMs = 100;

/** 换算用到的片段字段（整份保存的 `TimelineContentClip` 与读回的 `TimelineClip` 都满足）。 */
export type SmartcutLayoutClip = {
  id: string;
  sourceInMs: number;
  sourceOutMs: number;
  speed?: number;
  startMs?: number;
  transitionIn?: Transition | null;
  /** 跟随画面轨的一段（只在音频轨上，SC-F-56）。 */
  anchor?: SmartcutLayoutAnchor | null;
};

export type SmartcutLayoutTrack = { id: string; kind: string; clips?: readonly SmartcutLayoutClip[] };

export type SmartcutLayoutAnchor = { clipId: string; srcStartMs: number; srcEndMs: number };

export type SmartcutLayoutSpanInput = {
  id: string;
  startMs: number;
  endMs: number;
  anchor?: SmartcutLayoutAnchor | null;
};

export type SmartcutLayoutInput = {
  tracks: readonly SmartcutLayoutTrack[];
  subtitles?: readonly SmartcutLayoutSpanInput[];
  texts?: readonly Omit<SmartcutLayoutSpanInput, "anchor">[];
};

/** 跟随片段的音频按锚点算出的取用区间与速度；`hidden` = 跟随的那一段被裁到看不见了（不播放、不渲染）。 */
export type SmartcutFollow = { sourceInMs: number; sourceOutMs: number; speed: number; hidden: boolean };

/**
 * 片段在成片里的位置。`overlapMs` = 与上一段的转场重叠（只有画面轨 i>0 才可能非 0）。
 * `follow` 只在跟随片段的音频上有。
 */
export type SmartcutPlacedClip = { trackId: string; startMs: number; durationMs: number; overlapMs: number; follow?: SmartcutFollow };

/** 字幕 / 文字在成片里的位置。 */
export type SmartcutPlacedSpan = { startMs: number; endMs: number; hidden: boolean };

export type SmartcutLayout = {
  clips: Record<string, SmartcutPlacedClip>;
  subtitles: Record<string, SmartcutPlacedSpan>;
  texts: Record<string, SmartcutPlacedSpan>;
  totalMs: number;
  effectiveMs: number;
};

/** 取用区间长度（素材时间）。 */
export function smartcutSourceLen(clip: Pick<SmartcutLayoutClip, "sourceInMs" | "sourceOutMs">): number {
  return clip.sourceOutMs - clip.sourceInMs;
}

/**
 * 这一段在成片里占多久（与 Go 的 `Clip.PlayLen` 一致）：2 倍速的 10 秒素材只占 5 秒。变速缺省或非正数按原速。
 */
export function smartcutPlayLen(clip: Pick<SmartcutLayoutClip, "sourceInMs" | "sourceOutMs" | "speed">): number {
  const len = smartcutSourceLen(clip);
  const speed = clip.speed ?? 100;
  if (!(speed > 0)) return len;
  return Math.trunc((len * 100) / speed);
}

/** 有没有真的设转场（与 Go 的 `Transition.IsActive` 一致）：`none` 或 0 毫秒都是"没有"。 */
export function smartcutTransitionActive(t: Pick<Transition, "durationMs"> & { kind: string } | null | undefined): boolean {
  return !!t && t.kind !== "" && t.kind !== "none" && t.durationMs > 0;
}

/** 按起点放置的一个条目：成片起止，以及它在调用方列表里的下标（报错时指回去）。 */
export type SmartcutSpan = { start: number; end: number; index: number };

/**
 * 同一轨内第一处重叠里**后一个**条目的下标（与 Go 的 `firstOverlap` 一致）：按（起点，下标）排序后看相邻两个，
 * 后一个的起点早于前一个的结束即为重叠；首尾相接不算。没有重叠回 -1。
 * 音频轨与文字轨同轨不许重叠（SCD-15），字幕轨允许（两个人同时说话）。
 */
export function smartcutFirstOverlap(items: readonly SmartcutSpan[]): number {
  const sorted = [...items].sort((a, b) => a.start - b.start || a.index - b.index);
  for (let i = 1; i < sorted.length; i++) {
    if (sorted[i]!.start < sorted[i - 1]!.end) return sorted[i]!.index;
  }
  return -1;
}

/** 音频轨上第一处重叠里后一段在 `clips` 里的下标（拖动松手时的校验，SC-F-24）；没有重叠回 -1。 */
export function smartcutOverlapIndex(clips: readonly Pick<SmartcutLayoutClip, "sourceInMs" | "sourceOutMs" | "speed" | "startMs">[]): number {
  return smartcutFirstOverlap(clips.map((clip, index) => {
    const start = clip.startMs ?? 0;
    return { start, end: start + smartcutPlayLen(clip), index };
  }));
}

/** 按起点放置的轨道上最后一段的结束时刻（空轨为 0）：往这条轨上追加新段时默认放在这里，不与谁重叠。 */
export function smartcutTrackEnd(clips: readonly Pick<SmartcutLayoutClip, "sourceInMs" | "sourceOutMs" | "speed" | "startMs">[]): number {
  return clips.reduce((end, clip) => Math.max(end, (clip.startMs ?? 0) + smartcutPlayLen(clip)), 0);
}

/**
 * 画面轨的落点：按数组顺序排，后一段扣掉与上一段的转场重叠。
 */
export function smartcutSequence(clips: readonly SmartcutLayoutClip[]): Array<{ startMs: number; durationMs: number; overlapMs: number }> {
  const out: Array<{ startMs: number; durationMs: number; overlapMs: number }> = [];
  let cursor = 0;
  clips.forEach((clip, i) => {
    const durationMs = smartcutPlayLen(clip);
    const overlapMs = i > 0 && smartcutTransitionActive(clip.transitionIn) ? clip.transitionIn!.durationMs : 0;
    const startMs = cursor - overlapMs;
    out.push({ startMs, durationMs, overlapMs });
    cursor = startMs + durationMs;
  });
  return out;
}

/**
 * 跟随片段的条目在成片里的位置（SC-F-56，与 Go 的 `PlaceAnchored` 一致）。`clipStartMs` 是锚定片段的成片起点。
 * 返回 null 表示「隐藏」：交集为空，或不到原区间的一半。
 */
export function smartcutAnchoredSpan(
  anchor: SmartcutLayoutAnchor,
  clip: Pick<SmartcutLayoutClip, "sourceInMs" | "sourceOutMs" | "speed">,
  clipStartMs: number
): { startMs: number; endMs: number } | null {
  const visStart = Math.max(anchor.srcStartMs, clip.sourceInMs);
  const visEnd = Math.min(anchor.srcEndMs, clip.sourceOutMs);
  const visible = visEnd - visStart;
  if (visible <= 0 || visible * 2 < anchor.srcEndMs - anchor.srcStartMs) return null;
  const speed = clip.speed && clip.speed > 0 ? clip.speed : 100;
  return {
    startMs: clipStartMs + Math.trunc(((visStart - clip.sourceInMs) * 100) / speed),
    endMs: clipStartMs + Math.trunc(((visEnd - clip.sourceInMs) * 100) / speed)
  };
}

/**
 * 跟随片段的音频在成片里的落点与取用区间（SC-F-56，与 Go 的 `FollowClip` 一致）。`stored` 是音频片段自己存的值，
 * 隐藏时原样回它（按存储值留在原处）。
 */
export function smartcutFollowClip(
  anchor: SmartcutLayoutAnchor,
  target: Pick<SmartcutLayoutClip, "sourceInMs" | "sourceOutMs" | "speed">,
  targetStartMs: number,
  stored: Pick<SmartcutLayoutClip, "sourceInMs" | "sourceOutMs" | "speed" | "startMs">
): { startMs: number; durationMs: number; follow: SmartcutFollow } {
  const speed = target.speed && target.speed > 0 ? target.speed : 100;
  const lo = Math.max(anchor.srcStartMs, target.sourceInMs);
  const hi = Math.min(anchor.srcEndMs, target.sourceOutMs);
  if (hi - lo < smartcutMinClipMs) {
    const own = stored.speed && stored.speed > 0 ? stored.speed : 100;
    return {
      startMs: stored.startMs ?? 0,
      durationMs: smartcutPlayLen(stored),
      follow: { sourceInMs: stored.sourceInMs, sourceOutMs: stored.sourceOutMs, speed: own, hidden: true }
    };
  }
  const sourceInMs = lo - anchor.srcStartMs;
  const sourceOutMs = hi - anchor.srcStartMs;
  return {
    startMs: targetStartMs + Math.trunc(((lo - target.sourceInMs) * 100) / speed),
    durationMs: smartcutPlayLen({ sourceInMs, sourceOutMs, speed }),
    follow: { sourceInMs, sourceOutMs, speed, hidden: false }
  };
}

/** 锚点本身成立吗（与 Go 的 `ParseAnchor` 一致：缺片段或 `srcEnd ≤ srcStart` 都当「不跟随」）。 */
function validAnchor(anchor: SmartcutLayoutAnchor | null | undefined): anchor is SmartcutLayoutAnchor {
  return !!anchor && !!anchor.clipId && anchor.srcEndMs > anchor.srcStartMs;
}

/**
 * 整条时间线的成片时间。画面轨按数组顺序（整份保存的请求体就是这个顺序；读回的时间线已按 `idx` 排好）。
 * `totalMs` / `effectiveMs` 只看第一条画面轨（画面轨只有 1 条，SC-F-22）。
 */
export function smartcutLayout(input: SmartcutLayoutInput): SmartcutLayout {
  const clips: Record<string, SmartcutPlacedClip> = {};
  const byId = new Map<string, SmartcutLayoutClip>();
  const videoIds = new Set<string>();
  const anchored: SmartcutLayoutClip[] = [];
  let totalMs = 0;
  let effectiveMs = 0;
  let seenVideo = false;

  for (const track of input.tracks) {
    const list = track.clips ?? [];
    if (track.kind === "video") {
      const seq = smartcutSequence(list);
      list.forEach((clip, i) => {
        clips[clip.id] = { trackId: track.id, ...seq[i]! };
        byId.set(clip.id, clip);
        videoIds.add(clip.id);
      });
      if (!seenVideo) {
        seenVideo = true;
        totalMs = seq.reduce((sum, item) => sum + item.durationMs, 0);
        const overlaps = seq.reduce((sum, item) => sum + item.overlapMs, 0);
        effectiveMs = Math.max(0, totalMs - overlaps);
      }
      continue;
    }
    for (const clip of list) {
      clips[clip.id] = { trackId: track.id, startMs: clip.startMs ?? 0, durationMs: smartcutPlayLen(clip), overlapMs: 0 };
      byId.set(clip.id, clip);
      if (track.kind === "audio" && clip.anchor) anchored.push(clip);
    }
  }

  // 跟随片段的音频：画面轨都排好之后再算（锚点失效 —— 所属片段不在或不在画面轨上 —— 时按存储值）。
  for (const clip of anchored) {
    const anchor = clip.anchor;
    if (!validAnchor(anchor) || !videoIds.has(anchor.clipId)) continue;
    const target = byId.get(anchor.clipId)!;
    const placed = smartcutFollowClip(anchor, target, clips[anchor.clipId]!.startMs, clip);
    clips[clip.id] = { ...clips[clip.id]!, startMs: placed.startMs, durationMs: placed.durationMs, follow: placed.follow };
    // 跟随这段音频的字幕（识别出来的）按它算出来的位置换算。
    if (!placed.follow.hidden) {
      byId.set(clip.id, { ...clip, sourceInMs: placed.follow.sourceInMs, sourceOutMs: placed.follow.sourceOutMs,
        speed: placed.follow.speed, startMs: placed.startMs });
    }
  }

  const subtitles: Record<string, SmartcutPlacedSpan> = {};
  for (const sub of input.subtitles ?? []) {
    const stored = { startMs: sub.startMs, endMs: sub.endMs, hidden: false };
    const anchor = sub.anchor;
    const clip = validAnchor(anchor) ? byId.get(anchor.clipId) : undefined;
    if (!validAnchor(anchor) || !clip) {
      subtitles[sub.id] = stored;
      continue;
    }
    const span = smartcutAnchoredSpan(anchor, clip, clips[clip.id]!.startMs);
    subtitles[sub.id] = span ? { ...span, hidden: false } : { ...stored, hidden: true };
  }

  const texts: Record<string, SmartcutPlacedSpan> = {};
  for (const text of input.texts ?? []) {
    texts[text.id] = { startMs: text.startMs, endMs: text.endMs, hidden: false };
  }

  return { clips, subtitles, texts, totalMs, effectiveMs };
}
