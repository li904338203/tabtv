<script setup lang="ts">
/**
 * 设置 → 模型与厂商（TT-T-18，TT-F-26/27/28）。
 *
 * - 模板（默认）：内置厂商模板，填 Key → 测试连接 → 启用。不懂协议也能用起来。
 * - 厂商 / 模型（高级）：完整配置任意中转网关 —— 厂商、模型、操作绑定、能力矩阵、参数支持、发布检查。
 *   后端校验全部复用目录域（与来源管理端同一套端点）。
 *
 * ⚠️ 两个列表端点一次回全量、不读筛选参数：关键词在已取回的列表里本地过滤。
 */
import { ElButton, ElInput, ElLoading, ElMessage, ElMessageBox, ElOption, ElSelect, ElTabPane, ElTable, ElTableColumn, ElTabs, ElTag } from "element-plus";
import ModelBindingsDrawer from "~/components/settings/ModelBindingsDrawer.vue";
import ModelConfigTransfer from "~/components/settings/ModelConfigTransfer.vue";
import ModelEditor from "~/components/settings/ModelEditor.vue";
import ModelTemplateCard from "~/components/settings/ModelTemplateCard.vue";
import ProviderEditor from "~/components/settings/ProviderEditor.vue";
import { settingsApiError } from "~/utils/settingsApi";
import { SETTINGS_MODEL_DOMAINS, type SettingsProtocol, type SettingsTemplate } from "~/utils/settingsModelForm";

type Row = Record<string, unknown>;
const vLoading = ElLoading.directive;

const api = useApi();
type Tab = "templates" | "providers" | "models";
// `?tab=models&provider=xxx` 直达高级模式（别处的「去配置模型」链接用）。
const route = useRoute();
const initialTab = String(route.query.tab ?? "");
const tab = ref<Tab>(["providers", "models"].includes(initialTab) ? initialTab as Tab : "templates");
const templates = ref<SettingsTemplate[]>([]);
const providers = ref<Row[]>([]);
const models = ref<Row[]>([]);
const protocols = ref<SettingsProtocol[]>([]);
const operations = ref<Row[]>([]);
const allowPrivate = ref(false);
const loading = ref(false);
const loadError = ref("");

const filter = reactive({ keyword: "", provider: typeof route.query.provider === "string" ? route.query.provider : "", domain: "" });
const modelEditor = ref<{ open: boolean; row: Row | null }>({ open: false, row: null });
const providerEditor = ref<{ open: boolean; row: Row | null }>({ open: false, row: null });
const bindings = ref<{ open: boolean; model: Row | null }>({ open: false, model: null });

async function load() {
  loading.value = true;
  loadError.value = "";
  try {
    const [t, p, m, pr, ops] = await Promise.all([
      api<{ items: SettingsTemplate[] }>("/admin/model-templates"),
      api<Row[]>("/admin/models/providers"),
      api<Row[]>("/admin/models/"),
      api<SettingsProtocol[]>("/admin/models/protocols"),
      api<Row[]>("/admin/operations")
    ]);
    templates.value = t.items ?? [];
    providers.value = p ?? [];
    models.value = m ?? [];
    protocols.value = pr ?? [];
    operations.value = ops ?? [];
  } catch (e) {
    loadError.value = settingsApiError(e).message;
  } finally {
    loading.value = false;
  }
  try {
    const s = await api<{ items: Array<{ key: string; value: unknown }> }>("/settings");
    allowPrivate.value = s.items.find(i => i.key === "provider.allow_private_network")?.value === true;
  } catch {
    allowPrivate.value = false;
  }
}
onMounted(() => { void load(); });

const providerOf = (code: string) => providers.value.find(p => p.code === code) ?? null;
const providerName = (code: unknown) => String(providerOf(String(code))?.name ?? code ?? "");
const domainLabel = (d: unknown) => SETTINGS_MODEL_DOMAINS.find(x => x.value === d)?.label ?? String(d ?? "");
const opsOf = (r: Row) => (Array.isArray(r.operations) ? (r.operations as Array<{ operation: string; enabled: boolean }>) : []);
const sec = (v: unknown) => (Number(v) > 0 ? `${Math.round(Number(v) / 1000)}s` : "—");

