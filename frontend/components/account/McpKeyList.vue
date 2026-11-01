<script setup lang="ts">
/**
 * 访问密钥列表（MCPT-11F）：名称、前缀、工作区、权限、工具集、到期、最近使用、撤销。
 *
 * ⚠️ 列表里**从来没有明文**（服务端也不给）—— 只有用来认出它的前缀。
 * ⚠️ 到期 / 过期只按服务端给的 `status` 与 `expiresAt` 显示（见 utils/mcpKeys.expiryText）。
 */
import { KeyRound } from "@lucide/vue";
import {
  expiryText,
  keyPrefixText,
  lastUsedText,
  scopesText,
  statusText,
  toolsetsText,
  type McpAccessKey,
} from "~/utils/mcpKeys";

defineProps<{
  keys: McpAccessKey[];
  /** workspaceId → 名称（取自工作区列表；已不是成员的工作区查不到名字）。 */
  workspaceNames: Record<string, string>;
  /** 正在撤销的那一把（按钮转圈、防连点）。 */
  revokingId?: string | null;
  /** 可以切换「花钱需在 seeAitv 页面确认」（MCPT-13 的 PATCH /me/mcp/grants/{grantId}，MCPT-14 接入）。 */
  allowPageConfirm?: boolean;
}>();
const emit = defineEmits<{ revoke: [key: McpAccessKey]; togglePageConfirm: [key: McpAccessKey, on: boolean] }>();
</script>

<template>
  <section class="mcp-key-list">
    <article v-for="key in keys" :key="key.id" class="mcp-key-row" :class="key.status">
      <span class="order-icon"><KeyRound :size="16" /></span>
      <div class="mcp-key-main">
        <strong>{{ key.clientName }}</strong>
        <small><code>{{ keyPrefixText(key.prefix) }}</code> · {{ workspaceNames[key.workspaceId] ?? `工作区 ${key.workspaceId}` }}</small>
        <small>权限：{{ scopesText(key.scopes) }}</small>
        <small>工具集：{{ toolsetsText(key.toolsets) }}</small>
      </div>
      <div class="mcp-key-meta">
        <small>{{ expiryText(key) }}</small>
        <small>最近使用：{{ lastUsedText(key.lastUsedAt) }}</small>
        <label v-if="allowPageConfirm && key.status === 'active'" class="mcp-inline-check">
          <input
            type="checkbox"
            :checked="key.requirePageConfirm"
            :disabled="revokingId === key.id"
            @change="emit('togglePageConfirm', key, ($event.target as HTMLInputElement).checked)"
          >
          <small>花钱需在 seeAitv 页面确认</small>
        </label>
      </div>
      <div class="mcp-key-actions">
        <span class="order-status" :class="key.status === 'active' ? 'paid' : ''">{{ statusText(key.status) }}</span>
        <button
          v-if="key.status !== 'revoked'"
          type="button"
          class="secondary-command danger"
          :disabled="revokingId === key.id"
          @click="emit('revoke', key)"
        >{{ revokingId === key.id ? '撤销中…' : '撤销' }}</button>
      </div>
    </article>
  </section>
</template>
