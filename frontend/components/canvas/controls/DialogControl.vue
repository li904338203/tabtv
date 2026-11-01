<script setup lang="ts">
import { ChevronDown, SlidersHorizontal, Video } from "@lucide/vue";
import { asObject } from "~/utils/canvas";
const props = defineProps<{ modelValue?: unknown; config: Record<string, unknown>; name: string }>();
const emit = defineEmits<{ "update:modelValue": [value: unknown] }>();
const root = ref<HTMLElement>();
const open = ref(false);
const icon = computed(() => props.config.icon === "video" ? Video : SlidersHorizontal);
/** 悬停提示取配置（`config.hint`）。 */
const hint = computed(() => typeof props.config.hint === "string" && props.config.hint ? props.config.hint : props.name);
const sections = computed(() => Array.isArray(props.config.sections) ? props.config.sections.map(asObject) : []);
const values = computed(() => asObject(props.modelValue));
function update(key: string, value: unknown) { emit("update:modelValue", { ...values.value, [key]: value }); }
function sectionName(section: Record<string, unknown>) { return String(section.label ?? section.name ?? section.key ?? "参数"); }
function sectionValue(section: Record<string, unknown>) { return values.value[String(section.key)] ?? section.default; }
function onOutside(event: PointerEvent) {
  if (root.value && event.target instanceof Node && !root.value.contains(event.target)) open.value = false;
}
onMounted(() => document.addEventListener("pointerdown", onOutside));
onBeforeUnmount(() => document.removeEventListener("pointerdown", onOutside));
</script>

<template>
  <div ref="root" class="canvas-control control-dialog">
    <button type="button" :title="hint" :aria-expanded="open" @click.stop="open = !open"><component :is="icon" :size="14" /><span>{{ name }}</span><ChevronDown :size="13" /></button>
    <div v-if="open" class="control-popover nodrag nowheel">
      <strong>{{ name }}</strong>
      <template v-for="section in sections" :key="String(section.key)">
        <CanvasControlsSliderControl v-if="section.control === 'slider'" :name="sectionName(section)" :config="section" :model-value="sectionValue(section)" @update:model-value="update(String(section.key), $event)" />
        <CanvasControlsSwitchControl v-else-if="section.control === 'switch'" :name="sectionName(section)" :config="section" :model-value="sectionValue(section)" @update:model-value="update(String(section.key), $event)" />
        <CanvasControlsGridControl v-else :name="sectionName(section)" :config="section" :model-value="sectionValue(section)" @update:model-value="update(String(section.key), $event)" />
      </template>
      <span v-if="!sections.length" class="control-empty">使用默认配置</span>
    </div>
  </div>
</template>
