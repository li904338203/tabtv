<script setup lang="ts">
/**
 * 模型的操作绑定、能力矩阵、参数支持与发布检查（TT-T-18 高级模式；照来源管理端 `ModelBindingsDrawer.vue`，参数支持是新增）。
 *
 * ⚠️ 能力矩阵是**生成之前**拒掉上游做不了的输入组合的依据：没配的话，连了一张上游不收的图，
 *    要到执行时构造请求才失败。
 * ⚠️ 参数支持 = 这个型号能选哪些时长 / 清晰度 / 画幅（画布参数合并的第三层）。默认值必须在可选项里。
 */
import {
  ElButton, ElDialog, ElDrawer, ElForm, ElFormItem, ElInput, ElInputNumber, ElLoading, ElMessage, ElMessageBox, ElOption,
  ElSelect, ElSwitch, ElTable, ElTableColumn, ElTag
} from "element-plus";
import { settingsApiError } from "~/utils/settingsApi";
import {
  SETTINGS_INPUT_ROLES, settingsCapabilityPayload, settingsParamSupportDraft, settingsParamSupportPayload, settingsValidateCapability,
  settingsValidateParamSupport, type SettingsCapabilityDraft, type SettingsParamSupportDraft
} from "~/utils/settingsModelForm";

type Row = Record<string, unknown>;
type Cap = { role: string; minCount: number; maxCount: number; allowedMime?: string[] };
type Binding = { operation: string; enabled: boolean; estSeconds?: number | null };
type Blocker = { code: string; detail: string };
type ParamDef = { code: string; name: string; controlType: string };

const props = defineProps<{ open: boolean; model: Row | null; operations: Row[] }>();
const emit = defineEmits<{ (e: "update:open", v: boolean): void; (e: "changed"): void }>();
const vLoading = ElLoading.directive;

const api = useApi();
const code = computed(() => String(props.model?.code ?? ""));
const bindings = ref<Binding[]>([]);
const caps = ref<Record<string, Cap[]>>({});
const params = ref<Row[]>([]);
const paramDefs = ref<ParamDef[]>([]);
const blockers = ref<Blocker[]>([]);
const status = ref("");
const loading = ref(false);
const roleLabel = (r: string) => SETTINGS_INPUT_ROLES.find(x => x.value === r)?.label ?? r;
const opName = (c: string) => String(props.operations.find(o => o.code === c)?.name ?? c);
const path = (suffix: string) => `/admin/models/${encodeURIComponent(code.value)}${suffix}`;

const recommending = ref(false);
/** 按内置模板绑推荐的操作与能力规则（已绑的不动）。 */
async function recommend() {
  recommending.value = true;
  try {
    const res = await api<{ bound: string[]; skipped: string[] }>(`/admin/model-templates/recommend-bindings/${encodeURIComponent(code.value)}`, { method: "POST" });
    ElMessage.success(res.bound.length ? `已绑定：${res.bound.map(opName).join("、")}` : "推荐的操作都已经绑过了");
    await load();
    emit("changed");
  } catch (e) {
    ElMessage.error({ message: settingsApiError(e).message, duration: 8000 });
  } finally {
    recommending.value = false;
  }
}

async function load() {
  loading.value = true;
  try {
    const [models, c, p, pub] = await Promise.all([
      api<Row[]>("/admin/models/"),
      api<Record<string, Cap[]>>(path("/capabilities")),
      api<{ items: Row[]; params: ParamDef[] }>(path("/param-supports")),
      api<{ blockers: Blocker[] }>(path("/publishable"))
    ]);
    const m = models.find(x => x.code === code.value);
    bindings.value = (Array.isArray(m?.operations) ? m.operations : []) as Binding[];
    status.value = String(m?.status ?? "");
    caps.value = c ?? {};
    params.value = p.items ?? [];
    paramDefs.value = p.params ?? [];
    blockers.value = pub.blockers ?? [];
  } catch (e) {
    ElMessage.error(settingsApiError(e).message);
  } finally {
    loading.value = false;
  }
}
watch(() => props.open, open => {
  if (open) void load();
});

