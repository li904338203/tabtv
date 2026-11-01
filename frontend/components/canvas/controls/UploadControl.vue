<script setup lang="ts">
import { Upload } from "@lucide/vue";
defineProps<{ modelValue?: unknown; config: Record<string, unknown>; name: string }>();
const emit = defineEmits<{ "update:modelValue": [value: unknown] }>();
function pick(event: Event) { const file = (event.target as HTMLInputElement).files?.[0]; if (file) emit("update:modelValue", { name: file.name, size: file.size }); }
</script>

<template><label class="canvas-control control-button"><Upload :size="14" />{{ name }}<input type="file" :accept="String(config.accept ?? '*/*')" @change="pick"></label></template>
