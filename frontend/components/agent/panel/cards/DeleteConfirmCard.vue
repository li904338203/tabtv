<script setup lang="ts">
/**
 * 删除确认卡（设计 03 §3.4 E）：Agent 不能直接删用户的节点，只能建议。
 *
 * 「删除」**走画布正常的删除 + 保存**（与用户在画布上按 Delete 是同一条路：进撤销栈、可 Ctrl+Z），
 * 不经 Agent；保存成功后再提交「同意」告诉 Agent。保存失败（冲突 / 断网）就不提交 ——
 * 否则 Agent 以为删了，而画布上那几个节点其实还在。
 * 「在画布上查看」由用户点了才平移（不自动平移，会打断用户）。
 *
 * ⚠️ 全屏对话页（AGT-44）没有载入画布：这里**不能删** —— 画布 store 里可能是上一个打开过的画布，
 *    删了再保存会写到别的项目上；即便是同一个项目，没载入的画布整份保存也会把内容清空。
 *    所以只给「保留」（不碰画布）与「去画布处理」（带 `?node=` 跳过去，在画布上删）。
 */
import CardSummary from "./CardSummary.vue";
import type { AgentCard } from "~/utils/agentMessages";
import { AGENT_PANEL_BRIDGE, NOOP_BRIDGE, apiFailure } from "~/utils/agentPanelBridge";

const props = defineProps<{ card: AgentCard; readOnlyReason?: string; submit: (submission: Record<string, unknown>) => Promise<void> }>();
const canvas = useCanvasStore();
const bridge = inject(AGENT_PANEL_BRIDGE, NOOP_BRIDGE);

const payload = computed(() => props.card.payload as { reason?: string; nodeIds?: string[] });
const nodeIds = computed(() => payload.value.nodeIds ?? []);
const canvasLoaded = bridge.canvasLoaded;
const present = computed(() => nodeIds.value.map(id => ({ id, node: canvasLoaded ? canvas.nodes.find(node => node.id === id) : undefined })));
const busy = ref(false);
const error = ref("");

async function decide(decision: "approve" | "reject") {
  if (busy.value || props.readOnlyReason) return;
  if (decision === "approve" && !canvasLoaded) return;
  busy.value = true;
  error.value = "";
  try {
    if (decision === "approve") {
      const existing = present.value.filter(item => item.node).map(item => item.id);
      if (existing.length) {
        for (const id of existing) canvas.removeNode(id);
        await canvas.save();
        if (canvas.saveState === "conflict" || canvas.saveState === "error") {
          error.value = canvas.saveState === "conflict"
            ? "画布有冲突，删除还没保存上，请先处理画布顶部的冲突提示"
            : "删除还没保存上，请检查网络后再点一次";
          return;
        }
      }
    }
    await props.submit({ decision });
  } catch (cause) {
    error.value = apiFailure(cause).message;
  } finally {
    busy.value = false;
  }
}

function view() {
  bridge.locate(nodeIds.value);
}
</script>

<template>
  <CardSummary v-if="card.status !== 'pending'" :title="`建议删除 ${nodeIds.length} 个节点`" :status="card.status" :detail="card.submission?.decision === 'approve' ? '已删除' : card.status === 'submitted' ? '已保留' : undefined" />
  <section v-else class="agent-card" :data-card-id="card.id">
    <header class="agent-card-title">建议删除这 {{ nodeIds.length }} 个节点</header>
    <p v-if="payload.reason" class="agent-muted">{{ payload.reason }}</p>
    <div class="agent-ref-chips">
      <span v-for="item in present" :key="item.id" class="agent-chip is-static" :class="{ 'is-gone': canvasLoaded && !item.node }">
        {{ item.node?.data.title || `节点 ${item.id.slice(-4)}` }}<template v-if="canvasLoaded && !item.node">（已不在画布上）</template>
      </span>
      <button v-if="canvasLoaded || readOnlyReason" type="button" class="agent-link-button" @click="view">在画布上查看</button>
    </div>
    <p v-if="!canvasLoaded && !readOnlyReason" class="agent-muted">删除要在画布上进行（可撤销）；这里可以直接选择保留。</p>
    <p v-if="error" class="agent-error-text">{{ error }}</p>
    <footer class="agent-card-actions">
      <span v-if="readOnlyReason" class="agent-muted">{{ readOnlyReason }}</span>
      <template v-else>
        <button type="button" class="agent-secondary" :disabled="busy" @click="decide('reject')">保留</button>
        <button v-if="canvasLoaded" type="button" class="agent-danger" :disabled="busy" @click="decide('approve')">删除</button>
        <button v-else type="button" class="agent-danger" :disabled="busy" title="打开画布并定位到这些节点" @click="view">去画布处理</button>
      </template>
    </footer>
  </section>
</template>
