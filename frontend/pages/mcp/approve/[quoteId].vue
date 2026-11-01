<script setup lang="ts">
/**
 * 页面审批 `/mcp/approve/[quoteId]`（MCPT-13，MCP-F-44 / MCP-AC-15）。
 *
 * 凭据开了「花钱需在 seeAitv 页面确认」时，外部客户端的花钱工具返回指向这里的审批链接。只有**发起授权的
 * 同一个用户本人**能看到金额并批准 / 拒绝（服务端按 quote 所属授权的 user_id 判，非本人 403 且不回内容）。
 *
 * ⚠️ 页面打开时**只发 GET**；批准 / 拒绝只在用户点击之后发生。
 * ⚠️ 非本人那一屏不显示任何报价内容（客户端名、金额、工作区），只说「换账号」—— 见 utils/mcpApproval。
 * ⚠️ 不套 platform 布局（同同意页）。
 */
import { Check, LogOut, X } from "@lucide/vue";
import McpGatePanel from "~/components/mcp/McpGatePanel.vue";
import { formatDateTime } from "~/utils/mcpKeys";
import {
  afterText,
  approveErrorText,
  errorScreen,
  payerText,
  stateScreen,
  type ApprovalScreen,
  type McpApproval,
} from "~/utils/mcpApproval";
import { loginPath } from "~/utils/safeRedirect";

definePageMeta({ middleware: "auth" });
useHead({ title: "确认花费 · seeAitv" });

const route = useRoute();
const auth = useAuthStore();

const quoteId = computed(() => {
  const v = route.params.quoteId;
  return typeof v === "string" ? v : "";
});
const screen = ref<ApprovalScreen | null>(null);
const loading = ref(true);
const acting = ref<"approve" | "reject" | null>(null);
const actionError = ref("");

const approval = computed<McpApproval | null>(() => (screen.value?.kind === "approval" ? screen.value.approval : null));

function online() {
  return !import.meta.client || navigator.onLine;
}

async function load() {
  loading.value = true;
  try {
    const a = await useApi()<McpApproval>(`/mcp/approvals/${encodeURIComponent(quoteId.value)}`);
    screen.value = stateScreen(a);
  } catch (cause) {
    screen.value = errorScreen(cause, online());
  } finally {
    loading.value = false;
  }
}

async function act(kind: "approve" | "reject") {
  if (acting.value || screen.value?.kind !== "approval") return;
  acting.value = kind;
  actionError.value = "";
  try {
    // 服务端回的是写入之后（或当前真实）的状态：重复点、另一个标签页先点了，都按它显示。
    const a = await useApi()<McpApproval>(`/mcp/approvals/${encodeURIComponent(quoteId.value)}/${kind}`, { method: "POST" });
    screen.value = stateScreen(a);
  } catch (cause) {
    const short = approveErrorText(cause);
    if (short) {
      actionError.value = short;
      return;
    }
    const next = errorScreen(cause, online());
    // 非本人 / 撤销 / 不存在：整屏切换；其余（断网、500）留在原屏只提示，不丢掉用户正看着的金额。
    if (next.kind === "error") actionError.value = next.text;
    else screen.value = next;
  } finally {
    acting.value = null;
  }
}

/** 非本人：退出后带着本页地址去登录，换回发起授权的账号。 */
async function switchAccount() {
  try {
    await auth.logout();
  } catch {
    // 退出接口失败也已清掉本地会话（auth.logout 的 finally）：照样去登录页。
  }
  await navigateTo(loginPath(route.fullPath));
}

onMounted(load);
</script>

