<script setup lang="ts">
/**
 * 行内控件有两种形态，**由配置决定**：
 *  - `config.tags` 有值 → 一排快捷标签（老后端的用法）
 *  - `config.type` 有值 → 一个输入框（新后端的 `seed` 是 `{type:"number", clearable:true}`）
 *
 * ⚠️ 只实现前一种的表现是：`seed` 渲染出一个光秃秃的"随机种子"四个字，
 *    没有任何可点的东西 —— 看起来像样式坏了，其实是控件没实现那一半。
 */
const props = defineProps<{ modelValue?: unknown; config: Record<string, unknown>; name: string }>();
const emit = defineEmits<{ "update:modelValue": [value: unknown] }>();
const tags = computed(() => Array.isArray(props.config.tags) ? props.config.tags : []);
const inputType = computed(() => typeof props.config.type === "string" ? props.config.type : "");
const clearable = computed(() => Boolean(props.config.clearable) && String(props.modelValue ?? "") !== "");

function onInput(event: Event) {
  const raw = (event.target as HTMLInputElement).value;
  // ⚠️ 空串的语义是"没填"不是 0：发 0 出去会让上游拿到一个固定种子，
  //    用户每次生成都得到同一张图，而他以为自己什么都没设。
  if (raw === "") return emit("update:modelValue", undefined);
  emit("update:modelValue", inputType.value === "number" ? Number(raw) : raw);
}
</script>

<template>
  <div class="canvas-control control-inline">
    <span>{{ name }}</span>
    <template v-if="tags.length">
      <button v-for="tag in tags" :key="String(tag)" type="button" @click="emit('update:modelValue', tag)">{{ tag }}</button>
    </template>
    <template v-else-if="inputType">
      <input
        :type="inputType"
        :placeholder="String(config.placeholder ?? '随机')"
        :value="modelValue ?? ''"
        @input="onInput"
      >
      <button v-if="clearable" type="button" title="清空" @click="emit('update:modelValue', undefined)">×</button>
    </template>
  </div>
</template>
