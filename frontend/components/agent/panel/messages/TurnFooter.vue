<script setup lang="ts">
/**
 * 每轮页脚（设计 03 §3.7，R-6 / R-7 按用户要求简化）：只显示「完整日期时间 本轮消耗 ✦N」，有退回时补「（已退回 ✦z）」；
 * 纯文字，不能点开（逐笔流水不再在这里展开）。「对话 ✦x（N 次调用）｜ 生成 ✦y ｜ 退回 ✦z」三项明细留在悬浮提示里。
 * 金额**全部来自账本**（`GET /agent/sessions/{id}/turns/{turnId}/ledger`）。
 *
 * ⚠️ 账本没取到时金额写「—」，不拿轮次上的报价顶替：报价不是实扣，用户会拿它去对余额变化。
 */
import { turnFooter, type AgentTurn, type AgentTurnLedger } from "~/utils/agentMessages";

const props = defineProps<{ turn: AgentTurn }>();
const agent = useAgentStore();

const ledger = ref<AgentTurnLedger>();
const view = computed(() => turnFooter(ledger.value, props.turn));

let token = 0;
async function load() {
  const mine = ++token;
  try {
    const value = await agent.turnLedger(props.turn.id);
    if (mine === token) ledger.value = value;
  } catch {
    // 取不到时金额显示「—」（turnFooter 处理），不报错打扰。
  }
}

// 轮次状态变了（例如唤醒后又退回了一笔）就重取一次
watch(() => [props.turn.id, props.turn.status, props.turn.finishedAt], () => { void load(); }, { immediate: true });
</script>

<template>
  <div class="agent-turn-footer" :title="`${view.chat}｜${view.generation}｜${view.refunded}`">
    <span>{{ view.dateTime }}</span>
    <span>{{ view.spent }}</span>
  </div>
</template>
