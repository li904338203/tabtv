<script setup lang="ts">
import type { NodeProps } from "@vue-flow/core";
import { Handle, Position } from "@vue-flow/core";
import type { Component } from "vue";
import {
  AlignLeft,
  AudioLines,
  CircleAlert,
  Clapperboard,
  Download,
  FilePen,
  FileText,
  FileImage,
  Film,
  Image as ImageIcon,
  ImageUpscale,
  Layers,
  LoaderCircle,
  Music,
  NotebookText,
  Palette,
  Play,
  RefreshCw,
  Scissors,
  ScrollText,
  Sparkles,
  Square,
  UserRound,
  Video
} from "@lucide/vue";
import ParamPanel from "./ParamPanel.vue";
import DocumentNodeBody from "./nodes/DocumentNodeBody.vue";
import DirectorNodeBody from "./nodes/DirectorNodeBody.vue";
import DirectorSourceBadge from "./DirectorSourceBadge.vue";
import OriginBadge from "./OriginBadge.vue";
import type { components } from '~/types/legacy';
import type { CanvasNodeData } from "~/types/canvas";
import type { GroupDetail, GroupItem, ScriptSourceType } from "~/types/script";
import {
  aspectFromRatio,
  generationStatusLabel,
  imagePreviewHeight,
  ruleOf,
  scriptCanOpen,
  scriptHasStoryboard,
  shouldSpawnPresetNode,
  generatingLabel,
  VIDEO_NODE_FRAME,
  videoPreviewFrame
} from "~/utils/canvas";
import { groupItemReferences, groupItemStatusLabel as itemStatusLabel, groupNeedsRepair, groupStatusLabel } from "~/utils/scriptContract";
import { directorSourceOf, type DirectorPendingAction } from "~/utils/directorPublish";
import type { SmartcutPendingAction } from "~/utils/smartcutCanvas";
import { smartcutSourceOf } from "~/utils/smartcutPublish";
import { imageToolOfParams, type ImageToolCode } from "~/utils/imageTools";
// 工具条只在「选中一个已出图的图片节点」时出现：异步加载，不进画布页首屏（包体上限）。
const ImageNodeToolbar = defineAsyncComponent(() => import("./ImageNodeToolbar.vue"));

/**
 * 导演台的提示词面板（DRT-20）：只在选中导演台节点时出现，按需加载 —— 它带着 `@` 候选列表，
 * 没有导演台的画布不必为它多下一份代码（画布页的包体积有上限，DRT-09）。
 */
const DirectorPromptPanel = defineAsyncComponent(() => import("./nodes/DirectorPromptPanel.vue"));
/** 智能剪辑的指令面板（SCT-23）：同上，选中时才加载（它带着素材条与上传）。 */
const SmartcutPromptPanel = defineAsyncComponent(() => import("./nodes/SmartcutPromptPanel.vue"));
/**
 * 只在选中脚本 / 组节点时才出现的面板按需加载：画布页首屏包体积有上限（scripts/director-bundle-check.mjs，导演台与智能剪辑
 * 合计不超过基线 20 KB），2026-10-30 已经顶到上限 —— 这三块（合计约 50 KB 源码）挪出首屏给后续改动留出余量。
 */
const ScriptNodePanel = defineAsyncComponent(() => import("./ScriptNodePanel.vue"));
const GroupNodePanel = defineAsyncComponent(() => import("./GroupNodePanel.vue"));
const GroupImageNodePanel = defineAsyncComponent(() => import("./GroupImageNodePanel.vue"));
/**
 * 智能剪辑的节点卡与发布节点的来源角标（SCT-23 / SCT-24）：画布上有这两种节点时才加载 ——
 * 画布页首屏的包体积有上限（导演台与智能剪辑合计不超过基线 20 KB），没有智能剪辑的画布不必为它们多下一份代码。
 */
const SmartcutNodeBody = defineAsyncComponent(() => import("./nodes/SmartcutNodeBody.vue"));
/**
 * 视频节点的画面与底部控制条（播放 / 进度 / 声音 / 截帧，2026-11-01）：同样按需加载 —— 截帧带着上传与建节点，
 * 不能进画布页首屏包。
 */
const VideoNodeBody = defineAsyncComponent(() => import("./nodes/VideoNodeBody.vue"));
const SmartcutSourceBadge = defineAsyncComponent(() => import("./SmartcutSourceBadge.vue"));
/**
 * 图片工具条的四个编辑器（docs/agent/imagetools IMGD-21，IMG-N-06）：**只能异步引入** —— 画布页首屏包体只剩约 5 KB 余量，
 * 静态 import 任何一个都会超线（utils/imageTools.test.ts 有源码扫描钉住）。
 */
const TOOL_EDITORS: Record<ImageToolCode, Component> = {
  expression_edit: defineAsyncComponent(() => import("./imagetools/EmotionEditor.vue")),
  multi_angle: defineAsyncComponent(() => import("./imagetools/MultiAngleEditor.vue")),
  relight: defineAsyncComponent(() => import("./imagetools/RelightEditor.vue")),
  upscale: defineAsyncComponent(() => import("./imagetools/UpscalePanel.vue"))
};

const props = defineProps<NodeProps<CanvasNodeData>>();
const emit = defineEmits<{
  "select-node": [nodeId: string];
  "open-script": [nodeId: string];
  "choose-script-source": [nodeId: string, sourceType: ScriptSourceType];
  "script-action": [nodeId: string, action: "regenerate" | "storyboard" | "video" | "download"];
  "update-group": [nodeId: string, group: GroupDetail];
  "ungroup": [nodeId: string, group: GroupDetail];
  /** 打开 3D 导演台：CanvasBoard 先做自愈判定（DRD-43）再挂载编辑器；`action` 是打开后要顺手做的事（重新截图，DRT-17）。 */
  "open-director": [nodeId: string, action?: DirectorPendingAction];
  /** 打开智能剪辑（SCT-23）：CanvasBoard 先做自愈判定再挂载剪辑台；`action` 是打开后要顺手做的事（AI 剪辑、追加素材）。 */
  "open-smartcut": [nodeId: string, action?: SmartcutPendingAction];
  /** 定位到某个节点（发布节点的来源角标，DRT-17）。 */
  "locate-node": [nodeId: string];
  /** 复制节点：交给 CanvasBoard，导演台复制出来要立刻分叉出自己的场景（DRD-43）。 */
  "duplicate-node": [nodeId: string];
}>();
const canvas = useCanvasStore();
const icons = { text: FileText, image: ImageIcon, video: Video, audio: AudioLines, script: ScrollText, group: Layers, style: Palette, effect: Sparkles, document: NotebookText, director: Clapperboard, smartcut: Scissors };
const icon = computed(() => icons[props.data.nodeType as keyof typeof icons] ?? FileText);
const generation = computed(() => props.data.generation);

