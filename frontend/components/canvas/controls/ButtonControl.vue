<script setup lang="ts">
import { Library } from "@lucide/vue";
const props = defineProps<{ modelValue?: unknown; config: Record<string, unknown>; name: string }>();
const emit = defineEmits<{
  "update:modelValue": [value: unknown];
  action: [value: string];
}>();
const label = computed(() => typeof props.modelValue === "string" && props.modelValue ? props.modelValue : props.name);
function activate() {
  const action = typeof props.config.action === "string" ? props.config.action : "";
  if (action) emit("action", action);
  else emit("update:modelValue", props.modelValue ?? true);
}
</script>

<template><button class="canvas-control control-button" type="button" @click="activate"><Library :size="14" /><span>{{ label }}</span></button></template>
