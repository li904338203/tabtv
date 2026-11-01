<script setup lang="ts">
import type { components } from '~/types/legacy';
import type { components as ApiComponents } from "@tabtv/api-client";
import type { Connection, EdgeMouseEvent, NodeDragEvent, NodeMouseEvent, OnConnectStartParams, ViewportTransform } from "@vue-flow/core";
import { VueFlow, useVueFlow } from "@vue-flow/core";
import { Background } from "@vue-flow/background";
import {
  ChevronDown,
  CircleAlert,
  Clapperboard,
  CloudCheck,
  FileText,
  History,
  Home,
  Image as ImageIcon,
  LoaderCircle,
  Layers,
  Menu,
  MousePointer2,
  PanelLeftClose,
  PanelLeftOpen,
  Palette,
  Plus,
  Redo2,
  Scissors,
  ScrollText,
  Search,
  Sparkles,
  Undo2,
  Video,
  Volume2,
  WandSparkles,
  Zap,
  NotebookText
} from "@lucide/vue";
import { ElMessage, ElMessageBox } from "element-plus";
import BaseNode from "./BaseNode.vue";
import FrameNode from "./FrameNode.vue";
import CanvasOpsToast from "./CanvasOpsToast.vue";
import CanvasViewTools from "./CanvasViewTools.vue";
import { DOCUMENT_NODE_TYPE, classifyDocumentFailure, newDocumentPath } from "~/utils/documentNode";
import MaterialPlaza from "./MaterialPlaza.vue";
import { canvasAddMenuBadge, canvasAddMenuTypes } from "~/utils/canvasResources";
import type { CanvasNode } from "~/types/canvas";
import { markdownPlainText, nodeMenuItems, textNodeMarkdown, type NodeMenuAction } from "~/utils/canvasNodeMenu";
import { extensionFor, safeFilename, saveBlob } from "~/utils/download";
import CanvasConflictBar from "./CanvasConflictBar.vue";
import ScriptStudio from "../script-studio/ScriptStudio.vue";
import AgentPanel from "../agent/panel/AgentPanel.vue";
import { canvasShortcutsEnabled, canvasViewShortcut, isAgentPanelToggle, type CanvasViewShortcut } from "~/utils/canvasShortcuts";
import {
  CANVAS_FIT_MAX_ZOOM,
  CANVAS_GRID_SIZE,
  CANVAS_MAX_ZOOM,
  CANVAS_MIN_ZOOM,
  CANVAS_SNAP_GRID,
  readCanvasViewPrefs,
  writeCanvasViewPrefs
} from "~/utils/canvasView";
import { AGENT_PANEL_BRIDGE } from "~/utils/agentPanelBridge";
import type { GroupDetail, ScriptDetail, ScriptSourceType } from "~/types/script";
import {
  connectedNodePosition,
  connectedVideoMode,
  connectionRejection,
  connectionTargetOptions,
  nextInputOrder,
  orderedTargetEdges,
  referenceResources,
  scriptCanOpen,
  scriptCanvasReferences,
  scriptEntryMode as resolveScriptEntryMode,
  scriptReferencesForEntryMode,
  scriptRequestReferences,
  scriptTranslatedInput,
  videoModeForInputs
} from "~/utils/canvas";
import { scriptStoryInput } from "~/utils/scriptContract";
import { applyDefaultByUpstream } from "~/utils/paramLayout";
import {
  DIRECTOR_NODE_TYPE,
  type DirectorSceneLookup,
  directorFailureText,
  directorOpenPlan,
  directorSceneIdOf,
  directorUpstreamImages,
  isDirectorNotFound
} from "~/utils/directorCanvas";
import {
  directorMirrorParams,
  directorPublishPositions,
  directorPublishedNode,
  directorReplacePublished,
  type DirectorCapturedEvent,
  type DirectorPendingAction,
  type DirectorPublishEvent
} from "~/utils/directorPublish";
import {
  SMARTCUT_NODE_TYPE,
  smartcutInboundSignature,
  smartcutOpenPlan,
  smartcutTimelineIdOf,
  type SmartcutPendingAction,
  type SmartcutTimelineLookup
} from "~/utils/smartcutCanvas";
import { smartcutPublishPosition, smartcutPublishedNode, smartcutSourceOf } from "~/utils/smartcutPublish";

// ⚠️ 取生成的新契约（DRB-1）：`types/legacy` 那份是老 Java 契约的抄本，没有 `reason`。
type ValidateResponse = ApiComponents["schemas"]["CanvasConnectionValidateResponse"];
type MaterialItem = ApiComponents["schemas"]["MaterialItem"];
type NodeTypeConfig = components["schemas"]["NodeTypeResp"];
type ConnectionTarget = {
  nodeType: NodeTypeConfig;
  allowed: boolean;
  operation?: string;
};
type ConnectionOrigin = {
  sourceId: string;
  sourceHandle: string;
};
type ConnectionMenuState = ConnectionOrigin & {
  sourceType: string;
  flowPoint: { x: number; y: number };
  x: number;
  y: number;
};
type PendingConnectionLine = {
  startX: number;
  startY: number;
  endX: number;
  endY: number;
};
type ScriptStudioAction = "storyboard" | "video";
const canvas = useCanvasStore();
const config = useCanvasConfigStore();
// 顶栏不再单独放主题开关与保存按钮（2026-11-01 按用户要求）：主题在头像悬停面板的「模式切换」里，
// 画布本来就自动保存（项目名旁的保存状态照常显示）。这里只读当前主题给点阵底纹配色。
const { resolved: theme } = useTheme();
// 点阵底纹的颜色是 VueFlow 的组件 prop、不走 CSS，canvas.css 的 token 覆盖不到它，
// 只能在这里跟着主题切。漏了的话亮色下就是深灰点打在白底上，糊得很明显。
const dotColor = computed(() => (theme.value === "dark" ? "#3b3d42" : "#d7d8dc"));
const generation = useGenerationStore();
const scripts = useScriptApi();
const sidebarOpen = ref(true);
const sidebarTab = ref<"layers" | "assets">("layers");
const addMenuOpen = ref(false);
const assetPlazaOpen = ref(false);
const assetType = ref<"style" | "effect">("style");
/** 画布左侧「资产」页签 = 我的资产（保存过的生成产物）。按需加载：画布页首屏包体积有上限。 */
const AssetLibraryPanel = defineAsyncComponent(() => import("./AssetLibraryPanel.vue"));
/** 资产库弹窗（资产页签的「管理」）：生成历史、我保存的、我上传的。按需加载。 */
const AssetLibraryDialog = defineAsyncComponent(() => import("./AssetLibraryDialog.vue"));
const assetDialogOpen = ref(false);
const renaming = ref(false);
const renameDraft = ref("");
const renameInput = ref<HTMLInputElement>();
const search = ref("");
const stageRef = ref<HTMLElement>();
const connectionMenuRef = ref<HTMLElement>();
const addMenuRef = ref<HTMLElement>();
const connectionMenu = ref<ConnectionMenuState>();
const pendingConnectionLine = ref<PendingConnectionLine>();
const creatingConnectedType = ref("");
/** 拖线进行中。连接加号平时只在选中节点上可见，拖线时要让所有节点的锚点露出来当靶子。 */
const connecting = ref(false);
const scriptStudioNodeId = ref<string | null>(null);
/**
 * 工作台打开那一刻就把脚本 ID 定下来，不要每次渲染都回画布节点上去取：
 * 节点被删 / 被替换的话，工作台会拿着空 ID 继续发请求（「scriptID 非法」，CD-78）。
 */
const scriptStudioScriptId = ref("");
const scriptStudioAction = ref<ScriptStudioAction>();
/**
 * 3D 导演台编辑器（DRT-16）。⚠️ **只此一处引用**，且只能是异步组件（frontend/director/README.md）：
 * 写成 `<DirectorStudio>` / `<LazyDirectorStudio>` 标签的话 three 会进画布首屏的包（DRT-09 的包体积检查会红）。
 */
const DirectorStudio = defineAsyncComponent(() => import("~/components/director/DirectorStudio.vue"));
const directorStudioNodeId = ref<string | null>(null);
/** 打开那一刻就把场景 ID 定下来（同 scriptStudioScriptId，CD-78）：节点被删 / 被替换时编辑器不会拿着空 ID 发请求。 */
const directorStudioSceneId = ref("");
/** 打开后要编辑器顺手做的事（重新截图，DRT-17）；关掉时清空。 */
const directorStudioPendingAction = ref<DirectorPendingAction>();
/**
 * 智能剪辑的剪辑台（SCT-23，SCD-41）。⚠️ 同导演台：**只此一处引用**，只能是异步组件 —— 写成标签的话
 * 剪辑台（时间线、预览引擎、面板）会进画布首屏的包（包体积检查会红）。
 */
const SmartcutStudio = defineAsyncComponent(() => import("~/components/smartcut/SmartcutStudio.vue"));
const smartcutStudioNodeId = ref<string | null>(null);
/** 打开那一刻就把时间线 ID 定下来：节点被删 / 被替换时剪辑台不会拿着空 ID 发请求。 */
const smartcutStudioTimelineId = ref("");
/** 打开后要剪辑台顺手做的事（AI 剪辑、追加没用上的素材）；关掉时清空。 */
const smartcutStudioPendingAction = ref<SmartcutPendingAction>();
let lastPaneClickAt = Number.NEGATIVE_INFINITY;
let connectionOrigin: ConnectionOrigin | undefined;
let connectionCompleted = false;
let connectionMenuOpenedAt = Number.NEGATIVE_INFINITY;
const props = defineProps<{
  /**
   * `?node=`（AGT-44）：打开画布后定位到这些节点（从全屏对话页「在画布中查看」跳过来）。
   * ⚠️ 只平移 + 选中 + 闪烁，**不写数据**：这次定位引起的视口变化不触发画布保存（打开页面不该写东西）。
   */
  focusNodeIds?: string[];
}>();
const { fitView: fitViewUncapped, zoomIn, zoomOut, zoomTo, getNode, getSelectedNodes, addSelectedNodes, removeSelectedElements, screenToFlowCoordinate, viewport: flowViewport, setViewport, onNodesInitialized } = useVueFlow();
/**
 * 定位到节点 / 适应画布最多放大到 180%（CANVAS_FIT_MAX_ZOOM）：画布本身能放大到 800%（左下角的缩放菜单），
 * 而 fitView 不给上限时用的是画布的最大缩放 —— 定位一个小节点会把它放大到糊满整屏。调用方自己给了上限的照它的。
 */
const fitView: typeof fitViewUncapped = params => fitViewUncapped({ maxZoom: CANVAS_FIT_MAX_ZOOM, ...params });
/** 小地图（2026-11-01 起默认收起，在左下角工具条上开）：用到才加载，不进画布首屏的包。 */
const MiniMap = defineAsyncComponent(() => import("@vue-flow/minimap").then(module => module.MiniMap));
/**
 * 左下角工具条的开关（2026-11-01）：小地图、网格吸附记在本机，下次打开还是这样；
 * 隐藏连线只管这一次 —— 记住的话下次打开画布连线全不见了，看上去像是数据丢了。
 */
const viewPrefs = ref(readCanvasViewPrefs(import.meta.client ? localStorage : undefined));
watch(viewPrefs, prefs => writeCanvasViewPrefs(import.meta.client ? localStorage : undefined, prefs), { deep: true });
const edgesHidden = ref(false);
// 多选同步进 store（AGT-07）：框选 / Shift 多选都以 Vue Flow 的选择为准。`selectedNodeId` 仍由 selectCanvasNode 写。
watch(() => getSelectedNodes.value.map(node => node.id), (ids) => {
  if (ids.length === canvas.selectedNodeIds.length && ids.every((id, index) => id === canvas.selectedNodeIds[index])) return;
  canvas.selectedNodeIds = ids;
});
const filteredNodes = computed(() => canvas.nodes.filter(node => node.data.title.toLowerCase().includes(search.value.toLowerCase())));
const nodeIcons = { text: FileText, image: ImageIcon, video: Video, audio: Volume2, script: ScrollText, group: Layers, style: WandSparkles, effect: Sparkles, document: NotebookText, director: Clapperboard, smartcut: Scissors };
/**
 * 空画布的快捷入口（2026-11-01 按参考图）：一排「图标 + 名字」的小卡片，点了直接在画布上建对应的节点 ——
 * 与加号菜单同一个 addNode（智能剪辑这类要先建后台对象的节点照样走它自己的流程）。图标与左侧节点列表同一套（nodeIcons）。
 * 此前这里是四个带封面图的工作流模板（故事脚本生成 / 角色三视图 / 首帧图生视频 / 音频生视频），按用户要求换掉。
 * 只列配置里真有的节点类型：后台关掉的不出现，免得点了没反应。
 */
const QUICK_NODE_ENTRIES = [
  { code: "image", label: "图片生成" },
  { code: "video", label: "视频生成" },
  { code: "audio", label: "音频生成" },
  { code: "script", label: "剧本生成" },
  { code: SMARTCUT_NODE_TYPE, label: "智能剪辑" }
] as const;
const quickNodes = computed(() => QUICK_NODE_ENTRIES
  .filter(entry => config.nodeType(entry.code))
  .map(entry => ({ ...entry, icon: nodeIcons[entry.code] })));
