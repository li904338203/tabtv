<script setup lang="ts">
/**
 * 全站唯一的 Markdown 展示组件（AGT-08）。文档节点、Agent 消息都只用它。
 *
 * 渲染与清洗都在 `utils/markdown.ts`（markdown-it `html:false` + DOMPurify 白名单），
 * 这里只负责把结果放进 DOM 与排版。⚠️ 全站 `v-html` 只允许出现在本文件
 * （ESLint `vue/no-v-html: error`），别处要显示 Markdown 一律用这个组件。
 *
 * `maxLines` 用 CSS line-clamp 截断，而不是在字符串层截：按字符截会切出半个标签，
 * 清洗后的结构就不再是作者写的那样。
 */
import { renderMarkdown } from "~/utils/markdown";

const props = defineProps<{
  source: string;
  /** 最多显示几行，超出以省略号收尾；不传或 ≤0 表示不截断。 */
  maxLines?: number;
}>();

const html = computed(() => renderMarkdown(props.source ?? ""));
const clamped = computed(() => (props.maxLines ?? 0) > 0);
const clampStyle = computed(() => (clamped.value ? { "--md-max-lines": String(props.maxLines) } : undefined));
</script>

<template>
  <!-- eslint-disable-next-line vue/no-v-html -- 唯一豁免：内容已经 utils/markdown.ts 的 html:false + DOMPurify 白名单两层清洗 -->
  <div class="markdown-view" :class="{ 'is-clamped': clamped }" :style="clampStyle" v-html="html" />
</template>

<style scoped>
.markdown-view {
  color: var(--portal-text);
  font-size: var(--fs-14);
  line-height: 1.65;
  overflow-wrap: anywhere;
  word-break: break-word;
}

.markdown-view.is-clamped {
  display: -webkit-box;
  -webkit-box-orient: vertical;
  -webkit-line-clamp: var(--md-max-lines, 6);
  line-clamp: var(--md-max-lines, 6);
  overflow: hidden;
}

.markdown-view > :deep(:first-child) {
  margin-top: 0;
}

.markdown-view > :deep(:last-child) {
  margin-bottom: 0;
}

.markdown-view :deep(p),
.markdown-view :deep(ul),
.markdown-view :deep(ol),
.markdown-view :deep(blockquote),
.markdown-view :deep(pre),
.markdown-view :deep(table) {
  margin: 0 0 var(--sp-2);
}

.markdown-view :deep(h1),
.markdown-view :deep(h2),
.markdown-view :deep(h3),
.markdown-view :deep(h4),
.markdown-view :deep(h5),
.markdown-view :deep(h6) {
  margin: var(--sp-3) 0 var(--sp-2);
  color: var(--portal-text);
  font-weight: 600;
  line-height: 1.35;
}

.markdown-view :deep(h1) { font-size: var(--fs-20); }
.markdown-view :deep(h2) { font-size: var(--fs-18); }
.markdown-view :deep(h3) { font-size: var(--fs-16); }
.markdown-view :deep(h4),
.markdown-view :deep(h5),
.markdown-view :deep(h6) { font-size: var(--fs-14); }

.markdown-view :deep(ul),
.markdown-view :deep(ol) {
  padding-left: 1.4em;
}

.markdown-view :deep(li + li) {
  margin-top: var(--sp-1);
}

.markdown-view :deep(a) {
  color: var(--brand-primary);
  text-decoration: underline;
  text-underline-offset: 2px;
  word-break: break-all;
}

.markdown-view :deep(blockquote) {
  padding: var(--sp-1) var(--sp-3);
  border-left: 3px solid var(--portal-line-strong);
  color: var(--portal-muted);
}

.markdown-view :deep(code) {
  padding: 0.1em 0.35em;
  border-radius: var(--radius-sm);
  background: var(--portal-code-bg);
  font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
  font-size: 0.92em;
}

.markdown-view :deep(pre) {
  padding: var(--sp-2) var(--sp-3);
  border-radius: var(--radius-md);
  background: var(--portal-code-bg);
  overflow-x: auto;
}

.markdown-view :deep(pre code) {
  padding: 0;
  background: none;
  white-space: pre;
}

.markdown-view :deep(hr) {
  margin: var(--sp-3) 0;
  border: 0;
  border-top: 1px solid var(--portal-line);
}

.markdown-view :deep(table) {
  border-collapse: collapse;
  display: block;
  max-width: 100%;
  overflow-x: auto;
}

.markdown-view :deep(th),
.markdown-view :deep(td) {
  padding: var(--sp-1) var(--sp-2);
  border: 1px solid var(--portal-line);
}

.markdown-view :deep(th) {
  background: var(--portal-surface-2);
  font-weight: 600;
}
</style>
