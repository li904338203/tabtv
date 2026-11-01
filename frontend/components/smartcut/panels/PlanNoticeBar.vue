<script setup lang="ts">
/**
 * AI 剪辑没写进去时的提示条（SCT-31）：
 * - 剪辑期间时间线被改过（40902，SC-F-86）：说清是谁改的，给「仍然应用（覆盖这些改动，可撤销）」与「不应用」；
 * - 42217（SC-UX-09）：说明与可点的出路（按服务端的 `details.options`）；
 * - 被限流（42901 / 42902）：说多久后可以再试，倒数到 0 为止；
 * - 其余失败：一句话（按业务码分流，`smartcutPlanErrorText`）。
 */
import { CircleAlert, X } from "@lucide/vue";
import { smartcutForceApplyLabel, smartcutLimitText, type SmartcutPlanFailure, type SmartcutUnsuitableAction } from "~/utils/smartcutPlan";

const props = defineProps<{
  failure: SmartcutPlanFailure;
  /** 服务端带回了调用记录（方案已经生成），可以「仍然应用」。 */
  canForce: boolean;
  /** 出路不能点的原因（只读、进行中）；空 = 能点。 */
  disabledReason?: string;
  /** 被限流时还要等多少秒（每秒刷新）。 */
  retryAfter: number;
}>();
const emit = defineEmits<{ out: [action: SmartcutUnsuitableAction]; force: []; keep: []; dismiss: [] }>();

const kind = computed(() => (props.failure.conflict ? "conflict" : props.failure.unsuitable ? "unsuitable" : props.failure.limit ? "limited" : "error"));
const text = computed(() => {
  const limit = props.failure.limit;
  if (!limit || !props.failure.retryAfterSeconds) return props.failure.text;
  // 限流：按剩下的秒数倒数；等到了就说可以再试了（不让用户对着一个已经过期的时间干等）。
  return props.retryAfter > 0 ? smartcutLimitText(limit, props.retryAfter) : "现在可以再试了";
});
</script>

<template>
  <div class="sc-notice" :data-kind="kind" role="alert" data-testid="smartcut-plan-notice">
    <CircleAlert :size="14" aria-hidden="true" />
    <p>{{ text }}</p>
    <div class="sc-seg">
      <template v-if="kind === 'conflict'">
        <button v-if="canForce" type="button" class="sc-btn" :disabled="Boolean(disabledReason)" :title="disabledReason || ''" data-testid="smartcut-force-apply" @click="emit('force')">{{ smartcutForceApplyLabel }}</button>
        <button type="button" class="sc-btn" @click="emit('keep')">不应用</button>
      </template>
      <template v-else-if="kind === 'unsuitable'">
        <button
          v-for="option in failure.unsuitable!.options"
          :key="option.action"
          type="button"
          class="sc-btn"
          :disabled="Boolean(disabledReason)"
          :title="disabledReason || ''"
          :data-action="option.action"
          @click="emit('out', option.action)"
        >{{ option.label }}</button>
      </template>
    </div>
    <button type="button" class="sc-btn icon" aria-label="关闭提示" @click="emit('dismiss')"><X :size="13" /></button>
  </div>
</template>

<style scoped>
.sc-notice {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 8px;
  min-height: 36px;
  padding: 4px 10px 4px 14px;
  border-bottom: 1px solid var(--portal-line);
  color: var(--portal-text);
  background: var(--portal-panel);
  font-size: 12px;
}
.sc-notice[data-kind="conflict"],
.sc-notice[data-kind="unsuitable"],
.sc-notice[data-kind="limited"] { border-bottom-color: var(--portal-gold-line); background: var(--portal-gold-soft); }
.sc-notice[data-kind="error"] { border-bottom-color: var(--portal-danger-line); }
.sc-notice > svg { flex-shrink: 0; color: var(--portal-gold); }
.sc-notice[data-kind="error"] > svg { color: var(--portal-danger); }
.sc-notice p { flex: 1; min-width: 180px; margin: 0; line-height: 1.5; }
</style>
