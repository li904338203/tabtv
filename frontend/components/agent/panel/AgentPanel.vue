<script setup lang="ts">
/**
 * 画布右侧的 Agent 面板（AGT-28，设计 03）。展开 = 头部 + 消息流 + 状态条 + 输入框；
 * 折叠 = 不占位置（2026-10-31 去掉了右侧竖条）：入口是画布顶栏的「Agent」按钮，转圈与红点也在那里。
 *
 * 状态全部来自 `stores/agent.ts`（AGT-27，HTTP 为准、事件只作对账信号），面板只读它、调它的动作。
 * 面板 → 画布的联动（定位、Esc 回画布）走 `CanvasBoard` provide 的桥（`utils/agentPanelBridge`）。
 *
 * ⚠️ 打开面板不写任何数据：不建会话、不改画布；写只发生在用户发送 / 提交卡片 / 删除确认时。
 * ⚠️ 只读成员看得到全部会话与卡片，但卡片点不动并说明原因；别人发起的会话可看不可操作。
 *
 * 两种布局（AGT-44，设计 02 §12）：
 * - `layout="dock"`（默认）：画布右侧一栏，可折叠、可拖宽，头部是 `AgentPanelHeader`。
 * - `layout="page"`：全屏对话页的主体 —— 不折叠、不拖宽，头部由页面经 `topbar` 插槽给出
 *   （插槽拿到打开记忆 / 历史 / 对话文件 / 偏好、新建会话这几个动作）；消息列与输入框居中约 780px。
 *   消息流、卡片、页脚、空状态、记忆抽屉都是同一套组件，不另写。
 *   这一页没有载入画布：画布相关的判断看桥上的 `canvasLoaded`（页面提供的桥把「定位」实现成跳画布页）。
 */
import { LoaderCircle, Search, X } from "@lucide/vue";
import AgentFilesDrawer from "./AgentFilesDrawer.vue";
import { ElMessage } from "element-plus";
import AgentComposer from "~/components/agent/composer/AgentComposer.vue";
import AgentEmptyState from "./AgentEmptyState.vue";
import AgentMemoryDrawer from "./AgentMemoryDrawer.vue";
import AgentMessageList from "./AgentMessageList.vue";
import AgentPanelHeader from "./AgentPanelHeader.vue";
import AgentStatusBar from "./AgentStatusBar.vue";
import { SESSION_STATE_LABELS, buildTimeline, relativeTime, sessionRunning, sessionWaitingUser, type AgentCard } from "~/utils/agentMessages";
import { composerAvailability } from "~/utils/agentState";
import { AGENT_PANEL_BRIDGE, NOOP_BRIDGE, apiFailure } from "~/utils/agentPanelBridge";
import { agentChatPath } from "~/utils/agentChat";
import { clampFloatPosition } from "~/stores/agent";

const props = withDefaults(defineProps<{ layout?: "dock" | "page" }>(), { layout: "dock" });

const agent = useAgentStore();
const workspace = useWorkspaceStore();
const bridge = inject(AGENT_PANEL_BRIDGE, NOOP_BRIDGE);

const isPage = computed(() => props.layout === "page");
/** 整页布局永远展开；面板布局看 store 里的开合。 */
const expanded = computed(() => isPage.value || agent.panelOpen);

const listRef = ref<InstanceType<typeof AgentMessageList>>();
type MediaRefInput = { id: string; label: string; thumbMediaId?: string; hint?: string };
const composerRef = ref<{
  focus?: () => void;
  fillText?: (text: string) => void;
  addNodeRef?: (id: string) => void;
  addMediaRef?: (item: MediaRefInput) => void;
  openPrefs?: () => void;
}>();
const memoryOpen = ref(false);
const historyOpen = ref(false);
const filesOpen = ref(false);
const historyQuery = ref("");

const DOCK_TIPS = ["小技巧：输入 @ 引用画布上的节点", "小技巧：在画布上选中节点，会自动带进对话", "小技巧：输入 / 选择一个技能", "小技巧：⌘J 开关面板，Esc 回到画布"];
const PAGE_TIPS = ["小技巧：Agent 在这里写的节点都在画布上，点右上角「画布」查看", "小技巧：输入 / 选择一个技能", "小技巧：输入 @ 引用素材或角色资产", "小技巧：「对话文件」里能看到规划等文档"];
const TIPS = computed(() => (isPage.value ? PAGE_TIPS : DOCK_TIPS));
const tipIndex = ref(0);
let tipTimer: ReturnType<typeof setInterval> | undefined;

const current = computed(() => agent.current);
const role = computed(() => workspace.current?.role ?? null);

/** 卡片 / 产出能不能操作；不能时给最根本的那一条原因。 */
const readOnlyReason = computed(() => {
  if (role.value === "viewer") return "你在该工作区是只读成员，卡片仅供查看";
  const value = current.value;
  if (!value) return "";
  if (!value.isInitiator) return `只有发起人${value.session.initiatorName ? ` ${value.session.initiatorName} ` : ""}能操作这个会话`;
  if (value.state === "cancelled") return "会话已结束";
  return "";
});

/** 发消息（建议胶囊 / 空状态 / 重试）的可用性 —— 与输入框同一个判定。 */
const sendAvailability = computed(() => composerAvailability({
  role: role.value,
  isInitiator: current.value ? current.value.isInitiator : true,
  state: current.value?.state,
  pendingCardId: current.value?.pendingCardId
}));
const sendDisabledReason = computed(() => (sendAvailability.value.canSend ? (agent.sending ? "正在发送" : "") : sendAvailability.value.reason));

const lostPermission = computed(() => current.value?.state === "cancelled" && current.value.latestTurn?.errorCode === "lost_permission");

const allCards = computed<AgentCard[]>(() => {
  const map = new Map(Object.values(agent.cards).map(card => [card.id, card]));
  for (const card of current.value?.pendingCards ?? []) if (!map.has(card.id)) map.set(card.id, card);
  return [...map.values()];
});
const timeline = computed(() => buildTimeline({
  messages: agent.messages,
  steps: Object.values(agent.steps),
  cards: allCards.value,
  turns: Object.values(agent.turns),
  streaming: agent.streaming
}));

