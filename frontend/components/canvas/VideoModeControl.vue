<script setup lang="ts">
import {
  ChevronDown,
  GalleryHorizontalEnd,
  Image as ImageIcon,
  ImagePlus,
  ScanSearch,
  Type as TypeIcon,
  Video
} from "@lucide/vue";

type ModeOption = string | { label?: string; value?: string; disabled?: boolean };

const props = defineProps<{
  modelValue?: unknown;
  config: Record<string, unknown>;
}>();
const emit = defineEmits<{ "update:modelValue": [value: string] }>();
const root = ref<HTMLElement>();
const open = ref(false);
const icons = {
  text2video: TypeIcon,
  ref2video: ScanSearch,
  img2video: ImageIcon,
  first_last_frame: GalleryHorizontalEnd,
  image_reference: ImagePlus,
  video2video: Video
};
const options = computed(() => Array.isArray(props.config.options) ? props.config.options as ModeOption[] : []);

function value(option: ModeOption) {
  return typeof option === "object" ? option.value ?? "" : option;
}
function label(option: ModeOption) {
  return typeof option === "object" ? option.label ?? value(option) : option;
}
function disabled(option: ModeOption) {
  return typeof option === "object" && Boolean(option.disabled);
}
function icon(option: ModeOption) {
  return icons[value(option) as keyof typeof icons] ?? Video;
}
const selected = computed(() => options.value.find(option => value(option) === props.modelValue));
const selectedLabel = computed(() => selected.value ? label(selected.value) : "文生视频");

function choose(option: ModeOption) {
  if (disabled(option)) return;
  emit("update:modelValue", value(option));
  open.value = false;
}
function onOutside(event: PointerEvent) {
  if (root.value && event.target instanceof Node && !root.value.contains(event.target)) open.value = false;
}
onMounted(() => document.addEventListener("pointerdown", onOutside));
onBeforeUnmount(() => document.removeEventListener("pointerdown", onOutside));
</script>

<template>
  <div ref="root" class="canvas-control video-mode-control">
    <button type="button" :title="selectedLabel" :aria-expanded="open" @click.stop="open = !open">
      <component :is="selected ? icon(selected) : TypeIcon" :size="15" />
      <span>{{ selectedLabel }}</span>
      <ChevronDown :size="13" />
    </button>
    <div v-if="open" class="video-mode-popover nodrag nowheel">
      <strong>视频生成模式</strong>
      <button
        v-for="option in options"
        :key="value(option)"
        type="button"
        :disabled="disabled(option)"
        :class="{ active: value(option) === modelValue }"
        @click="choose(option)"
      >
        <component :is="icon(option)" :size="17" />
        <span>{{ label(option) }}</span>
      </button>
    </div>
  </div>
</template>
