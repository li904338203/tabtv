<script setup lang="ts">
/**
 * 营销选项（创意 / Hook / 风格）的新建与编辑（TT-T-21；字段照来源系统管理端 `MarketingOptionEditor.vue`）。
 *
 * ⚠️ 保存带 version 乐观锁，冲突不静默覆盖。新建一律停用，上线在列表上单独启用（启用时服务端完整校验）。
 * ⚠️ 提示词片段只在服务端拼接，用户端只看到名称、说明与封面。
 * ⚠️ 已建的营销会话按快照冻结，改这里不影响它们；新会话才用新内容。
 */
import {
  ElAlert, ElButton, ElCheckbox, ElCheckboxGroup, ElCollapse, ElCollapseItem, ElDatePicker, ElDrawer, ElForm, ElFormItem,
  ElInput, ElInputNumber, ElLoading, ElMessage, ElOption, ElRadioButton, ElRadioGroup, ElSelect, ElSwitch
} from "element-plus";
import MediaPickField from "~/components/settings/MediaPickField.vue";
import {
  MARKETING_MAX_DESCRIPTION, MARKETING_MAX_NAME, MARKETING_MAX_PROMPT, MARKETING_OUTPUT_TYPES, MARKETING_SLOTS,
  marketingCoverLimit, marketingCreativeOnly, marketingIssuesOf, marketingKindLabel, marketingOptionHasAudio,
  optionDraft, optionIssue, optionNotes, optionPayload, type OptionDraft, type SettingsMarketingKind
} from "~/utils/marketingForm";
import { settingsApiError } from "~/utils/settingsApi";

const vLoading = ElLoading.directive;

type Row = Record<string, unknown>;
type Version = { id: string; version: number; createdAt: string; snapshot: Row };

const props = defineProps<{
  open: boolean;
  optionId: string | null;
  kind: SettingsMarketingKind;
  categories: Row[];
  models: Array<{ code: string; name: string; domain: string; status: string }>;
  skills: Array<{ code: string; name: string; isEnabled: boolean; scenes: string[] }>;
  nextSort: number;
}>();
const emit = defineEmits<{ (e: "update:open", v: boolean): void; (e: "changed"): void }>();

const api = useApi();
const draft = reactive<OptionDraft>(optionDraft(null));
const loading = ref(false);
const saving = ref(false);
const conflict = ref(false);
const issues = ref<string[]>([]);
const versions = ref<Version[]>([]);
const currentId = ref<string | null>(null);
const creating = computed(() => !currentId.value);
const notes = computed(() => optionNotes(draft));
const kindCategories = computed(() => props.categories.filter(c => c.kind === draft.kind));
const videoModels = computed(() => props.models.filter(m => m.domain === "video" || m.domain === "image"));
const marketingSkills = computed(() => props.skills.filter(s => s.scenes.includes("marketing")));

async function load(id: string | null) {
  conflict.value = false;
  issues.value = [];
  versions.value = [];
  currentId.value = id;
  if (!id) {
    Object.assign(draft, optionDraft(null, props.kind, props.nextSort));
    return;
  }
  loading.value = true;
  try {
    const row = await api<Row>(`/admin/marketing/options/${id}`);
    Object.assign(draft, optionDraft(row));
    versions.value = (await api<{ items: Version[] }>(`/admin/marketing/options/${id}/versions`)).items ?? [];
  } catch (e) {
    ElMessage.error(settingsApiError(e).message);
  } finally {
    loading.value = false;
  }
}

watch(() => props.open, open => {
  if (open) void load(props.optionId);
});

const requirement = (key: string) => draft.inputRequirements[key] ?? "";
function setRequirement(key: string, v: string) {
  // 「不需要」= 去掉这个键（不认识的键原样保留）
  const next = Object.fromEntries(Object.entries(draft.inputRequirements).filter(([k]) => k !== key));
  if (v) next[key] = v;
  draft.inputRequirements = next;
}

