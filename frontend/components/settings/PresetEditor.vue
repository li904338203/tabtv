<script setup lang="ts">
/**
 * 图片预设的新建与编辑（TT-T-20）。来源系统管理端对预设只读，这里是新增的写表单；规则见 `utils/presetForm.ts`。
 *
 * ⚠️ 配方提示词只在服务端拼进生成请求，画布拿到的配置里没有它；这里能看能改。
 * ⚠️ 预览走服务端：没选的维度按 default 出词，这条规则只有一份。
 * ⚠️ 编码建好后不能改（历史节点按 code 找配方）；改内置预设会标「已修改」，启动时不再被内置内容覆盖。
 */
import {
  ElAlert, ElButton, ElDrawer, ElForm, ElFormItem, ElInput, ElInputNumber, ElMessage, ElMessageBox, ElOption, ElSelect,
  ElSwitch, ElTag
} from "element-plus";
import { builtinBadge } from "~/utils/builtinStatus";
import {
  PRESET_GROUPS, PRESET_INPUT_MODES, PRESET_SPECS, parsePresetDimensions, presetDraft, presetPayload, validatePresetDraft,
  type PresetDraft
} from "~/utils/presetForm";
import { settingsApiError } from "~/utils/settingsApi";

type Row = Record<string, unknown>;
type Operation = { code: string; name: string; domain: string };

const props = defineProps<{ open: boolean; row: Row | null; list: Row[]; operations: Operation[] }>();
const emit = defineEmits<{ (e: "update:open", v: boolean): void; (e: "changed"): void }>();

const api = useApi();
const draft = reactive<PresetDraft>(presetDraft(null));
const saving = ref(false);
const creating = ref(true);
const preview = reactive({ text: "", loading: false });
const badge = computed(() => builtinBadge(draft));
// 图片类操作排前面；其余照样列出（现有预设绑的操作不能因为分类不同就在下拉里消失）
const imageOps = computed(() => [...props.operations].sort((a, b) => Number(b.domain === "image") - Number(a.domain === "image")));
const dimensions = computed(() => parsePresetDimensions(draft.dimensionsText).value ?? []);
const dimsError = computed(() => parsePresetDimensions(draft.dimensionsText).error);

watch(() => props.open, open => {
  if (!open) return;
  creating.value = !props.row;
  const next = props.list.reduce((m, r) => Math.max(m, typeof r.sort === "number" ? r.sort : 0), 0) + 10;
  Object.assign(draft, presetDraft(props.row, next));
  preview.text = "";
});

async function save() {
  const issue = validatePresetDraft(draft, { creating: creating.value, list: props.list, operations: props.operations.map(o => o.code) });
  if (issue) {
    ElMessage.warning(issue);
    return;
  }
  saving.value = true;
  try {
    const body = presetPayload(draft);
    const row = creating.value
      ? await api<Row>("/admin/image-presets", { method: "POST", body })
      : await api<Row>(`/admin/image-presets/${encodeURIComponent(draft.code)}`, { method: "PUT", body });
    Object.assign(draft, presetDraft(row));
    creating.value = false;
    ElMessage.success("已保存：画布下次拉取配置时生效");
    emit("changed");
  } catch (e) {
    ElMessage.error({ message: settingsApiError(e).message, duration: 8000 });
  } finally {
    saving.value = false;
  }
}

async function restore() {
  try {
    await ElMessageBox.confirm("名称、分组、配方、维度、参数默认等全部回到内置那份；启用状态不变。", "恢复内置版本",
      { confirmButtonText: "恢复", cancelButtonText: "取消", type: "warning" });
  } catch {
    return;
  }
  saving.value = true;
  try {
    const row = await api<Row>(`/admin/image-presets/${encodeURIComponent(draft.code)}/restore-builtin`, { method: "POST" });
    Object.assign(draft, presetDraft(row));
    ElMessage.success("已恢复内置版本");
    emit("changed");
  } catch (e) {
    ElMessage.error(settingsApiError(e).message);
  } finally {
    saving.value = false;
  }
}

async function runPreview() {
  if (dimsError.value) {
    ElMessage.warning(dimsError.value);
    return;
  }
  preview.loading = true;
  try {
    const r = await api<{ rendered: string }>("/admin/image-presets/preview", {
      method: "POST", body: { prompt: draft.prompt, dimensions: dimensions.value }
    });
    preview.text = r.rendered;
  } catch (e) {
    ElMessage.error(settingsApiError(e).message);
  } finally {
    preview.loading = false;
  }
}
</script>