async function confirm(title: string, message: string, ok: string): Promise<boolean> {
  try {
    await ElMessageBox.confirm(message, title, { confirmButtonText: ok, cancelButtonText: "取消", type: "warning" });
    return true;
  } catch {
    return false;
  }
}

async function changed() {
  await load();
  emit("changed");
}

// ---------------------------------------------------------------- 绑定
const adding = reactive<{ operation: string; estSeconds: number | null }>({ operation: "", estSeconds: null });
const unbound = computed(() => props.operations.filter(o => !bindings.value.some(b => b.operation === o.code) && o.modelless !== true));

async function saveBinding(operation: string, enabled: boolean, estSeconds: number | null | undefined, done: string) {
  try {
    await api(path("/operations"), { method: "PUT", body: { operation, isEnabled: enabled, estSeconds: estSeconds || null } });
    ElMessage.success(done);
    await changed();
    return true;
  } catch (e) {
    ElMessage.error({ message: settingsApiError(e).message, duration: 8000 });
    return false;
  }
}

async function addBinding() {
  if (!adding.operation) {
    ElMessage.warning("请选择要绑定的操作");
    return;
  }
  if (await saveBinding(adding.operation, true, adding.estSeconds, "已绑定")) {
    adding.operation = "";
    adding.estSeconds = null;
  }
}

async function toggleBinding(b: Binding) {
  if (b.enabled && status.value === "enabled"
    && !(await confirm(`停用「${opName(b.operation)}」`, `这个模型正在启用中：停用后画布上立刻不能再用它做「${opName(b.operation)}」。`, "停用"))) return;
  await saveBinding(b.operation, !b.enabled, b.estSeconds, b.enabled ? "已停用" : "已启用");
}

// 失焦时才保存：每敲一个数字发一次请求的话，改个 120 要连发三次
const estEdits = ref<Record<string, number | null>>({});
watch(bindings, list => {
  estEdits.value = Object.fromEntries(list.map(b => [b.operation, b.estSeconds ?? null]));
});
async function saveEst(b: Binding) {
  const v = estEdits.value[b.operation] ?? null;
  if ((b.estSeconds ?? null) === v) return;
  await saveBinding(b.operation, b.enabled, v, "预计耗时已保存");
}

// ---------------------------------------------------------------- 能力矩阵
const capEditor = reactive<{ open: boolean; operation: string; editing: boolean; draft: SettingsCapabilityDraft }>({
  open: false, operation: "", editing: false, draft: { role: "", minCount: 0, maxCount: 1, allowedMime: [] }
});
function editCap(operation: string, c: Cap | null) {
  capEditor.operation = operation;
  capEditor.editing = c !== null;
  capEditor.draft = c
    ? { role: c.role, minCount: c.minCount, maxCount: c.maxCount, allowedMime: [...(c.allowedMime ?? [])] }
    : { role: "", minCount: 0, maxCount: 1, allowedMime: [] };
  capEditor.open = true;
}
async function saveCap() {
  const err = settingsValidateCapability(capEditor.draft, caps.value[capEditor.operation] ?? [], capEditor.editing);
  if (err) {
    ElMessage.warning(err);
    return;
  }
  try {
    await api(path("/capabilities"), { method: "PUT", body: settingsCapabilityPayload(capEditor.operation, capEditor.draft) });
    ElMessage.success("能力已保存");
    capEditor.open = false;
    await changed();
  } catch (e) {
    ElMessage.error({ message: settingsApiError(e).message, duration: 8000 });
  }
}
async function removeCap(operation: string, c: Cap) {
  if (!(await confirm("删除输入声明", `删除「${opName(operation)}」的「${roleLabel(c.role)}」输入声明？删掉之后这个操作不再接受这类输入。`, "删除"))) return;
  try {
    await api(path(`/capabilities/${encodeURIComponent(operation)}/${encodeURIComponent(c.role)}`), { method: "DELETE" });
    ElMessage.success("已删除");
    await changed();
  } catch (e) {
    ElMessage.error(settingsApiError(e).message);
  }
}

