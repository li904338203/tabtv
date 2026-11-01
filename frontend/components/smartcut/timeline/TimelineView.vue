<script setup lang="ts">
/**
 * 多轨时间线（SCT-20，SCD-44）：刻度与播放头、轨道头、片段 / 字幕 / 文字层、节拍、横向虚拟化、拖动与裁切。
 *
 * 交互规则：
 * - **拖动中只改本地视图，松手才提交一次**（一次拖动 = 一步撤销 = 一次保存请求）：拖动中的样子是 `dragPreview`
 *   （纯函数算出来的整条时间线），松手时用同一个纯函数对 store 里的最新内容再算一次再 `commit`；
 * - 画面轨拖动 = 调整顺序（插入线按中点规则），其它轨拖动 = 改成片起点（可吸附到片段边缘、播放头、节拍）；
 *   松手时同轨重叠就退回原处并说明；
 * - 指针捕获放在滚动容器上，不放在片段块上：块被横向虚拟化卸载时捕获不会断；
 * - 只读或屏幕太窄时只能选中、定位播放头，不能拖。
 *
 * ⚠️ 只渲染可见范围前后各一屏的条目（`smartcutVisibleRange`）；200 个片段全量渲染时拖动掉帧。
 */
import { provide } from "vue";
import { ElMessage } from "element-plus";
import type { components } from "@tabtv/api-client";
import { useSmartcutPreviewAssets } from "~/composables/useSmartcutPreviewAssets";
import type { SmartcutEditResult } from "~/utils/smartcutContent";
import {
  smartcutInRange,
  smartcutInsertMarkMs,
  smartcutMoveClip,
  smartcutMoveSpan,
  smartcutPatchTrack,
  smartcutReorderClip,
  smartcutReorderTarget,
  smartcutSnapSpan,
  smartcutTrimClip,
  smartcutVisibleRange,
  type SmartcutTrackPatch
} from "~/utils/smartcutEdit";
import { smartcutTransitionActive } from "~/utils/smartcutLayout";
import { SMARTCUT_GENERATION_DRAG, smartcutFailedClipIds, smartcutLatestByClip } from "~/utils/smartcutClipGen";
import { smartcutRelayout } from "~/utils/smartcutModel";
import { smartcutMediaUrl } from "~/utils/smartcutPublish";
import { smartcutTrackLabel } from "~/utils/smartcutStudio";
import { smartcutFormatTimecode } from "~/utils/smartcutTimecode";
import { smartcutAnchorsOf, smartcutFormatTime, smartcutMsToPx, smartcutPxToMs, smartcutSnapMs, smartcutTicks } from "~/utils/smartcutTimeline";
import BeatMarkers from "./BeatMarkers.vue";
import TrackRow from "./TrackRow.vue";
import { smartcutTimelineMediaKey, type SmartcutLaneItem } from "./context";

type Timeline = components["schemas"]["Timeline"];
type TimelineTrack = components["schemas"]["TimelineTrack"];
type TimelineClip = components["schemas"]["TimelineClip"];
type Input = components["schemas"]["SmartcutInput"];

const props = defineProps<{ zoom: number; snap: boolean; editable: boolean; disabledReason: string }>();
const emit = defineEmits<{ "edit-transition": [clipId: string]; "place-generation": [generationId: string, ms: number] }>();
const store = useSmartcutStore();
const previews = useSmartcutPreviewAssets();
const media = useAuthenticatedMedia();

const HEAD_W = 168;
const ROW_H: Record<TimelineTrack["kind"], number> = { video: 66, audio: 54, subtitle: 40, text: 40 };
/** 时间线末尾留多少空白：要能把音频、字幕拖到成片之后一点。 */
const TAIL_MS = 10000;
const DRAG_THRESHOLD_PX = 4;

const scrollEl = ref<HTMLElement | null>(null);
const rulerEl = ref<HTMLElement | null>(null);
const scrollLeft = ref(0);
const viewport = ref(960);
const laneViewport = computed(() => Math.max(200, viewport.value - HEAD_W));

// ---------------------------------------------------------------- 素材信息