/**
 * 手写文本节点（「尝试：自己编写内容」，2026-10-30 用户反馈）：卡片本身就是输入框，内容存 `params.text`。
 * 下游取文本输入本来就先认 `params.text`（utils/canvas generationInputs）；进入手写时去掉模型参数 ——
 * 整图运行把「没配模型的文本节点」当静态输入跳过（workflow/domain.IsLiteralInput），带着默认模型的话会被当成要生成的节点，
 * 一跑就把手写的内容换成模型的输出。手写模式下不出生成面板。
 */
const manualText = computed(() => props.data.nodeType === "text" && props.data.params.textMode === "manual"
  && generation.value?.outputText === undefined);
/** 刚点了「自己编写内容」：卡片换成输入框后把光标放进去。 */
const manualTextFocus = ref(false);
/** 排队中 / 生成中（任何节点类型）。 */
const generationActive = computed(() => generation.value?.status === "queued" || generation.value?.status === "running");
/** 文本节点生成中：骨架行代替转圈（各行宽度固定，免得每次渲染跳动）。 */
const textGenerating = computed(() => props.data.nodeType === "text" && generationActive.value);
const TEXT_SKELETON = ["82%", "100%", "100%", "90%", "100%", "100%", "70%", "100%", "62%", "44%"];
/**
 * 生成中的「等了多久」（2026-11-01）：拿不到进度时（文本模型中途不报，不少图片 / 视频模型也只报开始和结束，
 * 服务端进度一直是 0）显示等了多久，不再挂着「0%」像是卡住了。文本节点的底部胶囊与其他节点的状态层共用。
 * 从这张卡看到排队 / 生成中开始每秒走一次（刷新页面后从头算），结束即停表。
 */
const waitStart = ref(0);
const waitNow = ref(0);
let waitTimer: ReturnType<typeof setInterval> | undefined;
watch(generationActive, (active) => {
  clearInterval(waitTimer);
  waitTimer = undefined;
  if (!active) return;
  waitStart.value = Date.now();
  waitNow.value = waitStart.value;
  waitTimer = setInterval(() => { waitNow.value = Date.now(); }, 1000);
}, { immediate: true });
onBeforeUnmount(() => clearInterval(waitTimer));
const progressLabel = computed(() => generatingLabel(generation.value?.status, generation.value?.progress, waitNow.value - waitStart.value));
/** 文本节点有生成结果或在生成中：与手写文本同样的宽卡片。 */
const textWide = computed(() => props.data.nodeType === "text"
  && (manualText.value || textGenerating.value || generation.value?.outputText !== undefined));
const TextManualBody = defineAsyncComponent(() => import("./nodes/TextManualBody.vue"));
function startManualText() {
  const next = { ...props.data.params, textMode: "manual", text: String(props.data.params.text ?? "") } as Record<string, unknown>;
  delete next.model;
  delete next.modelCode;
  canvas.markTouched(props.id);
  canvas.updateParams(props.id, next as CanvasNodeData["params"]);
  manualTextFocus.value = true;
}
function runSuggestion(item: string) {
  if (props.data.nodeType !== "text") return;
  if (item === "自己编写内容") startManualText();
}
const configStore = useCanvasConfigStore();
const imageToolbarVisible = computed(() => {
  const node = canvas.nodes.find(item => item.id === props.id);
  return Boolean(selected.value && node && shouldSpawnPresetNode(node));
});
const generationStore = useGenerationStore();
const media = useAuthenticatedMedia();
const selected = computed(() => canvas.selectedNodeId === props.id);
const materialNode = computed(() => props.data.nodeType === "style" || props.data.nodeType === "effect");
/**
 * 文档节点（AGT-09）：只存 docId，不能作为可执行输入 —— 没有入/出把手、没有参数面板。
 * 服务端也不配任何连线规则，整图解析会把它剔除；这里藏把手只是让人不去尝试。
 */
const documentNode = computed(() => props.data.nodeType === "document");
const documentId = computed(() => String(props.data.params.docId ?? ""));
/**
 * 3D 导演台（DRT-16）：节点上只存 sceneId 与截图时镜像过来的几个字段，场景正文不进 canvasData（DRK-9）。
 * 自己不能「生成」（DR-F-08），所以没有通用参数面板；选中时挂提示词面板（DRT-20）。
 */
const directorNode = computed(() => props.data.nodeType === "director");
/**
 * 智能剪辑（SCT-23）：节点上只存 timelineId 与用户输入，成片与状态从摘要取（SCK-13）。卡片只读（SCK-17），
 * 没有通用参数面板；选中时挂指令面板。
 */
const smartcutNode = computed(() => props.data.nodeType === "smartcut");
/**
 * 智能剪辑选中后的指令面板只在 AI 开着时出现（utils/smartcutFeatures）：这一版只保留手动剪辑，面板里剩下的「+ 参考」与素材条
 * 也一起不出现（用户决定 2026-10-29）—— 素材靠在画布上连线进来，剪辑台里照样能看素材条。
 */
const smartcutAi = useSmartcutFeatures().ai;
/**
 * 从导演台发布出来的节点（DRT-17）：静态素材，不显示参数面板（DR-F-81），挂来源角标；
 * 加工工具条照常显示 —— 用户可以直接从它派生「按白模构图出图」节点。
 */
const publishedNode = computed(() => Boolean(directorSourceOf(props.data.params)));
/** 从智能剪辑发布出来的成片节点（SCT-24）：同样是静态素材，挂来源角标，不显示参数面板。 */
const smartcutPublished = computed(() => Boolean(smartcutSourceOf(props.data.params)));
const materialCover = computed(() => typeof props.data.params.cover === "string" ? props.data.params.cover : "");
/**
 * 素材封面是受保护媒体（`/api/v1/media/{id}`）：`<img src>` 不带鉴权头，直接引用一律 401、显示成裂图。
 * fetch → blob → objectURL（useAuthenticatedMedia，卸载时释放）。
 */
