<script setup lang="ts">
/**
 * 节点来源角标（AGT-11 → MCPT-09 泛化，原 AgentBadge）：
 *  - 站内 Agent：实心 ✦ = 由 Agent 创建；空心 ✧ = 由 Agent 创建、已被你修改；
 *  - 外部 AI 客户端（MCP）：⌘ 客户端名；已被你修改时变成空心描边样式。
 *
 * 空心不是装饰：用户改过的节点 Agent / MCP **之后不再改它**（服务端所有权规则），
 * 角标是用户唯一能看到这条规则生效的地方。
 *
 * ⚠️ 来源只经 `nodeOrigin` 解析（旧 `agent` 与新 `origin` 两种字段），这里不自己读字段。
 * ⚠️ 项目卡片上的 `components/project/AgentBadge.vue` 是另一个组件，与这里无关。
 */
import { nodeOrigin, originLabel, type OriginCarrier } from "~/utils/canvasOrigin";

const props = defineProps<{ source: OriginCarrier }>();
const origin = computed(() => nodeOrigin(props.source));
const label = computed(() => originLabel(origin.value));
const isMcp = computed(() => origin.value?.kind === "mcp");
</script>

<template>
  <span
    v-if="label"
    class="origin-badge"
    :class="{ touched: origin?.userTouched, mcp: isMcp }"
    :title="label.title"
    :aria-label="label.title"
    role="img"
  ><span class="mark">{{ label.mark }}</span><span v-if="label.text" class="client">{{ label.text }}</span></span>
</template>

<style scoped>
.origin-badge {
  display: inline-flex;
  align-items: center;
  gap: 2px;
  flex: none;
  min-width: 16px;
  height: 16px;
  color: var(--canvas-cyan);
  font-size: 12px;
  line-height: 1;
  cursor: help;
}
.origin-badge .mark { display: inline-grid; place-items: center; width: 16px; }
.origin-badge.touched { color: var(--canvas-muted); }
.origin-badge.mcp { padding: 0 4px 0 0; border: 1px solid var(--canvas-cyan); border-radius: 8px; background: var(--cv-cyan-52a14); }
/* 空心：被你修改过的外部客户端节点只留描边 */
.origin-badge.mcp.touched { border-color: var(--canvas-muted); background: transparent; }
.origin-badge .client { max-width: 96px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; font-size: 11px; }
</style>
