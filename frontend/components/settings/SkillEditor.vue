<script setup lang="ts">
/**
 * Agent 技能的新建与编辑（TT-T-19；字段与校验照来源系统管理端 `AgentSkillEditor.vue`）。
 *
 * ⚠️ 保存带 version 乐观锁：两处同时改同一个技能时，后保存的收到冲突，**不静默覆盖**；
 *    附属文档的增删改也推进技能的版本（它们是技能内容的一部分）。
 * ⚠️ 新建的技能一律**停用**：确认无误后在列表上单独启用。
 * ⚠️ 正文是模板：保存前服务端按运行时同一份变量试渲染，失败就拒绝保存并说明原因。
 * ⚠️ 保存是**整份替换**：场景 / 输入槽 / 预填 / 批量四项每次都回传。
 * ⚠️ 内置技能改过之后，启动时不再被内置内容覆盖；「恢复内置版本」把内容（含附属文档）回到内置那份。
 */
import {
  ElAlert, ElButton, ElLoading, ElCheckbox, ElCheckboxGroup, ElDivider, ElDrawer, ElForm, ElFormItem, ElInput, ElInputNumber,
  ElMessage, ElMessageBox, ElOption, ElSelect, ElSwitch, ElTabPane, ElTabs, ElTag
} from "element-plus";
import MediaPickField from "~/components/settings/MediaPickField.vue";
import {
  SKILL_SCENES, SKILL_VERSION_CONFLICT, moveSkillStep, renumberSkillSteps, skillBindingIssue, skillBodyIssue, skillCoverNote,
  skillDraft, skillModelOptionsFor, skillPayload, skillServerIssues, skillToolMeaning, skillUnknownTools, skillWarningsOf,
  skillWhitelistNotes, slotsFromText, validateSkillDraft, type SkillBindings, type SkillDraft, type SkillModelOption, type ToolOption
} from "~/utils/agentSkillForm";
import { builtinBadge } from "~/utils/builtinStatus";
import { settingsApiError } from "~/utils/settingsApi";

const vLoading = ElLoading.directive;

type Row = Record<string, unknown>;
type RefEdit = { id: string; name: string; body: string; sort: number; saved: string; open: boolean };

const props = defineProps<{
  open: boolean;
  skillId: string | null;
  list: Row[];
  tools: ToolOption[];
  vars: string[];
  models: SkillModelOption[];
}>();
const emit = defineEmits<{ (e: "update:open", v: boolean): void; (e: "changed"): void }>();

const api = useApi();
const draft = reactive<SkillDraft>(skillDraft(null));
const refs = ref<RefEdit[]>([]);
const loading = ref(false);
const saving = ref(false);
const conflict = ref(false);
const loadError = ref("");
const issues = ref<string[]>([]);
const warnings = ref<string[]>([]);
const tab = ref("basic");
const currentId = ref<string | null>(null);
const coverIds = computed({
  get: () => (draft.coverMediaId && draft.coverMediaId !== "0" ? [draft.coverMediaId] : []),
  set: (v: string[]) => { draft.coverMediaId = v[0] ?? ""; }
});

const creating = computed(() => !currentId.value);
const badge = computed(() => builtinBadge(draft));
const token = (n: string) => "{" + "{" + n + "}" + "}";
const bodyHint = computed(() => skillBodyIssue("主说明", draft.body, props.vars));
const unknown = computed(() => skillUnknownTools(draft.tools, props.tools));
const notes = computed(() => skillWhitelistNotes(draft.tools, refs.value.length));
const coverHint = computed(() => skillCoverNote(draft.scenes, !!coverIds.value.length));
const slotsHint = computed(() => slotsFromText(draft.inputSlotsText).error);
const BINDINGS: Array<{ key: keyof SkillBindings; label: string; extra: string }> = [
  { key: "reasoning", label: "推理模型", extra: "只能选「可作推理模型」的文本模型；不绑定时按偏好 / 默认。" },
  { key: "video", label: "视频模型", extra: "不绑定时按偏好 / 智能匹配。" },
  { key: "image", label: "图片模型", extra: "不绑定时按偏好 / 智能匹配。" }
];