const filteredModels = computed(() => {
  const k = filter.keyword.trim().toLowerCase();
  return models.value.filter(m =>
    (!filter.provider || m.providerCode === filter.provider)
    && (!filter.domain || m.domain === filter.domain)
    && (!k || [m.code, m.name, m.upstreamModelCode].some(v => String(v ?? "").toLowerCase().includes(k))));
});

function showAdvanced(code: string) {
  filter.provider = code;
  tab.value = "models";
}

async function probe(row: Row) {
  try {
    await ElMessageBox.confirm("会向这个厂商真实发送一次请求（在对方那里留下访问记录，也可能触发限流）。", `连接测试 · ${row.name}`,
      { confirmButtonText: "开始测试", cancelButtonText: "取消" });
  } catch {
    return;
  }
  try {
    const r = await api<{ ok: boolean; statusCode?: number; errorKind?: string; message?: string; latencyMs?: number }>(
      `/admin/models/providers/${encodeURIComponent(String(row.code))}/probe`, { method: "POST" });
    if (r.ok) ElMessage.success(`连通：HTTP ${r.statusCode ?? "—"}，${r.latencyMs ?? 0}ms`);
    else ElMessage.warning({ message: `不通（${r.errorKind ?? "未知"}${r.statusCode ? ` · HTTP ${r.statusCode}` : ""}）：${r.message ?? ""}`, duration: 10000 });
  } catch (e) {
    ElMessage.error(settingsApiError(e).message);
  }
}
</script>

