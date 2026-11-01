<script setup lang="ts">
/**
 * 画布保存冲突条（AGT-07、AG-P3、CD-58）。
 *
 * 两种形态：
 *  - **冲突态**：说清「谁在什么时候改的」+ 三个出路（采纳对方 / 以我为准 / 你的改动已存为草稿的说明）；
 *  - **有草稿**（采纳对方之后、或冲突后刷新过页面）：恢复草稿 / 丢弃。
 *
 * ⚠️ 文案分流只按 `bySelf` 布尔（见 utils/canvasConflict），不匹配 message。
 * ⚠️ 「以我为准」是真覆盖，不做成主按钮，且按钮上写明覆盖的是谁。
 */
import { ElMessage, ElMessageBox } from "element-plus";
import { adoptLabel, conflictSummary, overwriteLabel } from "~/utils/canvasConflict";

const canvas = useCanvasStore();

const inConflict = computed(() => canvas.saveState === "conflict");
const showDraft = computed(() => !inConflict.value && canvas.draftAvailable);
const summary = computed(() => conflictSummary(canvas.conflict));
const adoptText = computed(() => adoptLabel(canvas.conflict));
const overwriteText = computed(() => overwriteLabel(canvas.conflict));

function failure(cause: unknown, fallback: string) {
  ElMessage.error(cause instanceof Error && cause.message ? cause.message : fallback);
}

async function adopt() {
  try {
    await canvas.resolveConflict("adopt");
  } catch (cause) {
    failure(cause, "载入最新版本失败，你的改动仍在画布上");
  }
}

async function overwrite() {
  try {
    const result = await canvas.resolveConflict("overwrite");
    if (result === "conflict") ElMessage.warning("保存期间又有人修改了画布，请重新选择");
    else ElMessage.success("已用你的版本覆盖");
  } catch (cause) {
    failure(cause, "覆盖保存失败，你的改动仍在画布上");
  }
}

async function restore() {
  try {
    await ElMessageBox.confirm(
      "恢复草稿会用你之前的版本替换当前画布（刚载入的他人修改会被覆盖，可用撤销找回）。是否继续？",
      "恢复草稿",
      { confirmButtonText: "恢复", cancelButtonText: "取消" }
    );
  } catch {
    return;
  }
  try {
    await canvas.restoreDraft();
  } catch (cause) {
    failure(cause, "恢复草稿失败");
  }
}

async function discard() {
  try {
    await ElMessageBox.confirm("丢弃后这份本地草稿无法找回。是否继续？", "丢弃草稿", {
      confirmButtonText: "丢弃",
      cancelButtonText: "取消"
    });
  } catch {
    return;
  }
  canvas.discardDraft();
}
</script>

<template>
  <div v-if="inConflict" class="canvas-conflict-bar" role="alert">
    <p>
      <strong>{{ summary }}</strong>
      <span>你的改动没有丢，已存为本标签页的草稿。</span>
    </p>
    <div class="actions">
      <button type="button" class="primary" :disabled="canvas.resolving" @click="adopt">{{ adoptText }}（本地另存草稿）</button>
      <button type="button" class="danger" :disabled="canvas.resolving" @click="overwrite">{{ overwriteText }}</button>
    </div>
  </div>
  <div v-else-if="showDraft" class="canvas-conflict-bar draft" role="status">
    <p>
      <strong>已载入最新版本</strong>
      <span>你之前的改动已存为草稿。</span>
    </p>
    <div class="actions">
      <button type="button" class="primary" @click="restore">恢复草稿</button>
      <button type="button" @click="discard">丢弃</button>
    </div>
  </div>
</template>

<style scoped>
.canvas-conflict-bar {
  position: absolute;
  top: 12px;
  left: 50%;
  z-index: 30;
  display: flex;
  align-items: center;
  gap: 14px;
  max-width: min(860px, calc(100% - 32px));
  padding: 9px 12px 9px 14px;
  border: 1px solid var(--canvas-danger);
  border-radius: 8px;
  color: var(--canvas-text);
  background: var(--canvas-raised);
  box-shadow: 0 6px 24px rgb(0 0 0 / 28%);
  font-size: 12px;
  transform: translateX(-50%);
}

.canvas-conflict-bar.draft { border-color: var(--canvas-line-strong); }

.canvas-conflict-bar p {
  min-width: 0;
  margin: 0;
  display: flex;
  flex-direction: column;
  gap: 2px;
}

.canvas-conflict-bar p span { color: var(--canvas-muted); }

.canvas-conflict-bar .actions {
  display: flex;
  flex-shrink: 0;
  gap: 8px;
}

.canvas-conflict-bar button {
  height: 28px;
  padding: 0 10px;
  border: 1px solid var(--canvas-line-strong);
  border-radius: 6px;
  color: var(--canvas-text);
  background: transparent;
  font-size: 12px;
  white-space: nowrap;
  cursor: pointer;
}

.canvas-conflict-bar button.primary { border-color: var(--canvas-cyan); color: var(--canvas-cyan); }
.canvas-conflict-bar button.danger { color: var(--canvas-danger); }
.canvas-conflict-bar button:disabled { opacity: 0.5; cursor: progress; }
</style>