const videoModel = computed(() => {
  const prefs = (current.value?.session.prefs ?? {}) as Record<string, unknown>;
  const video = prefs.video && typeof prefs.video === "object" ? prefs.video as Record<string, unknown> : {};
  return typeof video.model === "string" ? video.model : undefined;
});

const historyList = computed(() => {
  const q = historyQuery.value.trim().toLowerCase();
  return agent.sessions.filter(session => !q || (session.title || "").toLowerCase().includes(q));
});

// ---------------------------------------------------------------- 动作

async function send(text: string) {
  if (sendDisabledReason.value) {
    ElMessage.info(sendDisabledReason.value);
    return;
  }
  try {
    await agent.send(text);
  } catch (cause) {
    ElMessage.error(apiFailure(cause).message);
  }
}

/** 「重试这一步」：交给 Agent（重新报价、走额度检查），不重新执行已经成功的步骤。 */
function retry(title?: string) {
  void send(title ? `请重试刚才没有成功的「${title}」这一步，已经成功的不要重做` : "请重试刚才没有成功的那一步，已经成功的不要重做");
}

async function submitCard(card: AgentCard, submission: Record<string, unknown>) {
  await agent.submitCard(card.id, submission);
}

function jumpToCard() {
  if (!listRef.value?.scrollToCard(current.value?.pendingCardId)) listRef.value?.scrollToBottom(true);
}

function pickHistory(id: string) {
  historyOpen.value = false;
  if (id !== agent.currentId) void agent.switchSession(id);
}

function openMemory() {
  historyOpen.value = false;
  filesOpen.value = false;
  memoryOpen.value = true;
}

function openHistory() {
  memoryOpen.value = false;
  filesOpen.value = false;
  historyOpen.value = !historyOpen.value;
}

function openFiles() {
  memoryOpen.value = false;
  historyOpen.value = false;
  filesOpen.value = !filesOpen.value;
}

function openPrefs() {
  composerRef.value?.openPrefs?.();
}

function newSession() {
  agent.newSession();
}

/** 「⤢ 全屏对话」：同一项目、同一会话（新会话草稿时不带会话参数，对话页打开最近的会话）。 */
function goFullscreen() {
  if (!agent.projectId) return;
  void navigateTo(agentChatPath(agent.projectId, agent.currentId));
}

/** 面板里按 Esc：焦点回到画布（抽屉 / 浮层开着时先关它们）。 */
function onKeydown(event: KeyboardEvent) {
  if (event.key !== "Escape" || event.defaultPrevented) return;
  if (memoryOpen.value) {
    memoryOpen.value = false;
    return;
  }
  if (filesOpen.value) {
    filesOpen.value = false;
    return;
  }
  if (historyOpen.value) {
    historyOpen.value = false;
    return;
  }
  bridge.focusCanvas();
}

// ---------------------------------------------------------------- 拖宽

const resizing = ref(false);
function startResize(event: PointerEvent) {
  event.preventDefault();
  const startX = event.clientX;
  const startWidth = agent.panelWidth;
  resizing.value = true;
  const move = (e: PointerEvent) => {
    // 只改内存里的宽度（画布跟着变窄并保持视口中心）；松手时再存本地
    agent.panelWidth = Math.min(640, Math.max(320, Math.round(startWidth + startX - e.clientX)));
  };
  const up = () => {
    resizing.value = false;
    window.removeEventListener("pointermove", move);
    window.removeEventListener("pointerup", up);
    agent.setPanelWidth(agent.panelWidth);
  };
  window.addEventListener("pointermove", move);
  window.addEventListener("pointerup", up);
}

// ---------------------------------------------------------------- 悬浮（2026-11-01，设计 03 R-14）

/** 悬浮：面板浮在画布上的一张卡片、不占画布宽度。只有画布右侧那种布局能悬浮，全屏对话页不能。 */
const floating = computed(() => !isPage.value && agent.panelFloating);
const panelRef = ref<HTMLElement>();
const dragging = ref(false);
/** 位置与宽度经 CSS 变量交给样式；样式里再按窗口夹一次，窗口缩小时卡片不会跑出屏幕。 */
const floatStyle = computed(() => (floating.value
  ? { "--agent-float-right": `${agent.floatRight}px`, "--agent-float-top": `${agent.floatTop}px`, "--agent-float-w": `${agent.panelWidth}px` }
  : undefined));

function toggleFloating() {
  agent.setPanelFloating(!agent.panelFloating);
}

/** 悬浮时按住头部空白处拖动卡片；按在按钮、标题、改名框、会话下拉上不算。 */
function startDrag(event: PointerEvent) {
  const panel = panelRef.value;
  if (!floating.value || !panel || event.button !== 0) return;
  if (event.target instanceof Element && event.target.closest("button, input, a, [role='menu']")) return;
  event.preventDefault();
  const rect = panel.getBoundingClientRect();
  const startX = event.clientX;
  const startY = event.clientY;
  // 以卡片眼下的实际位置为起点（样式夹过的值）：窗口缩小过的话，存着的值可能已经越界
  const startRight = window.innerWidth - rect.right;
  const startTop = rect.top;
  const card = { width: rect.width, height: rect.height };
  dragging.value = true;
  const move = (e: PointerEvent) => {
    const next = clampFloatPosition(
      { right: startRight - (e.clientX - startX), top: startTop + (e.clientY - startY) },
      card,
      { width: window.innerWidth, height: window.innerHeight }
    );
    // 只改内存（卡片跟着走）；松手时再存本地
    agent.floatRight = next.right;
    agent.floatTop = next.top;
  };
  const up = () => {
    dragging.value = false;
    window.removeEventListener("pointermove", move);
    window.removeEventListener("pointerup", up);
    window.removeEventListener("pointercancel", up);
    agent.setFloatPosition(agent.floatRight, agent.floatTop);
  };
  window.addEventListener("pointermove", move);
  window.addEventListener("pointerup", up);
  window.addEventListener("pointercancel", up);
}

