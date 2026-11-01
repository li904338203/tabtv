<script setup lang="ts">
/**
 * 轨道上的一块（SCT-20）：片段（缩略图条或波形）、字幕条目、文字层。
 *
 * - 缩略图条来自预览素材的拼图（第 i 帧 → 第 i/100 张、行 (i%100)/cols、列 (i%100)%cols），按块内可见范围只画看得见的那几格；
 * - 波形来自峰值（每 `intervalMs` 一个 0~255），每 2 像素一根竖线；没有音轨的素材画一条平线；
 * - 标记：转场、AI、来源已断开、生成中（底边一条进度条，SCT-40）、失败、已隐藏（跟随的片段把它裁掉了）；
 * - 两端裁切把手只在片段上出现；拖动由 TimelineView 统一处理（指针捕获在滚动容器上，块被虚拟化卸载也不断）。
 *
 * ⚠️ roving tabindex（SCD-53）：只有一块可聚焦（tabindex=0），←→ 在同一条轨的块之间移动，Enter / 空格选中。
 */
import { inject } from "vue";
import { smartcutPeaksOf, useSmartcutPreviewAssets } from "~/composables/useSmartcutPreviewAssets";
import { smartcutTimelineMediaKey, type SmartcutLaneItem } from "./context";

const props = defineProps<{
  item: SmartcutLaneItem;
  selected: boolean;
  focusable: boolean;
  dragging: boolean;
  invalid: boolean;
  editable: boolean;
  /** 块所在轨道的可见范围（相对轨道左边缘的像素），块内只画落在里面的缩略图与波形。 */
  visibleFromPx: number;
  visibleToPx: number;
  height: number;
}>();
const emit = defineEmits<{
  "item-pointerdown": [event: PointerEvent, part: "body" | "start" | "end"];
  "item-keydown": [event: KeyboardEvent];
}>();

const media = inject(smartcutTimelineMediaKey, { sheetUrl: () => "", imageUrl: () => "" });
const previews = useSmartcutPreviewAssets();
const assets = computed(() => previews.get(props.item.mediaId));

const STRIP_H = computed(() => Math.max(18, props.height - 20));
const MAX_THUMBS = 400;

/** 块内可见的像素区间（相对块左边缘）。 */
const span = computed(() => ({
  from: Math.max(0, props.visibleFromPx - props.item.left),
  to: Math.min(props.item.width, props.visibleToPx - props.item.left)
}));

const thumbs = computed(() => {
  const it = props.item;
  if (it.kind !== "clip" || it.mediaKind !== "video") return [];
  const fs = assets.value?.filmstrip;
  const sheets = fs?.sheets ?? [];
  if (!fs || !sheets.length || !fs.frameW || !fs.frameH || !fs.count || !fs.cols) return [];
  const h = STRIP_H.value;
  const w = Math.max(16, Math.round((fs.frameW * h) / fs.frameH));
  const srcIn = it.sourceInMs ?? 0;
  const srcLen = (it.sourceOutMs ?? 0) - srcIn;
  if (srcLen <= 0 || it.width <= 0) return [];
  const interval = fs.intervalMs || 1000;
  const out: Array<{ x: number; w: number; url: string; pos: string; size: string }> = [];
  const first = Math.max(0, Math.floor(span.value.from / w));
  const last = Math.min(Math.ceil(it.width / w), Math.ceil(span.value.to / w));
  for (let k = first; k < last && out.length < MAX_THUMBS; k++) {
    const x = k * w;
    const t = srcIn + ((x + w / 2) / it.width) * srcLen;
    const index = Math.max(0, Math.min(fs.count - 1, Math.floor(t / interval)));
    const sheet = sheets[Math.floor(index / 100)];
    const url = sheet ? media.sheetUrl(sheet) : "";
    if (!url) continue;
    const within = index % 100;
    const col = within % fs.cols;
    const row = Math.floor(within / fs.cols);
    out.push({ x, w, url, pos: `${-col * w}px ${-row * h}px`, size: `${fs.cols * w}px auto` });
  }
  return out;
});

const imageThumb = computed(() => (props.item.kind === "clip" && props.item.mediaKind === "image" && props.item.mediaId ? media.imageUrl(props.item.mediaId) : ""));

