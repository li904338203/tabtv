<script setup lang="ts">
/**
 * 模型新建 / 编辑（TT-T-18 高级模式；字段与校验照来源管理端 `ModelEditor.vue`，去掉会员门槛）。
 *
 * ⚠️ 保存是整条替换：表单必须原样带回全部字段（提交 / 查询路径、输入上限、排序），漏一个就被清空。
 * ⚠️ 启用要过发布检查（服务端逐条给阻塞原因）；有历史任务后厂商 / 协议 / 类型 / 上游模型名不可再改。
 */
import {
  ElAlert, ElButton, ElCheckbox, ElDrawer, ElForm, ElFormItem, ElInput, ElInputNumber, ElMessage, ElOption, ElSelect, ElSwitch
} from "element-plus";
import { settingsApiError } from "~/utils/settingsApi";
import {
  SETTINGS_MAX_VIDEO_DURATION_SEC, SETTINGS_MODEL_DOMAINS, settingsAgentCapableAllowed, settingsModelDraft, settingsModelPayload,
  settingsValidateModel, type SettingsModelDraft, type SettingsProtocol
} from "~/utils/settingsModelForm";

type Row = Record<string, unknown>;
type Blocker = { code: string; detail: string };

const props = defineProps<{ open: boolean; row: Row | null; models: Row[]; providers: Row[]; protocols: SettingsProtocol[] }>();
const emit = defineEmits<{ (e: "update:open", v: boolean): void; (e: "saved"): void }>();

const api = useApi();
const draft = reactive<SettingsModelDraft>(settingsModelDraft(null));
const saving = ref(false);
const editing = computed(() => props.row !== null);
const blockers = ref<Blocker[]>([]);
const protocol = computed(() => props.protocols.find(p => p.code === draft.apiProtocol));
const agentAllowed = computed(() => settingsAgentCapableAllowed(draft, props.protocols));

watch(() => props.open, async open => {
  if (!open) return;
  Object.assign(draft, settingsModelDraft(props.row));
  blockers.value = [];
  if (props.row) {
    try {
      const r = await api<{ blockers: Blocker[] }>(`/admin/models/${encodeURIComponent(String(props.row.code))}/publishable`);
      blockers.value = r.blockers ?? [];
    } catch {
      blockers.value = [];
    }
  }
});

async function save() {
  const err = settingsValidateModel(draft, props.models, props.protocols, editing.value);
  if (err) {
    ElMessage.warning(err);
    return;
  }
  saving.value = true;
  try {
    await api("/admin/models/", { method: "PUT", body: settingsModelPayload(draft) });
    ElMessage.success("模型已保存");
    emit("update:open", false);
    emit("saved");
  } catch (e) {
    const err = settingsApiError(e);
    const list = err.details?.blockers;
    if (Array.isArray(list)) blockers.value = list as Blocker[];
    ElMessage.error({ message: err.message, duration: 8000 });
  } finally {
    saving.value = false;
  }
}
</script>