/** 「问 Agent」：打开面板并把节点放进输入框（输入框没给这个口时，退回「选中即引用」）。 */
async function askAbout(nodeId: string) {
  agent.setPanelOpen(true);
  await nextTick();
  composerRef.value?.addNodeRef?.(nodeId);
  composerRef.value?.focus?.();
}

/** 「添加到 Agent」：打开面板，把几份媒体（图片 / 视频 / 音频）作为附件放进输入框。 */
async function attachMedia(items: MediaRefInput[]) {
  agent.setPanelOpen(true);
  await nextTick();
  for (const item of items) composerRef.value?.addMediaRef?.(item);
  composerRef.value?.focus?.();
}

/** 空会话的推荐词：只填进输入框、不直接发送（2026-11-01 按用户要求）—— 用户看过、改过再自己点发送。 */
function fillComposer(text: string) {
  composerRef.value?.fillText?.(text);
}

function openRef(item: { kind?: string; id?: string }) {
  if (item.kind === "node" && item.id && !bridge.locate([item.id])) ElMessage.info("这个节点已经不在画布上了");
}

onMounted(() => {
  tipTimer = setInterval(() => { tipIndex.value = (tipIndex.value + 1) % TIPS.value.length; }, 8000);
});
onBeforeUnmount(() => clearInterval(tipTimer));

// 换会话时关掉浮层
watch(() => agent.currentId, () => { historyOpen.value = false; });

defineExpose({ askAbout, attachMedia, openMemory, openHistory, openFiles, openPrefs });
</script>

<template>
  <aside
    ref="panelRef"
    class="agent-panel"
    :class="{ 'is-open': expanded, 'is-page': isPage, 'is-resizing': resizing, 'is-floating': floating, 'is-dragging': dragging }"
    :style="floatStyle"
    :aria-label="isPage ? 'Agent 对话' : 'Agent 面板'"
    @keydown="onKeydown"
  >
    <template v-if="expanded">
      <slot
        v-if="isPage"
        name="topbar"
        :open-memory="openMemory"
        :open-history="openHistory"
        :open-files="openFiles"
        :open-prefs="openPrefs"
        :new-session="newSession"
        :files-open="filesOpen"
        :memory-open="memoryOpen"
        :history-open="historyOpen"
      />
      <template v-else>
        <div class="agent-resizer" title="拖动调整宽度" @pointerdown="startResize" />
        <AgentPanelHeader
          :floating="floating"
          @memory="openMemory"
          @history="openHistory"
          @fullscreen="goFullscreen"
          @float="toggleFloating"
          @collapse="agent.setPanelOpen(false)"
          @pointerdown="startDrag"
        />
      </template>

      <div class="agent-panel-body">
        <p v-if="lostPermission" class="agent-banner is-danger">发起人已不在工作区，会话已结束</p>
        <p v-else-if="agent.syncError" class="agent-banner">{{ agent.syncError }}</p>

        <div v-if="agent.loadingDetail && !timeline.length" class="agent-loading"><LoaderCircle :size="16" class="spin" />正在载入会话</div>
        <AgentEmptyState v-else-if="!timeline.length" :disabled-reason="sendDisabledReason" @pick="fillComposer" />
        <AgentMessageList
          v-else
          ref="listRef"
          :session-key="agent.currentId"
          :items="timeline"
          :has-older="Boolean(agent.olderCursor)"
          :loading-older="agent.loadingOlder"
          :pending-card-id="current?.pendingCardId"
          :read-only-reason="readOnlyReason"
          :send-disabled-reason="sendDisabledReason"
          :video-model="videoModel"
          :submit-card="submitCard"
          @load-older="agent.loadMoreMessages()"
          @send="send"
          @retry="retry"
        />

        <AgentFilesDrawer v-if="filesOpen" @close="filesOpen = false" />
        <AgentMemoryDrawer v-if="memoryOpen" :read-only-reason="role === 'viewer' ? '你在该工作区是只读成员，不能编辑项目记忆' : ''" @close="memoryOpen = false" />

        <aside v-if="historyOpen" class="agent-drawer" aria-label="历史会话">
          <header class="agent-drawer-head">
            <div><strong>历史会话</strong><small>本项目的全部会话（含已结束的）</small></div>
            <button type="button" title="关闭" @click="historyOpen = false"><X :size="16" /></button>
          </header>
          <label class="agent-search"><Search :size="13" /><input v-model="historyQuery" placeholder="搜索标题"></label>
          <div class="agent-drawer-body">
            <button
              v-for="session in historyList"
              :key="session.id"
              type="button"
              class="agent-history-item"
              :class="{ 'is-active': session.id === agent.currentId }"
              @click="pickHistory(session.id)"
            >
              <span class="agent-session-mark">
                <LoaderCircle v-if="sessionRunning(session.state)" :size="12" class="spin" />
                <i v-else-if="sessionWaitingUser(session.state)" class="agent-red-dot" />
              </span>
              <span class="agent-session-name">
                <strong>{{ session.title || '未命名会话' }}</strong>
                <small>{{ SESSION_STATE_LABELS[session.state] ?? session.state }}<template v-if="!session.isInitiator"> · {{ session.initiatorName || '其他成员' }} 发起</template> · {{ relativeTime(session.lastActiveAt, Date.now()) }}</small>
              </span>
            </button>
            <p v-if="!historyList.length" class="agent-muted">{{ historyQuery ? '没有匹配的会话' : '本项目还没有会话' }}</p>
          </div>
        </aside>
      </div>

      <AgentStatusBar :retry-disabled-reason="sendDisabledReason" @jump="jumpToCard" @retry="retry()" />

      <div class="agent-panel-composer">
        <p v-if="current?.pendingCardId && !readOnlyReason" class="agent-composer-hint">有一张卡片等你确认 · 直接发文字会当作对卡片的补充说明</p>
        <p v-else class="agent-composer-hint">{{ TIPS[tipIndex] }}</p>
        <AgentComposer ref="composerRef" context="canvas" :project-id="agent.projectId" :with-canvas="bridge.canvasLoaded" @open-ref="openRef" />
        <small class="agent-disclaimer">AI 生成内容可能有误，请核实重要信息</small>
      </div>
    </template>
  </aside>
