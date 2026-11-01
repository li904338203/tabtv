<script setup lang="ts">
/**
 * 预览上的画面变换控制点（SCT-21，SC-F-43）：拖框体移动、拖四角 / 四边等比缩放、拖顶上的把手旋转（按住 Shift 吸 15°）。
 *
 * 换算全部来自 `smartcutTransform.ts`（与服务端渲染同一套：先裁切 → 等比放进「画布 × 缩放」→ 绕中心旋转 → 中心偏移）。
 * 拖动中只发 `preview`（外壳拿它刷新预览），松手发一次 `commit`（一步撤销）；Esc / 指针被取消时发 `cancel`。
 * 每次 pointermove 都从**拖动开始时**的变换算，不累加（累加会把舍入误差越拖越大）。
 */
import type { components } from "@tabtv/api-client";
import {
  smartcutTransformHandles,
  smartcutTransformMove,
  smartcutTransformRotate,
  smartcutTransformScale,
  type SmartcutHandle,
  type SmartcutPoint,
  type SmartcutSize
} from "~/utils/smartcutTransform";

type ClipTransform = components["schemas"]["ClipTransform"];

const props = defineProps<{
  transform: ClipTransform | null | undefined;
  canvas: SmartcutSize;
  media: Partial<SmartcutSize> | null;
  stage: SmartcutSize;
  label: string;
}>();
const emit = defineEmits<{ preview: [transform: ClipTransform]; commit: [transform: ClipTransform]; cancel: [] }>();

type Mode = { kind: "move" } | { kind: "scale"; handle: SmartcutHandle } | { kind: "rotate" };
const dragging = shallowRef<{ mode: Mode; pointerId: number; start: SmartcutPoint; origin: ClipTransform | null | undefined; last: ClipTransform | null } | null>(null);
const rootEl = ref<HTMLElement | null>(null);

const handles = computed(() => smartcutTransformHandles(props.transform, props.canvas, props.media, props.stage));
const corners = computed(() => Object.entries(handles.value.corners) as Array<[SmartcutHandle, SmartcutPoint]>);
const edges = computed(() => Object.entries(handles.value.edges) as Array<[SmartcutHandle, SmartcutPoint]>);

function point(event: PointerEvent): SmartcutPoint {
  const rect = rootEl.value?.getBoundingClientRect();
  return { x: event.clientX - (rect?.left ?? 0), y: event.clientY - (rect?.top ?? 0) };
}

function begin(event: PointerEvent, mode: Mode) {
  if (event.button !== 0) return;
  event.stopPropagation();
  (event.currentTarget as Element).setPointerCapture?.(event.pointerId);
  dragging.value = { mode, pointerId: event.pointerId, start: point(event), origin: props.transform, last: null };
}

function move(event: PointerEvent) {
  const d = dragging.value;
  if (!d || event.pointerId !== d.pointerId) return;
  const p = point(event);
  const center = smartcutTransformHandles(d.origin, props.canvas, props.media, props.stage).center;
  let next: ClipTransform;
  if (d.mode.kind === "move") next = smartcutTransformMove(d.origin, p.x - d.start.x, p.y - d.start.y, props.stage);
  else if (d.mode.kind === "scale") next = smartcutTransformScale(d.origin, d.mode.handle, center, d.start, p);
  else next = smartcutTransformRotate(d.origin, center, d.start, p, event.shiftKey ? 15 : 0);
  dragging.value = { ...d, last: next };
  emit("preview", next);
}

function end(event: PointerEvent) {
  const d = dragging.value;
  if (!d || event.pointerId !== d.pointerId) return;
  dragging.value = null;
  if (d.last) emit("commit", d.last);
  else emit("cancel");
}

function cancel() {
  if (!dragging.value) return false;
  dragging.value = null;
  emit("cancel");
  return true;
}

defineExpose({ cancel });

const frameStyle = computed(() => {
  const h = handles.value;
  return {
    left: `${h.center.x - h.width / 2}px`,
    top: `${h.center.y - h.height / 2}px`,
    width: `${h.width}px`,
    height: `${h.height}px`,
    transform: h.rotation ? `rotate(${h.rotation}deg)` : "none"
  };
});
</script>

<template>
  <div ref="rootEl" class="sc-handles" @pointermove="move" @pointerup="end" @pointercancel="cancel">
    <div
      class="sc-handles-frame"
      :style="frameStyle"
      role="group"
      :aria-label="`${label} 的画面位置：拖动移动，拖角缩放，拖顶上的把手旋转`"
      data-testid="smartcut-transform-frame"
      @pointerdown="begin($event, { kind: 'move' })"
    />
    <span
      v-for="[name, p] in corners"
      :key="name"
      class="sc-handle corner"
      :class="name"
      :style="{ left: `${p.x}px`, top: `${p.y}px` }"
      :data-handle="name"
      aria-hidden="true"
      @pointerdown="begin($event, { kind: 'scale', handle: name })"
    />
    <span
      v-for="[name, p] in edges"
      :key="name"
      class="sc-handle edge"
      :class="name"
      :style="{ left: `${p.x}px`, top: `${p.y}px` }"
      :data-handle="name"
      aria-hidden="true"
      @pointerdown="begin($event, { kind: 'scale', handle: name })"
    />
    <span
      class="sc-handle rotate"
      :style="{ left: `${handles.rotate.x}px`, top: `${handles.rotate.y}px` }"
      data-handle="rotate"
      title="拖动旋转（按住 Shift 每 15° 一档）"
      aria-hidden="true"
      @pointerdown="begin($event, { kind: 'rotate' })"
    />
  </div>
</template>

<style scoped>
.sc-handles { position: absolute; inset: 0; z-index: 4; pointer-events: none; }
.sc-handles-frame { position: absolute; box-sizing: border-box; border: 1px solid var(--portal-cyan); pointer-events: auto; cursor: move; touch-action: none; }
.sc-handle {
  position: absolute;
  width: 10px;
  height: 10px;
  margin: -5px 0 0 -5px;
  border: 1px solid var(--portal-cyan);
  border-radius: 2px;
  background: var(--portal-elevated);
  pointer-events: auto;
  touch-action: none;
}
.sc-handle.nw, .sc-handle.se { cursor: nwse-resize; }
.sc-handle.ne, .sc-handle.sw { cursor: nesw-resize; }
.sc-handle.n, .sc-handle.s { cursor: ns-resize; }
.sc-handle.e, .sc-handle.w { cursor: ew-resize; }
.sc-handle.rotate { border-radius: 50%; cursor: grab; }
</style>