const saveLabel = computed(() => ({ saved: "已保存", saving: "保存中", conflict: "版本冲突", error: "保存失败" })[canvas.saveState]);
const connectionTargets = computed<ConnectionTarget[]>(() => connectionTargetOptions(
  connectionMenu.value?.sourceType ?? "",
  config.config?.nodeTypes ?? [],
  config.config?.connectionRules ?? []
));
const pendingConnectionPath = computed(() => {
  const line = pendingConnectionLine.value;
  if (!line) return "";
  const direction = line.endX >= line.startX ? 1 : -1;
  const bend = Math.max(52, Math.abs(line.endX - line.startX) * 0.48);
  return `M ${line.startX} ${line.startY} C ${line.startX + direction * bend} ${line.startY}, ${line.endX - direction * Math.min(bend, 84)} ${line.endY}, ${line.endX} ${line.endY}`;
});

async function startRename() {
  renameDraft.value = canvas.projectName;
  renaming.value = true;
  await nextTick();
  // 全选而不是把光标放末尾：默认名「未命名」几乎总是要整个替换掉
  renameInput.value?.select();
}

async function commitRename() {
  if (!renaming.value) return;
  const next = renameDraft.value.trim();
  renaming.value = false;
  if (!next || next === canvas.projectName) return;
  if (next.length > 100) {
    ElMessage.error("项目名不能超过 100 个字符");
    return;
  }
  try {
    await canvas.rename(next);
  } catch {
    ElMessage.error("项目重命名失败");
  }
}

function cancelRename() {
  renaming.value = false;
}

function defaultParams(type: NodeTypeConfig) {
  return Object.fromEntries(
    (type.params ?? [])
      .filter(param => param.code && param.defaultValue !== undefined)
      .map(param => [param.code, param.defaultValue])
  );
}

async function validateConnection(targetType: string, mode: unknown, inputs: Array<{ sourceType: string; order: number }>) {
  return useApi()<ValidateResponse>("/canvas/connections/validate", {
    method: "POST",
    body: {
      targetType,
      mode: typeof mode === "string" ? mode : undefined,
      inputs
    }
  });
}

async function connect(connection: Connection) {
  connectionCompleted = true;
  if (!connection.source || !connection.target) return;
  const source = canvas.nodes.find(node => node.id === connection.source);
  const target = canvas.nodes.find(node => node.id === connection.target);
  if (!source || !target) return;
  const existing = orderedTargetEdges(canvas.edges, target.id);
  const order = nextInputOrder(canvas.edges, target.id);
  const inputs = [...existing.map(edge => ({
    sourceType: canvas.nodes.find(node => node.id === edge.source)?.data.nodeType ?? "text",
    order: edge.data?.order ?? 1
  })), { sourceType: source.data.nodeType, order }];
  const currentMode = String(target.data.params.mode || "text2video");
  // 节点类型原样交给 videoModeForInputs：它先按媒体角色归一（导演台按图片算，DRT-16）再过滤。
  // ⚠️ 别在这里先按 text/image/video/audio 过滤 —— 那样导演台上游在归一之前就被丢掉了
  const mode = target.data.nodeType === "video"
    ? videoModeForInputs(currentMode, inputs.map(input => input.sourceType))
    : target.data.params.mode;
  try {
    const validated = await validateConnection(target.data.nodeType, mode, inputs);
    // DRB-1：服务端说不合规则时不落这条线（`valid:false` 是 HTTP 200，不会进 catch）
    const rejection = connectionRejection(validated);
    if (rejection) {
      ElMessage.error(rejection);
      return;
    }
    canvas.snapshot();
    if (target.data.nodeType === "video" && mode !== currentMode) {
      canvas.updateParams(target.id, { ...target.data.params, mode });
    }
    if (target.data.nodeType === "script"
      && source.data.nodeType === "image"
      && !target.data.params.scriptId
      && !target.data.params.sourceType) {
      target.data.title = "脚本生成器";
      canvas.updateParams(target.id, {
        ...target.data.params,
        sourceType: "character",
        scriptEntryMode: "character_reference",
        scriptEntryInitialized: true,
        scriptStatus: "editing"
      });
    }
    // order 用校验前算好的那个：校验请求按它报给服务端，落边必须与之一致
    canvas.addEdge(connection.source, connection.target, {
      order,
      operation: validated.operation,
      sourceHandle: connection.sourceHandle ?? "out",
      targetHandle: connection.targetHandle ?? "in",
      recordHistory: false
    });
  } catch (cause) {
    ElMessage.error(cause instanceof Error ? cause.message : "此连接不受支持");
  }
}

function onConnectStart(params: OnConnectStartParams) {
  connecting.value = true;
  connectionCompleted = false;
  connectionOrigin = params.handleType === "source" && params.nodeId
    ? { sourceId: params.nodeId, sourceHandle: params.handleId ?? "out" }
    : undefined;
  dismissConnectionMenu();
  addMenuOpen.value = false;
}

function eventPoint(event: MouseEvent | TouchEvent) {
  if ("changedTouches" in event) {
    const touch = event.changedTouches[0];
    return touch ? { x: touch.clientX, y: touch.clientY } : undefined;
  }
  return { x: event.clientX, y: event.clientY };
}

async function clampConnectionMenu() {
  await nextTick();
  const state = connectionMenu.value;
  const stage = stageRef.value;
  const menu = connectionMenuRef.value;
  if (!state || !stage || !menu) return;
  const stageRect = stage.getBoundingClientRect();
  const menuRect = menu.getBoundingClientRect();
  state.x = Math.min(Math.max(12, state.x), Math.max(12, stageRect.width - menuRect.width - 12));
  state.y = Math.min(Math.max(12, state.y), Math.max(12, stageRect.height - menuRect.height - 12));
  await nextTick();
  updatePendingConnectionLine();
}

function updatePendingConnectionLine() {
  const state = connectionMenu.value;
  const stage = stageRef.value;
  const menu = connectionMenuRef.value;
  if (!state || !stage || !menu) {
    pendingConnectionLine.value = undefined;
    return;
  }
  const sourceNode = Array.from(stage.querySelectorAll<HTMLElement>(".vue-flow__node"))
    .find(node => node.dataset.id === state.sourceId);
  const sourceHandle = sourceNode?.querySelector<HTMLElement>(".vue-flow__handle.source, .vue-flow__handle-right");
  const sourceFrame = sourceNode?.querySelector<HTMLElement>(".node-preview-frame");
  const sourceRect = sourceHandle?.getBoundingClientRect() ?? sourceFrame?.getBoundingClientRect();
  if (!sourceRect) {
    pendingConnectionLine.value = undefined;
    return;
  }
  const stageRect = stage.getBoundingClientRect();
  const menuRect = menu.getBoundingClientRect();
  const startX = sourceHandle
    ? sourceRect.left + sourceRect.width / 2 - stageRect.left
    : sourceRect.right - stageRect.left;
  const menuCenterX = menuRect.left + menuRect.width / 2 - stageRect.left;
  pendingConnectionLine.value = {
    startX,
    startY: sourceRect.top + sourceRect.height / 2 - stageRect.top,
    endX: startX <= menuCenterX ? menuRect.left - stageRect.left : menuRect.right - stageRect.left,
    endY: Math.min(menuRect.bottom - 12, menuRect.top + 46) - stageRect.top
  };
}

function dismissConnectionMenu() {
  connectionMenu.value = undefined;
  pendingConnectionLine.value = undefined;
}

function onConnectEnd(event?: MouseEvent | TouchEvent) {
  // 放在最前面：下面有多个提前 return，漏掉任何一条都会让加号一直亮着
  connecting.value = false;
  const origin = connectionOrigin;
  connectionOrigin = undefined;
  if (connectionCompleted || !origin || !event) {
    connectionCompleted = false;
    return;
  }
  connectionCompleted = false;
  const eventTarget = event.target;
  if (eventTarget instanceof Element && eventTarget.closest(".vue-flow__node")) return;
  const point = eventPoint(event);
  const stage = stageRef.value;
  const source = canvas.nodes.find(node => node.id === origin.sourceId);
  if (!point || !stage || !source) return;
  const stageRect = stage.getBoundingClientRect();
  connectionMenu.value = {
    ...origin,
    sourceType: source.data.nodeType,
    flowPoint: screenToFlowCoordinate(point),
    x: point.x - stageRect.left + 16,
    y: point.y - stageRect.top - 24
  };
  connectionMenuOpenedAt = performance.now();
  void clampConnectionMenu();
}

async function createConnectedNode(option: ConnectionTarget) {
  const menu = connectionMenu.value;
  const code = option.nodeType.code;
  const source = menu ? canvas.nodes.find(node => node.id === menu.sourceId) : undefined;
  if (!menu || !source || !code || !option.allowed || creatingConnectedType.value) return;
  creatingConnectedType.value = code;
  let params: Record<string, unknown> = defaultParams(option.nodeType);
  const mode = connectedVideoMode(code, option.operation, source.data.nodeType, source.data.params);
  if (mode) params.mode = mode;
  // 按上游类型的默认参数（DRD-45）：从导演台拖出的图片节点默认「按白模构图出图」、视频节点默认全能参考。
  // 值都来自配置；放在最后，配置的默认压过按连线规则推出来的模式。只用于新建，已有节点连上时只给建议条。
  params = applyDefaultByUpstream(params, option.nodeType, source.data.nodeType, config.imagePresets);
  try {
    const validated = await validateConnection(code, params.mode, [{ sourceType: source.data.nodeType, order: 1 }]);
    // DRB-1：不合规则时连节点都不建 —— 建出来就是一个连不上上游、生成必失败的节点
    const rejection = connectionRejection(validated);
    if (rejection) {
      ElMessage.error(rejection);
      return;
    }
    canvas.snapshot();
    const nodeId = canvas.addNode(code, option.nodeType.name ?? code, params, {
      position: connectedNodePosition(menu.flowPoint),
      recordHistory: false,
      persist: false
    });
    canvas.addEdge(source.id, nodeId, {
      order: 1,
      operation: validated.operation,
      sourceHandle: menu.sourceHandle,
      targetHandle: "in",
      recordHistory: false,
      persist: false
    });
    dismissConnectionMenu();
    canvas.scheduleSave();
    await nextTick();
    selectCanvasNode(nodeId);
  } catch (cause) {
    ElMessage.error(cause instanceof Error ? cause.message : "无法创建此引用节点");
  } finally {
    creatingConnectedType.value = "";
  }
}

/** 加节点菜单列的节点（组 / 风格 / 特效 / 文档不在菜单里，见 utils/canvasResources）。 */
const addMenuTypes = computed(() => canvasAddMenuTypes(config.config?.nodeTypes));
/** 「从生成历史选择」点了才加载（画布页首屏包体积有上限，见 scripts/director-bundle-check.mjs）。 */
const GenerationHistoryPicker = defineAsyncComponent(() => import("./GenerationHistoryPicker.vue"));
const historyPickerOpen = ref(false);

/** 「添加资源」与生成历史弹窗都是按需加载的，建节点的逻辑在它们自己那里（useCanvasResourceNodes）；画布只负责选中与对焦。 */
const CanvasAddResources = defineAsyncComponent(() => import("./CanvasAddResources.vue"));
/**
 * 顶栏的 ⚡ 积分与头像：悬停弹出与门户顶栏同一个面板（2026-11-01，AppAccountPanel，⚡ 用它的 `variant="credits"`）。
 * 按需加载：画布页首屏包体积有上限（scripts/director-bundle-check.mjs），这两块到不了首屏也不影响画布。
 */
const AccountPanel = defineAsyncComponent(() => import("~/components/AppAccountPanel.vue"));
function onResourcesCreated(ids: string[]) {
  addMenuOpen.value = false;
  if (!ids.length) return;
  selectCanvasNode(ids[0]!);
  void fitView({ nodes: ids, duration: 300, padding: 0.35 });
}
function openHistoryPicker() {
  addMenuOpen.value = false;
  historyPickerOpen.value = true;
}

function addNode(code: string) {
  const type = config.nodeType(code);
  if (!type) return;
  if (code === "style" || code === "effect") {
    openAssetPlaza(code);
    return;
  }
  if (code === DOCUMENT_NODE_TYPE) {
    void addDocumentNode(type.name ?? "文档");
    return;
  }
  if (code === DIRECTOR_NODE_TYPE) {
    void addDirectorNode(type.name ?? "3D 导演台");
    return;
  }
  if (code === SMARTCUT_NODE_TYPE) {
    void addSmartcutNode(type);
    return;
  }
  const defaults = defaultParams(type);
  const nodeId = canvas.addNode(code, type.name ?? code, defaults);
  addMenuOpen.value = false;
  void nextTick(() => {
    selectCanvasNode(nodeId);
    return fitView({ nodes: [nodeId], duration: 300, padding: 0.35 });
  });
}

/**
 * 文档节点（AGT-09）：先建文档、再加节点，节点上**只存 docId** —— 正文不进 canvasData。
 *
 * ⚠️ 顺序不能反：先加节点再建文档的话，建文档失败会留下一个指向空气的节点，
 *    而它已经被自动保存进画布了。删节点不删文档（Agent 可能还引用它）。
 */
