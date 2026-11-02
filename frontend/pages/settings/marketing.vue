<script setup lang="ts">
/**
 * 设置 → 营销（TT-T-21，TT-F-32）：创意 / Hook / 风格选项与分类、预置营销角色。
 * 列表照来源系统管理端 `MarketingOptionsView.vue`、`MarketingCharactersView.vue`。营销 Agent 读的就是这里的数据。
 *
 * ⚠️ 列表不按投放时段过滤（看的是「配了什么」）；投放状态由服务端现算后随行下发，不拿本地时钟再算一遍。
 * ⚠️ 没有删除：历史会话与生成快照按 (code, 版本) 引用，下线用停用。
 * ⚠️ 导入导出只搬文字内容：封面、角色图与参考音频的媒体 ID 只在本机有意义，导入后需要重新上传
 *    （启用时服务端会挡下没封面的选项）。覆盖已有条目时保留它现有的媒体。
 * ⚠️ 营销选项与角色没有「内置版本」标记（内置营销内容随包安装后就是普通数据），所以这里没有「恢复内置版本」。
 */
import { ElButton, ElLoading, ElMessage, ElMessageBox, ElSwitch, ElTable, ElTableColumn, ElTabPane, ElTabs, ElTag } from "element-plus";
import MarketingCategoriesDrawer from "~/components/settings/MarketingCategoriesDrawer.vue";
import MarketingCharacterEditor from "~/components/settings/MarketingCharacterEditor.vue";
import MarketingOptionEditor from "~/components/settings/MarketingOptionEditor.vue";
import TransferButtons from "~/components/settings/TransferButtons.vue";
import {
  MARKETING_KINDS, characterExportItem, characterImportPayload, marketingIssuesOf, marketingKindLabel, marketingLifecycleTag,
  optionExportItem, optionImportPayload, type SettingsMarketingKind
} from "~/utils/marketingForm";
import { settingsApiError, settingsMediaUrl } from "~/utils/settingsApi";
import { buildExport, downloadJson, exportFileName, importSummary, planImport } from "~/utils/settingsTransfer";

const vLoading = ElLoading.directive;

type Row = Record<string, unknown>;
type Tab = SettingsMarketingKind | "characters";

const api = useApi();
const tab = ref<Tab>("creative");
const options = ref<Row[]>([]);
const characters = ref<Row[]>([]);
const categories = ref<Row[]>([]);
const models = ref<Array<{ code: string; name: string; domain: string; status: string }>>([]);
const skills = ref<Array<{ code: string; name: string; isEnabled: boolean; scenes: string[] }>>([]);
const loading = ref(false);
const loadError = ref("");
const busy = ref(false);
const optionEditor = ref<{ open: boolean; id: string | null }>({ open: false, id: null });
const characterEditor = ref<{ open: boolean; id: string | null }>({ open: false, id: null });
const catDrawer = ref(false);

const kind = computed<SettingsMarketingKind>(() => (tab.value === "characters" ? "creative" : tab.value));
const shownOptions = computed(() => options.value.filter(o => o.kind === tab.value));
const nextSort = computed(() => shownOptions.value.reduce((m, r) => Math.max(m, Number(r.sort ?? 0)), 0) + 10);
const categoryName = (r: Row) => {
  const c = categories.value.find(x => x.kind === r.kind && x.code === r.categoryCode);
  return c ? `${c.name}${c.isEnabled ? "" : "（停用）"}` : `${r.categoryCode}（不存在）`;
};
const windowText = (r: Row) => (r.startAt || r.endAt ? `${r.startAt || "不限"} ~ ${r.endAt || "不限"}` : "长期");

/** 管理端列表是游标分页：设置页一次取完（条目是运营配置，量级几十到几百）。 */
async function listAll(path: string): Promise<Row[]> {
  const out: Row[] = [];
  let cursor = "";
  for (let i = 0; i < 50; i++) {
    const page = await api<{ items: Row[]; nextCursor?: string | null }>(path, { query: { limit: 100, ...(cursor ? { cursor } : {}) } });
    out.push(...(page.items ?? []));
    if (!page.nextCursor) break;
    cursor = page.nextCursor;
  }
  return out;
}

