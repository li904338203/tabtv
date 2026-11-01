<script setup lang="ts">
/**
 * OAuth 同意页 `/mcp/authorize?req=…`（MCPT-13，MCP-F-05 / MCP-AC-14）。
 *
 * 浏览器从 `/oauth/authorize` 被 302 到这里，`req` 是服务端签名的请求句柄（10 分钟，内容不可篡改、不含秘密）。
 * 未登录时 `middleware/auth` 带着完整路径（含 `req`）去登录页，登录后回到这里（`safeRedirect` 只认站内路径）。
 *
 * ⚠️ 页面打开时**只发 GET**（同意页数据、工作区、工具集可用性）；授权只在用户点「允许 / 拒绝」之后发生。
 * ⚠️ 回跳**只用服务端响应里的 `redirectTo`**：这是全站唯一允许跳站外的地方，地址从不取 URL 参数 ——
 *    拿 `route.query` 拼地址就是开放重定向（用户在我们的页面上点了「允许」，被带去一个钓鱼站）。
 * ⚠️ 不套 platform 布局：这一页是「替外部客户端做决定」，不该让人误以为在站内操作。
 */
import { AlertTriangle, BadgeCheck, Coins, ExternalLink } from "@lucide/vue";
import McpGatePanel from "~/components/mcp/McpGatePanel.vue";
import {
  PAGE_CONFIRM_HINT,
  clientSource,
  consentAllowBody,
  consentDenyBody,
  consentError,
  consentRoleNotice,
  consentScopeOptions,
  consentToolsetOptions,
  initialConsentForm,
  roleLabel,
  spendNotice,
  toggleConsentScope,
  toggleConsentToolset,
  validateConsentForm,
  type ConsentForm,
  type McpConsentInfo,
  type McpConsentResult,
} from "~/utils/mcpScopes";
import { formatDateTime, type McpScope, type McpToolset, type McpToolsetInfo } from "~/utils/mcpKeys";
import type { McpClientSnippets } from "~/utils/mcpSnippets";

definePageMeta({ middleware: "auth" });
useHead({ title: "授权外部客户端 · seeAitv" });

const route = useRoute();
const workspace = useWorkspaceStore();

/** 句柄：只当作不透明字符串原样交回服务端。 */
const reqHandle = computed(() => {
  const v = route.query.req;
  return typeof v === "string" ? v : Array.isArray(v) && typeof v[0] === "string" ? v[0] : "";
});

const info = ref<McpConsentInfo | null>(null);
const toolsetInfos = ref<McpToolsetInfo[] | null>(null);
const loading = ref(true);
const failure = ref<{ text: string; retry: boolean; expired: boolean } | null>(null);
const form = ref<ConsentForm | null>(null);
const submitting = ref<"allow" | "deny" | null>(null);
const submitError = ref("");
const redirecting = ref(false);

const workspaces = computed(() => workspace.list);
const selected = computed(() => workspaces.value.find((w) => w.id === form.value?.workspaceId) ?? null);
const source = computed(() => (info.value ? clientSource(info.value.client) : null));
const scopeOptions = computed(() => (info.value ? consentScopeOptions(info.value.requestedScopes, selected.value?.role) : []));
const toolsetOptions = computed(() => consentToolsetOptions(toolsetInfos.value));
const roleNotice = computed(() => (form.value ? consentRoleNotice(selected.value?.role, form.value.scopes) : null));
const spend = computed(() => (form.value ? spendNotice(form.value.scopes, selected.value) : null));
const invalid = computed(() => (form.value ? validateConsentForm(form.value) : "正在加载"));

function online() {
  return !import.meta.client || navigator.onLine;
}

async function load() {
  loading.value = true;
  failure.value = null;
  if (!reqHandle.value) {
    failure.value = { text: "授权请求已过期，请回到客户端重新连接", retry: false, expired: true };
    loading.value = false;
    return;
  }
  try {
    await workspace.load();
    const api = useApi();
    // 工具集可用性只是锦上添花：取不到时全部可选，不让同意页因此打不开。
    const [consent, snippets] = await Promise.all([
      api<McpConsentInfo>(`/mcp/consent?req=${encodeURIComponent(reqHandle.value)}`),
      api<McpClientSnippets>("/me/mcp/client-snippets").catch(() => null),
    ]);
    info.value = consent;
    toolsetInfos.value = snippets?.toolsets ?? null;
    form.value = initialConsentForm(consent, workspace.list, workspace.currentId, toolsetInfos.value);
  } catch (cause) {
    failure.value = consentError(cause, online());
  } finally {
    loading.value = false;
  }
}

function setScope(scope: McpScope, on: boolean) {
  if (form.value) form.value = { ...form.value, scopes: toggleConsentScope(form.value.scopes, scope, on) };
}