const creatingDocument = ref(false);
async function addDocumentNode(title: string) {
  if (creatingDocument.value || !canvas.projectId) return;
  creatingDocument.value = true;
  try {
    const doc = await useApi()<{ id: string }>(`/projects/${canvas.projectId}/documents`, {
      method: "POST",
      body: { path: newDocumentPath(), content: "" }
    });
    const nodeId = canvas.addNode(DOCUMENT_NODE_TYPE, title, { docId: doc.id });
    addMenuOpen.value = false;
    void nextTick(() => {
      selectCanvasNode(nodeId);
      return fitView({ nodes: [nodeId], duration: 300, padding: 0.35 });
    });
  } catch (cause) {
    ElMessage.error(classifyDocumentFailure(cause).message || "新建文档失败");
  } finally {
    creatingDocument.value = false;
  }
}

// ---------------------------------------------------------------- 3D 导演台（DRT-16，dev/03 §6.2~6.3）

const directorScenes = useDirectorScenes();
const directorBuildConfig = useDirectorBuildConfig();
/**
 * 画布上有导演台（或从导演台发布出来的节点）时才拉场景摘要与 AI 搭景的可用性（DRT-20）：
 * 只用画布的人不必白付两次请求。
 */
const hasDirectorContent = computed(() => canvas.nodes.some(node => node.data.nodeType === DIRECTOR_NODE_TYPE
  || Boolean(node.data.params?.directorSource)));
watch([hasDirectorContent, () => canvas.projectId], ([has, projectId]) => {
  if (!has || !projectId) return;
  void directorScenes.load(projectId);
  void directorBuildConfig.load();
}, { immediate: true });

/**
 * 新建导演台：先生成节点 ID、建场景，再加节点（同文档节点）。
 *
 * ⚠️ 顺序不能反：先加节点再建场景的话，建场景失败会留下一个指向空气的节点，而它已经被自动保存进画布了。
 *    场景按（项目，画布节点）绑定（DRD-17），所以节点 ID 要在建场景之前定下来。
 */
const creatingDirector = ref(false);
async function addDirectorNode(title: string) {
  if (creatingDirector.value || !canvas.projectId) return;
  creatingDirector.value = true;
  try {
    const nodeId = canvas.newNodeId();
    const scene = await directorScenes.create(canvas.projectId, nodeId);
    canvas.addNode(DIRECTOR_NODE_TYPE, title, { sceneId: scene.id }, { id: nodeId });
    addMenuOpen.value = false;
    void nextTick(() => {
      selectCanvasNode(nodeId);
      return fitView({ nodes: [nodeId], duration: 300, padding: 0.35 });
    });
  } catch (cause) {
    ElMessage.error(directorFailureText(cause, "新建导演台失败，请检查网络后重试"));
  } finally {
    creatingDirector.value = false;
  }
}

/** 复制节点（⌘D 与节点上的复制按钮共用）：导演台与智能剪辑复制出来立刻分叉出自己的场景 / 时间线（DR-F-07、SC-F-07）。 */
function duplicateCanvasNode(nodeId: string) {
  const source = canvas.nodes.find(node => node.id === nodeId);
  const copyId = canvas.duplicateNode(nodeId);
  if (!copyId) return;
  // 文本节点：副本不带生成结果（store 的规则），把卡片上的内容落成手写文本，否则副本是空的
  if (source?.data.nodeType === "text" && source.data.generation?.outputText !== undefined) {
    canvas.updateParams(copyId, textCopyParams(source) as CanvasNode["data"]["params"]);
  }
  void forkDirectorCopy(copyId);
  void forkSmartcutCopy(copyId);
}

/**
 * 复制即分叉（DRD-43 第一道）：副本的 params 是深拷贝，与原节点共用同一个 `sceneId`，改一个另一个跟着变。
 * 失败只提示、节点保留原 `sceneId` —— 打开时由 `openDirectorStudio` 再分叉一次（第二道）。
 */
async function forkDirectorCopy(nodeId: string) {
  const node = canvas.nodes.find(item => item.id === nodeId);
  if (node?.data.nodeType !== DIRECTOR_NODE_TYPE || !canvas.projectId) return;
  const sourceSceneId = directorSceneIdOf(node.data.params);
  if (!sourceSceneId) return;
  try {
    const scene = await directorScenes.create(canvas.projectId, nodeId, sourceSceneId);
    // 分叉期间副本可能已被撤销 / 删除：那就什么都不写（场景留着无害，随项目清理）
    const current = canvas.nodes.find(item => item.id === nodeId);
    if (current) canvas.updateParams(nodeId, { ...current.data.params, sceneId: scene.id });
  } catch {
    ElMessage.warning("复制的导演台还没有独立的场景，打开时会自动补上");
  }
}

// ---------------------------------------------------------------- 智能剪辑（SCT-23，dev/03 §8）

const smartcuts = useSmartcuts();
/** 画布上有智能剪辑节点时才拉摘要（节点卡、下游取输入、自愈都看它）。 */
const hasSmartcut = computed(() => canvas.nodes.some(node => node.data.nodeType === SMARTCUT_NODE_TYPE));
watch([hasSmartcut, () => canvas.projectId], ([has, projectId]) => {
  if (has && projectId) void smartcuts.load(projectId);
}, { immediate: true });

/**
 * 新建智能剪辑：先生成节点 ID、建时间线（三条默认轨，按项目 + 画布节点幂等），再加节点。
 * ⚠️ 顺序不能反（同导演台）：先加节点再建时间线的话，建失败会留下一个指向空气的节点，而它已经被自动保存进画布了。
 */
const creatingSmartcut = ref(false);
async function addSmartcutNode(type: NodeTypeConfig) {
  if (creatingSmartcut.value || !canvas.projectId) return;
  creatingSmartcut.value = true;
  try {
    const nodeId = canvas.newNodeId();
    const summary = await smartcuts.create(canvas.projectId, nodeId);
    canvas.addNode(SMARTCUT_NODE_TYPE, type.name ?? "智能剪辑", { ...defaultParams(type), timelineId: summary.timelineId }, { id: nodeId });
    addMenuOpen.value = false;
    void nextTick(() => {
      selectCanvasNode(nodeId);
      return fitView({ nodes: [nodeId], duration: 300, padding: 0.35 });
    });
  } catch (cause) {
    ElMessage.error(directorFailureText(cause, "新建智能剪辑失败，请检查网络后重试"));
  } finally {
    creatingSmartcut.value = false;
  }
}

/**
 * 复制即分叉（SC-F-07 第一道）：副本的 params 是深拷贝，与原节点共用同一个 `timelineId`，改一个另一个跟着变。
 * 失败只提示、节点保留原 `timelineId` —— 打开时由 `openSmartcutStudio` 再分叉一次（第二道）。
 */
async function forkSmartcutCopy(nodeId: string) {
  const node = canvas.nodes.find(item => item.id === nodeId);
  if (node?.data.nodeType !== SMARTCUT_NODE_TYPE || !canvas.projectId) return;
  const sourceTimelineId = smartcutTimelineIdOf(node.data.params);
  if (!sourceTimelineId) return;
  try {
    const summary = await smartcuts.create(canvas.projectId, nodeId, sourceTimelineId);
    const current = canvas.nodes.find(item => item.id === nodeId);
    if (current) canvas.updateParams(nodeId, { ...current.data.params, timelineId: summary.timelineId });
  } catch {
    ElMessage.warning("复制的智能剪辑还没有独立的时间线，打开时会自动补上");
  }
}

function bindSmartcutTimeline(nodeId: string, timelineId: string) {
  const node = canvas.nodes.find(item => item.id === nodeId);
  if (node && smartcutTimelineIdOf(node.data.params) !== timelineId) {
    canvas.updateParams(nodeId, { ...node.data.params, timelineId });
  }
}

/** 打开剪辑台（判定在纯函数 `smartcutOpenPlan` 里；这里只负责查摘要、发请求、写回）。 */
const openingSmartcut = ref(false);
async function openSmartcutStudio(nodeId: string, action?: SmartcutPendingAction) {
  const node = canvas.nodes.find(item => item.id === nodeId);
  if (node?.data.nodeType !== SMARTCUT_NODE_TYPE || openingSmartcut.value || !canvas.projectId) return;
  const projectId = canvas.projectId;
  openingSmartcut.value = true;
  try {
    const timelineId = smartcutTimelineIdOf(node.data.params);
    let lookup: SmartcutTimelineLookup | null = null;
    if (timelineId) {
      let summary = smartcuts.byTimeline(timelineId);
      if (!summary) {
        await smartcuts.refresh(projectId);
        summary = smartcuts.byTimeline(timelineId);
      }
      lookup = summary ? { status: "found", canvasNodeId: summary.canvasNodeId } : { status: "missing" };
    }
    const plan = smartcutOpenPlan(node, lookup);
    let openTimelineId = plan.action === "open" ? plan.timelineId : "";
    if (plan.action === "create") {
      openTimelineId = (await smartcuts.create(projectId, nodeId)).timelineId;
    } else if (plan.action === "fork") {
      openTimelineId = (await smartcuts.create(projectId, nodeId, plan.sourceTimelineId)).timelineId;
    } else if (plan.action === "recreate") {
      try {
        openTimelineId = (await smartcuts.create(projectId, nodeId, plan.sourceTimelineId)).timelineId;
      } catch (cause) {
        if (!isDirectorNotFound(cause)) throw cause;
        openTimelineId = (await smartcuts.create(projectId, nodeId)).timelineId;
        ElMessage.info(plan.notice);
      }
    }
    if (plan.action !== "open") bindSmartcutTimeline(nodeId, openTimelineId);
    smartcutStudioTimelineId.value = openTimelineId;
    smartcutStudioPendingAction.value = action;
    smartcutStudioNodeId.value = nodeId;
  } catch (cause) {
    ElMessage.error(directorFailureText(cause, "打开智能剪辑失败，请检查网络后重试"));
  } finally {
    openingSmartcut.value = false;
  }
}

/** 收起与关闭都卸载剪辑台（收起时 store 保留，再打开秒开，SCD-52）；之后对一次账，节点卡上的状态才准。 */
function leaveSmartcutStudio() {
  const nodeId = smartcutStudioNodeId.value;
  smartcutStudioNodeId.value = null;
  smartcutStudioTimelineId.value = "";
  smartcutStudioPendingAction.value = undefined;
  if (canvas.projectId) void smartcuts.refresh(canvas.projectId).catch(() => undefined);
  // 焦点回到节点卡（SCD-53）
  if (nodeId) void nextTick(() => document.querySelector<HTMLElement>(`.vue-flow__node[data-id="${CSS.escape(nodeId)}"] [data-testid="smartcut-open"]`)?.focus());
}
/**
 * 成片发布到画布（SCD-51，照 onDirectorPublish）：在智能剪辑节点右侧建一个带来源的视频节点（已有产出、没有生成 ID），
 * 不自动连线。一次撤销撤掉；建好当场取预览 —— `restore()` 只在页面挂载时跑过，不取的话新节点直到刷新都是空的。
 * ⚠️ `updateGeneration` 不排保存：最后必须 `scheduleSave`，否则发布节点的产出不落库，刷新就丢。
 */
async function onSmartcutPublish(render: ApiComponents["schemas"]["RenderJob"]) {
  const source = canvas.nodes.find(node => node.id === smartcutStudioNodeId.value);
  const published = smartcutPublishedNode(render, { timelineId: smartcutStudioTimelineId.value, title: source?.data.title });
  if (!published) return;
  const already = canvas.nodes.filter(node => smartcutSourceOf(node.data.params)?.timelineId === smartcutStudioTimelineId.value).length;
  canvas.snapshot();
  const id = canvas.addNode(published.nodeType, published.title, published.params, {
    position: smartcutPublishPosition(source?.position ?? { x: 480, y: 220 }, already), exactTitle: true, recordHistory: false, persist: false
  });
  canvas.updateGeneration(id, published.generation);
  canvas.scheduleSave();
  ElMessage.success("成片已发布到画布");
  await nextTick();
  const node = canvas.nodes.find(item => item.id === id);
  if (node) void generation.restoreNode(node).catch(() => undefined);
}
const smartcutStudioTitle = computed(() => canvas.nodes.find(node => node.id === smartcutStudioNodeId.value)?.data.title ?? "智能剪辑");

/**
 * 同步时机（SCD-49）：画布保存成功后，连线变了的智能剪辑节点按已保存的画布同步一次（没剪过时把新素材铺上轨道）；
 * 上游的生成进入终态时也同步（节流 2 秒）。剪辑台开着的那个节点只更新素材条（`listOnly`，由剪辑台自己做）。
 */
const syncedSignatures = new Map<string, string>();
let syncTimer: ReturnType<typeof setTimeout> | undefined;
function syncSmartcuts(force = false) {
  const projectId = canvas.projectId;
  if (!projectId || !hasSmartcut.value) return;
  for (const node of canvas.nodes) {
    if (node.data.nodeType !== SMARTCUT_NODE_TYPE || node.id === smartcutStudioNodeId.value) continue;
    const signature = smartcutInboundSignature(canvas.edges, node.id);
    if (!force && syncedSignatures.get(node.id) === signature) continue;
    syncedSignatures.set(node.id, signature);
    void smartcuts.sync(projectId, node.id, "autofill").catch(() => undefined);
  }
}
watch(() => canvas.saveState, (state, prev) => {
  if (state === "saved" && prev === "saving") syncSmartcuts();
});
/** 上游生成状态的签名：只看连进智能剪辑的那些节点。 */
const smartcutUpstreamStatus = computed(() => canvas.edges
  .filter(edge => canvas.nodes.some(node => node.id === edge.target && node.data.nodeType === SMARTCUT_NODE_TYPE))
  .map((edge) => {
    const g = canvas.nodes.find(node => node.id === edge.source)?.data.generation;
    return `${edge.source}:${g?.id ?? ""}:${g?.status ?? ""}`;
  })
  .join("|"));
