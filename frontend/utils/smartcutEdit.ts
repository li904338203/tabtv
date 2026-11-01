/**
 * 剪辑台对整条时间线的编辑操作（docs/agent/smartcut SCT-20，SC-F-24）：工具条、快捷键、拖动松手都经这里算出新时间线，
 * 再 `store.commit` —— 一次操作 = 一个纯函数 = 一步撤销。
 *
 * 规则：
 * - 片段内的换算（分割、删左右、裁切、变速、转场收缩）全部复用 `smartcutContent.ts`（与 Go 同一套整数算法），这里只负责
 *   「在哪条轨上、换掉哪一段」；
 * - 画面轨按数组顺序首尾相接，落点由 `smartcutRelayout` / 服务端重算，这里**不写**画面轨的 `startMs`；
 *   其它轨按 `startMs` 放置，同轨不许重叠（字幕轨除外，那是字幕条目不是片段）——重叠时返回失败原因，调用方退回原处并提示；
 * - 什么都没变时原样返回同一个对象（`store.commit` 据此不压空撤销步）。
 *
 * ⚠️ 不碰 DOM、不发请求；所有导出带 `smartcut` 前缀（Nuxt 自动导入 `utils/` 的全部导出，重名时悄悄顶替）。
 */
import type { components } from "@tabtv/api-client";
import {
  smartcutFitTransitions,
  smartcutSetSpeed,
  smartcutSplitClip,
  smartcutTrimEdge,
  smartcutTrimEndAt,
  smartcutTrimStartAt,
  type SmartcutEditResult
} from "./smartcutContent";
import { smartcutOverlapIndex, smartcutPlayLen, smartcutTrackEnd } from "./smartcutLayout";
import { smartcutMsToPx, smartcutPxToMs } from "./smartcutTimeline";

type Timeline = components["schemas"]["Timeline"];
type TimelineTrack = components["schemas"]["TimelineTrack"];
type TimelineClip = components["schemas"]["TimelineClip"];

export type SmartcutClipRef = { track: TimelineTrack; trackIndex: number; clip: TimelineClip; index: number };

const fail = (reason: string): { ok: false; reason: string } => ({ ok: false, reason });

/** 找一段片段在哪条轨上、第几个。 */
export function smartcutFindClip(t: Timeline, clipId: string): SmartcutClipRef | null {
  for (let trackIndex = 0; trackIndex < t.tracks.length; trackIndex++) {
    const track = t.tracks[trackIndex]!;
    const index = (track.clips ?? []).findIndex(clip => clip.id === clipId);
    if (index >= 0) return { track, trackIndex, clip: track.clips[index]!, index };
  }
  return null;
}

/** 换掉一条轨的片段列表（其余轨原样）。 */
function withClips(t: Timeline, trackIndex: number, clips: TimelineClip[]): Timeline {
  return { ...t, tracks: t.tracks.map((track, i) => (i === trackIndex ? { ...track, clips } : track)) };
}

/** 画面轨的片段列表变了之后把转场收进合法范围（片段变短时上一段的转入可能超出上限）。 */
function fitted(track: TimelineTrack, clips: TimelineClip[]): TimelineClip[] {
  return smartcutFitTransitions(clips, track.kind).clips;
}

/** 按起点放置的轨道上第一处重叠的说明；没有重叠为空串。 */
function overlapReason(track: TimelineTrack, clips: TimelineClip[]): string {
  if (track.kind === "video") return "";
  // 跟随片段的音效不参与同轨重叠（SC-F-56：落点跟着画面走，与服务端的校验一致）
  return smartcutOverlapIndex(clips.filter(c => !c.anchor)) >= 0 ? "与同一条轨上的片段重叠，已退回原处" : "";
}

/** 跟随画面一段的音效（SC-F-56）：落点与取用区间跟着那一段算，不能单独拖动或裁切。 */
const ANCHORED_CLIP = "这段音效跟随画面的一段：移动、裁切那一段时它会一起走";

