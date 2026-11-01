<script setup lang="ts">
/**
 * 剪辑台的预览窗（SCT-21，dev/03 §6，SCD-42 / SCD-46）：按时间线真正连续播放（画面、声音、文字），只加载低清预览版。
 *
 * - 引擎（`smartcut/engine/PlaybackEngine`）只管媒体调度，这里负责：预览框按画幅等比缩放、播放头与 store 双向同步、
 *   文字与字幕的叠加层（服务端排版 PNG，输入时先用 DOM 临时显示）、缺预览版时的占位、画面变换控制点、全屏；
 * - 媒体一律 fetch 成 blob（受保护端点不认 `<img src>` 的直链）：预览版走 `GET /media/{预览版}`，图片取 640 宽缩略图，
 *   音频轨取音频文件本身；预览版还没生成好时 loader 回 null，引擎显示黑底并发「缺素材」，这里盖一层「预览版生成中」；
 * - 预览是近似效果（转场一律按淡入淡出、不做人声闪避与响度归一），常驻一句说明（SC-F-32）。
 *
 * ⚠️ 播放必须由用户手势触发（`toggle` 由按钮 / 空格调用）：`AudioContext` 在第一次点击时建 / resume（SCR-4）。
 * ⚠️ 卸载时释放引擎（object URL、媒体元素、AudioContext）；整页只有一个在播的引擎（节点卡与剪辑台不能同时出声）。
 */
import type { components } from "@tabtv/api-client";
import { PlaybackEngine, type PlaybackMissing } from "~/smartcut/engine/PlaybackEngine";
import { stackSubtitleShifts, subtitleBottomPct, type ScheduleMediaInfo, type ScheduleOverlayItem } from "~/smartcut/engine/Scheduler";
import { useSmartcutPreviewAssets } from "~/composables/useSmartcutPreviewAssets";
import { useSmartcutTypeset } from "~/composables/useSmartcutTypeset";
import { smartcutFindClip, smartcutSetClipTransform } from "~/utils/smartcutEdit";
import { smartcutMediaUrl } from "~/utils/smartcutPublish";
import { smartcutPreviewBox } from "~/utils/smartcutTimeline";
import { smartcutTypesetFallbackStyle } from "~/utils/smartcutTypeset";
import TransformHandles from "./TransformHandles.vue";

type Timeline = components["schemas"]["Timeline"];
type ClipTransform = components["schemas"]["ClipTransform"];

const props = defineProps<{ editable: boolean }>();
const store = useSmartcutStore();
const previews = useSmartcutPreviewAssets();
const media = useAuthenticatedMedia();
const typeset = useSmartcutTypeset();

const hostEl = ref<HTMLElement | null>(null);
const stageEl = ref<HTMLElement | null>(null);
const host = ref({ width: 640, height: 360 });
const missing = ref<PlaybackMissing[]>([]);
const overlays = shallowRef<readonly ScheduleOverlayItem[]>([]);
const waiting = ref(false);
const fullscreen = ref(false);
/** 拖动变换控制点时的预览（松手之前不进 store）。 */
const transformDraft = shallowRef<{ clipId: string; transform: ClipTransform } | null>(null);
const handlesRef = ref<InstanceType<typeof TransformHandles> | null>(null);
let engine: PlaybackEngine | null = null;
/** 引擎自己推进播放头时为 true：那一次 store 的变化不要反过来 seek 引擎。 */
let syncing = false;

const frame = computed(() => ({ width: store.timeline?.width ?? 1920, height: store.timeline?.height ?? 1080 }));
const box = computed(() => smartcutPreviewBox(frame.value.width, frame.value.height, host.value));

// ---------------------------------------------------------------- 素材

const inputByMedia = computed(() => {
  const map = new Map<string, components["schemas"]["SmartcutInput"]>();
  for (const input of store.inputs) if (input.mediaId && !map.has(input.mediaId)) map.set(input.mediaId, input);
  return map;
});

