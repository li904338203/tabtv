<script setup lang="ts">
/**
 * AI 剪辑的结果卡（SCT-31，SC-F-82）：写进时间线之后弹出 —— 用了哪几段、总时长、每段理由（可折叠）、警告、兜底说明、
 * 追加指令改了哪几处，以及「撤销这轮 AI」。多方案时带 A / B / C 切换。
 *
 * ⚠️ 素材不够的出路（允许放慢 / 允许重复 / 去补镜）只按结果里的 `shortage` 给（`smartcutShortage`），不匹配警告文案。
 */
import { ChevronDown, ChevronUp, Sparkles, Undo2, X } from "@lucide/vue";
import type { SmartcutPlanState } from "~/stores/smartcut";
import { smartcutPlanView, type SmartcutUnsuitableAction } from "~/utils/smartcutPlan";
import VariantSwitcher from "./VariantSwitcher.vue";

const props = defineProps<{
  plan: SmartcutPlanState;
  /** 出路、切换方案不能点的原因（只读、进行中、被限流）；空 = 能点。 */
  disabledReason?: string;
  /** 「撤销这轮 AI」不能点的原因；空 = 能点。 */
  undoDisabledReason?: string;
}>();
const emit = defineEmits<{ undo: []; close: []; out: [action: SmartcutUnsuitableAction]; select: [index: number] }>();

const view = computed(() => smartcutPlanView({
  callId: props.plan.callId, appliedVariant: props.plan.appliedVariant, variants: props.plan.variants, changes: props.plan.changes
}, (props.plan.mode || undefined) as Parameters<typeof smartcutPlanView>[1], props.plan.kind));
const reasonsOpen = ref(false);
</script>

<template>
  <section class="sc-result" role="dialog" aria-modal="false" :aria-label="view.title" data-testid="smartcut-plan-result" :data-kind="plan.kind">
    <header>
      <Sparkles :size="14" aria-hidden="true" />
      <h2>{{ view.title }}</h2>
      <button type="button" class="sc-btn icon" aria-label="关闭结果卡" title="关闭（「撤销这轮 AI」仍可在「历史」里找到）" @click="emit('close')"><X :size="14" /></button>
    </header>
    <p v-if="view.summary" class="summary" data-testid="smartcut-plan-summary">{{ view.summary }}</p>
    <p v-if="view.fallbackNote" class="sc-warn" data-testid="smartcut-plan-fallback">{{ view.fallbackNote }}</p>

    <div v-if="view.shortage" class="sc-result-out" data-testid="smartcut-plan-shortage">
      <p class="sc-warn">{{ view.shortage.text }}</p>
      <div class="sc-seg">
        <button
          v-for="option in view.shortage.options"
          :key="option.action"
          type="button"
          class="sc-btn"
          :disabled="Boolean(disabledReason)"
          :title="disabledReason || ''"
          :data-action="option.action"
          @click="emit('out', option.action)"
        >{{ option.label }}</button>
      </div>
    </div>

    <ul v-if="view.changes.length" class="sc-result-list changes" aria-label="改了这几处" data-testid="smartcut-plan-changes">
      <li v-for="(change, i) in view.changes" :key="`c${i}`">{{ change }}</li>
    </ul>
    <ul v-if="view.warnings.length" class="sc-result-list warnings" aria-label="提醒" data-testid="smartcut-plan-warnings">
      <li v-for="(warning, i) in view.warnings" :key="`w${i}`">{{ warning }}</li>
    </ul>
    <ul v-if="view.notes.length" class="sc-result-list notes" aria-label="说明">
      <li v-for="(note, i) in view.notes" :key="`n${i}`">{{ note }}</li>
    </ul>

    <div v-if="view.reasons.length" class="sc-result-reasons">
      <button type="button" class="sc-btn" :aria-expanded="reasonsOpen" @click="reasonsOpen = !reasonsOpen">
        每段的理由（{{ view.reasons.length }}）<ChevronUp v-if="reasonsOpen" :size="13" /><ChevronDown v-else :size="13" />
      </button>
      <ul v-if="reasonsOpen" class="sc-result-list" data-testid="smartcut-plan-reasons">
        <li v-for="item in view.reasons" :key="item.clip"><strong>编号 {{ item.clip }}</strong>：{{ item.reason }}</li>
      </ul>
    </div>

    <VariantSwitcher
      v-if="plan.variants.length > 1"
      :variants="plan.variants"
      :applied-variant="plan.appliedVariant"
      :disabled-reason="disabledReason"
      @select="index => emit('select', index)"
    />

    <footer>
      <button
        type="button"
        class="sc-btn"
        :disabled="Boolean(undoDisabledReason)"
        :title="undoDisabledReason || '回到这轮 AI 写入之前（之后的手动改动会先确认）'"
        data-testid="smartcut-undo-ai"
        @click="emit('undo')"
      ><Undo2 :size="13" />撤销这轮 AI</button>
      <button type="button" class="sc-btn" @click="emit('close')">关闭</button>
    </footer>
  </section>
</template>

<style scoped>
.sc-result {
  display: grid;
  gap: 8px;
  width: min(380px, 100%);
  max-height: 100%;
  padding: 12px;
  overflow-y: auto;
  border: 1px solid var(--portal-line);
  border-radius: 10px;
  color: var(--portal-text);
  background: var(--portal-elevated);
  box-shadow: 0 10px 30px var(--portal-shadow);
  font-size: 12px;
}
.sc-result header { display: flex; align-items: center; gap: 6px; color: var(--portal-cyan); }
.sc-result h2 { flex: 1; margin: 0; color: var(--portal-text); font-size: 13px; font-weight: 600; }
.sc-result .summary { margin: 0; color: var(--portal-text-soft); line-height: 1.5; }
.sc-result-out { display: grid; gap: 6px; padding: 8px; border: 1px dashed var(--portal-gold-line); border-radius: 6px; }
.sc-result-list { display: grid; gap: 3px; margin: 0; padding-left: 16px; line-height: 1.5; }
.sc-result-list.warnings { color: var(--portal-gold); }
.sc-result-list.notes { color: var(--portal-muted); }
.sc-result-reasons { display: grid; gap: 6px; justify-items: start; }
.sc-result footer { display: flex; justify-content: flex-end; gap: 6px; padding-top: 4px; border-top: 1px solid var(--portal-line); }
</style>