/** 用户动过的片段：来源改写为 user（SCD-5：AI 的追加指令不碰用户改过的）。 */
function touched(clip: TimelineClip): TimelineClip {
  return clip.origin === "user" ? clip : { ...clip, origin: "user" };
}

// ---------------------------------------------------------------- 移动

/**
 * 画面轨拖动排序：把 `clipId` 挪到第 `position` 位（1 起，取自 `smartcutDropPosition`）。位置没变时原样返回。
 * 落点由重算给出（后面的段跟着移）。
 */
export function smartcutReorderClip(t: Timeline, clipId: string, position: number): Timeline {
  const ref = smartcutFindClip(t, clipId);
  if (!ref || ref.track.kind !== "video") return t;
  const clips = [...ref.track.clips];
  const to = Math.max(0, Math.min(clips.length - 1, Math.round(position) - 1));
  if (to === ref.index) return t;
  const [moved] = clips.splice(ref.index, 1);
  clips.splice(to, 0, touched(moved!));
  return withClips(t, ref.trackIndex, fitted(ref.track, clips));
}

/** 按起点放置的轨道上把片段挪到成片时间 `startMs`（不小于 0）。重叠时失败。 */
export function smartcutMoveClip(t: Timeline, clipId: string, startMs: number): SmartcutEditResult<Timeline> {
  const ref = smartcutFindClip(t, clipId);
  if (!ref) return fail("片段不存在");
  if (ref.track.kind === "video") return fail("画面轨的片段按顺序首尾相接，拖动是调整顺序");
  if (ref.clip.anchor) return fail(ANCHORED_CLIP);
  const start = Math.max(0, Math.round(startMs));
  if (start === ref.clip.startMs) return { ok: true, value: t };
  const clips = ref.track.clips.map((clip, i) => (i === ref.index ? touched({ ...clip, startMs: start }) : clip));
  const reason = overlapReason(ref.track, clips);
  return reason ? fail(reason) : { ok: true, value: withClips(t, ref.trackIndex, clips) };
}

// ---------------------------------------------------------------- 裁切与分割

/**
 * 拖动裁切把手（`deltaMs` 是成片毫秒，右正左负）：取用区间不超出素材（`sourceDurationMs` 未知时只夹 0）、
 * 不短于 100 毫秒；画面轨后面的段跟着移，按起点放置的轨道拖左端时右边缘不动。重叠时失败。
 */
export function smartcutTrimClip(
  t: Timeline,
  clipId: string,
  edge: "start" | "end",
  deltaMs: number,
  sourceDurationMs?: number
): SmartcutEditResult<Timeline> {
  const ref = smartcutFindClip(t, clipId);
  if (!ref) return fail("片段不存在");
  if (ref.clip.anchor) return fail(ANCHORED_CLIP);
  const placed = ref.track.kind !== "video";
  const next = smartcutTrimEdge(ref.clip, edge, deltaMs, { sourceDurationMs, placed });
  if (next === ref.clip) return { ok: true, value: t };
  const clips = ref.track.clips.map((clip, i) => (i === ref.index ? next : clip));
  const reason = overlapReason(ref.track, clips);
  if (reason) return fail(reason);
  return { ok: true, value: withClips(t, ref.trackIndex, placed ? clips : fitted(ref.track, clips)) };
}

/** 播放头落在这段里时它在片段里的成片偏移；不在里面时为 null。 */
function offsetIn(clip: TimelineClip, playheadMs: number): number | null {
  const len = smartcutPlayLen(clip);
  const at = playheadMs - clip.startMs;
  return at > 0 && at < len ? at : null;
}

/** 以播放头为界分割（`newId` 是右半段的临时 ID）。 */
export function smartcutSplitAt(t: Timeline, clipId: string, playheadMs: number, newId: string): SmartcutEditResult<Timeline> {
  const ref = smartcutFindClip(t, clipId);
  if (!ref) return fail("片段不存在");
  if (ref.clip.anchor) return fail(ANCHORED_CLIP);
  const at = offsetIn(ref.clip, playheadMs);
  if (at === null) return fail("播放头不在这个片段里");
  const split = smartcutSplitClip(ref.clip, at, newId);
  if (!split.ok) return split;
  const clips = [...ref.track.clips];
  clips.splice(ref.index, 1, split.value[0], split.value[1]);
  return { ok: true, value: withClips(t, ref.trackIndex, ref.track.kind === "video" ? fitted(ref.track, clips) : clips) };
}

