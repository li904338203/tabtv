<script setup lang="ts">
/**
 * 多角度的球形机位示意（IMG-F-32、IMGD-27）：SVG 经纬网线 + 相机投影点，不引 3D 库。
 *
 * - 相机（带源图缩略）按水平环绕 / 垂直俯仰画在球面投影上，景别越近越贴近主体；在球背面时画淡、压在主体后面。
 * - 点球面 = 相机跳到点中的位置（按投影反算，取看得见的那一面）；按住拖动 = 环绕（横向可一路绕到背面）。
 * - 上下左右箭头：俯仰 ±15°、环绕 ±45°（`imageToolStepAngle`）；球获得焦点后方向键微调 5°、Shift 大步进、Home 回正面。
 * 只 emit 新值，不自己存状态：页签切回「自定义」等规则都在纯函数里。
 */
import { ChevronDown, ChevronLeft, ChevronRight, ChevronUp } from "@lucide/vue";
import { imageToolStepAngle, type ImageToolMultiAngleOptions } from "~/utils/imageTools";
import {
  multiAngleCameraDistance,
  multiAngleFromScreen,
  multiAngleGrid,
  multiAngleHorizontalText,
  multiAngleKeyStep,
  multiAngleOrbit,
  multiAngleProject,
  multiAngleVerticalText
} from "~/utils/imageToolMultiAngle";

const props = withDefaults(defineProps<{
  modelValue: ImageToolMultiAngleOptions;
  /** 源图缩略（object URL）；没有时相机里画一个占位。 */
  thumbUrl?: string;
}>(), { thumbUrl: "" });
const emit = defineEmits<{ "update:modelValue": [value: ImageToolMultiAngleOptions] }>();

const WIDTH = 220;
const HEIGHT = 196;
const CX = WIDTH / 2;
const CY = HEIGHT / 2 + 2;
const R = 74;
const clipId = `cam-thumb-${Math.random().toString(36).slice(2, 8)}`;

const svgRef = ref<SVGSVGElement | null>(null);
const dragging = ref(false);

const grid = multiAngleGrid(CX, CY, R);
const backGrid = grid.filter(item => item.back);
const frontGrid = grid.filter(item => !item.back);

const camera = computed(() => {
  const value = props.modelValue;
  const point = multiAngleProject(value.horizontal ?? 0, value.vertical ?? 0);
  const distance = multiAngleCameraDistance(value.zoom ?? 5);
  return {
    x: CX + point.x * R * distance,
    y: CY + point.y * R * distance,
    back: point.depth < -0.02,
    // 越近越大一点：示意景别
    scale: 0.85 + (1 - distance) * 0.6
  };
});

const valueText = computed(() => {
  const h = props.modelValue.horizontal ?? 0;
  const v = props.modelValue.vertical ?? 0;
  return `水平 ${h}°（${multiAngleHorizontalText(h)}），俯仰 ${v}°（${multiAngleVerticalText(v)}）`;
});

function update(next: ImageToolMultiAngleOptions) {
  emit("update:modelValue", next);
}

function step(direction: "left" | "right" | "up" | "down") {
  update(imageToolStepAngle(props.modelValue, direction));
}

/** 屏幕像素 → SVG 坐标（viewBox 等比缩放）。 */
function toSvg(event: PointerEvent) {
  const rect = svgRef.value?.getBoundingClientRect();
  if (!rect || !rect.width || !rect.height) return null;
  const scale = Math.min(rect.width / WIDTH, rect.height / HEIGHT);
  const offsetX = (rect.width - WIDTH * scale) / 2;
  const offsetY = (rect.height - HEIGHT * scale) / 2;
  return {
    x: (event.clientX - rect.left - offsetX) / scale,
    y: (event.clientY - rect.top - offsetY) / scale,
    scale
  };
}

let drag: { pointerId: number; startX: number; startY: number; start: ImageToolMultiAngleOptions; radiusPx: number } | null = null;

