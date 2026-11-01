<script setup lang="ts">
/**
 * 远端批次提示（AGT-11）：右下角「Agent 新增了 N 个节点 · 撤销 · 查看」；
 * MCP 写入按客户端名说（「Claude Code 新增了 2 个节点」，MCPT-09；没有客户端名时说「外部 AI 客户端」）。
 *
 * ⚠️ 新节点不在视口里时**不自动平移**（会打断正在画布上干活的用户），只在这里给「查看」。
 * ⚠️ 「撤销」只在这一批仍是撤销栈顶时可用；之后还有别的改动的话说清楚为什么不能撤，
 *    而不是静默无效或者跳过中间的改动去撤它。
 */
import { ElMessage } from "element-plus";
import { Sparkles, X } from "@lucide/vue";
import { actorLabel } from "~/utils/canvasOrigin";

const emit = defineEmits<{ view: [nodeIds: string[]] }>();
const canvas = useCanvasStore();

const VISIBLE_MS = 8000;
const visible = ref(false);
let timer: ReturnType<typeof setTimeout> | undefined;

const notice = computed(() => canvas.lastRemoteBatch);
const who = computed(() => actorLabel(notice.value?.actorKind ?? "agent", notice.value?.clientName));
const message = computed(() => {
  const value = notice.value;
  if (!value) return "";
  if (value.nodeIds.length) return `${who.value} 新增了 ${value.nodeIds.length} 个节点`;
  return `${who.value} 修改了 ${value.changedCount} 个节点`;
});

watch(() => notice.value?.seq, () => {
  const value = notice.value;
  clearTimeout(timer);
  visible.value = Boolean(value && (value.nodeIds.length || value.changedCount));
  if (visible.value) timer = setTimeout(() => { visible.value = false; }, VISIBLE_MS);
});

function undo() {
  const value = notice.value;
  if (!value) return;
  if (canvas.undoAgentBatch(value.batchId)) {
    visible.value = false;
    return;
  }
  ElMessage.warning("之后还有改动，请在画布上手动删除");
}

function view() {
  if (notice.value?.nodeIds.length) emit("view", [...notice.value.nodeIds]);
}

function close() {
  clearTimeout(timer);
  visible.value = false;
}

onBeforeUnmount(() => clearTimeout(timer));
</script>

<template>
  <div v-if="visible && notice" class="canvas-ops-toast" role="status" aria-live="polite">
    <Sparkles :size="14" />
    <span>{{ message }}</span>
    <button type="button" @click="undo">撤销</button>
    <button v-if="notice.nodeIds.length" type="button" @click="view">查看</button>
    <button type="button" class="close" aria-label="关闭" @click="close"><X :size="13" /></button>
  </div>
</template>

<style scoped>
.canvas-ops-toast {
  position: absolute;
  right: 16px;
  /* 底栏（dock 高 49，离底 17）之上：画布区窄时 dock 会往右让开、伸到这一侧（小地图 2026-11-01 起在左下角） */
  bottom: 74px;
  z-index: 30;
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 8px 10px 8px 12px;
  border: 1px solid var(--canvas-line-strong);
  border-radius: 10px;
  background: var(--canvas-panel);
  color: var(--canvas-text);
  font-size: 12px;
  box-shadow: 0 8px 24px var(--cv-blue-04a84);
}
.canvas-ops-toast > svg { color: var(--canvas-cyan); }
.canvas-ops-toast button {
  padding: 2px 6px;
  border: 0;
  border-radius: 5px;
  background: transparent;
  color: var(--canvas-cyan);
  font: inherit;
  cursor: pointer;
}
.canvas-ops-toast button:hover { background: var(--canvas-raised); }
.canvas-ops-toast button.close { display: inline-grid; place-items: center; color: var(--canvas-muted); }
</style>
