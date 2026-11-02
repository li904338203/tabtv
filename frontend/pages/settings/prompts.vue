<script setup lang="ts">
/**
 * 设置 → 提示词（TT-T-19，TT-F-30；照来源系统管理端 `PromptsView.vue`）。
 *
 * 提示词是「覆盖行 > 代码内置原文」：一行覆盖都没有时用内置原文。每个用途一张卡片，标出现在生效的是
 * 内置还是你改过的版本；下面列出全部覆盖行（含模型专属版与已归档的）。
 * ⚠️ 导入一律**落草稿**、不自动发布：导入的内容要先在这里看一眼、试渲染，再显式发布。
 * ⚠️ Agent 系统提示词骨架与阶段说明是格式契约（代码拼），不在这里。
 */
import { ElButton, ElLoading, ElMessage, ElTable, ElTableColumn, ElTag } from "element-plus";
import PromptEditor from "~/components/settings/PromptEditor.vue";
import TransferButtons from "~/components/settings/TransferButtons.vue";
import { promptBadge } from "~/utils/builtinStatus";
import { normalizePromptSpec, type PromptRow, type PromptSpec } from "~/utils/promptForm";
import { settingsApiError } from "~/utils/settingsApi";
import { buildExport, downloadJson, exportFileName, importSummary } from "~/utils/settingsTransfer";

const vLoading = ElLoading.directive;

type ModelRow = { code: string; name?: string; domain?: string };

const api = useApi();
const specs = ref<PromptSpec[]>([]);
const rows = ref<PromptRow[]>([]);
const models = ref<ModelRow[]>([]);
const loading = ref(false);
const loadError = ref("");
const busy = ref(false);
const editor = ref<{ open: boolean; spec: PromptSpec | null; row: PromptRow | null }>({ open: false, spec: null, row: null });

const STATUS: Record<string, { label: string; type: "success" | "info" | "warning" }> = {
  published: { label: "已发布", type: "success" },
  draft: { label: "草稿", type: "warning" },
  archived: { label: "已归档", type: "info" }
};

async function load() {
  loading.value = true;
  loadError.value = "";
  try {
    const [s, r] = await Promise.all([
      api<Record<string, unknown>[]>("/admin/prompts/specs"),
      api<PromptRow[]>("/admin/prompts")
    ]);
    specs.value = s.map(normalizePromptSpec);
    rows.value = r ?? [];
  } catch (e) {
    loadError.value = settingsApiError(e).message;
  } finally {
    loading.value = false;
  }
}

onMounted(async () => {
  void load();
  try {
    const m = await api<ModelRow[] | { items: ModelRow[] }>("/admin/models");
    models.value = Array.isArray(m) ? m : (m.items ?? []);
  } catch {
    models.value = [];
  }
});

const specOf = (code: string) => specs.value.find(s => s.code === code);
const rowCount = (code: string) => rows.value.filter(r => r.code === code && r.status !== "archived").length;

function edit(spec: PromptSpec | undefined, row: PromptRow | null) {
  if (!spec) {
    ElMessage.warning("这个用途不在当前的规格目录里（可能已下线），不能编辑");
    return;
  }
  editor.value = { open: true, spec, row };
}
/** 卡片上的「编辑通用版」：已有通用行就编辑它，没有就从内置原文起步。 */
function editGeneric(spec: PromptSpec) {
  edit(spec, rows.value.find(r => r.code === spec.code && !r.modelCode) ?? null);
}

function exportAll() {
  // 只导出生效中的覆盖行（含草稿）：内置原文随应用附带，不需要导出
  const items = rows.value.filter(r => r.status !== "archived").map(r => ({
    code: r.code, modelCode: r.modelCode ?? "", content: r.draftContent || r.content, note: r.note ?? ""
  }));
  if (!items.length) {
    ElMessage.info("还没有改过任何提示词：当前全部用内置原文，没有可导出的内容");
    return;
  }
  downloadJson(exportFileName("prompts"), buildExport("prompts", items));
}

async function importItems(items: Record<string, unknown>[]) {
  busy.value = true;
  const failures: Array<{ key: string; message: string }> = [];
  let created = 0;
  let updated = 0;
  for (const it of items) {
    const code = String(it.code).trim();
    const modelCode = typeof it.modelCode === "string" ? it.modelCode.trim() : "";
    const key = modelCode ? `${code}/${modelCode}` : code;
    if (!specOf(code)) {
      failures.push({ key, message: "这个用途在本应用里不存在" });
      continue;
    }
    try {
      const exists = rows.value.some(r => r.code === code && (r.modelCode ?? "") === modelCode && r.status !== "archived");
      await api("/admin/prompts/draft", {
        method: "PUT",
        body: { code, modelCode, content: String(it.content ?? ""), note: typeof it.note === "string" ? it.note : "导入" }
      });
      if (exists) updated++;
      else created++;
    } catch (e) {
      failures.push({ key, message: settingsApiError(e).message });
    }
  }
  busy.value = false;
  const text = `${importSummary(created, updated, failures)}。导入的内容是草稿，逐条确认后再发布`;
  if (failures.length) ElMessage.warning({ message: text, duration: 12000 });
  else ElMessage.success(text);
  await load();
}
</script>

