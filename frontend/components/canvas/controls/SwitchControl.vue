<script setup lang="ts">
defineProps<{ modelValue?: unknown; config: Record<string, unknown>; name: string }>();
const emit = defineEmits<{ "update:modelValue": [value: unknown] }>();
</script>

<template><label class="canvas-control control-switch"><span>{{ name }}</span><input type="checkbox" :checked="Boolean(modelValue)" @change="emit('update:modelValue', ($event.target as HTMLInputElement).checked)"><i /></label></template>