const materialCovers = useAuthenticatedMedia();
const materialCoverSrc = computed(() => materialCovers.urls.value.cover ?? "");
watch(materialCover, (url) => {
  materialCovers.release();
  if (url) void materialCovers.appendAll([{ key: "cover", url: url.includes("?") ? url : `${url}?w=640` }]);
}, { immediate: true });
const outputType = computed(() => generation.value?.outputType ?? props.data.nodeType);
const scriptId = computed(() => String(props.data.params.scriptId ?? ""));
const scriptStep = computed(() => {
  const value = Number(props.data.params.scriptStep ?? 1);
  return Number.isFinite(value) ? Math.min(3, Math.max(1, value)) : 1;
});
const hasStoryboard = computed(() => scriptHasStoryboard(props.data.params));
const canOpenScript = computed(() => scriptCanOpen(props.data.params));
/**
 * 工具条打开的编辑器（`mode="spawn"`，IMG-F-04）：同一时间节点下方只有一个面板 —— 打开时普通 ParamPanel 让位；
 * 再点同一个按钮或按 Esc 关闭；取消选中（含发送后选中新子节点，IMG-UX-03）时自动关闭。
 */
const toolEditor = ref<ImageToolCode | "">("");
/** 工具子节点（`params.tool` 存在）：下方的面板是对应编辑器（`mode="node"`），取代 ParamPanel（IMGD-22）。 */
const nodeTool = computed(() => props.data.nodeType === "image" ? imageToolOfParams(props.data.params) : undefined);
/** 工具子节点的源图节点：连线 order=1 的上游（spawnToolNode 建的那条）。 */
const toolSourceNodeId = computed(() => {
  const incoming = canvas.edges
    .filter(edge => edge.target === props.id)
    .sort((a, b) => (a.data?.order ?? 0) - (b.data?.order ?? 0));
  return incoming[0]?.source ?? "";
});
/** 子节点上的编辑器被用户关掉（再次选中时恢复）。 */
const nodeToolPanelClosed = ref(false);
function toggleToolEditor(tool: ImageToolCode) {
  toolEditor.value = toolEditor.value === tool ? "" : tool;
}
function onToolEditorKeydown(event: KeyboardEvent) {
  // 编辑器自己处理过的 Esc（如情绪调节框选途中取消框选）不再关掉整个面板。
  if (event.key !== "Escape" || event.defaultPrevented) return;
  if (toolEditor.value) toolEditor.value = "";
  else if (nodeTool.value && selected.value) nodeToolPanelClosed.value = true;
}
watch(selected, (value) => {
  if (!value) toolEditor.value = "";
  nodeToolPanelClosed.value = false;
});
watch(() => Boolean(toolEditor.value) || Boolean(nodeTool.value && selected.value), (listening) => {
  if (typeof window === "undefined") return;
  if (listening) window.addEventListener("keydown", onToolEditorKeydown);
  else window.removeEventListener("keydown", onToolEditorKeydown);
}, { immediate: true });
onBeforeUnmount(() => window.removeEventListener("keydown", onToolEditorKeydown));

/** 加工条选预设：一律派生下游节点 —— 能看到这条工具条就说明本节点已经出图了。 */
function applyToolbarPreset(preset: components["schemas"]["PresetResp"]) {
  const rule = (configStore.config?.connectionRules ?? []).find(
    (item) => {
      const r = ruleOf(item);
      return r.source === "image" && r.target === "image" && r.allowed;
    });
  canvas.addPresetNode(props.id, preset, String(rule?.operation ?? "img2img"));
}

const scriptToolbarVisible = computed(() => Boolean(scriptId.value && props.data.params.scriptStudioOpened));
const imagePromptReady = computed(() => Number(props.data.params.scriptImagePromptReady ?? 0));
const videoPromptReady = computed(() => Number(props.data.params.scriptVideoPromptReady ?? 0));
const scriptSourceType = computed<ScriptSourceType | "">(() => {
  const value = props.data.params.sourceType;
  if (value === "script" || value === "character" || value === "manual") return value;
  return scriptId.value ? "manual" : "";
});
const scriptEntries: Array<{ value: ScriptSourceType; label: string; icon: Component }> = [
  { value: "script", label: "剧本生成分镜脚本", icon: FileText },
  { value: "character", label: "角色生成分镜脚本", icon: UserRound },
  { value: "manual", label: "自己编写分镜脚本", icon: ScrollText }
];
const groupItems = computed(() => Array.isArray(props.data.params.items) ? props.data.params.items as Array<Record<string, unknown>> : []);
const groupStatus = computed(() => String(props.data.params.groupStatus ?? "draft"));
const groupId = computed(() => String(props.data.params.groupId ?? ""));
/** 组接口只走脚本接口的统一入口：轮询直接把接口原样写进节点，正是 CD-81 那批坏节点的来历。 */
const scripts = useScriptApi();
/** 节点上存的组没归一化过（见 groupNeedsRepair）：打开画布时按画布节点取一次组把它修好。 */
const groupNeedsFix = computed(() => props.data.nodeType === "group"
  && groupNeedsRepair({ groupId: props.data.params.groupId, items: props.data.params.items }));
/** 修复只试一次：组已不存在（取回 404）时不必每次渲染都再打一遍接口。 */
let groupRepairTried = false;
const groupType = computed(() => String(props.data.params.groupType ?? ""));
/** 用大卡片铺开展示的组：视频组与图片组同款尺寸，小方块看不清内容也看不出进度 */
const mediaCollectionGroup = computed(() =>
  ["asset", "storyboard_image", "video_generator"].includes(groupType.value));
const videoCollectionGroup = computed(() => groupType.value === "video_generator");
// 子项可单独选中、编辑参数并重新生成的组（与后端 ITEM_EDITABLE_TYPES 对应）
/** 可点开子项面板的组：与后端 ITEM_EDITABLE_TYPES 保持一致 */
const itemEditableGroup = computed(() =>
  ["asset", "storyboard_image", "video_generator"].includes(groupType.value));
const groupPreviewLimit = 8;
const groupVisibleItems = computed(() => mediaCollectionGroup.value
  ? groupItems.value
  : groupItems.value.slice(0, groupPreviewLimit));
const groupCompletedCount = computed(() => groupItems.value.filter(item =>
  ["success", "ready", "completed"].includes(String(item.status ?? ""))
).length);
const groupPreviewUrls = ref<Record<string, string>>({});
const groupPreviewSources = ref<Record<string, string>>({});
const groupPreviewHeights = ref<Record<string, number>>({});
const selectedGroupItemId = ref("");
const nodeRef = ref<HTMLElement>();
let groupRefreshTimer: ReturnType<typeof setTimeout> | undefined;
const groupLabel = computed(() => ({
  asset: "资产组",
  storyboard_image: "分镜图组",
  video_generator: "视频生成器组"
	} as Record<string, string>)[String(props.data.params.groupType ?? "")] ?? "生成组");
