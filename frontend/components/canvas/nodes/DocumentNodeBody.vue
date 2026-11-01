<script setup lang="ts">
/**
 * 文档节点的节点体（AGT-09）。
 *
 * 节点上只存 `params.docId`；正文按需从 `/projects/{id}/documents/{docId}` 取，
 * 保存走文档自己的乐观锁（与画布的 version 无关）—— 所以这里**不碰画布 store**：
 * 改文档不该让画布进入"未保存"，两个人一个改文档、一个挪节点也不会互相撞 409。
 *
 * ⚠️ 打开节点只做 GET，不写任何东西：画布页会自动保存用户的真实数据，
 *    渲染一个节点就产生一次写入的话，翻一次画布就是一堆用户没做过的修改。
 */
import type { components as ApiComponents } from "@tabtv/api-client";
import { NotebookText } from "@lucide/vue";
import MarkdownView from "~/components/common/MarkdownView.vue";
import { classifyDocumentFailure, documentFileName } from "~/utils/documentNode";

type ProjectDocument = ApiComponents["schemas"]["ProjectDocument"];

const props = defineProps<{
  projectId: string;
  docId: string;
}>();

const api = useApi();
const doc = ref<ProjectDocument>();
const loading = ref(false);
const loadError = ref("");
const editing = ref(false);
const draft = ref("");
const saving = ref(false);
const saveError = ref("");
/** 版本冲突：保住本地草稿，让用户选「载入最新」或「以我为准」，绝不静默覆盖。 */
const conflict = ref("");
const editor = ref<HTMLTextAreaElement>();

const fileName = computed(() => (doc.value ? documentFileName(doc.value.path) : "文档"));
const dirty = computed(() => editing.value && doc.value !== undefined && draft.value !== doc.value.content);

function documentUrl() {
  return `/projects/${props.projectId}/documents/${props.docId}`;
}

async function load() {
  if (!props.projectId || !props.docId) return;
  loading.value = true;
  loadError.value = "";
  try {
    doc.value = await api<ProjectDocument>(documentUrl());
  } catch (cause) {
    loadError.value = classifyDocumentFailure(cause).message;
  } finally {
    loading.value = false;
  }
}

watch(() => `${props.projectId}:${props.docId}`, () => {
  editing.value = false;
  conflict.value = "";
  void load();
}, { immediate: true });

async function startEdit() {
  if (!doc.value || editing.value) return;
  draft.value = doc.value.content;
  saveError.value = "";
  conflict.value = "";
  editing.value = true;
  await nextTick();
  editor.value?.focus();
}

function cancelEdit() {
  editing.value = false;
  conflict.value = "";
  saveError.value = "";
}

async function put(version: number) {
  doc.value = await api<ProjectDocument>(documentUrl(), {
    method: "PUT",
    body: { content: draft.value, version }
  });
}

async function save() {
  if (!doc.value || saving.value) return;
  saving.value = true;
  saveError.value = "";
  try {
    await put(doc.value.version);
    editing.value = false;
    conflict.value = "";
  } catch (cause) {
    const failure = classifyDocumentFailure(cause);
    if (failure.kind === "conflict") conflict.value = failure.message;
    else saveError.value = failure.message;
  } finally {
    saving.value = false;
  }
}

/** 载入最新：放弃本地改动，编辑框换成服务端当前内容。 */
async function adoptLatest() {
  saving.value = true;
  try {
    doc.value = await api<ProjectDocument>(documentUrl());
    draft.value = doc.value.content;
    conflict.value = "";
    saveError.value = "";
  } catch (cause) {
    saveError.value = classifyDocumentFailure(cause).message;
  } finally {
    saving.value = false;
  }
}

/** 以我为准：取到服务端当前版本号，再用本地内容覆盖它（用户已明确选择覆盖）。 */
async function overwrite() {
  saving.value = true;
  saveError.value = "";
  try {
    const latest = await api<ProjectDocument>(documentUrl());
    await put(latest.version);
    editing.value = false;
    conflict.value = "";
  } catch (cause) {
    const failure = classifyDocumentFailure(cause);
    if (failure.kind === "conflict") conflict.value = failure.message;
    else saveError.value = failure.message;
  } finally {
    saving.value = false;
  }
}

function onEditorKeydown(event: KeyboardEvent) {
  if ((event.metaKey || event.ctrlKey) && event.key.toLowerCase() === "s") {
    event.preventDefault();
    void save();
  } else if (event.key === "Escape" && !dirty.value) {
    cancelEdit();
  }
}
</script>

