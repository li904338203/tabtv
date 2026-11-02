<script setup lang="ts">
/**
 * 每轮页脚（设计 03 §3.7，R-6 / R-7 按用户要求简化）：显示完整日期时间；纯文字，不能点开。
 * tabtv 不计价（TT-T-14）：原来的「本轮消耗 ✦N」与对话 / 生成 / 退回明细不再显示，悬浮提示只说调用了几次模型。
 */
import { turnFooter, type AgentTurn } from "~/utils/agentMessages";

const props = defineProps<{ turn: AgentTurn }>();
const view = computed(() => turnFooter(props.turn));
</script>

<template>
  <div class="agent-turn-footer" :title="view.calls || undefined">
    <span>{{ view.dateTime }}</span>
  </div>
</template>