function mediaInfo(mediaId: string): ScheduleMediaInfo | undefined {
  const input = inputByMedia.value.get(mediaId);
  const assets = previews.get(mediaId);
  if (!input && !assets) return undefined;
  const kind = input?.kind === "image" ? "image" : input?.kind === "audio" ? "audio" : input ? "video" : undefined;
  return {
    ...(kind ? { kind } : {}),
    ...(assets?.peaks?.noAudio ? { hasAudio: false } : input?.hasAudio !== undefined ? { hasAudio: input.hasAudio } : {}),
    ...(input?.durationMs ?? assets?.preview?.durationMs ? { durationMs: input?.durationMs ?? assets?.preview?.durationMs } : {}),
    ...(assets?.preview?.width && assets.preview.height ? { width: assets.preview.width, height: assets.preview.height } : {})
  };
}

async function loadMedia(mediaId: string, purpose: "video" | "audio" | "image"): Promise<Blob | null> {
  if (purpose === "image") return media.blob(`${smartcutMediaUrl(mediaId)}?w=640`);
  if (purpose === "audio") return media.blob(smartcutMediaUrl(mediaId));
  let assets = previews.get(mediaId);
  if (!assets?.preview?.mediaId) assets = (await previews.refresh(mediaId)) ?? undefined;
  const proxy = assets?.preview?.mediaId;
  // 预览版还没生成好：不取原片（原片可能几百 MB，SCK-10），引擎显示占位，生成好之后由下面的 watch 通知重试。
  if (!proxy) return null;
  return media.blob(smartcutMediaUrl(proxy));
}

/** 缺预览版时的封面帧（素材条里的海报），没有就只显示文字。 */
const posterOf = (mediaId: string) => inputByMedia.value.get(mediaId)?.posterMediaId ?? "";
const missingView = computed(() => {
  const first = missing.value[0];
  if (!first) return null;
  const notReady = missing.value.some(item => item.reason === "not_ready");
  return {
    text: notReady ? "预览版生成中，生成好之后自动播放" : first.reason === "error" ? "这一段的素材取不到" : "加载中…",
    poster: posterOf(first.mediaId)
  };
});
watch(() => missingView.value?.poster, (id) => {
  if (id && !media.urls.value[`poster:${id}`]) void media.appendAll([{ key: `poster:${id}`, url: `${smartcutMediaUrl(id)}?w=640` }]);
});

// ---------------------------------------------------------------- 引擎

function shownTimeline(): Timeline | null {
  const t = store.timeline;
  const draft = transformDraft.value;
  return t && draft ? smartcutSetClipTransform(t, draft.clipId, draft.transform) : t;
}

onMounted(() => {
  if (!stageEl.value) return;
  engine = new PlaybackEngine({ stage: stageEl.value, loadMedia, mediaInfo });
  engine.on("timeupdate", ({ ms }) => {
    if (!engine?.playing) return;
    syncing = true;
    store.setPlayhead(ms);
    syncing = false;
  });
  engine.on("state", ({ playing, waiting: w }) => {
    store.playing = playing;
    waiting.value = w;
  });
  engine.on("ended", ({ ms }) => {
    store.playing = false;
    syncing = true;
    store.setPlayhead(ms);
    syncing = false;
  });
  engine.on("preempted", () => { store.playing = false; });
  engine.on("missing", ({ items }) => { missing.value = items; });
  engine.on("overlay", ({ items }) => { overlays.value = items; });
  engine.setTimeline(shownTimeline());
  engine.seek(store.playheadMs);
  observeHost();
});

onBeforeUnmount(() => {
  resizeObserver?.disconnect();
  typeset.cancelAll();
  engine?.dispose();
  engine = null;
  store.playing = false;
  if (document.fullscreenElement && document.fullscreenElement === hostEl.value) void document.exitFullscreen().catch(() => undefined);
});

watch(() => store.timeline, () => engine?.setTimeline(shownTimeline()));
watch(transformDraft, () => engine?.setTimeline(shownTimeline()));
watch(() => store.playheadMs, (ms) => {
  if (!engine || syncing) return;
  if (!engine.playing || Math.abs(ms - engine.currentMs) > 120) engine.seek(ms);
});
/** 预览版生成好了（轮询或分析完成事件刷新了预览素材）：让引擎重试那几段。 */
watch(() => (store.timeline?.tracks ?? []).flatMap(t => (t.clips ?? []).map(c => `${c.mediaId}:${previews.get(c.mediaId)?.preview?.mediaId ?? ""}:${previews.get(c.mediaId)?.peaks?.noAudio ? 1 : 0}`)).join("|"), () => {
  engine?.refreshMedia(undefined, mediaInfo);
});