/** 删除播放头左侧（保留右边那部分）。 */
export function smartcutDeleteLeftAt(t: Timeline, clipId: string, playheadMs: number): SmartcutEditResult<Timeline> {
  const ref = smartcutFindClip(t, clipId);
  if (!ref) return fail("片段不存在");
  if (ref.clip.anchor) return fail(ANCHORED_CLIP);
  const at = offsetIn(ref.clip, playheadMs);
  if (at === null) return fail("播放头不在这个片段里");
  const next = smartcutTrimStartAt(ref.clip, at);
  if (!next.ok) return next;
  const clips = ref.track.clips.map((clip, i) => (i === ref.index ? next.value : clip));
  return { ok: true, value: withClips(t, ref.trackIndex, ref.track.kind === "video" ? fitted(ref.track, clips) : clips) };
}

/** 删除播放头右侧（保留左边那部分）。 */
export function smartcutDeleteRightAt(t: Timeline, clipId: string, playheadMs: number): SmartcutEditResult<Timeline> {
  const ref = smartcutFindClip(t, clipId);
  if (!ref) return fail("片段不存在");
  if (ref.clip.anchor) return fail(ANCHORED_CLIP);
  const at = offsetIn(ref.clip, playheadMs);
  if (at === null) return fail("播放头不在这个片段里");
  const next = smartcutTrimEndAt(ref.clip, at);
  if (!next.ok) return next;
  const clips = ref.track.clips.map((clip, i) => (i === ref.index ? next.value : clip));
  return { ok: true, value: withClips(t, ref.trackIndex, ref.track.kind === "video" ? fitted(ref.track, clips) : clips) };
}

// ---------------------------------------------------------------- 删除、复制、变速

/** 删掉一组片段（跨轨也行）。一个都不存在时原样返回。跟随它们的字幕不删（退回存储的起止，SC-F-56）。 */
export function smartcutRemoveClips(t: Timeline, clipIds: readonly string[]): Timeline {
  const ids = new Set(clipIds);
  let changed = false;
  const tracks = t.tracks.map((track) => {
    const clips = (track.clips ?? []).filter(clip => !ids.has(clip.id));
    if (clips.length === (track.clips ?? []).length) return track;
    changed = true;
    return { ...track, clips };
  });
  return changed ? { ...t, tracks } : t;
}

/**
 * 复制一段（⌘D）：画面轨插在原片段之后；按起点放置的轨道放在原片段结束处，那里放不下就放到轨道末尾。
 * 副本不继承溯源（`sourceShotId`，复制出来的会撞「同一镜一段」的唯一约束）、进行中的生成、备选与跟随。
 */
export function smartcutDuplicateClip(t: Timeline, clipId: string, newId: string): SmartcutEditResult<Timeline> {
  const ref = smartcutFindClip(t, clipId);
  if (!ref) return fail("片段不存在");
  // 副本也不跟随：两段跟着同一处画面会叠在一起响（跟随的音效复制出来就是一段普通音效，放在原来那段之后）
  const { sourceShotId: _shot, pendingGenerationId: _pending, alternates: _alternates, anchor: _anchor, hidden: _hidden, ...rest } = ref.clip;
  const copy: TimelineClip = { ...rest, id: newId, origin: "user" };
  const clips = [...ref.track.clips];
  if (ref.track.kind === "video") {
    clips.splice(ref.index + 1, 0, copy);
    return { ok: true, value: withClips(t, ref.trackIndex, clips) };
  }
  copy.startMs = ref.clip.startMs + smartcutPlayLen(ref.clip);
  clips.push(copy);
  if (smartcutOverlapIndex(clips.filter(c => !c.anchor)) >= 0) {
    copy.startMs = smartcutTrackEnd(ref.track.clips);
  }
  return { ok: true, value: withClips(t, ref.trackIndex, clips) };
}