<template>
  <div class="settings-panel">
    <div class="section-head">
      <div>
        <h2>提示词</h2>
        <p>覆盖行 + 不可变的版本快照：草稿不影响线上，发布才生效，回滚是发布一个新版本。没有覆盖时用内置原文。</p>
      </div>
      <TransferButtons kind="prompts" :busy="busy" note="提示词导入后是草稿，不会自动发布。" @export="exportAll" @import="importItems" />
    </div>
    <p v-if="loadError" class="settings-error">{{ loadError }} <button type="button" class="portal-link-button" @click="load">重试</button></p>

    <div v-loading="loading" class="prompt-specs">
      <article v-for="s in specs" :key="s.code" class="prompt-spec">
        <header>
          <div><strong>{{ s.name }}</strong><code>{{ s.code }}</code></div>
          <ElTag size="small" :type="promptBadge(s, rows).type" effect="plain">{{ promptBadge(s, rows).label }}</ElTag>
        </header>
        <span class="prompt-muted">上限 {{ s.maxChars || "不限" }} 字 · {{ rowCount(s.code) }} 条覆盖</span>
        <div class="prompt-spec-vars">
          <ElTag v-for="v in s.required" :key="v" size="small">{{ v }}（必需）</ElTag>
          <ElTag v-for="v in s.optional" :key="v" size="small" type="info">{{ v }}</ElTag>
        </div>
        <div class="prompt-spec-actions">
          <ElButton size="small" @click="editGeneric(s)">编辑通用版</ElButton>
          <ElButton size="small" link type="primary" @click="edit(s, null)">新建模型专属版</ElButton>
        </div>
      </article>
    </div>

    <h3 class="settings-sub">覆盖行</h3>
    <ElTable :data="rows" :row-key="(r: PromptRow) => `${r.code}/${r.modelCode ?? '*'}`" empty-text="还没有改过任何提示词 —— 当前全部用内置原文。从上面的卡片开始写。">
      <ElTableColumn label="用途" min-width="200">
        <template #default="{ row }">
          <div>{{ specOf(row.code)?.name ?? row.code }}</div>
          <code class="cell-sub">{{ row.code }}</code>
        </template>
      </ElTableColumn>
      <ElTableColumn label="适用模型" min-width="160">
        <template #default="{ row }">{{ row.modelCode || "通用版（所有模型）" }}</template>
      </ElTableColumn>
      <ElTableColumn label="线上版本" width="90" align="right">
        <template #default="{ row }">v{{ row.version }}</template>
      </ElTableColumn>
      <ElTableColumn label="状态" width="90">
        <template #default="{ row }">
          <ElTag size="small" :type="STATUS[row.status]?.type ?? 'info'">{{ STATUS[row.status]?.label ?? row.status }}</ElTag>
        </template>
      </ElTableColumn>
      <ElTableColumn label="草稿" width="140">
        <template #default="{ row }">
          <ElTag v-if="row.hasUnpublishedDraft" size="small" type="warning">有未发布的修改</ElTag>
          <span v-else class="prompt-muted">—</span>
        </template>
      </ElTableColumn>
      <ElTableColumn label="" width="80" fixed="right">
        <template #default="{ row }">
          <ElButton link type="primary" @click="edit(specOf(row.code), row)">编辑</ElButton>
        </template>
      </ElTableColumn>
    </ElTable>
    <p class="settings-note">
      ⚠️ 系统提示词必须<b>渲染</b>后再发，变量要给全（含可选的）—— 少给一个就整条回落到内置原文。
      剧情、镜头描述这些上下文由调用方的用户消息送达，模板里只写「怎么做」。
    </p>

    <PromptEditor v-model:open="editor.open" :spec="editor.spec" :row="editor.row" :models="models" @changed="load" />
  </div>
</template>

<style scoped>
.section-head { display: flex; align-items: flex-start; justify-content: space-between; gap: 16px; }
.prompt-specs { display: grid; margin: 16px 0; grid-template-columns: repeat(auto-fill, minmax(280px, 1fr)); gap: 10px; }
.prompt-spec { display: grid; gap: 8px; padding: 14px; border: 1px solid var(--portal-line); border-radius: 8px; background: var(--portal-surface); }
.prompt-spec header { display: flex; align-items: flex-start; justify-content: space-between; gap: 10px; }
.prompt-spec header > div { display: grid; gap: 2px; }
.prompt-spec strong { color: var(--portal-text); font-weight: 500; }
.prompt-spec code { color: var(--portal-faint); font-size: 11px; }
.prompt-spec-vars { display: flex; flex-wrap: wrap; gap: 4px; }
.prompt-spec-actions { display: flex; gap: 6px; }
.prompt-muted { color: var(--portal-muted); font-size: 12px; }
.cell-sub { color: var(--portal-faint); font-size: 11px; }
.settings-sub { margin: 20px 0 8px; color: var(--portal-text); font-size: 15px; font-weight: 500; }
.settings-note { margin-top: 12px; color: var(--portal-muted); font-size: 12px; line-height: 1.6; }
.settings-error { color: var(--portal-danger); font-size: 13px; }
</style>
