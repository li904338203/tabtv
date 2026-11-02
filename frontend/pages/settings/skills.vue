<script setup lang="ts">
/**
 * 设置 → Agent 技能（TT-T-19，TT-F-29；列表照来源系统管理端 `AgentSkillsView.vue`）。
 *
 * ⚠️ 没有删除：历史会话记着加载过哪些技能，删掉就追溯不了 —— 下线用「启用」开关。
 * ⚠️ 内置技能改过之后标「已修改」，启动时不再被内置内容覆盖；编辑抽屉里可「恢复内置版本」。
 * ⚠️ 导入按 code 覆盖：新建的一律停用；覆盖已有的走正常的保存接口（同样试渲染、同样乐观锁），
 *    附属文档按名称新建或覆盖，导入文件里没有的附属文档不动。
 */
import { ElButton, ElInput, ElLoading, ElMessage, ElMessageBox, ElSwitch, ElTable, ElTableColumn, ElTag } from "element-plus";
import SkillEditor from "~/components/settings/SkillEditor.vue";
import TransferButtons from "~/components/settings/TransferButtons.vue";
import { skillExportItem, skillImportPayload, skillImportRefs, type SkillModelOption, type ToolOption } from "~/utils/agentSkillForm";
import { builtinBadge } from "~/utils/builtinStatus";
import { settingsApiError } from "~/utils/settingsApi";
import { buildExport, downloadJson, exportFileName, importSummary, planImport } from "~/utils/settingsTransfer";

const vLoading = ElLoading.directive;

type Row = Record<string, unknown>;

const api = useApi();
const rows = ref<Row[]>([]);
const loading = ref(false);
const loadError = ref("");
const keyword = ref("");
const tools = ref<ToolOption[]>([]);
const vars = ref<string[]>([]);
const models = ref<SkillModelOption[]>([]);
const editor = ref<{ open: boolean; id: string | null }>({ open: false, id: null });
const busy = ref(false);

const shown = computed(() => {
  const k = keyword.value.trim().toLowerCase();
  if (!k) return rows.value;
  return rows.value.filter(r => [r.name, r.code, r.category].some(v => String(v ?? "").toLowerCase().includes(k)));
});

async function load() {
  loading.value = true;
  loadError.value = "";
  try {
    rows.value = (await api<{ items: Row[] }>("/admin/agent/skills")).items ?? [];
  } catch (e) {
    loadError.value = settingsApiError(e).message;
  } finally {
    loading.value = false;
  }
}

onMounted(async () => {
  void load();
  try {
    const [t, m] = await Promise.all([
      api<{ items: ToolOption[]; vars: string[] }>("/admin/agent/tools"),
      api<{ items: SkillModelOption[] }>("/admin/agent/model-options")
    ]);
    tools.value = t.items ?? [];
    vars.value = t.vars ?? [];
    models.value = m.items ?? [];
  } catch (e) {
    ElMessage.error(`取技能配置的可选项失败：${settingsApiError(e).message}`);
  }
});

const toolCount = (r: Row) => (Array.isArray(r.tools) ? r.tools.length : 0);

async function toggle(row: Row) {
  const enable = row.isEnabled !== true;
  if (!enable) {
    try {
      await ElMessageBox.confirm("停用后技能列表里不再出现它；已加载它的会话从下一轮起不再使用它的主说明，历史消息照常可看。",
        `停用「${row.name}」`, { confirmButtonText: "停用", cancelButtonText: "取消", type: "warning" });
    } catch {
      return;
    }
  }
  try {
    await api(`/admin/agent/skills/${row.id}/${enable ? "enable" : "disable"}`, { method: "POST" });
    ElMessage.success(enable ? "已启用：技能列表里可以看到它了" : "已停用");
    await load();
  } catch (e) {
    const err = settingsApiError(e);
    const problems = Array.isArray(err.details?.problems) ? (err.details!.problems as string[]).join("；") : "";
    ElMessage.error({ message: problems ? `${err.message}：${problems}` : err.message, duration: 10000 });
  }
}

async function exportAll() {
  busy.value = true;
  try {
    // 列表不带正文与附属文档，逐个取详情
    const items: Row[] = [];
    for (const r of rows.value) {
      const d = await api<Row>(`/admin/agent/skills/${r.id}`);
      items.push(skillExportItem(d, Array.isArray(d.references) ? (d.references as Row[]) : []));
    }
    downloadJson(exportFileName("skills"), buildExport("skills", items));
  } catch (e) {
    ElMessage.error(`导出失败：${settingsApiError(e).message}`);
  } finally {
    busy.value = false;
  }
}