const inputByMedia = computed(() => {
  const map = new Map<string, Input>();
  for (const input of store.inputs) if (input.mediaId && !map.has(input.mediaId)) map.set(input.mediaId, input);
  return map;
});
const inputByNode = computed(() => {
  const map = new Map<string, Input>();
  for (const input of store.inputs) if (!map.has(input.sourceNodeId)) map.set(input.sourceNodeId, input);
  return map;
});
/** 素材条还没取回来时不判「来源已断开」：否则打开剪辑台的一瞬间所有片段都标成断开。 */
const inputsKnown = computed(() => Boolean(store.inputsFingerprint));
// 失败标记按每段最近一次片段级生成判：失败之后又成功了的不再标失败（SCT-40）
const failedClips = computed(() => smartcutFailedClipIds(store.generations));
const latestByClip = computed(() => smartcutLatestByClip(store.generations));

function mediaKindOf(clip: TimelineClip, trackKind: TimelineTrack["kind"]): "video" | "image" | "audio" {
  if (trackKind === "audio") return "audio";
  return inputByMedia.value.get(clip.mediaId)?.kind === "image" ? "image" : "video";
}

function sourceDurationOf(mediaId: string | undefined): number | undefined {
  if (!mediaId) return undefined;
  if (inputByMedia.value.get(mediaId)?.kind === "image") return undefined;
  return inputByMedia.value.get(mediaId)?.durationMs ?? previews.get(mediaId)?.preview?.durationMs ?? undefined;
}

// ---------------------------------------------------------------- 拖动

type Drag = {
  pointerId: number;
  startX: number;
  item: SmartcutLaneItem;
  track: TimelineTrack;
  part: "body" | "start" | "end";
  shift: boolean;
  moved: boolean;
  /** 画面轨排序时幽灵块跟着指针走的像素偏移。 */
  dx: number;
  insertPx: number | null;
  invalid: boolean;
  anchors: number[];
  /** 松手时对 store 里的最新内容执行的那一步（与拖动中预览用的是同一个纯函数）。 */
  apply: ((t: Timeline) => SmartcutEditResult<Timeline>) | null;
};
const drag = shallowRef<Drag | null>(null);
const dragPreview = shallowRef<Timeline | null>(null);
const scrubbing = ref(false);

const shown = computed<Timeline | null>(() => dragPreview.value ?? store.timeline);

// ---------------------------------------------------------------- 可见范围与条目

const range = computed(() => smartcutVisibleRange(scrollLeft.value, laneViewport.value, props.zoom));
const visibleFromPx = computed(() => smartcutMsToPx(range.value.fromMs, props.zoom));
const visibleToPx = computed(() => smartcutMsToPx(range.value.toMs, props.zoom));
const fps = computed(() => store.timeline?.fps ?? 30);

function clipItems(track: TimelineTrack): SmartcutLaneItem[] {
  return (track.clips ?? []).map((clip, i) => {
    const input = clip.sourceNodeId ? inputByNode.value.get(clip.sourceNodeId) : undefined;
    const label = input ? `#${input.no}${input.title ? ` ${input.title}` : ""}` : `片段 ${i + 1}`;
    const speed = clip.speed ?? 100;
    const transition = track.kind === "video" && i > 0 && smartcutTransitionActive(clip.transitionIn);
    const meta = `${smartcutFormatTimecode(clip.durationMs, fps.value)}${speed !== 100 ? ` · ${speed / 100}×` : ""}`;
    return {
      id: clip.id,
      kind: "clip" as const,
      left: smartcutMsToPx(clip.startMs, props.zoom),
      width: smartcutMsToPx(clip.durationMs, props.zoom),
      startMs: clip.startMs,
      durationMs: clip.durationMs,
      label,
      meta,
      title: `${label}（${meta}）`,
      mediaId: clip.mediaId,
      mediaKind: mediaKindOf(clip, track.kind),
      sourceInMs: clip.sourceInMs,
      sourceOutMs: clip.sourceOutMs,
      speed,
      overlapPx: transition ? smartcutMsToPx(clip.transitionIn!.durationMs, props.zoom) : 0,
      marks: {
        ai: clip.origin === "ai",
        disconnected: inputsKnown.value && Boolean(clip.sourceNodeId) && !inputByNode.value.has(clip.sourceNodeId!),
        generating: Boolean(clip.pendingGenerationId),
        failed: failedClips.value.has(clip.id),
        transition,
        // 跟随画面一段的音效（SC-F-56）：不能单独拖动；那一段被裁到看不见时隐藏（不删除）
        anchored: Boolean(clip.anchor),
        hidden: Boolean(clip.hidden)
      },
      ...(failedClips.value.has(clip.id) ? { failReason: latestByClip.value.get(clip.id)?.error || "生成失败（已退款）" } : {}),
      ...(clip.pendingGenerationId ? { progress: store.generations[clip.pendingGenerationId]?.progress ?? 0 } : {}),
      // 跟随画面的音效：取用区间跟着那一段算，不能单独裁切
      trimmable: !clip.anchor
    };
  });
}