</template>

<style>
/*
 * Agent 面板（AGT-28）。全部用画布既有 token（--canvas-* / --cv-*），深色优先、品牌青，不另立色板。
 * 不用 scoped：子组件（messages/、cards/）共用这一份类名，各写一份的话同一种按钮会在两处长得不一样。
 */
.agent-panel { position: relative; min-width: 0; min-height: 0; display: flex; flex-direction: column; border-left: 1px solid var(--canvas-line); background: var(--cv-n09); color: var(--canvas-text); font-size: 13px; z-index: 12; }
.agent-panel.is-resizing { user-select: none; }
.agent-panel button { font: inherit; }

/* 折叠时整个面板不显示（列宽也是 0）：不留一条只有边线的空栏 */
.agent-panel:not(.is-open):not(.is-page) { display: none; }
.agent-red-dot { display: inline-block; width: 7px; height: 7px; border-radius: 50%; background: var(--canvas-danger); }

.agent-resizer { position: absolute; top: 0; bottom: 0; left: -3px; width: 6px; cursor: col-resize; z-index: 3; }
.agent-resizer:hover, .agent-panel.is-resizing .agent-resizer { background: var(--canvas-line-strong); }

/*
 * 悬浮（2026-11-01 按用户给的参考图，设计 03 R-14）：面板是浮在画布上的一张圆角卡片，不占画布宽度。
 * 位置（离右边 / 离顶部）与宽度由面板写进 CSS 变量，这里再按窗口夹一次 —— 窗口缩小时卡片不会跑出屏幕。
 * 高度最多 680，底下给画布右下角的「Agent 新增了 N 个节点」提示留出位置（小地图 2026-11-01 起挪到了左下角）。
 */
.agent-panel.is-floating {
  --agent-float-h: clamp(360px, calc(100vh - 198px), 680px);
  position: fixed;
  top: clamp(8px, var(--agent-float-top, 72px), calc(100vh - var(--agent-float-h) - 8px));
  right: clamp(8px, var(--agent-float-right, 16px), calc(100vw - var(--agent-float-w, 400px) - 8px));
  width: var(--agent-float-w, 400px);
  height: var(--agent-float-h);
  overflow: hidden;
  border: 1px solid var(--canvas-line-strong);
  border-radius: 16px;
  box-shadow: 0 18px 48px var(--cv-shade-42);
  z-index: 30;
}
/* 卡片裁掉了溢出：拖宽的把手收进卡片里 */
.agent-panel.is-floating .agent-resizer { left: 0; }
.agent-panel.is-floating .agent-panel-head { cursor: move; }
.agent-panel.is-floating .agent-title-input { cursor: text; }
.agent-panel.is-dragging { user-select: none; }

/* 头部 */
.agent-panel-head { height: 48px; flex: none; display: flex; align-items: center; justify-content: space-between; gap: 8px; padding: 0 8px 0 12px; border-bottom: 1px solid var(--canvas-line); }
.agent-session-switch { position: relative; min-width: 0; display: flex; align-items: center; gap: 6px; }
.agent-brand-icon { flex: none; color: var(--canvas-text); }
.agent-session-title { min-width: 0; display: flex; align-items: center; gap: 4px; border: 1px solid transparent; border-radius: 6px; padding: 3px 6px; color: var(--cv-n87); background: transparent; cursor: pointer; }
.agent-session-title:hover { border-color: var(--canvas-line); background: var(--canvas-raised); }
.agent-session-title strong { max-width: 200px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; font-weight: 550; }
.agent-title-input { width: 200px; }
.agent-head-actions { display: flex; gap: 2px; }
.agent-head-actions button { width: 30px; height: 30px; display: grid; place-items: center; border: 0; border-radius: 6px; color: var(--cv-n80); background: transparent; cursor: pointer; }
.agent-head-actions button:hover { color: var(--canvas-text); background: var(--canvas-raised); }
.agent-session-menu { position: absolute; top: 34px; left: 0; width: 300px; max-height: 360px; overflow: auto; padding: 6px; border: 1px solid var(--canvas-line-strong); border-radius: 8px; background: var(--cv-n12); box-shadow: 0 16px 40px var(--cv-shade-42); z-index: 20; }
.agent-session-menu > button:not(.agent-link-button), .agent-history-item { width: 100%; display: grid; grid-template-columns: 16px minmax(0, 1fr); align-items: center; gap: 8px; border: 0; border-radius: 6px; padding: 7px 8px; color: var(--cv-n87); background: transparent; text-align: left; cursor: pointer; }
.agent-session-menu > button:hover, .agent-history-item:hover { background: var(--canvas-raised); }
.agent-session-menu > button.is-active, .agent-history-item.is-active { background: var(--cv-n24); }
.agent-session-mark { display: grid; place-items: center; color: var(--canvas-text); }
.agent-session-name { min-width: 0; display: grid; gap: 2px; }
.agent-session-name strong { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; font-weight: 500; }
.agent-session-name small { overflow: hidden; color: var(--canvas-muted); font-size: 11px; text-overflow: ellipsis; white-space: nowrap; }

/* 主体 */
.agent-panel-body { position: relative; flex: 1; min-height: 0; display: flex; flex-direction: column; }
.agent-banner { margin: 8px 12px 0; padding: 7px 10px; border: 1px solid var(--cv-gold-64a18); border-radius: 6px; color: var(--canvas-gold); background: var(--cv-gold-64a6); font-size: 12px; }
.agent-banner.is-danger { border-color: var(--cv-red-68a28); color: var(--canvas-danger); background: transparent; }
.agent-loading { flex: 1; display: flex; align-items: center; justify-content: center; gap: 8px; color: var(--canvas-muted); }