<template>
  <ElDrawer :model-value="open" :title="creating ? '新建图片预设' : draft.name || draft.code" size="min(760px, 100vw)" @update:model-value="emit('update:open', $event)">
    <div v-if="!creating" class="preset-meta">
      <code>{{ draft.code }}</code>
      <ElTag size="small" :type="draft.isEnabled ? 'success' : 'info'">{{ draft.isEnabled ? "启用" : "停用" }}</ElTag>
      <ElTag size="small" :type="badge.type" effect="plain">{{ badge.label }}</ElTag>
      <ElButton v-if="draft.isBuiltin" size="small" :loading="saving" @click="restore">恢复内置版本</ElButton>
    </div>
    <ElForm label-position="top">
      <div class="form-grid">
        <ElFormItem label="编码" required>
          <ElInput v-model="draft.code" :disabled="!creating" :maxlength="64" placeholder="my_film_tone" />
          <small class="preset-help">历史节点按它找配方，建好后不能改。</small>
        </ElFormItem>
        <ElFormItem label="名称" required><ElInput v-model="draft.name" :maxlength="64" /></ElFormItem>
        <ElFormItem label="分组" required>
          <ElSelect v-model="draft.presetGroup"><ElOption v-for="g in PRESET_GROUPS" :key="g.value" :label="g.label" :value="g.value" /></ElSelect>
        </ElFormItem>
        <ElFormItem label="产出类型" required>
          <ElSelect v-model="draft.spec"><ElOption v-for="g in PRESET_SPECS" :key="g.value" :label="`${g.label}（${g.value}）`" :value="g.value" /></ElSelect>
          <small class="preset-help">同时是产出物在素材库里的分类标签。</small>
        </ElFormItem>
        <ElFormItem label="输入要求" required>
          <ElSelect v-model="draft.inputMode"><ElOption v-for="g in PRESET_INPUT_MODES" :key="g.value" :label="g.label" :value="g.value" /></ElSelect>
          <small class="preset-help">在扣费之前校验：不满足就拒绝，不会白花钱。</small>
        </ElFormItem>
        <ElFormItem label="操作">
          <ElSelect v-model="draft.operation" clearable filterable placeholder="不指定">
            <ElOption v-for="o in imageOps" :key="o.code" :label="`${o.name}（${o.code}）`" :value="o.code" />
          </ElSelect>
        </ElFormItem>
        <ElFormItem label="图标"><ElInput v-model="draft.icon" placeholder="图标名（可选）" /></ElFormItem>
        <ElFormItem label="排序"><ElInputNumber v-model="draft.sort" :min="0" :max="100000" :precision="0" /></ElFormItem>
      </div>
      <ElFormItem label="说明"><ElInput v-model="draft.description" type="textarea" :rows="2" show-word-limit :maxlength="300" /></ElFormItem>
      <ElFormItem label="NEW 角标"><ElSwitch v-model="draft.isNew" /></ElFormItem>
      <ElFormItem label="配方提示词" required>
        <ElInput v-model="draft.prompt" type="textarea" :autosize="{ minRows: 6, maxRows: 20 }" class="preset-mono" />
        <small class="preset-help">拼在用户描述之后、风格素材词之前。只在服务端使用，画布上看不到。</small>
      </ElFormItem>
      <ElFormItem label="调节维度（JSON 数组，可选）" :error="dimsError">
        <ElInput
v-model="draft.dimensionsText" type="textarea" :autosize="{ minRows: 4, maxRows: 18 }" class="preset-mono"
          placeholder='[{"code":"strength","name":"强度","default":"medium","options":[{"value":"medium","label":"中","prompt":"……"}]}]' />
        <small class="preset-help">用户没打开过调节面板时按每维的 default 出词，所以 default 必须是其中一个选项。</small>
      </ElFormItem>
      <ElFormItem label="参数默认（JSON 对象）">
        <ElInput v-model="draft.paramsText" type="textarea" :autosize="{ minRows: 2, maxRows: 10 }" class="preset-mono" />
        <small class="preset-help">只填用户没设过的参数（他调好的画幅不会被改回去）。通常含 presetCode。</small>
      </ElFormItem>
    </ElForm>
    <div class="preset-preview">
      <ElButton size="small" :loading="preview.loading" @click="runPreview">预览默认出词</ElButton>
      <ElAlert v-if="preview.text" type="info" :closable="false" class="preset-alert">
        <pre class="preset-rendered">{{ preview.text }}</pre>
      </ElAlert>
    </div>
    <template #footer>
      <div class="preset-actions">
        <ElButton @click="emit('update:open', false)">关闭</ElButton>
        <ElButton type="primary" :loading="saving" @click="save">{{ creating ? "创建" : "保存" }}</ElButton>
      </div>
    </template>
  </ElDrawer>
</template>

<style scoped>
.preset-meta { display: flex; flex-wrap: wrap; align-items: center; gap: 8px 12px; margin-bottom: 8px; font-size: 12px; }
.form-grid { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 0 14px; }
.preset-help { display: block; width: 100%; margin-top: 4px; color: var(--portal-faint); font-size: 12px; line-height: 1.5; }
.preset-mono { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; font-size: 13px; }
.preset-preview { display: flex; flex-direction: column; align-items: flex-start; gap: 8px; }
.preset-alert { width: 100%; }
.preset-rendered { margin: 0; font-size: 12px; white-space: pre-wrap; }
.preset-actions { display: flex; justify-content: flex-end; gap: 8px; }
</style>
