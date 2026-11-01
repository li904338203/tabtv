<script setup lang="ts">
import { CONTROL_MAP } from "./controls/controlMap";
const props = defineProps<{ control: string; name: string; config: Record<string, unknown>; modelValue?: unknown }>();
const emit = defineEmits<{
  "update:modelValue": [value: unknown];
  action: [value: string];
}>();
const implementation = computed(() => CONTROL_MAP[props.control]);
watchEffect(() => { if (!implementation.value) console.warn(`[canvas] unknown control skipped: ${props.control}`); });
</script>

<template><component :is="implementation" v-if="implementation" :name="name" :config="config" :model-value="modelValue" @update:model-value="emit('update:modelValue', $event)" @action="emit('action', $event)" /></template>