function setToolset(name: McpToolset, on: boolean) {
  if (form.value) form.value = { ...form.value, toolsets: toggleConsentToolset(form.value.toolsets, name, on, toolsetInfos.value) };
}

async function submit(decision: "allow" | "deny") {
  if (!form.value || submitting.value || redirecting.value) return;
  if (decision === "allow" && invalid.value) {
    submitError.value = invalid.value;
    return;
  }
  submitting.value = decision;
  submitError.value = "";
  try {
    const body = decision === "allow" ? consentAllowBody(reqHandle.value, form.value) : consentDenyBody(reqHandle.value);
    const res = await useApi()<McpConsentResult>("/mcp/consent", { method: "POST", body });
    redirecting.value = true;
    // ⚠️ 唯一允许跳站外的地方：地址只取服务端响应。
    window.location.assign(res.redirectTo);
  } catch (cause) {
    const state = consentError(cause, online());
    if (state.expired) failure.value = state;
    else submitError.value = state.text;
  } finally {
    submitting.value = null;
  }
}

onMounted(load);
</script>

<template>
  <McpGatePanel badge="授权确认">
    <div v-if="loading" class="mcp-gate-state">正在加载授权请求…</div>

    <div v-else-if="failure" class="mcp-gate-state">
      <h1>{{ failure.expired ? "授权请求已过期" : "无法打开授权请求" }}</h1>
      <p class="auth-message error">{{ failure.text }}</p>
      <button v-if="failure.retry" type="button" class="auth-submit" @click="load">重试</button>
    </div>

    <template v-else-if="info && form">
      <div class="consent-head">
        <h1>「{{ info.client.name }}」请求访问你的 seeAitv</h1>
        <p v-if="source?.verified" class="consent-source verified"><BadgeCheck :size="14" />{{ source.text }}</p>
        <p v-else class="consent-source unverified"><AlertTriangle :size="14" />{{ source?.text }}</p>
        <p class="consent-meta">
          授权后将返回 <strong>{{ info.redirectHost }}</strong>
          <span v-if="info.readOnly"> · 客户端申请的是只读端点</span>
          <span v-else-if="info.toolset"> · 客户端只连「{{ info.toolset }}」工具集</span>
        </p>
      </div>

      <section class="consent-section">
        <h2>授权到哪个工作区</h2>
        <div v-if="!workspaces.length" class="auth-message error">没有可用的工作区</div>
        <label v-for="w in workspaces" :key="w.id" class="consent-option" :class="{ active: form.workspaceId === w.id }">
          <input v-model="form.workspaceId" type="radio" name="workspace" :value="w.id">
          <span class="consent-option-main">
            <strong>{{ w.name }}</strong>
            <small>{{ w.kind === "team" ? "团队" : "个人" }} · 你的角色：{{ roleLabel(w.role) }}</small>
          </span>
        </label>
      </section>

      <section class="consent-section">
        <h2>允许它做什么</h2>
        <p v-if="roleNotice" class="consent-hint warn">{{ roleNotice }}</p>
        <label v-for="o in scopeOptions" :key="o.value" class="consent-option" :class="{ muted: !!o.roleHint }">
          <input type="checkbox" :checked="form.scopes.includes(o.value)" :disabled="o.locked" @change="setScope(o.value, ($event.target as HTMLInputElement).checked)">
          <span class="consent-option-main">
            <strong>{{ o.label }}<em v-if="o.locked">必选</em></strong>
            <small>{{ o.hint }}</small>
            <small v-if="o.roleHint && form.scopes.includes(o.value)" class="warn">{{ o.roleHint }}</small>
          </span>
        </label>
      </section>

      <div v-if="spend" class="consent-spend">
        <Coins :size="16" />
        <div>
          <p>{{ spend.text }}</p>
          <p v-if="spend.team" class="team">{{ spend.team }}</p>
        </div>
      </div>

      <section class="consent-section">
        <h2>开放的工具集</h2>
        <div class="consent-toolsets">
          <label v-for="o in toolsetOptions" :key="o.name" class="consent-chip" :class="{ active: form.toolsets.includes(o.name), disabled: !o.selectable }">
            <input type="checkbox" :checked="form.toolsets.includes(o.name)" :disabled="!o.selectable" @change="setToolset(o.name, ($event.target as HTMLInputElement).checked)">
            {{ o.label }}<small v-if="o.hint">{{ o.hint }}</small>
          </label>
        </div>
      </section>

      <label class="consent-option consent-switch" :class="{ active: form.requirePageConfirm }">
        <input v-model="form.requirePageConfirm" type="checkbox">
        <span class="consent-option-main">
          <strong>花钱需在 seeAitv 页面确认</strong>
          <small>{{ PAGE_CONFIRM_HINT }}</small>
        </span>
      </label>

      <p v-if="submitError" class="auth-message error">{{ submitError }}</p>

      <div class="consent-actions">
        <button type="button" class="secondary-command" :disabled="!!submitting || redirecting" @click="submit('deny')">
          {{ submitting === "deny" ? "处理中" : "拒绝" }}
        </button>
        <button type="button" class="auth-submit" :disabled="!!submitting || redirecting || !!invalid" @click="submit('allow')">
          <span>{{ redirecting ? "正在返回客户端" : submitting === "allow" ? "处理中" : "允许" }}</span><ExternalLink :size="15" />
        </button>
      </div>
      <p class="consent-foot">授权请求 {{ formatDateTime(info.expiresAt) }} 前有效。之后可随时在「账号中心 → MCP 访问」里撤销。</p>
    </template>
  </McpGatePanel>
