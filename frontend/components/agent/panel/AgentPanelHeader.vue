<script setup lang="ts">
/**
 * 面板头部（设计 03 §2.1）：会话标题 ▾（下拉切换）· ＋ 新建 · 🧠 项目记忆 · 🕘 历史 · ⤢ 全屏对话 · ⧉ 悬浮 / 停靠 · ⟨ 折叠。
 * 「弹出到新窗口」第一期不做，不放按钮。「⤢ 全屏对话」（AGT-44）跳同一项目、同一会话的全屏对话页。
 * 「⧉ 悬浮」（2026-11-01 按参考图，设计 03 R-14）：面板浮在画布上的一张卡片，再点一次停靠回右侧；
 * 悬浮时头部空白处可以拖着移动（拖动由 AgentPanel 接，这里只是按钮），「收起」换成「—」。
 *
 * 下拉里：运行中的会话转圈、等我处理的带红点、别人发起的带名字（可看，不能继续对话）。
 * 标题双击可改名（发起人或工作区管理员，与服务端规则一致；改不了时不进编辑态）。
 */
import { Brain, ChevronDown, History, LoaderCircle, Maximize2, Minus, PanelRight, PanelRightClose, PictureInPicture2, Plus, Sparkles } from "@lucide/vue";
import { ElMessage } from "element-plus";
import { SESSION_STATE_LABELS, relativeTime, sessionRunning, sessionWaitingUser } from "~/utils/agentMessages";
import { apiFailure } from "~/utils/agentPanelBridge";

defineProps<{ floating?: boolean }>();
const emit = defineEmits<{ memory: []; history: []; collapse: []; fullscreen: []; float: [] }>();
const agent = useAgentStore();
const workspace = useWorkspaceStore();

const open = ref(false);
const renaming = ref(false);
const draft = ref("");
const rootRef = ref<HTMLElement>();
const inputRef = ref<HTMLInputElement>();

const title = computed(() => agent.current?.session.title || (agent.currentId ? "会话" : "新会话"));
const canRename = computed(() => Boolean(agent.current) && (agent.current!.isInitiator || ["owner", "admin"].includes(workspace.current?.role ?? "")));
const recent = computed(() => agent.sessions.slice(0, 12));

function pick(id: string) {
  open.value = false;
  if (id !== agent.currentId) void agent.switchSession(id);
}

function newSession() {
  open.value = false;
  agent.newSession();
}

async function startRename() {
  if (!canRename.value) return;
  draft.value = title.value;
  renaming.value = true;
  await nextTick();
  inputRef.value?.select();
}

async function commitRename() {
  if (!renaming.value) return;
  renaming.value = false;
  const next = draft.value.trim();
  if (!next || next === title.value) return;
  try {
    await agent.patchSession({ title: next });
  } catch (cause) {
    ElMessage.error(apiFailure(cause).message);
  }
}

function onDocPointer(event: PointerEvent) {
  if (open.value && rootRef.value && event.target instanceof Node && !rootRef.value.contains(event.target)) open.value = false;
}
onMounted(() => document.addEventListener("pointerdown", onDocPointer));
onBeforeUnmount(() => document.removeEventListener("pointerdown", onDocPointer));
</script>

<template>
  <header ref="rootRef" class="agent-panel-head">
    <div class="agent-session-switch">
      <Sparkles :size="15" class="agent-brand-icon" />
      <input v-if="renaming" ref="inputRef" v-model="draft" class="agent-input agent-title-input" maxlength="60" aria-label="会话标题" @keyup.enter="commitRename" @keyup.esc="renaming = false" @blur="commitRename">
      <button v-else type="button" class="agent-session-title" :title="canRename ? '点击切换会话，双击改名' : '点击切换会话'" :aria-expanded="open" @click="open = !open" @dblclick.stop="startRename">
        <strong>{{ title }}</strong><ChevronDown :size="13" />
      </button>
      <div v-if="open" class="agent-session-menu" role="menu">
        <button
          v-for="session in recent"
          :key="session.id"
          type="button"
          role="menuitem"
          :class="{ 'is-active': session.id === agent.currentId }"
          @click="pick(session.id)"
        >
          <span class="agent-session-mark">
            <LoaderCircle v-if="sessionRunning(session.state)" :size="12" class="spin" />
            <i v-else-if="sessionWaitingUser(session.state)" class="agent-red-dot" title="有卡片等处理" />
          </span>
          <span class="agent-session-name">
            <strong>{{ session.title || '未命名会话' }}</strong>
            <small>{{ SESSION_STATE_LABELS[session.state] ?? session.state }}<template v-if="!session.isInitiator"> · {{ session.initiatorName || '其他成员' }} 发起</template> · {{ relativeTime(session.lastActiveAt, Date.now()) }}</small>
          </span>
        </button>
        <p v-if="!recent.length" class="agent-muted">本项目还没有会话</p>
        <button v-if="agent.sessions.length > recent.length" type="button" class="agent-link-button" @click="open = false; emit('history')">查看全部 {{ agent.sessions.length }} 个会话</button>
      </div>
    </div>
    <div class="agent-head-actions">
      <button type="button" title="新建会话（正在运行的会话不受影响）" @click="newSession"><Plus :size="16" /></button>
      <button type="button" title="项目记忆" @click="emit('memory')"><Brain :size="16" /></button>
      <button type="button" title="历史会话" @click="emit('history')"><History :size="16" /></button>
      <button type="button" title="全屏对话（同一会话，不显示画布）" aria-label="全屏对话" @click="emit('fullscreen')"><Maximize2 :size="15" /></button>
      <button
        type="button"
        :title="floating ? '停靠到右侧' : '悬浮（浮在画布上，拖动头部可移动）'"
        :aria-label="floating ? '停靠到右侧' : '悬浮'"
        :aria-pressed="Boolean(floating)"
        @click="emit('float')"
      >
        <PanelRight v-if="floating" :size="16" />
        <PictureInPicture2 v-else :size="16" />
      </button>
      <button type="button" title="收起（⌘J）" aria-label="收起" @click="emit('collapse')">
        <Minus v-if="floating" :size="16" />
        <PanelRightClose v-else :size="16" />
      </button>
    </div>
  </header>
</template>