.agent-messages-wrap { position: relative; flex: 1; min-height: 0; }
.agent-messages { height: 100%; overflow-y: auto; padding: 14px 14px 20px; display: flex; flex-direction: column; gap: 12px; }
.agent-older { display: flex; justify-content: center; color: var(--canvas-muted); }
.agent-new-messages { position: absolute; right: 14px; bottom: 12px; display: flex; align-items: center; gap: 4px; border: 1px solid var(--canvas-line-strong); border-radius: 14px; padding: 5px 10px; color: var(--canvas-text); background: var(--cv-n12); box-shadow: 0 6px 18px var(--cv-shade-38); cursor: pointer; font-size: 12px; }

/* 消息 */
.agent-msg { display: grid; gap: 4px; }
.agent-msg-user { justify-items: end; }
.agent-msg-model { display: inline-flex; align-items: center; gap: 4px; color: var(--canvas-muted); font-size: 11px; }
.agent-bubble { max-width: 88%; padding: 8px 12px; border-radius: 12px 12px 3px 12px; background: var(--canvas-raised); }
.agent-bubble p { margin: 0; white-space: pre-wrap; overflow-wrap: anywhere; }
.agent-msg-meta { display: flex; align-items: center; gap: 8px; color: var(--canvas-faint); font-size: 11px; }
.agent-msg-meta button { display: inline-flex; align-items: center; gap: 3px; border: 0; padding: 0; color: inherit; background: transparent; cursor: pointer; opacity: 0; transition: opacity 120ms; }
.agent-msg:hover .agent-msg-meta button { opacity: 1; }
.agent-msg-assistant .markdown-view { color: var(--cv-n90); font-size: 13px; }
.agent-msg-assistant.is-streaming { display: block; }
.agent-msg-assistant.is-streaming .markdown-view { display: inline; }
.agent-cursor { display: inline-block; width: 7px; height: 14px; margin-left: 2px; vertical-align: text-bottom; background: var(--canvas-text); animation: agent-blink 1s steps(2) infinite; }
@keyframes agent-blink { 50% { opacity: 0; } }
.agent-stopped-tag { padding: 1px 6px; border: 1px solid var(--canvas-line-strong); border-radius: 4px; color: var(--canvas-muted); }
.agent-system { align-self: center; max-width: 92%; padding: 4px 10px; border-radius: 10px; color: var(--canvas-muted); background: var(--cv-tint-6); font-size: 12px; text-align: center; white-space: pre-wrap; }

.agent-ref-chips { display: flex; flex-wrap: wrap; align-items: center; gap: 6px; margin-top: 6px; }
/* 胶囊（引用 / 建议 / 步骤 / 空状态共用）用中性色，与输入框统一（2026-10-25，design/03 R-2）。 */
.agent-chip { max-width: 100%; overflow: hidden; border: 1px solid var(--canvas-line-strong); border-radius: 14px; padding: 3px 10px; color: var(--canvas-text); background: var(--canvas-raised); font-size: 12px; text-overflow: ellipsis; white-space: nowrap; cursor: pointer; }
.agent-chip:hover:not(:disabled) { background: var(--cv-n24); }
.agent-chip:disabled { cursor: not-allowed; opacity: .55; }
.agent-chip.is-static { cursor: default; }
.agent-chip.is-gone { color: var(--canvas-faint); border-color: var(--canvas-line); background: transparent; }
.agent-chip.is-suggestion { white-space: normal; text-align: left; }
.agent-suggestions { display: grid; gap: 6px; color: var(--canvas-muted); font-size: 12px; }
/* 每条建议单独一行（2026-10-25 按用户要求，design/03 R-11）；宽度随文字，不拉满整行。 */
.agent-suggestions > div { display: flex; flex-direction: column; align-items: flex-start; gap: 6px; }

/* 步骤行 */
.agent-step { display: grid; gap: 6px; }
.agent-step-head { min-width: 0; display: grid; grid-template-columns: 16px auto minmax(0, 1fr) 14px; align-items: center; gap: 7px; border: 0; border-radius: 6px; padding: 4px 6px; color: var(--cv-n80); background: transparent; text-align: left; cursor: pointer; }
.agent-step-head:disabled { cursor: default; }
.agent-step-head:hover:not(:disabled) { background: var(--canvas-raised); }
.agent-step-head strong { font-weight: 550; white-space: nowrap; }
.agent-step-detail { overflow: hidden; color: var(--canvas-muted); text-overflow: ellipsis; white-space: nowrap; }
.agent-step-icon { display: grid; place-items: center; color: var(--canvas-muted); }
/* 完成 / 成功类也用中性色（2026-10-25，design/03 R-4）：靠图标形状与灰度层次区分，不再用绿。 */
.agent-step.is-done .agent-step-icon { color: var(--canvas-muted); }
.agent-step.is-running .agent-step-icon, .agent-step.is-waiting .agent-step-icon { color: var(--canvas-text); }
.agent-step.is-failed .agent-step-icon, .agent-step.is-failed .agent-step-detail { color: var(--canvas-danger); }
.agent-step-chevron { color: var(--canvas-faint); transition: transform 120ms; }
.agent-step.is-expanded .agent-step-chevron { transform: rotate(180deg); }
.agent-step-body { display: grid; gap: 6px; margin-left: 29px; padding: 8px 10px; border-left: 2px solid var(--canvas-line); font-size: 12px; }
.agent-step-list { margin: 0; padding-left: 16px; color: var(--canvas-muted); }

