<script setup lang="ts">
/**
 * 账号设置「MCP 访问」（MCPT-11F，MCP-F-03 / F-08 / F-72 访问密钥部分）。
 *
 * 访问密钥列表 · 新建（明文只显示一次）· 已连接的客户端（OAuth，MCPT-14）·
 * 本工作区连接与工作区开关（当前工作区角色为 owner / admin 时才显示，MCPT-14）· 指向 `/mcp` 接入指南的入口。
 * 片段仍在这里取一次：新建密钥弹层要工具集可用性、明文弹层要在内存里填好占位符供复制。
 *
 * ⚠️ 页面打开时**只发 GET**（密钥列表、授权列表、片段、工作区；管理员再加工作区连接与开关）；
 *    创建 / 撤销 / 切换只在用户点击之后发生。
 * ⚠️ 明文只存在 `secret` 这个局部 ref 里：关闭弹层、离开页面即清空，不进 store、不进本地存储。
 */
import { Cable, ChevronLeft, ChevronRight, KeyRound, Plus } from "@lucide/vue";
import { ElMessage, ElMessageBox } from "element-plus";
import type { components } from "@tabtv/api-client";
import PageCrumb from "~/components/common/PageCrumb.vue";
import McpGrantList from "~/components/account/McpGrantList.vue";
import McpKeyCreateDialog from "~/components/account/McpKeyCreateDialog.vue";
import McpKeyList from "~/components/account/McpKeyList.vue";
import McpKeySecretDialog from "~/components/account/McpKeySecretDialog.vue";
import { mcpLoadError, revokeConfirmText, type McpAccessKey } from "~/utils/mcpKeys";
import type { McpClientSnippets as Snippets } from "~/utils/mcpSnippets";
import {
  canManageWorkspaceMcp,
  grantRevokeConfirmText,
  oauthGrants,
  pageConfirmOffText,
  workspaceSwitchNotice,
  workspaceSwitchText,
  type McpConnection,
  type McpConnectionPage,
  type McpGrant,
  type McpWorkspaceSettings,
} from "~/utils/mcpConnections";

definePageMeta({ layout: "platform", middleware: "auth" });

type Created = components["schemas"]["McpAccessKeyCreated"];

const workspace = useWorkspaceStore();

const keys = ref<McpAccessKey[]>([]);
const keysLoading = ref(true);
const keysError = ref<{ text: string; retry: boolean } | null>(null);

const snippets = ref<Snippets | null>(null);
const snippetsError = ref<{ text: string; retry: boolean } | null>(null);

const creating = ref(false);
const revokingId = ref<string | null>(null);
// ⚠️ 明文的唯一持有处（局部 shallowRef，不进 store）。
const secret = shallowRef<{ plaintext: string; key: McpAccessKey } | null>(null);

const workspaceOptions = computed(() => workspace.list.map((w) => ({ id: w.id, name: w.name, role: w.role })));
const workspaceNames = computed<Record<string, string>>(() => Object.fromEntries(workspace.list.map((w) => [w.id, w.name])));
const defaultWorkspaceId = computed(() => workspace.currentId || workspace.list[0]?.id || "");

// ---- 已连接的客户端（本人的 OAuth 授权，MCPT-14）
const grants = ref<McpGrant[]>([]);
const grantsError = ref<{ text: string; retry: boolean } | null>(null);
const busyGrantId = ref<string | null>(null);
const oauthList = computed(() => oauthGrants(grants.value));

// ---- 本工作区连接与开关（owner / admin，MCPT-14）
const currentRole = computed(() => workspace.current?.role);
const showWorkspace = computed(() => canManageWorkspaceMcp(currentRole.value));
const wsConns = ref<McpConnection[]>([]);
const wsNext = ref<string | null>(null);
const wsLoading = ref(false);
const wsError = ref<{ text: string; retry: boolean } | null>(null);
const wsSetting = ref<McpWorkspaceSettings | null>(null);
const wsSwitching = ref(false);
const wsBusyId = ref<string | null>(null);
const wsNotice = computed(() => workspaceSwitchNotice(wsSetting.value));

function errorText(cause: unknown, fallback: string): string {
  const message = typeof cause === "object" && cause !== null && "message" in cause ? String((cause as { message?: unknown }).message ?? "") : "";
  return online() ? (message || fallback) : "网络已断开，恢复后再试";
}

