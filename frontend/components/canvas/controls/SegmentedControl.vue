<script setup lang="ts">
import { controlOptions, optionDisabled, optionLabel, optionSelected, optionValue } from "~/utils/controlOptions";
const props = defineProps<{ modelValue?: unknown; config: Record<string, unknown>; name: string }>();
const emit = defineEmits<{ "update:modelValue": [value: unknown] }>();
const options = computed(() => controlOptions(props.config));
// ⚠️ 带 `domainFrom` 却没取到内容时**整个控件不渲染**：只剩一个光秃秃的
//    "模式"/"风格" 标签占着工具条的位置，用户以为坏了，其实是目录是空的。
const hidden = computed(() => !options.value.length && Boolean(props.config.domainFrom));
</script>

<template>
  <div v-if="!hidden" class="canvas-control control-stack"><span>{{ name }}</span><div class="control-segments">
    <button v-for="option in options" :key="optionValue(option)" type="button" :disabled="optionDisabled(option)" :class="{ active: optionSelected(option, modelValue) }" @click="emit('update:modelValue', optionValue(option))">{{ optionLabel(option) }}</button>
  </div></div>
</template>
