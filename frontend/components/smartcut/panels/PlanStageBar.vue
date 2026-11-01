<script setup lang="ts">
/**
 * AI 剪辑的阶段条（SCT-31，SC-F-80）：同步素材 → 理解素材 → 规划 → 装配。阶段只来自服务端（`smartcut.updated`
 * 推进 `store.planRunning.stage`），前端不按耗时猜；事件丢了也没关系，请求本身返回时阶段条就收起。
 * 切换方案只是一次重新编译与写入，没有阶段可看，只显示「切换方案中」。
 */
import { LoaderCircle } from "@lucide/vue";
import type { SmartcutPlanRunning } from "~/stores/smartcut";
import { smartcutPlanSteps } from "~/utils/smartcutPlan";

const props = defineProps<{ running: SmartcutPlanRunning }>();

const steps = computed(() => smartcutPlanSteps(props.running.stage));
const label = computed(() => {
  switch (props.running.kind) {
    case "edit": return "追加修改中";
    case "variant": return "切换方案中";
    default: return "AI 剪辑中";
  }
});

const now = ref(Date.now());
const timer = setInterval(() => { now.value = Date.now(); }, 1000);
onBeforeUnmount(() => clearInterval(timer));
const elapsed = computed(() => Math.max(0, Math.round((now.value - props.running.startedAt) / 1000)));
</script>

<template>
  <div class="sc-stagebar" role="status" aria-live="polite" data-testid="smartcut-plan-stage" :data-stage="running.stage" :data-kind="running.kind">
    <LoaderCircle class="sc-spin" :size="14" aria-hidden="true" />
    <strong>{{ label }}</strong>
    <ol v-if="running.kind !== 'variant'" class="sc-stagebar-steps" aria-label="进度">
      <li v-for="step in steps" :key="step.key" :data-state="step.state" :aria-current="step.state === 'current' ? 'step' : undefined">{{ step.label }}</li>
    </ol>
    <span class="sc-note">已用 {{ elapsed }} 秒 · 免费</span>
  </div>
</template>

<style scoped>
.sc-stagebar {
  display: flex;
  align-items: center;
  gap: 10px;
  min-height: 34px;
  padding: 0 14px;
  border-bottom: 1px solid var(--portal-cyan-line);
  color: var(--portal-text);
  background: var(--portal-cyan-soft);
  font-size: 12px;
}
.sc-stagebar strong { font-weight: 600; }
.sc-stagebar-steps { display: flex; align-items: center; gap: 6px; margin: 0; padding: 0; list-style: none; }
.sc-stagebar-steps li { display: inline-flex; align-items: center; gap: 6px; color: var(--portal-muted); }
.sc-stagebar-steps li + li::before { content: "→"; color: var(--portal-muted); }
.sc-stagebar-steps li[data-state="done"] { color: var(--portal-text-soft); }
.sc-stagebar-steps li[data-state="current"] { color: var(--portal-cyan); font-weight: 600; }
.sc-stagebar .sc-note { margin-left: auto; }
</style>