watch(smartcutUpstreamStatus, (next, prev) => {
  if (!prev || next === prev) return;
  clearTimeout(syncTimer);
  syncTimer = setTimeout(() => syncSmartcuts(true), 2000);
});

/** 把节点指向新建 / 分叉出来的场景（自愈写回）。不进撤销栈：这是修复，不是用户的改动。 */
function bindDirectorScene(nodeId: string, sceneId: string) {
  const node = canvas.nodes.find(item => item.id === nodeId);
  if (node && directorSceneIdOf(node.data.params) !== sceneId) {
    canvas.updateParams(nodeId, { ...node.data.params, sceneId });
  }
}

/**
 * 打开导演台（dev/03 §6.3）：先判定节点与场景是不是一一对应，不是就先修好再打开（DRD-43 第二道）。
 * 判定在纯函数 `directorOpenPlan` 里；这里只负责查摘要、发请求、写回。
 */
const openingDirector = ref(false);
async function openDirectorStudio(nodeId: string, action?: DirectorPendingAction) {
  const node = canvas.nodes.find(item => item.id === nodeId);
  if (node?.data.nodeType !== DIRECTOR_NODE_TYPE || openingDirector.value || !canvas.projectId) return;
  const projectId = canvas.projectId;
  openingDirector.value = true;
  selectCanvasNode(nodeId);
  try {
    const sceneId = directorSceneIdOf(node.data.params);
    let lookup: DirectorSceneLookup | null = null;
    if (sceneId) {
      // 摘要里有就不再请求（场景的 canvasNodeId 建了就不会变）；没有才重新拉一次再下结论
      let summary = directorScenes.summaries.value[sceneId];
      if (!summary) {
        await directorScenes.refresh(projectId);
        summary = directorScenes.summaries.value[sceneId];
      }
      lookup = summary ? { status: "found", canvasNodeId: summary.canvasNodeId } : { status: "missing" };
    }
    const plan = directorOpenPlan(node, lookup);
    let openSceneId = "";
    if (plan.action === "open") {
      openSceneId = plan.sceneId;
    } else if (plan.action === "create") {
      openSceneId = (await directorScenes.create(projectId, nodeId)).id;
    } else if (plan.action === "fork") {
      openSceneId = (await directorScenes.create(projectId, nodeId, plan.sourceSceneId)).id;
    } else {
      // 场景不在本项目：同一工作区的别的项目能复制（契约 createDirectorScene），复制不了才新建空场景
      try {
        openSceneId = (await directorScenes.create(projectId, nodeId, plan.sourceSceneId)).id;
      } catch (cause) {
        if (!isDirectorNotFound(cause)) throw cause;
        openSceneId = (await directorScenes.create(projectId, nodeId)).id;
        ElMessage.info(plan.notice);
      }
    }
    if (plan.action !== "open") bindDirectorScene(nodeId, openSceneId);
    // 打开期间节点被删了也照常打开：编辑器拿着冻结的场景 ID 工作，关掉时再说
    directorStudioSceneId.value = openSceneId;
    directorStudioPendingAction.value = action;
    directorStudioNodeId.value = nodeId;
  } catch (cause) {
    ElMessage.error(directorFailureText(cause, "打开导演台失败，请检查网络后重试"));
  } finally {
    openingDirector.value = false;
  }
}

function closeDirectorStudio() {
  directorStudioNodeId.value = null;
  directorStudioSceneId.value = "";
  directorStudioPendingAction.value = undefined;
  // 编辑器里存过的版本与指纹：实时通道断着时事件收不到，关的时候对一次账，节点卡上的过期角标才准（DRT-17）
  if (canvas.projectId) void directorScenes.refresh(canvas.projectId).catch(() => undefined);
}

/**
 * 编辑器截完图（dev/03 §7.1 第 3 步）：把输出机位的最新截图与每个机位的摘要镜像进导演台节点；
 * 「重新截图」截完时**只替换**那一个发布节点的产出与来源（DR-F-84），并当场取一次预览（DRB-7）。
 * 回写不进撤销栈：这是把服务端已有的记录同步过来，不是用户的编辑。
 */
function onDirectorCaptured(event: DirectorCapturedEvent) {
  const directorId = directorStudioNodeId.value;
  const director = canvas.nodes.find(node => node.id === directorId);
  if (director) {
    // 镜头文字由编辑器算好带过来（DRT-26）：摘要里存截图那一刻的文字，下游「插入镜头文字」用
    const next = directorMirrorParams(director.data.params, event.records, event.scene, { shotTextOf: id => event.shotTexts?.[id] });
    if (next !== director.data.params) canvas.updateParams(director.id, next);
  }
  const target = event.replaceNodeId ? canvas.nodes.find(node => node.id === event.replaceNodeId) : undefined;
  const record = event.records.find(item => item.kind === "capture");
  if (target && record) {
    const replaced = directorReplacePublished(
      { params: target.data.params, generation: target.data.generation },
      record,
      record.cameraId ? event.shotTexts?.[record.cameraId] : undefined
    );
    canvas.updateParams(target.id, replaced.params);
    canvas.updateGeneration(target.id, replaced.generation);
    canvas.scheduleSave();
    const refreshed = canvas.nodes.find(node => node.id === target.id);
    if (refreshed) void generation.restoreNode(refreshed).catch(() => undefined);
  }
}

/**
 * 发布到画布（dev/03 §7.2、DR-F-80）：每条记录在导演台右侧纵向建一个节点，产出是「已有产出」形态（没有生成 ID）。
 * 一次撤销撤掉整批；建好当场取预览 —— `restore()` 只在页面挂载时跑过，不取的话新节点直到刷新都是空的（DRB-7）。
 * ⚠️ `updateGeneration` 不排保存：最后必须 `scheduleSave`，否则发布节点的产出不落库，刷新就丢。
 */
async function onDirectorPublish(event: DirectorPublishEvent) {
  const director = canvas.nodes.find(node => node.id === directorStudioNodeId.value);
  const published = event.records
    .map(record => directorPublishedNode(record, event.shotTexts?.[record.id]))
    .filter((node): node is NonNullable<typeof node> => Boolean(node));
  if (!published.length) return;
  const origin = director?.position ?? { x: 480, y: 220 };
  const positions = directorPublishPositions(origin, published.length);
  canvas.snapshot();
  const created = published.map((node, index) => {
    const id = canvas.addNode(node.nodeType, node.title, node.params, {
      position: positions[index], exactTitle: true, recordHistory: false, persist: false
    });
    canvas.updateGeneration(id, node.generation);
    return id;
  });
  canvas.scheduleSave();
  ElMessage.success(`已发布 ${created.length} 个节点到画布`);
  await nextTick();
  for (const id of created) {
    const node = canvas.nodes.find(item => item.id === id);
    if (node) void generation.restoreNode(node).catch(() => undefined);
  }
}

/** 编辑器的「背景 → 全景图」与 AI 搭景参考图：指向本节点的上游图片，连线变了实时更新（DR-F-02）。 */
const directorUpstream = computed(() => directorStudioNodeId.value
  ? directorUpstreamImages(canvas.nodes, canvas.edges, directorStudioNodeId.value)
  : []);
const directorStudioTitle = computed(() => canvas.nodes.find(node => node.id === directorStudioNodeId.value)?.data.title ?? "3D 导演台");
/** 编辑器 AI 条的默认内容：节点面板上写的描述与参考图（DRT-20）。 */
const directorBuildDefaults = computed(() => {
  const params = canvas.nodes.find(node => node.id === directorStudioNodeId.value)?.data.params;
  return params ? { prompt: String(params.prompt ?? ""), uploads: referenceResources(params.reference) } : undefined;
});

function selectCanvasNode(nodeId: string | null) {
  canvas.selectedNodeId = nodeId;
  removeSelectedElements();
  if (!nodeId) return;
  const node = getNode.value(nodeId);
  if (node) addSelectedNodes([node]);
}

// ---------------------------------------------------------------- Agent 面板（AGT-28）

const agent = useAgentStore();
const agentPanelRef = ref<InstanceType<typeof AgentPanel>>();
/** 折叠时 Agent 列不占位置（2026-10-31 去掉右侧竖条，入口在顶栏的「Agent」按钮）。 */
const AGENT_RAIL_WIDTH = 0;
/** 窄于这个宽度时面板改为覆盖在画布上的抽屉（不再挤占画布）。 */
const AGENT_OVERLAY_QUERY = "(max-width: 1279px)";
// 首屏就取准（画布在 ClientOnly 里，window 一定在）：挂载后才改的话第一屏会多一次「保持中心」的平移
const agentOverlay = ref(typeof window !== "undefined" && window.matchMedia(AGENT_OVERLAY_QUERY).matches);
// 悬浮（2026-11-01）：面板是浮在画布上的卡片，同覆盖抽屉一样不占列宽 —— 切悬浮 / 停靠时照常「保持视口中心」平移
const agentColumnWidth = computed(() => (agent.panelOpen && !agentOverlay.value && !agent.panelFloating ? agent.panelWidth : AGENT_RAIL_WIDTH));
const appStyle = computed(() => ({ "--agent-panel-w": `${agent.panelWidth}px`, "--agent-col-w": `${agentColumnWidth.value}px` }));
/** 面板开合 / 拖宽引起的视口平移：只为保持视口中心，不算用户的改动，不触发保存。 */
let agentViewportShift = false;

/**
 * 面板开合 / 拖宽时**保持缩放与视口中心不变**（设计 03 §1）：画布区变窄 Δ，视口 x 左移 Δ/2，
 * 原来在正中间的节点仍在正中间 —— 否则用户正看着的节点会随面板一开一合跳走。
 * 覆盖抽屉模式下画布宽度不变，不平移。
 */
watch(agentColumnWidth, (next, prev) => {
  const delta = next - prev;
  if (!delta) return;
  const current = flowViewport.value;
  agentViewportShift = true;
  void Promise.resolve(setViewport({ x: current.x - delta / 2, y: current.y, zoom: current.zoom })).finally(() => {
    setTimeout(() => { agentViewportShift = false; }, 0);
  });
});

let overlayMedia: MediaQueryList | undefined;
function syncAgentOverlay() {
  agentOverlay.value = Boolean(overlayMedia?.matches);
}

/** 面板 → 画布：平移到节点、选中、闪烁 1 秒（设计 03 §5）。只在用户点了之后调用。 */
const LOCATE_FLASH_MS = 1000;
function locateNodes(nodeIds: string[]): number {
  const ids = [...new Set(nodeIds)].filter(id => canvas.nodes.some(node => node.id === id));
  if (!ids.length) return 0;
  canvas.selectedNodeId = ids[0] ?? null;
  removeSelectedElements();
  const nodes = ids.map(id => getNode.value(id)).filter((node): node is NonNullable<typeof node> => Boolean(node));
  if (nodes.length) addSelectedNodes(nodes);
  void fitView({ nodes: ids, duration: 300, padding: 0.35, maxZoom: Math.max(1, canvas.viewport.zoom) });
  void nextTick(() => {
    for (const id of ids) {
      const el = stageRef.value?.querySelector<HTMLElement>(`.vue-flow__node[data-id="${CSS.escape(id)}"]`);
      if (!el) continue;
      el.classList.remove("agent-locate-flash");
      void el.offsetWidth;
      el.classList.add("agent-locate-flash");
      setTimeout(() => el.classList.remove("agent-locate-flash"), LOCATE_FLASH_MS);
    }
  });
  return ids.length;
}

provide(AGENT_PANEL_BRIDGE, {
  canvasLoaded: true,
  locate: locateNodes,
  focusCanvas: () => {
    if (document.activeElement instanceof HTMLElement) document.activeElement.blur();
    stageRef.value?.focus({ preventScroll: true });
  }
});

/**
 * 打开画布时按 `?node=` 定位一次（AGT-44）。要等节点量过尺寸（`nodesInitialized`）才能 fitView 到它们；
 * 画布上一个节点都没有时那个事件不会来，就在挂载时直接说「找不到」。
 * 定位引起的视口变化落在「安静窗口」里：只记内存不保存（同面板开合的平移）。
 */
let quietViewportUntil = 0;
let initialFocusDone = false;
function focusInitialNodes() {
  if (initialFocusDone) return;
  initialFocusDone = true;
  const ids = props.focusNodeIds ?? [];
  if (!ids.length) return;
  quietViewportUntil = Date.now() + 1500;
  if (!locateNodes(ids)) ElMessage.info("要定位的节点已经不在画布上了");
}
onNodesInitialized(() => focusInitialNodes());
onMounted(() => {
  const ids = props.focusNodeIds ?? [];
  if (ids.length && !ids.some(id => canvas.nodes.some(node => node.id === id))) focusInitialNodes();
});