function spanItems(t: Timeline, track: TimelineTrack, firstSubtitleTrack: string | undefined): SmartcutLaneItem[] {
  if (track.kind === "subtitle") {
    return (t.subtitles ?? [])
      .filter(sub => (sub.trackId ?? firstSubtitleTrack) === track.id)
      .map((sub) => ({
        id: sub.id,
        kind: "subtitle" as const,
        left: smartcutMsToPx(sub.startMs, props.zoom),
        width: smartcutMsToPx(sub.endMs - sub.startMs, props.zoom),
        startMs: sub.startMs,
        durationMs: sub.endMs - sub.startMs,
        label: sub.text,
        meta: "",
        title: `${sub.text}（${smartcutFormatTimecode(sub.startMs, fps.value)}）${sub.anchor ? " · 跟随片段" : ""}`,
        marks: { hidden: Boolean(sub.hidden), anchored: Boolean(sub.anchor) },
        trimmable: false
      }));
  }
  return (t.texts ?? [])
    .filter(text => text.trackId === track.id)
    .map((text) => ({
      id: text.id,
      kind: "text" as const,
      left: smartcutMsToPx(text.startMs, props.zoom),
      width: smartcutMsToPx(text.endMs - text.startMs, props.zoom),
      startMs: text.startMs,
      durationMs: text.endMs - text.startMs,
      label: text.text,
      meta: "",
      title: text.text,
      marks: {},
      trimmable: false
    }));
}

const rows = computed(() => {
  const t = shown.value;
  if (!t) return [];
  const ordinals: Record<string, number> = {};
  const firstSubtitleTrack = t.tracks.find(track => track.kind === "subtitle")?.id;
  return t.tracks.map((track) => {
    ordinals[track.kind] = (ordinals[track.kind] ?? 0) + 1;
    const all = track.kind === "video" || track.kind === "audio" ? clipItems(track) : spanItems(t, track, firstSubtitleTrack);
    return {
      track,
      label: smartcutTrackLabel(track, ordinals[track.kind]!),
      all,
      items: all.filter(item => smartcutInRange(item.startMs, item.startMs + item.durationMs, range.value)),
      height: ROW_H[track.kind]
    };
  });
});

const contentMs = computed(() => {
  const t = shown.value;
  if (!t) return TAIL_MS;
  let end = Math.max(t.effectiveMs, store.playheadMs);
  for (const row of rows.value) for (const item of row.all) end = Math.max(end, item.startMs + item.durationMs);
  return end + TAIL_MS;
});
const laneWidth = computed(() => Math.max(laneViewport.value, smartcutMsToPx(contentMs.value, props.zoom)));
const ticks = computed(() => smartcutTicks(contentMs.value, props.zoom).filter(ms => smartcutInRange(ms, ms, range.value)));
const playheadPx = computed(() => smartcutMsToPx(store.playheadMs, props.zoom));
const selectedIds = computed(() => new Set(store.selection?.ids ?? []));
/** roving tabindex：每条轨一个可聚焦的块（键盘移动过的、选中的、否则第一个）。 */
const focusOverride = ref<Record<string, string>>({});

