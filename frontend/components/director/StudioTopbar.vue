<script setup lang="ts">
/**
 * 编辑器顶栏（DRT-15）：返回画布、场景名（= 节点标题，只读显示）、保存状态、撤销 / 重做、许可链接，
 * 以及给各卡片挂按钮的 `actions` 插槽（截图 DRT-17、AI 搭景 DRT-20、导出预演 DRT-25）。
 *
 * ⚠️ 不放占位按钮：点了没反应的按钮就是「选了等于没选」。各功能由各自的卡片放进插槽。
 * ⚠️ 置灰的按钮要说清为什么（title + aria-label），只读成员「点不动并知道为什么」（DR-F-156）。
 */
import { ChevronLeft, CircleAlert, CloudCheck, LoaderCircle, Redo2, Undo2, WifiOff } from "@lucide/vue";
import { directorHistoryDisabledReason, directorReadonlyText, directorSaveStateView } from "~/utils/directorStudio";

defineProps<{ title: string; closing?: boolean }>();
const emit = defineEmits<{ back: []; "show-bar": [] }>();
const store = useDirectorStore();

const saveView = computed(() => directorSaveStateView(store.saveState));
const undoReason = computed(() => directorHistoryDisabledReason({
  readonlyReason: store.readonlyReason, proposal: Boolean(store.proposal), available: store.undoStack.length > 0, kind: "undo"
}));
const redoReason = computed(() => directorHistoryDisabledReason({
  readonlyReason: store.readonlyReason, proposal: Boolean(store.proposal), available: store.redoStack.length > 0, kind: "redo"
}));
const readonlyText = computed(() => directorReadonlyText(store.readonlyReason));
</script>

<template>
  <header class="studio-topbar">
    <button type="button" class="studio-back" title="保存并返回画布" aria-label="返回画布" :disabled="closing" @click="emit('back')">
      <LoaderCircle v-if="closing" :size="15" class="spin" /><ChevronLeft v-else :size="16" />返回画布
    </button>
    <strong class="studio-title" :title="title">{{ title }}</strong>
    <button
      type="button"
      class="studio-save"
      :class="`tone-${saveView.tone}`"
      :data-save-state="store.saveState"
      :disabled="!saveView.actionable"
      :title="saveView.actionable ? '查看详情与出路' : saveView.label"
      :aria-label="`保存状态：${saveView.label}`"
      @click="emit('show-bar')"
    >
      <LoaderCircle v-if="saveView.tone === 'busy'" :size="13" class="spin" />
      <WifiOff v-else-if="store.saveState === 'offline'" :size="13" />
      <CircleAlert v-else-if="saveView.tone === 'alert'" :size="13" />
      <CloudCheck v-else :size="13" />
      {{ saveView.label }}
    </button>
    <span v-if="readonlyText" class="studio-readonly" role="status">{{ readonlyText }}</span>
    <div class="studio-history">
      <button type="button" :disabled="Boolean(undoReason)" :title="undoReason || '撤销（⌘Z）'" :aria-label="undoReason || '撤销'" @click="store.undo()"><Undo2 :size="16" /></button>
      <button type="button" :disabled="Boolean(redoReason)" :title="redoReason || '重做（⇧⌘Z）'" :aria-label="redoReason || '重做'" @click="store.redo()"><Redo2 :size="16" /></button>
    </div>
    <div class="studio-actions">
      <slot name="actions" />
    </div>
    <a class="studio-licenses" href="/licenses" target="_blank" rel="noopener" title="第三方素材与代码的许可" aria-label="许可">许可</a>
  </header>
</template>

<style scoped>
.studio-topbar {
  display: flex;
  align-items: center;
  gap: 12px;
  min-width: 0;
  padding: 0 12px;
  border-bottom: 1px solid var(--portal-line);
  color: var(--portal-text);
  background: var(--portal-topbar-bg);
}
.studio-topbar button {
  display: inline-flex;
  align-items: center;
  gap: 4px;
  border: 1px solid var(--portal-line);
  border-radius: 6px;
  color: var(--portal-text);
  background: transparent;
  font-size: 12px;
  cursor: pointer;
}
.studio-topbar button:hover:not(:disabled) { background: var(--portal-hover); }
.studio-topbar button:disabled { cursor: not-allowed; opacity: 0.5; }
.studio-back { height: 30px; padding: 0 10px; }
.studio-title { min-width: 0; overflow: hidden; font-size: 14px; text-overflow: ellipsis; white-space: nowrap; }
.studio-topbar .studio-save { height: 26px; padding: 0 9px; border-color: transparent; color: var(--portal-muted); }
.studio-topbar .studio-save:disabled { opacity: 1; cursor: default; }
.studio-topbar .studio-save.tone-alert { border-color: var(--portal-danger-line); color: var(--portal-danger); background: var(--portal-danger-soft); }
.studio-readonly { padding: 2px 8px; border-radius: 999px; color: var(--portal-text-soft); background: var(--portal-surface-2); font-size: 12px; }
.studio-history { display: flex; gap: 4px; margin-left: auto; }
.studio-history button { width: 30px; height: 30px; justify-content: center; }
.studio-actions { display: flex; align-items: center; gap: 8px; }
.studio-licenses { color: var(--portal-muted); font-size: 12px; text-decoration: none; }
.studio-licenses:hover { color: var(--portal-text); text-decoration: underline; }
.spin { animation: studio-spin 0.9s linear infinite; }
@keyframes studio-spin { to { transform: rotate(360deg); } }
@media (prefers-reduced-motion: reduce) { .spin { animation: none; } }
</style>