/**
 * 节点右键菜单（utils/canvasNodeMenu）：复制 / 创建副本 / 粘贴 / 删除 / 下载，文本节点另有复制文本、复制到剪贴板。
 * 节点右上角原来的复制 / 删除图标已去掉（2026-10-30 用户要求）；「问 Agent」这一版不出现。
 */
const nodeMenu = ref<{ nodeId: string; x: number; y: number }>();
/** 「复制节点」放这里（只在本页面里）；粘贴时照它新建一个节点。 */
const nodeClipboard = ref<{ nodeType: string; title: string; params: Record<string, unknown> } | null>(null);
const isMac = typeof navigator !== "undefined" && /Mac|iPhone|iPad/i.test(navigator.platform || navigator.userAgent);
const menuNode = computed(() => canvas.nodes.find(node => node.id === nodeMenu.value?.nodeId));
/** 菜单里的评级与保存：只对本项目里成功的那次生成成立（复制项目带来的只有 sourceGenerationId，不算）。 */
const menuGenerationId = computed(() => {
  const g = menuNode.value?.data.generation;
  return g?.status === "success" && g.id ? String(g.id) : "";
});
const menuMarks = ref<{ generationId: string; rating: number; saved: boolean; loading: boolean }>({ generationId: "", rating: 0, saved: false, loading: false });
const assetLibrary = useAssetLibraryStore();
watch(menuGenerationId, async (id) => {
  menuMarks.value = { generationId: id, rating: 0, saved: false, loading: Boolean(id) };
  if (!id) return;
  try {
    const res = await useApi()<{ items: Array<{ generationId: string; rating: number; saved: unknown[] }> }>("/library/marks", { query: { generationIds: id } });
    if (menuGenerationId.value !== id) return;
    const mark = res.items.find(item => item.generationId === id);
    menuMarks.value = { generationId: id, rating: mark?.rating ?? 0, saved: Boolean(mark?.saved.length), loading: false };
  } catch {
    // 取不到也让菜单能用：评级显示为空、保存照常（保存本身是幂等的）
    if (menuGenerationId.value === id) menuMarks.value = { ...menuMarks.value, loading: false };
  }
});
const menuItems = computed(() => {
  const node = menuNode.value;
  if (!node) return [];
  return nodeMenuItems({
    nodeType: node.data.nodeType,
    hasOutput: node.data.generation?.status === "success" && Boolean(node.data.generation.outputUrl),
    hasText: Boolean(textNodeMarkdown(node.data).trim()),
    canPaste: Boolean(nodeClipboard.value),
    mac: isMac,
    marks: { generationId: menuMarks.value.generationId || undefined, saved: menuMarks.value.saved, loading: menuMarks.value.loading }
  });
});
/** 评级：点哪颗星就是几分，再点当前那颗清除。菜单不关（连着改几次是常见的）。 */
async function rateMenuNode(star: number) {
  const id = menuMarks.value.generationId;
  if (!id) return;
  const next = menuMarks.value.rating === star ? 0 : star;
  const previous = menuMarks.value.rating;
  menuMarks.value = { ...menuMarks.value, rating: next };
  try {
    await useApi()(`/library/ratings/${id}`, { method: "PUT", body: { rating: next } });
    assetLibrary.touch();
  } catch (cause) {
    menuMarks.value = { ...menuMarks.value, rating: previous };
    ElMessage.error(cause instanceof Error ? cause.message : "评级失败，请稍后再试");
  }
}
/** 保存到我的资产：文本存卡片上此刻显示的内容（可能是改过的），标题用节点标题。幂等。 */
async function saveMenuNode(node: CanvasNode) {
  const id = menuMarks.value.generationId;
  if (!id) return;
  try {
    await useApi()("/library/items", {
      method: "POST",
      body: {
        generationId: id,
        outputIndex: 0,
        title: node.data.title,
        ...(node.data.nodeType === "text" ? { text: textNodeMarkdown(node.data) } : {})
      }
    });
    assetLibrary.touch();
    ElMessage.success("已保存到我的资产");
  } catch (cause) {
    ElMessage.error(cause instanceof Error ? cause.message : "保存失败，请稍后再试");
  }
}
/** 文本节点的副本 / 复制：把卡片上此刻的内容落成手写文本（生成结果不跟着副本走，否则副本是空的）。 */
function textCopyParams(node: CanvasNode): Record<string, unknown> {
  const params = { ...node.data.params } as Record<string, unknown>;
  if (node.data.nodeType !== "text" || node.data.generation?.outputText === undefined) return params;
  delete params.model;
  delete params.modelCode;
  delete params.textEditedFor;
  return { ...params, textMode: "manual", text: textNodeMarkdown(node.data) };
}
function copyNodeToClipboard(nodeId: string) {
  const node = canvas.nodes.find(item => item.id === nodeId);
  if (!node) return;
  nodeClipboard.value = { nodeType: node.data.nodeType, title: node.data.title, params: JSON.parse(JSON.stringify(textCopyParams(node))) };
  ElMessage.success("已复制节点");
}
function pasteNodeFromClipboard(near?: { x: number; y: number }) {
  const clip = nodeClipboard.value;
  if (!clip) return;
  const id = canvas.addNode(clip.nodeType, clip.title, JSON.parse(JSON.stringify(clip.params)), {
    ...(near ? { position: { x: near.x + 42, y: near.y + 42 } } : {}), exactTitle: true
  });
  // 导演台 / 智能剪辑：粘贴出来的也要分叉出自己的场景 / 时间线（同创建副本）
  void forkDirectorCopy(id);
  void forkSmartcutCopy(id);
  selectCanvasNode(id);
}
async function copyToClipboard(text: string, done: string) {
  try {
    await navigator.clipboard.writeText(text);
    ElMessage.success(done);
  } catch {
    ElMessage.error("复制失败：浏览器不允许访问剪贴板");
  }
}
async function downloadNode(node: CanvasNode) {
  if (node.data.nodeType === "text") {
    saveBlob(new Blob([textNodeMarkdown(node.data)], { type: "text/markdown;charset=utf-8" }), `${safeFilename(node.data.title, "文本")}.md`);
    return;
  }
  const url = node.data.generation?.outputUrl;
  if (!url) return;
  try {
    const blob = await menuMedia.blob(url);
    saveBlob(blob, `${safeFilename(node.data.title)}.${extensionFor(blob.type)}`);
  } catch {
    ElMessage.error("下载失败，请稍后再试");
  }
}
const menuMedia = useAuthenticatedMedia();
function runNodeMenu(action: NodeMenuAction) {
  const node = menuNode.value;
  nodeMenu.value = undefined;
  if (!node) return;
  if (action === "save-asset") void saveMenuNode(node);
  else if (action === "copy-node") copyNodeToClipboard(node.id);
  else if (action === "duplicate") duplicateCanvasNode(node.id);
  else if (action === "paste") pasteNodeFromClipboard(node.position);
  else if (action === "delete") canvas.removeNode(node.id);
  else if (action === "download") void downloadNode(node);
  else if (action === "copy-text") void copyToClipboard(markdownPlainText(textNodeMarkdown(node.data)), "已复制文本");
  else if (action === "copy-markdown") void copyToClipboard(textNodeMarkdown(node.data), "已复制到剪贴板");
}
function onNodeContextMenu(payload: NodeMouseEvent) {
  const event = payload.event as MouseEvent;
  const stage = stageRef.value;
  if (!stage || !("clientX" in event)) return;
  event.preventDefault();
  const rect = stage.getBoundingClientRect();
  nodeMenu.value = { nodeId: payload.node.id, x: event.clientX - rect.left, y: event.clientY - rect.top };
  // 菜单有十来行：贴着屏幕下沿 / 右沿点开时整体挪回可视区里，不然后半截看不见
  void nextTick(() => {
    const menu = nodeMenuRef.value;
    const current = nodeMenu.value;
    if (!menu || !current) return;
    const margin = 8;
    const x = Math.min(current.x, rect.width - menu.offsetWidth - margin);
    const y = Math.min(current.y, rect.height - menu.offsetHeight - margin);
    nodeMenu.value = { ...current, x: Math.max(margin, x), y: Math.max(margin, y) };
  });
}
const nodeMenuRef = ref<HTMLElement | null>(null);
/**
 * 菜单开着时，页面上**任何地方**的左键按下都关掉它 —— 包括节点本身（Vue Flow 把节点上的点击吞了，不会走到画布空白处的关闭逻辑）。
 * 捕获阶段监听，免得被节点的 stopPropagation 挡住；右键不关（右键是在别处重新打开菜单）；菜单里面的点击不关（评级要连着点）。
 */
function closeNodeMenuOnPointer(event: PointerEvent) {
  if (!nodeMenu.value || event.button !== 0) return;
  if (event.target instanceof Node && nodeMenuRef.value?.contains(event.target)) return;
  nodeMenu.value = undefined;
}
onMounted(() => document.addEventListener("pointerdown", closeNodeMenuOnPointer, true));
onBeforeUnmount(() => document.removeEventListener("pointerdown", closeNodeMenuOnPointer, true));

function openScriptStudio(nodeId: string, action?: ScriptStudioAction) {
  const node = canvas.nodes.find(item => item.id === nodeId);
  const scriptId = String(node?.data.params.scriptId ?? "");
  if (!node || !scriptCanOpen(node.data.params)) {
    selectCanvasNode(nodeId);
    ElMessage.info(scriptId ? "请先在下方生成并保存分镜脚本" : "请先在脚本节点中选择一种尝试方式");
    return;
  }
  canvas.updateParams(nodeId, { ...node.data.params, scriptStudioOpened: true });
  canvas.scheduleSave();
  selectCanvasNode(nodeId);
  scriptStudioScriptId.value = scriptId;
  scriptStudioNodeId.value = nodeId;
  scriptStudioAction.value = action;
}

async function chooseScriptSource(nodeId: string, sourceType: ScriptSourceType) {
  const target = canvas.nodes.find(node => node.id === nodeId);
  if (!target || target.data.nodeType !== "script" || target.data.params.scriptId || target.data.params.sourceType) return;

  if (sourceType === "manual") {
    try {
      const created = await scripts.create({ projectId: canvas.projectId, nodeId, sourceType, name: "脚本生成器" });
      canvas.snapshot();
      target.data.title = "脚本生成器";
      canvas.updateParams(nodeId, {
        ...target.data.params,
        scriptId: created.scriptId,
        sourceType,
        scriptEntryMode: "manual",
        scriptEntryInitialized: true,
        scriptStatus: "editing",
        scriptStep: created.currentStep,
        scriptVersion: created.version,
        scriptShotCount: 0,
        scriptPromptReady: 0,
        scriptImagePromptReady: 0,
        scriptVideoPromptReady: 0,
        scriptStudioOpened: true
      });
      scriptStudioScriptId.value = created.scriptId;
      scriptStudioNodeId.value = nodeId;
      canvas.scheduleSave();
    } catch (cause) {
      ElMessage.error(cause instanceof Error ? cause.message : "无法创建手动脚本");
    }
    return;
  }

  const requiredSourceType = sourceType === "script" ? "text" : "image";
  const existingEdges = orderedTargetEdges(canvas.edges, nodeId);
  const existingSources = existingEdges
    .map(edge => canvas.nodes.find(node => node.id === edge.source))
    .filter((node): node is typeof canvas.nodes[number] => Boolean(node));
  const existingRequired = existingSources.filter(node => node.data.nodeType === requiredSourceType);
  const missing = sourceType === "script" ? Math.max(0, 1 - existingRequired.length) : Math.max(0, 2 - existingRequired.length);
  const sourceConfig = config.nodeType(requiredSourceType);
  if (!sourceConfig) {
    ElMessage.error("画布配置中缺少脚本上游节点类型");
    return;
  }
  const validationInputs = [
    ...existingEdges.map(edge => ({
      sourceType: canvas.nodes.find(node => node.id === edge.source)?.data.nodeType ?? "text",
      order: edge.data?.order ?? 1
    })),
    ...Array.from({ length: missing }, () => ({ sourceType: requiredSourceType, order: 0 }))
  ].map((input, index) => ({ ...input, order: index + 1 }));
  try {
    const validated = await validateConnection("script", undefined, validationInputs);
    // DRB-1：同上，服务端拒绝时一个上游占位节点都不建
    const rejection = connectionRejection(validated);
    if (rejection) {
      ElMessage.error(rejection);
      return;
    }
    canvas.snapshot();
    const createdIds: string[] = [];
    for (let index = 0; index < missing; index += 1) {
      const ordinal = existingRequired.length + index + 1;
      const params = {
        ...defaultParams(sourceConfig),
        scriptSourceNode: true,
        scriptNodeId: nodeId,
        ...(requiredSourceType === "image" ? { roleIndex: ordinal, prompt: "" } : { prompt: "" })
      };
      const sourceId = canvas.addNode(requiredSourceType, requiredSourceType === "text" ? "剧本文本" : `角色 ${ordinal}`, params, {
        position: target.position
          ? { x: target.position.x - 480, y: target.position.y + (requiredSourceType === "image" ? (ordinal === 1 ? -210 : 210) : 0) }
          : undefined,
        recordHistory: false,
        persist: false
      });
      const sourceNode = canvas.nodes.find(node => node.id === sourceId);
      if (sourceNode) sourceNode.data.title = requiredSourceType === "text" ? "剧本文本" : `角色 ${ordinal}`;
      canvas.addEdge(sourceId, nodeId, {
        id: `e_${sourceId}_${nodeId}_${Date.now().toString(36)}_${ordinal}`,
        order: existingEdges.length + index + 1,
        operation: validated.operation,
        recordHistory: false,
        persist: false
      });
      createdIds.push(sourceId);
    }
    target.data.title = "脚本生成器";
    canvas.updateParams(nodeId, {
      ...target.data.params,
      sourceType,
      scriptEntryMode: sourceType === "script" ? "text_reference" : "character_reference",
      scriptEntryInitialized: true,
      scriptStatus: "editing"
    });
    selectCanvasNode(nodeId);
    canvas.scheduleSave();
    await nextTick();
    await fitView({ nodes: [nodeId, ...existingRequired.map(node => node.id), ...createdIds], duration: 360, padding: 0.28 });
  } catch (cause) {
    ElMessage.error(cause instanceof Error ? cause.message : "脚本入口初始化失败");
  }
}

