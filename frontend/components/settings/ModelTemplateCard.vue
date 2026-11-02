<script setup lang="ts">
/**
 * 模板模式的一张厂商卡（TT-T-18，TT-F-26）：填 Key → 测试连接 → 启用。
 *
 * ⚠️ 测试连接会**真的向厂商发一次请求**，所以先把 Key 存进去再测（测的是库里那份配置，不是输入框里的字）。
 * ⚠️ 启用 = 启用厂商 + 逐个启用模板里属于它的模型；每个模型照常过发布检查，没过的不启用并说清原因。
 */
import { ElButton, ElInput, ElMessage, ElTag } from "element-plus";
import { settingsApiError } from "~/utils/settingsApi";
import {
  SETTINGS_MODEL_DOMAINS, settingsEnableSummary, settingsKeyValue, settingsLooksMasked, settingsProviderDraft, settingsProviderPayload,
  settingsTemplateAuthHint, settingsTemplateState, type SettingsEnableResult, type SettingsTemplate
} from "~/utils/settingsModelForm";

type Row = Record<string, unknown>;

const props = defineProps<{ template: SettingsTemplate; provider: Row | null }>();
const emit = defineEmits<{ (e: "changed"): void; (e: "advanced", code: string): void }>();

const api = useApi();
const key = reactive({ value: "", ak: "", sk: "" });
const busy = ref<"" | "save" | "probe" | "enable">("");
const result = ref<{ text: string; ok: boolean } | null>(null);
const state = computed(() => settingsTemplateState(props.template));
const volc = computed(() => props.template.authType === "volc_v4");
const needsKey = computed(() => props.template.authType !== "none");
const domainLabel = (d: string) => SETTINGS_MODEL_DOMAINS.find(x => x.value === d)?.label ?? d;
const typed = computed(() => settingsKeyValue({ authType: props.template.authType as "bearer", keyValue: key.value, keyAk: key.ak, keySk: key.sk }));

function keyIssue(): string {
  if (volc.value && (key.ak.trim() || key.sk.trim()) && !typed.value) return "请同时填写 AccessKey 与 SecretKey";
  if (settingsLooksMasked(key.value) || settingsLooksMasked(key.ak) || settingsLooksMasked(key.sk)) return "填的值看起来是掩码，请粘贴真实的 Key";
  return "";
}

function clearKey() {
  Object.assign(key, { value: "", ak: "", sk: "" });
}

/** 只存 Key，不改启用状态（测试连接之前用）。 */
async function saveKey(): Promise<boolean> {
  if (!typed.value) return true;
  const issue = keyIssue();
  if (issue) {
    ElMessage.warning(issue);
    return false;
  }
  if (!props.provider) {
    ElMessage.error("模板厂商还没有写入，请重启应用后再试");
    return false;
  }
  const d = settingsProviderDraft(props.provider);
  Object.assign(d, { keyAction: "replace", keyValue: key.value, keyAk: key.ak, keySk: key.sk });
  await api("/admin/models/providers", { method: "PUT", body: settingsProviderPayload(d) });
  clearKey();
  return true;
}

async function save() {
  busy.value = "save";
  try {
    if (await saveKey()) {
      ElMessage.success("Key 已保存");
      emit("changed");
    }
  } catch (e) {
    ElMessage.error({ message: settingsApiError(e).message, duration: 8000 });
  } finally {
    busy.value = "";
  }
}

async function probe() {
  busy.value = "probe";
  try {
    if (!(await saveKey())) return;
    emit("changed");
    const r = await api<{ ok: boolean; statusCode?: number; errorKind?: string; message?: string; latencyMs?: number }>(
      `/admin/models/providers/${encodeURIComponent(props.template.code)}/probe`, { method: "POST" });
    if (r.ok) ElMessage.success(`连通：HTTP ${r.statusCode ?? "—"}，${r.latencyMs ?? 0}ms`);
    else ElMessage.warning({ message: `不通（${r.errorKind ?? "未知"}${r.statusCode ? ` · HTTP ${r.statusCode}` : ""}）：${r.message ?? ""}`, duration: 10000 });
  } catch (e) {
    ElMessage.error({ message: settingsApiError(e).message, duration: 8000 });
  } finally {
    busy.value = "";
  }
}

