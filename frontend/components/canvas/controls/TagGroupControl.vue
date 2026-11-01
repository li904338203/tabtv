<script setup lang="ts">
import { controlOptions, optionDisabled, optionLabel, optionSelected, optionValue } from "~/utils/controlOptions";
const props = defineProps<{ modelValue?: unknown; config: Record<string, unknown>; name: string }>();
const emit = defineEmits<{ "update:modelValue": [value: unknown] }>();
const options = computed(() => controlOptions(props.config));
const hidden = computed(() => !options.value.length && Boolean(props.config.domainFrom));
</script>

<template>
  <div v-if="!hidden" class="canvas-control control-stack control-tags"><span>{{ name }}</span><div>
    <button v-for="option in options" :key="optionValue(option)" type="button" :disabled="optionDisabled(option)" :class="{ active: optionSelected(option, modelValue) }" @click="emit('update:modelValue', optionValue(option))">{{ optionLabel(option) }}</button>
  </div></div>
</template>