function focusIdOf(row: { track: TimelineTrack; all: SmartcutLaneItem[] }): string {
  const override = focusOverride.value[row.track.id];
  if (override && row.all.some(item => item.id === override)) return override;
  return row.all.find(item => selectedIds.value.has(item.id))?.id ?? row.all[0]?.id ?? "";
}

/** 配乐的拍点（成片时间）：片段起点 + (拍点 − 取用起点) × 100 / 变速。 */
const beatMarks = computed(() => {
  const out: Array<{ ms: number; strong: boolean }> = [];
  let approx = false;
  for (const track of shown.value?.tracks ?? []) {
    if (track.kind !== "audio" || track.role !== "music" || track.isMuted) continue;
    for (const clip of track.clips ?? []) {
      const beats = previews.get(clip.mediaId)?.beats;
      if (!beats?.beatsMs?.length) continue;
      approx ||= Boolean(beats.downbeatApprox);
      const strong = new Set(beats.downbeatsMs ?? []);
      const speed = clip.speed && clip.speed > 0 ? clip.speed : 100;
      for (const ms of beats.beatsMs) {
        if (ms < clip.sourceInMs || ms >= clip.sourceOutMs) continue;
        out.push({ ms: clip.startMs + Math.trunc(((ms - clip.sourceInMs) * 100) / speed), strong: strong.has(ms) });
      }
    }
  }
  return { beats: out.sort((a, b) => a.ms - b.ms), approx };
});

// ---------------------------------------------------------------- 媒体：拼图与图片缩略图

const SHEET = "sheet:";
const IMAGE = "img:";
provide(smartcutTimelineMediaKey, {
  sheetUrl: (id: string) => media.urls.value[SHEET + id] ?? "",
  imageUrl: (id: string) => media.urls.value[IMAGE + id] ?? ""
});

watch(() => (store.timeline?.tracks ?? []).flatMap(track => (track.clips ?? []).map(clip => clip.mediaId)), (ids) => {
  void previews.ensure(ids);
}, { immediate: true });

// 也跟着预览素材变：片段换了素材（重拍 / 去字幕回填、补镜插入、从生成记录放进来，SCT-40）时，那份素材的条带图
// 是在 rows 算完之后才取回来的 —— 只跟 rows 的话新素材永远拿不到条带图（片段一直是空白），直到滚动或缩放。
// 预览素材是一份一份回来的，这里会连着触发好几次：正在取的不再发第二次（取失败的放回去，下次再试）。
const sheetsRequested = new Set<string>();
watch([rows, previews.assets], ([list]) => {
  const wanted: Array<{ key: string; url: string }> = [];
  for (const row of list) {
    for (const item of row.items) {
      if (item.kind !== "clip" || !item.mediaId) continue;
      if (item.mediaKind === "image") {
        wanted.push({ key: IMAGE + item.mediaId, url: `${smartcutMediaUrl(item.mediaId)}?w=160` });
        continue;
      }
      if (item.mediaKind !== "video") continue;
      for (const sheet of previews.get(item.mediaId)?.filmstrip?.sheets ?? []) wanted.push({ key: SHEET + sheet, url: smartcutMediaUrl(sheet) });
    }
  }
  const missing = wanted.filter(w => !media.urls.value[w.key] && !sheetsRequested.has(w.key));
  if (!missing.length) return;
  for (const w of missing) sheetsRequested.add(w.key);
  void media.appendAll(missing).finally(() => {
    for (const w of missing) if (!media.urls.value[w.key]) sheetsRequested.delete(w.key);
  });
});

// ---------------------------------------------------------------- 拖动处理

function allClips(t: Timeline): TimelineClip[] {
  return t.tracks.flatMap(track => track.clips ?? []);
}

