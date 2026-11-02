<script setup lang="ts">
/**
 * 营销分类管理（TT-T-21；照来源系统管理端 `MarketingCategoriesDrawer.vue`）。
 *
 * ⚠️ 分类没有删除：选项按 categoryCode 引用它，下线用停用（停用的分类下的选项在用户端整组不出现）。
 * ⚠️ code 建好后不能改。
 */
import { ElButton, ElDrawer, ElInput, ElInputNumber, ElMessage, ElSwitch, ElTable, ElTableColumn, ElTabPane, ElTabs } from "element-plus";
import { MARKETING_KINDS, categoryIssue, type CategoryDraft, type SettingsMarketingKind } from "~/utils/marketingForm";
import { settingsApiError } from "~/utils/settingsApi";

type Row = Record<string, unknown>;

const props = defineProps<{ open: boolean; categories: Row[]; kind: SettingsMarketingKind }>();
const emit = defineEmits<{ (e: "update:open", v: boolean): void; (e: "changed"): void }>();

const api = useApi();
const tab = ref<SettingsMarketingKind>("creative");
const edits = ref<CategoryDraft[]>([]);
const fresh = reactive<CategoryDraft>({ id: "", kind: "creative", code: "", name: "", sort: 10, isEnabled: true });
const saving = ref(false);

watch(() => props.open, open => {
  if (!open) return;
  tab.value = props.kind;
  reset();
});
watch(() => props.categories, reset);

function reset() {
  edits.value = props.categories.map(c => ({
    id: String(c.id), kind: c.kind as SettingsMarketingKind, code: String(c.code), name: String(c.name),
    sort: Number(c.sort ?? 0), isEnabled: c.isEnabled === true
  }));
  Object.assign(fresh, { id: "", kind: tab.value, code: "", name: "", sort: nextSort(), isEnabled: true });
}
const nextSort = () => edits.value.filter(c => c.kind === tab.value).reduce((m, c) => Math.max(m, c.sort), 0) + 10;
const shown = computed(() => edits.value.filter(c => c.kind === tab.value));
watch(tab, () => Object.assign(fresh, { kind: tab.value, sort: nextSort() }));

async function saveRow(c: CategoryDraft) {
  const issue = categoryIssue(c, false, props.categories);
  if (issue) {
    ElMessage.warning(issue);
    return;
  }
  saving.value = true;
  try {
    await api(`/admin/marketing/categories/${c.id}`, { method: "PUT", body: { kind: c.kind, code: c.code, name: c.name.trim(), sort: c.sort, isEnabled: c.isEnabled } });
    ElMessage.success("已保存");
    emit("changed");
  } catch (e) {
    ElMessage.error(settingsApiError(e).message);
  } finally {
    saving.value = false;
  }
}

async function create() {
  const issue = categoryIssue(fresh, true, props.categories);
  if (issue) {
    ElMessage.warning(issue);
    return;
  }
  saving.value = true;
  try {
    await api("/admin/marketing/categories", { method: "POST", body: { kind: fresh.kind, code: fresh.code.trim(), name: fresh.name.trim(), sort: fresh.sort, isEnabled: true } });
    ElMessage.success("分类已创建");
    emit("changed");
  } catch (e) {
    ElMessage.error(settingsApiError(e).message);
  } finally {
    saving.value = false;
  }
}
</script>

<template>
  <ElDrawer :model-value="open" title="分类管理" size="min(640px, 100vw)" @update:model-value="emit('update:open', $event)">
    <ElTabs v-model="tab">
      <ElTabPane v-for="k in MARKETING_KINDS" :key="k.value" :name="k.value" :label="k.label" />
    </ElTabs>
    <ElTable :data="shown" row-key="id" empty-text="这个类型还没有分类">
      <ElTableColumn label="code" width="140">
        <template #default="{ row }"><code>{{ row.code }}</code></template>
      </ElTableColumn>
      <ElTableColumn label="名称" min-width="140">
        <template #default="{ row }"><ElInput v-model="row.name" size="small" :maxlength="24" /></template>
      </ElTableColumn>
      <ElTableColumn label="排序" width="120">
        <template #default="{ row }"><ElInputNumber v-model="row.sort" size="small" :min="0" :max="100000" :precision="0" controls-position="right" /></template>
      </ElTableColumn>
      <ElTableColumn label="启用" width="70">
        <template #default="{ row }"><ElSwitch v-model="row.isEnabled" size="small" /></template>
      </ElTableColumn>
      <ElTableColumn label="" width="70">
        <template #default="{ row }"><ElButton link type="primary" :loading="saving" @click="saveRow(row)">保存</ElButton></template>
      </ElTableColumn>
    </ElTable>
    <div class="cat-new">
      <strong>新建分类</strong>
      <ElInput v-model="fresh.code" size="small" placeholder="code，如 opening" :maxlength="48" />
      <ElInput v-model="fresh.name" size="small" placeholder="名称" :maxlength="24" />
      <ElInputNumber v-model="fresh.sort" size="small" :min="0" :max="100000" :precision="0" controls-position="right" />
      <ElButton size="small" type="primary" :loading="saving" @click="create">新建</ElButton>
    </div>
    <p class="cat-note">分类没有删除：选项按分类 code 引用它，下线用停用。code 建好后不能改。</p>
  </ElDrawer>
</template>

<style scoped>
.cat-new { display: grid; grid-template-columns: max-content 1fr 1fr 120px max-content; align-items: center; gap: 8px; margin-top: 16px; }
.cat-note { margin-top: 12px; color: var(--portal-muted); font-size: 12px; }
</style>
