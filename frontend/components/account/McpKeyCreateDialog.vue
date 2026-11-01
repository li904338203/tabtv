<script setup lang="ts">
/**
 * 新建访问密钥（MCPT-11F）。
 *
 * ⚠️ 只在用户点「创建」后发一次 POST；组件打开时不写任何数据（热更新下会反复触发，且是在替用户做决定）。
 * ⚠️ 角色不足时**不禁止创建**，只如实说明哪些权限不会生效 —— 有效权限 = scope ∩ 当前角色，
 *    由服务端每次调用实时算（成员之后被升级，这把密钥的 generate 就会自动生效）。
 * ⚠️ 创建响应里的明文原样交给父组件（父组件只放进一个局部 ref 给明文弹层用），这里不保留。
 */
import { TriangleAlert, X } from "@lucide/vue";
import type { components } from "@tabtv/api-client";
import {
  EXPIRY_OPTIONS,
  MAX_CLIENT_NAME,
  SCOPE_OPTIONS,
  defaultKeyForm,
  generateNotice,
  isScopeLocked,
  keyFormToRequest,
  roleScopeNotice,
  toggleScope,
  toggleToolset,
  toolsetOptions,
  validateKeyForm,
  type KeyForm,
  type McpScope,
  type McpToolset,
  type McpToolsetInfo,
} from "~/utils/mcpKeys";
import type { WorkspaceRole } from "~/utils/agentState";

type Created = components["schemas"]["McpAccessKeyCreated"];
type WorkspaceOption = { id: string; name: string; role: WorkspaceRole };

const props = defineProps<{
  workspaces: WorkspaceOption[];
  defaultWorkspaceId: string;
  toolsets: McpToolsetInfo[] | null;
}>();
const emit = defineEmits<{ close: []; created: [result: Created] }>();

const ROLE_LABELS: Record<WorkspaceRole, string> = { owner: "所有者", admin: "管理员", editor: "编辑者", viewer: "只读" };

const form = ref<KeyForm>(defaultKeyForm(props.defaultWorkspaceId, props.toolsets));
const submitting = ref(false);
const error = ref("");

const role = computed(() => props.workspaces.find((w) => w.id === form.value.workspaceId)?.role);
const options = computed(() => toolsetOptions(props.toolsets));
const roleNotice = computed(() => roleScopeNotice(role.value, form.value.scopes));
const moneyNotice = computed(() => generateNotice(form.value.scopes));

function setScope(scope: McpScope, on: boolean) {
  form.value.scopes = toggleScope(form.value.scopes, scope, on);
}
function setToolset(name: McpToolset, on: boolean) {
  form.value.toolsets = toggleToolset(form.value.toolsets, name, on, props.toolsets);
}

async function submit() {
  if (submitting.value) return;
  const problem = validateKeyForm(form.value);
  if (problem) {
    error.value = problem;
    return;
  }
  submitting.value = true;
  error.value = "";
  try {
    const created = await useApi()<Created>("/me/mcp/keys", { method: "POST", body: keyFormToRequest(form.value) });
    emit("created", created);
  } catch (cause) {
    const message = typeof cause === "object" && cause !== null && "message" in cause ? String((cause as { message?: unknown }).message ?? "") : "";
    error.value = !navigator.onLine ? "网络已断开，恢复后再试" : (message || "创建失败，请稍后重试");
  } finally {
    submitting.value = false;
  }
}

function onKeydown(event: KeyboardEvent) {
  event.stopPropagation();
  if (event.key === "Escape" && !submitting.value) emit("close");
}
onMounted(() => window.addEventListener("keydown", onKeydown, true));
onBeforeUnmount(() => window.removeEventListener("keydown", onKeydown, true));
</script>

<template>
  <Teleport to="body">
    <div class="portal-overlay" @click.self="!submitting && emit('close')">
      <form class="mcp-dialog" role="dialog" aria-modal="true" aria-label="新建访问密钥" @submit.prevent="submit">
        <header class="mcp-dialog-head">
          <div><h2>新建访问密钥</h2><p>给不支持一键登录的客户端或脚本使用；只显示一次</p></div>
          <button type="button" class="icon-command" title="关闭（Esc）" :disabled="submitting" @click="emit('close')"><X :size="16" /></button>
        </header>

        <label class="mcp-field">
          <span>工作区</span>
          <select v-model="form.workspaceId">
            <option v-for="w in workspaces" :key="w.id" :value="w.id">{{ w.name }}（{{ ROLE_LABELS[w.role] ?? w.role }}）</option>
          </select>
        </label>

        <label class="mcp-field">
          <span>名称</span>
          <input v-model="form.clientName" type="text" :maxlength="MAX_CLIENT_NAME" placeholder="如：我的 Claude Code" autocomplete="off">
        </label>

        <fieldset class="mcp-field">
          <legend>权限</legend>
          <label v-for="o in SCOPE_OPTIONS" :key="o.value" class="mcp-check" :class="{ locked: isScopeLocked(o.value) }">
            <input
              type="checkbox"
              :checked="form.scopes.includes(o.value)"
              :disabled="isScopeLocked(o.value)"
              @change="setScope(o.value, ($event.target as HTMLInputElement).checked)"
            >
            <span><strong>{{ o.label }}</strong><small>{{ o.hint }}{{ isScopeLocked(o.value) ? '（必选）' : '' }}</small></span>
          </label>
          <p v-if="moneyNotice" class="mcp-warning"><TriangleAlert :size="14" />{{ moneyNotice }}</p>
          <p v-if="roleNotice" class="portal-notice">{{ roleNotice }}</p>
        </fieldset>

        <fieldset class="mcp-field">
          <legend>工具集</legend>
          <label v-for="o in options" :key="o.name" class="mcp-check" :class="{ locked: !o.selectable }">
            <input
              type="checkbox"
              :checked="form.toolsets.includes(o.name)"
              :disabled="!o.selectable"
              @change="setToolset(o.name, ($event.target as HTMLInputElement).checked)"
            >
            <span><strong>{{ o.label }}</strong><small v-if="o.hint">{{ o.hint }}</small></span>
          </label>
        </fieldset>

        <fieldset class="mcp-field">
          <legend>有效期</legend>
          <div class="mcp-radios">
            <label v-for="o in EXPIRY_OPTIONS" :key="o.value" class="mcp-check">
              <input v-model="form.expiry" type="radio" name="mcp-expiry" :value="o.value">
              <span><strong>{{ o.label }}</strong></span>
            </label>
          </div>
        </fieldset>

        <!-- 页面审批已接通（MCPT-13）：开了它，每次花积分都要你本人到 seeAitv 页面批准。 -->
        <label class="mcp-check">
          <input v-model="form.requirePageConfirm" type="checkbox">
          <span><strong>花钱需在 seeAitv 页面确认</strong><small>开启后，这把密钥每次花积分都要你回到 seeAitv 页面批准；不开则在客户端里确认报价即可。创建后也能在列表里切换</small></span>
        </label>

        <p v-if="error" class="portal-notice error">{{ error }}</p>
        <footer class="mcp-dialog-foot">
          <button type="button" class="secondary-command" :disabled="submitting" @click="emit('close')">取消</button>
          <button type="submit" class="product-primary-command" :disabled="submitting">{{ submitting ? '创建中…' : '创建' }}</button>
        </footer>
      </form>
    </div>
  </Teleport>
</template>
