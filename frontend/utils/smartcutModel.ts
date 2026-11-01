/**
 * 剪辑台本地模型与整份保存请求体之间的换算（docs/agent/smartcut SCT-19，SCD-43）。
 *
 * store 里编辑的是读回来的 `Timeline`（带服务端算好的落点、`totalMs` / `effectiveMs`），保存时发的是 `TimelineContent`。
 * 两者字段名相同但不是一个东西：
 * - 读回的片段带 `idx` / `durationMs` / `mediaUrl` / `pendingGenerationId` 这些**服务端算的或服务端管的**字段，
 *   发回去要么被忽略、要么让人误以为客户端能改它们 —— 一律不发；
 * - 画面轨的 `startMs` 服务端按顺序重算，传了也忽略（SCD-15），不发，免得看起来像客户端在指定落点；
 * - 其它轨的 `startMs` 是放置位置（成片时间），必须发。
 *
 * ⚠️ 本地改动之后马上用 {@link smartcutRelayout} 重算落点（与 Go 的 `timeline/domain.Layout` 同一规则）：
 *    界面要立刻看到「拖完之后后面的段跟着移」，而保存回来之前服务端的落点还是旧的。
 *    这只是显示与播放用的近似，保存成功后以服务端返回的时间线为准（dev/03 §11）。
 */
import type { components } from "@tabtv/api-client";
import { smartcutLayout } from "./smartcutLayout";

type Timeline = components["schemas"]["Timeline"];
type TimelineTrack = components["schemas"]["TimelineTrack"];
type TimelineClip = components["schemas"]["TimelineClip"];
type TimelineContent = components["schemas"]["TimelineContent"];
type ContentTrack = components["schemas"]["TimelineContentTrack"];
type ContentClip = components["schemas"]["TimelineContentClip"];

let tempSeq = 0;

/**
 * 新建对象的临时 ID（`tmp-` 开头，正文最多 40 个字符，与服务端的 ID 规则一致）。保存后由响应的 `idMap` 换成正式 ID。
 * 时间戳 + 随机数 + 本页计数：同一毫秒里连建几个也不会撞（撞了的话服务端回 40001「ID 重复」）。
 */
export function smartcutTempId(): string {
  tempSeq += 1;
  const rand = Math.random().toString(36).slice(2, 10);
  return `tmp-${Date.now().toString(36)}${rand}${tempSeq.toString(36)}`.slice(0, 44);
}

function present<T>(v: T | null | undefined): v is T {
  return v !== undefined && v !== null;
}

function clipContent(clip: TimelineClip, trackKind: TimelineTrack["kind"]): ContentClip {
  const out: ContentClip = {
    id: clip.id,
    mediaId: clip.mediaId,
    sourceInMs: clip.sourceInMs,
    sourceOutMs: clip.sourceOutMs
  };
  if (trackKind !== "video") out.startMs = clip.startMs;
  if (present(clip.speed)) out.speed = clip.speed;
  if (present(clip.volume)) out.volume = clip.volume;
  if (present(clip.fadeInMs)) out.fadeInMs = clip.fadeInMs;
  if (present(clip.fadeOutMs)) out.fadeOutMs = clip.fadeOutMs;
  if (present(clip.transitionIn)) out.transitionIn = clip.transitionIn;
  if (present(clip.transitionOut)) out.transitionOut = clip.transitionOut;
  if (present(clip.transform)) out.transform = clip.transform;
  if (present(clip.origin)) out.origin = clip.origin;
  if (typeof clip.sourceNodeId === "string" && clip.sourceNodeId) out.sourceNodeId = clip.sourceNodeId;
  if (typeof clip.sourceShotId === "string" && clip.sourceShotId) out.sourceShotId = clip.sourceShotId;
  if (clip.alternates?.length) out.alternates = clip.alternates;
  if (trackKind === "audio" && present(clip.anchor)) out.anchor = clip.anchor;
  return out;
}

function trackContent(track: TimelineTrack): ContentTrack {
  const out: ContentTrack = { id: track.id, kind: track.kind, isMuted: track.isMuted, volume: track.volume };
  if (present(track.name)) out.name = track.name;
  if (present(track.role)) out.role = track.role;
  if (typeof track.lang === "string" && track.lang) out.lang = track.lang;
  if (present(track.isHidden)) out.isHidden = track.isHidden;
  if (present(track.style)) out.style = track.style;
  out.clips = (track.clips ?? []).map(clip => clipContent(clip, track.kind));
  return out;
}

