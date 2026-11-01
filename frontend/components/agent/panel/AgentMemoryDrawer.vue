<script setup lang="ts">
/**
 * 项目记忆抽屉（设计 03 §7）：读写 `/.memory.md`（AGT-09 的文档接口），从面板右侧滑出、覆盖消息流。
 *
 * - 打开只读不写；没有记忆文件时显示空状态，**点了「编辑」并保存才建文件**。
 * - 保存带 `version`（文档自己的乐观锁）。撞上 40902 时保住草稿，给「载入最新（放弃我的改动）」/
 *   「以我为准」两条路 —— 冲突措辞复用文档节点那一份（`documentConflictText`），不另写。
 * - 只读成员看得到、点不动编辑，并说明原因。
 */
import { LoaderCircle, X } from "@lucide/vue";
import { ElMessage } from "element-plus";
import MarkdownView from "~/components/common/MarkdownView.vue";
import { classifyDocumentFailure } from "~/utils/documentNode";
import { MEMORY_DOC_PATH, relativeTime } from "~/utils/agentMessages";

type Doc = { id: string; content: string; version: number; source: "agent" | "user"; updatedAt: string; lastEditorName: string };
type Summary = { id: string; path: string; version: number; source: "agent" | "user"; updatedAt: string };

const props = defineProps<{ readOnlyReason?: string }>();
const emit = defineEmits<{ close: [] }>();
const agent = useAgentStore();

const doc = ref<Doc | null>(null);
const loading = ref(true);
const loadError = ref("");
const editing = ref(false);
const draft = ref("");
const saving = ref(false);
const conflict = ref<{ message: string; version?: number }>();
const saveError = ref("");

const base = computed(() => `/projects/${agent.projectId}/documents`);

async function load() {
  loading.value = true;
  loadError.value = "";
  try {
    const list = await useApi()<{ items: Summary[] }>(base.value);
    const hit = list.items?.find(item => item.path === MEMORY_DOC_PATH);
    doc.value = hit ? await useApi()<Doc>(`${base.value}/${hit.id}`) : null;
  } catch (cause) {
    loadError.value = classifyDocumentFailure(cause).message;
  } finally {
    loading.value = false;
  }
}
onMounted(load);

function startEdit() {
  if (props.readOnlyReason) return;
  draft.value = doc.value?.content ?? "";
  conflict.value = undefined;
  saveError.value = "";
  editing.value = true;
}

async function save(force = false) {
  if (saving.value) return;
  saving.value = true;
  saveError.value = "";
  try {
    if (doc.value) {
      const version = force && conflict.value?.version !== undefined ? conflict.value.version : doc.value.version;
      doc.value = await useApi()<Doc>(`${base.value}/${doc.value.id}`, { method: "PUT", body: { content: draft.value, version } });
    } else {
      doc.value = await useApi()<Doc>(base.value, { method: "POST", body: { path: MEMORY_DOC_PATH, content: draft.value } });
    }
    conflict.value = undefined;
    editing.value = false;
    ElMessage.success("项目记忆已保存");
  } catch (cause) {
    const failure = classifyDocumentFailure(cause);
    if (failure.kind === "conflict") conflict.value = { message: failure.message, version: failure.currentVersion };
    else saveError.value = failure.message;
  } finally {
    saving.value = false;
  }
}

async function reloadLatest() {
  conflict.value = undefined;
  editing.value = false;
  await load();
}

const footer = computed(() => {
  if (!doc.value) return "";
  const who = doc.value.source === "agent" ? "Agent" : doc.value.lastEditorName || "你";
  return `最近由 ${who} 写入：${relativeTime(doc.value.updatedAt, Date.now())}`;
});

function onKeydown(event: KeyboardEvent) {
  if (event.key === "Escape" && !editing.value) {
    event.stopPropagation();
    emit("close");
  }
}
</script>

<template>
  <aside class="agent-drawer" role="dialog" aria-label="项目记忆" @keydown="onKeydown">
    <header class="agent-drawer-head">
      <div><strong>项目记忆</strong><small>Agent 在本项目的所有会话里都会读取这里</small></div>
      <button type="button" title="关闭" @click="emit('close')"><X :size="16" /></button>
    </header>
    <div class="agent-drawer-body">
      <p v-if="loading" class="agent-muted"><LoaderCircle :size="13" class="spin" />正在读取</p>
      <p v-else-if="loadError" class="agent-error-text">{{ loadError }} <button type="button" class="agent-link-button" @click="load">重试</button></p>
      <template v-else-if="editing">
        <textarea v-model="draft" class="agent-textarea agent-memory-editor" placeholder="例如：主角孙悟空，写实古风；画幅统一 9:16；不要出现现代元素" />
        <div v-if="conflict" class="agent-notice">
          {{ conflict.message }}
          <div class="agent-card-actions">
            <button type="button" class="agent-secondary" @click="reloadLatest">载入最新（放弃我的改动）</button>
            <button type="button" class="agent-primary" :disabled="saving" @click="save(true)">以我为准</button>
          </div>
        </div>
        <p v-if="saveError" class="agent-error-text">{{ saveError }}</p>
      </template>
      <MarkdownView v-else-if="doc?.content" :source="doc.content" />
      <p v-else class="agent-muted">还没有项目记忆。写下这个项目里一直成立的事（角色设定、画幅、禁忌），Agent 每次都会读到。</p>
    </div>
    <footer class="agent-drawer-foot">
      <span class="agent-muted">{{ footer }}</span>
      <span v-if="readOnlyReason" class="agent-muted">{{ readOnlyReason }}</span>
      <template v-else-if="editing">
        <button type="button" class="agent-secondary" :disabled="saving" @click="editing = false">取消</button>
        <button type="button" class="agent-primary" :disabled="saving || Boolean(conflict)" @click="save()">保存</button>
      </template>
      <button v-else-if="!loading && !loadError" type="button" class="agent-primary" @click="startEdit">编辑</button>
    </footer>
  </aside>
</template>