const groupTypeClass = computed(() => groupType.value ? `group-${groupType.value}` : "group-generic");
/**
 * ⚠️ 没选中时必须是 undefined：子项缺 `itemId` 的话 `'' === ''` 对每一项都成立，
 *    模板里每一项都会挂一个编辑面板（CD-81 截图里那一摞「镜头 01」）。
 */
const selectedGroupItem = computed(() => selectedGroupItemId.value
  ? groupItems.value.find(item => String(item.itemId ?? "") === selectedGroupItemId.value) as GroupItem | undefined
  : undefined);
const selectedGroupItemIndex = computed(() => selectedGroupItemId.value
  ? groupItems.value.findIndex(item => String(item.itemId ?? "") === selectedGroupItemId.value)
  : -1);
const selectedGroupItemTitle = computed(() => selectedGroupItem.value
  ? groupItemTitle(selectedGroupItem.value as unknown as Record<string, unknown>, selectedGroupItemIndex.value)
  : "分镜图");
const groupDetail = computed<GroupDetail>(() => ({
  groupId: groupId.value,
  projectId: String(props.data.params.projectId ?? canvas.projectId),
  scriptId: String(props.data.params.scriptId ?? ""),
  canvasNodeId: props.id,
  groupType: groupType.value,
  name: props.data.title,
  status: groupStatus.value,
  version: Number(props.data.params.groupVersion ?? 1),
  items: groupItems.value as GroupItem[]
}));
const generatedImageUrl = computed(() => {
  const url = generation.value?.previewUrl;
  return url && !["video", "audio"].includes(outputType.value) && generation.value?.outputText === undefined
    ? url
    : "";
});
const generatedImageHeight = ref(180);
const generatedImageFrameStyle = computed(() => generatedImageUrl.value
  ? { height: `${generatedImageHeight.value}px`, minHeight: `${generatedImageHeight.value}px` }
  : undefined);

watch(generatedImageUrl, () => {
  generatedImageHeight.value = 180;
});

/**
 * 视频节点按产物的真实比例摆（竖屏就是竖的卡片，见 videoPreviewFrame）。
 *
 * 比例先取视频元数据（videoWidth/videoHeight）；元数据到之前按生成时选的画幅摆 ——
 * 否则一打开画布每个竖屏视频都先是横框、再跳成竖的。两者不一致时（首尾帧按首帧的比例出片）以元数据为准。
 */
const videoUrl = computed(() => generation.value?.previewUrl && outputType.value === "video" ? generation.value.previewUrl : "");
const videoMeta = ref<{ width: number; height: number }>();
const videoFrame = computed(() => {
  if (!videoUrl.value) return undefined;
  const size = videoMeta.value ?? aspectFromRatio(props.data.params.aspect_ratio);
  return videoPreviewFrame(size?.width, size?.height);
});
const nodeStyle = computed(() => videoFrame.value ? { width: `${videoFrame.value.width}px` } : undefined);
const previewFrameStyle = computed(() => videoFrame.value
  ? { height: `${videoFrame.value.height}px`, minHeight: `${videoFrame.value.height}px` }
  : generatedImageFrameStyle.value);
watch(videoUrl, () => {
  videoMeta.value = undefined;
});
// Vue Flow 的节点外框（连线端点、框选范围都按它算）跟着卡片一起变宽变窄；没有产出时回到默认宽度。
// ⚠️ 用 watchEffect 而不是只盯 videoFrame：远端批次会整个替换节点对象，宽度被重置回默认值时要再收一次。
watchEffect(() => {
  if (props.data.nodeType !== "video") return;
  canvas.fitNodeWidth(props.id, videoFrame.value?.width ?? VIDEO_NODE_FRAME.maxWidth);
});

/** 画面与控制条在 nodes/VideoNodeBody（悬浮静音预览 + 底部控制条 + 截帧）；它读到元数据后把真实宽高报上来。 */
function onVideoMeta(size: { width: number; height: number }) {
  videoMeta.value = size;
}

/**
 * 双击节点上的图片放大预览（工具条「放大查看」同一个入口）。
 *
 * - 当前这张直接用节点内存里的原图（节点显示的就是完整产物，不是缩略图），不再请求一次；
 * - 一次生成出了好几张（GO-40）时预览里能逐张看 —— 节点上只显示第一张，另外几张此前在画布上无从查看；
 * - 副本里的节点没有 `id`、只有 `sourceGenerationId`（GAP-16）：列产物用它照样成立（同一工作区）。
 */
const preview = useOutputPreview();
async function openImagePreview() {
  const view = generation.value;
  if (!generatedImageUrl.value || !view) return;
  let seed: Blob | undefined;
  try {
    seed = await fetch(generatedImageUrl.value).then(response => response.blob());
  } catch {
    // 取不到内存里那份就按地址重新取，不影响预览
  }
  await preview.open({
    generationId: view.id ?? view.sourceGenerationId,
    fallback: view.outputUrl ? { type: "image", url: view.outputUrl } : undefined,
    seed,
    types: ["image"],
    type: "image",
    title: props.data.title,
    subtitle: canvas.projectName,
    filename: `${canvas.projectName}-${props.data.title}`
  });
}

function onGeneratedImageLoad(event: Event) {
  const image = event.currentTarget;
  if (!(image instanceof HTMLImageElement)) return;
  const frameWidth = image.closest<HTMLElement>(".node-preview-frame")?.clientWidth ?? 330;
  generatedImageHeight.value = imagePreviewHeight(frameWidth, image.naturalWidth, image.naturalHeight);
}

function groupItemKey(item: Record<string, unknown>, index: number) {
  return String(item.itemId ?? index);
}

function groupItemPreview(item: Record<string, unknown>, index: number) {
  const outputUrl = typeof item.outputUrl === "string" ? item.outputUrl : "";
  if (!outputUrl) return "";
  if (!outputUrl.startsWith("/api/")) return outputUrl;
  return groupPreviewUrls.value[groupItemKey(item, index)] ?? "";
}

function groupItemMediaStyle(item: Record<string, unknown>, index: number) {
  const height = groupPreviewHeights.value[groupItemKey(item, index)] ?? 180;
  return { height: `${height}px`, minHeight: `${height}px` };
}

/** 视频没有 naturalWidth，改读 videoWidth/videoHeight，其余与图片同一套高度计算 */
function onGroupVideoLoad(event: Event, item: Record<string, unknown>, index: number) {
  const video = event.currentTarget;
  if (!(video instanceof HTMLVideoElement) || !video.videoWidth) return;
  const frameWidth = video.closest<HTMLElement>(".group-stack-media")?.clientWidth ?? 330;
  groupPreviewHeights.value = {
    ...groupPreviewHeights.value,
    [groupItemKey(item, index)]: imagePreviewHeight(frameWidth, video.videoWidth, video.videoHeight)
  };
}

