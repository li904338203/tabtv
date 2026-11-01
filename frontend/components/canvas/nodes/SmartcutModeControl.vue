<script setup lang="ts">
/**
 * 智能剪辑指令面板底部的「剪辑方式」下拉（对标视频节点的生成模式下拉，样式类名共用）。
 * 选项来自节点参数 `smartcut_mode` 的配置（配置驱动），认不出的值按「素材混剪」的图标显示。
 */
import { AudioWaveform, ChevronDown, LayoutPanelTop, Mic, Scissors, Sparkles } from "@lucide/vue";
import { controlOptions, optionLabel, optionValue, type ControlOption } from "~/utils/controlOptions";

const props = defineProps<{ modelValue?: unknown; config: Record<string, unknown>; disabled?: boolean }>();
const emit = defineEmits<{ "update:modelValue": [value: string] }>();
const root = ref<HTMLElement>();
const open = ref(false);
const ICONS: Record<string, typeof Scissors> = { mix: Sparkles, board: LayoutPanelTop, talk: Mic, narr: AudioWaveform };
const options = computed(() => controlOptions(props.config));
const selected = computed(() => options.value.find(option => optionValue(option) === String(props.modelValue ?? "")));
const selectedLabel = computed(() => (selected.value ? optionLabel(selected.value) : "剪辑方式"));
const iconOf = (option?: ControlOption) => (option ? ICONS[optionValue(option)] ?? Scissors : Scissors);

function choose(option: ControlOption) {
  emit("update:modelValue", optionValue(option));
  open.value = false;
}
function onOutside(event: PointerEvent) {
  if (root.value && event.target instanceof Node && !root.value.contains(event.target)) open.value = false;
}
onMounted(() => document.addEventListener("pointerdown", onOutside));
onBeforeUnmount(() => document.removeEventListener("pointerdown", onOutside));
</script>

<template>
  <div ref="root" class="canvas-control video-mode-control smartcut-mode-control">
    <button type="button" :title="disabled ? '需要编辑权限：你在该工作区是只读成员' : selectedLabel" :disabled="disabled" :aria-expanded="open" aria-label="剪辑方式" @click.stop="open = !open">
      <component :is="iconOf(selected)" :size="15" />
      <span>{{ selectedLabel }}</span>
      <ChevronDown :size="13" />
    </button>
    <div v-if="open" class="video-mode-popover nodrag nowheel" role="menu">
      <strong>剪辑方式</strong>
      <button
        v-for="option in options"
        :key="optionValue(option)"
        type="button"
        role="menuitemradio"
        :aria-checked="optionValue(option) === String(modelValue ?? '')"
        :class="{ active: optionValue(option) === String(modelValue ?? '') }"
        @click="choose(option)"
      >
        <component :is="iconOf(option)" :size="17" />
        <span>{{ optionLabel(option) }}</span>
      </button>
    </div>
  </div>
</template>