/** 吸附锚点：所有片段的首尾（被拖的那段除外）、字幕与文字的首尾、播放头、配乐节拍。 */
function anchorsFor(exceptId: string): number[] {
  const t = store.timeline;
  if (!t) return [];
  const spans = [
    ...allClips(t).filter(clip => clip.id !== exceptId),
    ...(t.subtitles ?? []).filter(sub => sub.id !== exceptId && !sub.hidden).map(sub => ({ startMs: sub.startMs, durationMs: sub.endMs - sub.startMs })),
    ...(t.texts ?? []).filter(text => text.id !== exceptId).map(text => ({ startMs: text.startMs, durationMs: text.endMs - text.startMs }))
  ];
  return smartcutAnchorsOf(spans, store.playheadMs, beatMarks.value.beats.map(b => b.ms));
}

/** 预览用：直接把起点改掉（重叠也照样显示，标红），松手时再由纯函数判能不能放。 */
function withStart(t: Timeline, item: SmartcutLaneItem, startMs: number): Timeline {
  if (item.kind === "clip") {
    return { ...t, tracks: t.tracks.map(track => ({ ...track, clips: (track.clips ?? []).map(clip => (clip.id === item.id ? { ...clip, startMs } : clip)) })) };
  }
  if (item.kind === "subtitle") {
    return { ...t, subtitles: (t.subtitles ?? []).map(sub => (sub.id === item.id ? { ...sub, startMs, endMs: startMs + (sub.endMs - sub.startMs) } : sub)) };
  }
  return { ...t, texts: (t.texts ?? []).map(text => (text.id === item.id ? { ...text, startMs, endMs: startMs + (text.endMs - text.startMs) } : text)) };
}

function onItemPointerDown(event: PointerEvent, item: SmartcutLaneItem, part: "body" | "start" | "end", track: TimelineTrack) {
  if (event.button !== 0 || !scrollEl.value) return;
  scrollEl.value.setPointerCapture?.(event.pointerId);
  drag.value = {
    pointerId: event.pointerId, startX: event.clientX, item, track, part, shift: event.shiftKey || event.metaKey || event.ctrlKey,
    moved: false, dx: 0, insertPx: null, invalid: false, anchors: anchorsFor(item.id), apply: null
  };
}

function snapped(ms: number, durationMs: number, anchors: number[]): number {
  return props.snap ? smartcutSnapSpan(ms, durationMs, x => smartcutSnapMs(x, anchors, props.zoom)) : ms;
}

function onPointerMove(event: PointerEvent) {
  if (scrubbing.value) {
    seekToClientX(event.clientX);
    return;
  }
  const d = drag.value;
  const base = store.timeline;
  if (!d || event.pointerId !== d.pointerId || !base) return;
  const dxPx = event.clientX - d.startX;
  if (!d.moved && Math.abs(dxPx) < DRAG_THRESHOLD_PX) return;
  if (!props.editable) return;
  const deltaMs = smartcutPxToMs(dxPx, props.zoom);
  const { item, track, part } = d;
  const next: Drag = { ...d, moved: true };
  if (item.kind === "subtitle" && item.marks.anchored) {
    next.invalid = true;
    next.apply = () => ({ ok: false, reason: "这条字幕跟随片段，移动片段时它会一起走" });
    drag.value = next;
    return;
  }
  if (item.kind === "clip" && item.marks.anchored) {
    next.invalid = true;
    next.apply = () => ({ ok: false, reason: "这段音效跟随画面的一段：移动、裁切那一段时它会一起走" });
    drag.value = next;
    return;
  }
  if (part !== "body") {
    const srcDur = sourceDurationOf(item.mediaId);
    const apply = (t: Timeline) => smartcutTrimClip(t, store.currentId(item.id), part, deltaMs, srcDur);
    const result = apply(base);
    next.apply = apply;
    next.invalid = !result.ok;
    if (result.ok) dragPreview.value = smartcutRelayout(result.value);
  } else if (track.kind === "video") {
    const clips = (track.clips ?? []).map(clip => ({ id: clip.id, startMs: clip.startMs, durationMs: clip.durationMs }));
    const position = smartcutReorderTarget(clips, item.id, item.startMs + item.durationMs / 2 + deltaMs);
    next.dx = dxPx;
    next.insertPx = smartcutMsToPx(smartcutInsertMarkMs(clips, item.id, position), props.zoom);
    next.apply = t => ({ ok: true, value: smartcutReorderClip(t, store.currentId(item.id), position) });
  } else {
    const start = Math.max(0, snapped(item.startMs + deltaMs, item.durationMs, d.anchors));
    const apply = item.kind === "clip"
      ? (t: Timeline) => smartcutMoveClip(t, store.currentId(item.id), start)
      : (t: Timeline) => smartcutMoveSpan(t, item.kind === "subtitle" ? "subtitle" : "text", store.currentId(item.id), start);
    next.apply = apply;
    next.invalid = !apply(base).ok;
    dragPreview.value = smartcutRelayout(withStart(base, item, start));
  }
  drag.value = next;
}

