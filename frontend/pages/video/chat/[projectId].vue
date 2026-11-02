<script setup lang="ts">
/**
 * 全屏对话页（AGT-44，设计 02 §12，对标截图 #31）：`/video/chat/{projectId}?session={sessionId}`。
 *
 * 与画布页是**同一项目、同一会话**的两种视图：Agent 写画布在服务端完成（D-2），这一页开着时画布照常被写，
 * 点「画布 ↗」过去就能看到。与画布页一样不套平台布局。
 *
 * ⚠️ 不渲染画布、**不加载也不保存 canvasData**：不调 `canvas.load`、不碰画布 store —— 画布 store 是全局的，
 *    里面可能还留着上一个打开过的画布，所以面板组件在这一页一律不读它（桥上 `canvasLoaded: false`）。
 * ⚠️ 打开页面只读不写：项目名（只取名字）、会话列表与详情；订阅 `project:{id}` 与对账沿用
 *    `agent.openProject`。写只发生在用户发送 / 提交卡片 / 改偏好时。
 * 「在画布中查看」= 跳画布页并带 `?node=`（画布页只平移选中、不写数据）。
 */
import type { components } from "@tabtv/api-client";
import { ArrowUpRight, Brain, FolderOpen, History, LoaderCircle, Plus, SlidersHorizontal, Sparkles } from "@lucide/vue";
import AgentPanel from "~/components/agent/panel/AgentPanel.vue";
import { AGENT_PANEL_BRIDGE } from "~/utils/agentPanelBridge";
import { agentCanvasViewPath, agentChatPath, parseSessionParam } from "~/utils/agentChat";

definePageMeta({});

type ProjectResponse = Pick<components["schemas"]["Project"], "id" | "name">;

const route = useRoute();
const router = useRouter();
const agent = useAgentStore();

const projectId = String(route.params.projectId);
const projectName = ref("");
const ready = ref(false);
const error = ref("");
let unmounted = false;

/** 面板 → 「画布」：这一页没有画布，定位 = 跳画布页并带 `?node=`（同一会话、面板展开）。 */
provide(AGENT_PANEL_BRIDGE, {
  canvasLoaded: false,
  locate: (nodeIds: string[]) => {
    if (!nodeIds.length) return 0;
    void navigateTo(agentCanvasViewPath(projectId, { sessionId: agent.currentId, nodeIds }));
    return nodeIds.length;
  },
  focusCanvas: () => undefined
});

const canvasPath = computed(() => agentCanvasViewPath(projectId, { sessionId: agent.currentId }));
const sessionTitle = computed(() => agent.current?.session.title || (agent.currentId ? "" : "新会话"));

useHead(() => ({ title: projectName.value ? `${projectName.value} · 对话` : "对话" }));

/**
 * 地址栏跟着当前会话走（刷新后还是这个会话；复制链接给同事打开的是同一个会话）。
 * 只改地址不改数据；新会话草稿时去掉参数。
 */
watch(() => agent.currentId, (id) => {
  if (!ready.value || unmounted) return;
  if ((parseSessionParam(route.query.session) ?? "") === id) return;
  void router.replace(agentChatPath(projectId, id));
});

onMounted(async () => {
  try {
    // 只取项目名：响应里虽然带 canvasData，但不交给画布 store、不渲染，也就不会有任何保存。
    const project = await useApi()<ProjectResponse>(`/projects/${projectId}`);
    if (unmounted) return;
    projectName.value = project.name ?? "";
    ready.value = true;
    void agent.openProject(projectId, parseSessionParam(route.query.session)).catch(() => undefined);
  } catch (cause) {
    error.value = cause instanceof Error ? cause.message : "项目加载失败";
  }
});

onBeforeUnmount(() => {
  unmounted = true;
  agent.closeProject();
});
</script>