/**
 * 读回的时间线 → 整份保存的请求体（不含乐观锁版本以外的服务端字段）。`version` 由调用方用当前的服务端版本覆盖。
 *
 * 字幕缺 `trackId` 时（旧数据，迁移 00067 之前写的）挂到第一条字幕轨上 —— 与迁移回填的规则一致；
 * 一条字幕轨都没有的话原样不带，交给保存前校验报出来，而不是凭空造一条轨。
 */
export function smartcutContentOf(timeline: Timeline): TimelineContent {
  const firstSubtitleTrack = timeline.tracks.find(track => track.kind === "subtitle")?.id;
  return {
    version: timeline.version,
    aspectRatio: timeline.aspectRatio as TimelineContent["aspectRatio"],
    fps: timeline.fps,
    width: timeline.width,
    height: timeline.height,
    tracks: timeline.tracks.map(trackContent),
    subtitles: (timeline.subtitles ?? []).map((sub) => {
      const out: NonNullable<TimelineContent["subtitles"]>[number] = {
        id: sub.id,
        trackId: sub.trackId ?? firstSubtitleTrack ?? "",
        startMs: sub.startMs,
        endMs: sub.endMs,
        text: sub.text
      };
      if (present(sub.style)) out.style = sub.style as Record<string, unknown>;
      if (present(sub.anchor)) out.anchor = sub.anchor;
      return out;
    }),
    texts: (timeline.texts ?? []).map((text) => {
      const out: NonNullable<TimelineContent["texts"]>[number] = {
        id: text.id,
        trackId: text.trackId,
        startMs: text.startMs,
        endMs: text.endMs,
        text: text.text
      };
      if (present(text.style)) out.style = text.style;
      if (present(text.box)) out.box = text.box;
      return out;
    })
  };
}

/**
 * 本地改动之后重算成片时间：画面轨的 `idx` / `startMs` / `durationMs`、其它轨的 `durationMs`、
 * 跟随片段的字幕的起止与「隐藏」、`totalMs` / `effectiveMs`。规则全部取自 {@link smartcutLayout}（与 Go 同一份 fixture）。
 *
 * 跟随片段的字幕被裁到隐藏时起止**保持现值**（与服务端读时的做法一致：隐藏的条目回到存储值，区间恢复后重新出现）。
 */
export function smartcutRelayout(timeline: Timeline): Timeline {
  const subtitles = timeline.subtitles ?? [];
  const layout = smartcutLayout({
    tracks: timeline.tracks,
    subtitles: subtitles.map(sub => ({ id: sub.id, startMs: sub.startMs, endMs: sub.endMs, anchor: sub.anchor ?? null })),
    texts: timeline.texts ?? []
  });
  const tracks = timeline.tracks.map((track) => ({
    ...track,
    clips: (track.clips ?? []).map((clip, i) => {
      const placed = layout.clips[clip.id];
      if (!placed) return clip;
      // 跟随片段的音频（SC-F-56）：落点、取用区间、速度跟着画面那一段走；隐藏的按存储值留在原处。
      if (placed.follow) {
        return placed.follow.hidden
          ? { ...clip, idx: i + 1, durationMs: placed.durationMs, hidden: true }
          : {
              ...clip, idx: i + 1, startMs: placed.startMs, durationMs: placed.durationMs, sourceInMs: placed.follow.sourceInMs,
              sourceOutMs: placed.follow.sourceOutMs, speed: placed.follow.speed, hidden: false
            };
      }
      return {
        ...clip,
        idx: i + 1,
        startMs: track.kind === "video" ? placed.startMs : clip.startMs,
        durationMs: placed.durationMs
      };
    })
  }));
  const placedSubs = subtitles.map((sub) => {
    if (!sub.anchor) return sub.hidden ? { ...sub, hidden: false } : sub;
    const placed = layout.subtitles[sub.id];
    if (!placed) return sub;
    return placed.hidden
      ? { ...sub, hidden: true }
      : { ...sub, startMs: placed.startMs, endMs: placed.endMs, hidden: false };
  });
  return { ...timeline, tracks, subtitles: placedSubs, totalMs: layout.totalMs, effectiveMs: layout.effectiveMs };
}

/**
 * 两份时间线的**内容**是否相同（只比会发给服务端的那部分）：草稿与服务端版本比对、判断一次改动是不是空改动都用它。
 * 落点、`totalMs` 这些算出来的字段不参与比较 —— 它们不同只说明还没重算，不说明内容变了。
 */
export function smartcutSameContent(a: Timeline | null | undefined, b: Timeline | null | undefined): boolean {
  if (!a || !b) return a === b;
  const strip = (t: Timeline) => JSON.stringify({ ...smartcutContentOf(t), version: 0 });
  return strip(a) === strip(b);
}
