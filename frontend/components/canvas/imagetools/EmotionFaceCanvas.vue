<script setup lang="ts">
/**
 * 情绪调节左侧的「画面」视图（docs/agent/imagetools IMG-F-12、IMGD-30）：源图 + 角色框（选中的高亮）+ 框选。
 * 方案变更后（2026-10-31）没有自动识别，框选是添加角色的唯一入口；一个框都没有时给空态提示。
 *
 * 坐标换算（同 CLAUDE.md §5.9 蒙版那三条）：
 * - 图按 `object-fit: contain` 摆放，换算由 `imageToolFit` / `imageToolDisplayBoxToImage` 做（减居中偏移、除缩放比、夹到图内、< 24px 丢弃）；
 * - ⚠️ 面板在 Vue Flow 带 transform 的视口里：`getBoundingClientRect` 是**缩放后**的屏幕尺寸，而框的摆放用的是
 *   CSS 像素（`clientWidth`）。指针坐标要先按 `clientWidth / rect.width` 换回 CSS 像素，否则画布缩放到 50% 时
 *   拖出来的框会偏到别处 —— 而在 100% 缩放下一切正常。
 * - 显示的可能是缩略图，换算一律按**原图尺寸**（`natural`）算：缩略图与原图同宽高比，contain 摆放完全一致。
 */
import { X } from "@lucide/vue";
import {
  imageToolDisplayBoxToImage,
  imageToolFit,
  imageToolImageBoxToDisplay,
  type ImageToolEditorRole,
  type ImageToolPixelBox
} from "~/utils/imageTools";

const props = withDefaults(defineProps<{
  /** 已取到的源图 object URL（受保护媒体，父组件负责取与释放）。 */
  src?: string;
  /** 原图像素尺寸；未知（0）时不让框选。 */
  natural: { width: number; height: number };
  roles: readonly ImageToolEditorRole[];
  labels: Record<string, string>;
  selectedId?: string;
  /** 是否处在「手动添加」的框选状态。 */
  drawing?: boolean;
}>(), { src: "", selectedId: "", drawing: false });
const emit = defineEmits<{
  draw: [box: ImageToolPixelBox];
  /** 拖得太小（任一边 < 24px 原图像素）被丢弃。 */
  "draw-rejected": [];
  select: [id: string];
  remove: [id: string];
}>();

const stage = ref<HTMLElement | null>(null);
const size = ref({ width: 0, height: 0 });
let observer: ResizeObserver | null = null;

const fit = computed(() => imageToolFit(size.value.width, size.value.height, props.natural.width, props.natural.height));
const canDraw = computed(() => props.drawing && Boolean(fit.value) && Boolean(props.src));
const boxes = computed(() => fit.value
  ? props.roles.map(role => ({ role, rect: imageToolImageBoxToDisplay(role.box, fit.value!) }))
  : []);

/** 拖拽中的两个对角点（CSS 像素，相对舞台左上角）。 */
const dragStart = ref<{ x: number; y: number } | null>(null);
const dragEnd = ref<{ x: number; y: number } | null>(null);
const dragRect = computed(() => {
  if (!dragStart.value || !dragEnd.value) return null;
  return {
    left: Math.min(dragStart.value.x, dragEnd.value.x),
    top: Math.min(dragStart.value.y, dragEnd.value.y),
    width: Math.abs(dragEnd.value.x - dragStart.value.x),
    height: Math.abs(dragEnd.value.y - dragStart.value.y)
  };
});

function localPoint(event: PointerEvent) {
  const el = stage.value!;
  const rect = el.getBoundingClientRect();
  const sx = rect.width ? el.clientWidth / rect.width : 1;
  const sy = rect.height ? el.clientHeight / rect.height : 1;
  return { x: (event.clientX - rect.left) * sx, y: (event.clientY - rect.top) * sy };
}

function onPointerDown(event: PointerEvent) {
  if (!canDraw.value || event.button !== 0) return;
  event.preventDefault();
  (event.currentTarget as HTMLElement).setPointerCapture?.(event.pointerId);
  dragStart.value = localPoint(event);
  dragEnd.value = dragStart.value;
}

function onPointerMove(event: PointerEvent) {
  if (!dragStart.value) return;
  dragEnd.value = localPoint(event);
}

function onPointerUp(event: PointerEvent) {
  if (!dragStart.value || !fit.value) return;
  const end = localPoint(event);
  const box = imageToolDisplayBoxToImage(dragStart.value, end, fit.value);
  dragStart.value = null;
  dragEnd.value = null;
  if (box) emit("draw", box);
  else emit("draw-rejected");
}

function onPointerCancel() {
  dragStart.value = null;
  dragEnd.value = null;
}

function measure() {
  if (!stage.value) return;
  size.value = { width: stage.value.clientWidth, height: stage.value.clientHeight };
}