function nextSort(): number {
  return props.list.reduce((m, r) => Math.max(m, typeof r.sort === "number" ? r.sort : 0), 0) + 10;
}

function toRefEdits(raw: unknown): RefEdit[] {
  return (Array.isArray(raw) ? (raw as Row[]) : []).map(r => ({
    id: String(r.id), name: String(r.name ?? ""), body: String(r.body ?? ""), sort: Number(r.sort ?? 0),
    saved: JSON.stringify([r.name, r.body, r.sort]), open: false
  }));
}

async function load(id: string | null) {
  conflict.value = false;
  loadError.value = "";
  issues.value = [];
  warnings.value = [];
  currentId.value = id;
  if (!id) {
    Object.assign(draft, skillDraft(null, nextSort()));
    refs.value = [];
    return;
  }
  loading.value = true;
  try {
    const row = await api<Row>(`/admin/agent/skills/${id}`);
    Object.assign(draft, skillDraft(row));
    refs.value = toRefEdits(row.references);
  } catch (e) {
    loadError.value = settingsApiError(e).message;
  } finally {
    loading.value = false;
  }
}

watch(() => props.open, open => {
  if (!open) return;
  tab.value = "basic";
  void load(props.skillId);
});

function onWriteError(e: unknown) {
  const err = settingsApiError(e);
  if (err.code === SKILL_VERSION_CONFLICT) {
    conflict.value = true;
    return;
  }
  const found = skillServerIssues(err.details);
  if (found.length) {
    issues.value = found;
    ElMessage.error(`保存被拦下：${found.length} 处问题，已在上方列出`);
    return;
  }
  ElMessage.error({ message: err.message, duration: 8000 });
}

async function save() {
  const issue = validateSkillDraft(draft, { creating: creating.value, vars: props.vars, tools: props.tools, list: props.list });
  if (issue) {
    ElMessage.warning(issue);
    return;
  }
  saving.value = true;
  issues.value = [];
  warnings.value = [];
  try {
    if (creating.value) {
      const row = await api<Row>("/admin/agent/skills", { method: "POST", body: skillPayload(draft, draft.coverMediaId, "create") });
      ElMessage.success("技能已创建（停用状态）。可以继续添加附属文档，确认无误后在列表上启用。");
      currentId.value = String(row.id);
      Object.assign(draft, skillDraft(row));
      warnings.value = skillWarningsOf(row);
    } else {
      const row = await api<Row>(`/admin/agent/skills/${currentId.value}`, { method: "PUT", body: skillPayload(draft, draft.coverMediaId, "update") });
      Object.assign(draft, skillDraft(row));
      warnings.value = skillWarningsOf(row);
      ElMessage.success(`已保存为 v${draft.version}：进行中的会话从下一轮起使用新内容`);
    }
    emit("changed");
  } catch (e) {
    onWriteError(e);
  } finally {
    saving.value = false;
  }
}

async function confirm(title: string, message: string, ok: string): Promise<boolean> {
  try {
    await ElMessageBox.confirm(message, title, { confirmButtonText: ok, cancelButtonText: "取消", type: "warning" });
    return true;
  } catch {
    return false;
  }
}

async function reload() {
  if (!(await confirm("重新加载", "重新加载会丢弃这里还没保存的修改，换成刚保存的那个版本。", "重新加载"))) return;
  await load(currentId.value);
}

async function restore() {
  if (!(await confirm("恢复内置版本",
    "名称、分类、说明、标签、主说明、步骤、工具白名单、快捷入口、排序与附属文档都会回到内置那份；你后加的附属文档会被删除。封面、绑定模型、场景与输入槽保持不变。",
    "恢复"))) return;
  saving.value = true;
  try {
    await api<Row>(`/admin/agent/skills/${currentId.value}/restore-builtin`, { method: "POST" });
    // 重新取一次：恢复也改了附属文档，详情接口一并给全
    await load(currentId.value);
    ElMessage.success(`已恢复内置版本（v${draft.version}）`);
    emit("changed");
  } catch (e) {
    onWriteError(e);
  } finally {
    saving.value = false;
  }
}