<template>
  <div class="settings-panel">
    <div class="section-head">
      <div>
        <h2>模型与厂商</h2>
        <p>填入各厂商的 API Key 即可启用内置模板；接自己的中转网关或新型号用「厂商 / 模型」两页。Key 只保存在这台电脑上。换电脑时用「导出配置 / 导入配置」整体同步（含 Key）。</p>
      </div>
      <ModelConfigTransfer @changed="load" />
    </div>
    <p v-if="loadError" class="settings-error">{{ loadError }} <button type="button" class="portal-link-button" @click="load">重试</button></p>

    <ElTabs v-model="tab" v-loading="loading">
      <ElTabPane label="模板" name="templates">
        <div class="tpl-grid">
          <ModelTemplateCard
            v-for="t in templates" :key="t.code" :template="t" :provider="providerOf(t.code)"
            @changed="load" @advanced="showAdvanced" />
        </div>
        <p v-if="!loading && !templates.length" class="cell-muted">没有内置模板。</p>
      </ElTabPane>

      <ElTabPane label="厂商" name="providers">
        <div class="toolbar">
          <span class="cell-muted">共 {{ providers.length }} 个厂商</span>
          <ElButton type="primary" @click="providerEditor = { open: true, row: null }">新建厂商</ElButton>
        </div>
        <ElTable :data="providers" row-key="code">
          <ElTableColumn label="厂商" min-width="150">
            <template #default="{ row }"><div>{{ row.name }}</div><code class="cell-sub">{{ row.code }}</code></template>
          </ElTableColumn>
          <ElTableColumn label="地址" min-width="180"><template #default="{ row }"><span class="cell-url">{{ row.baseUrl }}</span></template></ElTableColumn>
          <ElTableColumn label="认证" width="100">
            <template #default="{ row }">{{ row.authType }}<div class="cell-muted">{{ row.authType === "none" ? "" : row.hasApiKey ? "已填 Key" : "未填 Key" }}</div></template>
          </ElTableColumn>
          <ElTableColumn label="超时" width="120">
            <template #default="{ row }"><span class="cell-muted">{{ sec(row.connectTimeoutMs) }} / {{ sec(row.requestTimeoutMs) }} / {{ sec(row.idleTimeoutMs) }}</span></template>
          </ElTableColumn>
          <ElTableColumn label="状态" width="80">
            <template #default="{ row }">
              <ElTag size="small" :type="row.isReady ? 'success' : row.status === 'enabled' ? 'warning' : 'info'">
                {{ row.isReady ? "就绪" : row.status === "enabled" ? "缺 Key" : "停用" }}
              </ElTag>
            </template>
          </ElTableColumn>
          <ElTableColumn label="" width="210" align="right">
            <template #default="{ row }">
              <ElButton link type="primary" @click="probe(row)">测试连接</ElButton>
              <ElButton link type="primary" @click="showAdvanced(String(row.code))">模型</ElButton>
              <ElButton link type="primary" @click="providerEditor = { open: true, row }">编辑</ElButton>
            </template>
          </ElTableColumn>
        </ElTable>
      </ElTabPane>

      <ElTabPane label="模型" name="models">
        <div class="toolbar">
          <div class="filters">
            <ElInput v-model="filter.keyword" clearable placeholder="搜索编码、名称、上游模型名" style="width: 240px" />
            <ElSelect v-model="filter.provider" clearable placeholder="全部厂商" style="width: 180px">
              <ElOption v-for="p in providers" :key="String(p.code)" :value="String(p.code)" :label="String(p.name)" />
            </ElSelect>
            <ElSelect v-model="filter.domain" clearable placeholder="全部类型" style="width: 120px">
              <ElOption v-for="d in SETTINGS_MODEL_DOMAINS" :key="d.value" :value="d.value" :label="d.label" />
            </ElSelect>
          </div>
          <ElButton type="primary" @click="modelEditor = { open: true, row: null }">新建模型</ElButton>
        </div>
        <ElTable :data="filteredModels" row-key="code" :empty-text="models.length ? '没有符合筛选的模型' : '还没有模型'">
          <ElTableColumn label="模型" min-width="160">
            <template #default="{ row }"><div>{{ row.name }}</div><code class="cell-sub">{{ row.code }}</code></template>
          </ElTableColumn>
          <ElTableColumn label="厂商" min-width="100"><template #default="{ row }">{{ providerName(row.providerCode) }}</template></ElTableColumn>
          <ElTableColumn label="类型" width="70"><template #default="{ row }">{{ domainLabel(row.domain) }}</template></ElTableColumn>
          <ElTableColumn label="上游模型 / 协议" min-width="180">
            <template #default="{ row }"><div>{{ row.upstreamModelCode }}</div><code class="cell-sub">{{ row.apiProtocol }}</code></template>
          </ElTableColumn>
          <ElTableColumn label="操作" min-width="140">
            <template #default="{ row }">
              <span class="cell-muted">{{ opsOf(row).filter(o => o.enabled).map(o => o.operation).join("、") || "未绑定" }}</span>
            </template>
          </ElTableColumn>
          <ElTableColumn label="状态" width="80">
            <template #default="{ row }"><ElTag size="small" :type="row.status === 'enabled' ? 'success' : 'info'">{{ row.status === "enabled" ? "启用" : "停用" }}</ElTag></template>
          </ElTableColumn>
          <ElTableColumn label="" width="170" align="right">
            <template #default="{ row }">
              <ElButton link type="primary" @click="bindings = { open: true, model: row }">操作与参数</ElButton>
              <ElButton link type="primary" @click="modelEditor = { open: true, row }">编辑</ElButton>
            </template>
          </ElTableColumn>
        </ElTable>
      </ElTabPane>
    </ElTabs>

    <ProviderEditor v-model:open="providerEditor.open" :row="providerEditor.row" :providers="providers" :allow-private="allowPrivate" @saved="load" />
    <ModelEditor v-model:open="modelEditor.open" :row="modelEditor.row" :models="models" :providers="providers" :protocols="protocols" @saved="load" />
    <ModelBindingsDrawer v-model:open="bindings.open" :model="bindings.model" :operations="operations" @changed="load" />
  </div>
</template>

<style scoped>
.tpl-grid { display: grid; grid-template-columns: repeat(auto-fill, minmax(340px, 1fr)); gap: 12px; margin-top: 4px; }
.toolbar { display: flex; flex-wrap: wrap; align-items: center; justify-content: space-between; gap: 8px; margin-bottom: 10px; }
.filters { display: flex; flex-wrap: wrap; gap: 8px; }
.cell-sub { color: var(--portal-faint); font-size: 11px; }
.cell-muted { color: var(--portal-faint); font-size: 12px; }
.cell-url { font-size: 12px; word-break: break-all; }
.settings-error { color: var(--portal-danger); font-size: 13px; }
</style>