onMounted(() => {
  measure();
  if (typeof ResizeObserver !== "undefined" && stage.value) {
    observer = new ResizeObserver(measure);
    observer.observe(stage.value);
  }
});
onBeforeUnmount(() => observer?.disconnect());
</script>

<template>
  <div
    ref="stage"
    class="emotion-face-canvas"
    :class="{ 'is-drawing': canDraw }"
    @pointerdown="onPointerDown"
    @pointermove="onPointerMove"
    @pointerup="onPointerUp"
    @pointercancel="onPointerCancel"
  >
    <img v-if="src" :src="src" alt="源图" class="emotion-face-image" draggable="false" @load="measure">
    <p v-else class="emotion-face-empty">源图加载中…</p>
    <div
      v-for="item in boxes"
      :key="item.role.id"
      class="emotion-face-box"
      :class="{ 'is-selected': item.role.id === selectedId, 'is-touched': item.role.touched }"
      :style="{ left: `${item.rect.left}px`, top: `${item.rect.top}px`, width: `${item.rect.width}px`, height: `${item.rect.height}px` }"
    >
      <button
        type="button"
        class="emotion-face-box-label"
        :aria-pressed="item.role.id === selectedId"
        :aria-label="`选中${labels[item.role.id] ?? '角色'}`"
        @pointerdown.stop
        @click="emit('select', item.role.id)"
      >
        {{ labels[item.role.id] ?? "角色" }}
      </button>
      <button
        v-if="item.role.id === selectedId"
        type="button"
        class="emotion-face-box-remove"
        :aria-label="`删除${labels[item.role.id] ?? '角色'}`"
        title="删除这个角色"
        @pointerdown.stop
        @click="emit('remove', item.role.id)"
      >
        <X :size="11" />
      </button>
    </div>
    <div
      v-if="dragRect"
      class="emotion-face-drag"
      :style="{ left: `${dragRect.left}px`, top: `${dragRect.top}px`, width: `${dragRect.width}px`, height: `${dragRect.height}px` }"
    />
    <p v-if="canDraw && !dragRect" class="emotion-face-tip" role="status" data-testid="emotion-face-tip">
      {{ roles.length ? "再拖出一个框，添加一个角色" : "在画面上框选要调整表情的人" }}
    </p>
    <p v-else-if="drawing && !fit && src" class="emotion-face-tip" role="status">源图尺寸未知，暂时不能框选</p>
    <p v-else-if="!drawing && !roles.length && src" class="emotion-face-tip" role="status">点「框选添加」，在画面上框选要调整表情的人</p>
  </div>
</template>

<style scoped>
.emotion-face-canvas { position: relative; width: 100%; height: 100%; overflow: hidden; border-radius: 12px; background: var(--cv-n08); user-select: none; touch-action: none; }
.emotion-face-canvas.is-drawing { cursor: crosshair; }
.emotion-face-image { width: 100%; height: 100%; display: block; object-fit: contain; pointer-events: none; }
.emotion-face-empty { position: absolute; inset: 0; display: grid; place-items: center; margin: 0; color: var(--cv-n63); font-size: 12px; }
.emotion-face-box { position: absolute; border: 1.5px solid var(--cv-n70); border-radius: 4px; pointer-events: none; }
.emotion-face-box.is-touched { border-style: solid; }
.emotion-face-box.is-selected { border: 2px solid var(--cv-n100); box-shadow: 0 0 0 1px var(--cv-shade-32, rgba(0,0,0,.32)); }
.emotion-face-box-label { position: absolute; top: calc(100% + 2px); left: -1px; height: 18px; border: 0; border-radius: 4px; padding: 0 6px; color: var(--cv-n08); background: var(--cv-n70); font-size: 10px; line-height: 18px; white-space: nowrap; cursor: pointer; pointer-events: auto; }
.emotion-face-box.is-selected .emotion-face-box-label { background: var(--cv-n100); }
.emotion-face-box-remove { position: absolute; top: -9px; right: -9px; width: 18px; height: 18px; display: grid; place-items: center; border: 0; border-radius: 50%; padding: 0; color: var(--cv-n08); background: var(--cv-n100); cursor: pointer; pointer-events: auto; }
.emotion-face-box-label:focus-visible,
.emotion-face-box-remove:focus-visible { outline: 2px solid var(--cv-n88); outline-offset: 1px; }
.emotion-face-drag { position: absolute; border: 1.5px dashed var(--cv-n100); background: var(--cv-tint-8, rgba(255,255,255,.08)); pointer-events: none; }
.emotion-face-tip { position: absolute; left: 50%; bottom: 10px; margin: 0; padding: 5px 10px; border-radius: 6px; color: var(--cv-n92); background: var(--cv-n15); font-size: 11px; white-space: nowrap; transform: translateX(-50%); pointer-events: none; }
</style>
