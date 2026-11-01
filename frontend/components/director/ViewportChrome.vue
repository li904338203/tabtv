<script setup lang="ts">
/**
 * 视口上的叠加控件（DRT-15）：导演视角 / 机位视角与机位下拉（DR-F-20）、重置视角（DR-F-21）、
 * 移动 / 旋转 / 缩放（与 W / E / R 同步）、机位预览小窗的开关与边框（DR-F-22）、自由走位开关与提示（DR-F-25），
 * 以及机位视角下的画幅框外遮罩（遮罩矩形来自引擎的 `getFrameRect()`）。
 *
 * 叠加层整体不接收指针事件，只有按钮接收：视口里的点选、拖动照常交给引擎。
 * 轴向小方块由引擎画在角上，这里不再画。
 */
import { Camera, Footprints, Move3d, PictureInPicture2, RotateCcw, Rotate3d, Scaling, Video } from "@lucide/vue";
import type { DirectorRect } from "~/utils/directorCamera";
import type { DirectorTransformMode } from "~/director/engine/events";

const props = defineProps<{
  viewMode: "director" | "camera";
  viewCameraId: string | null;
  cameras: Array<{ id: string; name: string }>;
  transformMode: DirectorTransformMode;
  walking: boolean;
  previewOn: boolean;
  frameRect: DirectorRect | null;
  previewRect: DirectorRect | null;
  readonly: boolean;
  readonlyText: string;
}>();
const emit = defineEmits<{
  "set-view": [view: { mode: "director" | "camera"; cameraId: string | null }];
  "reset-view": [];
  "toggle-preview": [];
  "toggle-walk": [];
  "set-mode": [mode: DirectorTransformMode];
}>();

const MODES: Array<{ mode: DirectorTransformMode; label: string; key: string; icon: unknown }> = [
  { mode: "translate", label: "移动", key: "W", icon: Move3d },
  { mode: "rotate", label: "旋转", key: "E", icon: Rotate3d },
  { mode: "scale", label: "缩放", key: "R", icon: Scaling }
];

/** 画幅框外的四块遮罩（上、下、左、右）。 */
const masks = computed(() => {
  const r = props.frameRect;
  if (!r) return [];
  return [
    { top: "0", left: "0", right: "0", height: `${r.y}px` },
    { top: `${r.y + r.height}px`, left: "0", right: "0", bottom: "0" },
    { top: `${r.y}px`, left: "0", width: `${r.x}px`, height: `${r.height}px` },
    { top: `${r.y}px`, left: `${r.x + r.width}px`, right: "0", height: `${r.height}px` }
  ];
});

function chooseCamera(id: string) {
  emit("set-view", { mode: "camera", cameraId: id || null });
}
</script>