function closeScriptStudio() {
  scriptStudioNodeId.value = null;
  scriptStudioScriptId.value = "";
  scriptStudioAction.value = undefined;
}

// nodeId 由调用方给时优先用它：早先坏掉的创建流程建出的脚本节点 ID 为空，按 detail.nodeId 找会静默落空
function updateScriptNode(detail: ScriptDetail, nodeId = detail.nodeId) {
  const node = canvas.nodes.find(item => item.id === nodeId);
  if (!node) return;
  canvas.updateParams(node.id, {
    ...node.data.params,
    scriptId: detail.scriptId,
    sourceType: detail.sourceType,
    sourceText: detail.input.text ?? node.data.params.sourceText,
    sourceReferences: detail.input.references ?? node.data.params.sourceReferences,
    scriptStatus: detail.status,
    scriptStep: detail.currentStep,
    scriptVersion: detail.version,
    scriptShotCount: detail.shots.length,
    scriptPromptReady: detail.promptsProgress.completed,
    scriptImagePromptReady: detail.shots.filter(shot => shot.imagePromptStatus === "ready").length,
    scriptVideoPromptReady: detail.shots.filter(shot => shot.videoPromptStatus === "ready").length
  });
}

async function regenerateScript(nodeId: string) {
  const node = canvas.nodes.find(item => item.id === nodeId);
  if (!node) return;
  const params = node.data.params;
  const sourceType = params.sourceType === "character" || params.sourceType === "script" || params.sourceType === "manual"
    ? params.sourceType
    : "";
  const scriptId = String(params.scriptId ?? "");
  if (!scriptId || !sourceType) {
    ElMessage.info("脚本尚未创建");
    return;
  }
  const allReferences = scriptCanvasReferences(canvas.nodes, canvas.edges, nodeId);
  const entryMode = resolveScriptEntryMode(params, allReferences.map(reference => reference.type));
  const references = scriptReferencesForEntryMode(allReferences, entryMode);
  const required = entryMode === "character_reference" ? 2 : entryMode === "text_reference" ? 1 : 0;
  if (references.length < required || !references.slice(0, required).every(reference => reference.ready)) {
    ElMessage.warning(entryMode === "character_reference" ? "请先完成两个角色节点" : "请先完成上游文本节点");
    return;
  }
  try {
    // 一律取一次详情：写剧情要它的版本号（乐观锁），手工脚本要看已有镜头
    const existing = await scripts.load(scriptId, { name: node.data.title });
    const sourceText = String(params.sourceText ?? existing.input.text ?? "").trim();
    if (sourceType === "manual" && !sourceText && !existing?.shots.length) {
      ElMessage.warning("请先在工作台添加镜头，或在下方输入剧情描述");
      return;
    }
    if (entryMode === "direct" && !sourceText) {
      ElMessage.warning("请输入剧情描述");
      return;
    }
    let model = String(params.model ?? "").trim();
    if (!model) {
      model = (await config.models("text", "storyboard_gen"))[0]?.code ?? "";
      if (model) canvas.updateParams(nodeId, { ...params, model });
    }
    if (!model) {
      ElMessage.warning("请先选择文本模型");
      return;
    }
    const requestText = scriptTranslatedInput(sourceText, Boolean(params.translatePrompt));
    // 同 ScriptNodePanel：references 只用于就绪校验，请求带全部已连接的上游
    const requestReferences = scriptRequestReferences(allReferences);
    // 剧情存在脚本上（先写再生成），上游引用走补充上下文 —— 规则见 utils/scriptContract
    const story = scriptStoryInput(entryMode || "direct", requestText, requestReferences);
    const current = await scripts.saveStory(existing, story.rawInput);
    const detail = await scripts.generateStoryboard(current.scriptId, model, story.extraContext,
      { name: node.data.title, references: requestReferences });
    updateScriptNode(detail, nodeId);
    ElMessage.success("分镜脚本已重新生成");
  } catch (cause) {
    ElMessage.error(cause instanceof Error ? cause.message : "分镜脚本生成失败");
  }
}

async function confirmRegenerateScript(nodeId: string) {
  const node = canvas.nodes.find(item => item.id === nodeId);
  if (!node?.data.params.scriptId) {
    ElMessage.info("脚本尚未创建");
    return;
  }
  try {
    await ElMessageBox.confirm(
      "重新生成会覆盖当前脚本内容及最终提示词，是否继续？",
      "重新生成脚本",
      {
        confirmButtonText: "开始生成",
        cancelButtonText: "取消",
        closeOnClickModal: false,
        showClose: false,
        customClass: "script-regenerate-confirm"
      }
    );
  } catch {
    return;
  }
  await regenerateScript(nodeId);
}

async function downloadScript(nodeId: string) {
  const node = canvas.nodes.find(item => item.id === nodeId);
  const scriptId = String(node?.data.params.scriptId ?? "");
  if (!scriptId) return;
  try {
    // 用服务端的导出格式（显式字段 + schemaVersion + 未完成部分的 warnings），不把详情接口的内部结构直接给用户
    const exported = await scripts.exportScript(scriptId);
    if (!import.meta.client) return;
    const blob = new Blob([JSON.stringify(exported, null, 2)], { type: "application/json;charset=utf-8" });
    const url = URL.createObjectURL(blob);
    const link = document.createElement("a");
    link.href = url;
    link.download = `aitv-script-${scriptId}.json`;
    document.body.appendChild(link);
    link.click();
    link.remove();
    window.setTimeout(() => URL.revokeObjectURL(url), 0);
    ElMessage.success("脚本已下载");
  } catch (cause) {
    ElMessage.error(cause instanceof Error ? cause.message : "脚本下载失败");
  }
}

async function handleScriptAction(nodeId: string, action: "regenerate" | "storyboard" | "video" | "download") {
  if (action === "regenerate") {
    await confirmRegenerateScript(nodeId);
    return;
  }
  if (action === "download") {
    await downloadScript(nodeId);
    return;
  }
  openScriptStudio(nodeId, action);
}

async function addGroupNode(group: GroupDetail) {
  if (!group?.canvasNodeId || canvas.nodes.some(node => String(node.data.params.groupId ?? "") === group.groupId)) return;
  const scriptNode = canvas.nodes.find(node => node.id === group.scriptId || node.data.params.scriptId === group.scriptId);
  const groupOffsets = {
    asset: { x: 480, y: 0 },
    storyboard_image: { x: 1000, y: 0 },
    video_generator: { x: 1520, y: 0 }
  } as Record<string, { x: number; y: number }>;
  const offset = groupOffsets[group.groupType] ?? { x: 480, y: 0 };
  const sameTypeCount = canvas.nodes.filter(node => {
    if (node.data.nodeType !== "group" || String(node.data.params.groupType ?? "") !== group.groupType) return false;
    const existingScriptId = String(node.data.params.scriptId ?? "");
    if (existingScriptId) return existingScriptId === group.scriptId;
    return Boolean(scriptNode && canvas.edges.some(edge => edge.source === scriptNode.id && edge.target === node.id));
  }).length;
  canvas.snapshot();
  canvas.nodes.push({
    id: group.canvasNodeId,
    type: "aitv",
    position: scriptNode
      ? { x: scriptNode.position.x + offset.x, y: scriptNode.position.y + offset.y + sameTypeCount * 720 }
      : { x: 480 + offset.x, y: 220 + sameTypeCount * 720 },
    width: ["asset", "storyboard_image", "video_generator"].includes(group.groupType) ? 368 : 460,
    data: {
      nodeType: "group",
      title: group.name,
      params: {
        groupId: group.groupId,
        projectId: group.projectId,
        groupType: group.groupType,
        groupStatus: group.status,
        groupVersion: group.version,
        scriptId: group.scriptId,
        // 执行 / 重试时要带模型：新后端组上不存模型，建组时选的记在节点上
        ...(group.modelCode ? { modelCode: group.modelCode } : {}),
        itemCount: group.items.length,
        items: group.items
      }
    }
  });
  if (scriptNode && !canvas.edges.some(edge => edge.source === scriptNode.id && edge.target === group.canvasNodeId)) {
    canvas.addEdge(scriptNode.id, group.canvasNodeId, {
      id: `e_${scriptNode.id}_${group.canvasNodeId}`,
      order: 1,
      recordHistory: false,
      persist: false
    });
  }
  // 组节点必须立刻落库，不能走 700ms 防抖：视频生成器组建出来是 draft 状态、
  // 不提交生成，用户看完就可能刷新，防抖窗口内刷新会让节点凭空消失。
  await canvas.save();
  // save() 内部吞掉异常只改状态，这里必须把失败讲出来 —— 否则画布上看得见节点、
  // 刷新后却没了，用户无从知道是没存上（版本冲突时要在冲突条上选一个出路才能恢复保存）。
  if (canvas.saveState === "conflict") {
    ElMessage.error(`${group.name} 未能保存到画布：画布版本冲突，请在画布顶部的冲突提示中处理（改动已存为草稿）`);
  } else if (canvas.saveState === "error") {
    ElMessage.error(`${group.name} 未能保存到画布，请检查网络后重试`);
  }
}

function updateGroupNode(nodeId: string, group: GroupDetail) {
  const node = canvas.nodes.find(item => item.id === nodeId);
  if (!node) return;
  // ⚠️ 写进来的必须是归一化过的组（useScriptApi.adoptGroup）：缺 groupId 的不收 —— 收了就把节点上已知的
  //    groupId 抹成空串，轮询随之停掉、子项编辑面板一项一个地叠出来（CD-81）
  if (!group.groupId) return;
  const next = {
    ...node.data.params,
    groupId: group.groupId,
    projectId: group.projectId,
    groupType: group.groupType,
    groupStatus: group.status,
    groupVersion: group.version,
    itemCount: group.items.length,
    items: group.items
  };
  // 运行中的组每 1.4 秒轮询一次：什么都没变就别动画布 —— 每次都写的话整张画布跟着每 1.4 秒存一次
  if (JSON.stringify(next) === JSON.stringify(node.data.params)) return;
  canvas.updateParams(nodeId, next);
}

function materializeGroup(nodeId: string, group: GroupDetail) {
  const source = canvas.nodes.find(item => item.id === nodeId);
  const origin = source?.position ?? { x: 480, y: 220 };
  canvas.snapshot();
  canvas.nodes = canvas.nodes.filter(item => item.id !== nodeId);
  canvas.edges = canvas.edges.filter(edge => edge.source !== nodeId && edge.target !== nodeId);
  group.items.forEach((item, index) => {
    const nodeType = group.groupType === "video_generator" ? "video" : "image";
    canvas.nodes.push({
      id: item.canvasNodeId,
      type: "aitv",
      position: { x: origin.x + (index % 3) * 430, y: origin.y + Math.floor(index / 3) * 370 },
      width: nodeType === "video" ? 390 : 330,
      data: {
        nodeType,
        title: group.groupType === "asset"
          ? `资产 · ${String(item.paramsSnapshot?.name ?? item.assetId ?? index + 1)}`
          // 镜号不是 shotId：后者是雪花号，标题会变成「镜头 103845514381885440」
          : `${nodeType === "image" ? "分镜图" : "视频"} · 镜头 ${String(item.paramsSnapshot?.shotNo ?? index + 1).padStart(2, "0")}`,
        params: {
          ...(item.paramsSnapshot ?? {}),
          groupId: group.groupId,
          groupItemId: item.itemId,
          shotId: item.shotId,
          assetId: item.assetId
        },
        generation: item.generationId || item.outputUrl
          ? {
              id: item.generationId ?? undefined,
              status: item.status,
              progress: item.progress ?? (item.status === "success" ? 100 : 0),
              outputType: item.outputType ?? nodeType,
              outputUrl: item.outputUrl ?? undefined
            }
          : undefined
      }
    });
  });
  canvas.selectedNodeId = group.items[0]?.canvasNodeId ?? null;
  canvas.scheduleSave();
}

function openAssetPlaza(type: "style" | "effect") {
  assetType.value = type;
  assetPlazaOpen.value = true;
  addMenuOpen.value = false;
}

/**
 * 广场里选一个风格 / 特效：在画布上建一个素材节点（连到图片 / 视频节点后，服务端按素材码拼词）。
 * ⚠️ 素材节点存 **`materialCode`**（生成输入按它声明 `kind=material`）；此前存的是老后端的
 *    `styleId` / `effectId`，新后端的条目没有 id，于是选了什么都不发生。
 */
