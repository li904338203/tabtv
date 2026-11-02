<script setup lang="ts">
/**
 * 状态条（设计 03 §2.2）：只在会话非空闲时出现。文案全部来自 `utils/agentState.statusBar`（AGT-27）——
 * 「生成比预计慢」不说失败、额度 80% 警示色、断线如实说。
 *
 * 点击：等你确认 → 跳到卡片；其余 → 展开任务清单（`agent_session.todo`）。
 * 失败时给「重试这一步」（交给 Agent，不重新执行已成功的步骤）。
 */
import { Check, ChevronUp, Circle, CircleAlert, LoaderCircle, WifiOff } from "@lucide/vue";
import { normalizeTodo, statusBar } from "~/utils/agentState";

defineProps<{ retryDisabledReason?: string }>();
const emit = defineEmits<{ jump: []; retry: [] }>();
const agent = useAgentStore();
const realtime = useRealtimeStore();

/** 只有「连上过又断了」才说断线：页面刚打开、还在握手时不该吓人。 */
const wasOnline = ref(realtime.state === "online");
watch(() => realtime.state, (state) => { if (state === "online") wasOnline.value = true; });

const waitingSince = computed(() => Object.values(agent.steps)
  .filter(step => step.status === "waiting" && step.generationId)
  .map(step => step.startedAt)
  .sort()
  .at(0));

const view = computed(() => {
  const current = agent.current;
  if (!current) return statusBar(null, []);
  return statusBar({
    state: current.state,
    pendingCardId: current.pendingCardId,
    failureReason: current.latestTurn?.errorMessage
  }, current.todo, {
    connection: wasOnline.value && realtime.state === "offline" ? "offline" : undefined,
    waitingSince: waitingSince.value
  });
});
const todo = computed(() => normalizeTodo(agent.current?.todo ?? []));
const open = ref(false);

function click() {
  if (view.value.jumpToCard) {
    emit("jump");
    return;
  }
  if (todo.value.length) open.value = !open.value;
}
</script>

<template>
  <div v-if="view.visible" class="agent-status" :class="`is-${view.icon}`">
    <ul v-if="open && todo.length" class="agent-todo">
      <li v-for="(item, index) in todo" :key="index" :class="`is-${item.status}`">
        <Check v-if="item.status === 'done'" :size="12" />
        <LoaderCircle v-else-if="item.status === 'doing'" :size="12" class="spin" />
        <Circle v-else :size="12" />
        <span>{{ item.title }}</span>
      </li>
    </ul>
    <div class="agent-status-line">
      <button type="button" class="agent-status-main" :title="view.jumpToCard ? '跳到卡片' : todo.length ? '查看任务清单' : ''" @click="click">
        <LoaderCircle v-if="view.icon === 'spinner'" :size="13" class="spin" />
        <CircleAlert v-else-if="view.icon === 'error'" :size="13" />
        <WifiOff v-else-if="view.icon === 'offline'" :size="13" />
        <span v-else class="agent-status-dot" />
        <span class="agent-status-text">{{ view.text }}</span>
        <ChevronUp v-if="todo.length && !view.jumpToCard" :size="12" :class="{ 'is-open': open }" />
      </button>
      <button v-if="view.icon === 'error'" type="button" class="agent-link-button" :disabled="Boolean(retryDisabledReason)" :title="retryDisabledReason || '不会重新执行已经成功的步骤'" @click="emit('retry')">重试这一步</button>
    </div>
  </div>
</template>