function onPointerDown(event: PointerEvent) {
  if (event.button !== 0) return;
  const point = toSvg(event);
  if (!point) return;
  let start = props.modelValue;
  const onCamera = (event.target as Element | null)?.closest?.("[data-camera]");
  // 点在球上（不是相机本身）：先跳到点中的位置，再从那里继续拖
  if (!onCamera && Math.hypot(point.x - CX, point.y - CY) <= R * 1.08) {
    const jumped = multiAngleFromScreen((point.x - CX) / R, (point.y - CY) / R);
    start = { ...start, preset: "custom", ...jumped };
    update(start);
  }
  drag = { pointerId: event.pointerId, startX: event.clientX, startY: event.clientY, start, radiusPx: R * point.scale };
  dragging.value = true;
  svgRef.value?.setPointerCapture?.(event.pointerId);
  svgRef.value?.focus({ preventScroll: true });
  event.preventDefault();
}

function onPointerMove(event: PointerEvent) {
  if (!drag || event.pointerId !== drag.pointerId) return;
  const dx = event.clientX - drag.startX;
  const dy = event.clientY - drag.startY;
  if (!dx && !dy) return;
  update(multiAngleOrbit(drag.start, dx, dy, drag.radiusPx));
}

function onPointerUp(event: PointerEvent) {
  if (!drag || event.pointerId !== drag.pointerId) return;
  svgRef.value?.releasePointerCapture?.(event.pointerId);
  drag = null;
  dragging.value = false;
}

function onKeydown(event: KeyboardEvent) {
  // ⚠️ 球不是 input，画布快捷键认不出它是可编辑区域：除 Esc（关面板）与 Tab（移焦点）外一律不冒泡 ——
  //    否则在球上按退格会删掉选中的节点（CLAUDE.md §5.7 画布快捷键那一条）。
  if (event.key !== "Escape" && event.key !== "Tab") event.stopPropagation();
  const next = multiAngleKeyStep(props.modelValue, event.key, event.shiftKey);
  if (!next) return;
  event.preventDefault();
  update(next);
}
</script>

<template>
  <div class="camera-sphere">
    <svg
      ref="svgRef"
      class="camera-sphere-svg"
      :class="{ dragging }"
      :viewBox="`0 0 ${WIDTH} ${HEIGHT}`"
      tabindex="0"
      role="slider"
      aria-label="机位球：拖动或用方向键调整环绕与俯仰"
      aria-roledescription="机位球"
      :aria-valuenow="modelValue.horizontal ?? 0"
      aria-valuemin="0"
      aria-valuemax="360"
      :aria-valuetext="valueText"
      data-testid="camera-sphere"
      @pointerdown="onPointerDown"
      @pointermove="onPointerMove"
      @pointerup="onPointerUp"
      @pointercancel="onPointerUp"
      @keydown="onKeydown"
    >
      <title>{{ valueText }}</title>
      <defs>
        <clipPath :id="clipId"><rect x="-15" y="-11" width="30" height="22" rx="4" /></clipPath>
      </defs>
      <circle class="sphere-disc" :cx="CX" :cy="CY" :r="R" />
      <path v-for="(item, index) in backGrid" :key="`b${index}`" class="sphere-grid is-back" :d="item.d" />

      <!-- 背面的相机压在主体后面 -->
      <g v-if="camera.back" class="sphere-camera is-back" data-camera :transform="`translate(${camera.x} ${camera.y}) scale(${camera.scale})`">
        <rect class="camera-body" x="-16" y="-12" width="32" height="24" rx="5" />
        <image v-if="thumbUrl" :href="thumbUrl" x="-15" y="-11" width="30" height="22" preserveAspectRatio="xMidYMid slice" :clip-path="`url(#${clipId})`" />
      </g>
      <line v-if="camera.back" class="sphere-sight is-back" :x1="CX" :y1="CY" :x2="camera.x" :y2="camera.y" />

      <!-- 主体：球心的一张小卡片 + 落在赤道面上的影子 -->
      <ellipse class="subject-shadow" :cx="CX" :cy="CY + 4" rx="16" ry="5" />
      <rect class="subject-card" :x="CX - 9" :y="CY - 18" width="18" height="22" rx="3" />

      <path v-for="(item, index) in frontGrid" :key="`f${index}`" class="sphere-grid" :d="item.d" />
      <circle class="sphere-rim" :cx="CX" :cy="CY" :r="R" />

      <template v-if="!camera.back">
        <line class="sphere-sight" :x1="CX" :y1="CY - 7" :x2="camera.x" :y2="camera.y" />
        <g class="sphere-camera" data-camera :transform="`translate(${camera.x} ${camera.y}) scale(${camera.scale})`">
          <rect class="camera-body" x="-16" y="-12" width="32" height="24" rx="5" />
          <image v-if="thumbUrl" :href="thumbUrl" x="-15" y="-11" width="30" height="22" preserveAspectRatio="xMidYMid slice" :clip-path="`url(#${clipId})`" />
          <circle v-else class="camera-lens" cx="0" cy="0" r="6" />
        </g>
      </template>
    </svg>
    <button type="button" class="sphere-arrow is-up" title="向上俯仰 15°" aria-label="向上俯仰 15°" @click="step('up')"><ChevronUp :size="14" /></button>
    <button type="button" class="sphere-arrow is-down" title="向下俯仰 15°" aria-label="向下俯仰 15°" @click="step('down')"><ChevronDown :size="14" /></button>
    <button type="button" class="sphere-arrow is-left" title="向左环绕 45°" aria-label="向左环绕 45°" @click="step('left')"><ChevronLeft :size="14" /></button>
    <button type="button" class="sphere-arrow is-right" title="向右环绕 45°" aria-label="向右环绕 45°" @click="step('right')"><ChevronRight :size="14" /></button>
  </div>