/** 变速（档位或数值）。画面轨后面的段跟着移；按起点放置的轨道变长后撞上下一段时失败。 */
export function smartcutSetClipSpeed(t: Timeline, clipId: string, speed: number): SmartcutEditResult<Timeline> {
  const ref = smartcutFindClip(t, clipId);
  if (!ref) return fail("片段不存在");
  if (ref.clip.anchor) return fail("这段音效跟随画面的一段：速度跟着那一段，改那一段的速度");
  if ((ref.clip.speed ?? 100) === speed) return { ok: true, value: t };
  const next = smartcutSetSpeed(ref.clip, speed);
  if (!next.ok) return next;
  const clips = ref.track.clips.map((clip, i) => (i === ref.index ? next.value : clip));
  const reason = overlapReason(ref.track, clips);
  if (reason) return fail(reason.replace("已退回原处", "变速后放不下"));
  return { ok: true, value: withClips(t, ref.trackIndex, ref.track.kind === "video" ? fitted(ref.track, clips) : clips) };
}

// ---------------------------------------------------------------- 轨道

export type SmartcutTrackPatch = Partial<Pick<TimelineTrack, "name" | "isMuted" | "isHidden" | "volume" | "role" | "lang" | "style">>;

/** 改轨道属性（名称、静音、隐藏、音量、用途、语言、样式）。值都没变时原样返回。 */
export function smartcutPatchTrack(t: Timeline, trackId: string, patch: SmartcutTrackPatch): Timeline {
  const index = t.tracks.findIndex(track => track.id === trackId);
  if (index < 0) return t;
  const track = t.tracks[index]!;
  const changed = (Object.keys(patch) as Array<keyof SmartcutTrackPatch>)
    .some(key => JSON.stringify(track[key] ?? null) !== JSON.stringify(patch[key] ?? null));
  if (!changed) return t;
  return { ...t, tracks: t.tracks.map((tr, i) => (i === index ? { ...tr, ...patch } : tr)) };
}

// ---------------------------------------------------------------- 字幕与文字的时间

/**
 * 拖动一条字幕或文字层到新的成片起点（时长不变，不早于 0）。跟随片段的字幕不能单独拖（它跟着片段走，SC-F-56）；
 * 同一条文字轨不许重叠，字幕轨允许（两个人同时说话）。
 */
export function smartcutMoveSpan(t: Timeline, kind: "subtitle" | "text", id: string, startMs: number): SmartcutEditResult<Timeline> {
  const start = Math.max(0, Math.round(startMs));
  if (kind === "subtitle") {
    const sub = (t.subtitles ?? []).find(s => s.id === id);
    if (!sub) return fail("字幕不存在");
    if (sub.anchor) return fail("这条字幕跟随片段，移动片段时它会一起走");
    if (sub.startMs === start) return { ok: true, value: t };
    const len = sub.endMs - sub.startMs;
    return { ok: true, value: { ...t, subtitles: t.subtitles.map(s => (s.id === id ? { ...s, startMs: start, endMs: start + len } : s)) } };
  }
  const text = (t.texts ?? []).find(x => x.id === id);
  if (!text) return fail("文字不存在");
  if (text.startMs === start) return { ok: true, value: t };
  const len = text.endMs - text.startMs;
  const texts = (t.texts ?? []).map(x => (x.id === id ? { ...x, startMs: start, endMs: start + len } : x));
  const sameTrack = texts.filter(x => x.trackId === text.trackId).sort((a, b) => a.startMs - b.startMs);
  for (let i = 1; i < sameTrack.length; i++) {
    if (sameTrack[i]!.startMs < sameTrack[i - 1]!.endMs) return fail("与同一条文字轨上的文字重叠，已退回原处");
  }
  return { ok: true, value: { ...t, texts } };
}

