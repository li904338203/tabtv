<script setup lang="ts">
/**
 * 卡片提交后的只读摘要（设计 03 §3.4：提交后折叠、标「已提交」，不可再改 —— 对标 XYQ-97）。
 * 提交了什么由服务端写成一条用户消息（「我选择了：…」），这里不再复述，只给标题与结论。
 */
import { CircleCheck, CircleSlash } from "@lucide/vue";
import { CARD_STATUS_LABELS } from "~/utils/agentMessages";

defineProps<{ title: string; status: string; detail?: string }>();
</script>

<template>
  <div class="agent-card is-closed" :class="`is-${status}`">
    <CircleCheck v-if="status === 'submitted'" :size="14" />
    <CircleSlash v-else :size="14" />
    <span class="agent-card-closed-title">{{ title }}</span>
    <em>{{ CARD_STATUS_LABELS[status] ?? status }}</em>
    <small v-if="detail">{{ detail }}</small>
  </div>
</template>
