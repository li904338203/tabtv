<script setup lang="ts">
/**
 * MCP 接入指南页的客户端标识：仓库内自绘的单色简化图形（不是各家品牌素材，也不从网络取）。
 *
 * 线条一律 `currentColor`，底色由调用方的 class 决定，亮 / 暗主题跟着 token 走。
 * 认不出来的客户端码显示首字母徽标（服务端加了新客户端时前端不用先发版）。
 */
import { clientInitial } from "~/utils/mcpGuide";

const props = withDefaults(defineProps<{ client: string; label?: string; size?: number }>(), { label: "", size: 20 });

const KNOWN = new Set(["seeaitv", "claude_code", "codex", "cursor", "gemini_cli", "opencode", "workbuddy", "stdio_bridge"]);
const known = computed(() => KNOWN.has(props.client));
const initial = computed(() => clientInitial(props.label || props.client));
</script>

<template>
  <svg
    v-if="known"
    class="mcp-client-icon"
    :width="size"
    :height="size"
    viewBox="0 0 24 24"
    fill="none"
    stroke="currentColor"
    stroke-width="1.9"
    stroke-linecap="round"
    stroke-linejoin="round"
    aria-hidden="true"
  >
    <template v-if="client === 'seeaitv'">
      <path d="M8.5 6.8 17.2 12l-8.7 5.2Z" fill="currentColor" stroke="none" />
      <path d="M18.6 3.2v3.2M17 4.8h3.2" stroke-width="1.6" />
      <path d="M4 7.5v9" />
    </template>
    <template v-else-if="client === 'claude_code'">
      <path d="M12 3v5.2M12 15.8V21M3 12h5.2M15.8 12H21M5.6 5.6l3.7 3.7M14.7 14.7l3.7 3.7M18.4 5.6l-3.7 3.7M9.3 14.7l-3.7 3.7" stroke-width="2.2" />
    </template>
    <template v-else-if="client === 'codex'">
      <rect x="3.5" y="4.5" width="17" height="15" rx="4" />
      <path d="m8 10 2.6 2.2L8 14.4M13 14.6h3.2" />
    </template>
    <template v-else-if="client === 'cursor'">
      <path d="M12 3 20 7.5v9L12 21l-8-4.5v-9Z" />
      <path d="M4 7.5 12 12l8-4.5M12 12v9" stroke-width="1.5" />
    </template>
    <template v-else-if="client === 'gemini_cli'">
      <path d="M12 2.5c.6 5.2 4.3 8.9 9.5 9.5-5.2.6-8.9 4.3-9.5 9.5-.6-5.2-4.3-8.9-9.5-9.5 5.2-.6 8.9-4.3 9.5-9.5Z" fill="currentColor" stroke="none" />
    </template>
    <template v-else-if="client === 'opencode'">
      <path d="M9 4.5H5.5v15H9M15 4.5h3.5v15H15" />
      <rect x="10" y="10" width="4" height="4" rx=".8" fill="currentColor" stroke="none" />
    </template>
    <template v-else-if="client === 'workbuddy'">
      <circle cx="9" cy="12" r="5" />
      <circle cx="15" cy="12" r="5" />
    </template>
    <template v-else-if="client === 'stdio_bridge'">
      <path d="M9 3v4.5M15 3v4.5M6.5 7.5h11V11a5.5 5.5 0 0 1-11 0ZM12 16.5V21" />
    </template>
  </svg>
  <span v-else class="mcp-client-initial" :style="{ width: `${size}px`, height: `${size}px`, fontSize: `${Math.round(size * 0.62)}px` }" aria-hidden="true">{{ initial }}</span>
</template>

<style scoped>
.mcp-client-icon { display: block; flex: 0 0 auto; }
.mcp-client-initial { display: inline-grid; place-items: center; flex: 0 0 auto; border-radius: 30%; font-weight: 700; line-height: 1; }
</style>