// ---------------------------------------------------------------- 参数支持
const paramEditor = reactive<{ open: boolean; editing: boolean; draft: SettingsParamSupportDraft }>({
  open: false, editing: false, draft: settingsParamSupportDraft(null)
});
const paramName = (c: unknown) => paramDefs.value.find(p => p.code === c)?.name ?? String(c ?? "");
function editParam(row: Row | null) {
  paramEditor.editing = row !== null;
  paramEditor.draft = settingsParamSupportDraft(row);
  paramEditor.open = true;
}
async function saveParam() {
  const err = settingsValidateParamSupport(paramEditor.draft);
  if (err) {
    ElMessage.warning(err);
    return;
  }
  try {
    await api(path("/param-supports"), { method: "PUT", body: settingsParamSupportPayload(paramEditor.draft) });
    ElMessage.success("参数支持已保存：画布下次选这个模型时生效");
    paramEditor.open = false;
    await changed();
  } catch (e) {
    ElMessage.error({ message: settingsApiError(e).message, duration: 8000 });
  }
}
async function removeParam(row: Row) {
  if (!(await confirm("删除参数支持", `删除「${paramName(row.paramCode)}」的型号覆盖？之后这个参数回到节点通用的选项。`, "删除"))) return;
  try {
    await api(path(`/param-supports/${encodeURIComponent(String(row.paramCode))}`), { method: "DELETE" });
    ElMessage.success("已删除");
    await changed();
  } catch (e) {
    ElMessage.error(settingsApiError(e).message);
  }
}
const allowedText = (v: unknown) => (Array.isArray(v) && v.length ? v.join("、") : "沿用通用");
</script>