</template>

<style scoped>
.camera-sphere { position: relative; width: 236px; height: 212px; flex: 0 0 auto; border-radius: 12px; background: var(--cv-n14); }
.camera-sphere-svg { position: absolute; inset: 8px; width: calc(100% - 16px); height: calc(100% - 16px); border-radius: 10px; outline: none; cursor: grab; touch-action: none; user-select: none; }
.camera-sphere-svg.dragging { cursor: grabbing; }
.camera-sphere-svg:focus-visible { box-shadow: 0 0 0 2px var(--cv-cyan-52a50); }
.sphere-disc { fill: var(--cv-n18); }
.sphere-rim { fill: none; stroke: var(--cv-n42); stroke-width: 1.2; }
.sphere-grid { fill: none; stroke: var(--cv-n48); stroke-width: 0.8; }
.sphere-grid.is-back { stroke: var(--cv-n30); stroke-dasharray: 2 3; }
.subject-shadow { fill: var(--cv-n26); }
.subject-card { fill: var(--cv-n33); stroke: var(--cv-n57); stroke-width: 1; }
.sphere-sight { stroke: var(--cv-cyan-52a70); stroke-width: 1.2; stroke-dasharray: 3 3; }
.sphere-sight.is-back { stroke: var(--cv-cyan-52a30); }
.sphere-camera { cursor: grab; }
.camera-body { fill: var(--cv-n24); stroke: var(--cv-cyan-60); stroke-width: 2; }
.camera-lens { fill: var(--cv-cyan-52a50); stroke: var(--cv-cyan-72); stroke-width: 1.5; }
.sphere-camera.is-back { opacity: 0.45; }
.sphere-arrow { position: absolute; width: 22px; height: 22px; display: grid; place-items: center; border: 0; border-radius: 6px; padding: 0; color: var(--cv-n63); background: var(--cv-n20); cursor: pointer; }
.sphere-arrow:hover,
.sphere-arrow:focus-visible { color: var(--cv-n92); background: var(--cv-n24); outline: none; box-shadow: 0 0 0 1px var(--cv-n42); }
.sphere-arrow.is-up { top: 6px; left: calc(50% - 11px); }
.sphere-arrow.is-down { bottom: 6px; left: calc(50% - 11px); }
.sphere-arrow.is-left { top: calc(50% - 11px); left: 6px; }
.sphere-arrow.is-right { top: calc(50% - 11px); right: 6px; }
</style>