// ---- 步骤
function addStep() { draft.steps = renumberSkillSteps([...draft.steps, { no: 0, name: "", checkpoint: false }]); }
function removeStep(i: number) { draft.steps = renumberSkillSteps(draft.steps.filter((_, j) => j !== i)); }
function move(i: number, d: -1 | 1) { draft.steps = moveSkillStep(draft.steps, i, d); }

// ---- 附属文档（每份单独保存；都推进技能版本）
const refDirty = (r: RefEdit) => JSON.stringify([r.name, r.body, r.sort]) !== r.saved;
function addRef() {
  const sort = refs.value.reduce((m, r) => Math.max(m, r.sort), 0) + 10;
  refs.value.push({ id: "", name: "", body: "", sort, saved: "", open: true });
}
async function saveRef(r: RefEdit) {
  const issue = !r.name.trim() ? "附属文档名称不能为空" : skillBodyIssue(`附属文档「${r.name}」`, r.body, props.vars);
  if (issue) {
    ElMessage.warning(issue);
    return;
  }
  saving.value = true;
  try {
    const body = { skillVersion: draft.version, name: r.name.trim(), body: r.body, sort: r.sort };
    const out = r.id
      ? await api<{ skillVersion: number }>(`/admin/agent/skills/${currentId.value}/references/${r.id}`, { method: "PUT", body })
      : await api<{ skillVersion: number }>(`/admin/agent/skills/${currentId.value}/references`, { method: "POST", body });
    const list = await api<{ items: Row[] }>(`/admin/agent/skills/${currentId.value}/references`);
    const opened = new Set(refs.value.filter(x => x.open && x !== r).map(x => x.id));
    refs.value = toRefEdits(list.items).map(x => ({ ...x, open: opened.has(x.id) }));
    draft.version = out.skillVersion;
    draft.isUserModified = draft.isBuiltin;
    ElMessage.success(`附属文档已保存（技能 v${draft.version}）`);
    emit("changed");
  } catch (e) {
    onWriteError(e);
  } finally {
    saving.value = false;
  }
}
async function deleteRef(r: RefEdit) {
  if (!r.id) {
    refs.value = refs.value.filter(x => x !== r);
    return;
  }
  if (!(await confirm(`删除附属文档「${r.name}」`, "删除后模型再也读不到它；正文里提到这份文档的地方需要一并改掉。", "删除"))) return;
  saving.value = true;
  try {
    const out = await api<{ skillVersion: number }>(`/admin/agent/skills/${currentId.value}/references/${r.id}`, {
      method: "DELETE", query: { skillVersion: draft.version }
    });
    refs.value = refs.value.filter(x => x !== r);
    draft.version = out.skillVersion;
    draft.isUserModified = draft.isBuiltin;
    ElMessage.success("已删除");
    emit("changed");
  } catch (e) {
    onWriteError(e);
  } finally {
    saving.value = false;
  }
}
</script>