/* 产出 */
.agent-output { margin-left: 29px; display: grid; gap: 6px; }
.agent-output-pending, .agent-output-failed { display: flex; flex-wrap: wrap; align-items: center; gap: 8px; padding: 8px 10px; border: 1px solid var(--canvas-line); border-radius: 8px; color: var(--canvas-muted); background: var(--cv-n12); font-size: 12px; }
.agent-output-pending { color: var(--canvas-text); }
.agent-output-failed { color: var(--canvas-danger); border-color: var(--cv-red-68a28); }
.agent-output-grid { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 8px; }
.agent-output-grid.is-single { grid-template-columns: minmax(0, 220px); }
/* 含视频的产出：视频卡片按真实比例自带宽高（OutputVideoTile），改成自然换行 —— 固定的格子会把竖屏片子压扁或拉宽。 */
.agent-output-grid.has-video { display: flex; flex-wrap: wrap; align-items: flex-start; }
.agent-output-grid.has-video .agent-output-item:not(.is-video) { width: 220px; }
.agent-output-item { margin: 0; display: grid; gap: 4px; }
.agent-output-thumb { aspect-ratio: 16 / 10; display: grid; place-items: center; overflow: hidden; border: 1px solid var(--canvas-line); border-radius: 8px; padding: 0; background: var(--cv-n12); cursor: zoom-in; }
.agent-output-thumb img { width: 100%; height: 100%; object-fit: cover; }
.agent-output-placeholder { display: flex; align-items: center; gap: 4px; color: var(--canvas-muted); }
.agent-output-text { padding: 8px; color: var(--cv-n87); font-size: 12px; text-align: left; overflow: hidden; }
.agent-output-item figcaption { display: flex; align-items: center; justify-content: space-between; color: var(--canvas-muted); font-size: 11px; }
.agent-output-item figcaption button { border: 0; padding: 2px; color: inherit; background: transparent; cursor: pointer; }
.agent-output-actions { display: flex; align-items: center; justify-content: space-between; }

/* 文件卡 */
.agent-file-card { margin-left: 29px; border: 1px solid var(--canvas-line); border-radius: 8px; background: var(--cv-n12); overflow: hidden; }
.agent-plan-section ~ .agent-file-card, .agent-card .agent-file-card { margin-left: 0; }
.agent-file-head { width: 100%; display: grid; grid-template-columns: 18px minmax(0, 1fr) auto; align-items: center; gap: 8px; border: 0; padding: 9px 11px; color: var(--cv-n87); background: transparent; text-align: left; cursor: pointer; }
.agent-file-head:hover { background: var(--canvas-raised); }
.agent-file-head span { min-width: 0; display: grid; }
.agent-file-head strong { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; font-weight: 550; }
.agent-file-head small { color: var(--canvas-muted); font-size: 11px; }
.agent-file-head em { color: var(--canvas-text); font-size: 12px; font-style: normal; }
.agent-file-body { max-height: 320px; overflow: auto; padding: 10px 12px; border-top: 1px solid var(--canvas-line); }

/* 页脚 */
/* 页脚只显示「日期时间 本轮消耗」，纯文字不可点（design/03 R-6 / R-7）。⚠️ 用 muted 不用 faint：faint 在亮色主题下白底上几乎看不见。 */
.agent-turn-footer { display: flex; flex-wrap: wrap; align-items: center; gap: 12px; color: var(--canvas-muted); font-size: 11px; }
.agent-panel .is-refund { color: var(--canvas-text); }

/* 卡片 */
.agent-card { display: grid; gap: 9px; padding: 12px; border: 1px solid var(--canvas-line-strong); border-radius: 10px; background: var(--cv-n12); transition: box-shadow 200ms; }
.agent-card.is-flash { box-shadow: 0 0 0 2px var(--canvas-text); }
.agent-card.is-closed { display: flex; align-items: center; gap: 7px; padding: 8px 12px; border-color: var(--canvas-line); color: var(--canvas-muted); }
.agent-card.is-closed em { margin-left: auto; padding: 1px 6px; border: 1px solid var(--canvas-line-strong); border-radius: 4px; font-size: 11px; font-style: normal; }
.agent-card.is-closed.is-submitted svg { color: var(--canvas-muted); }
.agent-card-closed-title { min-width: 0; overflow: hidden; color: var(--cv-n80); text-overflow: ellipsis; white-space: nowrap; }
.agent-card.is-closed small { flex: none; font-size: 11px; }
.agent-card p { margin: 0; }
.agent-card-title { display: flex; align-items: center; justify-content: space-between; gap: 8px; color: var(--cv-n92); font-weight: 600; }
.agent-card-title small { color: var(--canvas-muted); font-weight: 400; }
.agent-card-actions { display: flex; flex-wrap: wrap; align-items: center; justify-content: flex-end; gap: 8px; }
.agent-card-actions.is-split { justify-content: space-between; }
.agent-card-actions > .agent-muted:first-child { margin-right: auto; }
.agent-primary, .agent-secondary, .agent-danger { height: 30px; border-radius: 6px; padding: 0 14px; font-size: 12px; cursor: pointer; }
/* 主按钮用反转色：暗色下浅底深字、亮色下深底浅字（--cv-n94 / --cv-n06 随主题翻转），与输入框的发送按钮同一套。 */
.agent-primary { border: 0; color: var(--cv-n06); background: var(--cv-n94); font-weight: 600; }
.agent-primary.is-blocked { opacity: .55; }
.agent-secondary { border: 1px solid var(--canvas-line-strong); color: var(--cv-n87); background: transparent; }
.agent-danger { border: 1px solid var(--cv-red-68a50); color: var(--canvas-danger); background: transparent; }
.agent-primary:disabled, .agent-secondary:disabled, .agent-danger:disabled { cursor: not-allowed; opacity: .45; }
.agent-link-button { display: inline-flex; align-items: center; gap: 3px; border: 0; padding: 0; color: var(--canvas-text); background: transparent; font-size: 12px; cursor: pointer;
  /* 链接不再靠颜色区分，改用下划线提示可点（平时淡、悬浮时实）。 */
  text-decoration: underline; text-decoration-color: var(--canvas-line-strong); text-underline-offset: 2px; }
.agent-link-button:hover:not(:disabled) { text-decoration-color: currentColor; }
.agent-link-button:disabled { color: var(--canvas-faint); cursor: not-allowed; }
.agent-muted { margin: 0; color: var(--canvas-muted); font-size: 12px; }
.agent-error-text { margin: 0; color: var(--canvas-danger); font-size: 12px; }
.agent-notice { margin: 0; padding: 7px 10px; border-radius: 6px; color: var(--canvas-gold); background: var(--cv-gold-64a6); font-size: 12px; }
.agent-textarea, .agent-input { width: 100%; box-sizing: border-box; border: 1px solid var(--canvas-line-strong); border-radius: 6px; padding: 7px 9px; color: var(--cv-n90); background: var(--cv-n09); font: inherit; font-size: 12px; outline: 0; resize: vertical; }
.agent-textarea:focus, .agent-input:focus { border-color: var(--canvas-muted); }