function toggle() {
  if (!engine) return;
  if (engine.playing) engine.pause();
  else engine.play();
}
function pause() {
  engine?.pause();
}

// ---------------------------------------------------------------- 文字与字幕（两段式）

type OverlayView = { key: string; kind: "subtitle" | "text"; img?: { url: string; style: Record<string, string> }; fallback?: { text: string; style: Record<string, string> } };

const overlayViews = computed<OverlayView[]>(() => {
  const W = frame.value.width;
  const H = frame.value.height;
  const items = overlays.value;
  const entries = items.map(item => typeset.entry({ kind: item.kind, text: item.text, style: item.style, box: item.box }, W, H));
  // 字幕叠放（与服务端同一算法）：每条的高度取排版结果；还没排好的那条按 0 处理，排好之后再挪到位。
  const subIndexes = items.map((item, i) => (item.kind === "subtitle" ? i : -1)).filter(i => i >= 0);
  const shifts = stackSubtitleShifts(
    subIndexes.map(i => subtitleBottomPct(items[i]!.style)),
    subIndexes.map(i => (entries[i] ? (entries[i]!.box.h / H) * 100 : 0))
  );
  const shiftOf = new Map(subIndexes.map((index, k) => [index, shifts[k] ?? 0]));
  return items.map((item, i) => {
    const entry = entries[i];
    const up = shiftOf.get(i) ?? 0;
    if (entry) {
      return {
        key: item.key,
        kind: item.kind,
        img: {
          url: entry.url,
          style: {
            left: `${(entry.box.x / W) * 100}%`,
            top: `${(entry.box.y / H) * 100 - up}%`,
            width: `${(entry.box.w / W) * 100}%`
          }
        }
      };
    }
    return { key: item.key, kind: item.kind, fallback: { text: item.text, style: smartcutTypesetFallbackStyle({ kind: item.kind, text: item.text, style: item.style, box: item.box }, box.value.height) } };
  });
});

/** 停手 400ms 再排版：同一条（key）里新的输入顶掉旧的。 */
watch(overlays, (items) => {
  const W = frame.value.width;
  const H = frame.value.height;
  for (const item of items) typeset.schedule(item.key, { kind: item.kind, text: item.text, style: item.style, box: item.box }, W, H);
});

// ---------------------------------------------------------------- 画面变换控制点

const transformTarget = computed(() => {
  const sel = store.selection;
  const t = store.timeline;
  if (!props.editable || !t || sel?.kind !== "clip" || sel.ids.length !== 1) return null;
  const ref = smartcutFindClip(t, sel.ids[0]!);
  if (!ref || ref.track.kind !== "video") return null;
  const at = store.playheadMs;
  if (at < ref.clip.startMs || at >= ref.clip.startMs + ref.clip.durationMs) return null;
  const info = mediaInfo(ref.clip.mediaId);
  return {
    clipId: ref.clip.id,
    label: `第 ${ref.index + 1} 段`,
    transform: transformDraft.value?.clipId === ref.clip.id ? transformDraft.value.transform : ref.clip.transform ?? null,
    media: info?.width && info.height ? { width: info.width, height: info.height } : null
  };
});

function previewTransform(transform: ClipTransform) {
  const target = transformTarget.value;
  if (target) transformDraft.value = { clipId: target.clipId, transform };
}

function commitTransform(transform: ClipTransform) {
  const target = transformTarget.value ?? (transformDraft.value ? { clipId: transformDraft.value.clipId } : null);
  transformDraft.value = null;
  if (target) store.commit(t => smartcutSetClipTransform(t, store.currentId(target.clipId), transform));
}

function cancelTransform() {
  transformDraft.value = null;
}

/** Esc：取消正在拖的控制点。返回有没有东西被取消。 */
function cancel(): boolean {
  return handlesRef.value?.cancel() ?? false;
}

// ---------------------------------------------------------------- 尺寸与全屏