</template>

<style scoped>
h1 { margin: 18px 0 8px; font-size: 18px; line-height: 1.4; }
h2 { margin: 0 0 8px; color: var(--portal-muted); font-size: 11px; font-weight: 600; }
.mcp-gate-state { display: grid; gap: 12px; padding: 18px 0 4px; color: var(--portal-muted); font-size: 12px; }
.consent-head { padding-bottom: 14px; border-bottom: 1px solid var(--portal-line-soft); }
.consent-source { display: inline-flex; align-items: flex-start; gap: 6px; margin: 0; border-radius: 6px; padding: 7px 10px; font-size: 11px; line-height: 1.5; }
.consent-source.verified { color: var(--portal-success); background: var(--portal-green-soft); border: 1px solid var(--portal-green-line); }
.consent-source.unverified { color: var(--portal-gold); background: var(--portal-gold-soft); border: 1px solid var(--portal-gold-line); font-weight: 600; }
.consent-source svg { flex: none; margin-top: 2px; }
.consent-meta { margin: 10px 0 0; color: var(--portal-muted); font-size: 11px; }
.consent-meta strong { color: var(--portal-text); }
.consent-section { display: grid; gap: 6px; margin-top: 16px; }
.consent-option { display: grid; grid-template-columns: 18px minmax(0,1fr); align-items: start; gap: 9px; border: 1px solid var(--portal-line); border-radius: 6px; padding: 9px 11px; background: var(--portal-input-bg); cursor: pointer; }
.consent-option.active { border-color: var(--portal-cyan-line); background: var(--portal-cyan-soft); }
.consent-option.muted { opacity: .8; }
.consent-option input { margin-top: 3px; accent-color: var(--portal-cyan); }
.consent-option-main { min-width: 0; display: grid; gap: 3px; }
.consent-option-main strong { font-size: 12px; }
.consent-option-main em { margin-left: 6px; color: var(--portal-faint); font-size: 9px; font-style: normal; }
.consent-option-main small { color: var(--portal-muted); font-size: 10px; line-height: 1.5; }
.consent-option-main small.warn, .consent-hint.warn { color: var(--portal-gold); }
.consent-hint { margin: 0; font-size: 10px; line-height: 1.5; }
.consent-spend { display: grid; grid-template-columns: 18px minmax(0,1fr); gap: 9px; margin-top: 14px; border: 1px solid var(--portal-gold-line); border-left: 3px solid var(--portal-gold); border-radius: 6px; padding: 10px 12px; color: var(--portal-text); background: var(--portal-gold-soft); font-size: 11px; line-height: 1.6; }
.consent-spend svg { color: var(--portal-gold); margin-top: 2px; }
.consent-spend p { margin: 0; }
.consent-spend p.team { margin-top: 4px; color: var(--portal-gold); }
.consent-toolsets { display: flex; flex-wrap: wrap; gap: 6px; }
.consent-chip { display: inline-flex; align-items: center; gap: 5px; border: 1px solid var(--portal-line); border-radius: 999px; padding: 5px 10px; color: var(--portal-muted); background: var(--portal-input-bg); font-size: 10px; cursor: pointer; }
.consent-chip.active { color: var(--portal-cyan); border-color: var(--portal-cyan-line); background: var(--portal-cyan-soft); }
.consent-chip.disabled { opacity: .55; cursor: not-allowed; }
.consent-chip input { margin: 0; accent-color: var(--portal-cyan); }
.consent-chip small { color: var(--portal-faint); font-size: 9px; }
.consent-switch { margin-top: 16px; }
.consent-actions { display: grid; grid-template-columns: 1fr 2fr; gap: 10px; margin-top: 18px; }
.consent-actions .secondary-command { justify-content: center; min-height: 42px; font-size: 11px; cursor: pointer; }
.consent-foot { margin: 12px 0 0; color: var(--portal-faint); font-size: 9px; }
.auth-message { margin-top: 12px; }
</style>