async function save() {
  const issue = optionIssue(draft, creating.value);
  if (issue) {
    ElMessage.warning(issue);
    return;
  }
  saving.value = true;
  issues.value = [];
  const wasCreating = creating.value;
  try {
    const row = wasCreating
      ? await api<Row>("/admin/marketing/options", { method: "POST", body: optionPayload(draft, "create") })
      : await api<Row>(`/admin/marketing/options/${currentId.value}`, { method: "PUT", body: optionPayload(draft, "update") });
    currentId.value = String(row.id);
    Object.assign(draft, optionDraft(row));
    ElMessage.success(wasCreating ? "已创建（停用状态）" : `已保存为 v${draft.version}`);
    emit("changed");
  } catch (e) {
    const err = settingsApiError(e);
    if (err.code === 40902) conflict.value = true;
    else {
      issues.value = marketingIssuesOf(err.details);
      ElMessage.error({ message: err.message, duration: 8000 });
    }
  } finally {
    saving.value = false;
  }
}
</script>

<template>
  <ElDrawer :model-value="open" :title="creating ? `新建${marketingKindLabel(draft.kind)}` : `${draft.name} · v${draft.version}`" size="min(820px, 100vw)" @update:model-value="emit('update:open', $event)">
    <ElAlert v-if="conflict" type="warning" show-icon :closable="false" class="mkt-alert" title="这一条刚被修改过，你的保存没有生效（不会静默覆盖）。">
      <ElButton size="small" @click="load(currentId)">重新加载</ElButton>
    </ElAlert>
    <ElAlert v-if="issues.length" type="error" show-icon :closable="false" class="mkt-alert" title="保存被拦下：">
      <ul class="mkt-list"><li v-for="(x, i) in issues" :key="i">{{ x }}</li></ul>
    </ElAlert>
    <ElAlert v-for="n in notes" :key="n" type="warning" show-icon :closable="false" class="mkt-alert" :title="n" />
    <ElForm v-loading="loading" label-position="top">
      <div class="form-grid">
        <ElFormItem label="code" required>
          <ElInput v-model="draft.code" :disabled="!creating" :maxlength="64" placeholder="unbox_show" />
          <small class="mkt-help">建好后不能改（历史会话按 code + 版本引用）。</small>
        </ElFormItem>
        <ElFormItem :label="`名称（${[...draft.name].length} / ${MARKETING_MAX_NAME}）`" required><ElInput v-model="draft.name" /></ElFormItem>
        <ElFormItem label="分类" required>
          <ElSelect v-model="draft.categoryCode" placeholder="先在「分类管理」里建分类">
            <ElOption v-for="c in kindCategories" :key="String(c.code)" :label="c.isEnabled ? String(c.name) : `${c.name}（停用）`" :value="String(c.code)" />
          </ElSelect>
        </ElFormItem>
        <ElFormItem label="排序"><ElInputNumber v-model="draft.sort" :min="0" :max="100000" :precision="0" /></ElFormItem>
      </div>
      <ElFormItem :label="`说明（${[...draft.description].length} / ${MARKETING_MAX_DESCRIPTION}）`">
        <ElInput v-model="draft.description" type="textarea" :rows="2" />
        <small class="mkt-help">用户端选项卡片上的一句话。</small>
      </ElFormItem>
      <ElFormItem :label="`封面（${marketingKindLabel(draft.kind)} 1~${marketingCoverLimit(draft.kind)} 张，启用前必须有）`">
        <MediaPickField v-model:ids="draft.coverMediaIds" :max="marketingCoverLimit(draft.kind)" kind="image" />
      </ElFormItem>
      <ElFormItem label="预览视频（可选）">
        <MediaPickField :ids="draft.previewMediaId ? [draft.previewMediaId] : []" :max="1" kind="video" @update:ids="draft.previewMediaId = $event[0] ?? ''" />
      </ElFormItem>
      <ElFormItem :label="`提示词片段（${[...draft.prompt].length} / ${MARKETING_MAX_PROMPT}）`">
        <ElInput v-model="draft.prompt" type="textarea" :autosize="{ minRows: 4, maxRows: 14 }" class="mkt-mono" />
        <small class="mkt-help">只在服务端拼进生成请求，用户端看不到。</small>
      </ElFormItem>
      <div class="form-grid">
        <ElFormItem label="适用产出" required>
          <ElCheckboxGroup v-model="draft.outputTypes">
            <ElCheckbox v-for="o in MARKETING_OUTPUT_TYPES" :key="o.value" :value="o.value">{{ o.label }}</ElCheckbox>
          </ElCheckboxGroup>
        </ElFormItem>
        <ElFormItem v-if="marketingOptionHasAudio(draft.kind)" label="依赖出声"><ElSwitch v-model="draft.isAudioRequired" /></ElFormItem>
      </div>
      <ElFormItem label="输入要求">
        <div v-for="s in MARKETING_SLOTS" :key="s.key" class="mkt-slot">
          <span>{{ s.label }}</span>
          <ElRadioGroup :model-value="requirement(s.key)" size="small" @update:model-value="setRequirement(s.key, String($event))">
            <ElRadioButton value="">不需要</ElRadioButton>
            <ElRadioButton value="optional">可选</ElRadioButton>
            <ElRadioButton value="required">必需</ElRadioButton>
          </ElRadioGroup>
        </div>
      </ElFormItem>
      <template v-if="marketingCreativeOnly(draft.kind)">
        <ElFormItem label="推荐模型">
          <ElSelect v-model="draft.recommendedModels" multiple filterable>
            <ElOption v-for="m in videoModels" :key="m.code" :label="`${m.name || m.code}${m.status === 'enabled' ? '' : '（已停用）'}`" :value="m.code" />
          </ElSelect>
        </ElFormItem>
        <div class="form-grid three">
          <ElFormItem label="推荐画幅">
            <ElSelect v-model="draft.aspectRatio" clearable placeholder="不推荐">
              <ElOption v-for="a in ['9:16', '16:9', '1:1', '3:4', '4:3']" :key="a" :label="a" :value="a" />
            </ElSelect>
          </ElFormItem>
          <ElFormItem label="推荐时长下限（秒）"><ElInputNumber v-model="draft.durationMin" :min="1" :max="600" :precision="0" /></ElFormItem>
          <ElFormItem label="推荐时长上限（秒）"><ElInputNumber v-model="draft.durationMax" :min="1" :max="600" :precision="0" /></ElFormItem>
        </div>
        <ElFormItem label="默认技能">
          <ElSelect v-model="draft.defaultSkillCode" clearable filterable placeholder="不指定">
            <ElOption v-for="s in marketingSkills" :key="s.code" :label="`${s.name}${s.isEnabled ? '' : '（停用）'}`" :value="s.code" />
          </ElSelect>
        </ElFormItem>
      </template>
      <ElFormItem label="标签">
        <ElSelect v-model="draft.tags" multiple filterable allow-create default-first-option placeholder="回车添加；「口播」会触发文案检查" />
      </ElFormItem>
      <div class="form-grid">
        <ElFormItem label="投放开始（空 = 不限）">
          <ElDatePicker v-model="draft.startAt" type="datetime" value-format="YYYY-MM-DDTHH:mm:ssZ" clearable />
        </ElFormItem>
        <ElFormItem label="投放结束（空 = 不限）">
          <ElDatePicker v-model="draft.endAt" type="datetime" value-format="YYYY-MM-DDTHH:mm:ssZ" clearable />
        </ElFormItem>
      </div>
    </ElForm>
    <ElCollapse v-if="versions.length">
      <ElCollapseItem :title="`版本历史（${versions.length}）`" name="v">
        <div v-for="v in versions" :key="v.id" class="mkt-version">
          <strong>v{{ v.version }}</strong> <span class="mkt-help-inline">{{ v.createdAt }}</span>
          <div class="mkt-help-inline">{{ String(v.snapshot?.name ?? "") }} · {{ String(v.snapshot?.description ?? "") }}</div>
        </div>
      </ElCollapseItem>
    </ElCollapse>
    <template #footer>
      <div class="mkt-actions">
        <ElButton @click="emit('update:open', false)">关闭</ElButton>
        <ElButton type="primary" :loading="saving" @click="save">{{ creating ? "创建（停用状态）" : "保存" }}</ElButton>
      </div>
    </template>
  </ElDrawer>
</template>

<style scoped>
.mkt-alert { margin-bottom: 12px; }
.form-grid { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 0 14px; }
.form-grid.three { grid-template-columns: repeat(3, minmax(0, 1fr)); }
.mkt-help { display: block; width: 100%; margin-top: 4px; color: var(--portal-faint); font-size: 12px; }
.mkt-help-inline { color: var(--portal-faint); font-size: 12px; }
.mkt-mono { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; font-size: 13px; }
.mkt-slot { display: flex; align-items: center; gap: 12px; width: 100%; margin-bottom: 6px; }
.mkt-slot > span { width: 56px; color: var(--portal-text-soft); }
.mkt-list { margin: 4px 0 0; padding-left: 18px; }
.mkt-version { padding: 6px 0; border-bottom: 1px solid var(--portal-line); }
.mkt-actions { display: flex; justify-content: flex-end; gap: 8px; }
</style>