const wave = computed(() => {
  const it = props.item;
  if (it.kind !== "clip" || it.mediaKind !== "audio") return null;
  const peaks = assets.value?.peaks;
  if (!peaks) return null;
  const h = STRIP_H.value;
  const mid = h / 2;
  if (peaks.noAudio || !peaks.values) return { d: `M0 ${mid}H${it.width}`, flat: true };
  const values = smartcutPeaksOf(it.mediaId ?? "", peaks.values);
  const srcIn = it.sourceInMs ?? 0;
  const srcLen = (it.sourceOutMs ?? 0) - srcIn;
  const interval = peaks.intervalMs || 50;
  if (!values.length || srcLen <= 0) return null;
  let d = "";
  for (let x = Math.floor(span.value.from / 2) * 2; x <= span.value.to; x += 2) {
    const t = srcIn + (x / it.width) * srcLen;
    const v = values[Math.min(values.length - 1, Math.max(0, Math.floor(t / interval)))]! / 255;
    const half = Math.max(0.5, v * (mid - 1));
    d += `M${x} ${(mid - half).toFixed(1)}V${(mid + half).toFixed(1)}`;
  }
  return { d, flat: false };
});

const ariaLabel = computed(() => {
  const it = props.item;
  const what = it.kind === "clip" ? "片段" : it.kind === "subtitle" ? "字幕" : "文字";
  const flags = [
    it.marks.ai ? "AI 放入" : "",
    it.marks.disconnected ? "来源已断开" : "",
    it.marks.generating ? `生成中${it.progress ? ` ${it.progress}%` : ""}` : "",
    it.marks.failed ? `生成失败${it.failReason ? `：${it.failReason}` : ""}` : "",
    it.kind === "clip" && it.marks.anchored ? "跟随画面" : "",
    it.marks.hidden ? "已隐藏" : ""
  ].filter(Boolean).join("，");
  return `${what} ${it.label}${it.meta ? `，${it.meta}` : ""}${flags ? `，${flags}` : ""}${props.selected ? "，已选中" : ""}`;
});

function down(event: PointerEvent, part: "body" | "start" | "end") {
  emit("item-pointerdown", event, part);
}
</script>

<template>
  <div
    class="sc-clip"
    :class="[`kind-${item.kind}`, `media-${item.mediaKind ?? 'none'}`, { selected, dragging, invalid, hidden: item.marks.hidden }]"
    :style="{ left: `${item.left}px`, width: `${Math.max(2, item.width)}px`, height: `${height}px` }"
    role="button"
    :tabindex="focusable ? 0 : -1"
    :aria-pressed="selected"
    :aria-label="ariaLabel"
    :title="item.title"
    data-clip-block
    :data-item-id="item.id"
    :data-item-kind="item.kind"
    @pointerdown="down($event, 'body')"
    @keydown="emit('item-keydown', $event)"
  >
    <div v-if="thumbs.length" class="sc-clip-strip" :style="{ height: `${STRIP_H}px` }" aria-hidden="true">
      <span
        v-for="thumb in thumbs"
        :key="thumb.x"
        class="sc-clip-thumb"
        :style="{ left: `${thumb.x}px`, width: `${thumb.w}px`, backgroundImage: `url(${thumb.url})`, backgroundPosition: thumb.pos, backgroundSize: thumb.size }"
      />
    </div>
    <div v-else-if="imageThumb" class="sc-clip-strip image" :style="{ height: `${STRIP_H}px`, backgroundImage: `url(${imageThumb})` }" aria-hidden="true" />
    <svg v-else-if="wave" class="sc-clip-wave" :class="{ flat: wave.flat }" :height="STRIP_H" :width="item.width" aria-hidden="true">
      <path :d="wave.d" />
    </svg>
    <div class="sc-clip-label">
      <span v-if="item.marks.transition" class="sc-clip-mark" title="与上一段之间有转场">⇄</span>
      <span v-if="item.marks.ai" class="sc-clip-mark ai" title="AI 放入的片段">AI</span>
      <span v-if="item.marks.disconnected" class="sc-clip-mark warn" title="来源节点的连线已断开（片段保留）">断开</span>
      <span v-if="item.marks.generating" class="sc-clip-mark busy" title="片段生成中，完成后自动换上新结果">生成中{{ item.progress ? ` ${item.progress}%` : "" }}</span>
      <span v-if="item.marks.failed" class="sc-clip-mark warn" :title="`生成失败：${item.failReason ?? ''}（选中这一段可以重试）`">失败</span>
      <span v-if="item.kind === 'clip' && item.marks.anchored" class="sc-clip-mark" title="跟随画面的一段：移动、裁切那一段时它会一起走">跟随</span>
      <span v-if="item.marks.hidden" class="sc-clip-mark" title="跟随的片段把它所在的区间裁掉了（隐藏，不删除）">已隐藏</span>
      <span class="sc-clip-text">{{ item.label }}</span>
      <span v-if="item.meta" class="sc-clip-meta">{{ item.meta }}</span>
    </div>
    <div
      v-if="item.marks.generating" class="sc-clip-progress" :class="{ unknown: !item.progress }" role="progressbar"
      aria-label="生成进度" aria-valuemin="0" aria-valuemax="100" :aria-valuenow="item.progress || undefined"
    >
      <span :style="{ width: `${Math.min(100, Math.max(0, item.progress ?? 0))}%` }" />
    </div>
    <template v-if="item.trimmable && editable">
      <div class="sc-clip-handle start" title="拖动裁切开头" aria-hidden="true" @pointerdown.stop="down($event, 'start')" />
      <div class="sc-clip-handle end" title="拖动裁切结尾" aria-hidden="true" @pointerdown.stop="down($event, 'end')" />
    </template>
  </div>
