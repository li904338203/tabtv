<script setup lang="ts">
/**
 * 打光编辑器的光源球（docs/agent/imagetools IMG-F-40，IMGD-27）：**SVG 画**（经纬线 + 投影点），不引 3D 库。
 *
 * - 「透视 / 正面」只是视角不同，数据都是 `{azimuth, elevation}`（投影 / 反算在 utils/imageToolRelight）；
 * - 拖动光源点（或点球面任意处）→ 反算方向并 emit `update`；拖动开始时光源在背面就一直在背面拖，
 *   否则「后方」光一拖就从球后面跳到前面；
 * - 键盘：聚焦球体后 ←/→/↑/↓ 调方向（`role="slider"` + `aria-valuetext` 读出当前角度）；
 * - 光锥从光源照向中间的源图小卡片；光在背面时整组画在卡片后面并变淡（被球挡住）。
 */
import {
  imageToolRelightCone,
  imageToolRelightIsBack,
  imageToolRelightProject,
  imageToolRelightStep,
  imageToolRelightUnproject,
  type ImageToolRelightView
} from "~/utils/imageToolRelight";

const props = withDefaults(defineProps<{
  azimuth: number;
  elevation: number;
  /** 源图缩略（节点上已有的 object URL）；没有时画一个占位卡片。 */
  thumbnail?: string;
  /** 光色（`#rrggbb`）；null = 无颜色（白光）。 */
  color?: string | null;
  disabled?: boolean;
}>(), { thumbnail: "", color: null, disabled: false });
const emit = defineEmits<{ update: [value: { azimuth: number; elevation: number }] }>();

const view = ref<ImageToolRelightView>("perspective");
const VIEWS: Array<{ code: ImageToolRelightView; name: string }> = [
  { code: "perspective", name: "透视" },
  { code: "front", name: "正面" }
];

/** 画布：viewBox 240×220，球心 (120,112)、半径 86。 */
const CX = 120;
const CY = 112;
const R = 86;
const CARD = 46;
const uid = `ls-${Math.random().toString(36).slice(2, 9)}`;

const svg = ref<SVGSVGElement | null>(null);
const light = computed(() => {
  const point = imageToolRelightProject(props.azimuth, props.elevation, view.value);
  return { x: CX + point.x * R, y: CY + point.y * R, visible: point.visible };
});
const cone = computed(() => imageToolRelightCone(light.value, { x: CX, y: CY }, CARD * 0.42));
/** 「无」颜色时用一点暖白示意光线（纯白在浅色主题下与球面分不开）。 */
const lightFill = computed(() => props.color || "#ffe7b0");

/** 一条曲线按可见性切成前后两组折线（前半实线、后半淡虚线）。 */
function curve(sample: (t: number) => { azimuth: number; elevation: number }) {
  const front: string[][] = [];
  const back: string[][] = [];
  let current: string[] = [];
  let currentVisible: boolean | null = null;
  const STEPS = 72;
  for (let i = 0; i <= STEPS; i += 1) {
    const { azimuth, elevation } = sample(i / STEPS);
    const point = imageToolRelightProject(azimuth, elevation, view.value);
    const xy = `${(CX + point.x * R).toFixed(1)},${(CY + point.y * R).toFixed(1)}`;
    if (currentVisible !== null && point.visible !== currentVisible) {
      current.push(xy);
      (currentVisible ? front : back).push(current);
      current = [];
    }
    currentVisible = point.visible;
    current.push(xy);
  }
  if (current.length > 1 && currentVisible !== null) (currentVisible ? front : back).push(current);
  return { front: front.map(list => list.join(" ")), back: back.map(list => list.join(" ")) };
}

/** 经纬线：赤道 + ±45° 纬线 + 前后 / 左右两条经线。 */
const grid = computed(() => {
  const lines = [
    curve(t => ({ azimuth: t * 360, elevation: 0 })),
    curve(t => ({ azimuth: t * 360, elevation: 45 })),
    curve(t => ({ azimuth: t * 360, elevation: -45 })),
    curve(t => ({ azimuth: 0, elevation: -90 + t * 180 })),
    curve(t => ({ azimuth: 180, elevation: -90 + t * 180 })),
    curve(t => ({ azimuth: 90, elevation: -90 + t * 180 })),
    curve(t => ({ azimuth: 270, elevation: -90 + t * 180 }))
  ];
  return { front: lines.flatMap(line => line.front), back: lines.flatMap(line => line.back) };
});

