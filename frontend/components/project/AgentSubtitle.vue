<script setup lang="ts">
/**
 * Agent 项目卡片的副标题（AGT-45）：最近会话标题 + 状态（进行中 / 等你确认 / 空闲 / 失败 / 已结束）。
 *
 * 项目名取的是首句前 20 字，同名很常见；副标题是用户区分「是哪一次对话」的主要依据。
 * 文案与配色档位来自 `utils/projectKind`（有单测），这里只负责摆放。
 */
import { agentSessionTitle, agentStateText, agentStateTone, type ProjectAgentSummary } from "~/utils/projectKind";

const props = defineProps<{ agent: ProjectAgentSummary }>();
const title = computed(() => agentSessionTitle(props.agent));
const tone = computed(() => agentStateTone(props.agent.state));
const stateText = computed(() => agentStateText(props.agent.state));
</script>

<template>
  <p class="project-agent-sub" :title="`${title} · ${stateText}`">
    <span class="project-agent-sub-title">{{ title }}</span>
    <span class="project-agent-sub-state" :class="`tone-${tone}`">{{ stateText }}</span>
  </p>
</template>

<style scoped>
.project-agent-sub {
  display: flex;
  align-items: center;
  gap: 6px;
  min-width: 0;
  margin: 0 0 4px;
  color: var(--portal-muted);
  font-size: 12px;
}
.project-agent-sub-title {
  min-width: 0;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.project-agent-sub-state {
  flex: none;
  padding: 1px 6px;
  border-radius: 999px;
  color: var(--portal-muted);
  background: var(--portal-surface-3);
  font-size: 11px;
  line-height: 16px;
}
.project-agent-sub-state.tone-running { color: var(--portal-cyan); background: var(--portal-cyan-soft); }
.project-agent-sub-state.tone-waiting { color: var(--portal-gold); background: var(--portal-gold-soft); }
.project-agent-sub-state.tone-failed { color: var(--portal-danger); background: var(--portal-danger-soft); }
.project-agent-sub-state.tone-ended { color: var(--portal-faint); }
</style>