</template>

<style scoped>
.sc-clip {
  position: absolute;
  top: 0;
  box-sizing: border-box;
  overflow: hidden;
  border: 1px solid var(--portal-line-strong);
  border-radius: 5px;
  color: var(--portal-text);
  background: var(--portal-surface-2);
  cursor: grab;
  touch-action: none;
  user-select: none;
}
.sc-clip.kind-clip.media-audio { background: var(--portal-green-soft); border-color: var(--portal-green-line); }
.sc-clip.kind-subtitle { background: var(--portal-cyan-soft); border-color: var(--portal-cyan-line); }
.sc-clip.kind-text { background: var(--portal-gold-soft); border-color: var(--portal-gold-line); }
.sc-clip.selected { border-color: var(--portal-cyan); box-shadow: inset 0 0 0 1px var(--portal-cyan); }
.sc-clip.dragging { z-index: 5; opacity: 0.85; cursor: grabbing; box-shadow: 0 4px 14px var(--portal-shadow); }
.sc-clip.invalid { border-color: var(--portal-danger); box-shadow: inset 0 0 0 1px var(--portal-danger); }
.sc-clip.hidden { opacity: 0.45; border-style: dashed; }
.sc-clip:focus-visible { outline: 2px solid var(--portal-cyan); outline-offset: 1px; }
.sc-clip-strip { position: relative; overflow: hidden; background: var(--portal-media-letterbox); }
.sc-clip-strip.image { background-repeat: repeat-x; background-size: auto 100%; }
.sc-clip-thumb { position: absolute; top: 0; bottom: 0; background-repeat: no-repeat; }
.sc-clip-wave { display: block; }
.sc-clip-wave path { stroke: var(--portal-green); stroke-width: 1.5; fill: none; }
.sc-clip-wave.flat path { stroke: var(--portal-muted); stroke-dasharray: 3 3; }
.sc-clip-label { display: flex; align-items: center; gap: 4px; height: 18px; padding: 0 6px; overflow: hidden; font-size: 11px; white-space: nowrap; }
.sc-clip-text { overflow: hidden; text-overflow: ellipsis; }
.sc-clip-meta { color: var(--portal-muted); font-variant-numeric: tabular-nums; }
.sc-clip-mark { padding: 0 4px; border-radius: 3px; color: var(--portal-text-soft); background: var(--portal-surface-3); font-size: 10px; }
.sc-clip-mark.ai { color: var(--portal-cyan); background: var(--portal-cyan-soft); }
.sc-clip-mark.warn { color: var(--portal-danger); background: var(--portal-danger-soft); }
.sc-clip-mark.busy { color: var(--portal-gold); background: var(--portal-gold-soft); }
/* 生成进度：贴底边的一条；还没收到进度时整条做往复流动，免得看着像卡在 0 */
.sc-clip-progress { position: absolute; right: 0; bottom: 0; left: 0; height: 3px; overflow: hidden; background: var(--portal-gold-soft); pointer-events: none; }
.sc-clip-progress span { display: block; height: 100%; background: var(--portal-gold); transition: width 0.3s ease; }
.sc-clip-progress.unknown span { width: 30% !important; animation: sc-clip-progress-slide 1.4s ease-in-out infinite; }
@keyframes sc-clip-progress-slide { from { transform: translateX(-100%); } to { transform: translateX(340%); } }
@media (prefers-reduced-motion: reduce) { .sc-clip-progress.unknown span { animation: none; transform: none; } }
.sc-clip-handle { position: absolute; top: 0; bottom: 0; width: 7px; cursor: ew-resize; background: transparent; }
.sc-clip-handle.start { left: 0; border-left: 3px solid transparent; }
.sc-clip-handle.end { right: 0; border-right: 3px solid transparent; }
.sc-clip:hover .sc-clip-handle.start,
.sc-clip.selected .sc-clip-handle.start { border-left-color: var(--portal-cyan); }
.sc-clip:hover .sc-clip-handle.end,
.sc-clip.selected .sc-clip-handle.end { border-right-color: var(--portal-cyan); }
</style>
