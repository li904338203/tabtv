<script setup lang="ts">
/**
 * 模式下拉：Agent / 视频 / 图片 / 音频（设计 02 §4.2）。
 *
 * 样式照对标：触发器是无边框的弱底色胶囊（「视频生成 ⌄」）；浮层每项「名称 + 小标签」，
 * 选中项右侧 ✓、下方一行灰色说明（`utils/composerUi.MODE_META`）。
 *
 * Agent 模式（AGT-43）是默认模式，排在第一项，带「分钟级成片」标签；它不依赖某一类生成模型，
 * 不按模型目录置灰（推理模型不可用的原因由价格处说明）。
 * ⚠️ 某个模式没有可用模型时同样列出、置灰并写原因（「暂无可用的音频模型」）：
 *    藏起来的话用户会以为这个功能不存在。
 */
import { Check, ChevronDown } from "@lucide/vue";
import { MODE_META } from "~/utils/composerUi";
import { MODE_LABELS, type ComposerMode } from "~/utils/quickGenerate";

const props = defineProps<{
  modelValue: ComposerMode;
  /** 各模式不可用的原因；没有原因即可用。 */
  unavailable?: Partial<Record<ComposerMode, string>>;
  disabled?: boolean;
  /** 浮层向上弹（输入框贴底时）。 */
  up?: boolean;
}>();
const emit = defineEmits<{ "update:modelValue": [value: ComposerMode] }>();

const root = ref<HTMLElement>();
const open = ref(false);
const modes: ComposerMode[] = ["agent", "video", "image", "audio"];

function choose(mode: ComposerMode) {
  if (props.unavailable?.[mode]) return;
  emit("update:modelValue", mode);
  open.value = false;
}
function onOutside(event: PointerEvent) {
  if (root.value && event.target instanceof Node && !root.value.contains(event.target)) open.value = false;
}
function onKey(event: KeyboardEvent) {
  if (event.key === "Escape") open.value = false;
}
onMounted(() => document.addEventListener("pointerdown", onOutside));
onBeforeUnmount(() => document.removeEventListener("pointerdown", onOutside));
</script>

<template>
  <div ref="root" class="mode-select" :class="{ up }" @keydown="onKey">
    <button type="button" class="mode-trigger" :class="{ open }" :disabled="disabled" :aria-expanded="open" aria-haspopup="listbox" @click.stop="open = !open">
      <span>{{ MODE_LABELS[modelValue] }}</span>
      <ChevronDown :size="13" class="mode-chevron" />
    </button>
    <div v-if="open" class="mode-popover" role="listbox" aria-label="创作模式">
      <button
        v-for="mode in modes"
        :key="mode"
        type="button"
        role="option"
        class="mode-option"
        :class="{ active: mode === modelValue }"
        :aria-selected="mode === modelValue"
        :disabled="Boolean(unavailable?.[mode])"
        @click="choose(mode)"
      >
        <span class="mode-option-head">
          <strong>{{ MODE_LABELS[mode] }}</strong>
          <em v-if="MODE_META[mode].tag">{{ MODE_META[mode].tag }}</em>
          <Check v-if="mode === modelValue" :size="14" class="mode-check" />
        </span>
        <small v-if="unavailable?.[mode]">{{ unavailable[mode] }}</small>
        <small v-else-if="mode === modelValue || mode === 'agent'">{{ MODE_META[mode].description }}</small>
      </button>
    </div>
  </div>
</template>

<style scoped>
.mode-select { position: relative; min-width: 0; }
.mode-trigger {
  height: 30px;
  display: inline-flex;
  align-items: center;
  gap: 4px;
  border: 0;
  border-radius: 999px;
  padding: 0 10px 0 12px;
  color: var(--portal-text);
  background: var(--portal-surface-2);
  font: inherit;
  font-size: 13px;
  white-space: nowrap;
  cursor: pointer;
}
.mode-trigger:hover:not(:disabled), .mode-trigger.open { background: var(--portal-surface-3); }
.mode-trigger:disabled { color: var(--portal-disabled-text); cursor: not-allowed; }
.mode-chevron { color: var(--portal-muted); transition: transform .15s ease; }
.mode-trigger.open .mode-chevron { transform: rotate(180deg); }
.mode-popover {
  width: 290px;
  max-width: calc(100vw - 24px);
  position: absolute;
  top: calc(100% + 8px);
  left: 0;
  z-index: 80;
  display: grid;
  gap: 2px;
  padding: 6px;
  border: 1px solid var(--portal-line-strong);
  border-radius: 14px;
  background: var(--portal-elevated);
  box-shadow: 0 18px 48px var(--portal-shadow-strong);
}
.mode-select.up .mode-popover { top: auto; bottom: calc(100% + 8px); }
.mode-option {
  display: grid;
  gap: 4px;
  border: 0;
  border-radius: 10px;
  padding: 10px 12px;
  color: var(--portal-text);
  background: transparent;
  font: inherit;
  text-align: left;
  cursor: pointer;
}
.mode-option:hover:not(:disabled) { background: var(--portal-hover); }
.mode-option.active { background: var(--portal-surface-2); }
.mode-option:disabled { color: var(--portal-disabled-text); cursor: not-allowed; }
.mode-option-head { display: flex; align-items: center; gap: 6px; font-size: 13px; }
.mode-option-head strong { font-weight: 500; white-space: nowrap; }
.mode-option-head em {
  border-radius: 5px;
  padding: 1px 6px;
  color: var(--portal-text-soft);
  background: var(--portal-surface-3);
  font-size: 11px;
  font-style: normal;
  white-space: nowrap;
}
.mode-option:disabled .mode-option-head em { color: var(--portal-disabled-text); }
.mode-check { margin-left: auto; color: var(--portal-text); }
.mode-option small { color: var(--portal-faint); font-size: 11px; line-height: 16px; }
</style>
