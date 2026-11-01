<script setup lang="ts">
/**
 * 分页问答卡（设计 03 §3.4 B）：每题单选 / 多选 / 文本；分页点上未答的标红；**全部答完才能提交**。
 * 提交体与服务端 `QuestionsSubmission` 一致（`utils/agentMessages.questionsSubmission`）。
 */
import { ChevronLeft, ChevronRight } from "@lucide/vue";
import CardSummary from "./CardSummary.vue";
import { questionsSubmission, unansweredQuestions, type AgentCard, type QuestionAnswer } from "~/utils/agentMessages";
import { apiFailure } from "~/utils/agentPanelBridge";

type Option = { id: string; label: string; detail?: string };
type Question = { id: string; text: string; type: "single" | "multi" | "text"; options?: Option[]; allowOther?: boolean };
const props = defineProps<{ card: AgentCard; readOnlyReason?: string; submit: (submission: Record<string, unknown>) => Promise<void> }>();

const payload = computed(() => props.card.payload as { title?: string; questions?: Question[] });
const questions = computed(() => payload.value.questions ?? []);
const page = ref(0);
const answers = reactive<Record<string, QuestionAnswer>>({});
const visited = ref(new Set<number>([0]));
const busy = ref(false);
const error = ref("");

const current = computed(() => questions.value[page.value]);
const missing = computed(() => unansweredQuestions(questions.value, answers));
watch(questions, (list) => {
  for (const q of list) answers[q.id] ??= { selected: [] };
}, { immediate: true });
const answer = computed<QuestionAnswer>(() => (current.value && answers[current.value.id]) || { selected: [] });

function go(target: number) {
  page.value = Math.min(Math.max(0, target), questions.value.length - 1);
  visited.value = new Set([...visited.value, page.value]);
}

function pick(id: string) {
  const q = current.value;
  if (!q || props.readOnlyReason) return;
  const a = answer.value;
  if (q.type === "multi") a.selected = a.selected.includes(id) ? a.selected.filter(item => item !== id) : [...a.selected, id];
  else {
    a.selected = [id];
    delete a.other;
  }
}

function toggleOther() {
  const q = current.value;
  if (!q || props.readOnlyReason) return;
  const a = answer.value;
  if (a.other !== undefined) delete a.other;
  else {
    a.other = "";
    if (q.type === "single") a.selected = [];
  }
}

/** 分页点标红：未答且已经翻到过（还没看过的题不急着红）——提交被拦时全部标红。 */
const showAllMissing = ref(false);
function dotMissing(index: number) {
  const q = questions.value[index];
  return Boolean(q && missing.value.includes(q.id) && (showAllMissing.value || visited.value.has(index)) && index !== page.value);
}

async function confirm() {
  if (props.readOnlyReason || busy.value) return;
  if (missing.value.length) {
    showAllMissing.value = true;
    go(questions.value.findIndex(q => q.id === missing.value[0]));
    return;
  }
  busy.value = true;
  error.value = "";
  try {
    await props.submit(questionsSubmission(questions.value, answers));
  } catch (cause) {
    error.value = apiFailure(cause).message;
  } finally {
    busy.value = false;
  }
}
</script>

<template>
  <CardSummary v-if="card.status !== 'pending'" :title="payload.title || '问答'" :status="card.status" />
  <section v-else class="agent-card" :data-card-id="card.id">
    <header class="agent-card-title">
      {{ payload.title || '再确认几个细节' }}
      <span class="agent-pager">
        <button type="button" :disabled="page === 0" title="上一题" @click="go(page - 1)"><ChevronLeft :size="13" /></button>
        <span>{{ page + 1 }}/{{ questions.length }}</span>
        <button type="button" :disabled="page >= questions.length - 1" title="下一题" @click="go(page + 1)"><ChevronRight :size="13" /></button>
      </span>
    </header>
    <div class="agent-pager-dots">
      <button
        v-for="(q, index) in questions"
        :key="q.id"
        type="button"
        :class="{ 'is-current': index === page, 'is-missing': dotMissing(index), 'is-done': !missing.includes(q.id) }"
        :title="missing.includes(q.id) ? `第 ${index + 1} 题还没回答` : `第 ${index + 1} 题`"
        @click="go(index)"
      />
    </div>
    <template v-if="current">
      <p class="agent-question">{{ current.text }}</p>
      <textarea v-if="current.type === 'text'" v-model="answer.text" class="agent-textarea" rows="3" maxlength="500" :disabled="Boolean(readOnlyReason)" placeholder="写下你的回答" />
      <div v-else class="agent-choice-list is-compact">
        <button
          v-for="option in current.options ?? []"
          :key="option.id"
          type="button"
          class="agent-choice"
          :class="{ 'is-selected': answer.selected.includes(option.id) }"
          :disabled="Boolean(readOnlyReason)"
          @click="pick(option.id)"
        >
          <strong>{{ current.type === 'multi' ? '☐' : '○' }} {{ option.label }}</strong>
          <span v-if="option.detail">{{ option.detail }}</span>
        </button>
        <button v-if="current.allowOther" type="button" class="agent-choice is-other" :class="{ 'is-selected': answer.other !== undefined }" :disabled="Boolean(readOnlyReason)" @click="toggleOther">
          <strong>自定义</strong>
        </button>
        <input v-if="answer.other !== undefined" v-model="answer.other" class="agent-input" maxlength="500" placeholder="写下你的想法">
      </div>
    </template>
    <p v-if="error" class="agent-error-text">{{ error }}</p>
    <p v-else-if="showAllMissing && missing.length" class="agent-error-text">还有 {{ missing.length }} 题没回答</p>
    <footer class="agent-card-actions">
      <span v-if="readOnlyReason" class="agent-muted">{{ readOnlyReason }}</span>
      <button type="button" class="agent-secondary" :disabled="page === 0" @click="go(page - 1)">上一题</button>
      <button v-if="page < questions.length - 1" type="button" class="agent-secondary" @click="go(page + 1)">下一题</button>
      <button v-else type="button" class="agent-primary" :class="{ 'is-blocked': missing.length > 0 }" :disabled="busy || Boolean(readOnlyReason)" :title="missing.length ? `还有 ${missing.length} 题没回答` : ''" @click="confirm">提交</button>
    </footer>
  </section>
</template>