const valueText = computed(() => `方位角 ${props.azimuth}°，仰角 ${props.elevation}°`);

// ---------------------------------------------------------------------------
// 拖动
// ---------------------------------------------------------------------------

let dragging = false;
let dragBack = false;

/** 屏幕坐标 → 单位圆坐标（按 SVG 的实际显示尺寸缩放，面板被画布缩放时也对得上）。 */
function unitPoint(event: PointerEvent) {
  const element = svg.value;
  if (!element) return null;
  const rect = element.getBoundingClientRect();
  if (!rect.width || !rect.height) return null;
  const x = (event.clientX - rect.left) / rect.width * 240;
  const y = (event.clientY - rect.top) / rect.height * 220;
  return { x: (x - CX) / R, y: (y - CY) / R };
}

function emitAt(event: PointerEvent) {
  const point = unitPoint(event);
  if (!point) return;
  const next = imageToolRelightUnproject(point, view.value, { back: dragBack, fallbackAzimuth: props.azimuth });
  if (next.azimuth !== props.azimuth || next.elevation !== props.elevation) emit("update", next);
}

function onPointerDown(event: PointerEvent, onHandle: boolean) {
  if (props.disabled || event.button !== 0) return;
  event.preventDefault();
  // 按在光源点上：保持它所在的半球；点球面别处：落到朝向观看者的那一半（看得见的地方）
  dragBack = onHandle && imageToolRelightIsBack(props.azimuth, props.elevation, view.value);
  dragging = true;
  svg.value?.setPointerCapture?.(event.pointerId);
  svg.value?.focus({ preventScroll: true });
  emitAt(event);
}

function onPointerMove(event: PointerEvent) {
  if (dragging && !props.disabled) emitAt(event);
}

function onPointerUp(event: PointerEvent) {
  if (!dragging) return;
  dragging = false;
  svg.value?.releasePointerCapture?.(event.pointerId);
}

const KEYS: Record<string, "left" | "right" | "up" | "down"> = { ArrowLeft: "left", ArrowRight: "right", ArrowUp: "up", ArrowDown: "down" };

function onKeydown(event: KeyboardEvent) {
  const direction = KEYS[event.key];
  if (!direction || props.disabled) return;
  event.preventDefault();
  event.stopPropagation();
  const next = imageToolRelightStep({ azimuth: props.azimuth, elevation: props.elevation }, direction, event.shiftKey ? 5 : 15);
  if (next.azimuth !== props.azimuth || next.elevation !== props.elevation) emit("update", next);
}
</script>