<template>
  <ElDrawer :model-value="open" :title="editing ? '编辑模型' : '新建模型'" size="min(700px, 100vw)" @update:model-value="emit('update:open', $event)">
    <ElAlert v-if="blockers.length" type="warning" :closable="false" class="me-alert" title="现在还不能启用">
      <ul class="me-blockers"><li v-for="b in blockers" :key="b.code + b.detail">{{ b.detail }}</li></ul>
    </ElAlert>
    <ElForm label-position="top">
      <div class="form-grid">
        <ElFormItem label="模型编码" required>
          <ElInput v-model="draft.code" :disabled="editing" :maxlength="64" placeholder="my_image_v1" />
          <small class="me-help">{{ editing ? "编码是业务键，保存后不能改。" : "小写字母、数字、下划线与连字符。" }}</small>
        </ElFormItem>
        <ElFormItem label="名称" required><ElInput v-model="draft.name" :maxlength="64" /></ElFormItem>
        <ElFormItem label="厂商" required>
          <ElSelect v-model="draft.providerCode" filterable placeholder="选择厂商">
            <ElOption v-for="p in providers" :key="String(p.code)" :value="String(p.code)" :label="`${p.name}（${p.code}）${p.isReady ? '' : ' · 未就绪'}`" />
          </ElSelect>
        </ElFormItem>
        <ElFormItem label="模型类型" required>
          <ElSelect v-model="draft.domain"><ElOption v-for="d in SETTINGS_MODEL_DOMAINS" :key="d.value" :value="d.value" :label="d.label" /></ElSelect>
        </ElFormItem>
        <ElFormItem v-if="draft.domain === 'audio'" label="音频子类型">
          <ElSelect v-model="draft.subType" clearable>
            <ElOption value="tts" label="文本转语音" />
            <ElOption value="music" label="音乐生成" />
          </ElSelect>
        </ElFormItem>
        <ElFormItem label="上游模型名" required><ElInput v-model="draft.upstreamModelCode" :maxlength="128" placeholder="上游接口里的 model 字段" /></ElFormItem>
        <ElFormItem label="API 协议" required>
          <ElSelect v-model="draft.apiProtocol" filterable placeholder="选择协议">
            <ElOption v-for="p in protocols" :key="p.code" :value="p.code" :label="`${p.name}（${p.code}）${p.isAsync ? ' · 异步' : ''}`" />
          </ElSelect>
          <small class="me-help">「通用」协议发的是本应用自有的请求信封，只适用于兼容它的中转；接厂商原生接口请选对应的协议。</small>
        </ElFormItem>
        <ElFormItem label="提交路径" required><ElInput v-model="draft.submitPath" :maxlength="256" placeholder="/images/generations" /></ElFormItem>
        <ElFormItem v-if="protocol?.isAsync" label="查询路径" :required="protocol?.requiresQueryPath">
          <ElInput v-model="draft.queryPath" :maxlength="256" placeholder="/videos/{taskId}" />
        </ElFormItem>
        <ElFormItem label="预计耗时（秒）" required>
          <ElInputNumber v-model="draft.estSeconds" :min="1" :max="3600" :precision="0" />
          <small class="me-help">决定超时：max(30, 预计耗时 × 3)。</small>
        </ElFormItem>
        <ElFormItem label="输入字数上限">
          <ElInputNumber v-model="draft.maxInputChars" :min="1" :precision="0" placeholder="不限" />
        </ElFormItem>
        <ElFormItem label="排序"><ElInputNumber v-model="draft.sort" :precision="0" /></ElFormItem>
      </div>
      <template v-if="draft.domain === 'video'">
        <div class="form-grid">
          <ElFormItem label="单次最长时长（秒）">
            <ElInputNumber v-model="draft.maxDurationSec" :min="1" :max="SETTINGS_MAX_VIDEO_DURATION_SEC" :precision="0" placeholder="未配置" />
          </ElFormItem>
          <ElFormItem label="视频能力">
            <ElCheckbox v-model="draft.isMultiShot">支持多镜头</ElCheckbox>
            <ElCheckbox v-model="draft.hasNativeAudio">自带音轨</ElCheckbox>
          </ElFormItem>
        </div>
      </template>
      <ElFormItem v-if="draft.domain === 'text'" label="支持 Agent">
        <ElCheckbox v-model="draft.agentCapable" :disabled="!agentAllowed && !draft.agentCapable">可作为 Agent 的推理模型</ElCheckbox>
        <small class="me-help">还要绑定并启用「agent_chat」操作，Agent 的模型下拉才会列出它。{{ agentAllowed ? "" : "当前协议不支持多轮对话与工具调用。" }}</small>
      </ElFormItem>
      <ElFormItem label="启用">
        <ElSwitch :model-value="draft.status === 'enabled'" @update:model-value="(v: string | number | boolean) => (draft.status = v ? 'enabled' : 'disabled')" />
        <small class="me-help">启用前会检查：厂商已就绪、至少绑定并启用一个操作、每个操作都有能力配置。</small>
      </ElFormItem>
    </ElForm>
    <template #footer>
      <div class="me-actions">
        <ElButton @click="emit('update:open', false)">取消</ElButton>
        <ElButton type="primary" :loading="saving" @click="save">保存</ElButton>
      </div>
    </template>
  </ElDrawer>
</template>

<style scoped>
.form-grid { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 0 14px; }
.form-grid :deep(.el-input-number), .form-grid :deep(.el-select) { width: 100%; }
.me-help { display: block; width: 100%; margin-top: 4px; color: var(--portal-faint); font-size: 12px; line-height: 1.5; }
.me-alert { margin-bottom: 12px; }
.me-blockers { margin: 4px 0 0; padding-left: 18px; }
.me-actions { display: flex; justify-content: flex-end; gap: 8px; }
</style>