async function loadCategories() {
  try {
    categories.value = (await api<{ items: Row[] }>("/admin/marketing/categories")).items ?? [];
  } catch (e) {
    ElMessage.error(`取分类失败：${settingsApiError(e).message}`);
  }
}

async function load() {
  loading.value = true;
  loadError.value = "";
  try {
    const [o, c] = await Promise.all([listAll("/admin/marketing/options"), listAll("/admin/marketing/preset-characters")]);
    options.value = o;
    characters.value = c;
  } catch (e) {
    loadError.value = settingsApiError(e).message;
  } finally {
    loading.value = false;
  }
}

onMounted(async () => {
  void load();
  void loadCategories();
  try {
    const [m, s] = await Promise.all([
      api<{ items: Array<{ code: string; name: string; domain: string; status: string }> }>("/admin/agent/model-options"),
      api<{ items: Row[] }>("/admin/agent/skills")
    ]);
    models.value = m.items ?? [];
    skills.value = (s.items ?? []).map(x => ({
      code: String(x.code), name: String(x.name), isEnabled: x.isEnabled === true,
      scenes: Array.isArray(x.scenes) ? x.scenes.map(String) : []
    }));
  } catch (e) {
    ElMessage.error(`取推荐模型 / 默认技能的可选项失败：${settingsApiError(e).message}`);
  }
});

async function toggle(path: string, row: Row, label: string) {
  const enable = row.isEnabled !== true;
  try {
    await ElMessageBox.confirm(
      enable
        ? "启用前服务端会完整校验一次（封面、提示词片段、引用的分类 / 模型 / 技能），不过会列出原因。"
        : "停用后营销页不再出现它，新会话也选不到；不删除，历史会话与已生成的内容仍按原版本可查。",
      `${enable ? "启用" : "停用"}「${row.name}」`,
      { confirmButtonText: enable ? "启用" : "停用", cancelButtonText: "取消", type: enable ? "info" : "warning" }
    );
  } catch {
    return;
  }
  try {
    await api(`${path}/${row.id}/${enable ? "enable" : "disable"}`, { method: "POST" });
    ElMessage.success(enable ? `已启用${label}` : `已停用${label}`);
    await load();
  } catch (e) {
    const err = settingsApiError(e);
    const issues = marketingIssuesOf(err.details);
    ElMessage.error({ message: issues.length ? `${err.message}：${issues.join("；")}` : err.message, duration: 10000 });
  }
}

const transferKind = computed(() => (tab.value === "characters" ? "marketing-characters" : "marketing-options"));

function exportAll() {
  if (tab.value === "characters") {
    downloadJson(exportFileName("marketing-characters"), buildExport("marketing-characters", characters.value.map(characterExportItem)));
  } else {
    // 导出全部类型（创意 / Hook / 风格一起），导入时按条目自己的 kind 落位
    downloadJson(exportFileName("marketing-options"), buildExport("marketing-options", options.value.map(optionExportItem)));
  }
}

