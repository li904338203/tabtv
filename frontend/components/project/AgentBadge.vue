<script setup lang="ts">
/**
 * 项目卡片封面左上角的角标 —— 工作台、AI 视频页、全部项目页共用。
 *
 * - `agent`（AGT-45）：列表项带 `agent`（服务端算的：项目有过 Agent 会话）时出现。
 * - `marketing`（MKT-F-61）：项目 `origin === "marketing"`（服务端建项目时落的列）时出现。
 *   营销项目同时是 Agent 项目，但**只显示这一个**角标（两个挤在左上角会互相压住）。
 *
 * 显示哪一个由 `utils/projectKind.projectBadgeVariant` 决定（有单测），前端不自己判类型。
 * ⚠️ 父级封面按钮要 `position: relative`（main.css 里三种封面已加）。
 */
import type { ProjectBadgeVariant } from "~/utils/projectKind";

const props = withDefaults(defineProps<{ variant?: ProjectBadgeVariant }>(), { variant: "agent" });
const label = computed(() => (props.variant === "marketing" ? "✦ 营销" : "✦ Agent"));
const hint = computed(() => (props.variant === "marketing" ? "营销项目：点开进入对话" : "Agent 项目：点开进入对话"));
</script>

<template>
  <span class="project-agent-badge" :class="`variant-${props.variant}`" :title="hint">{{ label }}</span>
</template>

<style scoped>
.project-agent-badge {
  position: absolute;
  top: 7px;
  left: 7px;
  z-index: 1;
  display: inline-flex;
  align-items: center;
  height: 20px;
  padding: 0 7px;
  border: 1px solid var(--portal-cyan-line);
  border-radius: 999px;
  color: var(--portal-cyan);
  background: var(--portal-surface);
  font-size: 11px;
  font-weight: 600;
  line-height: 1;
  pointer-events: none;
}
.project-agent-badge.variant-marketing {
  border-color: var(--portal-gold-line);
  color: var(--portal-gold);
}
</style>
