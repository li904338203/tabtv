<script setup lang="ts">
/**
 * 首次打开编辑器的引导（DRT-15，DR-UX-06）：三步 —— 摆角色 → 调机位 → 截图。
 * 关闭后写 localStorage，之后不再出现；系统开了「减少动态效果」时不播放动画（DR-N-17，CSS 媒体查询）。
 * 存储不可用（隐私模式）时照常显示、照常能关：只是下次还会出现，不影响编辑。
 */
const STORAGE_KEY = "seeaitv:director-guide-dismissed";

const steps = [
  { title: "摆角色", text: "从左侧「添加」放入素模，拖动、旋转，或在右侧填数值；在「姿势」里选一个动作。" },
  { title: "调机位", text: "选中机位调焦距与画幅，切到「机位视角」看取景；按住 W A S D 可以自由走位找角度。" },
  { title: "截图", text: "顶栏「截图」截下输出机位的画面，连到图片或视频节点作为构图参考。" }
];

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
  if (index.value < steps.length - 1) index.value += 1;
  else dismiss();
}
</script>

<template>
  <aside v-if="visible" class="studio-guide" role="dialog" aria-label="导演台入门" data-testid="director-guide">
    <ol class="studio-guide-steps">
      <li v-for="(step, i) in steps" :key="step.title" :class="{ active: i === index, done: i < index }">
        <span>{{ i + 1 }}</span>{{ step.title }}
      </li>
    </ol>
    <p>{{ steps[index]?.text }}</p>
    <div class="studio-guide-actions">
      <button type="button" class="ghost" @click="dismiss">跳过</button>
      <button type="button" class="primary" @click="next">{{ index < steps.length - 1 ? "下一步" : "开始" }}</button>
    </div>
  </aside>
</template>

<style scoped>
.studio-guide {
  position: absolute;
  left: 50%;
  bottom: 24px;
  z-index: 25;
  width: min(440px, calc(100% - 32px));
  padding: 14px 16px;
  border: 1px solid var(--portal-line-strong);
  border-radius: 10px;
  color: var(--portal-text);
  background: var(--portal-elevated);
  box-shadow: 0 10px 30px var(--portal-shadow);
  transform: translateX(-50%);
  animation: guide-in 240ms ease-out;
}
@keyframes guide-in { from { opacity: 0; transform: translate(-50%, 8px); } }
@media (prefers-reduced-motion: reduce) { .studio-guide { animation: none; } }
.studio-guide-steps { display: flex; gap: 14px; margin: 0 0 10px; padding: 0; list-style: none; color: var(--portal-muted); font-size: 12px; }
.studio-guide-steps li { display: flex; align-items: center; gap: 6px; }
.studio-guide-steps li span { width: 18px; height: 18px; display: grid; place-items: center; border: 1px solid var(--portal-line-strong); border-radius: 50%; font-size: 11px; }
.studio-guide-steps li.active { color: var(--portal-text); font-weight: 600; }
.studio-guide-steps li.active span { border-color: transparent; color: var(--portal-on-accent); background: var(--portal-text); }
.studio-guide p { margin: 0 0 12px; color: var(--portal-text-soft); font-size: 13px; line-height: 1.6; }
.studio-guide-actions { display: flex; justify-content: flex-end; gap: 8px; }
.studio-guide-actions button { height: 28px; padding: 0 12px; border: 1px solid var(--portal-line-strong); border-radius: 6px; color: var(--portal-text); background: transparent; font-size: 12px; cursor: pointer; }
.studio-guide-actions button.primary { border-color: transparent; color: var(--portal-on-accent); background: var(--portal-text); }
.studio-guide-actions button:hover { background: var(--portal-hover); }
.studio-guide-actions button.primary:hover { opacity: 0.9; background: var(--portal-text); }
</style>
