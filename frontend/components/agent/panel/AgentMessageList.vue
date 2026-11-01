<script setup lang="ts">
/**
 * 消息流（设计 03 §3、§9）。不做虚拟滚动：分页加载更早的 + 自动滚底。
 *
 * - 在底部时新内容到达自动滚到底；用户手动上滑后**停止自动滚动**，右下角出现「↓ 有新消息」。
 * - 加载更早一页时保持当前阅读位置（不跳）。
 * - 切到一个会话的第一屏：有待处理卡片时滚到卡片，否则滚到底（设计 03 §8「关闭再打开」）。
 *
 * 排列规则（哪些消息显示、步骤插在哪、哪些合并）全在 `utils/agentMessages.buildTimeline`。
 * 末尾另挂批量计划的执行进度（`PlanProgressFooter`，只在营销会话有 ≥ 2 项的已批准计划时出现）。
 */
import { ArrowDown, LoaderCircle } from "@lucide/vue";
import AssistantText from "./messages/AssistantText.vue";
import StepRow from "./messages/StepRow.vue";
import PlanProgressFooter from "./messages/PlanProgressFooter.vue";
import SuggestionChips from "./messages/SuggestionChips.vue";
import TurnFooter from "./messages/TurnFooter.vue";
import UserMessage from "./messages/UserMessage.vue";
import BudgetCard from "./cards/BudgetCard.vue";
import ChoiceCard from "./cards/ChoiceCard.vue";
import DeleteConfirmCard from "./cards/DeleteConfirmCard.vue";
import PlanConfirmCard from "./cards/PlanConfirmCard.vue";
import QuestionsCard from "./cards/QuestionsCard.vue";
import type { AgentCard, TimelineItem } from "~/utils/agentMessages";

const props = defineProps<{
  sessionKey: string;
  items: TimelineItem[];
  hasOlder: boolean;
  loadingOlder: boolean;
  pendingCardId?: string | null;
  /** 卡片 / 产出不能操作的原因（只读成员、非发起人、会话已结束）。 */
  readOnlyReason?: string;
  /** 建议胶囊不能发的原因。 */
  sendDisabledReason?: string;
  /** 最近一条用户消息上方显示的视频模型。 */
  videoModel?: string;
  submitCard: (card: AgentCard, submission: Record<string, unknown>) => Promise<void>;
}>();
const emit = defineEmits<{ loadOlder: []; send: [text: string]; retry: [title: string] }>();

const CARD_COMPONENTS = {
  choice: ChoiceCard,
  questions: QuestionsCard,
  plan_confirm: PlanConfirmCard,
  budget: BudgetCard,
  delete_confirm: DeleteConfirmCard
} as const;

const scroller = ref<HTMLElement>();
const atBottom = ref(true);
const hasNew = ref(false);
let initialised = false;

const lastUserKey = computed(() => [...props.items].reverse().find(item => item.kind === "user")?.key);
const tailSignature = computed(() => {
  const last = props.items.at(-1);
  if (!last) return "";
  const size = last.kind === "assistant" ? last.text.length : last.kind === "step" ? `${last.step.status}:${last.card?.status ?? ""}` : "";
  return `${props.items.length}:${last.key}:${size}`;
});

function cardOf(item: TimelineItem): AgentCard | undefined {
  if (item.kind === "card") return item.card;
  if (item.kind === "step") return item.card;
  return undefined;
}

function onScroll() {
  const el = scroller.value;
  if (!el) return;
  atBottom.value = el.scrollHeight - el.scrollTop - el.clientHeight < 60;
  if (atBottom.value) hasNew.value = false;
  if (el.scrollTop < 40 && props.hasOlder && !props.loadingOlder) loadOlder();
}

function scrollToBottom(smooth = false) {
  const el = scroller.value;
  if (!el) return;
  el.scrollTo({ top: el.scrollHeight, behavior: smooth ? "smooth" : "auto" });
  hasNew.value = false;
  atBottom.value = true;
}

