<script setup lang="ts">
/**
 * 文件卡（设计 03 §3.6）。「查看」= 画布定位到文档节点并选中；按住 ⌘ / Ctrl 点击 = 在面板内只读展开全文。
 * 文档不在画布上（节点被删了、或这份文档本来就没有节点）时，直接在面板内展开 —— 不让「查看」变成没反应。
 *
 * 只读：这里从不写文档（编辑在文档节点上或记忆抽屉里做）。
 */
import { FileText, LoaderCircle } from "@lucide/vue";
import MarkdownView from "~/components/common/MarkdownView.vue";
import { DOCUMENT_NODE_TYPE, documentFileName } from "~/utils/documentNode";
import { AGENT_PANEL_BRIDGE, NOOP_BRIDGE, apiFailure } from "~/utils/agentPanelBridge";

const props = defineProps<{ path: string; docId?: string; nodeId?: string; subtitle?: string }>();
const agent = useAgentStore();
const canvas = useCanvasStore();
const bridge = inject(AGENT_PANEL_BRIDGE, NOOP_BRIDGE);
/** 画布没载入（全屏对话页）时不读画布 store：那里可能是上一个画布留下的节点。 */
const canvasNodes = computed(() => (bridge.canvasLoaded ? canvas.nodes : []));

const name = computed(() => documentFileName(props.path));
const expanded = ref(false);
const loading = ref(false);
const content = ref("");
const error = ref("");
const resolvedId = ref(props.docId ?? "");

type Summary = { id: string; path: string };

async function documentId(): Promise<string> {
  if (resolvedId.value) return resolvedId.value;
  const list = await useApi()<{ items: Summary[] }>(`/projects/${agent.projectId}/documents`);
  resolvedId.value = list.items?.find(item => item.path === props.path)?.id ?? "";
  return resolvedId.value;
}

function nodeFor(docId: string): string | undefined {
  if (props.nodeId && canvasNodes.value.some(node => node.id === props.nodeId)) return props.nodeId;
  return canvasNodes.value.find(node => node.data.nodeType === DOCUMENT_NODE_TYPE && String(node.data.params.docId ?? "") === docId)?.id;
}

async function expand() {
  if (expanded.value) {
    expanded.value = false;
    return;
  }
  expanded.value = true;
  loading.value = true;
  error.value = "";
  try {
    const id = await documentId();
    if (!id) throw new Error("找不到这份文档");
    const doc = await useApi()<{ content: string }>(`/projects/${agent.projectId}/documents/${id}`);
    content.value = doc.content ?? "";
  } catch (cause) {
    error.value = cause instanceof Error && !("data" in cause) ? cause.message : apiFailure(cause).message;
  } finally {
    loading.value = false;
  }
}

async function view(event: MouseEvent) {
  // 全屏对话页：没有画布可定位，「查看」就是原地只读展开（设计 02 §12）
  if (event.metaKey || event.ctrlKey || !bridge.canvasLoaded) {
    await expand();
    return;
  }
  try {
    const node = props.nodeId && canvasNodes.value.some(item => item.id === props.nodeId) ? props.nodeId : nodeFor(await documentId());
    if (node && bridge.locate([node])) return;
  } catch {
    // 查不到文档列表时退回面板内展开
  }
  await expand();
}
</script>

<template>
  <div class="agent-file-card">
    <button type="button" class="agent-file-head" :title="bridge.canvasLoaded ? '查看（按住 ⌘ / Ctrl 在面板内展开）' : '展开全文（只读）'" @click="view">
      <FileText :size="16" />
      <span><strong>{{ name }}</strong><small>{{ subtitle || '项目文档' }}</small></span>
      <em>查看 →</em>
    </button>
    <div v-if="expanded" class="agent-file-body">
      <p v-if="loading" class="agent-muted"><LoaderCircle :size="13" class="spin" />正在读取</p>
      <p v-else-if="error" class="agent-error-text">{{ error }}</p>
      <MarkdownView v-else-if="content" :source="content" />
      <p v-else class="agent-muted">（空文档）</p>
    </div>
  </div>
</template>