function onPointerUp(event: PointerEvent) {
  if (scrubbing.value) {
    scrubbing.value = false;
    rulerEl.value?.releasePointerCapture?.(event.pointerId);
    return;
  }
  const d = drag.value;
  if (!d || event.pointerId !== d.pointerId) return;
  scrollEl.value?.releasePointerCapture?.(event.pointerId);
  drag.value = null;
  dragPreview.value = null;
  if (!d.moved) {
    select(d.item, d.shift);
    return;
  }
  if (!d.apply) return;
  let failure = "";
  store.commit((t) => {
    // 拖动开始时还是临时 ID 的片段可能在拖动中途被保存换成了正式 ID：apply 里一律按 store.currentId 取
    const result = d.apply!(t);
    if (!result.ok) {
      failure = result.reason;
      return t;
    }
    return result.value;
  });
  if (failure) ElMessage.warning(failure);
}

function select(raw: SmartcutLaneItem, additive: boolean) {
  const item = { ...raw, id: store.currentId(raw.id) };
  const current = store.selection;
  if (additive && current?.kind === item.kind) {
    const ids = current.ids.includes(item.id) ? current.ids.filter(id => id !== item.id) : [...current.ids, item.id];
    store.select(ids.length ? { kind: item.kind, ids } : null);
  } else {
    store.select({ kind: item.kind, ids: [item.id] });
  }
}

/** Esc：只取消当前的拖动 / 裁切（不关闭剪辑台）。返回有没有东西被取消。 */
function cancel(): boolean {
  if (scrubbing.value) {
    scrubbing.value = false;
    return true;
  }
  const d = drag.value;
  if (!d) return false;
  scrollEl.value?.releasePointerCapture?.(d.pointerId);
  drag.value = null;
  dragPreview.value = null;
  return true;
}

// ---------------------------------------------------------------- 播放头与刻度

function seekToClientX(clientX: number) {
  const rect = rulerEl.value?.getBoundingClientRect();
  if (!rect) return;
  store.setPlayhead(smartcutPxToMs(clientX - rect.left, props.zoom));
}

function onRulerDown(event: PointerEvent) {
  if (event.button !== 0) return;
  scrubbing.value = true;
  rulerEl.value?.setPointerCapture?.(event.pointerId);
  seekToClientX(event.clientX);
}

function onLaneDown(event: PointerEvent) {
  if (event.button !== 0) return;
  store.setPlayhead(smartcutPxToMs(event.offsetX, props.zoom));
  store.select(null);
}

// ---------------------------------------------------------------- 键盘（roving tabindex）

function onItemKeydown(event: KeyboardEvent, item: SmartcutLaneItem, row: { track: TimelineTrack; all: SmartcutLaneItem[] }) {
  if (event.key === "ArrowLeft" || event.key === "ArrowRight") {
    event.preventDefault();
    const sorted = [...row.all].sort((a, b) => a.left - b.left);
    const index = sorted.findIndex(x => x.id === item.id);
    const target = sorted[index + (event.key === "ArrowLeft" ? -1 : 1)];
    if (target) void focusItem(row.track.id, target);
    return;
  }
  if (event.key === "Enter" || event.key === " ") {
    event.preventDefault();
    select(item, event.shiftKey);
  }
}