// ---------------------------------------------------------------- 选择与视图

/** 播放头下画面轨的那一段（分割、删左右没有选中片段时用它）。 */
export function smartcutVideoClipAt(t: Timeline, playheadMs: number): TimelineClip | null {
  const video = t.tracks.find(track => track.kind === "video");
  if (!video) return null;
  return video.clips.find(clip => playheadMs > clip.startMs && playheadMs < clip.startMs + smartcutPlayLen(clip)) ?? null;
}

/**
 * 横向虚拟化（SCD-44）：只渲染可见范围前后各一屏的片段。返回成片时间的区间 [fromMs, toMs)。
 * 200 个片段的时间线全部渲染成 DOM 时拖动掉帧；多留一屏是为了快速滚动时边缘不闪白。
 */
export function smartcutVisibleRange(scrollLeftPx: number, viewportPx: number, zoom: number): { fromMs: number; toMs: number } {
  const from = Math.max(0, scrollLeftPx - viewportPx);
  const to = scrollLeftPx + viewportPx * 2;
  return { fromMs: smartcutPxToMs(from, zoom), toMs: smartcutPxToMs(to, zoom) };
}

/** 一段（或一条字幕）与可见区间有没有交集。 */
export function smartcutInRange(startMs: number, endMs: number, range: { fromMs: number; toMs: number }): boolean {
  return endMs >= range.fromMs && startMs <= range.toMs;
}

/** 适配宽度的缩放档位：最大的、能把整条成片放进 `viewportPx` 的档位（放不下时取最小档）。 */
export function smartcutFitZoom(totalMs: number, viewportPx: number, levels: readonly number[]): number {
  if (!(totalMs > 0) || !(viewportPx > 0)) return 0;
  for (let i = levels.length - 1; i >= 0; i--) {
    if (smartcutMsToPx(totalMs, i) <= viewportPx) return i;
  }
  return 0;
}

/**
 * 画面轨拖动时的落点（SC-F-24）：被拖的那一段**不算在内**，数其余各段里中点已经被越过的有几个，得出它在新列表里排第几
 * （1 起，最大是其余段数 + 1 = 放到最后）。
 *
 * ⚠️ 不能直接用 `smartcutDropPosition`：把被拖的段自己也算进去的话，往右拖一点点它自己的中点就被「越过」了，
 *    落点多算一位；把它排除之后那个函数又把上限夹在其余段数，放不到最后一位。
 */
export function smartcutReorderTarget(
  clips: ReadonlyArray<{ id: string; startMs: number; durationMs: number }>,
  draggedId: string,
  dropMs: number
): number {
  const others = clips.filter(clip => clip.id !== draggedId);
  return 1 + others.filter(clip => dropMs >= clip.startMs + clip.durationMs / 2).length;
}

/** 画面轨拖动时插入线的位置（成片时间）：落点那一位原来那段的起点；放到最后时是最后一段的结束。 */
export function smartcutInsertMarkMs(
  clips: ReadonlyArray<{ id: string; startMs: number; durationMs: number }>,
  draggedId: string,
  position: number
): number {
  const others = clips.filter(clip => clip.id !== draggedId);
  const at = others[position - 1];
  if (at) return at.startMs;
  const last = others[others.length - 1];
  return last ? last.startMs + last.durationMs : 0;
}

/**
 * 拖动按起点放置的片段时的吸附：起点与终点各自找最近的锚点，取移动量小的那个（两端都能贴住别的片段边缘、播放头、节拍）。
 * `snapOne` 是单点吸附（`smartcutSnapMs` 套上当前缩放），这里只决定用哪一端。
 */
export function smartcutSnapSpan(startMs: number, durationMs: number, snapOne: (ms: number) => number): number {
  const byStart = snapOne(startMs);
  const byEnd = snapOne(startMs + durationMs) - durationMs;
  const startSnapped = byStart !== startMs;
  const endSnapped = byEnd !== startMs;
  if (startSnapped && endSnapped) return Math.abs(byStart - startMs) <= Math.abs(byEnd - startMs) ? byStart : byEnd;
  if (startSnapped) return byStart;
  if (endSnapped) return byEnd;
  return startMs;
}

