<script setup lang="ts">
/**
 * 对话文件抽屉（AGT-44，设计 02 §12）：列出项目文档（`/plan.md` 等），点开只读预览全文。
 *
 * - 列表 `GET /projects/{id}/documents`（不带正文）；正文点开才取 `GET .../documents/{documentId}`。
 * - **只读**：这里从不写文档（编辑在画布的文档节点上、项目记忆在记忆抽屉里）。
 * - 打开抽屉只读不写；Agent 刚写完一份文档时，点「刷新」或重新打开就能看到。
 */
import { ArrowLeft, FileText, LoaderCircle, RefreshCw, X } from "@lucide/vue";
import MarkdownView from "~/components/common/MarkdownView.vue";
import { classifyDocumentFailure } from "~/utils/documentNode";
import { relativeTime } from "~/utils/agentMessages";
import {
  documentDisplayName,
  documentSizeText,
  sortDocuments,
  type ProjectDocument,
  type ProjectDocumentSummary
} from "~/utils/agentChat";

const emit = defineEmits<{ close: [] }>();
const agent = useAgentStore();

const items = ref<ProjectDocumentSummary[]>([]);
const loading = ref(true);
const loadError = ref("");
const selected = ref<ProjectDocumentSummary | null>(null);
const doc = ref<ProjectDocument | null>(null);
const docLoading = ref(false);
const docError = ref("");
/** 换文档 / 关抽屉后，晚到的旧响应不覆盖当前内容。 */
let docSeq = 0;

const base = computed(() => `/projects/${agent.projectId}/documents`);

async function load() {
  if (!agent.projectId) return;
  loading.value = true;
  loadError.value = "";
  try {
    const list = await useApi()<{ items: ProjectDocumentSummary[] }>(base.value);
    items.value = sortDocuments(list.items ?? []);
  } catch (cause) {
    loadError.value = classifyDocumentFailure(cause).message;
  } finally {
    loading.value = false;
  }
}

async function open(item: ProjectDocumentSummary) {
  const seq = ++docSeq;
  selected.value = item;
  doc.value = null;
  docError.value = "";
  docLoading.value = true;
  try {
    const value = await useApi()<ProjectDocument>(`${base.value}/${item.id}`);
    if (seq === docSeq) doc.value = value;
  } catch (cause) {
    if (seq === docSeq) docError.value = classifyDocumentFailure(cause).message;
  } finally {
    if (seq === docSeq) docLoading.value = false;
  }
}

function back() {
  docSeq += 1;
  selected.value = null;
  doc.value = null;
  docError.value = "";
}

onMounted(load);
onBeforeUnmount(() => { docSeq += 1; });
</script>

<template>
  <aside class="agent-drawer agent-files-drawer" aria-label="对话文件">
    <header class="agent-drawer-head">
      <template v-if="selected">
        <button type="button" title="返回列表" @click="back"><ArrowLeft :size="16" /></button>
        <div class="agent-files-title">
          <strong>{{ documentDisplayName(selected.path) }}</strong>
          <small>{{ selected.source === 'agent' ? 'Agent 写入' : '手动编辑' }} · {{ relativeTime(selected.updatedAt, Date.now()) }} · 只读预览</small>
        </div>
      </template>
      <div v-else>
        <strong>对话文件</strong>
        <small>Agent 在这个项目里写下的文档（只读）</small>
      </div>
      <span class="agent-files-actions">
        <button v-if="!selected" type="button" title="刷新" :disabled="loading" @click="load"><RefreshCw :size="15" :class="{ spin: loading }" /></button>
        <button type="button" title="关闭" @click="emit('close')"><X :size="16" /></button>
      </span>
    </header>

    <div v-if="selected" class="agent-drawer-body agent-files-preview">
      <p v-if="docLoading" class="agent-muted"><LoaderCircle :size="13" class="spin" />正在读取</p>
      <p v-else-if="docError" class="agent-error-text">{{ docError }}</p>
      <MarkdownView v-else-if="doc?.content" :source="doc.content" />
      <p v-else class="agent-muted">（空文档）</p>
    </div>

    <div v-else class="agent-drawer-body">
      <p v-if="loading && !items.length" class="agent-muted"><LoaderCircle :size="13" class="spin" />正在读取</p>
      <p v-else-if="loadError" class="agent-error-text">{{ loadError }}</p>
      <p v-else-if="!items.length" class="agent-muted">还没有文件。Agent 写规划（plan.md）等文档后会出现在这里。</p>
      <button v-for="item in items" :key="item.id" type="button" class="agent-files-item" @click="open(item)">
        <FileText :size="16" />
        <span>
          <strong>{{ documentDisplayName(item.path) }}</strong>
          <small>{{ item.source === 'agent' ? 'Agent 写入' : '手动编辑' }} · {{ relativeTime(item.updatedAt, Date.now()) }} · {{ documentSizeText(item.bytes) }}</small>
        </span>
      </button>
    </div>
  </aside>
</template>
