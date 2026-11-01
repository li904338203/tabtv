<script setup lang="ts">
/**
 * 营销输入框底栏的「创意 / Hook / 风格」三个按钮（MKT-T-13；MKT-F-14、dev/03 §3.2 / §9）。
 *
 * - 只管按钮，不管弹层：点一下发 `toggle(dim)`，由输入框按 `dim` 挂 `MarketingOptionPicker`
 *   （与技能 / @ 弹层同一个挂载点、同一套 `pickerPlacement`）。弹层在按钮之外渲染，是因为它要以**整个输入框**为宽度定位。
 * - 选中后按钮显示已选名称（全名在 title 里），没选显示维度名。
 * - 按钮带 `data-picker-trigger`：弹层的「点外部关闭」不把它当外部 —— 否则点按钮关弹层时，先被外部点击关掉、
 *   再被按钮的 click 重新打开，看起来像是关不掉。
 * - **窄屏（< 640px）**：四个选择器（含技能）收进一个「选项」按钮，打开全屏面板，维度页签在顶部；
 *   面板内容由调用方经 `#panel="{ dim, close }"` 插槽给（技能页签放 `SkillPicker embedded`，其余放
 *   `MarketingOptionPicker embedded`）。调用方自己的「技能」按钮在窄屏要隐藏（用 `narrow` 事件或同一断点的 CSS）。
 */
import { ChevronDown, SlidersHorizontal, X } from "@lucide/vue";
import { CHIP_DIM_ORDER, type ComposerChip, type ComposerChipDim } from "~/utils/composerKeys";

type PickDim = Exclude<ComposerChipDim, "skill">;

const props = withDefaults(defineProps<{
  /** 当前胶囊（取各维已选名称）。 */
  chips?: ComposerChip[];
  /** 当前打开的弹层维度（按钮高亮、aria-expanded）。 */
  open?: ComposerChipDim | null;
  disabled?: boolean;
}>(), { chips: () => [], open: null, disabled: false });

const emit = defineEmits<{ toggle: [dim: PickDim]; narrow: [value: boolean] }>();

const PICK_DIMS: { dim: PickDim; label: string }[] = [
  { dim: "creative", label: "创意" },
  { dim: "hook", label: "Hook" },
  { dim: "style", label: "风格" }
];
const PANEL_LABEL: Record<ComposerChipDim, string> = { skill: "技能", creative: "创意", hook: "Hook", style: "风格" };

function picked(dim: ComposerChipDim): ComposerChip | undefined {
  return props.chips.find(chip => chip.dim === dim);
}

const pickedCount = computed(() => props.chips.length);

// ---- 窄屏：收进一个「选项」按钮 + 全屏面板 ----
const narrow = ref(false);
const panelOpen = ref(false);
const panelDim = ref<ComposerChipDim>("creative");
let query: MediaQueryList | undefined;

function syncNarrow() {
  narrow.value = !!query?.matches;
  if (!narrow.value) panelOpen.value = false;
  emit("narrow", narrow.value);
}

function openPanel(dim: ComposerChipDim = panelDim.value) {
  panelDim.value = dim;
  panelOpen.value = true;
}

function closePanel() {
  panelOpen.value = false;
}

function onPanelKey(event: KeyboardEvent) {
  if (event.key === "Escape" && !event.defaultPrevented) {
    event.stopPropagation();
    closePanel();
  }
}

onMounted(() => {
  query = window.matchMedia?.("(max-width: 639px)");
  query?.addEventListener?.("change", syncNarrow);
  syncNarrow();
});

onBeforeUnmount(() => query?.removeEventListener?.("change", syncNarrow));

defineExpose({ openPanel, closePanel });
</script>

