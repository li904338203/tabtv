<script setup lang="ts">
/**
 * 项目卡片的标题：平时是 h3，改名时就地变成输入框 —— 三个页面共用。
 *
 * 用输入框而不是弹窗：改个名字弹一个模态框太重，而且弹窗会盖住列表，
 * 用户看不到自己正在改的是哪一张卡。
 *
 * ⚠️ 渲染的仍是普通 `<h3>`，所以各页面原有的 `.xxx-card h3` 样式照常生效，
 *    卡片的版式一个像素都不变。
 */
import { PROJECT_NAME_MAX } from "~/composables/useProjectActions";

const props = defineProps<{
  name: string;
  editing: boolean;
  /** 首页卡片的标题是 13px（全部项目是 14px），输入框跟着缩一号，免得改名时整行跳动。 */
  compact?: boolean;
}>();
const emit = defineEmits<{
  "update:editing": [value: boolean];
  commit: [value: string];
}>();

const draft = ref("");
const input = ref<HTMLInputElement>();
/**
 * ⚠️ 回车提交之后输入框被卸载，部分浏览器还会再补一个 blur ——
 *    不挡一下就会提交两次，第二次拿着同一个名字再发一遍 PATCH。
 */
let settled = true;

watch(() => props.editing, async (editing) => {
  if (!editing) return;
  settled = false;
  draft.value = props.name;
  await nextTick();
  // 全选而不是把光标放末尾：默认名「未命名」几乎总是要整个替换掉
  input.value?.focus();
  input.value?.select();
}, { immediate: true });

function commit() {
  if (settled) return;
  settled = true;
  emit("update:editing", false);
  emit("commit", draft.value);
}
function cancel() {
  settled = true;
  emit("update:editing", false);
}
</script>

<template>
  <input
    v-if="editing"
    ref="input"
    v-model="draft"
    class="library-name-input"
    :class="{ compact }"
    :maxlength="PROJECT_NAME_MAX"
    aria-label="项目名称"
    @click.stop
    @keyup.enter="commit"
    @keyup.esc="cancel"
    @blur="commit"
  >
  <h3 v-else title="双击可重命名" @dblclick.stop="emit('update:editing', true)">{{ name }}</h3>
</template>
