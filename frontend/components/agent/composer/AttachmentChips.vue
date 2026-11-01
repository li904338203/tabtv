<script setup lang="ts">
/**
 * 附件与引用胶囊（设计 02 §4.1：有内容时才出现）。上传中显示进度态，失败可重试，可移除。
 *
 * 缩略图是**本地文件**的 object URL（选文件时就有），不是受保护端点 —— 不用为显示一张刚选的图
 * 再去请求一次服务端。object URL 由父组件在移除 / 卸载时 revoke。
 *
 * 引用胶囊（AGT-29，`refs`）：画布节点 / 素材 / 角色资产。缩略图由父组件按媒体 ID 经
 * `useAuthenticatedMedia` 取成 object URL 后传进来（`thumbs`），这里不碰受保护端点。
 * 「选中即引用」自动加入的胶囊用虚线框区分 —— 取消选中时它会自己消失，用户要能看出来为什么。
 */
import { Box, FileText, Image as ImageIcon, LoaderCircle, RotateCw, TriangleAlert, UserRound, X } from "@lucide/vue";
import type { ComposerRef } from "~/utils/composerRefs";

export type ComposerAttachment = {
  key: string;
  name: string;
  status: "uploading" | "done" | "failed";
  referenceId?: string;
  previewUrl?: string;
  error?: string;
};

withDefaults(defineProps<{
  items: ComposerAttachment[];
  disabled?: boolean;
  refs?: ComposerRef[];
  /** 引用胶囊的缩略图：媒体 ID → object URL。 */
  thumbs?: Record<string, string>;
}>(), { refs: () => [], thumbs: () => ({}) });
const emit = defineEmits<{
  remove: [key: string];
  retry: [key: string];
  "remove-ref": [ref: ComposerRef];
  "open-ref": [ref: ComposerRef];
}>();

const REF_KIND_LABELS: Record<ComposerRef["kind"], string> = { node: "画布节点", media: "素材", asset: "角色资产" };
</script>

<template>
  <ul v-if="items.length || refs.length" class="attachment-chips" aria-label="附件与引用">
    <li
      v-for="entry in refs"
      :key="`${entry.kind}:${entry.id}`"
      class="attachment-chip ref-chip"
      :class="{ auto: entry.auto }"
      :title="`${REF_KIND_LABELS[entry.kind]}：${entry.label}${entry.auto ? '（画布选中，自动引用）' : ''}`"
    >
      <button type="button" class="ref-open" @click="emit('open-ref', entry)">
        <img v-if="entry.thumbMediaId && thumbs[entry.thumbMediaId]" :src="thumbs[entry.thumbMediaId]" :alt="entry.label">
        <UserRound v-else-if="entry.kind === 'asset'" :size="14" />
        <FileText v-else-if="entry.hint === 'text' || entry.hint === 'document'" :size="14" />
        <ImageIcon v-else-if="entry.kind === 'media'" :size="14" />
        <Box v-else :size="14" />
        <span class="attachment-name">{{ entry.label }}</span>
      </button>
      <button type="button" class="attachment-action" title="移除引用" :disabled="disabled" @click="emit('remove-ref', entry)"><X :size="13" /></button>
    </li>
    <li v-for="(item, index) in items" :key="item.key" class="attachment-chip" :class="item.status" :title="item.error || item.name">
      <span class="attachment-order">{{ index + 1 }}</span>
      <img v-if="item.previewUrl" :src="item.previewUrl" :alt="item.name">
      <ImageIcon v-else :size="14" />
      <span class="attachment-name">{{ item.name }}</span>
      <LoaderCircle v-if="item.status === 'uploading'" :size="13" class="spin" aria-label="上传中" />
      <button v-else-if="item.status === 'failed'" type="button" class="attachment-action" title="重新上传" :disabled="disabled" @click="emit('retry', item.key)">
        <TriangleAlert :size="13" /><RotateCw :size="12" />
      </button>
      <button type="button" class="attachment-action" title="移除" :disabled="disabled" @click="emit('remove', item.key)"><X :size="13" /></button>
    </li>
  </ul>
</template>

<style scoped>
.attachment-chips { display: flex; flex-wrap: wrap; gap: 8px; margin: 10px 0 0; padding: 0; list-style: none; }
.attachment-chip {
  max-width: 220px;
  height: 32px;
  display: inline-flex;
  align-items: center;
  gap: 6px;
  border: 1px solid var(--portal-line);
  border-radius: 7px;
  padding: 0 4px 0 4px;
  color: var(--portal-text-soft);
  background: var(--portal-surface-2);
  font-size: 12px;
}
.attachment-chip.auto { border-style: dashed; }
.ref-open {
  min-width: 0;
  height: 100%;
  display: inline-flex;
  align-items: center;
  gap: 6px;
  border: 0;
  padding: 0;
  color: inherit;
  background: transparent;
  font: inherit;
  cursor: pointer;
}
.attachment-chip.failed { border-color: var(--portal-danger-line); color: var(--portal-danger); }
.attachment-chip img { width: 24px; height: 24px; border-radius: 4px; object-fit: cover; }
.attachment-order {
  min-width: 16px;
  height: 16px;
  display: grid;
  place-items: center;
  border-radius: 4px;
  color: var(--portal-mark-text);
  background: var(--portal-mark-bg);
  font-size: 10px;
}
.attachment-name { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.attachment-action {
  height: 24px;
  display: inline-flex;
  align-items: center;
  gap: 2px;
  border: 0;
  border-radius: 5px;
  padding: 0 4px;
  color: inherit;
  background: transparent;
  cursor: pointer;
}
.attachment-action:hover:not(:disabled) { background: var(--portal-hover); }
.attachment-action:disabled { cursor: not-allowed; opacity: .5; }
</style>