function onGroupImageLoad(event: Event, item: Record<string, unknown>, index: number) {
  const image = event.currentTarget;
  if (!(image instanceof HTMLImageElement)) return;
  const frameWidth = image.closest<HTMLElement>(".group-stack-media")?.clientWidth ?? 330;
  groupPreviewHeights.value = {
    ...groupPreviewHeights.value,
    [groupItemKey(item, index)]: imagePreviewHeight(frameWidth, image.naturalWidth, image.naturalHeight)
  };
}

/**
 * 子项选择与组节点选中互不影响：选子项时不把组也选中，
 * 否则组会同时显示选中态、并弹出组自己的操作条。
 */
function selectGroupItem(item: Record<string, unknown>) {
  if (!itemEditableGroup.value) return;
  const next = String(item.itemId ?? "");
  // 没有 itemId 的子项（还没修好的旧节点）无从编辑：面板要拿它去存参数、去执行
  if (!next) return;
  selectedGroupItemId.value = selectedGroupItemId.value === next ? "" : next;
}

function groupItemSnapshot(item: Record<string, unknown>) {
  return item.paramsSnapshot && typeof item.paramsSnapshot === "object"
    ? item.paramsSnapshot as Record<string, unknown>
    : {};
}

function groupItemTitle(item: Record<string, unknown>, index: number) {
  const snapshot = groupItemSnapshot(item);
  if (groupType.value === "asset") return String(snapshot.name ?? `资产 ${index + 1}`);
  if (groupType.value === "storyboard_image" || groupType.value === "video_generator") {
    return `镜头 ${String(snapshot.shotNo ?? index + 1).padStart(2, "0")}`;
  }
  return `子项 ${index + 1}`;
}

function groupItemSubtitle(item: Record<string, unknown>) {
  const snapshot = groupItemSnapshot(item);
  if (groupType.value === "asset") {
    const type = ({ character: "角色", scene: "场景", prop: "道具" } as Record<string, string>)[String(snapshot.assetType ?? "")];
    return type ? `${type}资产` : "脚本资产";
  }
  if (groupType.value === "storyboard_image") {
    // 一眼看得出这一镜关联了几张资产设定图（与单独的图片节点带参考图同一个意思，CD-82）
    const references = groupItemReferences(item.inputRefs).length;
    const size = String(snapshot.shotSize ?? "") || "分镜画面";
    return references ? `${size} · 参考图 ${references} 张` : size;
  }
  if (groupType.value === "video_generator") {
    const duration = Number(snapshot.duration ?? 0);
    return duration > 0 ? `分镜视频 · ${duration} 秒` : "分镜视频";
  }
  return String(item.outputType ?? "生成内容");
}

function groupItemStatusLabel(item: Record<string, unknown>) {
  return itemStatusLabel(item.status as string | undefined);
}

function revokeGroupPreview(key: string) {
  const value = groupPreviewUrls.value[key];
  if (value?.startsWith("blob:")) URL.revokeObjectURL(value);
  groupPreviewUrls.value = Object.fromEntries(
    Object.entries(groupPreviewUrls.value).filter(([entryKey]) => entryKey !== key)
  );
  groupPreviewSources.value = Object.fromEntries(
    Object.entries(groupPreviewSources.value).filter(([entryKey]) => entryKey !== key)
  );
}

async function syncGroupPreviews() {
  if (!import.meta.client || props.data.nodeType !== "group") return;
  const items = groupVisibleItems.value;
  const active = new Set(items.map(groupItemKey));
  Object.keys(groupPreviewUrls.value).forEach((key) => {
    if (!active.has(key)) revokeGroupPreview(key);
  });
  for (const [index, item] of items.entries()) {
    const key = groupItemKey(item, index);
    const outputUrl = typeof item.outputUrl === "string" ? item.outputUrl : "";
    if (!outputUrl.startsWith("/api/")) continue;
    if (groupPreviewSources.value[key] === outputUrl && groupPreviewUrls.value[key]) continue;
    revokeGroupPreview(key);
    try {
      const response = await media.fetchMedia(outputUrl);
      if (!response.ok) continue;
      groupPreviewUrls.value[key] = URL.createObjectURL(await response.blob());
      groupPreviewSources.value[key] = outputUrl;
    } catch {
      // The status grid remains available while an individual preview is unavailable.
    }
  }
}

function scheduleGroupRefresh() {
  clearTimeout(groupRefreshTimer);
  if (!import.meta.client || props.data.nodeType !== "group") return;
  if (groupNeedsFix.value && !groupRepairTried) {
    groupRepairTried = true;
    void refreshGroupState();
    return;
  }
  if (groupId.value && ["queued", "running"].includes(groupStatus.value)) {
    groupRefreshTimer = setTimeout(() => void refreshGroupState(), 1400);
  }
}

async function refreshGroupState() {
  if (props.data.nodeType !== "group") return;
  try {
    // ⚠️ 必须过 useScriptApi 的归一化：新后端回的是 `id` / 子项 `id`，原样写进节点就是 CD-81 那批坏节点
    const next = await scripts.refreshGroup(groupDetail.value);
    emit("update-group", props.id, next);
    clearTimeout(groupRefreshTimer);
    if (["queued", "running"].includes(next.status)) {
      groupRefreshTimer = setTimeout(() => void refreshGroupState(), 1400);
    }
  } catch {
    // 运行中的组过一会儿再问；修复失败（这个节点的组已经不在了）不重试
    if (groupId.value && ["queued", "running"].includes(groupStatus.value)) {
      groupRefreshTimer = setTimeout(() => void refreshGroupState(), 3000);
    }
  }
}

watch(
  () => groupItems.value.map(item => `${String(item.itemId)}:${String(item.outputUrl ?? "")}`).join("|"),
  () => void syncGroupPreviews(),
  { immediate: true }
);

watch(groupItems, (items) => {
  if (selectedGroupItemId.value && !items.some(item => String(item.itemId ?? "") === selectedGroupItemId.value)) {
    selectedGroupItemId.value = "";
  }
});


watch(
  () => `${groupId.value}:${groupStatus.value}`,
  scheduleGroupRefresh,
  { immediate: true }
);

/**
 * 点节点外部时收起子项面板。子项选择已与组的选中态解耦，
 * 不能再靠"组取消选中"来清空，需独立监听。
 */
