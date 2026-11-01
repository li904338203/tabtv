<script setup lang="ts">
/**
 * 历史版本（SCT-20 顶栏「历史」）：服务端在 AI 剪辑、追加指令、方案切换、片段回填、替换素材、识别字幕、翻译、恢复之前
 * 各存一份快照（每条时间线最多 20 份）。恢复 = 回到那一步之前；恢复本身也会先存一份，所以恢复之后还能再恢复回来。
 *
 * ⚠️ 恢复会覆盖当前内容：先确认；有还没存上的改动时 store 会先保存（flush）。
 * ⚠️ 快照引用的素材已被删除时照常恢复，并说出「第 N 段缺素材」（SC-F-146）。
 * 「撤销这轮 AI」放在最上面（SC-AC-33）：刷新之后结果卡没了，这里按快照列表找到最近一次 AI 写入；
 * 点了交给剪辑台（`undo-ai`），与结果卡上的同一个按钮走同一条路（之后有手动改动先确认）。
 */
import { ElMessage, ElMessageBox } from "element-plus";
import { SmartcutFlushError } from "~/stores/smartcut";
import { clockTime } from "~/utils/canvasConflict";
import { smartcutMissingText, smartcutReadonlyText, smartcutSnapshotLabel } from "~/utils/smartcutStudio";

const emit = defineEmits<{ close: []; "undo-ai": [] }>();
const store = useSmartcutStore();
const busy = ref("");
const loading = ref(false);

onMounted(async () => {
  loading.value = true;
  try {
    await store.refreshSnapshots();
  } catch {
    ElMessage.warning("历史版本取不到，请稍后再试");
  } finally {
    loading.value = false;
  }
});

function dateText(iso: string): string {
  const date = new Date(iso);
  if (Number.isNaN(date.getTime())) return "";
  const today = new Date();
  const sameDay = date.toDateString() === today.toDateString();
  return sameDay ? clockTime(iso) : `${date.getMonth() + 1}月${date.getDate()}日 ${clockTime(iso)}`;
}

async function restore(id: string, label: string) {
  if (store.readonly) return;
  try {
    await ElMessageBox.confirm(`时间线会回到「${label}」的样子，之后的改动会被撤掉（恢复前会再存一份，可以恢复回来）。是否继续？`, "恢复历史版本", {
      confirmButtonText: "恢复",
      cancelButtonText: "取消"
    });
  } catch {
    return;
  }
  busy.value = id;
  try {
    const missing = await store.restoreSnapshot(id);
    const note = smartcutMissingText(store.timeline, missing);
    if (note) ElMessage.warning(note);
    else ElMessage.success("已恢复");
  } catch (cause) {
    ElMessage.error(cause instanceof SmartcutFlushError || cause instanceof Error ? cause.message || "恢复失败" : "恢复失败");
  } finally {
    busy.value = "";
  }
}
</script>

<template>
  <section class="sc-panel sc-history" aria-label="历史版本">
    <div class="sc-section">
      <h3>历史版本 <button type="button" class="sc-btn icon" title="关闭历史版本" aria-label="关闭历史版本" @click="emit('close')">×</button></h3>
      <p class="sc-note">AI 剪辑、恢复等操作之前自动存下的版本，最多保留 20 份。</p>
      <p v-if="store.readonly" class="sc-note">{{ smartcutReadonlyText(store.readonlyReason) }}</p>
    </div>
    <div v-if="store.aiRound" class="sc-section sc-history-ai">
      <button
        type="button"
        class="sc-btn"
        :disabled="store.readonly || Boolean(busy) || Boolean(store.planRunning)"
        :title="store.readonly ? smartcutReadonlyText(store.readonlyReason) : store.planRunning ? 'AI 剪辑进行中，请等它完成' : '回到最近一轮 AI 写入之前'"
        data-testid="smartcut-history-undo-ai"
        @click="emit('undo-ai')"
      >撤销这轮 AI</button>
      <p v-if="store.aiRound.editedAfter" class="sc-note">这轮 AI 之后你又改过，撤销前会先确认。</p>
    </div>
    <p v-if="loading && !store.snapshots.length" class="sc-note">载入中…</p>
    <p v-else-if="!store.snapshots.length" class="sc-note">还没有历史版本：AI 剪辑之后这里会出现「AI 剪辑之前」。</p>
    <ul v-else class="sc-history-list">
      <li v-for="snap in store.snapshots" :key="snap.id">
        <div>
          <strong>{{ smartcutSnapshotLabel(snap.reason) }}</strong>
          <span>v{{ snap.version }} · {{ dateText(snap.createdAt) }}{{ snap.createdByName ? ` · ${snap.createdByName}` : "" }}</span>
        </div>
        <button
          type="button"
          class="sc-btn"
          :disabled="store.readonly || Boolean(busy)"
          :title="store.readonly ? smartcutReadonlyText(store.readonlyReason) : '恢复到这个版本'"
          :aria-label="`恢复到「${smartcutSnapshotLabel(snap.reason)}」`"
          @click="restore(snap.id, smartcutSnapshotLabel(snap.reason))"
        >
          {{ busy === snap.id ? "恢复中…" : "恢复" }}
        </button>
      </li>
    </ul>
  </section>
</template>

<style scoped>
.sc-history-ai { justify-items: start; }
.sc-history-list { display: grid; gap: 6px; margin: 0; padding: 0; list-style: none; }
.sc-history-list li { display: flex; align-items: center; justify-content: space-between; gap: 8px; padding: 8px; border: 1px solid var(--portal-line); border-radius: 6px; }
.sc-history-list div { display: grid; gap: 2px; min-width: 0; }
.sc-history-list span { color: var(--portal-muted); font-size: 11px; }
</style>
