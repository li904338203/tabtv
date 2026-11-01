<script setup lang="ts">
defineProps<{ modelValue?: unknown; config: Record<string, unknown>; name: string }>();
const emit = defineEmits<{ "update:modelValue": [value: unknown] }>();
</script>

<template>
  <label class="canvas-control control-stack"><span>{{ name }} <b>{{ modelValue ?? config.default }}</b></span><input type="range" :min="Number(config.min ?? 0)" :max="Number(config.max ?? 100)" :step="Number(config.step ?? 1)" :value="Number(modelValue ?? config.default ?? 0)" @input="emit('update:modelValue', Number(($event.target as HTMLInputElement).value))"></label>
</template>