<template>
  <ElDrawer :model-value="open" :title="creating ? '新建技能' : `${draft.name || draft.code} · v${draft.version}`" size="min(880px, 100vw)" @update:model-value="emit('update:open', $event)">
    <ElAlert v-if="loadError" type="error" show-icon :closable="false" :title="`读取技能失败：${loadError}`" class="skill-alert" />
    <ElAlert v-if="conflict" type="warning" show-icon :closable="false" class="skill-alert" title="这个技能刚被修改过，你的保存没有生效（不会静默覆盖）。">
      <ElButton size="small" @click="reload">重新加载</ElButton>
    </ElAlert>
    <ElAlert v-if="issues.length" type="error" show-icon :closable="false" class="skill-alert" :title="`保存被拦下（${issues.length} 处），改完再保存：`">
      <ul class="skill-list"><li v-for="(x, i) in issues" :key="i">{{ x }}</li></ul>
    </ElAlert>
    <ElAlert v-if="warnings.length" type="warning" show-icon class="skill-alert" title="已保存，但有以下提示（不拦截，建议改掉）：" @close="warnings = []">
      <ul class="skill-list"><li v-for="(x, i) in warnings" :key="i">{{ x }}</li></ul>
    </ElAlert>
    <div v-if="!creating" class="skill-meta">
      <code>{{ draft.code }}</code>
      <ElTag size="small" :type="draft.isEnabled ? 'success' : 'info'">{{ draft.isEnabled ? "启用" : "停用" }}</ElTag>
      <ElTag size="small" :type="badge.type" effect="plain">{{ badge.label }}</ElTag>
      <ElButton v-if="draft.isBuiltin" size="small" :loading="saving" @click="restore">恢复内置版本</ElButton>
      <span class="skill-muted">保存后，进行中的会话从下一轮起使用新内容</span>
    </div>

    <div v-loading="loading">
      <ElTabs v-model="tab">
        <ElTabPane name="basic" label="基本信息">
          <ElForm label-position="top">
            <div class="form-grid">
              <ElFormItem label="code" required>
                <ElInput v-model="draft.code" :disabled="!creating" :maxlength="64" placeholder="story_topic" />
                <small class="skill-help">{{ creating ? "小写字母开头，只含小写字母、数字、下划线；模型加载技能时用它，建好后不能改。" : "建好后不能改（历史会话按它追溯）。" }}</small>
              </ElFormItem>
              <ElFormItem label="名称" required><ElInput v-model="draft.name" :maxlength="64" placeholder="故事选题" /></ElFormItem>
              <ElFormItem label="分类" required><ElInput v-model="draft.category" :maxlength="32" placeholder="创作" /></ElFormItem>
              <ElFormItem label="标签">
                <ElSelect v-model="draft.tags" multiple filterable allow-create default-first-option placeholder="回车添加，最多 10 个" />
              </ElFormItem>
            </div>
            <ElFormItem label="说明">
              <ElInput v-model="draft.description" type="textarea" :rows="2" show-word-limit :maxlength="500" />
              <small class="skill-help">技能列表上显示的一句话。</small>
            </ElFormItem>
            <ElFormItem label="封面（可选）">
              <MediaPickField v-model:ids="coverIds" :max="1" kind="image" hint="JPG / PNG / WebP；技能卡片上展示" />
            </ElFormItem>
            <ElAlert v-if="coverHint" type="warning" show-icon :closable="false" class="skill-alert" :title="coverHint" />
            <div class="form-grid three">
              <ElFormItem label="快捷入口"><ElSwitch v-model="draft.isQuickEntry" /><small class="skill-help">在输入框上方作为快捷技能出现</small></ElFormItem>
              <ElFormItem label="排序"><ElInputNumber v-model="draft.sort" :min="0" :max="100000" :precision="0" /><small class="skill-help">越小越靠前</small></ElFormItem>
              <ElFormItem label="启用状态"><span>{{ draft.isEnabled ? "启用" : "停用" }}</span><small class="skill-help">在列表上启用 / 停用；停用不删除</small></ElFormItem>
            </div>
            <ElDivider content-position="left">绑定模型</ElDivider>
            <div class="form-grid three">
              <ElFormItem v-for="b in BINDINGS" :key="b.key" :label="b.label" :error="skillBindingIssue(models, b.key, draft.bindings[b.key])">
                <ElSelect v-model="draft.bindings[b.key]" filterable>
                  <ElOption v-for="o in skillModelOptionsFor(models, b.key)" :key="o.value" :label="o.label" :value="o.value" />
                </ElSelect>
                <small class="skill-help">{{ b.extra }}</small>
              </ElFormItem>
            </div>
          </ElForm>
        </ElTabPane>

        <ElTabPane name="body" label="主说明">
          <p class="skill-note">
            主说明在模型加载技能后放进系统提示词。只写<b>创作要求与流程指引</b>：卡片结构、规划文档骨架这些格式契约在代码里，写在这里改不动、还可能和它打架。
            「付费前必须经用户确认」由服务端保证，这里写什么都关不掉。
          </p>
          <ElForm label-position="top">
            <ElFormItem :label="`正文（${[...draft.body].length} / 20000 字）`" :error="bodyHint">
              <ElInput v-model="draft.body" type="textarea" :autosize="{ minRows: 14, maxRows: 30 }" class="skill-mono" />
            </ElFormItem>
          </ElForm>
          <div class="skill-vars">
            <span class="skill-muted">可用变量（运行时按当前会话替换；用了别的变量会渲染失败、技能加载不上）：</span>
            <ElTag v-for="v in vars" :key="v" size="small" class="skill-mono">{{ token(v) }}</ElTag>
          </div>
        </ElTabPane>

        <ElTabPane name="steps" label="步骤与工具">
          <ElDivider content-position="left">步骤</ElDivider>
          <p class="skill-note">步骤与检查点用于向用户展示流程；「检查点」只是标注，付费前的确认由服务端强制，不由这里决定。</p>
          <div v-for="(s, i) in draft.steps" :key="i" class="skill-step">
            <span class="skill-step-no">{{ s.no }}</span>
            <ElInput v-model="s.name" :maxlength="64" placeholder="步骤名称" />
            <ElCheckbox v-model="s.checkpoint">检查点</ElCheckbox>
            <ElButton size="small" text :disabled="i === 0" @click="move(i, -1)">上移</ElButton>
            <ElButton size="small" text :disabled="i === draft.steps.length - 1" @click="move(i, 1)">下移</ElButton>
            <ElButton size="small" text type="danger" @click="removeStep(i)">删除</ElButton>
          </div>
          <p v-if="!draft.steps.length" class="skill-muted">还没有步骤。</p>
          <ElButton size="small" @click="addStep">添加步骤</ElButton>
          <ElDivider content-position="left">工具白名单</ElDivider>
          <ElAlert type="info" show-icon :closable="false" class="skill-alert" title="白名单只能收窄，不能放宽：状态机不允许的工具写进来也不会下发（例如规划态永远没有付费工具）。" />
          <ElAlert v-if="unknown.length" type="error" show-icon :closable="false" class="skill-alert" :title="`有未注册的工具：${unknown.join('、')}（服务端会拒绝保存，请取消勾选）`" />
          <ElCheckboxGroup v-model="draft.tools" class="skill-tools">
            <ElCheckbox v-for="t in tools" :key="t.name" :value="t.name">
              <code>{{ t.name }}</code><span class="skill-muted"> · {{ skillToolMeaning(t) }}</span>
            </ElCheckbox>
            <ElCheckbox v-for="n in unknown" :key="n" :value="n"><code>{{ n }}</code><span class="skill-muted"> · 未注册</span></ElCheckbox>
          </ElCheckboxGroup>
          <ul class="skill-list skill-muted"><li v-for="n in notes" :key="n">{{ n }}</li></ul>
        </ElTabPane>

        <ElTabPane name="scene" label="场景与输入">
          <p class="skill-note">
            决定技能出现在哪个入口、营销页输入框上有哪些素材槽，以及选中技能时预填的描述。
            槽位 key、各槽能收的类型与字段长度上限是<b>格式契约</b>，只能收窄，不能新造（保存时服务端再验一次）。
          </p>
          <ElForm label-position="top">
            <ElFormItem label="所属场景">
              <ElCheckboxGroup v-model="draft.scenes">
                <ElCheckbox v-for="s in SKILL_SCENES" :key="s.value" :value="s.value">{{ s.label }}</ElCheckbox>
              </ElCheckboxGroup>
              <small class="skill-help">都不选 = 通用（创作页）。勾了「营销」的技能出现在营销页的技能弹层里。</small>
            </ElFormItem>
            <ElFormItem label="输入槽（JSON 数组）" :error="slotsHint">
              <ElInput