function selectMaterial(item: MaterialItem) {
  const type = assetType.value;
  if (!item.code) return;
  const nodeId = canvas.addNode(type, item.name ?? (type === "style" ? "风格" : "特效"), {
    materialCode: item.code,
    name: item.name,
    cover: item.coverUrl ?? "",
    category: item.category ?? "",
    commercial: item.isCommercial
  });
  assetPlazaOpen.value = false;
  void nextTick(() => {
    selectCanvasNode(nodeId);
    return fitView({ nodes: [nodeId], duration: 300, padding: 0.35 });
  });
}

function onDragStart() { canvas.snapshot(); }
function onDragStop(event: NodeDragEvent) {
  // 用户挪了 Agent 建的节点：标记为改过，Agent 之后不再改它（AGT-11）
  for (const node of event.nodes ?? [event.node]) if (node) canvas.markTouched(node.id);
  canvas.scheduleSave();
  updatePendingConnectionLine();
}

/** 远端批次提示上的「查看」：只在用户点了之后才移动视口（AGT-11，不自动平移）。 */
function viewRemoteNodes(nodeIds: string[]) {
  void fitView({ nodes: nodeIds, duration: 300, padding: 0.3 });
}

/** 节点还没量出尺寸时（刚建、还没渲染）按这个大小排：与新建节点的默认宽度同一量级。 */
const TIDY_FALLBACK_SIZE = { width: 320, height: 240 };

/**
 * 整理画布（左下角工具条 / ⌥⇧F，2026-11-01）：排法见 utils/canvasTidy —— 按连线从左往右分列，画框连同框里的节点整块挪。
 * 一次撤销回到整理前；挪到的节点按「用户挪过」记（与拖动一样，AGT-11：Agent 之后不再动它们）。
 * 排版算法按需加载：点了才用得到，不进画布首屏的包。
 */
async function tidyCanvas() {
  if (canvas.nodes.length < 2) return;
  const { tidyCanvasLayout } = await import("~/utils/canvasTidy");
  const positions = tidyCanvasLayout(
    canvas.nodes.map((node) => {
      const measured = getNode.value(node.id)?.dimensions;
      return {
        id: node.id,
        x: node.position.x,
        y: node.position.y,
        width: measured?.width || Number(node.width) || TIDY_FALLBACK_SIZE.width,
        height: measured?.height || Number(node.height) || TIDY_FALLBACK_SIZE.height,
        ...(node.type === "frame" ? { members: node.data.members ?? [] } : {})
      };
    }),
    canvas.edges,
    { snap: viewPrefs.value.snapToGrid ? CANVAS_GRID_SIZE : undefined }
  );
  const moved = canvas.nodes.filter((node) => {
    const next = positions.get(node.id);
    return next && (next.x !== node.position.x || next.y !== node.position.y);
  });
  if (moved.length) {
    canvas.snapshot();
    for (const node of moved) {
      node.position = { ...positions.get(node.id)! };
      canvas.markTouched(node.id);
    }
    canvas.scheduleSave();
    edgeCut.value = undefined;
    ElMessage.success({ message: `已整理画布，${isMac ? "⌘Z" : "Ctrl+Z"} 可撤销`, duration: 2500 });
  }
  await nextTick();
  void fitView({ duration: 300, padding: 0.18 });
}

/** 左下角工具条与快捷键共用的视图动作（⌥⇧F / ⌘+ / ⌘- / ⌘0）。 */
function runViewAction(action: CanvasViewShortcut) {
  if (action === "tidy") void tidyCanvas();
  else if (action === "zoomIn") void zoomIn({ duration: 160 });
  else if (action === "zoomOut") void zoomOut({ duration: 160 });
  else void fitView({ duration: 300, padding: 0.18 });
}
function onViewport(value: ViewportTransform) {
  canvas.viewport = value;
  // 面板开合带来的平移不是用户的改动：只记在内存里，不为它保存一次画布（打开面板不该写数据）
  if (agentViewportShift || Date.now() < quietViewportUntil) return;
  canvas.scheduleSave();
  void nextTick().then(updatePendingConnectionLine);
}


/**
 * 双击连线 → 在双击处出一个剪刀按钮，点它删掉这条连线（2026-10-31 用户要求）。
 * 记的是**流坐标**：画布平移 / 缩放时按钮跟着那一点走，而不是停在屏幕上原地。
 */
const edgeCut = ref<{ edgeId: string; x: number; y: number }>();
const edgeCutStyle = computed(() => {
  const cut = edgeCut.value;
  if (!cut) return undefined;
  const view = flowViewport.value;
  return { left: `${cut.x * view.zoom + view.x}px`, top: `${cut.y * view.zoom + view.y}px` };
});

function onEdgeDoubleClick({ event, edge }: EdgeMouseEvent) {
  if (!(event instanceof MouseEvent)) return;
  // 不让双击继续冒泡到画布：那里会把它当成「双击空白处」（缩放 / 唤出添加菜单）
  event.stopPropagation();
  event.preventDefault();
  const point = screenToFlowCoordinate({ x: event.clientX, y: event.clientY });
  edgeCut.value = { edgeId: edge.id, x: point.x, y: point.y };
}

function cutEdge() {
  const cut = edgeCut.value;
  edgeCut.value = undefined;
  if (cut) canvas.removeEdge(cut.edgeId);
}

// 连线藏起来时剪刀跟着收起：连线都看不见了，剪刀悬在半空不知道剪的是哪根
watch(edgesHidden, (hidden) => {
  if (hidden) edgeCut.value = undefined;
});

// 连线被别的途径删掉（撤销、删节点、远端改动）时按钮跟着消失，不留一个点了没反应的剪刀
watch(() => canvas.edges, (edges) => {
  if (edgeCut.value && !edges.some(edge => edge.id === edgeCut.value?.edgeId)) edgeCut.value = undefined;
});

function onPaneClick(event: MouseEvent) {
  nodeMenu.value = undefined;
  edgeCut.value = undefined;
  selectCanvasNode(null);
  const now = performance.now();
  if (connectionMenu.value && now - connectionMenuOpenedAt > 140) dismissConnectionMenu();
  // 菜单已开时点画布一律关闭；否则双击空白处唤出菜单。
  // 早先无条件走双击判定，导致开着菜单点画布反而把它重新打开、看似关不掉。
  if (addMenuOpen.value) addMenuOpen.value = false;
  else if (event.detail > 1 || now - lastPaneClickAt < 320) addMenuOpen.value = true;
  lastPaneClickAt = now;
}

/** 点击菜单与加号之外的任何地方都关闭添加节点菜单（含 dock 上的其他按钮）。 */
function onPointerDownOutsideAddMenu(event: PointerEvent) {
  if (!addMenuOpen.value) return;
  const anchor = addMenuRef.value;
  if (anchor && event.target instanceof Node && anchor.contains(event.target)) return;
  addMenuOpen.value = false;
}

function onKeydown(event: KeyboardEvent) {
  if (event.key === "Escape") {
    edgeCut.value = undefined;
    dismissConnectionMenu();
    addMenuOpen.value = false;
    nodeMenu.value = undefined;
  }
  // ⚠️ 编辑区域里的按键、以及全屏层（脚本工作台 / 素材广场）开着时，画布快捷键一律不响应（CD-78）
  // 导演台编辑器开着时同样一律不响应（DR-UX-04）；编辑器自己在捕获阶段拦它消费的键，这里是第二道
  if (!canvasShortcutsEnabled(event, Boolean(scriptStudioNodeId.value) || assetPlazaOpen.value
    || Boolean(directorStudioNodeId.value) || Boolean(smartcutStudioNodeId.value))) return;
  // ⌘J / Ctrl+J 开关 Agent 面板（AGT-28）；输入框里按下时上一行已经挡掉，不抢
  if (isAgentPanelToggle(event)) {
    event.preventDefault();
    agent.togglePanel();
    return;
  }
  // 焦点在 Agent 面板里（按钮、卡片）时，画布的删除 / 撤销不响应 —— 用户看的是面板，不是画布
  if (event.target instanceof Element && event.target.closest(".agent-panel")) return;
  // 整理画布 ⌥⇧F、放大 ⌘+、缩小 ⌘-、适合屏幕 ⌘0（2026-11-01）：拦下浏览器自己的页面缩放
  const viewAction = canvasViewShortcut(event);
  if (viewAction) {
    event.preventDefault();
    runViewAction(viewAction);
    return;
  }
  const modifier = event.metaKey || event.ctrlKey;
  if (modifier && event.key.toLowerCase() === "z") {
    event.preventDefault();
    if (event.shiftKey) canvas.redo();
    else canvas.undo();
  }
  if (modifier && event.key.toLowerCase() === "y") { event.preventDefault(); canvas.redo(); }
  if (modifier && event.key.toLowerCase() === "d" && canvas.selectedNodeId) { event.preventDefault(); duplicateCanvasNode(canvas.selectedNodeId); }
  if (modifier && event.key.toLowerCase() === "c" && canvas.selectedNodeId && !window.getSelection()?.toString()) {
    event.preventDefault();
    copyNodeToClipboard(canvas.selectedNodeId);
  }
  if (modifier && event.key.toLowerCase() === "v" && nodeClipboard.value) {
    event.preventDefault();
    pasteNodeFromClipboard(canvas.nodes.find(node => node.id === canvas.selectedNodeId)?.position);
  }
  if ((event.key === "Backspace" || event.key === "Delete") && canvas.selectedNodeId) canvas.removeNode(canvas.selectedNodeId);
}

watch(connectionMenu, (menu) => {
  if (!menu) {
    pendingConnectionLine.value = undefined;
    return;
  }
  void nextTick().then(updatePendingConnectionLine);
}, { deep: true });

onMounted(() => {
  overlayMedia = window.matchMedia(AGENT_OVERLAY_QUERY);
  syncAgentOverlay();
  overlayMedia.addEventListener("change", syncAgentOverlay);
  window.addEventListener("keydown", onKeydown);
  window.addEventListener("resize", updatePendingConnectionLine);
  window.addEventListener("pointerdown", onPointerDownOutsideAddMenu);
});
onBeforeUnmount(() => {
  overlayMedia?.removeEventListener("change", syncAgentOverlay);
  window.removeEventListener("keydown", onKeydown);
  window.removeEventListener("resize", updatePendingConnectionLine);
  window.removeEventListener("pointerdown", onPointerDownOutsideAddMenu);
});
</script>