.agent-choice-list { display: grid; gap: 6px; }
.agent-choice { display: grid; gap: 3px; border: 1px solid var(--canvas-line); border-radius: 8px; padding: 9px 11px; color: var(--cv-n87); background: transparent; text-align: left; cursor: pointer; }
.agent-choice:hover:not(:disabled) { border-color: var(--canvas-line-strong); }
.agent-choice.is-selected { border-color: var(--canvas-text); background: var(--canvas-raised); }
.agent-choice:disabled { cursor: default; }
.agent-choice strong { display: flex; align-items: center; gap: 4px; font-weight: 550; }
.agent-choice span { color: var(--canvas-muted); font-size: 12px; white-space: pre-wrap; }
.agent-choice-list.is-compact .agent-choice { padding: 6px 10px; }
.agent-question { color: var(--cv-n90); }
.agent-pager { display: inline-flex; align-items: center; gap: 4px; color: var(--canvas-muted); font-size: 12px; font-weight: 400; }
.agent-pager button { width: 22px; height: 22px; display: grid; place-items: center; border: 0; border-radius: 4px; color: inherit; background: transparent; cursor: pointer; }
.agent-pager button:disabled { opacity: .35; cursor: default; }
.agent-pager-dots { display: flex; gap: 5px; }
.agent-pager-dots button { width: 8px; height: 8px; border: 0; border-radius: 50%; padding: 0; background: var(--canvas-line-strong); cursor: pointer; }
.agent-pager-dots button.is-done { background: var(--cv-n60); }
.agent-pager-dots button.is-current { background: var(--canvas-text); }
.agent-pager-dots button.is-missing { background: var(--canvas-danger); }

.agent-plan-title { display: flex; align-items: center; gap: 6px; color: var(--cv-n87); }
.agent-plan-section { display: grid; gap: 6px; padding-top: 8px; border-top: 1px solid var(--canvas-line); }
.agent-plan-items { margin: 0; padding: 0; list-style: none; display: grid; gap: 5px; }
.agent-plan-items li { display: flex; align-items: flex-start; justify-content: space-between; gap: 10px; }
.agent-plan-items li > span { min-width: 0; display: grid; gap: 1px; }
.agent-plan-items li small { color: var(--canvas-muted); font-size: 11px; }
.agent-plan-items li em { flex: none; color: var(--canvas-gold); font-style: normal; font-weight: 600; }
.agent-plan-items li.is-blocked em { color: var(--canvas-faint); }
.agent-plan-confirm { display: flex; flex-wrap: wrap; align-items: center; justify-content: flex-end; gap: 8px; }
.agent-plan-adjust { display: grid; gap: 8px; }

/* 抽屉（项目记忆 / 历史会话）：从右侧滑出、覆盖消息流 */
.agent-drawer { position: absolute; inset: 0; display: flex; flex-direction: column; background: var(--cv-n09); z-index: 5; animation: agent-drawer-in 160ms ease-out; }
@keyframes agent-drawer-in { from { transform: translateX(24px); opacity: 0; } to { transform: none; opacity: 1; } }
.agent-drawer-head { display: flex; align-items: flex-start; justify-content: space-between; gap: 10px; padding: 12px 14px; border-bottom: 1px solid var(--canvas-line); }
.agent-drawer-head > div { display: grid; gap: 2px; }
.agent-drawer-head small { color: var(--canvas-muted); font-size: 11px; }
.agent-drawer-head button { width: 28px; height: 28px; display: grid; place-items: center; border: 0; border-radius: 6px; color: var(--cv-n80); background: transparent; cursor: pointer; }
.agent-drawer-body { flex: 1; min-height: 0; overflow: auto; padding: 12px 14px; display: grid; align-content: start; gap: 6px; }
.agent-drawer-foot { display: flex; flex-wrap: wrap; align-items: center; justify-content: flex-end; gap: 8px; padding: 10px 14px; border-top: 1px solid var(--canvas-line); }
.agent-drawer-foot > .agent-muted:first-child { margin-right: auto; }
.agent-memory-editor { min-height: 260px; }
.agent-search { display: flex; align-items: center; gap: 6px; margin: 10px 14px 0; padding: 0 9px; border: 1px solid var(--canvas-line-strong); border-radius: 6px; color: var(--canvas-muted); }
.agent-search input { flex: 1; height: 30px; border: 0; color: var(--cv-n90); background: transparent; outline: 0; font-size: 12px; }

