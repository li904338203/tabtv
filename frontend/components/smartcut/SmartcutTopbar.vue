<script setup lang="ts">
/**
 * 剪辑台顶栏（SCT-20，dev/03 §5.1）：标题、画幅与规格、保存状态、撤销 / 重做、历史、收起、导出 ▾、关闭。
 *
 * ⚠️ 置灰的按钮要说清为什么（title + aria-label），只读成员「点不动并知道为什么」（SC-AC-66）。
 * ⚠️ 导出入口由导出面板（SCT-24）放进 `export` 插槽；收起与关闭都要先把改动存上（外壳负责）。
 */
import { ChevronDown, CircleAlert, CloudCheck, History, LoaderCircle, Minimize2, Redo2, Undo2, WifiOff, X } from "@lucide/vue";
import { smartcutHistoryDisabledReason, smartcutReadonlyText, smartcutSaveStateView } from "~/utils/smartcutStudio";

defineProps<{ title: string; closing?: boolean; historyOpen?: boolean; smallScreen?: boolean }>();
const emit = defineEmits<{ close: []; collapse: []; "show-bar": []; "toggle-history": [] }>();
const store = useSmartcutStore();

const saveView = computed(() => smartcutSaveStateView(store.saveState));
const undoReason = computed(() => smartcutHistoryDisabledReason({ readonlyReason: store.readonlyReason, available: store.undoStack.length > 0, kind: "undo" }));
const redoReason = computed(() => smartcutHistoryDisabledReason({ readonlyReason: store.readonlyReason, available: store.redoStack.length > 0, kind: "redo" }));
const readonlyText = computed(() => smartcutReadonlyText(store.readonlyReason));
const spec = computed(() => {
  const t = store.timeline;
  return t ? `${t.aspectRatio === "custom" ? "自定义" : t.aspectRatio} · ${t.width}×${t.height} · ${t.fps}fps` : "";
});
</script>

<template>
  <header class="sc-topbar">
    <strong class="sc-topbar-title" :title="title">{{ title }}</strong>
    <span v-if="spec" class="sc-tag" title="画幅、尺寸与帧率">{{ spec }}</span>
    <button
      type="button"
      class="sc-topbar-save"
      :class="`tone-${saveView.tone}`"
      :data-save-state="store.saveState"
      :disabled="!saveView.actionable"
      :title="saveView.actionable ? '查看详情与出路' : saveView.label"
      :aria-label="`保存状态：${saveView.label}`"
      @click="emit('show-bar')"
    >
      <LoaderCircle v-if="saveView.tone === 'busy'" :size="13" class="sc-spin" />
      <WifiOff v-else-if="store.saveState === 'offline'" :size="13" />
      <CircleAlert v-else-if="saveView.tone === 'alert'" :size="13" />
      <CloudCheck v-else :size="13" />
      {{ saveView.label }}
    </button>
    <span v-if="readonlyText" class="sc-topbar-readonly" role="status" data-testid="smartcut-readonly">{{ readonlyText }}</span>
    <span v-else-if="smallScreen" class="sc-topbar-readonly" role="status">屏幕太窄，只能查看</span>
    <div class="sc-topbar-history">
      <button type="button" class="sc-btn icon" :disabled="Boolean(undoReason)" :title="undoReason || '撤销（⌘Z）'" :aria-label="undoReason || '撤销'" @click="store.undo()"><Undo2 :size="15" /></button>
      <button type="button" class="sc-btn icon" :disabled="Boolean(redoReason)" :title="redoReason || '重做（⇧⌘Z）'" :aria-label="redoReason || '重做'" @click="store.redo()"><Redo2 :size="15" /></button>
      <button
        type="button"
        class="sc-btn"
        :class="{ active: historyOpen }"
        title="历史版本：AI 剪辑、恢复之前自动存下的快照"
        aria-label="历史版本"
        :aria-pressed="historyOpen"
        @click="emit('toggle-history')"
      >
        <History :size="14" />历史
      </button>
    </div>
    <div class="sc-topbar-actions">
      <slot name="export">
        <button type="button" class="sc-btn" disabled title="导出面板加载中" aria-label="导出"><ChevronDown :size="14" />导出</button>
      </slot>
      <button type="button" class="sc-btn" :disabled="closing" title="收起剪辑台（保留编辑状态，再打开秒开）" aria-label="收起剪辑台" @click="emit('collapse')">
        <Minimize2 :size="14" />收起
      </button>
      <button type="button" class="sc-btn icon" :disabled="closing" title="保存并关闭剪辑台" aria-label="关闭剪辑台" @click="emit('close')">
        <LoaderCircle v-if="closing" :size="14" class="sc-spin" /><X v-else :size="15" />
      </button>
    </div>
  </header>
</template>

<style scoped>
.sc-topbar {
  display: flex;
  align-items: center;
  gap: 10px;
  min-width: 0;
  height: 48px;
  padding: 0 12px;
  border-bottom: 1px solid var(--portal-line);
  color: var(--portal-text);
  background: var(--portal-topbar-bg);
}
.sc-topbar-title { min-width: 0; max-width: 280px; overflow: hidden; font-size: 14px; text-overflow: ellipsis; white-space: nowrap; }
.sc-topbar-save {
  display: inline-flex;
  align-items: center;
  gap: 4px;
  height: 24px;
  padding: 0 9px;
  border: 1px solid transparent;
  border-radius: 6px;
  color: var(--portal-muted);
  background: transparent;
  font-size: 12px;
  cursor: pointer;
}
.sc-topbar-save:disabled { cursor: default; }
.sc-topbar-save.tone-alert { border-color: var(--portal-danger-line); color: var(--portal-danger); background: var(--portal-danger-soft); }
.sc-topbar-readonly { padding: 2px 8px; border-radius: 999px; color: var(--portal-text-soft); background: var(--portal-surface-2); font-size: 12px; }
.sc-topbar-history { display: flex; gap: 4px; margin-left: auto; }
.sc-topbar-actions { display: flex; align-items: center; gap: 6px; }
</style>
