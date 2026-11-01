<script setup lang="ts">
/**
 * 时间轴的一行（DRT-24）：一个对象的一条通道。菱形关键帧可以点选、拖动（吸附整帧），松手才提交一次（一步撤销）。
 *
 * - 关键帧是按钮：Tab 走得到，名称是「机位 1 · 第 90 帧 · 平滑」；选中后 ← / → 挪 1 帧（编辑器快捷键处理）；
 * - 拖动只改本行的显示位置（`dragging`），松手才发 `move`：每移一帧就提交的话一次拖动就是几十步撤销；
 * - 只读时点得到（看插值方式）但拖不动。
 */
import { directorFrameAtX, directorKeyframeLabel, type DirectorKeyframe } from "~/utils/directorTimeline";
import type { DirectorTimelineRow } from "~/utils/directorAnimation";

const props = defineProps<{
  row: DirectorTimelineRow;
  zoom: number;
  duration: number;
  /** 本行选中的关键帧（帧号）；没有时为 null。 */
  selectedFrame: number | null;
  readonly: boolean;
}>();
const emit = defineEmits<{ select: [frame: number]; move: [from: number, to: number] }>();

const lane = ref<HTMLElement | null>(null);
const dragging = ref<{ from: number; to: number; pointerId: number; moved: boolean } | null>(null);

function shownFrame(key: DirectorKeyframe): number {
  return dragging.value && dragging.value.from === key.frame ? dragging.value.to : key.frame;
}

function frameAt(event: PointerEvent): number {
  const rect = lane.value?.getBoundingClientRect();
  return rect ? directorFrameAtX(event.clientX - rect.left, props.zoom, props.duration) : 0;
}

function onDown(event: PointerEvent, key: DirectorKeyframe) {
  emit("select", key.frame);
  if (props.readonly || event.button !== 0) return;
  (event.currentTarget as HTMLElement).setPointerCapture(event.pointerId);
  dragging.value = { from: key.frame, to: key.frame, pointerId: event.pointerId, moved: false };
}

function onMove(event: PointerEvent) {
  const drag = dragging.value;
  if (!drag || drag.pointerId !== event.pointerId) return;
  const to = frameAt(event);
  if (to !== drag.to) dragging.value = { ...drag, to, moved: true };
}

function onUp(event: PointerEvent) {
  const drag = dragging.value;
  if (!drag || drag.pointerId !== event.pointerId) return;
  dragging.value = null;
  if (drag.moved && drag.to !== drag.from) emit("move", drag.from, drag.to);
}

function onCancel() {
  dragging.value = null;
}
</script>

<template>
  <div ref="lane" class="timeline-lane" :style="{ width: `${duration * zoom}px` }">
    <button
      v-for="key in row.keyframes"
      :key="key.frame"
      type="button"
      class="timeline-key"
      :class="[`interp-${key.interpolation ?? 'linear'}`, { selected: selectedFrame === key.frame, dragging: dragging?.from === key.frame }]"
      :style="{ left: `${shownFrame(key) * zoom}px` }"
      :aria-label="directorKeyframeLabel(row.label, key.frame, key.interpolation)"
      :aria-pressed="selectedFrame === key.frame"
      :title="directorKeyframeLabel(row.label, key.frame, key.interpolation)"
      :data-frame="key.frame"
      @pointerdown="onDown($event, key)"
      @pointermove="onMove"
      @pointerup="onUp"
      @pointercancel="onCancel"
    />
  </div>
</template>

<style scoped>
.timeline-lane { position: relative; height: 100%; }
.timeline-key {
  position: absolute;
  top: 50%;
  width: 11px;
  height: 11px;
  margin: -5.5px 0 0 -5.5px;
  padding: 0;
  border: 1px solid var(--portal-text-soft);
  background: var(--portal-panel);
  transform: rotate(45deg);
  cursor: ew-resize;
  touch-action: none;
}
.timeline-key.interp-smooth { border-radius: 50%; transform: none; }
.timeline-key.interp-hold { transform: none; }
.timeline-key.selected { border-color: var(--portal-cyan-line); background: var(--portal-cyan); }
.timeline-key.dragging { opacity: 0.75; }
.timeline-key:focus-visible { outline: 2px solid var(--portal-cyan-line); outline-offset: 2px; }
</style>