async function confirm(text: string, title: string, ok: string): Promise<boolean> {
  try {
    await ElMessageBox.confirm(text, title, { confirmButtonText: ok, cancelButtonText: "取消", type: "warning" });
    return true;
  } catch {
    return false;
  }
}

async function loadGrants() {
  try {
    grants.value = await useApi()<McpGrant[]>("/me/mcp/grants");
    grantsError.value = null;
  } catch (cause) {
    const state = mcpLoadError(cause, online());
    if (!state.retry) grants.value = [];
    grantsError.value = state;
  }
}

async function loadWorkspace(more = false) {
  if (!showWorkspace.value) {
    wsConns.value = [];
    wsSetting.value = null;
    wsError.value = null;
    return;
  }
  wsLoading.value = true;
  try {
    const query: Record<string, string> = { limit: "50" };
    if (more && wsNext.value) query.cursor = wsNext.value;
    const [page, setting] = await Promise.all([
      useApi()<McpConnectionPage>("/workspaces/current/mcp/grants", { query }),
      more ? Promise.resolve(wsSetting.value) : useApi()<McpWorkspaceSettings>("/workspaces/current/mcp/settings"),
    ]);
    wsConns.value = more ? [...wsConns.value, ...page.items] : page.items;
    wsNext.value = page.nextCursor ?? null;
    wsSetting.value = setting;
    wsError.value = null;
  } catch (cause) {
    const state = mcpLoadError(cause, online());
    if (!state.retry) {
      wsConns.value = [];
      wsSetting.value = null;
    }
    wsError.value = state;
  } finally {
    wsLoading.value = false;
  }
}

async function revokeGrant(g: McpGrant) {
  if (!(await confirm(grantRevokeConfirmText(g), "撤销连接", "撤销"))) return;
  busyGrantId.value = g.id;
  try {
    await useApi()(`/me/mcp/grants/${g.id}`, { method: "DELETE" });
    ElMessage.success("已撤销");
  } catch (cause) {
    ElMessage.error(errorText(cause, "撤销失败"));
  } finally {
    busyGrantId.value = null;
    void loadGrants();
    void loadWorkspace();
  }
}

/**
 * 切换「花钱需在 seeAitv 页面确认」（MCPT-13 的 PATCH）。⚠️ 关闭前二次确认；打开不需要（只会让花钱更难）。
 * 立即生效：确认服务每次调用都读授权上的当前值。
 */
async function togglePageConfirm(grantId: string, clientName: string, on: boolean) {
  if (!on && !(await confirm(pageConfirmOffText({ clientName }), "关闭页面确认", "关闭"))) {
    void loadKeys();
    void loadGrants();
    return;
  }
  busyGrantId.value = grantId;
  try {
    await useApi()(`/me/mcp/grants/${grantId}`, { method: "PATCH", body: { requirePageConfirm: on } });
    ElMessage.success(on ? "已开启：之后花钱都要到 seeAitv 页面批准" : "已关闭：在客户端里确认即可");
  } catch (cause) {
    ElMessage.error(errorText(cause, "修改失败"));
  } finally {
    busyGrantId.value = null;
    void loadKeys();
    void loadGrants();
  }
}

async function revokeWorkspaceGrant(c: McpConnection) {
  const who = c.userName ?? `成员 ${c.userId}`;
  if (!(await confirm(grantRevokeConfirmText(c, who), "撤销成员的连接", "撤销"))) return;
  wsBusyId.value = c.id;
  try {
    await useApi()(`/workspaces/current/mcp/grants/${c.id}`, { method: "DELETE" });
    ElMessage.success("已撤销");
  } catch (cause) {
    ElMessage.error(errorText(cause, "撤销失败"));
  } finally {
    wsBusyId.value = null;
    void loadWorkspace();
    void loadGrants();
    void loadKeys();
  }
}