<template>
  <div class="light-sphere" :class="{ disabled }">
    <div class="light-sphere-views" role="radiogroup" aria-label="视角">
      <button
        v-for="item in VIEWS"
        :key="item.code"
        type="button"
        role="radio"
        :aria-checked="view === item.code"
        :class="{ active: view === item.code }"
        @click="view = item.code"
      >
        {{ item.name }}
      </button>
    </div>
    <svg
      ref="svg"
      class="light-sphere-svg"
      viewBox="0 0 240 220"
      role="slider"
      :tabindex="disabled ? -1 : 0"
      aria-label="光源方向（方向键调整，按住 Shift 微调）"
      :aria-valuetext="valueText"
      :aria-valuenow="azimuth"
      aria-valuemin="0"
      aria-valuemax="360"
      :aria-disabled="disabled"
      @pointerdown="onPointerDown($event, false)"
      @pointermove="onPointerMove"
      @pointerup="onPointerUp"
      @pointercancel="onPointerUp"
      @keydown="onKeydown"
    >
      <defs>
        <radialGradient :id="`${uid}-ball`" cx="38%" cy="32%" r="75%">
          <stop offset="0%" class="stop-ball-hi" />
          <stop offset="100%" class="stop-ball-lo" />
        </radialGradient>
        <linearGradient :id="`${uid}-cone`" gradientUnits="userSpaceOnUse" :x1="light.x" :y1="light.y" :x2="CX" :y2="CY">
          <stop offset="0%" :stop-color="lightFill" stop-opacity=".75" />
          <stop offset="100%" :stop-color="lightFill" stop-opacity=".08" />
        </linearGradient>
        <clipPath :id="`${uid}-card`">
          <rect :x="CX - CARD / 2" :y="CY - CARD / 2" :width="CARD" :height="CARD" rx="5" />
        </clipPath>
      </defs>

      <circle :cx="CX" :cy="CY" :r="R" class="ball" :fill="`url(#${uid}-ball)`" />
      <polyline v-for="(points, index) in grid.back" :key="`b${index}`" :points="points" class="grid back" />

      <!-- 背面的光：画在卡片后面并变淡（被球挡住） -->
      <g v-if="!light.visible" class="light back">
        <polygon v-if="cone" :points="cone" :fill="`url(#${uid}-cone)`" />
        <circle :cx="light.x" :cy="light.y" r="8" class="handle" :stroke="lightFill" @pointerdown.stop="onPointerDown($event, true)" />
      </g>

      <g class="card">
        <rect :x="CX - CARD / 2" :y="CY - CARD / 2" :width="CARD" :height="CARD" rx="5" class="card-bg" />
        <image
          v-if="thumbnail"
          :href="thumbnail"
          :x="CX - CARD / 2"
          :y="CY - CARD / 2"
          :width="CARD"
          :height="CARD"
          preserveAspectRatio="xMidYMid slice"
          :clip-path="`url(#${uid}-card)`"
        />
      </g>

      <polyline v-for="(points, index) in grid.front" :key="`f${index}`" :points="points" class="grid" />

      <g v-if="light.visible" class="light">
        <polygon v-if="cone" :points="cone" :fill="`url(#${uid}-cone)`" />
        <circle v-else :cx="CX" :cy="CY" :r="CARD * 0.72" :fill="lightFill" opacity=".18" />
        <circle :cx="light.x" :cy="light.y" r="9" class="handle" :stroke="lightFill" @pointerdown.stop="onPointerDown($event, true)" />
      </g>
    </svg>
  </div>
</template>

<style scoped>
.light-sphere { display: flex; flex-direction: column; align-items: center; gap: 6px; }
.light-sphere-views { display: inline-flex; padding: 2px; border-radius: 7px; background: var(--cv-n20); }
.light-sphere-views > button { height: 24px; border: 0; border-radius: 5px; padding: 0 12px; color: var(--cv-n63); background: transparent; font-size: 11px; cursor: pointer; }
.light-sphere-views > button.active { color: var(--cv-n92); background: var(--cv-n26); }
.light-sphere-views > button:focus-visible { outline: 2px solid var(--canvas-cyan); outline-offset: 1px; }
.light-sphere-svg { width: 100%; max-width: 250px; height: auto; touch-action: none; cursor: grab; border-radius: 8px; outline: none; }
.light-sphere-svg:focus-visible { box-shadow: 0 0 0 2px var(--canvas-cyan); }
.light-sphere.disabled .light-sphere-svg { cursor: not-allowed; opacity: .45; }
.stop-ball-hi { stop-color: var(--cv-tint-22); }
.stop-ball-lo { stop-color: var(--cv-tint-6); }
.ball { stroke: var(--cv-tint-24); stroke-width: 1; }
.grid { fill: none; stroke: var(--cv-tint-24); stroke-width: .8; }
.grid.back { stroke: var(--cv-tint-10); stroke-dasharray: 2 3; }
.card-bg { fill: var(--cv-n26); stroke: var(--cv-tint-24); stroke-width: 1; }
.light.back { opacity: .45; }
/* 光源点在深浅两种主题下都是深色（截图约定），描边是光色 */
.handle { fill: #1f2125; stroke-width: 3; cursor: grab; filter: drop-shadow(0 1px 3px var(--cv-shade-42)); }
.light-sphere.disabled .handle { cursor: not-allowed; }
</style>
