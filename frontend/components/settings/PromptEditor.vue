<script setup lang="ts">
/**
 * 系统提示词编辑：草稿 → 试渲染 → 发布 · 版本历史与回滚 · 恢复内置（TT-T-19；照来源系统管理端 `PromptEditor.vue`）。
 *
 * ⚠️ 保存草稿**不影响线上**；发布才生效（每次发布产生一个不可变的版本快照）。
 *    回滚也是发布一个**新版本号**（内容取自旧版本），不是把版本号往回拨。
 * ⚠️ 「恢复内置版本」= 归档这一条覆盖行：通用版归档后回落到代码内置原文，模型专属版归档后回落到通用版。
 *    版本历史保留，之后再保存草稿会重新启用这一行。
 * ⚠️ 试渲染走服务端（`/admin/prompts/preview`），不在前端拼变量；变量要给全（含可选的）。
 */
import {
  ElAlert, ElButton, ElCollapse, ElCollapseItem, ElDrawer, ElForm, ElFormItem, ElInput, ElMessage, ElMessageBox,
  ElOption, ElSelect, ElTag
} from "element-plus";
import { insertPromptPlaceholder, promptInitialContent, promptIssues, promptSampleVars, type PromptRow, type PromptSpec } from "~/utils/promptForm";
import { settingsApiError } from "~/utils/settingsApi";

type Version = { version: number; content: string; note?: string; publishedAt: string; rollbackOfVersion?: number };
type ModelRow = { code: string; name?: string; domain?: string };

const props = defineProps<{ open: boolean; spec: PromptSpec | null; row: PromptRow | null; models: ModelRow[] }>();
const emit = defineEmits<{ (e: "update:open", v: boolean): void; (e: "changed"): void }>();

const api = useApi();
const form = reactive({ modelCode: "", content: "", note: "", saving: false });
const savedDraft = ref("");
const versions = ref<Version[]>([]);
const preview = reactive<{ vars: Record<string, string>; rendered: string; loading: boolean }>({ vars: {}, rendered: "", loading: false });
const textarea = ref<{ textarea?: HTMLTextAreaElement } | null>(null);

const editingRow = computed(() => props.row !== null && props.row.status !== "archived");
const code = computed(() => props.spec?.code ?? "");
const issue = computed(() => promptIssues(form.content, props.spec ?? undefined));
const count = computed(() => [...form.content].length);
const dirty = computed(() => form.content !== savedDraft.value);
const placeholders = computed(() => [
  ...(props.spec?.required ?? []).map(n => ({ n, required: true })),
  ...(props.spec?.optional ?? []).map(n => ({ n, required: false }))
]);
/** 模板里不能直接写 `{{…}}` 字面量：Vue 的插值会在第一个 `}}` 处提前结束。 */
const token = (n: string) => "{" + "{" + n + "}" + "}";
const textModels = computed(() => props.models.filter(m => m.domain === "text"));
const target = computed(() => ({ code: code.value, modelCode: form.modelCode }));
const sameAsBuiltin = computed(() => !!props.spec?.hasBuiltin && form.content === props.spec.builtin);

async function loadVersions() {
  if (!props.row) {
    versions.value = [];
    return;
  }
  try {
    versions.value = await api<Version[]>("/admin/prompts/versions", { query: { ...target.value, limit: 30 } });
  } catch {
    versions.value = [];
  }
}

watch(() => props.open, async open => {
  if (!open) return;
  form.modelCode = String(props.row?.modelCode ?? "");
  form.content = promptInitialContent(props.row, props.spec ?? undefined);
  form.note = "";
  savedDraft.value = editingRow.value ? form.content : "";
  preview.vars = promptSampleVars(props.spec ?? undefined, {});
  preview.rendered = "";
  await loadVersions();
});

async function insert(name: string) {
  const el = textarea.value?.textarea;
  const r = insertPromptPlaceholder(form.content, el?.selectionStart ?? form.content.length, name);
  form.content = r.content;
  await nextTick();
  el?.focus();
  el?.setSelectionRange(r.cursor, r.cursor);
}

function fail(e: unknown) {
  ElMessage.error({ message: settingsApiError(e).message, duration: 6000 });
}

async function saveDraft(silent = false): Promise<boolean> {
  if (issue.value) {
    ElMessage.warning(issue.value);
    return false;
  }
  form.saving = true;
  try {
    await api("/admin/prompts/draft", { method: "PUT", body: { ...target.value, content: form.content, note: form.note.trim() } });
    savedDraft.value = form.content;
    if (!silent) ElMessage.success("草稿已保存（线上内容不变，发布后才生效）");
    emit("changed");
    return true;
  } catch (e) {
    fail(e);
    return false;
  } finally {
    form.saving = false;
  }
}