function onPointerDownOutsideItem(event: PointerEvent) {
  if (!selectedGroupItemId.value) return;
  const root = nodeRef.value;
  if (root && event.target instanceof Node && root.contains(event.target)) return;
  selectedGroupItemId.value = "";
}

onMounted(() => window.addEventListener("pointerdown", onPointerDownOutsideItem));

onBeforeUnmount(() => {
  window.removeEventListener("pointerdown", onPointerDownOutsideItem);
  clearTimeout(groupRefreshTimer);
  Object.keys(groupPreviewUrls.value).forEach(revokeGroupPreview);
});

const suggestions: Record<string, string[]> = {
  text: ["自己编写内容"],
  image: ["文本生成图片", "图片高清处理"],
  video: ["首帧生成视频", "首尾帧生成视频"],
  audio: ["文本生成语音", "生成背景音乐"],
  script: ["确认镜头", "准备资产", "合成提示词"],
  style: ["连接到图片节点"],
  effect: ["连接到视频节点"]
};
/** 「尝试：」每一项前面的小图标（与参考产品同款：图标 + 文字，悬停有底色与阴影）。 */
const suggestionIcons: Record<string, Component> = {
  自己编写内容: FilePen,
  文本生成图片: ImageIcon, 图片高清处理: ImageUpscale,
  首帧生成视频: Play, 首尾帧生成视频: Film,
  文本生成语音: AudioLines, 生成背景音乐: Music,
  连接到图片节点: ImageIcon, 连接到视频节点: Video
};
</script>