async function toggleWorkspace(enable: boolean) {
  if (!(await confirm(workspaceSwitchText(enable), enable ? "打开本工作区 MCP" : "关闭本工作区 MCP", enable ? "打开" : "关闭"))) return;
  wsSwitching.value = true;
  try {
    wsSetting.value = await useApi()<McpWorkspaceSettings>("/workspaces/current/mcp/settings", {
      method: "PUT",
      body: { enabled: enable },
    });
    ElMessage.success(enable ? "已打开" : "已关闭：外部客户端的下一次调用即被拒绝");
  } catch (cause) {
    ElMessage.error(errorText(cause, "修改失败"));
  } finally {
    wsSwitching.value = false;
  }
}

function online() {
  return !import.meta.client || navigator.onLine;
}

async function loadKeys() {
  keysLoading.value = true;
  try {
    keys.value = await useApi()<McpAccessKey[]>("/me/mcp/keys");
    keysError.value = null;
  } catch (cause) {
    // ⚠️ 出错保留已显示的列表（只有权限相关才清）。
    const state = mcpLoadError(cause, online());
    if (!state.retry) keys.value = [];
    keysError.value = state;
  } finally {
    keysLoading.value = false;
  }
}

async function loadSnippets() {
  try {
    snippets.value = await useApi()<Snippets>("/me/mcp/client-snippets");
    snippetsError.value = null;
  } catch (cause) {
    snippetsError.value = mcpLoadError(cause, online());
  }
}

function onCreated(result: Created) {
  creating.value = false;
  const { plaintext, ...key } = result;
  secret.value = { plaintext, key };
  // 列表里只放不含明文的那一份；以服务端列表为准再取一次（只是 GET）。
  keys.value = [key, ...keys.value.filter((k) => k.id !== key.id)];
  void loadKeys();
}

function closeSecret() {
  secret.value = null;
}

async function revoke(key: McpAccessKey) {
  try {
    await ElMessageBox.confirm(revokeConfirmText(key), "撤销访问密钥", {
      confirmButtonText: "撤销",
      cancelButtonText: "取消",
      type: "warning",
    });
  } catch {
    return;
  }
  revokingId.value = key.id;
  try {
    await useApi()(`/me/mcp/keys/${key.id}`, { method: "DELETE" });
    ElMessage.success("已撤销");
  } catch (cause) {
    const message = typeof cause === "object" && cause !== null && "message" in cause ? String((cause as { message?: unknown }).message ?? "") : "";
    ElMessage.error(online() ? (message || "撤销失败") : "网络已断开，恢复后再试");
  } finally {
    revokingId.value = null;
    void loadKeys();
    void loadGrants();
    void loadWorkspace();
  }
}

onMounted(() => {
  void workspace.load();
  void loadKeys();
  void loadGrants();
  void loadSnippets();
  void loadWorkspace();
});
// 当前工作区或角色变了就重取工作区分区（只是 GET；非 owner / admin 直接清空不发请求）。
watch(() => `${workspace.currentId}:${currentRole.value ?? ""}`, () => void loadWorkspace());
// 离开页面即清空明文（弹层随页面卸载，这里再保险一次）。
onBeforeUnmount(() => {
  secret.value = null;
});
</script>

