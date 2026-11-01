<script setup lang="ts">
/**
 * 批量 / 裂变计划的执行进度（MKT-T-29，营销 dev/01 §3.6；MKT-F-51、MKT-AC-18 界面）。
 * 挂在消息流末尾（本轮页脚的位置）：「部分成功（2/3）」+ 逐个变体的状态与「重试」。
 *
 * - 数据只来自会话详情的 `planProgress`（服务端按步骤现算；`agent.state` 事件只是对账信号，store 收到后拉详情）。
 *   这里**不判断要不要停后续**，也不自己数成功几个。
 * - 「部分成功」不摊平：说成成功用户不会去翻失败那条，说成失败他会把成功的也重跑一遍 —— 两种都在花他的钱（AV-24）。
 * - 「重试」= 交给 Agent（重新报价、走额度检查，服务端建新步骤、新幂等键），点名条目键，已成功的不重做；
 *   与「重试这一步」同一条发送路径，能不能发与输入框同一个判定。
 * - 余额不足没提交的、因此被停掉的、上游生成失败的三种原因分开说（文案在 `utils/planItems.planProgressView`）。
 */
import { CircleAlert, CircleCheck, CircleDashed, LoaderCircle, OctagonPause } from "@lucide/vue";
import { planProgressView, planRetryMessage, type PlanProgressItemView } from "~/utils/planItems";

const props = defineProps<{ sendDisabledReason?: string }>();
const emit = defineEmits<{ send: [text: string] }>();
const agent = useAgentStore();

const view = computed(() => planProgressView(agent.detail?.planProgress, props.sendDisabledReason ?? ""));
/** 全部成功时默认收起明细（一行汇总就够了）；有没成功的默认展开。 */
const open = ref<boolean | null>(null);
const expanded = computed(() => open.value ?? (view.value ? view.value.tone !== "success" : false));

function retry(item: PlanProgressItemView) {
  if (!item.canRetry) return;
  emit("send", planRetryMessage(item));
}
</script>

<template>
  <section v-if="view" class="agent-plan-progress" :class="`is-${view.tone}`" aria-live="polite">
    <button type="button" class="agent-plan-progress-head" :aria-expanded="expanded" @click="open = !expanded">
      <LoaderCircle v-if="view.tone === 'running'" :size="13" class="spin" />
      <CircleCheck v-else-if="view.tone === 'success'" :size="13" />
      <CircleAlert v-else :size="13" />
      <strong>{{ view.summary }}</strong>
      <small v-if="view.detail">{{ view.detail }}</small>
    </button>
    <ul v-if="expanded" class="agent-plan-progress-items">
      <li v-for="item in view.items" :key="item.key" :class="`is-${item.state}`">
        <CircleCheck v-if="item.state === 'success'" :size="12" />
        <LoaderCircle v-else-if="item.state === 'submitted'" :size="12" class="spin" />
        <OctagonPause v-else-if="item.state === 'stopped'" :size="12" />
        <CircleDashed v-else-if="item.state === 'pending'" :size="12" />
        <CircleAlert v-else :size="12" />
        <span class="agent-plan-progress-name">
          <span>{{ item.title }}</span>
          <small v-if="item.reason">{{ item.reason }}</small>
        </span>
        <em>{{ item.stateText }}</em>
        <button
          v-if="item.state === 'failed' || item.state === 'stopped'"
          type="button"
          class="agent-link-button"
          :disabled="!item.canRetry"
          :title="item.retryBlocked || '交给 Agent 重新报价后只重试这一项（会走额度检查）'"
          @click="retry(item)"
        >重试</button>
      </li>
    </ul>
  </section>
</template>

<style scoped>
.agent-plan-progress {
  margin: 4px 0 8px;
  border: 1px solid var(--canvas-line, var(--portal-line));
  border-radius: 8px;
  padding: 6px 8px;
  color: var(--canvas-text, var(--portal-text));
  background: var(--cv-n12, var(--portal-surface-2));
  font-size: 12px;
}
.agent-plan-progress-head {
  width: 100%;
  display: flex;
  align-items: center;
  gap: 6px;
  border: 0;
  padding: 0;
  color: inherit;
  background: none;
  font: inherit;
  text-align: left;
  cursor: pointer;
}
.agent-plan-progress-head small { color: var(--canvas-faint, var(--portal-muted)); }
.agent-plan-progress.is-partial .agent-plan-progress-head,
.agent-plan-progress.is-failed .agent-plan-progress-head { color: var(--canvas-gold, var(--portal-danger)); }
/* 完成用正文色（中性，design/03 R-4）：与暗淡的未完成项区分；失败 / 部分完成仍是提醒色。 */
.agent-plan-progress.is-success .agent-plan-progress-head { color: var(--canvas-text, var(--portal-text)); }
.agent-plan-progress-items { margin: 6px 0 0; padding: 0; list-style: none; display: grid; gap: 4px; }
.agent-plan-progress-items li { display: flex; align-items: flex-start; gap: 6px; }
.agent-plan-progress-items li > svg { flex: none; margin-top: 2px; color: var(--canvas-faint, var(--portal-muted)); }
.agent-plan-progress-items li.is-success > svg { color: var(--canvas-text, var(--portal-text)); }
.agent-plan-progress-items li.is-failed > svg { color: var(--canvas-danger, var(--portal-danger)); }
.agent-plan-progress-name { flex: 1; min-width: 0; display: grid; gap: 1px; }
.agent-plan-progress-name small { color: var(--canvas-faint, var(--portal-muted)); }
.agent-plan-progress-items em { flex: none; font-style: normal; color: var(--canvas-faint, var(--portal-muted)); }
.agent-plan-progress-items .agent-link-button { flex: none; }
</style>