async function importItems(items: Row[]) {
  busy.value = true;
  const plan = planImport(items, rows.value, e => String(e.code), "code");
  const failures: Array<{ key: string; message: string }> = [];
  let created = 0;
  let updated = 0;
  const run = async (item: Row, current: Row | null) => {
    const code = String(item.code);
    try {
      const cur = current ? await api<Row>(`/admin/agent/skills/${current.id}`) : null;
      const saved = cur
        ? await api<Row>(`/admin/agent/skills/${cur.id}`, { method: "PUT", body: skillImportPayload(item, cur) })
        : await api<Row>("/admin/agent/skills", { method: "POST", body: skillImportPayload(item, null) });
      let version = Number(saved.version);
      const existing = new Map(((cur?.references as Row[] | undefined) ?? []).map(f => [String(f.name), String(f.id)]));
      for (const f of skillImportRefs(item)) {
        const body = { skillVersion: version, name: f.name, body: f.body, sort: f.sort };
        const refId = existing.get(f.name);
        const out = refId
          ? await api<{ skillVersion: number }>(`/admin/agent/skills/${saved.id}/references/${refId}`, { method: "PUT", body })
          : await api<{ skillVersion: number }>(`/admin/agent/skills/${saved.id}/references`, { method: "POST", body });
        version = out.skillVersion;
      }
      if (cur) updated++;
      else created++;
    } catch (e) {
      failures.push({ key: code, message: settingsApiError(e).message });
    }
  };
  for (const item of plan.creates) await run(item, null);
  for (const u of plan.updates) await run(u.item, u.current);
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
        <h2>Agent 技能</h2>
        <p>技能 = 主说明 + 按需读取的附属文档 + 步骤检查点 + 工具白名单 + 绑定模型。保存后进行中的会话从下一轮起生效。</p>
      </div>
      <div class="settings-actions">
        <TransferButtons kind="skills" :busy="busy" note="附属文档按名称新建或覆盖；封面不随文件导入。" @export="exportAll" @import="importItems" />
        <ElButton type="primary" @click="editor = { open: true, id: null }">新建技能</ElButton>
      </div>
    </div>
    <p v-if="loadError" class="settings-error">{{ loadError }} <button type="button" class="portal-link-button" @click="load">重试</button></p>
    <ElInput v-model="keyword" clearable placeholder="名称 / code / 分类" class="settings-search" />

    <ElTable v-loading="loading" :data="shown" row-key="id" size="default" class="settings-table" empty-text="还没有技能">
      <ElTableColumn label="技能" min-width="200">
        <template #default="{ row }">
          <div class="cell-main">{{ row.name }}</div>
          <code class="cell-sub">{{ row.code }}</code>
        </template>
      </ElTableColumn>
      <ElTableColumn prop="category" label="分类" width="100" />
      <ElTableColumn label="来源" width="130">
        <template #default="{ row }">
          <ElTag size="small" :type="builtinBadge(row).type" effect="plain">{{ builtinBadge(row).label }}</ElTag>
        </template>
      </ElTableColumn>
      <ElTableColumn label="白名单" width="110">
        <template #default="{ row }">
          <span v-if="toolCount(row)">收窄为 {{ toolCount(row) }} 个</span>
          <span v-else class="cell-muted">不收窄</span>
        </template>
      </ElTableColumn>
      <ElTableColumn label="附属文档" width="90" align="right">
        <template #default="{ row }">{{ row.refCount ?? 0 }}</template>
      </ElTableColumn>
      <ElTableColumn label="快捷入口" width="90">
        <template #default="{ row }">
          <ElTag v-if="row.isQuickEntry" size="small">快捷入口</ElTag>
          <span v-else class="cell-muted">—</span>
        </template>
      </ElTableColumn>
      <ElTableColumn prop="sort" label="排序" width="70" align="right" />
      <ElTableColumn label="版本" width="70" align="right">
        <template #default="{ row }">v{{ row.version }}</template>
      </ElTableColumn>
      <ElTableColumn label="启用" width="70">
        <template #default="{ row }">
          <ElSwitch :model-value="row.isEnabled === true" size="small" @change="toggle(row)" />
        </template>
      </ElTableColumn>
      <ElTableColumn label="" width="80" fixed="right">
        <template #default="{ row }">
          <ElButton link type="primary" @click="editor = { open: true, id: String(row.id) }">编辑</ElButton>
        </template>
      </ElTableColumn>
    </ElTable>
    <p class="settings-note">
      ⚠️ 正文是模板：保存与启用前都会按运行时的变量<b>试渲染</b>，渲染失败拒绝保存并说明原因。
      工具白名单<b>只能收窄</b>状态机允许的工具；「付费前必须经用户确认」写在代码里，任何技能都关不掉。技能没有删除，下线用「启用」开关。
    </p>

    <SkillEditor v-model:open="editor.open" :skill-id="editor.id" :list="rows" :tools="tools" :vars="vars" :models="models" @changed="load" />
  </div>
</template>

<style scoped>
.section-head { display: flex; align-items: flex-start; justify-content: space-between; gap: 16px; }
.settings-actions { display: flex; flex-wrap: wrap; gap: 8px; }
.settings-search { max-width: 320px; margin: 12px 0; }
.settings-table { width: 100%; }
.cell-main { color: var(--portal-text); }
.cell-sub { color: var(--portal-faint); font-size: 11px; }
.cell-muted { color: var(--portal-faint); }
.settings-note { margin-top: 12px; color: var(--portal-muted); font-size: 12px; line-height: 1.6; }
.settings-error { color: var(--portal-danger); font-size: 13px; }
</style>