<template>
  <main class="portal-page account-page mcp-page">
    <header class="portal-page-head compact">
      <div>
        <PageCrumb><span class="page-kicker"><KeyRound :size="15" />账号中心</span></PageCrumb>
        <h1>MCP 访问</h1>
        <p>让 Claude Code、Codex、Cursor 等外部 AI 工具，用你的权限访问 seeAitv 的项目、画布与生成能力</p>
      </div>
      <NuxtLink to="/account" class="secondary-command"><ChevronLeft :size="14" />返回账号中心</NuxtLink>
    </header>

    <div class="section-head portal-section-head">
      <div><h2>访问密钥</h2><p>每把密钥绑定一个工作区；实际权限 = 密钥勾选的权限 ∩ 你在该工作区的角色，每次调用实时计算</p></div>
      <button type="button" class="product-primary-command" :disabled="!workspaceOptions.length" @click="creating = true"><Plus :size="14" />新建密钥</button>
    </div>
    <div v-if="keysError" class="portal-notice error">
      {{ keysError.text }}
      <button v-if="keysError.retry" type="button" class="portal-link-button" @click="loadKeys">重试</button>
    </div>
    <div v-if="keysLoading && !keys.length" class="portal-empty">正在加载…</div>
    <McpKeyList
      v-else-if="keys.length"
      :keys="keys"
      :workspace-names="workspaceNames"
      :revoking-id="revokingId"
      allow-page-confirm
      @revoke="revoke"
      @toggle-page-confirm="(key, on) => togglePageConfirm(key.grantId, key.clientName, on)"
    />
    <div v-else-if="!keysError" class="portal-empty">还没有访问密钥。新建一把，然后按「客户端接入指南」配置到你的 AI 工具里</div>

    <div class="section-head portal-section-head">
      <div><h2>已连接的客户端</h2><p>通过 OAuth 一键连接的 AI 客户端（访问密钥见上方列表）；撤销后该客户端下一次调用即失效</p></div>
    </div>
    <div v-if="grantsError" class="portal-notice error">
      {{ grantsError.text }}
      <button v-if="grantsError.retry" type="button" class="portal-link-button" @click="loadGrants">重试</button>
    </div>
    <McpGrantList
      v-if="oauthList.length"
      :grants="oauthList"
      :workspace-names="workspaceNames"
      :busy-id="busyGrantId"
      allow-page-confirm
      @revoke="(g) => revokeGrant(g as McpGrant)"
      @toggle-page-confirm="(g, on) => togglePageConfirm(g.id, g.clientName, on)"
    />
    <div v-else-if="!grantsError" class="portal-empty">还没有通过 OAuth 连接的客户端。支持 OAuth 的客户端在添加 seeAitv 时会跳到这里授权</div>

    <template v-if="showWorkspace">
      <div class="section-head portal-section-head">
        <div>
          <h2>本工作区连接</h2>
          <p>「{{ workspace.current?.name }}」所有成员的外部客户端连接；你是该工作区的{{ currentRole === 'owner' ? '所有者' : '管理员' }}，可以撤销任意一条或关闭整个工作区的 MCP</p>
        </div>
      </div>
      <div v-if="wsError" class="portal-notice error">
        {{ wsError.text }}
        <button v-if="wsError.retry" type="button" class="portal-link-button" @click="loadWorkspace()">重试</button>
      </div>
      <div v-if="wsSetting" class="mcp-switch-row">
        <label class="mcp-inline-check">
          <input type="checkbox" :checked="wsSetting.enabled" :disabled="wsSwitching" @change="toggleWorkspace(($event.target as HTMLInputElement).checked); ($event.target as HTMLInputElement).checked = wsSetting.enabled">
          <span>允许外部 AI 客户端访问本工作区</span>
        </label>
        <small>关闭不撤销连接，重新打开即恢复；站内功能不受影响</small>
      </div>
      <div v-if="wsNotice" class="portal-notice">{{ wsNotice }}</div>
      <div v-if="wsLoading && !wsConns.length" class="portal-empty">正在加载…</div>
      <McpGrantList
        v-else-if="wsConns.length"
        :grants="wsConns"
        show-user
        :busy-id="wsBusyId"
        @revoke="(c) => revokeWorkspaceGrant(c as McpConnection)"
      />
      <div v-else-if="!wsError" class="portal-empty">本工作区还没有成员连接过外部客户端</div>
      <button v-if="wsNext" type="button" class="secondary-command" :disabled="wsLoading" @click="loadWorkspace(true)">加载更多</button>
    </template>

    <!-- 接入指南已搬到独立页面 /mcp（命令与话术都在那里），这里只留入口，不再重复渲染片段 -->
    <NuxtLink to="/mcp" class="mcp-guide-entry">
      <Cable :size="20" />
      <span>
        <strong>客户端接入指南</strong>
        <small>Claude Code、Codex、Cursor、Gemini CLI、OpenCode、WorkBuddy 的一键接入与密钥配置，复制一段话发给你的 Agent 即可</small>
      </span>
      <ChevronRight :size="17" />
    </NuxtLink>

    <McpKeyCreateDialog
      v-if="creating"
      :workspaces="workspaceOptions"
      :default-workspace-id="defaultWorkspaceId"
      :toolsets="snippets?.toolsets ?? null"
      @close="creating = false"
      @created="onCreated"
    />
    <McpKeySecretDialog
      v-if="secret"
      :plaintext="secret.plaintext"
      :access-key="secret.key"
      :snippets="snippets"
      @close="closeSecret"
    />
  </main>
</template>
