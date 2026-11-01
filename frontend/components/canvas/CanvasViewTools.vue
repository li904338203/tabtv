<script setup lang="ts">
/**
 * 画布左下角的视图工具条（2026-11-01 按用户给的参考图）：整理画布 · 画布小地图 · 隐藏节点连线 · 网格吸附 · 缩放菜单。
 *
 * 只管显示和收点击：三个开关走 v-model，动作抛给 CanvasBoard —— 同样的动作快捷键也要用
 * （⌥⇧F / ⌘+ / ⌘- / ⌘0，见 utils/canvasShortcuts），实现只放在一处。
 */
import { LayoutGrid, Magnet, MapPinned, Spline } from "@lucide/vue";
import { CANVAS_ZOOM_PRESETS, parseZoomPercent, zoomPercent } from "~/utils/canvasView";

const props = defineProps<{
  /** 当前缩放（1 = 100%）。 */
  zoom: number;
  /** 少于两个节点时没什么可整理的。 */
  canTidy: boolean;
  /** 快捷键按平台显示：Mac 是 ⌘ / ⌥⇧，其余是 Ctrl / Alt+Shift。 */
  mac: boolean;
}>();
const minimap = defineModel<boolean>("minimap", { required: true });
const edgesHidden = defineModel<boolean>("edgesHidden", { required: true });
const snapToGrid = defineModel<boolean>("snapToGrid", { required: true });
const emit = defineEmits<{
  tidy: [];
  zoomIn: [];
  zoomOut: [];
  fit: [];
  zoomTo: [zoom: number];
}>();

const keys = computed(() => props.mac
  ? { tidy: "⌥⇧F", zoomIn: "⌘ +", zoomOut: "⌘ -", fit: "⌘ 0" }
  : { tidy: "Alt+Shift+F", zoomIn: "Ctrl +", zoomOut: "Ctrl -", fit: "Ctrl 0" });

const menuOpen = ref(false);
const zoomRef = ref<HTMLElement>();
/** 输入框里的百分比：没在输入时跟着画布的缩放走。 */
const draft = ref(String(zoomPercent(props.zoom)));
const typing = ref(false);
watch(() => props.zoom, (zoom) => {
  if (!typing.value) draft.value = String(zoomPercent(zoom));
});

function closeMenu() {
  menuOpen.value = false;
}

function pick(action: "zoomIn" | "zoomOut" | "fit") {
  if (action === "zoomIn") emit("zoomIn");
  else if (action === "zoomOut") emit("zoomOut");
  else emit("fit");
  closeMenu();
}

function pickPreset(zoom: number) {
  emit("zoomTo", zoom);
  closeMenu();
}

/** 回车：认得出来就缩放到那个比例（超出范围的夹到 15%~800%），认不出来恢复成当前比例。 */
function applyDraft() {
  const zoom = parseZoomPercent(draft.value);
  typing.value = false;
  draft.value = String(zoomPercent(zoom ?? props.zoom));
  if (zoom !== null) emit("zoomTo", zoom);
  closeMenu();
}

function onDraftBlur() {
  typing.value = false;
  draft.value = String(zoomPercent(props.zoom));
}

// 菜单开着时：点菜单以外的任何地方、按 Esc 都关上。捕获阶段监听：节点、画布自己会拦下冒泡
function onPointerDown(event: PointerEvent) {
  if (zoomRef.value && event.target instanceof Node && zoomRef.value.contains(event.target)) return;
  closeMenu();
}
function onKeydown(event: KeyboardEvent) {
  if (event.key === "Escape") closeMenu();
}
function unlisten() {
  window.removeEventListener("pointerdown", onPointerDown, true);
  window.removeEventListener("keydown", onKeydown);
}
watch(menuOpen, (open) => {
  unlisten();
  if (!open) return;
  window.addEventListener("pointerdown", onPointerDown, true);
  window.addEventListener("keydown", onKeydown);
});
onBeforeUnmount(unlisten);
</script>