v-model="draft.inputSlotsText" type="textarea" :autosize="{ minRows: 4, maxRows: 16 }" class="skill-mono"
                placeholder='留空 = 用营销场景的默认槽位（商品必填 + 角色可选）。例：[{"key":"product","label":"商品","accept":["product"],"required":true,"maxCount":1}]' />
              <small class="skill-help">每项 {key, label, accept, required, maxCount}；key 只认 product / character / source_video。只在营销场景生效。</small>
            </ElFormItem>
            <ElFormItem label="批量技能">
              <ElSwitch v-model="draft.isBatch" />
              <small class="skill-help">一次出多个变体。勾上后价格明细多一行「最多 N 个变体」；只是说明，不影响计费与变体上限。</small>
            </ElFormItem>
            <ElFormItem :label="`预填描述（${[...draft.prefillText].length} / 200 字）`">
              <ElInput v-model="draft.prefillText" type="textarea" :autosize="{ minRows: 2, maxRows: 6 }" placeholder="帮我给这款商品做一条 15 秒的带货短视频" />
              <small class="skill-help">选中技能时放进输入框（正文为空或仍是上一条预填时才放）；留空 = 不预填。</small>
            </ElFormItem>
          </ElForm>
        </ElTabPane>

        <ElTabPane name="refs" :label="`附属文档（${refs.length}）`" :disabled="creating">
          <p class="skill-note">附属文档由模型按需用 read_skill_ref 按名称读取（流程指引、写法指南、示例）。每份单独保存，同样会试渲染。</p>
          <div v-for="(r, i) in refs" :key="r.id || `new-${i}`" class="skill-ref">
            <header @click="r.open = !r.open">
              <strong>{{ r.name || "（未命名）" }}</strong>
              <span class="skill-muted">{{ [...r.body].length }} 字</span>
              <ElTag v-if="!r.id" size="small">未保存</ElTag>
              <ElTag v-else-if="refDirty(r)" size="small" type="warning">有未保存的修改</ElTag>
            </header>
            <div v-if="r.open" class="skill-ref-body">
              <div class="form-grid">
                <ElFormItem label="名称" required><ElInput v-model="r.name" :maxlength="64" placeholder="单段流程工作指引" /></ElFormItem>
                <ElFormItem label="排序"><ElInputNumber v-model="r.sort" :min="0" :max="100000" :precision="0" /></ElFormItem>
              </div>
              <ElInput v-model="r.body" type="textarea" :autosize="{ minRows: 8, maxRows: 24 }" class="skill-mono" />
              <div class="skill-actions">
                <ElButton type="primary" size="small" :loading="saving" :disabled="!!r.id && !refDirty(r)" @click="saveRef(r)">保存这份文档</ElButton>
                <ElButton type="danger" size="small" plain @click="deleteRef(r)">删除</ElButton>
              </div>
            </div>
          </div>
          <p v-if="!refs.length" class="skill-muted">还没有附属文档。</p>
          <ElButton size="small" @click="addRef">添加附属文档</ElButton>
        </ElTabPane>
      </ElTabs>
    </div>
    <template #footer>
      <div class="skill-actions">
        <ElButton @click="emit('update:open', false)">关闭</ElButton>
        <ElButton type="primary" :loading="saving" :disabled="loading || !!loadError" @click="save">{{ creating ? "创建（停用状态）" : "保存" }}</ElButton>
      </div>
    </template>
  </ElDrawer>