/* 对话文件抽屉（AGT-44） */
.agent-files-actions { display: flex; gap: 2px; }
.agent-files-actions button:disabled { cursor: default; opacity: .5; }
.agent-files-title { min-width: 0; flex: 1; }
.agent-files-title strong { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.agent-files-item { width: 100%; display: grid; grid-template-columns: 18px minmax(0, 1fr); align-items: center; gap: 10px; border: 1px solid var(--canvas-line); border-radius: 8px; padding: 9px 11px; color: var(--cv-n87); background: var(--cv-n12); text-align: left; cursor: pointer; }
.agent-files-item:hover { border-color: var(--canvas-line-strong); background: var(--canvas-raised); }
.agent-files-item span { min-width: 0; display: grid; gap: 2px; }
.agent-files-item strong { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; font-weight: 550; }
.agent-files-item small { color: var(--canvas-muted); font-size: 11px; }
.agent-files-preview { display: block; }

/*
 * 空状态（2026-11-01 按用户要求与参考图，设计 03 R-13 / R-15 / R-16）：整块贴在消息区底部、紧挨输入框。
 * 顶上一行「圆形图标 + 标题」居中；推荐词是整行列表（左图标、文字、右箭头），左右与输入框对齐。
 * 靠第一个元素的 margin-top: auto 往下推，不用 justify-content: flex-end —— 后者在面板很矮、内容溢出时
 * 顶部那截滚不回来；这样溢出时照常从顶部开始滚动。
 */
.agent-empty { flex: 1; min-height: 0; overflow-y: auto; display: flex; flex-direction: column; align-items: center; gap: 8px; padding: 24px 12px 10px; color: var(--canvas-text); text-align: center; }
.agent-empty-head { flex: none; max-width: 100%; display: flex; align-items: center; justify-content: center; gap: 10px; margin-top: auto; }
.agent-empty-orb {
  flex: none;
  width: 30px;
  height: 30px;
  display: grid;
  place-items: center;
  border-radius: 50%;
  /* 与主按钮、输入框的发送按钮同一套反转色（2026-11-01 按用户要求，此前是品牌青渐变）：亮色下黑底白标、暗色下白底黑标 */
  color: var(--cv-n06);
  background: var(--cv-n94);
}
.agent-empty-title { min-width: 0; margin: 0; color: var(--cv-n87); font-size: 14px; font-weight: 550; text-align: left; }
.agent-empty-list { width: 100%; display: grid; gap: 2px; margin-top: 12px; text-align: left; }
.agent-empty-item {
  width: 100%;
  min-height: 40px;
  display: flex;
  align-items: center;
  gap: 10px;
  border: 0;
  border-radius: 10px;
  padding: 8px 12px;
  color: var(--cv-n80);
  background: transparent;
  text-align: left;
  cursor: pointer;
}
.agent-empty-item:hover:not(:disabled) { color: var(--canvas-text); background: var(--canvas-raised); }
.agent-empty-item:disabled { cursor: not-allowed; opacity: .55; }
.agent-empty-item-icon { flex: none; color: var(--canvas-muted); }
.agent-empty-item-text { flex: 1; min-width: 0; overflow-wrap: anywhere; }
/* 箭头平时淡一些、悬停时实。⚠️ 不用 --canvas-faint：亮色主题下它是 #e3e4e5，白底上几乎看不见 */
.agent-empty-item-arrow { flex: none; color: var(--canvas-muted); opacity: .6; transition: transform .15s ease, opacity .15s ease; }
.agent-empty-item:hover:not(:disabled) .agent-empty-item-icon,
.agent-empty-item:hover:not(:disabled) .agent-empty-item-arrow { color: var(--canvas-text); opacity: 1; }
.agent-empty-item:hover:not(:disabled) .agent-empty-item-arrow { transform: translateX(2px); }

/* 状态条 */
.agent-status { flex: none; border-top: 1px solid var(--canvas-line); background: var(--cv-n10); font-size: 12px; }
.agent-status-line { display: flex; align-items: center; justify-content: space-between; gap: 8px; padding: 6px 12px; }
.agent-status-main { min-width: 0; display: flex; align-items: center; gap: 6px; border: 0; padding: 0; color: var(--canvas-text); background: transparent; text-align: left; cursor: pointer; }
.agent-status.is-error .agent-status-main { color: var(--canvas-danger); }
.agent-status.is-offline .agent-status-main { color: var(--canvas-gold); }
.agent-status.is-dot .agent-status-main { color: var(--canvas-gold); }
.agent-status-dot { width: 7px; height: 7px; border-radius: 50%; background: currentcolor; }
.agent-status-text { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.agent-status-main svg.is-open { transform: rotate(180deg); }
.agent-todo { margin: 0; padding: 8px 12px 2px; list-style: none; display: grid; gap: 4px; }
.agent-todo li { display: flex; align-items: center; gap: 6px; color: var(--canvas-muted); }
/* 三种状态靠图标区分（对勾 / 转圈 / 空心圆）。⚠️ 不用 --canvas-faint：它在亮色主题下是 #e3e4e5，白底上几乎看不见。 */
.agent-todo li.is-done { color: var(--canvas-muted); }
.agent-todo li.is-doing { color: var(--canvas-text); font-weight: 600; }

/*
 * 整页布局（全屏对话页，AGT-44，对标截图 #31）：消息列与输入框居中约 780px，滚动条贴页面右缘；
 * 抽屉从右侧滑出、不盖住整页。宽度变量用带默认值的写法，页面不给也能用。
 */
.agent-panel.is-page { width: 100%; height: 100%; border-left: 0; font-size: 14px; }
.agent-panel.is-page .agent-messages { padding: 28px max(24px, calc((100% - var(--agent-page-col, 780px)) / 2)) 36px; gap: 14px; }
.agent-panel.is-page .agent-msg-assistant .markdown-view { font-size: 14px; }
.agent-panel.is-page .agent-new-messages { right: max(24px, calc((100% - var(--agent-page-col, 780px)) / 2)); }
.agent-panel.is-page .agent-banner,
.agent-panel.is-page .agent-status,
.agent-panel.is-page .agent-panel-composer { box-sizing: border-box; width: min(var(--agent-page-col, 780px), calc(100% - 48px)); margin-inline: auto; }
.agent-panel.is-page .agent-banner { margin-top: 12px; }
.agent-panel.is-page .agent-status { margin-bottom: 8px; border: 1px solid var(--canvas-line); border-radius: 10px; overflow: hidden; }
.agent-panel.is-page .agent-panel-composer { padding: 0 0 12px; border-top: 0; }
.agent-panel.is-page .agent-drawer { left: auto; width: min(460px, 100%); border-left: 1px solid var(--canvas-line); box-shadow: -18px 0 48px var(--cv-shade-42); }
/* 整页：推荐词列表与下面居中的输入框同宽 */
.agent-panel.is-page .agent-empty { padding-inline: 24px; }
.agent-panel.is-page .agent-empty-list { max-width: var(--agent-page-col, 780px); }

/* 输入区 */
.agent-panel-composer { flex: none; display: grid; gap: 6px; padding: 8px 12px 10px; border-top: 1px solid var(--canvas-line); }
.agent-composer-hint { margin: 0; overflow: hidden; color: var(--canvas-faint); font-size: 11px; text-overflow: ellipsis; white-space: nowrap; }
.agent-disclaimer { color: var(--canvas-faint); font-size: 10px; text-align: center; }
</style>
