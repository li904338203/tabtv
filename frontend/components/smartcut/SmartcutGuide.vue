<script setup lang="ts">
/**
 * 首次打开剪辑台的引导（SCT-20，SC-UX-14）：三步 —— 时间线怎么用 → AI 剪辑 → 导出。
 * 关闭后写 localStorage，之后不再出现；系统开了「减少动态效果」时不播放动画（SCD-53）。
 * 存储不可用（隐私模式）时照常显示、照常能关：只是下次还会出现，不影响编辑。
 */
import { smartcutGuideStepsFor } from "~/utils/smartcutStudio";

const STORAGE_KEY = "seeaitv:smartcut-guide-dismissed";

/** AI 关着时（这一版只保留手动剪辑）不讲 AI 剪辑那一步。 */
const smartcutGuideSteps = smartcutGuideStepsFor(useSmartcutFeatures().ai);
const visible = ref(false);
const index = ref(0);

onMounted(() => {
  try {
    visible.value = localStorage.getItem(STORAGE_KEY) !== "1";
  } catch {
    visible.value = true;
  }
});

function dismiss() {
  visible.value = false;
  try {
    localStorage.setItem(STORAGE_KEY, "1");
  } catch {
    // 存不进去只影响下一次是否再出现
  }
}

function next() {
  if (index.value < smartcutGuideSteps.length - 1) index.value += 1;
  else dismiss();
}
</script>

<template>
  <aside v-if="visible" class="sc-guide" role="dialog" aria-label="智能剪辑入门" data-testid="smartcut-guide">
    <ol class="sc-guide-steps">
      <li v-for="(step, i) in smartcutGuideSteps" :key="step.title" :class="{ active: i === index, done: i < index }">
        <span>{{ i + 1 }}</span>{{ step.title }}
      </li>
    </ol>
    <p>{{ smartcutGuideSteps[index]?.text }}</p>
    <div class="sc-guide-actions">
      <button type="button" class="sc-btn" @click="dismiss">跳过</button>
      <button type="button" class="sc-btn primary" @click="next">{{ index < smartcutGuideSteps.length - 1 ? "下一步" : "开始" }}</button>
    </div>
  </aside>
</template>

<style scoped>
.sc-guide {
  position: absolute;
  right: 24px;
  bottom: 24px;
  z-index: 25;
  width: min(440px, calc(100% - 32px));
  padding: 14px 16px;
  border: 1px solid var(--portal-line-strong);
  border-radius: 10px;
  color: var(--portal-text);
  background: var(--portal-elevated);
  box-shadow: 0 10px 30px var(--portal-shadow);
  animation: sc-guide-in 240ms ease-out;
}
@keyframes sc-guide-in { from { opacity: 0; transform: translateY(8px); } }
@media (prefers-reduced-motion: reduce) { .sc-guide { animation: none; } }
.sc-guide-steps { display: flex; gap: 14px; margin: 0 0 10px; padding: 0; list-style: none; color: var(--portal-muted); font-size: 12px; }
.sc-guide-steps li { display: flex; align-items: center; gap: 6px; }
.sc-guide-steps li span { width: 18px; height: 18px; display: grid; place-items: center; border: 1px solid var(--portal-line-strong); border-radius: 50%; font-size: 11px; }
.sc-guide-steps li.active { color: var(--portal-text); font-weight: 600; }
.sc-guide-steps li.active span { border-color: transparent; color: var(--portal-on-accent); background: var(--portal-text); }
.sc-guide p { margin: 0 0 12px; color: var(--portal-text-soft); font-size: 13px; line-height: 1.6; }
.sc-guide-actions { display: flex; justify-content: flex-end; gap: 8px; }
</style>
