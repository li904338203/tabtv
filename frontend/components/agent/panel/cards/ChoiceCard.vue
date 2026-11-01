<script setup lang="ts">
/**
 * 选择卡（`ask_user` 单选 / 多选，设计 03 §3.4 A），含「我有别的想法」（展开一个文本框）。
 * 提交体与服务端 `ChoiceSubmission` 逐字一致：`{selected, other?}`；单选恰好一项、多选至少一项，
 * 「我有别的想法」算一项且不能为空。
 */
import { Plus } from "@lucide/vue";
import CardSummary from "./CardSummary.vue";
import type { AgentCard } from "~/utils/agentMessages";
import { apiFailure } from "~/utils/agentPanelBridge";

type Option = { id: string; label: string; detail?: string };
const props = defineProps<{ card: AgentCard; readOnlyReason?: string; submit: (submission: Record<string, unknown>) => Promise<void> }>();

const payload = computed(() => props.card.payload as { title?: string; multi?: boolean; options?: Option[]; allowOther?: boolean });
const options = computed(() => payload.value.options ?? []);
const multi = computed(() => payload.value.multi === true);
const selected = ref<string[]>([]);
const otherOpen = ref(false);
const other = ref("");
const busy = ref(false);
const error = ref("");

const count = computed(() => selected.value.length + (otherOpen.value ? 1 : 0));
const valid = computed(() => {
  if (otherOpen.value && !other.value.trim()) return false;
  return multi.value ? count.value >= 1 : count.value === 1;
});

function toggle(id: string) {
  if (props.readOnlyReason) return;
  if (multi.value) {
    selected.value = selected.value.includes(id) ? selected.value.filter(item => item !== id) : [...selected.value, id];
    return;
  }
  selected.value = [id];
  otherOpen.value = false;
}

function toggleOther() {
  if (props.readOnlyReason) return;
  otherOpen.value = !otherOpen.value;
  if (otherOpen.value && !multi.value) selected.value = [];
}

async function confirm() {
  if (!valid.value || busy.value || props.readOnlyReason) return;
  busy.value = true;
  error.value = "";
  try {
    await props.submit({ selected: selected.value, ...(otherOpen.value ? { other: other.value.trim() } : {}) });
  } catch (cause) {
    error.value = apiFailure(cause).message;
  } finally {
    busy.value = false;
  }
}
</script>

<template>
  <CardSummary v-if="card.status !== 'pending'" :title="payload.title || '选择'" :status="card.status" />
  <section v-else class="agent-card" :data-card-id="card.id">
    <header class="agent-card-title">{{ payload.title || '请选择' }}<small v-if="multi">（可多选）</small></header>
    <div class="agent-choice-list" :class="{ 'is-readonly': readOnlyReason }">
      <button
        v-for="(option, index) in options"
        :key="option.id"
        type="button"
        class="agent-choice"
        :class="{ 'is-selected': selected.includes(option.id) }"
        :aria-pressed="selected.includes(option.id)"
        :disabled="Boolean(readOnlyReason)"
        @click="toggle(option.id)"
      >
        <strong>{{ index + 1 }} {{ option.label }}</strong>
        <span v-if="option.detail">{{ option.detail }}</span>
      </button>
      <button v-if="payload.allowOther" type="button" class="agent-choice is-other" :class="{ 'is-selected': otherOpen }" :disabled="Boolean(readOnlyReason)" @click="toggleOther">
        <strong><Plus :size="13" />我有别的想法</strong>
      </button>
      <textarea v-if="otherOpen" v-model="other" class="agent-textarea" maxlength="500" rows="3" placeholder="说说你的想法" />
    </div>
    <p v-if="error" class="agent-error-text">{{ error }}</p>
    <footer class="agent-card-actions">
      <span v-if="readOnlyReason" class="agent-muted">{{ readOnlyReason }}</span>
      <button type="button" class="agent-primary" :disabled="!valid || busy || Boolean(readOnlyReason)" @click="confirm">继续</button>
    </footer>
  </section>
</template>