</template>

<style scoped>
.skill-alert { margin-bottom: 12px; }
.skill-meta { display: flex; flex-wrap: wrap; align-items: center; gap: 8px 12px; margin-bottom: 8px; font-size: 12px; }
.form-grid { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 0 14px; }
.form-grid.three { grid-template-columns: repeat(3, minmax(0, 1fr)); }
.skill-mono { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; font-size: 13px; }
.skill-vars { display: flex; flex-wrap: wrap; align-items: center; gap: 6px; }
.skill-help { display: block; width: 100%; margin-top: 4px; color: var(--portal-faint); font-size: 12px; line-height: 1.5; }
.skill-muted { color: var(--portal-muted); font-size: 12px; }
.skill-note { margin: 0 0 12px; color: var(--portal-text-soft); font-size: 13px; line-height: 1.6; }
.skill-step { display: grid; grid-template-columns: 28px minmax(0, 1fr) repeat(4, max-content); align-items: center; gap: 8px; margin-bottom: 8px; }
.skill-step-no { color: var(--portal-muted); text-align: center; }
.skill-tools { display: flex; flex-direction: column; align-items: flex-start; gap: 4px; }
.skill-list { margin: 4px 0 0; padding-left: 18px; }
.skill-ref { margin-bottom: 10px; border: 1px solid var(--portal-line); border-radius: 8px; }
.skill-ref header { display: flex; align-items: center; gap: 10px; padding: 10px 12px; cursor: pointer; }
.skill-ref-body { padding: 0 12px 12px; }
.skill-actions { display: flex; justify-content: flex-end; gap: 8px; margin-top: 8px; }
</style>
