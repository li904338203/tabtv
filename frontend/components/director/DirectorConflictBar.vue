<script setup lang="ts">
/**
 * 编辑器的提示条（DRT-15，DR-UX-07）：冲突 / 离线 / 保存失败 / 有草稿，同一时刻只出一条，优先级照这个顺序。
 *
 * ⚠️ 冲突文案按冲突详情的布尔分流（`bySelf` / `editorKind`），不匹配 message（同画布冲突条，CD-58）；
 *    「以我为准」是真覆盖，不做成主按钮，按钮上写明覆盖的是谁。
 * ⚠️ 任何出路都不静默丢弃用户的改动（DR-N-12）：store 在进冲突 / 失败 / 离线时已把本地场景存成草稿。
 *
 * `open` 由外壳控制：保存状态是冲突、离线、失败时默认展开；用户收起之后，点顶栏的保存状态或尝试返回画布时
 * 再展开（DR-F-154）。
 */
import { ElMessage, ElMessageBox } from "element-plus";
import { clockTime, overwriteLabel } from "~/utils/canvasConflict";
import { directorConflictText } from "~/stores/director";

const open = defineModel<boolean>("open", { default: true });
const store = useDirectorStore();

const kind = computed<"conflict" | "offline" | "error" | "draft" | null>(() => {
  if (store.saveState === "conflict") return "conflict";
  if (store.saveState === "offline") return "offline";
  if (store.saveState === "error") return "error";
  if (store.pendingDraft) return "draft";
  return null;
});
const conflictText = computed(() => directorConflictText(store.conflict));
const overwriteText = computed(() => overwriteLabel(store.conflict));
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
    failure(cause, "载入最新版本失败，你的改动仍在编辑器里");
  }
}

async function overwrite() {
  try {
    const result = await store.resolveConflict("overwrite");
    if (result === "conflict") ElMessage.warning("保存期间又有人修改了场景，请重新选择");
    else ElMessage.success("已用你的版本覆盖");
  } catch (cause) {
    failure(cause, "覆盖保存失败，你的改动仍在编辑器里");
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
  try {
    await ElMessageBox.confirm("恢复草稿会用你之前的版本替换当前场景（可以撤销）。是否继续？", "恢复草稿", {
      confirmButtonText: "恢复",
      cancelButtonText: "取消"
    });
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
  <div v-if="kind && open" class="director-bar" :class="`is-${kind}`" :role="kind === 'draft' ? 'status' : 'alert'" :data-bar="kind">
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
      <span>你的改动仍在编辑器里。</span>
    </p>
    <p v-else>
      <strong>发现未保存的草稿{{ draftTime ? `（${draftTime}）` : "" }}</strong>
      <span>是上次没能存上的改动，可以恢复或丢弃。</span>
    </p>
    <div class="director-bar-actions">
      <template v-if="kind === 'conflict'">
        <button type="button" class="primary" :disabled="store.resolving" @click="adopt">载入最新</button>
        <button type="button" class="danger" :disabled="store.resolving" @click="overwrite">{{ overwriteText }}</button>
      </template>
      <button v-else-if="kind === 'error'" type="button" class="primary" :disabled="store.readonly" @click="retry">重试</button>
      <template v-else-if="kind === 'draft'">
        <button type="button" class="primary" :disabled="store.readonly" @click="restoreDraft">恢复</button>
        <button type="button" @click="discardDraft">丢弃</button>
      </template>
      <button type="button" class="director-bar-close" title="收起提示" aria-label="收起提示" @click="open = false">×</button>
    </div>
  </div>
</template>

<style scoped>
.director-bar {
  position: absolute;
  top: 12px;
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
.director-bar.is-conflict,
.director-bar.is-error { border-color: var(--portal-danger-line); }
.director-bar.is-offline { border-color: var(--portal-gold-line); }
.director-bar p { min-width: 0; margin: 0; display: flex; flex-direction: column; gap: 2px; }
.director-bar p span { color: var(--portal-muted); }
.director-bar-actions { display: flex; flex-shrink: 0; align-items: center; gap: 8px; }
.director-bar button {
  height: 28px;
  padding: 0 10px;
  border: 1px solid var(--portal-line-strong);
  border-radius: 6px;
  color: var(--portal-text);
  background: transparent;
  font-size: 12px;
  cursor: pointer;
}
.director-bar button:hover:not(:disabled) { background: var(--portal-hover); }
.director-bar button:disabled { opacity: 0.45; cursor: not-allowed; }
.director-bar button.primary { border-color: transparent; color: var(--portal-on-accent); background: var(--portal-text); }
.director-bar button.danger { border-color: var(--portal-danger-line); color: var(--portal-danger); }
.director-bar .director-bar-close { width: 28px; padding: 0; font-size: 16px; line-height: 1; }
</style>
