<script setup lang="ts">
/**
 * 下拉控件（画布节点的「模型」就是它）。
 *
 * 看得见的是自绘的触发器，宽度跟着**选中项的文案**走；真正的 `<select>` 透明地铺满触发器，
 * 接管点击与键盘，弹出的仍是系统原生列表（名字再长也完整显示）。
 *
 * ⚠️ 此前是裸 `<select>` + `max-width: 126px`：原生下拉的宽度由**最长的那个选项**决定 ——
 *    选了短名字也留一大截空白；名字一长又被截断、压在下拉箭头底下（「Grok Imagine Video 1.5」
 *    只剩前十几个字符）。现在空间不够时由触发器出省略号，鼠标悬停看全名。
 */
import { ChevronDown } from "@lucide/vue";
import { controlOptions, currentValue, optionDisabled, optionLabel, optionValue } from "~/utils/controlOptions";

const props = defineProps<{ modelValue?: unknown; config: Record<string, unknown>; name: string }>();
const emit = defineEmits<{ "update:modelValue": [value: unknown] }>();
const options = computed(() => controlOptions(props.config));
const hidden = computed(() => !options.value.length && Boolean(props.config.domainFrom));
const value = computed(() => currentValue(props.modelValue));
/** 当前值不在选项里（模型已下线、目录还没取到）时照实显示值本身；什么都没选时提示去选。 */
const selectedLabel = computed(() => {
  const hit = options.value.find(option => optionValue(option) === value.value);
  if (hit) return optionLabel(hit);
  return value.value || `选择${props.name}`;
});
</script>

<template>
  <label v-if="!hidden" class="canvas-control control-select" :title="selectedLabel">
    <span class="control-select-value">{{ selectedLabel }}</span>
    <ChevronDown :size="13" aria-hidden="true" />
    <select :value="value" :aria-label="name" @change="emit('update:modelValue', ($event.target as HTMLSelectElement).value)">
      <option v-for="option in options" :key="optionValue(option)" :disabled="optionDisabled(option)" :value="optionValue(option)">{{ optionLabel(option) }}</option>
    </select>
  </label>
</template>
