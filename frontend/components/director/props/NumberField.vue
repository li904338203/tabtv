<script setup lang="ts">
/**
 * 数值输入（DRT-15）：**失焦或回车时提交一次**，不在每次 input 事件提交 ——
 * 否则撤销栈被打满、自动保存一路触发（DRD-36）。超出范围的值夹到 `[min, max]`；不是数字时恢复原值。
 * Esc 放弃这次输入（恢复原值并失焦），不冒泡成编辑器的「取消」。
 */
const props = withDefaults(defineProps<{
  modelValue: number;
  min?: number;
  max?: number;
  /** 显示时保留几位小数。 */
  digits?: number;
  disabled?: boolean;
  label: string;
}>(), { min: Number.NEGATIVE_INFINITY, max: Number.POSITIVE_INFINITY, digits: 3, disabled: false });
const emit = defineEmits<{ commit: [value: number] }>();

const format = (value: number) => (Number.isFinite(value) ? String(Number(value.toFixed(props.digits))) : "");
const draft = ref(format(props.modelValue));
watch(() => props.modelValue, (value) => { draft.value = format(value); });

function commit() {
  const parsed = Number(draft.value.trim());
  if (draft.value.trim() === "" || !Number.isFinite(parsed)) {
    draft.value = format(props.modelValue);
    return;
  }
  const value = Math.min(props.max, Math.max(props.min, parsed));
  draft.value = format(value);
  if (Math.abs(value - props.modelValue) <= 1e-9) return;
  emit("commit", value);
  // 上层可能不采纳这个值（缩短时长被拒、只读、上限……）：那时 modelValue 不变、watch 不触发，
  // 输入框会一直显示被拒的那个数而数据是原来的。提交后按 modelValue 对一次（被采纳时两者本来就相同）。
  void nextTick(() => {
    draft.value = format(props.modelValue);
  });
}

function cancel(event: KeyboardEvent) {
  draft.value = format(props.modelValue);
  (event.target as HTMLInputElement | null)?.blur();
}
</script>

<template>
  <input
    v-model="draft"
    class="dr-input"
    type="text"
    inputmode="decimal"
    :aria-label="label"
    :title="label"
    :disabled="disabled"
    @blur="commit"
    @keydown.enter.prevent="($event.target as HTMLInputElement).blur()"
    @keydown.esc.stop.prevent="cancel"
  >
</template>
