<script setup lang="ts">
/**
 * 助手文本（设计 03 §3.2）：左对齐、无气泡、Markdown（只经 MarkdownView —— 全站唯一的 v-html）。
 * 流式输出时末尾光标闪烁；被停止的那一轮保留已输出的文字并标「已停止」。
 */
import { Copy } from "@lucide/vue";
import { ElMessage } from "element-plus";
import MarkdownView from "~/components/common/MarkdownView.vue";

const props = defineProps<{ text: string; streaming?: boolean; stopped?: boolean }>();

async function copy() {
  try {
    await navigator.clipboard.writeText(props.text);
    ElMessage.success("已复制");
  } catch {
    ElMessage.warning("复制没有成功，请手动选择文字");
  }
}
</script>

<template>
  <div class="agent-msg agent-msg-assistant" :class="{ 'is-streaming': streaming }">
    <MarkdownView :source="text" />
    <span v-if="streaming" class="agent-cursor" aria-hidden="true" />
    <footer v-if="!streaming" class="agent-msg-meta">
      <span v-if="stopped" class="agent-stopped-tag">已停止</span>
      <button type="button" @click="copy"><Copy :size="12" />复制</button>
    </footer>
  </div>
</template>