<template>
  <div class="selector-bar">
    <template v-if="!narrow">
      <button
        v-for="item in PICK_DIMS"
        :key="item.dim"
        type="button"
        class="selector-button"
        :class="{ open: open === item.dim, picked: !!picked(item.dim) }"
        :disabled="disabled"
        :title="picked(item.dim) ? `${item.label}：${picked(item.dim)!.name}` : `选择${item.label}`"
        :aria-expanded="open === item.dim"
        aria-haspopup="dialog"
        data-picker-trigger
        @click="emit('toggle', item.dim)"
      >
        <span class="selector-text">{{ picked(item.dim)?.name || item.label }}</span>
        <ChevronDown :size="13" />
      </button>
    </template>
    <button
      v-else
      type="button"
      class="selector-button"
      :class="{ picked: pickedCount > 0 }"
      :disabled="disabled"
      aria-haspopup="dialog"
      :aria-expanded="panelOpen"
      @click="openPanel()"
    >
      <SlidersHorizontal :size="13" />
      <span class="selector-text">选项</span>
      <small v-if="pickedCount" class="selector-count">{{ pickedCount }}</small>
    </button>

    <Teleport to="body">
      <div v-if="narrow && panelOpen" class="selector-panel" role="dialog" aria-label="选择技能、创意、Hook 与风格" @keydown="onPanelKey">
        <header class="selector-panel-head">
          <nav class="selector-tabs" aria-label="维度">
            <button
              v-for="dim in CHIP_DIM_ORDER"
              :key="dim"
              type="button"
              :class="{ active: panelDim === dim }"
              @click="panelDim = dim"
            >
              {{ PANEL_LABEL[dim] }}<small v-if="picked(dim)">·{{ picked(dim)!.name }}</small>
            </button>
          </nav>
          <button type="button" class="selector-close" aria-label="关闭" @click="closePanel"><X :size="18" /></button>
        </header>
        <div class="selector-panel-body">
          <slot name="panel" :dim="panelDim" :close="closePanel" />
        </div>
      </div>
    </Teleport>
  </div>
</template>

<style scoped>
.selector-bar { display: inline-flex; align-items: center; gap: 2px; min-width: 0; }
.selector-button {
  display: inline-flex;
  align-items: center;
  gap: 4px;
  max-width: 132px;
  height: 30px;
  border: 0;
  border-radius: 8px;
  padding: 0 8px;
  color: var(--portal-text-soft);
  background: transparent;
  font: inherit;
  font-size: 12px;
  cursor: pointer;
}
.selector-button:hover:not(:disabled), .selector-button.open { color: var(--portal-text); background: var(--portal-hover); }
.selector-button.picked { color: var(--portal-text); }
.selector-button:disabled { color: var(--portal-disabled-text); cursor: not-allowed; }
.selector-text { min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.selector-count { border-radius: 999px; padding: 0 6px; color: var(--portal-mark-text); background: var(--portal-mark-bg); font-size: 10px; line-height: 16px; }
.selector-panel { position: fixed; inset: 0; z-index: 110; display: flex; flex-direction: column; color: var(--portal-text); background: var(--portal-elevated); }
.selector-panel-head { display: flex; align-items: center; gap: 8px; padding: 8px 10px; border-bottom: 1px solid var(--portal-line-soft); }
.selector-tabs { flex: 1; min-width: 0; display: flex; gap: 4px; overflow-x: auto; }
.selector-tabs button { flex: none; max-width: 160px; overflow: hidden; border: 0; border-radius: 8px; padding: 6px 10px; color: var(--portal-text-soft); background: transparent; font: inherit; font-size: 13px; text-overflow: ellipsis; white-space: nowrap; cursor: pointer; }
.selector-tabs button.active { color: var(--portal-text); background: var(--portal-hover); }
.selector-tabs small { margin-left: 2px; color: var(--portal-muted); }
.selector-close { flex: none; display: grid; place-items: center; width: 32px; height: 32px; border: 0; border-radius: 8px; color: var(--portal-muted); background: transparent; cursor: pointer; }
.selector-panel-body { position: relative; flex: 1; min-height: 0; }
</style>