<template>
  <div class="canvas-view-tools" role="toolbar" aria-label="画布视图">
    <button type="button" class="view-tool" :disabled="!canTidy" aria-label="整理画布" @click="emit('tidy')">
      <LayoutGrid :size="16" />
      <span class="view-tip">整理画布<kbd>{{ keys.tidy }}</kbd></span>
    </button>
    <button type="button" class="view-tool" :class="{ active: minimap }" :aria-pressed="minimap" aria-label="画布小地图" @click="minimap = !minimap">
      <MapPinned :size="16" />
      <span class="view-tip">画布小地图</span>
    </button>
    <button
      type="button"
      class="view-tool"
      :class="{ active: edgesHidden }"
      :aria-pressed="edgesHidden"
      :aria-label="edgesHidden ? '显示节点连线' : '隐藏节点连线'"
      @click="edgesHidden = !edgesHidden"
    >
      <Spline :size="16" />
      <span class="view-tip">{{ edgesHidden ? '显示节点连线' : '隐藏节点连线' }}</span>
    </button>
    <button type="button" class="view-tool" :class="{ active: snapToGrid }" :aria-pressed="snapToGrid" aria-label="网格吸附" @click="snapToGrid = !snapToGrid">
      <Magnet :size="16" />
      <span class="view-tip">网格吸附</span>
    </button>
    <div ref="zoomRef" class="view-zoom">
      <button
        type="button"
        class="view-tool view-zoom-trigger"
        :class="{ active: menuOpen }"
        aria-haspopup="menu"
        :aria-expanded="menuOpen"
        aria-label="缩放比例"
        @click="menuOpen = !menuOpen"
      >{{ zoomPercent(zoom) }}%</button>
      <div v-if="menuOpen" class="view-zoom-menu" role="menu" aria-label="缩放">
        <label class="view-zoom-input">
          <input
            v-model="draft"
            inputmode="decimal"
            maxlength="6"
            aria-label="缩放比例（%），回车生效"
            @focus="typing = true"
            @blur="onDraftBlur"
            @keydown.enter.prevent="applyDraft"
          >
          <span>%</span>
        </label>
        <button type="button" role="menuitem" @click="pick('zoomIn')"><span>放大</span><kbd>{{ keys.zoomIn }}</kbd></button>
        <button type="button" role="menuitem" @click="pick('zoomOut')"><span>缩小</span><kbd>{{ keys.zoomOut }}</kbd></button>
        <button type="button" role="menuitem" @click="pick('fit')"><span>适合屏幕</span><kbd>{{ keys.fit }}</kbd></button>
        <hr>
        <button v-for="preset in CANVAS_ZOOM_PRESETS" :key="preset" type="button" role="menuitem" @click="pickPreset(preset)">
          <span>缩放至{{ zoomPercent(preset) }}%</span>
        </button>
      </div>
    </div>
  </div>
</template>

<style scoped>
.canvas-view-tools { display: flex; align-items: center; gap: 4px; }
.view-tool {
  position: relative;
  min-width: 30px;
  height: 30px;
  display: grid;
  place-items: center;
  border: 0;
  border-radius: 7px;
  padding: 0 6px;
  color: var(--cv-n70);
  background: transparent;
  font-size: 12px;
  font-variant-numeric: tabular-nums;
  cursor: pointer;
}
.view-tool:hover:not(:disabled) { color: var(--cv-n100); background: var(--cv-n22); }
/* 开着的开关：与顶栏 Agent 按钮的选中态同一套（不是纯黑） */
.view-tool.active { color: var(--canvas-text); background: var(--cv-n24); }
.view-tool:disabled { opacity: .35; cursor: default; }
.view-zoom-trigger { padding: 0 8px; }

/* 悬停说明（参考图：图标正上方一个小标签，快捷键淡一号）；不用 title —— 系统提示要等一秒多才出 */
.view-tip {
  position: absolute;
  left: 50%;
  bottom: calc(100% + 8px);
  z-index: 2;
  display: none;
  border: 1px solid var(--canvas-line-strong);
  border-radius: 7px;
  padding: 4px 9px;
  color: var(--cv-n92);
  background: var(--cv-n15);
  box-shadow: 0 8px 24px var(--cv-shade-38);
  font-size: 12px;
  line-height: 1.5;
  white-space: nowrap;
  transform: translateX(-50%);
  pointer-events: none;
}
.view-tool:hover > .view-tip,
.view-tool:focus-visible > .view-tip { display: block; }
/* 第一个按钮离画布左边只有十几像素：说明居中会被画布边缘裁掉，改成左对齐 */
.view-tool:first-child > .view-tip { left: 0; transform: none; }
.view-tip kbd { margin-left: 8px; color: var(--cv-n57); font: inherit; }

.view-zoom { position: relative; }
.view-zoom-menu {
  position: absolute;
  left: 50%;
  bottom: calc(100% + 10px);
  z-index: 3;
  width: 196px;
  display: flex;
  flex-direction: column;
  gap: 2px;
  border: 1px solid var(--canvas-line-strong);
  border-radius: 12px;
  padding: 8px;
  background: var(--cv-n15);
  box-shadow: 0 24px 56px var(--cv-shade-50);
  transform: translateX(-50%);
}
.view-zoom-input {
  height: 34px;
  display: flex;
  align-items: center;
  gap: 6px;
  margin-bottom: 4px;
  border: 1px solid transparent;
  border-radius: 7px;
  padding: 0 10px;
  color: var(--cv-n57);
  background: var(--cv-n21);
  font-size: 13px;
}
.view-zoom-input:focus-within { border-color: var(--canvas-line-strong); }
.view-zoom-input input {
  min-width: 0;
  flex: 1;
  border: 0;
  padding: 0;
  color: var(--cv-n92);
  background: transparent;
  font: inherit;
  font-variant-numeric: tabular-nums;
  outline: 0;
}
.view-zoom-menu > button {
  height: 34px;
  display: flex;
  align-items: center;
  justify-content: space-between;
  border: 0;
  border-radius: 7px;
  padding: 0 10px;
  color: var(--cv-n92);
  background: transparent;
  font-size: 13px;
  text-align: left;
  cursor: pointer;
}
.view-zoom-menu > button:hover { color: var(--cv-n100); background: var(--cv-n21); }
.view-zoom-menu kbd { color: var(--cv-n57); font: inherit; font-size: 12px; }
.view-zoom-menu hr { width: calc(100% - 12px); height: 1px; margin: 4px 6px; border: 0; background: var(--canvas-line); }
</style>