function scrollToCard(cardId: string | null | undefined): boolean {
  const el = scroller.value;
  if (!el || !cardId) return false;
  const target = el.querySelector<HTMLElement>(`[data-card-id="${CSS.escape(cardId)}"]`);
  if (!target) return false;
  target.scrollIntoView({ block: "center", behavior: "smooth" });
  target.classList.add("is-flash");
  setTimeout(() => target.classList.remove("is-flash"), 1000);
  return true;
}

// 加载更早的一页：保持阅读位置
let anchor: { height: number; top: number } | null = null;
function loadOlder() {
  const el = scroller.value;
  if (el) anchor = { height: el.scrollHeight, top: el.scrollTop };
  emit("loadOlder");
}
watch(() => props.items[0]?.key, async () => {
  if (!anchor) return;
  const saved = anchor;
  anchor = null;
  await nextTick();
  const el = scroller.value;
  if (el) el.scrollTop = el.scrollHeight - saved.height + saved.top;
});

// 新内容到达：在底部就跟着滚，不在底部就亮「有新消息」
watch(tailSignature, async (next, prev) => {
  if (!next) return;
  await nextTick();
  if (!initialised) {
    initialised = true;
    if (!scrollToCard(props.pendingCardId)) scrollToBottom();
    return;
  }
  if (!prev) return;
  if (atBottom.value) scrollToBottom();
  else hasNew.value = true;
}, { immediate: true });

// 换会话：重新走「第一屏」逻辑
watch(() => props.sessionKey, () => {
  initialised = false;
  hasNew.value = false;
  atBottom.value = true;
});

defineExpose({ scrollToCard, scrollToBottom });
</script>

<template>
  <div class="agent-messages-wrap">
    <div ref="scroller" class="agent-messages" @scroll.passive="onScroll">
      <div v-if="hasOlder || loadingOlder" class="agent-older">
        <LoaderCircle v-if="loadingOlder" :size="13" class="spin" />
        <button v-else type="button" class="agent-link-button" @click="loadOlder">加载更早的消息</button>
      </div>
      <template v-for="item in items" :key="item.key">
        <UserMessage v-if="item.kind === 'user'" :message="item.message" :video-model="item.key === lastUserKey ? videoModel : undefined" />
        <AssistantText v-else-if="item.kind === 'assistant'" :text="item.text" :streaming="item.streaming" :stopped="item.stopped" />
        <div v-else-if="item.kind === 'system'" class="agent-system">{{ item.message.content }}</div>
        <StepRow v-else-if="item.kind === 'steps'" :steps="item.steps" :read-only-reason="readOnlyReason" />
        <TurnFooter v-else-if="item.kind === 'footer'" :turn="item.turn" />
        <SuggestionChips v-else-if="item.kind === 'suggestions'" :items="item.items" :disabled-reason="sendDisabledReason" @pick="emit('send', $event)" />
        <StepRow v-else-if="item.kind === 'step'" :steps="[item.step]" :args="item.args" :read-only-reason="readOnlyReason" @retry="emit('retry', $event)" />
        <component
          :is="CARD_COMPONENTS[cardOf(item)!.kind]"
          v-if="cardOf(item) && CARD_COMPONENTS[cardOf(item)!.kind]"
          :card="cardOf(item)!"
          :read-only-reason="readOnlyReason"
          :submit="(submission: Record<string, unknown>) => submitCard(cardOf(item)!, submission)"
        />
      </template>
      <!-- 批量 / 裂变的执行进度（MKT-T-29）：营销会话最近一张已批准计划，挂在最后一轮页脚之后 -->
      <PlanProgressFooter :send-disabled-reason="sendDisabledReason" @send="emit('send', $event)" />
    </div>
    <button v-if="hasNew" type="button" class="agent-new-messages" @click="scrollToBottom(true)"><ArrowDown :size="13" />有新消息</button>
  </div>
</template>