async function focusItem(trackId: string, item: SmartcutLaneItem) {
  focusOverride.value = { ...focusOverride.value, [trackId]: item.id };
  const el = scrollEl.value;
  if (el && (item.left < el.scrollLeft || item.left > el.scrollLeft + laneViewport.value - 40)) {
    el.scrollLeft = Math.max(0, item.left - 40);
    scrollLeft.value = el.scrollLeft;
  }
  await nextTick();
  el?.querySelector<HTMLElement>(`[data-item-id="${CSS.escape(item.id)}"]`)?.focus();
}

/** 把焦点放到第一条轨可聚焦的块上（剪辑台打开时给键盘用户一个起点）。 */
function focusFirst(): boolean {
  const row = rows.value.find(r => r.all.length);
  if (!row) return false;
  const id = focusIdOf(row);
  const item = row.all.find(x => x.id === id);
  if (!item) return false;
  void focusItem(row.track.id, item);
  return true;
}

function patchTrack(trackId: string, patch: SmartcutTrackPatch) {
  store.commit(t => smartcutPatchTrack(t, trackId, patch));
}

// ---------------------------------------------------------------- 滚动与尺寸

let raf = 0;
function onScroll() {
  if (raf) return;
  raf = requestAnimationFrame(() => {
    raf = 0;
    scrollLeft.value = scrollEl.value?.scrollLeft ?? 0;
  });
}

let resizeObserver: ResizeObserver | null = null;
onMounted(() => {
  if (scrollEl.value) {
    viewport.value = scrollEl.value.clientWidth || viewport.value;
    resizeObserver = new ResizeObserver(() => {
      viewport.value = scrollEl.value?.clientWidth || viewport.value;
    });
    resizeObserver.observe(scrollEl.value);
  }
});
onBeforeUnmount(() => {
  resizeObserver?.disconnect();
  if (raf) cancelAnimationFrame(raf);
});

/** 播放时播放头跑出可见范围就跟过去（翻一屏，不是逐帧滚动）。 */
watch(() => store.playheadMs, () => {
  const el = scrollEl.value;
  if (!el || !store.playing) return;
  const px = playheadPx.value;
  if (px < el.scrollLeft || px > el.scrollLeft + laneViewport.value - 24) {
    el.scrollLeft = Math.max(0, px - 40);
    scrollLeft.value = el.scrollLeft;
  }
});

/** 让成片时间 `ms` 落在可见范围里（导出面板「第 N 段还在生成」的定位）。 */
function reveal(ms: number) {
  const el = scrollEl.value;
  if (!el) return;
  const px = smartcutMsToPx(ms, props.zoom);
  if (px < el.scrollLeft || px > el.scrollLeft + laneViewport.value - 24) {
    el.scrollLeft = Math.max(0, px - 40);
    scrollLeft.value = el.scrollLeft;
  }
}

// ---------------------------------------------------------------- 生成记录拖进来（SCT-40）

const dropActive = ref(false);
function acceptsGeneration(event: DragEvent): boolean {
  return Boolean(props.editable && event.dataTransfer?.types.includes(SMARTCUT_GENERATION_DRAG));
}
function onDragOver(event: DragEvent) {
  if (!acceptsGeneration(event)) return;
  event.preventDefault();
  event.dataTransfer!.dropEffect = "copy";
  dropActive.value = true;
}
function onDrop(event: DragEvent) {
  dropActive.value = false;
  if (!acceptsGeneration(event)) return;
  event.preventDefault();
  const id = event.dataTransfer!.getData(SMARTCUT_GENERATION_DRAG);
  const rect = rulerEl.value?.getBoundingClientRect();
  if (!id || !rect) return;
  emit("place-generation", id, Math.max(0, smartcutPxToMs(event.clientX - rect.left, props.zoom)));
}

defineExpose({ cancel, focusFirst, laneViewport, reveal });
</script>