async function confirm(title: string, message: string, ok: string, danger = false): Promise<boolean> {
  try {
    await ElMessageBox.confirm(message, title, { confirmButtonText: ok, cancelButtonText: "取消", type: danger ? "warning" : "info" });
    return true;
  } catch {
    return false;
  }
}

async function publish() {
  if (issue.value) {
    ElMessage.warning(issue.value);
    return;
  }
  const scope = form.modelCode ? `（只对模型 ${form.modelCode}）` : "（所有没有专属版本的模型）";
  if (!(await confirm(`发布 · ${props.spec?.name ?? ""}`,
    `发布后下一次${props.spec?.name ?? "调用"}就用新内容${scope}。每次发布都会留下一个不可变的版本，出了问题可以在版本历史里回滚。`, "发布"))) return;
  if (dirty.value && !(await saveDraft(true))) return;
  form.saving = true;
  try {
    const r = await api<{ version: number }>("/admin/prompts/publish", { method: "POST", body: { ...target.value, note: form.note.trim() } });
    ElMessage.success(`已发布为 v${r.version}`);
    emit("changed");
    await loadVersions();
  } catch (e) {
    fail(e);
  } finally {
    form.saving = false;
  }
}

async function runPreview() {
  if (issue.value) {
    ElMessage.warning(issue.value);
    return;
  }
  preview.loading = true;
  try {
    const r = await api<{ rendered: string }>("/admin/prompts/preview", {
      method: "POST", body: { ...target.value, content: form.content, vars: promptSampleVars(props.spec ?? undefined, preview.vars) }
    });
    preview.rendered = r.rendered;
  } catch (e) {
    fail(e);
  } finally {
    preview.loading = false;
  }
}

async function rollback(v: Version) {
  let note = "";
  try {
    const r = await ElMessageBox.prompt(`会用 v${v.version} 的内容发布一个新版本（版本号继续往上走），立刻生效。`, `回滚到 v${v.version}`, {
      confirmButtonText: "回滚", cancelButtonText: "取消", inputPlaceholder: "原因（必填，写进版本说明）",
      inputValidator: (s: string) => (s.trim() ? true : "请填写原因")
    });
    note = typeof r === "object" && "value" in r ? String(r.value).trim() : "";
  } catch {
    return;
  }
  try {
    const r = await api<{ version: number }>("/admin/prompts/rollback", { method: "POST", body: { ...target.value, targetVersion: v.version, note } });
    ElMessage.success(`已回滚：线上现在是 v${r.version}（内容来自 v${v.version}）`);
    emit("changed");
    await loadVersions();
  } catch (e) {
    fail(e);
  }
}

async function restore() {
  const fallback = form.modelCode ? "这个模型回落到通用版（没有通用版就用内置原文）" : "所有模型回落到代码内置原文";
  if (!(await confirm("恢复内置版本", `这一条覆盖会被归档、不再生效：${fallback}。版本历史保留。`, "恢复", true))) return;
  form.saving = true;
  try {
    await api("/admin/prompts/archive", { method: "POST", body: target.value });
    ElMessage.success("已恢复：线上回落到内置内容");
    emit("changed");
    emit("update:open", false);
  } catch (e) {
    fail(e);
  } finally {
    form.saving = false;
  }
}

function useBuiltin() {
  if (props.spec?.hasBuiltin) form.content = props.spec.builtin;
}
</script>

<template>
  <ElDrawer :model-value="open" :title="`${spec?.name ?? ''} · ${form.modelCode || '通用版'}`" size="min(880px, 100vw)" @update:model-value="emit('update:open', $event)">
    <ElAlert
