<script setup lang="ts">
/**
 * 设置 → 图片预设（TT-T-20，TT-F-31）：列出全部预设（含停用），可编辑、新增、停用 / 启用、恢复内置版本。
 *
 * ⚠️ 停用不删行：历史节点里存着 presetCode，停用只让它从画布选择器里消失，打开旧节点按「无预设」处理。
 * ⚠️ 每次改动都推进画布配置的版本号，画布下次拉配置即为新内容，不用重启。
 * ⚠️ 导入按 code 覆盖已有预设（内置的会标「已修改」）；新建的预设导入后是启用的 —— 预设没有「草稿」语义，
 *    停用的预设在画布上就是不存在，导进来却看不到只会让人以为导入失败。
 */
import { ElButton, ElLoading, ElMessage, ElMessageBox, ElSwitch, ElTable, ElTableColumn, ElTag } from "element-plus";
import PresetEditor from "~/components/settings/PresetEditor.vue";
import TransferButtons from "~/components/settings/TransferButtons.vue";
import { builtinBadge } from "~/utils/builtinStatus";
import { PRESET_INPUT_MODES, groupPresets, presetExportItem } from "~/utils/presetForm";
import { settingsApiError } from "~/utils/settingsApi";
import { buildExport, downloadJson, exportFileName, importSummary, planImport } from "~/utils/settingsTransfer";

const vLoading = ElLoading.directive;

type Row = Record<string, unknown>;
type Operation = { code: string; name: string; domain: string };

const api = useApi();
const rows = ref<Row[]>([]);
const operations = ref<Operation[]>([]);
const loading = ref(false);
const loadError = ref("");
const busy = ref(false);
const editor = ref<{ open: boolean; row: Row | null }>({ open: false, row: null });
const groups = computed(() => groupPresets(rows.value));
const inputLabel = (v: unknown) => PRESET_INPUT_MODES.find(m => m.value === v)?.label ?? String(v ?? "");

async function load() {
  loading.value = true;
  loadError.value = "";
  try {
    rows.value = (await api<{ items: Row[] }>("/admin/image-presets")).items ?? [];
  } catch (e) {
    loadError.value = settingsApiError(e).message;
  } finally {
    loading.value = false;
  }
}

onMounted(async () => {
  void load();
  try {
    operations.value = await api<Operation[]>("/admin/operations");
  } catch {
    operations.value = [];
  }
});

async function toggle(row: Row) {
  const enable = row.isEnabled !== true;
  if (!enable) {
    try {
      await ElMessageBox.confirm("停用后它从画布的预设选择器里消失；已经用过它的节点照常打开，按「无预设」处理。", `停用「${row.name}」`,
        { confirmButtonText: "停用", cancelButtonText: "取消", type: "warning" });
    } catch {
      return;
    }
  }
  try {
    await api(`/admin/image-presets/${encodeURIComponent(String(row.code))}/${enable ? "enable" : "disable"}`, { method: "POST" });
    ElMessage.success(enable ? "已启用" : "已停用");
    await load();
  } catch (e) {
    ElMessage.error(settingsApiError(e).message);
  }
}

function exportAll() {
  downloadJson(exportFileName("presets"), buildExport("presets", rows.value.map(presetExportItem)));
}

async function importItems(items: Row[]) {
  busy.value = true;
  const plan = planImport(items, rows.value, e => String(e.code), "code");
  const failures: Array<{ key: string; message: string }> = [];
  let created = 0;
  let updated = 0;
  for (const it of plan.creates) {
    try {
      await api("/admin/image-presets", { method: "POST", body: it });
      created++;
    } catch (e) {
      failures.push({ key: String(it.code), message: settingsApiError(e).message });
    }
  }
  for (const { item } of plan.updates) {
    try {
      await api(`/admin/image-presets/${encodeURIComponent(String(item.code))}`, { method: "PUT", body: item });
      updated++;
    } catch (e) {
      failures.push({ key: String(item.code), message: settingsApiError(e).message });
    }
  }
  busy.value = false;
  const text = importSummary(created, updated, failures);
  if (failures.length) ElMessage.warning({ message: text, duration: 12000 });
  else ElMessage.success(text);
  await load();
}
</script>

<template>
  <div class="settings-panel">
    <div class="section-head">
      <div>
        <h2>图片预设</h2>
        <p>一个预设 = 归组 + 文案 + 配方提示词 + 参数默认。配方只在服务端拼进生成请求；停用不删除，历史节点仍可追溯。</p>
      </div>
      <div class="settings-actions">
        <TransferButtons kind="presets" :busy="busy" note="预设导入后保持启用（同 code 覆盖已有的，内置预设会标「已修改」）。" @export="exportAll" @import="importItems" />
        <ElButton type="primary" @click="editor = { open: true, row: null }">新建预设</ElButton>
      </div>
    </div>
    <p v-if="loadError" class="settings-error">{{ loadError }} <button type="button" class="portal-link-button" @click="load">重试</button></p>

    <div v-loading="loading">
      <section v-for="g in groups" :key="g.group" class="preset-group">
        <h3>{{ g.label }} <span class="cell-muted">（{{ g.items.length }}）</span></h3>
        <ElTable :data="g.items" row-key="code">
          <ElTableColumn label="预设" min-width="200">
            <template #default="{ row }">
              <div>{{ row.name }} <ElTag v-if="row.isNew" size="small">NEW</ElTag></div>
              <code class="cell-sub">{{ row.code }}</code>
            </template>
          </ElTableColumn>
          <ElTableColumn label="来源" width="130">
            <template #default="{ row }">
              <ElTag size="small" :type="builtinBadge(row).type" effect="plain">{{ builtinBadge(row).label }}</ElTag>
            </template>
          </ElTableColumn>
          <ElTableColumn label="输入要求" width="130">
            <template #default="{ row }">{{ inputLabel(row.inputMode) }}</template>
          </ElTableColumn>
          <ElTableColumn prop="operation" label="操作" width="130" />
          <ElTableColumn prop="sort" label="排序" width="70" align="right" />
          <ElTableColumn label="启用" width="70">
            <template #default="{ row }">
              <ElSwitch :model-value="row.isEnabled === true" size="small" @change="toggle(row)" />
            </template>
          </ElTableColumn>
          <ElTableColumn label="" width="80" fixed="right">
            <template #default="{ row }">
              <ElButton link type="primary" @click="editor = { open: true, row }">编辑</ElButton>
            </template>
          </ElTableColumn>
        </ElTable>
      </section>
      <p v-if="!loading && !rows.length" class="cell-muted">还没有图片预设。</p>
    </div>

    <PresetEditor v-model:open="editor.open" :row="editor.row" :list="rows" :operations="operations" @changed="load" />
  </div>
</template>

<style scoped>
.section-head { display: flex; align-items: flex-start; justify-content: space-between; gap: 16px; }
.settings-actions { display: flex; flex-wrap: wrap; gap: 8px; }
.preset-group { margin-top: 20px; }
.preset-group h3 { margin: 0 0 8px; color: var(--portal-text); font-size: 15px; font-weight: 500; }
.cell-sub { color: var(--portal-faint); font-size: 11px; }
.cell-muted { color: var(--portal-faint); font-size: 12px; }
.settings-error { color: var(--portal-danger); font-size: 13px; }
</style>