/**
 * 改一段的画面变换（SC-F-43）：`null` = 重置回等比适配（去掉 `transform` 字段）。内容没变时原样返回。
 */
export function smartcutSetClipTransform(t: Timeline, clipId: string, transform: TimelineClip["transform"] | null): Timeline {
  const ref = smartcutFindClip(t, clipId);
  if (!ref) return t;
  if (JSON.stringify(ref.clip.transform ?? null) === JSON.stringify(transform ?? null)) return t;
  const { transform: _old, ...rest } = ref.clip;
  const next: TimelineClip = transform ? { ...rest, transform, origin: "user" } : { ...rest, origin: "user" };
  return withClips(t, ref.trackIndex, ref.track.clips.map((clip, i) => (i === ref.index ? next : clip)));
}

/** 图片铺上画面轨时的默认时长（与 Go `timeline/app.SyncImageMs` 同值）。 */
export const smartcutSyncImageMs = 3000;
const MAX_AUDIO_TRACKS = 6;

type AppendInput = { kind: string; status: string; used: boolean; mediaId?: string; durationMs?: number; audioRole?: string | null; sourceNodeId: string };

/**
 * 「追加到末尾」（SC-F-14）：把素材条里就绪、还没用上的素材接到轨道末尾，规则照服务端的连线即入轨（`AppendSynced`）：
 * 视频、图片进画面轨（图片 3 秒），音频按用途进对应的音频轨（没有就认领一条没定用途的，再没有就新建，最多 6 条），
 * 接在那条轨现有内容之后；时长未知的视频与音频不铺（猜一个数的话成片长度对不上）。返回新时间线与铺上了几段。
 */
export function smartcutAppendInputs(t: Timeline, inputs: readonly AppendInput[], newId: () => string): { timeline: Timeline; appended: number } {
  let next: Timeline = { ...t, tracks: t.tracks.map(track => ({ ...track, clips: [...(track.clips ?? [])] })) };
  let appended = 0;
  for (const input of inputs) {
    if (input.status !== "ready" || input.used || !input.mediaId) continue;
    const base = { idx: 0, mediaId: input.mediaId, sourceInMs: 0, startMs: 0, durationMs: 0, speed: 100, volume: 100, origin: "user" as const, sourceNodeId: input.sourceNodeId };
    if (input.kind === "video" || input.kind === "image") {
      const out = input.kind === "image" ? smartcutSyncImageMs : input.durationMs ?? 0;
      const video = next.tracks.find(track => track.kind === "video");
      if (!video || out <= 0) continue;
      video.clips.push({ ...base, id: newId(), sourceOutMs: out });
      appended += 1;
      continue;
    }
    if (input.kind !== "audio" || !(input.durationMs && input.durationMs > 0)) continue;
    const role = (input.audioRole ?? "music") as NonNullable<TimelineTrack["role"]>;
    let track = next.tracks.find(tr => tr.kind === "audio" && tr.role === role)
      ?? next.tracks.find(tr => tr.kind === "audio" && !tr.role);
    if (track && !track.role) track.role = role;
    if (!track) {
      if (next.tracks.filter(tr => tr.kind === "audio").length >= MAX_AUDIO_TRACKS) continue;
      track = { id: newId(), kind: "audio", idx: 0, isMuted: false, volume: 100, role, clips: [] };
      const lastAudio = next.tracks.map(tr => tr.kind).lastIndexOf("audio");
      const tracks = [...next.tracks];
      tracks.splice(lastAudio >= 0 ? lastAudio + 1 : 1, 0, track);
      next = { ...next, tracks };
    }
    track.clips.push({ ...base, id: newId(), sourceOutMs: input.durationMs, startMs: smartcutTrackEnd(track.clips) });
    appended += 1;
  }
  return { timeline: appended ? next : t, appended };
}