<template>
  <ClientOnly>
    <div v-if="ready" class="agent-chat-page">
      <AgentPanel layout="page">
        <template #topbar="{ openMemory, openHistory, openFiles, openPrefs, newSession, filesOpen, memoryOpen, historyOpen }">
          <header class="agent-chat-topbar">
            <div class="agent-chat-left">
              <NuxtLink to="/video/projects" class="agent-chat-mark" title="返回项目"><img class="aitv-mark" src="/brand/logo-mark.png" alt="tabtv" draggable="false"></NuxtLink>
              <span class="agent-chat-project" :title="projectName">{{ projectName || '未命名项目' }}</span>
              <span v-if="sessionTitle" class="agent-chat-session" :title="sessionTitle">
                <LoaderCircle v-if="agent.anyWorking" :size="12" class="spin" />
                <Sparkles v-else :size="12" />{{ sessionTitle }}
              </span>
              <button type="button" class="agent-chat-icon" title="新建会话（正在运行的会话不受影响）" aria-label="新建会话" @click="newSession()"><Plus :size="17" /></button>
            </div>
            <div class="agent-chat-right">
              <button type="button" class="agent-chat-pill" :class="{ active: filesOpen }" @click="openFiles()"><FolderOpen :size="14" />对话文件</button>
              <span class="agent-chat-sep" aria-hidden="true" />
              <button type="button" class="agent-chat-icon" title="创作偏好（本会话的模型与参数）" aria-label="创作偏好" @click="openPrefs()"><SlidersHorizontal :size="16" /></button>
              <button type="button" class="agent-chat-icon" :class="{ active: memoryOpen }" title="项目记忆" aria-label="项目记忆" @click="openMemory()"><Brain :size="16" /></button>
              <button type="button" class="agent-chat-icon" :class="{ active: historyOpen }" title="历史会话" aria-label="历史会话" @click="openHistory()">
                <History :size="16" /><i v-if="agent.anyAwaiting" class="agent-red-dot" />
              </button>
              <span class="agent-chat-sep" aria-hidden="true" />
              <NuxtLink :to="canvasPath" class="agent-chat-pill is-canvas" title="打开画布（同一会话，Agent 写入的节点都在）">画布<ArrowUpRight :size="14" /></NuxtLink>
            </div>
          </header>
        </template>
      </AgentPanel>
    </div>
    <main v-else class="canvas-loading"><span class="status-ring" /><strong>{{ error || '正在载入对话' }}</strong><NuxtLink v-if="error" to="/video/projects">返回项目</NuxtLink></main>
  </ClientOnly>
</template>

<style scoped>
/* 全屏对话页（AGT-44）：只管外壳与顶栏；消息列 / 卡片 / 输入框的样式在 AgentPanel 那一份（不另写）。 */
.agent-chat-page { width: 100vw; height: 100vh; display: flex; overflow: hidden; color: var(--canvas-text); background: var(--canvas-bg); }
.agent-chat-page :deep(.agent-panel.is-page) { flex: 1; background: var(--canvas-bg); }

.agent-chat-topbar { height: 52px; flex: none; display: flex; align-items: center; justify-content: space-between; gap: 12px; padding: 0 14px; }
.agent-chat-left, .agent-chat-right { min-width: 0; display: flex; align-items: center; gap: 6px; }
.agent-chat-right { flex: none; }
.agent-chat-mark { display: grid; place-items: center; margin-right: 4px; text-decoration: none; }
.agent-chat-project { max-width: 220px; overflow: hidden; border: 1px solid var(--canvas-line-strong); border-radius: 8px; padding: 5px 12px; color: var(--cv-n90); font-size: 13px; font-weight: 550; text-overflow: ellipsis; white-space: nowrap; }
.agent-chat-session { min-width: 0; max-width: 240px; display: inline-flex; align-items: center; gap: 4px; overflow: hidden; color: var(--canvas-muted); font-size: 12px; text-overflow: ellipsis; white-space: nowrap; }
.agent-chat-session :deep(svg) { flex: none; color: var(--canvas-cyan); }

.agent-chat-icon { position: relative; width: 30px; height: 30px; display: grid; place-items: center; border: 0; border-radius: 7px; color: var(--cv-n80); background: transparent; cursor: pointer; }
.agent-chat-icon:hover, .agent-chat-icon.active { color: var(--canvas-text); background: var(--canvas-raised); }
.agent-chat-icon .agent-red-dot { position: absolute; top: 4px; right: 4px; width: 6px; height: 6px; }

.agent-chat-sep { width: 1px; height: 14px; margin: 0 4px; background: var(--canvas-line-strong); }

.agent-chat-pill { height: 28px; display: inline-flex; align-items: center; gap: 5px; border: 0; border-radius: 7px; padding: 0 10px; color: var(--cv-n90); background: var(--canvas-raised); font: inherit; font-size: 12px; text-decoration: none; white-space: nowrap; cursor: pointer; }
.agent-chat-pill:hover, .agent-chat-pill.active { color: var(--canvas-text); background: var(--cv-tint-10, var(--canvas-raised)); }
.agent-chat-pill.is-canvas { border: 1px solid var(--canvas-line-strong); background: transparent; }
.agent-chat-pill.is-canvas:hover { border-color: var(--canvas-cyan); color: var(--canvas-cyan); }
</style>