<template>
  <article ref="nodeRef" class="aitv-node" :class="[`node-${data.nodeType}`, data.nodeType === 'group' ? groupTypeClass : '', { selected, 'remote-new': canvas.highlightedNodeIds.includes(id), 'text-manual': textWide }]" :style="nodeStyle" @mousedown="emit('select-node', id)">
    <ImageNodeToolbar
      v-if="imageToolbarVisible"
      :node-id="id"
      :presets="configStore.imagePresets"
      :source-media-id="generation?.mediaId"
      :active-editor="toolEditor"
      @select="applyToolbarPreset"
      @preview="openImagePreview"
      @toggle-editor="toggleToolEditor"
    />
    <div v-if="data.nodeType === 'script' && scriptToolbarVisible" class="script-node-commandbar nodrag nopan" @pointerdown.stop @mousedown.stop>
      <button type="button" title="重新生成分镜脚本" @click.stop="emit('script-action', id, 'regenerate')"><RefreshCw :size="15" /><span>重新生成</span></button>
      <button type="button" title="批量生成分镜" :disabled="imagePromptReady <= 0" @click.stop="emit('script-action', id, 'storyboard')"><FileImage :size="15" /><span>批量生成分镜</span></button>
      <button type="button" title="批量生成视频" :disabled="videoPromptReady <= 0" @click.stop="emit('script-action', id, 'video')"><Film :size="15" /><span>批量生视频</span></button>
      <button type="button" title="下载脚本" :disabled="!hasStoryboard" @click.stop="emit('script-action', id, 'download')"><Download :size="15" /><span class="sr-only">下载脚本</span></button>
    </div>
    <GroupNodePanel
      v-if="selected && data.nodeType === 'group'"
      :node-id="id"
      :data="data"
      @update="emit('update-group', id, $event)"
      @ungroup="emit('ungroup', id, $event)"
    />
    <header class="node-heading drag-handle"><span><component :is="icon" :size="15" /><OriginBadge :source="data" /><b class="node-heading-title">{{ data.title }}</b></span><small v-if="generation?.status">{{ generationStatusLabel(generation) }}</small></header>
    <!-- 复制 / 删除在右键菜单里（2026-10-30 用户要求去掉右上角两个图标；CanvasBoard 的节点菜单） -->
    <div class="node-preview-frame" :class="{ 'has-generated-image': generatedImageUrl, 'has-generated-video': videoUrl }" :style="previewFrameStyle">
      <!-- 加号由 .aitv-handle::before 绘制，不能放子元素：Vue Flow 的落点判定
           只看 elementFromPoint 命中元素自身的 class，命中子元素就连不上线 -->
      <Handle v-if="!materialNode && !documentNode" id="in" type="target" :position="Position.Left" class="aitv-handle" />
      <!-- 文本节点生成中：骨架行 + 底部胶囊「生成中 · 12 秒」（有进度时是百分比），不盖一层转圈（参考产品同款，2026-10-30） -->
      <div v-if="textGenerating" class="node-text-skeleton" aria-hidden="true"><i v-for="(w, i) in TEXT_SKELETON" :key="i" :style="{ width: w }" /></div>
      <!-- 文本节点的生成结果：与手写文本同一个可编辑框，改了之后下游用改过的文字 -->
      <component
        :is="TextManualBody"
        v-else-if="data.nodeType === 'text' && generation?.outputText !== undefined"
        :node-id="id"
        :params="data.params"
        :output="generation.outputText"
        :generation-id="generation.id"
      />
      <pre v-else-if="generation?.outputText !== undefined" class="node-text-output" aria-label="文本生成结果">{{ generation.outputText }}</pre>
      <component :is="VideoNodeBody" v-else-if="videoUrl" :node-id="id" :title="data.title" :src="videoUrl" :selected="selected" @meta="onVideoMeta" />
      <div v-else-if="generation?.previewUrl && outputType === 'audio'" class="node-audio-output nodrag nowheel"><AudioLines :size="30" /><audio :src="generation.previewUrl" controls preload="metadata" /></div>
      <!-- ⚠️ `.stop`：双击要是冒泡到画布，Vue Flow 会顺手把视口放大一级 -->
      <div v-else-if="generatedImageUrl" class="node-image-output nowheel" title="双击放大预览" @dblclick.stop="openImagePreview"><img :src="generatedImageUrl" alt="生成结果" :draggable="false" @load="onGeneratedImageLoad"></div>
      <div v-else-if="materialNode && materialCoverSrc" class="node-image-output material"><img :src="materialCoverSrc" :alt="String(data.params.name ?? data.title)" :draggable="false"></div>
      <!-- 没有封面的素材（后台还没配图）：画一个占位，名字照样显示在下面 -->
      <div v-else-if="materialNode && data.params.materialCode" class="node-image-output material material-placeholder"><component :is="icon" :size="36" stroke-width="1.4" /></div>
      <DocumentNodeBody v-else-if="documentNode" :project-id="canvas.projectId" :doc-id="documentId" />
      <DirectorNodeBody
        v-else-if="directorNode"
        :params="data.params"
        :title="data.title"
        @open="emit('open-director', id)"
        @recapture="emit('open-director', id, { type: 'recapture', nodeId: id, cameraId: null })"
      />
      <component :is="SmartcutNodeBody" v-else-if="smartcutNode" :node-id="id" :params="data.params" :title="data.title" @open="emit('open-smartcut', id)" />
      <div v-else-if="data.nodeType === 'script'" class="script-node-preview" :class="{ 'entry-state': !scriptSourceType, 'waiting-state': scriptSourceType && !canOpenScript, 'ready-state': canOpenScript }">
        <template v-if="!scriptSourceType">
          <ScrollText :size="43" stroke-width="1.35" class="script-entry-mark" />
          <div class="script-entry-options nodrag nopan" @pointerdown.stop @mousedown.stop>
            <span>尝试：</span>
            <button v-for="entry in scriptEntries" :key="entry.value" class="nodrag nopan" type="button" @pointerdown.stop @mousedown.stop @click.stop="emit('choose-script-source', id, entry.value)">
              <component :is="entry.icon" :size="17" />{{ entry.label }}
            </button>
          </div>
        </template>
        <template v-else-if="!scriptId">
          <AlignLeft :size="54" stroke-width="2.6" class="script-entry-mark" />
        </template>
        <template v-else-if="!canOpenScript">
          <AlignLeft :size="54" stroke-width="2.6" class="script-entry-mark" />
        </template>
        <template v-else>
          <AlignLeft :size="54" stroke-width="2.6" class="script-ready-mark" />
          <div class="script-node-steps"><span :class="{ active: scriptStep === 1, done: scriptStep > 1 }"><b>1</b><em>确认镜头</em></span><span :class="{ active: scriptStep === 2, done: scriptStep > 2 }"><b>2</b><em>准备资产</em></span><span :class="{ active: scriptStep === 3 }"><b>3</b><em>合成提示词</em></span></div>
          <button class="script-node-open" type="button" @click.stop="emit('open-script', id)">打开脚本节点</button>
        </template>
      </div>
      <div v-else-if="data.nodeType === 'group'" class="group-node-preview" :class="{ 'collection-group': mediaCollectionGroup }">
        <div class="group-node-summary"><Layers :size="20" /><div><strong>{{ groupLabel }}</strong><small>{{ groupCompletedCount }}/{{ groupItems.length }} 已完成 · {{ groupStatusLabel(groupStatus) }}</small></div></div>
        <div v-if="mediaCollectionGroup" class="group-stack-list">
          <template v-for="(item, index) in groupVisibleItems" :key="String(item.itemId ?? index)">
            <article
              class="nodrag nopan"
              :class="[`item-${String(item.status ?? 'draft')}`, { 'is-selected': Boolean(selectedGroupItemId) && selectedGroupItemId === String(item.itemId ?? '') }]"
              :role="itemEditableGroup ? 'button' : undefined"
              :tabindex="itemEditableGroup ? 0 : undefined"
              @mousedown.stop
              @click.stop="selectGroupItem(item)"
              @keydown.enter.prevent="selectGroupItem(item)"
              @keydown.space.prevent="selectGroupItem(item)"
            >
              <header><span><Video v-if="videoCollectionGroup" :size="14" /><ImageIcon v-else :size="14" />{{ groupItemTitle(item, index) }}</span><small>{{ groupItemStatusLabel(item) }}</small></header>
              <div class="group-stack-media" :style="groupItemMediaStyle(item, index)">
                <video
                  v-if="videoCollectionGroup && groupItemPreview(item, index)"
                  :src="groupItemPreview(item, index)"
                  muted
                  playsinline
                  preload="metadata"
                  controls
                  @loadedmetadata="onGroupVideoLoad($event, item, index)"
                />
                <img v-else-if="groupItemPreview(item, index)" :src="groupItemPreview(item, index)" :alt="groupItemTitle(item, index)" @load="onGroupImageLoad($event, item, index)">
                <div v-else class="group-stack-placeholder">
                  <LoaderCircle v-if="['queued', 'running'].includes(String(item.status ?? ''))" :size="24" class="spin" />
                  <CircleAlert v-else-if="String(item.status ?? '') === 'failed'" :size="24" />
                  <Video v-else-if="videoCollectionGroup" :size="24" />
                  <ImageIcon v-else :size="24" />
                  <span>{{ groupItemStatusLabel(item) }}</span>
                </div>
                <i :style="{ width: `${Number(item.progress ?? 0)}%` }" />
              </div>
              <p>{{ groupItemSubtitle(item) }}</p>
              <!-- 编辑面板浮在该资产项上方：早先它是 article 的兄弟节点、
                   作为 grid item 参与布局，会把资产项撑开变形。 -->
              <div
                v-if="itemEditableGroup && selectedGroupItem && selectedGroupItemId === String(item.itemId ?? '')"
                class="group-stack-editor-slot nodrag nopan"
                @click.stop
                @mousedown.stop
              >
                <GroupImageNodePanel
                  :group="groupDetail"
                  :item="selectedGroupItem"
                  :title="selectedGroupItemTitle"
                  @update="emit('update-group', id, $event)"
                  @close="selectedGroupItemId = ''"
                />
              </div>
            </article>
          </template>
        </div>
        <div v-else class="group-item-grid"><span v-for="(item, index) in groupVisibleItems" :key="String(item.itemId ?? index)" :class="`item-${String(item.status ?? 'draft')}`"><template v-if="groupItemPreview(item, index)"><video v-if="groupType === 'video_generator'" :src="groupItemPreview(item, index)" muted playsinline preload="metadata" /><img v-else :src="groupItemPreview(item, index)" alt=""></template><template v-else><Video v-if="groupType === 'video_generator'" :size="15" /><ImageIcon v-else :size="15" /></template><i :style="{ width: `${Number(item.progress ?? 0)}%` }" /><small>{{ Number(index) + 1 }}</small></span><span v-if="groupItems.length > groupPreviewLimit" class="group-more">+{{ groupItems.length - groupPreviewLimit }}</span></div>
      </div>
      <component :is="TextManualBody" v-else-if="manualText" :node-id="id" :params="data.params" :autofocus="manualTextFocus" />
      <!-- 高清子节点还没生成：卡上说清下一步（IMG-F-50）；生成中 / 失败沿用下面的状态层 -->
      <!-- 生成中：空卡片上的提示藏起来（只是看不见、仍占位，卡片不跳高度）—— 此前从半透明的状态层底下透出来，和转圈叠在一起 -->
      <div v-else-if="nodeTool === 'upscale'" class="node-preview-empty node-tool-empty" :class="{ 'is-busy': generationActive }" data-testid="upscale-empty">
        <ImageUpscale :size="42" stroke-width="1.4" />
        <p>配置参数生成高清图像</p>
      </div>
      <div v-else class="node-preview-empty" :class="{ 'is-busy': generationActive }">
        <component :is="data.nodeType === 'video' ? Play : icon" :size="42" stroke-width="1.4" />
        <div><span>尝试：</span><button v-for="item in suggestions[data.nodeType] ?? []" :key="item" type="button" class="nodrag" @mousedown.stop @click.stop="runSuggestion(item)"><component :is="suggestionIcons[item] ?? Sparkles" :size="16" />{{ item }}</button></div>
      </div>
      <div v-if="materialNode && (materialCoverSrc || data.params.materialCode)" class="material-node-caption"><span>{{ data.params.category || (data.nodeType === 'style' ? '风格' : '特效') }}</span><strong>{{ data.params.name || data.title }}</strong><small>{{ data.params.commercial ? '可商用' : '授权受限' }}</small></div>
      <div v-if="textGenerating" class="node-text-progress" role="status">
        <LoaderCircle :size="15" class="spin" aria-hidden="true" />
        <span>{{ progressLabel.head }}</span>
        <!-- 每秒都在变：不让读屏软件每秒念一遍 -->
        <small v-if="progressLabel.detail" aria-hidden="true">{{ progressLabel.detail }}</small>
        <button v-if="generation?.id" type="button" class="nodrag" title="取消生成" aria-label="取消生成" @click.stop="generationStore.cancel(id, generation.id)"><Square :size="9" fill="currentColor" /></button>
      </div>
      <!-- 拿不到进度时显示等了多久（「生成中 · 12 秒」），不再挂着「0%」 -->
      <div v-else-if="generationActive && generation" class="node-status-overlay"><span class="status-ring" /><strong>{{ progressLabel.head }}</strong><small v-if="progressLabel.detail" aria-hidden="true">{{ progressLabel.detail }}</small><button v-if="generation.id" type="button" title="取消生成" @click.stop="generationStore.cancel(id, generation.id)"><Square :size="13" />取消</button></div>
      <div v-if="generation?.status === 'failed'" class="node-status-overlay failed"><strong>{{ generation.errorCode === 'cancelled' ? '已取消' : '生成失败' }}</strong><small>{{ generation.error }}</small><button v-if="generation.id" type="button" title="重试生成" @click.stop="generationStore.retry(id, generation.id)"><RefreshCw :size="13" />重试</button></div>
      <DirectorSourceBadge
        v-if="publishedNode"
        part="chip"
        :node-id="id"
        :params="data.params"
        @locate-director="emit('locate-node', $event)"
        @recapture="(directorId, cameraId) => emit('open-director', directorId, { type: 'recapture', nodeId: id, cameraId })"
      />
      <component :is="SmartcutSourceBadge" v-if="smartcutPublished" part="chip" :node-id="id" :params="data.params" @locate-smartcut="(nodeId: string) => emit('locate-node', nodeId)" />
      <Handle v-if="!documentNode" id="out" type="source" :position="Position.Right" class="aitv-handle" />
    </div>
    <ScriptNodePanel v-if="selected && data.nodeType === 'script'" :node-id="id" :data="data" />
    <!-- 工具条打开的编辑器排在最前：发布出来的节点（导演台 / 智能剪辑）也显示工具条，编辑器要能盖过它们的来源面板 -->
    <component
      :is="TOOL_EDITORS[toolEditor]"
      v-else-if="selected && toolEditor"
      :key="`spawn-${toolEditor}`"
      :node-id="id"
      :source-node-id="id"
      mode="spawn"
      @close="toolEditor = ''"
    />
    <DirectorSourceBadge
      v-else-if="selected && publishedNode"
      part="panel"
      :node-id="id"
      :params="data.params"
      @locate-director="emit('locate-node', $event)"
      @recapture="(directorId, cameraId) => emit('open-director', directorId, { type: 'recapture', nodeId: id, cameraId })"
    />
    <component :is="SmartcutSourceBadge" v-else-if="selected && smartcutPublished" part="panel" :node-id="id" :params="data.params" @locate-smartcut="(nodeId: string) => emit('locate-node', nodeId)" />
    <component :is="DirectorPromptPanel" v-else-if="selected && directorNode" :node-id="id" :data="data" @send="emit('open-director', id, { type: 'build' })" />
    <component
      :is="SmartcutPromptPanel"
      v-else-if="selected && smartcutNode && smartcutAi"
      :node-id="id"
      :data="data"
      @send="emit('open-smartcut', id, { type: 'plan' })"
      @append-unused="emit('open-smartcut', id, { type: 'append-unused' })"
      @open="emit('open-smartcut', id)"
    />
    <component
      :is="TOOL_EDITORS[nodeTool]"
      v-else-if="selected && nodeTool && !nodeToolPanelClosed"
      :key="`node-${nodeTool}`"
      :node-id="id"
      :source-node-id="toolSourceNodeId"
      mode="node"
      @close="nodeToolPanelClosed = true"
    />
    <ParamPanel v-else-if="selected && !nodeTool && data.nodeType !== 'group' && !documentNode && !directorNode && !smartcutNode && !smartcutPublished && !manualText" :node-id="id" :data="data" />
    <!--
      ⚠️ 必须传送到 body：节点在 Vue Flow 带 transform 的视口里，`position: fixed` 的浮层
         放在这里会以视口为参照 —— 跟着画布一起缩放、平移，铺不满屏幕。
    -->
    <Teleport to="body">
      <MediaPreviewDialog
        v-if="preview.opened.value"
        :type="preview.type.value"
        :src="preview.currentSrc.value"
        :title="preview.request.value?.title"
        :subtitle="preview.request.value?.subtitle"
        :loading="preview.loading.value"
        :error="preview.error.value"
        :index="preview.index.value"
        :count="preview.outputs.value.length"
        @close="preview.close"
        @prev="preview.step(-1)"
        @next="preview.step(1)"
        @retry="preview.retry"
      >
        <template #actions>
          <span class="media-kind">{{ preview.type.value }}</span>
          <button type="button" class="product-primary-command" :disabled="!preview.currentSrc.value || preview.loading.value" @click="preview.download">
            <Download :size="15" />下载
          </button>
        </template>
      </MediaPreviewDialog>
    </Teleport>
  </article>
</template>