async function enable() {
  const issue = keyIssue();
  if (issue) {
    ElMessage.warning(issue);
    return;
  }
  if (needsKey.value && !typed.value && !props.template.hasApiKey) {
    ElMessage.warning("请先填写 API Key");
    return;
  }
  busy.value = "enable";
  try {
    const body = typed.value ? { apiKey: { action: "replace", value: typed.value } } : {};
    const r = await api<{ models: SettingsEnableResult[] }>(`/admin/model-templates/${encodeURIComponent(props.template.code)}/enable`,
      { method: "POST", body });
    clearKey();
    result.value = settingsEnableSummary(r.models ?? []);
    if (result.value.ok) ElMessage.success(result.value.text);
    else ElMessage.warning({ message: result.value.text, duration: 12000 });
    emit("changed");
  } catch (e) {
    ElMessage.error({ message: settingsApiError(e).message, duration: 8000 });
  } finally {
    busy.value = "";
  }
}
</script>

<template>
  <article class="tpl-card">
    <header>
      <div>
        <strong>{{ template.name }}</strong>
        <code class="tpl-sub">{{ template.code }}</code>
      </div>
      <ElTag size="small" :type="state.type" effect="plain">{{ state.label }}</ElTag>
    </header>
    <p class="tpl-url">{{ template.baseUrl }}</p>
    <div class="tpl-models">
      <ElTag v-for="m in template.models" :key="m.code" size="small" :type="m.status === 'enabled' ? 'success' : 'info'" effect="plain">
        {{ m.name }} · {{ domainLabel(m.domain) }}
      </ElTag>
    </div>
    <template v-if="template.installed && needsKey">
      <div v-if="volc" class="tpl-key two">
        <ElInput v-model="key.ak" type="password" show-password autocomplete="new-password" placeholder="AccessKey" />
        <ElInput v-model="key.sk" type="password" show-password autocomplete="new-password" placeholder="SecretKey" />
      </div>
      <div v-else class="tpl-key">
        <ElInput v-model="key.value" type="password" show-password autocomplete="new-password" :placeholder="template.hasApiKey ? '已填写；要更换就粘贴新的 Key' : '粘贴 API Key'" />
      </div>
      <small class="tpl-help">{{ settingsTemplateAuthHint(template) }}；Key 只保存在这台电脑上。</small>
    </template>
    <p v-if="result && !result.ok" class="tpl-result">{{ result.text }}</p>
    <footer v-if="template.installed">
      <ElButton v-if="typed" size="small" :loading="busy === 'save'" :disabled="!!busy && busy !== 'save'" @click="save">保存 Key</ElButton>
      <ElButton size="small" :loading="busy === 'probe'" :disabled="!!busy && busy !== 'probe' || (needsKey && !typed && !template.hasApiKey)" @click="probe">测试连接</ElButton>
      <ElButton size="small" type="primary" :loading="busy === 'enable'" :disabled="!!busy && busy !== 'enable'" @click="enable">
        {{ template.status === "enabled" ? "启用其余模型" : "启用" }}
      </ElButton>
      <ElButton size="small" link type="primary" @click="emit('advanced', template.code)">在高级模式中编辑</ElButton>
    </footer>
  </article>
</template>

<style scoped>
.tpl-card { display: flex; flex-direction: column; gap: 8px; padding: 14px 16px; border: 1px solid var(--portal-line); border-radius: 10px; background: var(--portal-surface); }
.tpl-card header { display: flex; align-items: center; justify-content: space-between; gap: 8px; }
.tpl-card strong { color: var(--portal-text); font-size: 15px; font-weight: 500; }
.tpl-sub { margin-left: 6px; color: var(--portal-faint); font-size: 11px; }
.tpl-url { margin: 0; color: var(--portal-muted); font-size: 12px; word-break: break-all; }
.tpl-models { display: flex; flex-wrap: wrap; gap: 6px; }
.tpl-key { display: grid; gap: 8px; }
.tpl-key.two { grid-template-columns: repeat(2, minmax(0, 1fr)); }
.tpl-help { color: var(--portal-faint); font-size: 12px; }
.tpl-result { margin: 0; color: var(--portal-danger); font-size: 12px; line-height: 1.5; }
.tpl-card footer { display: flex; flex-wrap: wrap; align-items: center; gap: 8px; margin-top: 4px; }
</style>