v-if="!editingRow" type="info" show-icon :closable="false" class="prompt-alert"
      :title="spec?.hasBuiltin ? '当前线上用的是内置原文。下面从内置原文起步，保存草稿并发布后才生效。' : '这个用途还没有内容。'" />
    <ElForm label-position="top">
      <ElFormItem label="适用模型">
        <ElSelect v-model="form.modelCode" :disabled="editingRow" filterable style="width: 100%">
          <ElOption label="所有模型（通用版）" value="" />
          <ElOption v-for="m in textModels" :key="m.code" :label="`${m.name || m.code}（${m.code}）`" :value="m.code" />
        </ElSelect>
        <small class="prompt-help">模型专属版优先于通用版；建好后不能改适用模型。</small>
      </ElFormItem>
      <div class="prompt-vars">
        <span class="prompt-muted">点击插入变量：</span>
        <ElTag v-for="p in placeholders" :key="p.n" size="small" :type="p.required ? 'primary' : 'info'" class="prompt-var" @click="insert(p.n)">
          {{ token(p.n) }}{{ p.required ? "（必需）" : "" }}
        </ElTag>
      </div>
      <ElFormItem :label="`内容（${count}${spec?.maxChars ? ` / ${spec.maxChars}` : ''} 字）`" :error="issue">
        <ElInput ref="textarea" v-model="form.content" type="textarea" :autosize="{ minRows: 14, maxRows: 30 }" class="prompt-mono" />
      </ElFormItem>
      <div class="prompt-row">
        <ElTag v-if="sameAsBuiltin" size="small" effect="plain">与内置原文一致</ElTag>
        <ElTag v-else-if="dirty" size="small" type="warning">有未保存的修改</ElTag>
        <ElButton v-if="spec?.hasBuiltin && !sameAsBuiltin" size="small" link @click="useBuiltin">把内容换成内置原文</ElButton>
      </div>
      <ElFormItem label="版本说明（可选）"><ElInput v-model="form.note" :maxlength="200" placeholder="例如：分镜数改成按剧情长度自适应" /></ElFormItem>
    </ElForm>

    <ElCollapse>
      <ElCollapseItem title="试渲染（服务端按运行时规则替换变量）" name="preview">
        <div class="prompt-preview-vars">
          <ElFormItem v-for="p in placeholders" :key="p.n" :label="p.n">
            <ElInput v-model="preview.vars[p.n]" type="textarea" :autosize="{ minRows: 1, maxRows: 4 }" />
          </ElFormItem>
        </div>
        <ElButton size="small" :loading="preview.loading" @click="runPreview">试渲染</ElButton>
        <pre v-if="preview.rendered" class="prompt-rendered">{{ preview.rendered }}</pre>
      </ElCollapseItem>
      <ElCollapseItem v-if="spec?.hasBuiltin" title="内置原文（对照）" name="builtin">
        <pre class="prompt-rendered">{{ spec.builtin }}</pre>
      </ElCollapseItem>
      <ElCollapseItem v-if="versions.length" :title="`版本历史（${versions.length}）`" name="versions">
        <div v-for="v in versions" :key="v.version" class="prompt-version">
          <div>
            <strong>v{{ v.version }}</strong>
            <span class="prompt-muted"> · {{ v.publishedAt }}<template v-if="v.rollbackOfVersion"> · 回滚自 v{{ v.rollbackOfVersion }}</template></span>
            <p v-if="v.note" class="prompt-muted">{{ v.note }}</p>
          </div>
          <ElButton size="small" @click="rollback(v)">回滚到这一版</ElButton>
        </div>
      </ElCollapseItem>
    </ElCollapse>

    <template #footer>
      <div class="prompt-actions">
        <ElButton v-if="editingRow && spec?.hasBuiltin" type="danger" plain :loading="form.saving" @click="restore">恢复内置版本</ElButton>
        <ElButton v-else-if="editingRow" type="danger" plain :loading="form.saving" @click="restore">归档</ElButton>
        <span class="prompt-spacer" />
        <ElButton @click="emit('update:open', false)">关闭</ElButton>
        <ElButton :loading="form.saving" @click="saveDraft()">保存草稿</ElButton>
        <ElButton type="primary" :loading="form.saving" @click="publish">发布</ElButton>
      </div>
    </template>
  </ElDrawer>
</template>

<style scoped>
.prompt-alert { margin-bottom: 12px; }
.prompt-help { display: block; width: 100%; margin-top: 4px; color: var(--portal-faint); font-size: 12px; }
.prompt-muted { color: var(--portal-muted); font-size: 12px; }
.prompt-mono { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; font-size: 13px; }
.prompt-vars { display: flex; flex-wrap: wrap; align-items: center; gap: 6px; margin-bottom: 8px; }
.prompt-var { cursor: pointer; font-family: ui-monospace, SFMono-Regular, Menlo, monospace; }
.prompt-row { display: flex; align-items: center; gap: 8px; margin: -8px 0 12px; }
.prompt-preview-vars { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 0 12px; }
.prompt-rendered { max-height: 360px; overflow: auto; padding: 10px 12px; border-radius: 8px; background: var(--portal-surface-2); color: var(--portal-text); font-size: 12px; white-space: pre-wrap; }
.prompt-version { display: flex; align-items: flex-start; justify-content: space-between; gap: 12px; padding: 8px 0; border-bottom: 1px solid var(--portal-line); }
.prompt-version p { margin: 2px 0 0; }
.prompt-actions { display: flex; align-items: center; gap: 8px; }
.prompt-spacer { flex: 1; }
</style>
