<script setup lang="ts">
import type { components } from '~/types/legacy';
import { SlidersHorizontal, X } from "@lucide/vue";
import type { JsonObject } from "~/types/canvas";
import { presetDimensionsOf, presetOptionDefaults } from "~/utils/canvas";

type PresetItem = components["schemas"]["PresetResp"];

const props = defineProps<{ preset: PresetItem; options: JsonObject }>();
const emit = defineEmits<{ update: [value: JsonObject]; clear: [] }>();

const root = ref<HTMLElement>();
const open = ref(false);

// ⚠️ 维度字段新后端是 `code/name`、老后端是 `key/label`，归一走 utils 那一份。
//    只认老名字的话每一档都点不亮，调了「强度」画面也不会变。
const dimensions = computed(() => presetDimensionsOf(props.preset.dimensions));
const hasDimensions = computed(() => dimensions.value.length > 0);

/** 面板未打开过时 params 里没有 presetOptions，展示上要退回默认档，不能显示成"什么都没选"。 */
const effective = computed<JsonObject>(() => ({
  ...presetOptionDefaults(props.preset.dimensions),
  ...props.options
}));

function pick(key: string, value: string) {
  emit("update", { ...effective.value, [key]: value });
}
function onOutside(event: PointerEvent) {
  if (root.value && event.target instanceof Node && !root.value.contains(event.target)) open.value = false;
}
onMounted(() => document.addEventListener("pointerdown", onOutside));
onBeforeUnmount(() => document.removeEventListener("pointerdown", onOutside));
</script>

<template>
  <div ref="root" class="preset-chip nodrag nowheel">
    <button class="preset-chip-clear" type="button" title="取消预设" @click.stop="emit('clear')">
      <X :size="12" />
    </button>
    <b>{{ preset.name }}</b>
    <button
      v-if="hasDimensions"
      class="preset-chip-tune"
      type="button"
      title="调整强度"
      @click.stop="open = !open"
    >
      <SlidersHorizontal :size="12" />
    </button>
    <div v-if="open && hasDimensions" class="preset-chip-popover">
      <section v-for="dimension in dimensions" :key="dimension.code">
        <strong>{{ dimension.name }}</strong>
        <div>
          <button
            v-for="option in dimension.options"
            :key="option.value"
            type="button"
            :class="{ active: effective[dimension.code] === option.value }"
            @click.stop="pick(dimension.code, option.value)"
          >{{ option.label }}</button>
        </div>
      </section>
    </div>
  </div>
</template>
