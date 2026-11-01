<script setup lang="ts">
/**
 * 完成后的下一步建议（设计 03 §3.8）：点击即作为用户消息发送。
 * 不能发（只读 / 非发起人 / 正在发）时照样显示，但点不动并说明原因。
 */
defineProps<{ items: string[]; disabledReason?: string }>();
const emit = defineEmits<{ pick: [text: string] }>();
</script>

<template>
  <div class="agent-suggestions">
    <span>接下来你可以：</span>
    <div>
      <button
        v-for="item in items"
        :key="item"
        type="button"
        class="agent-chip is-suggestion"
        :disabled="Boolean(disabledReason)"
        :title="disabledReason || '点击发送'"
        @click="emit('pick', item)"
      >
        {{ item }}
      </button>
    </div>
  </div>
</template>