let resizeObserver: ResizeObserver | null = null;
function observeHost() {
  const el = hostEl.value;
  if (!el) return;
  const measure = () => {
    host.value = { width: Math.max(1, el.clientWidth - 16), height: Math.max(1, el.clientHeight - 40) };
  };
  measure();
  resizeObserver = new ResizeObserver(measure);
  resizeObserver.observe(el);
}

async function toggleFullscreen() {
  try {
    if (document.fullscreenElement) await document.exitFullscreen();
    else await hostEl.value?.requestFullscreen();
  } catch {
    // 浏览器不允许全屏（嵌在受限的 iframe 里）时什么都不做。
  }
}
function onFullscreenChange() {
  fullscreen.value = Boolean(document.fullscreenElement && document.fullscreenElement === hostEl.value);
}
onMounted(() => document.addEventListener("fullscreenchange", onFullscreenChange));
onBeforeUnmount(() => document.removeEventListener("fullscreenchange", onFullscreenChange));

defineExpose({ toggle, pause, cancel });
</script>

<template>
  <div ref="hostEl" class="sc-preview" :class="{ fullscreen }" data-testid="smartcut-preview">
    <div class="sc-preview-box" :style="{ width: `${box.width}px`, height: `${box.height}px` }">
      <div ref="stageEl" class="sc-preview-stage" />
      <div class="sc-preview-overlays" aria-hidden="true">
        <template v-for="view in overlayViews" :key="view.key">
          <img v-if="view.img" class="sc-preview-overlay" :src="view.img.url" :style="view.img.style" alt="" :data-overlay-kind="view.kind">
          <div v-else-if="view.fallback" class="sc-preview-fallback" :style="view.fallback.style" :data-overlay-kind="view.kind">{{ view.fallback.text }}</div>
        </template>
      </div>
      <div v-if="missingView" class="sc-preview-missing" role="status">
        <img v-if="missingView.poster && media.urls.value[`poster:${missingView.poster}`]" :src="media.urls.value[`poster:${missingView.poster}`]" alt="">
        <span>{{ missingView.text }}</span>
      </div>
      <div v-else-if="waiting" class="sc-preview-missing subtle" role="status"><span>加载中…</span></div>
      <TransformHandles
        v-if="transformTarget"
        ref="handlesRef"
        :transform="transformTarget.transform"
        :canvas="frame"
        :media="transformTarget.media"
        :stage="box"
        :label="transformTarget.label"
        @preview="previewTransform"
        @commit="commitTransform"
        @cancel="cancelTransform"
      />
    </div>
    <div class="sc-preview-foot">
      <span class="sc-note">预览为近似效果（转场、混音以导出为准）</span>
      <button type="button" class="sc-btn icon" :title="fullscreen ? '退出全屏' : '全屏预览'" :aria-label="fullscreen ? '退出全屏' : '全屏预览'" @click="toggleFullscreen">⛶</button>
    </div>
  </div>
</template>

<style scoped>
.sc-preview { position: relative; display: flex; flex-direction: column; align-items: center; justify-content: center; gap: 6px; min-width: 0; min-height: 0; padding: 8px; background: var(--portal-bg-deep); }
.sc-preview.fullscreen { background: var(--portal-media-letterbox); }
/* 成片里没有画面的地方就是黑：这块黑是内容本身，不跟主题走（同导演台视口）。 */
.sc-preview-box { position: relative; overflow: hidden; background: #000; }
.sc-preview-stage { position: absolute; inset: 0; }
.sc-preview-overlays { position: absolute; inset: 0; z-index: 2; pointer-events: none; }
.sc-preview-overlay { position: absolute; height: auto; }
.sc-preview-fallback { opacity: 0.85; }
.sc-preview-missing { position: absolute; inset: 0; z-index: 3; display: grid; place-items: center; color: var(--portal-on-media); background: var(--portal-scrim); font-size: 13px; }
.sc-preview-missing.subtle { place-items: end center; padding-bottom: 12px; background: transparent; }
.sc-preview-missing img { position: absolute; inset: 0; width: 100%; height: 100%; object-fit: contain; opacity: 0.6; }
.sc-preview-missing span { position: relative; padding: 4px 10px; border-radius: 999px; background: var(--portal-on-media-bg); }
.sc-preview-foot { display: flex; align-items: center; gap: 8px; }
</style>
