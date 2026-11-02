<script setup lang="ts">
import { projectTopic } from "~/utils/realtime";
import { parseNodeParam, parseSessionParam } from "~/utils/agentChat";

definePageMeta({});
const route = useRoute();
const canvas = useCanvasStore();
const config = useCanvasConfigStore();
const generation = useGenerationStore();
const realtime = useRealtimeStore();
const agent = useAgentStore();
/** 项目场景摘要（DRT-16）：节点卡判断截图过期、打开时的自愈判定都读它。 */
const directorScenes = useDirectorScenes();
/** 智能剪辑的摘要（SCT-23）：节点卡的状态与角标、下游取成片都读它。 */
const smartcuts = useSmartcuts();
const ready = ref(false);
/** `?node=`（AGT-44，从全屏对话页「在画布中查看」跳来）：画布打开后定位这些节点，只平移选中不写数据。 */
const focusNodeIds = parseNodeParam(route.query.node);
const error = ref("");
// 画布协作（AGT-11）：订 project:{id}，收到 canvas.ops_applied 本地重放；重连后按版本补取。
// ⚠️ 只在收到事件 / 重连对账时动画布，打开页面本身不写任何东西。
const releases: Array<() => void> = [];
let unmounted = false;

function watchProject(id: string) {
  releases.push(
    realtime.on("canvas.ops_applied", frame => canvas.applyRemoteOps(frame)),
    // 3D 导演台：项目场景摘要按帧更新版本与指纹（DRT-16），节点卡上的过期角标随之出现 / 消失。
    // ⚠️ 编辑器里的场景 store 自己在编辑器打开期间订这个事件（DirectorStudio）：在这里转发的话，
    //    导演台 store 连同场景校验、时间轴就成了画布页首屏的静态依赖（包体积检查，DRT-09）。
    realtime.on("director.scene_updated", frame => directorScenes.applyEvent(frame)),
    // 智能剪辑：三种事件只当信号，摘要合并重拉（剪辑台自己在打开期间另外订 timeline.updated 对账正文）
    realtime.on("timeline.updated", frame => smartcuts.applyEvent(frame)),
    realtime.on("smartcut.updated", frame => smartcuts.applyEvent(frame)),
    realtime.on("render.update", frame => smartcuts.applyEvent(frame)),
    realtime.onReconnected(() => { void canvas.fetchOpsSince(canvas.version); }),
    realtime.subscribe(projectTopic(id))
  );
  realtime.connect();
}

// 画布曾经恒定暗色（pin("dark")），理由是暗底上判断素材与视频画面的色彩更准。
// 现在改为跟随用户偏好并在顶栏给出开关：那个理由只对看片成立，而画布同时也是
// 长时间读写提示词、脚本与参数表单的地方，白天强制暗底反而更累眼。
// 需要暗底看画面时随手切回即可，比锁死更合适。pin() 仍保留给别的全屏场景用。

/**
 * Agent 面板（AGT-28）：`?session=` 来自创造页 —— 打开项目时选中这个会话并展开面板；
 * 直接打开项目时按本地记住的开合状态。
 * ⚠️ 在画布渲染**之前**定好面板开合与宽度（openProject 同步读本地偏好）：之后再开合会触发一次
 *    「保持视口中心」的平移，第一屏就看到画布跳一下。只读不写：不建会话、不改画布。
 */
function openAgent(id: string) {
  const sessionId = parseSessionParam(route.query.session);
  void agent.openProject(id, sessionId).catch(() => undefined);
  if (sessionId) agent.setPanelOpen(true);
}

onMounted(async () => {
  try {
    await Promise.all([config.load(), canvas.load(String(route.params.projectId))]);
    if (unmounted) return;
    watchProject(canvas.projectId);
    generation.open(canvas.projectId);
    openAgent(canvas.projectId);
    await generation.restore();
    ready.value = true;
  } catch (cause) {
    error.value = cause instanceof Error ? cause.message : "画布加载失败";
  }
});

onBeforeUnmount(() => {
  unmounted = true;
  // 先退订本页的 topic，再关生成通道：后者在没有任何 topic 时才断开连接
  for (const release of releases.splice(0)) release();
  agent.closeProject();
  generation.close();
});
</script>

<template>
  <ClientOnly>
    <CanvasBoard v-if="ready" :focus-node-ids="focusNodeIds" />
    <main v-else class="canvas-loading"><span class="status-ring" /><strong>{{ error || '正在加载画布' }}</strong><NuxtLink v-if="error" to="/video">返回项目</NuxtLink></main>
  </ClientOnly>
</template>
