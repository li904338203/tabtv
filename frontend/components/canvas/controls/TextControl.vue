<script setup lang="ts">
/**
 * ⚠️ `control: "text"` 是**新后端才有的控件类型**（`negative_prompt` 用它），
 *    老前端的 `CONTROL_MAP` 里没有 —— 未知控件会被 `DynamicControl` 静默跳过
 *    （那是对的，配置可以比前端新），代价是**负向提示词完全没有入口**。
 */
const props = defineProps<{ modelValue?: unknown; config: Record<string, unknown>; name: string }>();
const emit = defineEmits<{ "update:modelValue": [value: unknown] }>();
const rows = computed(() => Math.max(1, Number(props.config.rows ?? 2)));
const maxLength = computed(() => {
  const raw = Number(props.config.maxLength);
  return Number.isFinite(raw) && raw > 0 ? raw : undefined;
});
</script>

<template>
  <label class="canvas-control control-stack control-text">
    <span>{{ name }}</span>
    <textarea
      :rows="rows"
      :maxlength="maxLength"
      :placeholder="String(config.placeholder ?? '')"
      :value="String(modelValue ?? '')"
      @input="emit('update:modelValue', ($event.target as HTMLTextAreaElement).value)"
    />
  </label>
</template>
