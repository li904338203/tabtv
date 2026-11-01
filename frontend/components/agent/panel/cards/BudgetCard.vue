<script setup lang="ts">
/**
 * 追加额度卡（设计 03 §3.4 D）：执行中超出已批准额度时出现。金额来自服务端 payload。
 * 「不用了」= reject（会话回到空闲、不再扣费）；「追加 ✦x」= approve（服务端还会查余额，不足回 422 带差额）。
 */
import CardSummary from "./CardSummary.vue";
import { budgetCardView, type AgentCard } from "~/utils/agentMessages";
import { apiFailure } from "~/utils/agentPanelBridge";

const props = defineProps<{ card: AgentCard; readOnlyReason?: string; submit: (submission: Record<string, unknown>) => Promise<void> }>();
const view = computed(() => budgetCardView(props.card.payload));
const busy = ref(false);
const error = ref("");

async function decide(decision: "approve" | "reject") {
  if (busy.value || props.readOnlyReason) return;
  busy.value = true;
  error.value = "";
  try {
    await props.submit({ decision });
  } catch (cause) {
    error.value = apiFailure(cause).message;
  } finally {
    busy.value = false;
  }
}
</script>

<template>
  <CardSummary v-if="card.status !== 'pending'" title="追加额度" :status="card.status" :detail="card.submission?.decision === 'approve' ? `已追加 ${view.requested}` : card.status === 'submitted' ? '没有追加' : undefined" />
  <section v-else class="agent-card" :data-card-id="card.id">
    <header class="agent-card-title">需要追加额度</header>
    <p>{{ view.reason }}，需要 {{ view.requested }}</p>
    <p class="agent-muted">{{ view.usage }}</p>
    <p v-if="error" class="agent-error-text">{{ error }}</p>
    <footer class="agent-card-actions">
      <span v-if="readOnlyReason" class="agent-muted">{{ readOnlyReason }}</span>
      <template v-else>
        <button type="button" class="agent-secondary" :disabled="busy" @click="decide('reject')">不用了</button>
        <button type="button" class="agent-primary" :disabled="busy" @click="decide('approve')">{{ view.approveText }}</button>
      </template>
    </footer>
  </section>
</template>
