<script setup lang="ts">
/**
 * 预置营销角色的新建与编辑（TT-T-21；字段照来源系统管理端 `MarketingCharacterEditor.vue`）。
 *
 * ⚠️ 营销页「使用预置角色」会把它复制（adopt）进工作区：改这里的图只影响之后 adopt 的副本，已有副本不变。
 * ⚠️ 参考音频只存不发：任何上游请求都不带它（发给收音频参考的视频模型就成了声音克隆）。
 * ⚠️ 没有删除，下线用停用。第一张图是主图。
 */
import { ElAlert, ElButton, ElDrawer, ElForm, ElFormItem, ElInput, ElMessage } from "element-plus";
import MediaPickField from "~/components/settings/MediaPickField.vue";
import {
  CHARACTER_NAME_FORBIDDEN, MARKETING_MAX_APPEARANCE, MARKETING_MAX_IMAGES, MARKETING_MAX_SUMMARY, MARKETING_MAX_VOICE_TEXT,
  characterDraft, characterIssue, characterPayload, marketingIssuesOf, type CharacterDraft
} from "~/utils/marketingForm";
import { settingsApiError } from "~/utils/settingsApi";

type Row = Record<string, unknown>;

const props = defineProps<{ open: boolean; characterId: string | null }>();
const emit = defineEmits<{ (e: "update:open", v: boolean): void; (e: "changed"): void }>();

const api = useApi();
const draft = reactive<CharacterDraft>(characterDraft(null));
const saving = ref(false);
const conflict = ref(false);
const issues = ref<string[]>([]);
const warnings = ref<string[]>([]);
const currentId = ref<string | null>(null);
const creating = computed(() => !currentId.value);
const voiceIds = computed({
  get: () => (draft.voiceMediaId ? [draft.voiceMediaId] : []),
  set: (v: string[]) => { draft.voiceMediaId = v[0] ?? ""; }
});

function absorb(row: Row) {
  Object.assign(draft, characterDraft(row));
  warnings.value = marketingIssuesOf({ issues: row.warnings });
}

async function load(id: string | null) {
  conflict.value = false;
  issues.value = [];
  warnings.value = [];
  currentId.value = id;
  if (!id) {
    Object.assign(draft, characterDraft(null));
    return;
  }
  try {
    absorb(await api<Row>(`/admin/marketing/preset-characters/${id}`));
  } catch (e) {
    ElMessage.error(settingsApiError(e).message);
  }
}

watch(() => props.open, open => {
  if (open) void load(props.characterId);
});

async function save() {
  const issue = characterIssue(draft);
  if (issue) {
    ElMessage.warning(issue);
    return;
  }
  saving.value = true;
  issues.value = [];
  const wasCreating = creating.value;
  try {
    const row = wasCreating
      ? await api<Row>("/admin/marketing/preset-characters", { method: "POST", body: characterPayload(draft, "create") })
      : await api<Row>(`/admin/marketing/preset-characters/${currentId.value}`, { method: "PUT", body: characterPayload(draft, "update") });
    currentId.value = String(row.id);
    absorb(row);
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
  <ElDrawer :model-value="open" :title="creating ? '新建营销角色' : `${draft.name} · v${draft.version}`" size="min(720px, 100vw)" @update:model-value="emit('update:open', $event)">
    <ElAlert v-if="conflict" type="warning" show-icon :closable="false" class="chr-alert" title="这个角色刚被修改过，你的保存没有生效（不会静默覆盖）。">
      <ElButton size="small" @click="load(currentId)">重新加载</ElButton>
    </ElAlert>
    <ElAlert v-if="issues.length" type="error" show-icon :closable="false" class="chr-alert" title="保存被拦下：">
      <ul class="chr-list"><li v-for="(x, i) in issues" :key="i">{{ x }}</li></ul>
    </ElAlert>
    <ElAlert v-if="warnings.length" type="warning" show-icon :closable="false" class="chr-alert" title="提示（不拦截保存，启用前要处理）：">
      <ul class="chr-list"><li v-for="(x, i) in warnings" :key="i">{{ x }}</li></ul>
    </ElAlert>
    <ElForm label-position="top">
      <ElFormItem label="名称" required>
        <ElInput v-model="draft.name" :maxlength="50" />
        <small class="chr-help">1~50 字，不能包含 {{ CHARACTER_NAME_FORBIDDEN }}；全局唯一（不区分大小写）。添加到角色库时名称原样复制。</small>
      </ElFormItem>
      <ElFormItem :label="`简介（${[...draft.summary].length} / ${MARKETING_MAX_SUMMARY}）`"><ElInput v-model="draft.summary" /></ElFormItem>
      <ElFormItem :label="`图片（最多 ${MARKETING_MAX_IMAGES} 张，第一张是主图）`">
        <MediaPickField v-model:ids="draft.imageMediaIds" :max="MARKETING_MAX_IMAGES" kind="image" purpose="avatar" />
      </ElFormItem>
      <ElFormItem :label="`外观描述（${[...draft.appearance].length} / ${MARKETING_MAX_APPEARANCE}）`">
        <ElInput v-model="draft.appearance" type="textarea" :autosize="{ minRows: 3, maxRows: 10 }" />
        <small class="chr-help">只写去掉临时状态后仍成立的稳定特征（脸型、发型、体态），当下的服装与表情不写。</small>
      </ElFormItem>
      <ElFormItem :label="`声音描述（${[...draft.voiceText].length} / ${MARKETING_MAX_VOICE_TEXT}）`">
        <ElInput v-model="draft.voiceText" type="textarea" :autosize="{ minRows: 2, maxRows: 6 }" />
      </ElFormItem>
      <ElFormItem label="参考音频（可选）">
        <MediaPickField v-model:ids="voiceIds" :max="1" kind="audio" purpose="avatar" hint="只存不发：不会发送给任何模型" />
      </ElFormItem>
    </ElForm>
    <template #footer>
      <div class="chr-actions">
        <ElButton @click="emit('update:open', false)">关闭</ElButton>
        <ElButton type="primary" :loading="saving" @click="save">{{ creating ? "创建（停用状态）" : "保存" }}</ElButton>
      </div>
    </template>
  </ElDrawer>
</template>

<style scoped>
.chr-alert { margin-bottom: 12px; }
.chr-help { display: block; width: 100%; margin-top: 4px; color: var(--portal-faint); font-size: 12px; line-height: 1.5; }
.chr-list { margin: 4px 0 0; padding-left: 18px; }
.chr-actions { display: flex; justify-content: flex-end; gap: 8px; }
</style>