async function importItems(items: Row[]) {
  busy.value = true;
  const failures: Array<{ key: string; message: string }> = [];
  let created = 0;
  let updated = 0;
  if (tab.value === "characters") {
    const plan = planImport(items, characters.value, e => String(e.name).trim().toLowerCase(), "name");
    // 名称不区分大小写唯一：键统一转小写比
    const byName = new Map(characters.value.map(c => [String(c.name).trim().toLowerCase(), c]));
    for (const it of [...plan.creates, ...plan.updates.map(u => u.item)]) {
      const cur = byName.get(String(it.name).trim().toLowerCase()) ?? null;
      try {
        if (cur) {
          await api(`/admin/marketing/preset-characters/${cur.id}`, { method: "PUT", body: characterImportPayload(it, cur) });
          updated++;
        } else {
          await api("/admin/marketing/preset-characters", { method: "POST", body: characterImportPayload(it, null) });
          created++;
        }
      } catch (e) {
        failures.push({ key: String(it.name), message: settingsApiError(e).message });
      }
    }
  } else {
    const keyOf = (r: Row) => `${r.kind}:${r.code}`;
    const byKey = new Map(options.value.map(o => [keyOf(o), o]));
    for (const it of items) {
      const cur = byKey.get(keyOf(it)) ?? null;
      try {
        if (cur) {
          await api(`/admin/marketing/options/${cur.id}`, { method: "PUT", body: optionImportPayload(it, cur) });
          updated++;
        } else {
          await api("/admin/marketing/options", { method: "POST", body: optionImportPayload(it, null) });
          created++;
        }
      } catch (e) {
        const err = settingsApiError(e);
        const issues = marketingIssuesOf(err.details);
        failures.push({ key: `${marketingKindLabel(String(it.kind))} ${it.code}`, message: issues.length ? issues.join("；") : err.message });
      }
    }
  }
  busy.value = false;
  const text = importSummary(created, updated, failures);
  if (failures.length) ElMessage.warning({ message: text, duration: 12000 });
  else ElMessage.success(`${text}。封面与图片需要重新上传后才能启用`);
  await load();
}
</script>