<template>
  <McpGatePanel badge="花费确认">
    <div v-if="loading && !screen" class="approve-state">正在加载审批…</div>

    <template v-else-if="screen && screen.kind === 'approval' && approval">
      <div class="approve-head" :class="screen.tone">
        <h1>{{ screen.title }}</h1>
        <p>{{ screen.text }}</p>
      </div>

      <dl class="approve-facts">
        <div><dt>客户端</dt><dd>{{ approval.clientName }}</dd></div>
        <div><dt>工作区</dt><dd>{{ approval.workspaceName }}（{{ approval.workspaceKind === "team" ? "团队" : "个人" }}）</dd></div>
        <div><dt>要做什么</dt><dd>{{ approval.summary }}</dd></div>
      </dl>

      <table class="approve-items">
        <tbody>
          <tr v-for="(it, i) in approval.items" :key="i"><td>{{ it.label }}</td><td>{{ it.credits }} 积分</td></tr>
        </tbody>
        <tfoot>
          <tr><td>合计</td><td>{{ approval.credits }} 积分</td></tr>
        </tfoot>
      </table>

      <dl class="approve-facts compact">
        <div><dt>付费账户</dt><dd>{{ payerText(approval) }}</dd></div>
        <div><dt>当前余额</dt><dd>{{ approval.balance }}</dd></div>
        <div><dt>确认后剩余</dt><dd :class="{ danger: approval.shortfall > 0 }">{{ afterText(approval) }}</dd></div>
        <div v-if="approval.state === 'pending'"><dt>有效期至</dt><dd>{{ formatDateTime(approval.expiresAt) }}</dd></div>
      </dl>

      <p v-if="screen.approveBlocked" class="auth-message error">{{ screen.approveBlocked }}</p>
      <p v-if="actionError" class="auth-message error">{{ actionError }}</p>

      <div v-if="screen.canApprove || screen.canReject" class="approve-actions">
        <button type="button" class="secondary-command danger" :disabled="!screen.canReject || !!acting" @click="act('reject')">
          <X :size="14" />{{ acting === "reject" ? "处理中" : "拒绝" }}
        </button>
        <button type="button" class="auth-submit" :disabled="!screen.canApprove || !!acting" @click="act('approve')">
          <Check :size="15" /><span>{{ acting === "approve" ? "处理中" : `确认花费 ${approval.credits} 积分` }}</span>
        </button>
      </div>
    </template>

    <div v-else-if="screen" class="approve-state">
      <h1>{{ screen.title }}</h1>
      <p class="auth-message" :class="screen.kind === 'error' ? 'error' : ''">{{ screen.text }}</p>
      <button v-if="screen.kind === 'not_owner'" type="button" class="auth-submit" @click="switchAccount">
        <LogOut :size="15" /><span>退出并换账号登录</span>
      </button>
      <button v-if="screen.kind === 'error' && screen.retry" type="button" class="auth-submit" @click="load">重试</button>
    </div>
  </McpGatePanel>
</template>

<style scoped>
h1 { margin: 18px 0 8px; font-size: 18px; }
.approve-state { display: grid; gap: 12px; padding: 4px 0; color: var(--portal-muted); font-size: 12px; }
.approve-head { border-left: 3px solid var(--portal-line-strong); padding: 2px 0 2px 12px; }
.approve-head.pending { border-color: var(--portal-gold); }
.approve-head.success { border-color: var(--portal-green); }
.approve-head.danger { border-color: var(--portal-danger); }
.approve-head h1 { margin-top: 18px; }
.approve-head p { margin: 0; color: var(--portal-text-soft); font-size: 12px; line-height: 1.6; }
.approve-facts { display: grid; gap: 8px; margin: 16px 0 0; }
.approve-facts > div { display: grid; grid-template-columns: 84px minmax(0,1fr); gap: 10px; font-size: 11px; }
.approve-facts dt { color: var(--portal-muted); }
.approve-facts dd { margin: 0; color: var(--portal-text); line-height: 1.5; word-break: break-word; }
.approve-facts dd.danger { color: var(--portal-danger); }
.approve-facts.compact { margin-top: 12px; }
.approve-items { width: 100%; margin-top: 14px; border-collapse: collapse; font-size: 11px; }
.approve-items td { border-top: 1px solid var(--portal-line-soft); padding: 8px 0; }
.approve-items td:last-child { text-align: right; white-space: nowrap; font-variant-numeric: tabular-nums; }
.approve-items tfoot td { border-top: 1px solid var(--portal-line-strong); font-size: 13px; font-weight: 700; }
.approve-items tfoot td:last-child { color: var(--portal-gold); }
.approve-actions { display: grid; grid-template-columns: 1fr 2fr; gap: 10px; margin-top: 18px; }
.approve-actions .secondary-command { justify-content: center; min-height: 42px; font-size: 11px; cursor: pointer; }
.auth-message { margin-top: 12px; }
</style>