<template>
  <div class="viewport-chrome">
    <div v-for="(style, index) in masks" :key="index" class="viewport-mask" :style="style" aria-hidden="true" />
    <div
      v-if="previewOn && previewRect"
      class="viewport-preview-frame"
      :style="{ left: `${previewRect.x}px`, top: `${previewRect.y}px`, width: `${previewRect.width}px`, height: `${previewRect.height}px` }"
      aria-hidden="true"
    ><span>机位预览</span></div>

    <div class="viewport-toolbar" role="toolbar" aria-label="视口">
      <div class="dr-seg" role="radiogroup" aria-label="视角">
        <button type="button" role="radio" :aria-checked="viewMode === 'director'" :class="{ active: viewMode === 'director' }" title="导演视角：自由旋转、平移、缩放" @click="emit('set-view', { mode: 'director', cameraId: null })">
          <Camera :size="13" />导演视角
        </button>
        <button type="button" role="radio" :aria-checked="viewMode === 'camera'" :class="{ active: viewMode === 'camera' }" :disabled="!cameras.length" :title="cameras.length ? '透过机位看' : '场景里还没有机位'" @click="emit('set-view', { mode: 'camera', cameraId: viewCameraId })">
          <Video :size="13" />机位视角
        </button>
      </div>
      <select v-if="viewMode === 'camera'" class="dr-select viewport-camera" :value="viewCameraId ?? ''" aria-label="透过哪个机位看" @change="chooseCamera(($event.target as HTMLSelectElement).value)">
        <option v-for="camera in cameras" :key="camera.id" :value="camera.id">{{ camera.name }}</option>
      </select>
      <button type="button" class="dr-btn" :disabled="viewMode !== 'director'" :title="viewMode === 'director' ? '回到能看全全部对象的位置' : '机位视角下不重置（视角就是机位本身）'" aria-label="重置视角" @click="emit('reset-view')">
        <RotateCcw :size="13" />重置视角
      </button>
      <span class="viewport-sep" aria-hidden="true" />
      <div class="dr-seg" role="radiogroup" aria-label="变换模式">
        <button
          v-for="item in MODES"
          :key="item.mode"
          type="button"
          role="radio"
          :aria-checked="transformMode === item.mode"
          :class="{ active: transformMode === item.mode }"
          :disabled="readonly"
          :title="readonly ? readonlyText : `${item.label}（${item.key}）`"
          :aria-label="item.label"
          @click="emit('set-mode', item.mode)"
        ><component :is="item.icon" :size="13" /></button>
      </div>
      <span class="viewport-sep" aria-hidden="true" />
      <button type="button" class="dr-btn" :class="{ primary: previewOn }" :aria-pressed="previewOn" :title="previewOn ? '关闭机位预览小窗' : '打开机位预览小窗（显示输出机位，选中机位时显示它）'" aria-label="机位预览小窗" @click="emit('toggle-preview')">
        <PictureInPicture2 :size="13" />预览
      </button>
      <button
        type="button"
        class="dr-btn"
        :class="{ primary: walking }"
        :aria-pressed="walking"
        :disabled="viewMode !== 'camera' || readonly"
        :title="readonly ? readonlyText : viewMode === 'camera' ? (walking ? '退出自由走位（Esc）' : '自由走位：WASD 移动 · E / Q 升降 · Shift 加速') : '先切到机位视角'"
        aria-label="自由走位"
        @click="emit('toggle-walk')"
      ><Footprints :size="13" />走位</button>
    </div>

    <p v-if="walking" class="viewport-walk-hint" role="status">WASD 移动 · E / Q 升降 · Shift 加速 · 拖动鼠标转向 · Esc 退出</p>
  </div>
</template>

<style scoped>
.viewport-chrome { position: absolute; inset: 0; z-index: 5; pointer-events: none; }
.viewport-mask { position: absolute; background: rgb(0 0 0 / 55%); }
.viewport-preview-frame { position: absolute; border: 1px solid var(--portal-on-media-line); border-radius: 4px; box-shadow: 0 6px 18px var(--portal-shadow); }
.viewport-preview-frame span { position: absolute; left: 6px; top: 4px; padding: 0 6px; border-radius: 4px; color: var(--portal-on-media); background: var(--portal-on-media-bg); font-size: 11px; }
.viewport-toolbar {
  position: absolute;
  left: 12px;
  top: 12px;
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 6px;
  max-width: calc(100% - 140px);
  padding: 6px;
  border: 1px solid var(--portal-line);
  border-radius: 8px;
  background: var(--portal-elevated);
  box-shadow: 0 6px 18px var(--portal-shadow);
  pointer-events: auto;
  font-size: 12px;
}
.viewport-camera { width: auto; max-width: 140px; }
.viewport-sep { width: 1px; height: 18px; background: var(--portal-line); }
.viewport-walk-hint {
  position: absolute;
  left: 50%;
  bottom: 16px;
  margin: 0;
  padding: 6px 12px;
  border-radius: 999px;
  color: var(--portal-on-media);
  background: var(--portal-on-media-bg);
  font-size: 12px;
  transform: translateX(-50%);
}
</style>
