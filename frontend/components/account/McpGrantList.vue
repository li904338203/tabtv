<script setup lang="ts">
/**
 * 已连接的客户端 / 本工作区连接（MCPT-14）：客户端名、来源（CIMD 已验证域名 / DCR 未验证 / 访问密钥）、
 * 工作区或成员、权限、工具集、最近使用、「花钱需页面确认」、状态、撤销。
 *
 * 「花钱需页面确认」的切换只在本人的连接上给（`allowPageConfirm`）：那是用户替自己做的决定，
 * 工作区管理员只能撤销同事的连接，不能替他改（服务端的 PATCH 也只认本人）。
 */
import { Cable } from "@lucide/vue";
import { lastUsedText, scopesText, toolsetsText } from "~/utils/mcpKeys";
import { clientSourceText, connectionUserText, grantStatusText, type McpConnection, type McpGrant } from "~/utils/mcpConnections";

const props = defineProps<{
  grants: Array<McpGrant | McpConnection>;
  /** workspaceId → 名称（本人视角显示工作区）。 */
  workspaceNames?: Record<string, string>;
  /** 工作区视角：显示是哪位成员的连接。 */
  showUser?: boolean;
  /** 本人视角：可以切换「花钱需在 seeAitv 页面确认」。 */
  allowPageConfirm?: boolean;
  /** 正在处理的那一条（按钮转圈、防连点）。 */
  busyId?: string | null;
}>();
const emit = defineEmits<{
  revoke: [grant: McpGrant | McpConnection];
  togglePageConfirm: [grant: McpGrant, on: boolean];
}>();

function owner(g: McpGrant | McpConnection): string {
  if (props.showUser && "userId" in g) return connectionUserText(g);
  return props.workspaceNames?.[g.workspaceId] ?? `工作区 ${g.workspaceId}`;
}
</script>

<template>
  <section class="mcp-key-list">
    <article v-for="g in grants" :key="g.id" class="mcp-key-row" :class="g.status">
      <span class="order-icon"><Cable :size="16" /></span>
      <div class="mcp-key-main">
        <strong>{{ g.clientName }}</strong>
        <small>{{ clientSourceText(g) }} · {{ owner(g) }}</small>
        <small>权限：{{ scopesText(g.scopes) }}</small>
        <small>工具集：{{ toolsetsText(g.toolsets) }}</small>
      </div>
      <div class="mcp-key-meta">
        <small>最近使用：{{ lastUsedText(g.lastUsedAt) }}</small>
        <label v-if="allowPageConfirm && g.status === 'active'" class="mcp-inline-check">
          <input
            type="checkbox"
            :checked="g.requirePageConfirm"
            :disabled="busyId === g.id"
            @change="emit('togglePageConfirm', g, ($event.target as HTMLInputElement).checked)"
          >
          <small>花钱需在 seeAitv 页面确认</small>
        </label>
        <small v-else>{{ g.requirePageConfirm ? '花钱需在 seeAitv 页面确认' : '花钱在客户端里确认' }}</small>
      </div>
      <div class="mcp-key-actions">
        <span class="order-status" :class="g.status === 'active' ? 'paid' : ''">{{ grantStatusText(g.status) }}</span>
        <button
          v-if="g.status === 'active'"
          type="button"
          class="secondary-command danger"
          :disabled="busyId === g.id"
          @click="emit('revoke', g)"
        >{{ busyId === g.id ? '处理中…' : '撤销' }}</button>
      </div>
    </article>
  </section>
</template>