<template>
  <section
    ref="scrollEl"
    class="sc-timeline"
    aria-label="时间线"
    data-testid="smartcut-timeline"
    :class="{ 'drop-active': dropActive }"
    @scroll="onScroll"
    @pointermove="onPointerMove"
    @pointerup="onPointerUp"
    @pointercancel="cancel"
    @dragover="onDragOver"
    @dragleave="dropActive = false"
    @drop="onDrop"
  >
    <div class="sc-timeline-content" :style="{ width: `${HEAD_W + laneWidth}px` }">
      <div class="sc-timeline-ruler-row">
        <div class="sc-timeline-corner" :style="{ width: `${HEAD_W}px` }">
          <span class="sc-tag" :title="`吸附${snap ? '开' : '关'}`">{{ smartcutFormatTime(store.timeline?.effectiveMs ?? 0) }}</span>
        </div>
        <div
          ref="rulerEl"
          class="sc-timeline-ruler"
          :style="{ width: `${laneWidth}px` }"
          role="slider"
          tabindex="-1"
          aria-label="刻度尺：点击或拖动定位播放头"
          :aria-valuemin="0"
          :aria-valuemax="store.timeline?.effectiveMs ?? 0"
          :aria-valuenow="store.playheadMs"
          @pointerdown="onRulerDown"
        >
          <span v-for="ms in ticks" :key="ms" class="sc-tick" :style="{ left: `${smartcutMsToPx(ms, zoom)}px` }">{{ smartcutFormatTime(ms) }}</span>
          <BeatMarkers v-if="beatMarks.beats.length" :beats="beatMarks.beats" :zoom="zoom" :from-ms="range.fromMs" :to-ms="range.toMs" :approx="beatMarks.approx" />
        </div>
      </div>
      <TrackRow
        v-for="row in rows"
        :key="row.track.id"
        :track="row.track"
        :label="row.label"
        :items="row.items"
        :selected-ids="selectedIds"
        :focus-id="focusIdOf(row)"
        :drag-id="drag?.moved ? drag.item.id : ''"
        :drag-dx="drag?.moved && drag.track.id === row.track.id ? drag.dx : 0"
        :invalid-id="drag?.moved && drag.invalid ? drag.item.id : ''"
        :insert-px="drag?.moved && drag.track.id === row.track.id ? drag.insertPx : null"
        :lane-width="laneWidth"
        :visible-from-px="visibleFromPx"
        :visible-to-px="visibleToPx"
        :editable="editable"
        :disabled-reason="disabledReason"
        :head-width="HEAD_W"
        :height="row.height"
        @item-pointerdown="(event, item, part) => onItemPointerDown(event, item, part, row.track)"
        @item-keydown="(event, item) => onItemKeydown(event, item, row)"
        @lane-pointerdown="onLaneDown"
        @patch="patch => patchTrack(row.track.id, patch)"
        @seam="clipId => emit('edit-transition', clipId)"
      />
      <div class="sc-playhead" :style="{ left: `${HEAD_W + playheadPx}px` }" aria-hidden="true" data-testid="smartcut-playhead" />
    </div>
  </section>
</template>

<style scoped>
.sc-timeline {
  position: relative;
  min-height: 0;
  overflow: auto;
  color: var(--portal-text);
  background: var(--portal-bg);
  overscroll-behavior: contain;
}
.sc-timeline-content { position: relative; min-height: 100%; }
.sc-timeline.drop-active { box-shadow: inset 0 0 0 2px var(--portal-cyan); }
.sc-timeline-ruler-row { position: sticky; top: 0; z-index: 8; display: flex; height: 28px; border-bottom: 1px solid var(--portal-line); background: var(--portal-panel); }
.sc-timeline-corner { position: sticky; left: 0; z-index: 9; display: flex; flex-shrink: 0; align-items: center; padding: 0 8px; border-right: 1px solid var(--portal-line); background: var(--portal-panel); }
.sc-timeline-ruler { position: relative; flex-shrink: 0; cursor: pointer; touch-action: none; }
.sc-tick { position: absolute; top: 4px; padding-left: 3px; border-left: 1px solid var(--portal-line-strong); color: var(--portal-muted); font-size: 10px; font-variant-numeric: tabular-nums; line-height: 1; pointer-events: none; }
.sc-playhead { position: absolute; top: 0; bottom: 0; z-index: 7; width: 2px; margin-left: -1px; background: var(--portal-danger); pointer-events: none; }
.sc-playhead::before { content: ""; position: absolute; top: 0; left: -5px; border: 6px solid transparent; border-top-color: var(--portal-danger); }
</style>
