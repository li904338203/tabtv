<script setup lang="ts">
/**
 * 提示词框里的运镜标签（对标产品同款）：图标 + 运镜名，悬停出现 ×。点名字重新打开运镜广场换一个。
 * 节点里只存 `params.camera = {code, name}`，提示词由服务端拼 —— 标签只是显示，不往提示词里写字。
 */
import { Camera, X } from "@lucide/vue";

defineProps<{ name: string }>();
const emit = defineEmits<{ open: []; remove: [] }>();
</script>

<template>
  <span class="node-camera-chip" :title="`运镜：${name}`">
    <button class="node-camera-chip-main" type="button" title="换一个运镜" @click="emit('open')">
      <i aria-hidden="true"><Camera :size="11" /></i>
      <span>{{ name }}</span>
    </button>
    <button class="node-camera-chip-remove" type="button" :aria-label="`去掉运镜「${name}」`" title="去掉运镜" @click="emit('remove')"><X :size="11" /></button>
  </span>
</template>
