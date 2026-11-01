<script setup lang="ts">
/**
 * 剪辑台的提示条（SCT-20，照导演台 DirectorConflictBar）：冲突 / 离线 / 保存失败 / 有草稿，同一时刻只出一条，
 * 优先级照这个顺序。
 *
 * ⚠️ 冲突文案按冲突详情的来源分流（`bySelf` / `editorKind`，`smartcutConflictText`），不匹配 message；
 *    「以我为准」是真覆盖，不做成主按钮，按钮上写明覆盖的是谁（SC-AC-09）。
 * ⚠️ 任何出路都不静默丢弃用户的改动：store 在进冲突 / 失败 / 离线时已把本地时间线存成草稿。
 */
import { ElMessage, ElMessageBox } from "element-plus";
import { clockTime } from "~/utils/canvasConflict";
import { smartcutAdoptLabel, smartcutConflictText, smartcutOverwriteLabel } from "~/utils/smartcutPlan";

const open = defineModel<boolean>("open", { default: true });
const store = useSmartcutStore();

const kind = computed<"conflict" | "offline" | "error" | "draft" | null>(() => {
  if (store.saveState === "conflict") return "conflict";
  if (store.saveState === "offline") return "offline";
  if (store.saveState === "error") return "error";
  if (store.pendingDraft) return "draft";
  return null;
});
const conflictText = computed(() => smartcutConflictText(store.conflict));
const overwriteText = computed(() => smartcutOverwriteLabel(store.conflict));
const adoptText = computed(() => smartcutAdoptLabel(store.conflict));
const errorText = computed(() => {
  const message = store.lastError?.message || "保存失败";
  return store.lastError?.path ? `${message}（${store.lastError.path}）` : message;
});
const draftTime = computed(() => clockTime(store.pendingDraft?.savedAt ?? ""));

function failure(cause: unknown, fallback: string) {
  ElMessage.error(cause instanceof Error && cause.message ? cause.message : fallback);
}

async function adopt() {
  try {
    await store.resolveConflict("adopt");
  } catch (cause) {
    failure(cause, "载入最新版本失败，你的改动仍在剪辑台里");
  }
}

async function overwrite() {
  try {
    const result = await store.resolveConflict("overwrite");
    if (result === "conflict") ElMessage.warning("保存期间又有人修改了时间线，请重新选择");
    else ElMessage.success("已用你的版本覆盖");
  } catch (cause) {
    failure(cause, "覆盖保存失败，你的改动仍在剪辑台里");
  }
}

async function retry() {
  try {
    await store.retry();
  } catch (cause) {
    failure(cause, "重试失败");
  }
}

async function restoreDraft() {
  const warn = store.pendingDraft?.serverChanged
    ? "这份草稿之后时间线又被改过，恢复会覆盖那些改动（可以撤销）。是否继续？"
    : "恢复草稿会用你之前的版本替换当前时间线（可以撤销）。是否继续？";
  try {
    await ElMessageBox.confirm(warn, "恢复草稿", { confirmButtonText: "恢复", cancelButtonText: "取消" });
  } catch {
    return;
  }
  try {
    await store.restoreDraft();
  } catch (cause) {
    failure(cause, "恢复草稿失败");
  }
}

async function discardDraft() {
  try {
    await ElMessageBox.confirm("丢弃后这份本地草稿无法找回。是否继续？", "丢弃草稿", { confirmButtonText: "丢弃", cancelButtonText: "取消" });
  } catch {
    return;
  }
  store.discardDraft();
}
</script>

<template>
  <div v-if="kind && open" class="sc-bar" :class="`is-${kind}`" :role="kind === 'draft' ? 'status' : 'alert'" :data-bar="kind">
    <p v-if="kind === 'conflict'">
      <strong>{{ conflictText }}</strong>
      <span>你的改动没有丢，已存为本标签页的草稿。</span>
    </p>
    <p v-else-if="kind === 'offline'">
      <strong>已离线，恢复网络后会自动对账</strong>
      <span>离线期间的改动先存在本标签页，不会丢。</span>
    </p>
    <p v-else-if="kind === 'error'">
      <strong>保存失败：{{ errorText }}</strong>
      <span>你的改动仍在剪辑台里。</span>
    </p>
    <p v-else>
      <strong>发现未保存的草稿{{ draftTime ? `（${draftTime}）` : "" }}</strong>
      <span>{{ store.pendingDraft?.serverChanged ? "是上次没能存上的改动；之后时间线又被改过。" : "是上次没能存上的改动，可以恢复或丢弃。" }}</span>
    </p>
    <div class="sc-bar-actions">
      <template v-if="kind === 'conflict'">
        <button type="button" class="sc-btn primary" :disabled="store.resolving" @click="adopt">{{ adoptText }}</button>
        <button type="button" class="sc-btn danger" :disabled="store.resolving" @click="overwrite">{{ overwriteText }}</button>
      </template>
      <button v-else-if="kind === 'error' || kind === 'offline'" type="button" class="sc-btn primary" :disabled="store.readonly" @click="retry">重试</button>
      <template v-else-if="kind === 'draft'">
        <button type="button" class="sc-btn primary" :disabled="store.readonly" @click="restoreDraft">恢复</button>
        <button type="button" class="sc-btn" @click="discardDraft">丢弃</button>
      </template>
      <button type="button" class="sc-btn icon" title="收起提示" aria-label="收起提示" @click="open = false">×</button>
    </div>
  </div>
</template>

<style scoped>
.sc-bar {
  position: absolute;
  top: 58px;
  left: 50%;
  z-index: 30;
  display: flex;
  align-items: center;
  gap: 14px;
  max-width: min(860px, calc(100% - 32px));
  padding: 9px 12px 9px 14px;
  border: 1px solid var(--portal-line-strong);
  border-radius: 8px;
  color: var(--portal-text);
  background: var(--portal-elevated);
  box-shadow: 0 6px 24px var(--portal-shadow);
  font-size: 12px;
  transform: translateX(-50%);
}
.sc-bar.is-conflict,
.sc-bar.is-error { border-color: var(--portal-danger-line); }
.sc-bar.is-offline { border-color: var(--portal-gold-line); }
.sc-bar p { min-width: 0; margin: 0; display: flex; flex-direction: column; gap: 2px; }
.sc-bar p span { color: var(--portal-muted); }
.sc-bar-actions { display: flex; flex-shrink: 0; align-items: center; gap: 8px; }
</style>