<template>
  <div class="settings-panel">
    <div class="section-head">
      <div>
        <h2>营销</h2>
        <p>创意 / Hook / 风格是营销页的选项：用户只看到名称、说明与封面，提示词片段只在服务端拼接。新建一律停用，上线在列表上单独启用。</p>
      </div>
      <div class="settings-actions">
        <TransferButtons :kind="transferKind" :busy="busy" note="只导入文字内容，封面 / 图片 / 音频需重新上传。" @export="exportAll" @import="importItems" />
        <template v-if="tab !== 'characters'">
          <ElButton @click="catDrawer = true">分类管理</ElButton>
          <ElButton type="primary" @click="optionEditor = { open: true, id: null }">新建{{ marketingKindLabel(tab) }}</ElButton>
        </template>
        <ElButton v-else type="primary" @click="characterEditor = { open: true, id: null }">新建营销角色</ElButton>
      </div>
    </div>
    <p v-if="loadError" class="settings-error">{{ loadError }} <button type="button" class="portal-link-button" @click="load">重试</button></p>

    <ElTabs v-model="tab">
      <ElTabPane v-for="k in MARKETING_KINDS" :key="k.value" :name="k.value" :label="k.label" />
      <ElTabPane name="characters" label="预置角色" />
    </ElTabs>

    <ElTable v-if="tab !== 'characters'" v-loading="loading" :data="shownOptions" row-key="id" :empty-text="`还没有${marketingKindLabel(tab)}。先在「分类管理」里建分类，再新建。`">
      <ElTableColumn label="名称" min-width="180">
        <template #default="{ row }">
          <div>{{ row.name }}</div>
          <code class="cell-sub">{{ row.code }}</code>
        </template>
      </ElTableColumn>
      <ElTableColumn label="封面" width="90">
        <template #default="{ row }">
          <img v-if="row.coverMediaIds?.length" :src="settingsMediaUrl(row.coverMediaIds[0])" alt="" class="cell-thumb">
          <span v-else class="cell-muted">无</span>
        </template>
      </ElTableColumn>
      <ElTableColumn label="分类" width="130">
        <template #default="{ row }">{{ categoryName(row) }}</template>
      </ElTableColumn>
      <ElTableColumn label="投放" min-width="160">
        <template #default="{ row }">
          <ElTag size="small" :type="marketingLifecycleTag(String(row.lifecycle)).type">{{ marketingLifecycleTag(String(row.lifecycle)).label }}</ElTag>
          <div class="cell-sub">{{ windowText(row) }}</div>
        </template>
      </ElTableColumn>
      <ElTableColumn label="问题" width="80">
        <template #default="{ row }">
          <span v-if="row.warnings?.length" class="cell-warn" :title="marketingIssuesOf({ issues: row.warnings }).join('\n')">{{ row.warnings.length }} 条</span>
          <span v-else class="cell-muted">—</span>
        </template>
      </ElTableColumn>
      <ElTableColumn label="版本" width="70" align="right">
        <template #default="{ row }">v{{ row.version }}</template>
      </ElTableColumn>
      <ElTableColumn prop="sort" label="排序" width="70" align="right" />
      <ElTableColumn label="启用" width="70">
        <template #default="{ row }"><ElSwitch :model-value="row.isEnabled === true" size="small" @change="toggle('/admin/marketing/options', row, '')" /></template>
      </ElTableColumn>
      <ElTableColumn label="" width="80" fixed="right">
        <template #default="{ row }"><ElButton link type="primary" @click="optionEditor = { open: true, id: String(row.id) }">编辑</ElButton></template>
      </ElTableColumn>
    </ElTable>

    <ElTable v-else v-loading="loading" :data="characters" row-key="id" empty-text="还没有预置营销角色">
      <ElTableColumn label="角色" min-width="200">
        <template #default="{ row }">
          <div class="cell-char">
            <img v-if="row.imageMediaIds?.length" :src="settingsMediaUrl(row.imageMediaIds[0])" alt="" class="cell-thumb">
            <div><div>{{ row.name }}</div><div class="cell-sub">{{ row.summary }}</div></div>
          </div>
        </template>
      </ElTableColumn>
      <ElTableColumn label="图片" width="70" align="right">
        <template #default="{ row }">{{ row.imageMediaIds?.length ?? 0 }}</template>
      </ElTableColumn>
      <ElTableColumn label="参考音频" width="90">
        <template #default="{ row }">{{ row.voiceMediaId ? "有" : "—" }}</template>
      </ElTableColumn>
      <ElTableColumn label="问题" width="80">
        <template #default="{ row }">
          <span v-if="row.warnings?.length" class="cell-warn" :title="marketingIssuesOf({ issues: row.warnings }).join('\n')">{{ row.warnings.length }} 条</span>
          <span v-else class="cell-muted">—</span>
        </template>
      </ElTableColumn>
      <ElTableColumn label="版本" width="70" align="right">
        <template #default="{ row }">v{{ row.version }}</template>
      </ElTableColumn>
      <ElTableColumn label="启用" width="70">
        <template #default="{ row }"><ElSwitch :model-value="row.isEnabled === true" size="small" @change="toggle('/admin/marketing/preset-characters', row, '')" /></template>
      </ElTableColumn>
      <ElTableColumn label="" width="80" fixed="right">
        <template #default="{ row }"><ElButton link type="primary" @click="characterEditor = { open: true, id: String(row.id) }">编辑</ElButton></template>
      </ElTableColumn>
    </ElTable>

    <MarketingOptionEditor
v-model:open="optionEditor.open" :option-id="optionEditor.id" :kind="kind" :categories="categories"
      :models="models" :skills="skills" :next-sort="nextSort" @changed="load" />
    <MarketingCharacterEditor v-model:open="characterEditor.open" :character-id="characterEditor.id" @changed="load" />
    <MarketingCategoriesDrawer v-model:open="catDrawer" :categories="categories" :kind="kind" @changed="loadCategories" />
  </div>
</template>

<style scoped>
.section-head { display: flex; align-items: flex-start; justify-content: space-between; gap: 16px; }
.settings-actions { display: flex; flex-wrap: wrap; gap: 8px; }
.cell-sub { color: var(--portal-faint); font-size: 11px; }
.cell-muted { color: var(--portal-faint); font-size: 12px; }
.cell-warn { color: var(--portal-gold); font-size: 12px; cursor: help; }
.cell-thumb { width: 48px; height: 48px; object-fit: cover; border-radius: 6px; background: var(--portal-surface-2); }
.cell-char { display: flex; align-items: center; gap: 10px; }
.settings-error { color: var(--portal-danger); font-size: 13px; }
</style>