<template>
  <div class="canvas-app" :class="{ 'sidebar-collapsed': !sidebarOpen, 'agent-open': agent.panelOpen, 'agent-overlay': agentOverlay }" :style="appStyle" :data-realtime-state="generation.connectionState">
    <header class="canvas-topbar">
      <div class="canvas-project"><NuxtLink to="/video" title="返回项目"><img class="aitv-mark" src="/brand/logo-mark.png" alt="tabtv" draggable="false"></NuxtLink><span class="top-separator" /><input
        v-if="renaming"
        ref="renameInput"
        v-model="renameDraft"
        class="project-name-input"
        maxlength="100"
        aria-label="项目名称"
        @keyup.enter="commitRename"
        @keyup.esc="cancelRename"
        @blur="commitRename"
      >
      <button v-else type="button" class="project-name" title="点击重命名项目" @click="startRename">
        <strong>{{ canvas.projectName }}</strong><ChevronDown :size="14" />
      </button><span :class="['save-indicator', canvas.saveState]"><LoaderCircle v-if="canvas.saveState === 'saving'" :size="13" class="spin" /><CircleAlert v-else-if="canvas.saveState === 'conflict' || canvas.saveState === 'error'" :size="13" /><CloudCheck v-else :size="13" />{{ saveLabel }}</span></div>
      <div class="canvas-account"><button type="button" class="agent-toggle" :class="{ active: agent.panelOpen }" :aria-pressed="agent.panelOpen" title="Agent 面板（⌘J）" @click="agent.togglePanel()"><Sparkles :size="15" />Agent<LoaderCircle v-if="agent.anyWorking" :size="12" class="spin" /><i v-else-if="agent.anyAwaiting" class="agent-red-dot" /></button><AccountPanel variant="credits" trigger-class="credit-pill" @open="generation.loadBalance()"><Zap :size="14" />{{ generation.balance.toLocaleString('zh-CN') }}</AccountPanel><AccountPanel trigger-class="canvas-avatar" /></div>
    </header>

    <aside v-if="sidebarOpen" class="canvas-sidebar">
      <div class="sidebar-tabs"><button :class="{ active: sidebarTab === 'layers' }" @click="sidebarTab = 'layers'">画布</button><button :class="{ active: sidebarTab === 'assets' }" @click="sidebarTab = 'assets'">资产</button></div>
      <label v-if="sidebarTab === 'layers'" class="sidebar-search"><Search :size="14" /><input v-model="search" placeholder="搜索节点"></label>
      <div v-if="sidebarTab === 'layers'" class="sidebar-section-title"><span>画布元素</span><Menu :size="14" /></div>
      <div v-if="sidebarTab === 'layers'" class="layer-list">
        <button v-for="node in filteredNodes" :key="node.id" :class="{ active: canvas.selectedNodeId === node.id }" @click="selectCanvasNode(node.id); fitView({ nodes: [node.id], duration: 300, padding: 0.3 })"><span><component :is="nodeIcons[node.data.nodeType as keyof typeof nodeIcons] ?? FileText" :size="14" /></span><strong>{{ node.data.title }}</strong><small>{{ node.data.nodeType }}</small></button>
        <p v-if="!filteredNodes.length" class="sidebar-empty">画布暂无节点</p>
      </div>
      <!-- 我的资产（迁移 00074）：只有保存过的生成产物；按需加载 -->
      <AssetLibraryPanel v-else @created="onResourcesCreated" @manage="assetDialogOpen = true" @agent="(refs) => agentPanelRef?.attachMedia(refs)" />
      <footer><button type="button" @click="sidebarOpen = false"><PanelLeftClose :size="14" />收起</button><span>共 {{ canvas.nodes.length }} 节点</span></footer>
    </aside>

    <main ref="stageRef" class="canvas-stage" tabindex="-1">
      <CanvasConflictBar />
      <button v-if="!sidebarOpen" class="sidebar-open" type="button" title="展开侧栏" @click="sidebarOpen = true"><PanelLeftOpen :size="17" /></button>
      <VueFlow v-model:nodes="canvas.nodes" v-model:edges="canvas.edges" class="aitv-flow" :class="{ connecting, 'edges-hidden': edgesHidden }" :min-zoom="CANVAS_MIN_ZOOM" :max-zoom="CANVAS_MAX_ZOOM" :snap-to-grid="viewPrefs.snapToGrid" :snap-grid="CANVAS_SNAP_GRID" :default-viewport="canvas.viewport" :delete-key-code="null" :multi-selection-key-code="'Shift'" selection-on-drag pan-on-scroll :nodes-draggable="true" :nodes-connectable="true" :elements-selectable="true" @connect="connect" @connect-start="onConnectStart" @connect-end="onConnectEnd" @node-drag-start="onDragStart" @node-drag-stop="onDragStop" @viewport-change-end="onViewport" @pane-click="onPaneClick" @edge-double-click="onEdgeDoubleClick" @node-context-menu="onNodeContextMenu">
        <Background variant="dots" :gap="CANVAS_GRID_SIZE" :size="1.2" :color="dotColor" />
        <template #node-aitv="nodeProps"><BaseNode v-bind="nodeProps" @select-node="selectCanvasNode" @open-script="openScriptStudio" @choose-script-source="chooseScriptSource" @script-action="handleScriptAction" @update-group="updateGroupNode" @ungroup="materializeGroup" @open-director="openDirectorStudio" @open-smartcut="openSmartcutStudio" @duplicate-node="duplicateCanvasNode" @locate-node="(id: string) => locateNodes([id])" /></template>
        <template #node-frame="nodeProps"><FrameNode v-bind="nodeProps" /></template>
        <MiniMap v-if="viewPrefs.minimap" position="bottom-left" pannable zoomable :node-color="'#858991'" :node-stroke-color="'#d7d9dc'" mask-color="rgba(8,9,10,.62)" />
      </VueFlow>

      <button
        v-if="edgeCut && edgeCutStyle"
        type="button"
        class="edge-cut-button"
        :style="edgeCutStyle"
        title="删除连线"
        aria-label="删除连线"
        data-testid="edge-cut"
        @click.stop="cutEdge"
        @dblclick.stop
      >
        <Scissors :size="16" />
      </button>

      <svg
        v-if="connectionMenu && pendingConnectionLine"
        class="pending-connection-overlay"
        width="100%"
        height="100%"
        aria-hidden="true"
        data-testid="pending-connection-line"
      >
        <path class="pending-connection-glow" :d="pendingConnectionPath" />
        <path class="pending-connection-path" :d="pendingConnectionPath" />
        <circle class="pending-connection-end" :cx="pendingConnectionLine.endX" :cy="pendingConnectionLine.endY" r="3.5" />
      </svg>

      <section v-if="!canvas.nodes.length" class="canvas-empty-state" aria-label="开始创作">
        <div class="canvas-empty-hint"><MousePointer2 :size="18" /><span>双击画布 自由生成节点</span></div>
        <div class="canvas-quick-nodes">
          <button v-for="entry in quickNodes" :key="entry.code" type="button" :data-testid="`quick-node-${entry.code}`" @click="addNode(entry.code)">
            <span><component :is="entry.icon" :size="18" /></span>
            <strong>{{ entry.label }}</strong>
          </button>
        </div>
      </section>
      <aside
        v-if="connectionMenu"
        ref="connectionMenuRef"
        class="node-reference-menu nodrag nowheel"
        :style="{ left: `${connectionMenu.x}px`, top: `${connectionMenu.y}px` }"
        aria-label="引用该节点生成"
        data-testid="connection-create-menu"
        @mousedown.stop
        @click.stop
      >
        <strong>引用该节点生成</strong>
        <button
          v-for="option in connectionTargets"
          :key="option.nodeType.code"
          type="button"
          :data-node-type="option.nodeType.code"
          :disabled="!option.allowed || Boolean(creatingConnectedType)"
          :title="option.allowed ? `创建${option.nodeType.name ?? option.nodeType.code}节点` : '当前节点不能连接此类型'"
          @click="createConnectedNode(option)"
        >
          <LoaderCircle v-if="creatingConnectedType === option.nodeType.code" :size="18" class="spin" />
          <component :is="nodeIcons[option.nodeType.code as keyof typeof nodeIcons] ?? FileText" v-else :size="18" />
          <span>{{ option.nodeType.name }}</span>
        </button>
      </aside>
      <!-- 底栏（2026-11-01）：左边视图工具条、中间 dock，排版见 canvas.css 的 .canvas-bottom-bar -->
      <div class="canvas-bottom-bar">
        <CanvasViewTools
          v-model:minimap="viewPrefs.minimap"
          v-model:edges-hidden="edgesHidden"
          v-model:snap-to-grid="viewPrefs.snapToGrid"
          :zoom="flowViewport.zoom"
          :can-tidy="canvas.nodes.length > 1"
          :mac="isMac"
          @tidy="runViewAction('tidy')"
          @zoom-in="runViewAction('zoomIn')"
          @zoom-out="runViewAction('zoomOut')"
          @fit="runViewAction('fit')"
          @zoom-to="(zoom: number) => zoomTo(zoom, { duration: 200 })"
        />
        <nav class="canvas-dock" aria-label="画布工具栏">
          <!-- 菜单锚在加号按钮上：dock 在底栏中间、加号在最左，
               菜单若同样居中就会偏到加号右侧，故包一层相对定位容器。 -->
          <div ref="addMenuRef" class="dock-add">
            <button class="primary" type="button" title="添加节点" :aria-expanded="addMenuOpen" @click.stop="addMenuOpen = !addMenuOpen"><Plus :size="19" /></button>
            <div v-if="addMenuOpen" class="node-add-menu" @click.stop>
              <strong>添加节点</strong>
              <button v-for="type in addMenuTypes" :key="type.code" type="button" @click="addNode(type.code ?? '')">
                <component :is="nodeIcons[type.code as keyof typeof nodeIcons] ?? FileText" :size="20" />
                <span>{{ type.name }}</span>
                <small v-if="canvasAddMenuBadge(type.code)" :class="`kind-${type.kind}`">{{ canvasAddMenuBadge(type.code) }}</small>
              </button>
              <hr>
              <strong>添加资源</strong>
              <CanvasAddResources :project-id="canvas.projectId" @history="openHistoryPicker" @created="onResourcesCreated" />
            </div>
          </div>
          <!-- 没接功能的按钮（选择与连接、角色库、快捷键、帮助）先不放（2026-11-01），接上功能再加回来 -->
          <button type="button" title="适应画布" @click="fitView({ duration: 300, padding: 0.18 })"><WandSparkles :size="17" /></button>
          <button type="button" title="风格广场" @click="openAssetPlaza('style')"><Palette :size="17" /></button>
          <button type="button" title="特效库" @click="openAssetPlaza('effect')"><Sparkles :size="17" /></button>
          <button type="button" title="生成历史" @click="openHistoryPicker"><History :size="17" /></button>
          <span />
          <button type="button" title="撤销" :disabled="!canvas.undoStack.length" @click="canvas.undo"><Undo2 :size="17" /></button>
          <button type="button" title="重做" :disabled="!canvas.redoStack.length" @click="canvas.redo"><Redo2 :size="17" /></button>
        </nav>
      </div>
      <CanvasOpsToast @view="viewRemoteNodes" />
      <div v-if="nodeMenu" ref="nodeMenuRef" class="node-context-menu nodrag nowheel" :style="{ left: `${nodeMenu.x}px`, top: `${nodeMenu.y}px` }" @click.stop @mousedown.stop>
        <template v-for="(item, index) in menuItems" :key="index">
          <hr v-if="item.kind === 'separator'">
          <div v-else-if="item.action === 'rate'" class="node-menu-rate" :class="{ disabled: item.disabled }" :title="item.hint">
            <span>{{ item.label }}</span>
            <span class="stars" role="radiogroup" aria-label="评级">
              <button
                v-for="star in 5"
                :key="star"
                type="button"
                role="radio"
                :aria-checked="menuMarks.rating === star"
                :aria-label="`${star} 星`"
                :disabled="item.disabled"
                :class="{ on: star <= menuMarks.rating }"
                :data-star="star"
                @click="rateMenuNode(star)"
              >★</button>
            </span>
          </div>
          <button v-else type="button" :disabled="item.disabled" :title="item.hint" :data-action="item.action" @click="runNodeMenu(item.action)">
            <span>{{ item.label }}</span><kbd v-if="item.shortcut">{{ item.shortcut }}</kbd>
          </button>
        </template>
      </div>
    </main>
    <AgentPanel ref="agentPanelRef" />

    <AssetLibraryDialog v-if="assetDialogOpen" :project-id="canvas.projectId" @close="assetDialogOpen = false" @created="(ids: string[]) => { assetDialogOpen = false; onResourcesCreated(ids); }" />
    <GenerationHistoryPicker v-if="historyPickerOpen" v-model="historyPickerOpen" :project-id="canvas.projectId" @created="onResourcesCreated" />
    <MaterialPlaza v-model="assetPlazaOpen" :type="assetType" @select="selectMaterial" />
    <ScriptStudio
      v-if="scriptStudioNodeId && scriptStudioScriptId"
      :script-id="scriptStudioScriptId"
      :node-id="scriptStudioNodeId"
      :initial-action="scriptStudioAction"
      @close="closeScriptStudio"
      @updated="updateScriptNode"
      @group-created="addGroupNode"
    />
    <component
      :is="DirectorStudio"
      v-if="directorStudioNodeId && directorStudioSceneId"
      :project-id="canvas.projectId"
      :scene-id="directorStudioSceneId"
      :node-id="directorStudioNodeId"
      :node-title="directorStudioTitle"
      :upstream-images="directorUpstream"
      :pending-action="directorStudioPendingAction"
      :build-defaults="directorBuildDefaults"
      @close="closeDirectorStudio"
      @captured="onDirectorCaptured"
      @publish="onDirectorPublish"
    />
    <component
      :is="SmartcutStudio"
      v-if="smartcutStudioNodeId && smartcutStudioTimelineId"
      :project-id="canvas.projectId"
      :timeline-id="smartcutStudioTimelineId"
      :node-id="smartcutStudioNodeId"
      :node-title="smartcutStudioTitle"
      :pending-action="smartcutStudioPendingAction"
      @close="leaveSmartcutStudio"
      @collapse="leaveSmartcutStudio"
      @publish="onSmartcutPublish"
    />
    <div class="canvas-mobile-block"><Home :size="26" /><strong>请使用桌面设备打开画布</strong><NuxtLink to="/video">返回项目</NuxtLink></div>
  </div>
</template>

<style>
/*
 * 画框（AGT-11）：画在其他节点下面，且只有标题栏接收指针事件 ——
 * 框内空白处要能照常框选 / 平移画布。选中时也不抬到最上层（否则会盖住框里的节点）。
 */
.vue-flow__node.vue-flow__node-frame,
.vue-flow__node.vue-flow__node-frame.selected { z-index: -1 !important; pointer-events: none; }

/* Agent 面板定位到的节点：闪烁 1 秒（AGT-28，类名由 locateNodes 临时加上） */
.vue-flow__node.agent-locate-flash > * { animation: agent-locate-flash 1s ease-out; }
@keyframes agent-locate-flash {
  0%, 50% { outline: 2px solid var(--canvas-cyan); outline-offset: 4px; }
  25%, 75% { outline: 2px solid var(--cv-cyan-52a30); outline-offset: 4px; }
  100% { outline: 2px solid transparent; outline-offset: 4px; }
}

/* 远端新增的节点：淡入 + 1 秒描边高亮（类名由 store.highlightedNodeIds 驱动） */
.aitv-node.remote-new,
.frame-node.remote-new { animation: canvas-remote-in 1.2s ease-out; }
@keyframes canvas-remote-in {
  0% { opacity: 0; outline: 2px solid var(--canvas-cyan); outline-offset: 4px; }
  25% { opacity: 1; outline: 2px solid var(--canvas-cyan); outline-offset: 4px; }
  100% { opacity: 1; outline: 2px solid transparent; outline-offset: 4px; }
}
</style>