<template>
  <div class="document-node-body">
    <div class="document-node-file"><NotebookText :size="14" /><span>{{ fileName }}</span><small v-if="doc">v{{ doc.version }}</small></div>
    <div v-if="!docId" class="document-node-state">这个文档节点没有关联文档</div>
    <div v-else-if="loading && !doc" class="document-node-state">正在载入…</div>
    <div v-else-if="loadError" class="document-node-state error">
      {{ loadError }}
      <button type="button" class="nodrag" @click.stop="load">重试</button>
    </div>
    <template v-else-if="doc">
      <div v-if="editing" class="document-node-editor nodrag nopan nowheel" @mousedown.stop @pointerdown.stop @dblclick.stop>
        <textarea
          ref="editor"
          v-model="draft"
          spellcheck="false"
          aria-label="编辑文档"
          placeholder="用 Markdown 书写…"
          @keydown="onEditorKeydown"
        />
        <div v-if="conflict" class="document-node-conflict" role="alert">
          <span>{{ conflict }}，你的改动还在编辑框里。</span>
          <div>
            <button type="button" :disabled="saving" @click.stop="adoptLatest">载入最新（放弃我的改动）</button>
            <button type="button" :disabled="saving" @click.stop="overwrite">以我为准（覆盖对方的改动）</button>
          </div>
        </div>
        <p v-if="saveError" class="document-node-error" role="alert">{{ saveError }}</p>
        <div class="document-node-actions">
          <small>{{ draft.length }} 字 · ⌘/Ctrl+S 保存</small>
          <button type="button" :disabled="saving" @click.stop="cancelEdit">取消</button>
          <button type="button" class="primary" :disabled="saving || !dirty || Boolean(conflict)" @click.stop="save">{{ saving ? "保存中…" : "保存" }}</button>
        </div>
      </div>
      <!-- ⚠️ `.stop`：双击冒泡到画布的话 Vue Flow 会顺手把视口放大一级 -->
      <div v-else class="document-node-preview nowheel" title="双击编辑" @dblclick.stop="startEdit">
        <MarkdownView v-if="doc.content.trim()" :source="doc.content" :max-lines="12" />
        <p v-else class="document-node-empty">空文档，双击开始编辑</p>
      </div>
    </template>
  </div>
</template>

<style scoped>
.document-node-body { display: grid; gap: 10px; padding: 14px 16px 16px; min-height: inherit; color: var(--cv-n92); }
.document-node-file { display: flex; align-items: center; gap: 6px; color: var(--cv-n63); font-size: 12px; }
.document-node-file span { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.document-node-file small { margin-left: auto; color: var(--cv-n42); }
.document-node-state { display: grid; place-items: center; gap: 8px; min-height: 120px; color: var(--cv-n63); font-size: 12px; text-align: center; }
.document-node-state.error { color: var(--cv-n88); }
.document-node-preview { cursor: text; min-height: 120px; font-size: 12px; }
.document-node-preview :deep(.markdown-view) { color: var(--cv-n92); font-size: 12px; }
.document-node-empty { margin: 0; padding: 40px 0; color: var(--cv-n42); font-size: 12px; text-align: center; }
.document-node-editor { display: grid; gap: 8px; }
.document-node-editor textarea {
  width: 100%;
  min-height: 220px;
  resize: vertical;
  padding: 10px 12px;
  border: 1px solid var(--cv-tint-22);
  border-radius: 6px;
  background: var(--cv-n09);
  color: var(--cv-n92);
  font: 12px/1.6 ui-monospace, SFMono-Regular, Menlo, monospace;
  user-select: text;
}
.document-node-actions { display: flex; align-items: center; gap: 8px; }
.document-node-actions small { margin-right: auto; color: var(--cv-n42); font-size: 11px; }
.document-node-body button {
  padding: 4px 10px;
  border: 1px solid var(--cv-tint-22);
  border-radius: 6px;
  background: transparent;
  color: var(--cv-n88);
  font-size: 12px;
  cursor: pointer;
}
.document-node-body button.primary { background: var(--cv-n92); color: var(--cv-n09); border-color: transparent; }
.document-node-body button:disabled { opacity: 0.45; cursor: not-allowed; }
.document-node-conflict { display: grid; gap: 6px; padding: 8px 10px; border: 1px solid var(--cv-tint-22); border-radius: 6px; background: var(--cv-n14); font-size: 12px; }
.document-node-conflict div { display: flex; flex-wrap: wrap; gap: 6px; }
.document-node-error { margin: 0; color: var(--cv-n88); font-size: 12px; }
</style>