<template>
  <ElDrawer :model-value="open" :title="`操作、能力与参数 · ${model?.name ?? ''}`" size="min(800px, 100vw)" @update:model-value="emit('update:open', $event)">
    <div v-loading="loading">
      <section class="bd-publish">
        <ElTag v-if="!blockers.length" type="success" size="small">可以启用</ElTag>
        <template v-else>
          <ElTag type="warning" size="small">现在还不能启用</ElTag>
          <ul class="bd-blockers"><li v-for="b in blockers" :key="b.code + b.detail">{{ b.detail }}</li></ul>
        </template>
      </section>

      <div class="bd-title-row">
        <h3 class="bd-title">操作与能力</h3>
        <ElButton size="small" :loading="recommending" @click="recommend">一键推荐配置</ElButton>
      </div>
      <p class="bd-note">一个模型能做哪些操作、每个操作收哪些输入各几个。启用的操作都要有能力配置，模型才能启用。「一键推荐配置」按内置模板给这个协议与类型绑上常用操作与输入规则，已绑的不动。</p>
      <div v-for="b in bindings" :key="b.operation" class="bd-binding">
        <header>
          <div><strong>{{ opName(b.operation) }}</strong> <code class="bd-sub">{{ b.operation }}</code></div>
          <div class="bd-actions">
            <span class="bd-muted">预计耗时</span>
            <ElInputNumber v-model="estEdits[b.operation]" :min="1" :precision="0" size="small" placeholder="用模型的" @blur="saveEst(b)" />
            <ElSwitch :model-value="b.enabled" size="small" @change="toggleBinding(b)" />
          </div>
        </header>
        <ElTable :data="caps[b.operation] ?? []" size="small" row-key="role" empty-text="还没有声明输入 —— 生成前无法校验输入，模型不能启用。">
          <ElTableColumn label="输入" min-width="120">
            <template #default="{ row }">{{ roleLabel(row.role) }} <code class="bd-sub">{{ row.role }}</code></template>
          </ElTableColumn>
          <ElTableColumn label="数量" width="110"><template #default="{ row }">{{ row.minCount }} ~ {{ row.maxCount }} 个</template></ElTableColumn>
          <ElTableColumn label="允许的类型" min-width="140"><template #default="{ row }"><span class="bd-muted">{{ (row.allowedMime ?? []).join("、") || "不限" }}</span></template></ElTableColumn>
          <ElTableColumn label="" width="120" align="right">
            <template #default="{ row }">
              <ElButton link type="primary" size="small" @click="editCap(b.operation, row)">编辑</ElButton>
              <ElButton link type="danger" size="small" @click="removeCap(b.operation, row)">删除</ElButton>
            </template>
          </ElTableColumn>
        </ElTable>
        <ElButton size="small" class="bd-add" @click="editCap(b.operation, null)">添加输入声明</ElButton>
      </div>
      <section class="bd-new">
        <strong>绑定新操作</strong>
        <div class="bd-new-row">
          <ElSelect v-model="adding.operation" filterable placeholder="选择操作">
            <ElOption v-for="o in unbound" :key="String(o.code)" :value="String(o.code)" :label="`${o.name}（${o.code}）`" />
          </ElSelect>
          <ElInputNumber v-model="adding.estSeconds" :min="1" :precision="0" placeholder="预计耗时（可空）" />
          <ElButton type="primary" @click="addBinding">绑定并启用</ElButton>
        </div>
        <p class="bd-note">协议做不了的操作会被拒绝（例如某些协议不支持首尾帧）。</p>
      </section>

      <h3 class="bd-title">参数支持</h3>
      <p class="bd-note">这个型号能选哪些时长、清晰度、画幅 —— 画布选中这个模型时，参数面板只给这些选项。没配的参数沿用节点通用选项。</p>
      <ElTable :data="params" size="small" row-key="paramCode" empty-text="没有型号覆盖，全部沿用节点通用选项。">
        <ElTableColumn label="参数" min-width="130">
          <template #default="{ row }">
            {{ row.paramName || row.paramCode }} <code class="bd-sub">{{ row.paramCode }}</code>
            <ElTag v-if="row.paramEnabled === false" size="small" type="info">参数已停用</ElTag>
          </template>
        </ElTableColumn>
        <ElTableColumn label="可选值" min-width="160"><template #default="{ row }">{{ allowedText(row.allowedValues) }}</template></ElTableColumn>
        <ElTableColumn label="默认" width="90"><template #default="{ row }">{{ row.defaultValue || "—" }}</template></ElTableColumn>
        <ElTableColumn label="控件覆盖" width="90"><template #default="{ row }">{{ row.override ? "有" : "—" }}</template></ElTableColumn>
        <ElTableColumn label="" width="120" align="right">
          <template #default="{ row }">
            <ElButton link type="primary" size="small" @click="editParam(row)">编辑</ElButton>
            <ElButton link type="danger" size="small" @click="removeParam(row)">删除</ElButton>
          </template>
        </ElTableColumn>
      </ElTable>
      <ElButton size="small" class="bd-add" @click="editParam(null)">添加参数支持</ElButton>
    </div>

    <ElDialog v-model="capEditor.open" :title="`${capEditor.editing ? '编辑' : '添加'}输入声明 · ${opName(capEditor.operation)}`" width="min(520px, 96vw)" append-to-body :close-on-click-modal="false">
      <ElForm label-position="top">
        <ElFormItem label="输入角色" required>
          <ElSelect v-model="capEditor.draft.role" :disabled="capEditor.editing">
            <ElOption v-for="r in SETTINGS_INPUT_ROLES" :key="r.value" :value="r.value" :label="`${r.label}（${r.value}）`" />
          </ElSelect>
        </ElFormItem>
        <div class="bd-grid">
          <ElFormItem label="最少几个" required><ElInputNumber v-model="capEditor.draft.minCount" :min="0" :precision="0" /></ElFormItem>
          <ElFormItem label="最多几个" required><ElInputNumber v-model="capEditor.draft.maxCount" :min="0" :precision="0" /></ElFormItem>
        </div>
        <ElFormItem label="允许的类型">
          <ElSelect v-model="capEditor.draft.allowedMime" multiple filterable allow-create default-first-option placeholder="留空不限；输入 image/png 后回车" />
          <small class="bd-muted">按文件头判，不信声明的类型。</small>
        </ElFormItem>
      </ElForm>
      <template #footer>
        <ElButton @click="capEditor.open = false">取消</ElButton>
        <ElButton type="primary" @click="saveCap">保存</ElButton>
      </template>
    </ElDialog>

    <ElDialog v-model="paramEditor.open" :title="`${paramEditor.editing ? '编辑' : '添加'}参数支持`" width="min(600px, 96vw)" append-to-body :close-on-click-modal="false">
      <ElForm label-position="top">
        <ElFormItem label="参数" required>
          <ElSelect v-model="paramEditor.draft.paramCode" :disabled="paramEditor.editing" filterable>
            <ElOption v-for="p in paramDefs" :key="p.code" :value="p.code" :label="`${p.name}（${p.code}）`" />
          </ElSelect>
        </ElFormItem>
        <ElFormItem label="可选值">
          <ElInput v-model="paramEditor.draft.allowedText" placeholder="例如：5, 10, 15（逗号或换行分隔）" />
          <small class="bd-muted">上游真能收的值。没有控件覆盖时，选项按这些值生成（文案就是值本身）。</small>
        </ElFormItem>
        <ElFormItem label="默认值">
          <ElInput v-model="paramEditor.draft.defaultValue" placeholder="必须是可选项之一" />
        </ElFormItem>
        <ElFormItem label="控件覆盖（JSON 对象，可选）">
          <ElInput
v-model="paramEditor.draft.overrideText" type="textarea" :autosize="{ minRows: 3, maxRows: 14 }" class="bd-mono"
            placeholder='{"options":[{"label":"5 秒","value":"5"},{"label":"10 秒","value":"10"}]}' />
          <small class="bd-muted">逐字段覆盖节点的控件配置（options / unit 等）；给了 options 就以它为准。</small>
        </ElFormItem>
      </ElForm>
      <template #footer>
        <ElButton @click="paramEditor.open = false">取消</ElButton>
        <ElButton type="primary" @click="saveParam">保存</ElButton>
      </template>
    </ElDialog>
  </ElDrawer>
</template>

<style scoped>
.bd-publish { display: flex; flex-wrap: wrap; align-items: flex-start; gap: 8px; margin-bottom: 12px; }
.bd-blockers { width: 100%; margin: 0; padding-left: 18px; color: var(--portal-text-soft); font-size: 12px; }
.bd-title { margin: 18px 0 4px; color: var(--portal-text); font-size: 15px; font-weight: 500; }
.bd-note { margin: 0 0 10px; color: var(--portal-faint); font-size: 12px; line-height: 1.5; }
.bd-binding { margin-bottom: 14px; padding: 12px; border: 1px solid var(--portal-line); border-radius: 8px; }
.bd-binding header { display: flex; align-items: center; justify-content: space-between; gap: 12px; margin-bottom: 8px; }
.bd-actions { display: flex; align-items: center; gap: 8px; }
.bd-actions :deep(.el-input-number) { width: 120px; }
.bd-add { margin-top: 8px; }
.bd-new { display: grid; gap: 8px; margin-top: 8px; padding: 12px; border: 1px dashed var(--portal-line); border-radius: 8px; }
.bd-new strong { color: var(--portal-text); font-weight: 500; }
.bd-new-row { display: grid; grid-template-columns: minmax(0, 1fr) 160px max-content; gap: 8px; }
.bd-grid { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 0 14px; }
.bd-grid :deep(.el-input-number) { width: 100%; }
.bd-sub { color: var(--portal-faint); font-size: 11px; }
.bd-muted { color: var(--portal-faint); font-size: 12px; }
.bd-mono { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; font-size: 13px; }
.bd-title-row { display: flex; align-items: center; justify-content: space-between; gap: 12px; }
</style>
