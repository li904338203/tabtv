<script setup lang="ts">
/**
 * 3D 导演台全屏编辑器（DRT-09 异步空壳 → DRT-12 引擎 → DRT-15 完整界面）。
 *
 * 布局（DR-UX-01）：顶栏 + 左场景树 + 中视口 + 右侧「属性 / 场景」页（+ 底部时间轴插槽，DRT-24）。
 * 生命周期：挂载 → `store.load` → 挂引擎 → `watch(store.scene) → engine.setScene`；卸载 → 释放引擎 → `store.reset`。
 * 数据流：引擎只投影场景；视口里的操作结束时发事件，这里经 `store.commit` 改数据、记一步撤销，再回灌（DRD-36）。
 *
 * ⚠️ 只能经 `defineAsyncComponent(() => import("~/components/director/DirectorStudio.vue"))` 引用
 *    （frontend/director/README.md）：写成标签会让 three 进画布首屏的包。
 * ⚠️ 卸载即释放引擎（DRD-34）：漏掉的话 WebGL 上下文越积越多，浏览器会开始丢别的上下文。
 * ⚠️ 上下文丢失后「重新加载编辑器」要换一个新的 canvas（`viewportKey` 让视口整块重建）；场景数据在 store 里，不丢。
 * ⚠️ 键盘分两道（DR-UX-04）：画布那边的布尔挡一道；这里在 window **捕获阶段**截断编辑器打开期间的按键，
 *    画布在冒泡阶段永远收不到。编辑器里的输入框例外：先放行到输入框（它要自己的回车 / Esc），
 *    再由根元素在冒泡阶段截断 —— 在捕获阶段一律截断的话，输入框自己的按键处理一个都收不到。
 * ⚠️ 返回画布要先把场景存上（DR-F-154）：冲突、离线、保存失败时不关，展开提示条。
 */
import "~/assets/css/director.css";
import { h } from "vue";
import { ElMessage, ElMessageBox } from "element-plus";
import { Aperture, Camera as CameraIcon, Clapperboard, Sparkles } from "@lucide/vue";
import type { components } from "@tabtv/api-client";
import { directorCameraSnapshot, directorCaptureForm, directorCurrentCameraId, directorDepthFilename } from "~/director/capture";
import { DirectorCaptureError } from "~/director/engine/Capture";
import { DirectorEngine } from "~/director/engine/DirectorEngine";
import { DIRECTOR_PREVIEW_UNSUPPORTED, directorCanRecordPreview } from "~/director/engine/Recorder";
import type { DirectorTransformMode, DirectorTransformPayload } from "~/director/engine/events";
import { createDirectorEngineHandle } from "~/director/engine/handle";
import { createDirectorMediaFetcher } from "~/director/media";
import { DirectorFlushError, directorRemoteNoticeText } from "~/stores/director";
import type { UploadedReference } from "~/utils/canvas";
import { isEditableTarget } from "~/utils/canvasShortcuts";
import { directorBuildEntryReason } from "~/utils/directorBuild";
import { directorFailureText, type DirectorUpstreamImage } from "~/utils/directorCanvas";
import { directorLookAtRotation, type DirectorRect } from "~/utils/directorCamera";
import type { DirectorCapturedEvent, DirectorPendingAction, DirectorPublishEvent } from "~/utils/directorPublish";
import {
  directorAddObject,
  directorDuplicateObject,
  directorFindObject,
  directorPatchSettings,
  directorRemoveObject,
  directorSetTransform,
  type DirectorLimitError,
  type DirectorObject,
  type DirectorScene
} from "~/utils/directorScene";
import { directorDisplayedObject, directorDisplayedScene, directorEditTimeline, directorTimelineOf, type DirectorKeyRef } from "~/utils/directorAnimation";
import { directorShotText } from "~/utils/directorCamera";
import { directorResetJoint, directorSetPose } from "~/utils/directorPose";
import { directorMoveKeyframe, directorTimelineErrorText } from "~/utils/directorTimeline";
import { directorShortcutAction, type DirectorShortcutAction } from "~/utils/directorShortcuts";
import { directorCloseBlocker, directorReadonlyText, type DirectorPlacement } from "~/utils/directorStudio";
import AiBuildBar from "./AiBuildBar.vue";
import CaptureStrip from "./CaptureStrip.vue";
import PosePanel from "./PosePanel.vue";
import PreviewExportDialog from "./PreviewExportDialog.vue";
import PoseFromImageDialog from "./PoseFromImageDialog.vue";
import { DirectorPoseDetector } from "~/director/PoseDetector";
import TimelinePanel from "./TimelinePanel.vue";
import DirectorConflictBar from "./DirectorConflictBar.vue";
import SceneSettings from "./SceneSettings.vue";
import SceneTree from "./SceneTree.vue";
import StudioGuide from "./StudioGuide.vue";
import StudioTopbar from "./StudioTopbar.vue";
import ViewportChrome from "./ViewportChrome.vue";
import CameraProps from "./props/CameraProps.vue";
import CharacterProps from "./props/CharacterProps.vue";
import PropProps from "./props/PropProps.vue";
import CrowdProps from "./props/CrowdProps.vue";
import TransformFields from "./props/TransformFields.vue";

const props = defineProps<{
  projectId: string;
  sceneId: string;
  nodeId: string;
  nodeTitle: string;
  /** 指向本节点的上游图片（全景背景的候选，dev/03 §6.6）；画布连线变了实时更新。 */
  upstreamImages: DirectorUpstreamImage[];
  /** 打开后要顺手做的事：重新截图（DRT-17）、按节点描述自动搭景一次（DRT-20）。 */
  pendingAction?: DirectorPendingAction;
  /** AI 搭景条的默认内容：节点上的描述与参考图（DRT-20）。 */
  buildDefaults?: { prompt: string; uploads: UploadedReference[] };
}>();
// `captured` / `publish` 由截图流程发（DRT-17）
const emit = defineEmits<{ close: []; captured: [event: DirectorCapturedEvent]; publish: [event: DirectorPublishEvent] }>();

type MediaRecord = components["schemas"]["DirectorMediaRecord"];

const store = useDirectorStore();
/**
 * 窄于这个宽度只提示换大屏（DR-UX-05）。画布页本身在 1024 以下整页隐藏，所以实际发生的是「编辑器开着时窗口被拉窄」：
 * 盖一层提示、引擎不动；一开始就窄的话等变宽了再挂引擎。
 */
const MIN_WIDTH = 1024;
const PREVIEW_SIZE = { width: 288, height: 162 };

const rootEl = ref<HTMLElement | null>(null);
const viewportEl = ref<HTMLElement | null>(null);
const canvasEl = ref<HTMLCanvasElement | null>(null);
const labelsEl = ref<HTMLElement | null>(null);
const viewportKey = ref(0);
const contextLost = ref(false);
const smallScreen = ref(false);
const loadError = ref("");
const selectedId = ref<string | null>(null);
const viewMode = ref<"director" | "camera">("director");
const viewCameraId = ref<string | null>(null);
const transformMode = ref<DirectorTransformMode>("translate");
const walking = ref(false);
const previewOn = ref(false);
const frameRect = ref<DirectorRect | null>(null);
const previewRect = ref<DirectorRect | null>(null);
const barOpen = ref(true);
const closing = ref(false);
const tab = ref<"props" | "pose" | "scene">("props");
/** 关节模式（DRT-21）：只对选中的那个角色有效；换了选中就退出。正在调的关节由视口点选或姿势页签的清单给出。 */
const jointMode = ref(false);
const jointBone = ref<string | null>(null);
/** 导出预演对话框（DRT-25）。 */
const previewOpen = ref(false);
/** 从图片识别姿势（DRT-23）：对话框作用于打开时选中的角色。 */
const poseImageId = ref<string | null>(null);
const poseImageObject = computed(() => {
  const object = poseImageId.value ? directorFindObject(store.scene, poseImageId.value) : undefined;
  return object?.kind === "character" ? object : undefined;
});
/**
 * 姿势识别的 Worker 第一次用时才建，整个编辑器会话共用（模型只下载一次）；登记进引擎的 Disposer，
 * 编辑器关闭时随引擎一起终止。
 */
let poseDetector: DirectorPoseDetector | null = null;
function poseDetectorOf(): DirectorPoseDetector {
  if (!poseDetector) {
    const detector = new DirectorPoseDetector();
    poseDetector = detector;
    engine.current()?.onDispose(() => {
      detector.dispose();
      if (poseDetector === detector) poseDetector = null;
    });
  }
  return poseDetector;
}
function openPoseFromImage() {
  if (selected.value?.kind === "character" && editable.value) poseImageId.value = selected.value.id;
}
function onPoseApplied() {
  ElMessage.success("已按图片摆好姿势：可以在「姿势」页签用关节模式继续微调");
}
/** 时间轴上选中的关键帧（DRT-24）：← / → 挪 1 帧、头部显示插值与删除。 */
const selectedKey = ref<DirectorKeyRef | null>(null);
const adjustPanorama = ref(false);
const capturing = ref(false);
/** AI 搭景条是否展开；有提案时一直展开（应用 / 放弃都在条里，DRT-20）。 */
const aiOpen = ref(false);
/** 从节点面板「发送」打开的：条第一次挂载时自动发起一次（只此一次）。 */
const aiAutoStart = ref(false);

const engine = createDirectorEngineHandle(() => new DirectorEngine());
const fetchMediaBlob = createDirectorMediaFetcher();

const readonlyText = computed(() => directorReadonlyText(store.readonlyReason));
const selected = computed(() => directorFindObject(store.scene, selectedId.value));
/** 选中对象在播放头这一帧显示的样子（有轨道时是采样值，DRT-24）：数值框与焦距按它显示、也以它为底改。 */
const selectedShown = computed(() => (selectedId.value ? directorDisplayedObject(store.scene, selectedId.value, store.playhead) ?? null : null));
/** 选中对象有没有关键帧：有的话属性面板顶部说清「改动会写入第 N 帧」（自动打帧，DRT-24）。 */
const selectedTracked = computed(() => Boolean(selectedId.value && directorTimelineOf(store.scene).tracks.some(t => t.objectId === selectedId.value)));
const cameras = computed(() => (store.scene?.objects ?? []).filter(o => o.kind === "camera").map(o => ({ id: o.id, name: o.name })));
const editable = computed(() => !store.readonly && !store.proposal);
/** 关节模式实际打开在哪个角色上：只读、提案预览中关掉（恢复后接着用）。 */
const jointCharacterId = computed(() => (jointMode.value && editable.value && selected.value?.kind === "character" ? selected.value.id : null));
const workspace = useWorkspaceStore();
const buildConfig = useDirectorBuildConfig();
/** 顶栏「AI 搭景」为什么点不了（DR-AC-32）：编辑器只读的原因在前，再看权限与服务端可用性。 */
const aiEntryReason = computed(() => (store.readonly
  ? readonlyText.value || "当前不能编辑"
  : directorBuildEntryReason({ config: buildConfig.config.value, role: workspace.current?.role })));
const aiBarShown = computed(() => store.loaded && (aiOpen.value || Boolean(store.proposal)));

// ---------------------------------------------------------------- 引擎

function attach() {
  if (!canvasEl.value) return;
  const e = engine.attach(canvasEl.value, labelsEl.value, { fetchMediaBlob });
  e.on("context-lost", () => {
    contextLost.value = true;
  });
  e.on("select", ({ id }) => {
    selectedId.value = id;
    syncPreviewCamera();
  });
  e.on("transform-end", ({ id, transform }) => commitTransform(id, transform));
  e.on("pose-end", event => commitPose(event));
  e.on("joint-select", ({ bone }) => {
    jointBone.value = bone;
  });
  e.on("frame", ({ frame, playing }) => {
    store.playing = playing;
    store.playhead = frame;
  });
  e.on("camera-moved", ({ id, transform }) => commitTransform(id, transform));
  e.on("mode-rejected", ({ kind }) => {
    transformMode.value = "translate";
    ElMessage.info(kind === "character" ? "角色请用身高调整大小" : "这个对象不能缩放");
  });
  // 引擎要告诉用户的话（DRT-28：泼溅加载失败、脚下的泼溅太稀已贴到地面）
  e.on("notice", ({ level, text }) => (level === "error" ? ElMessage.error(text) : ElMessage.info(text)));
  e.setEditable(editable.value);
  if (store.scene) e.setScene(store.scene);
  // 重新挂载（上下文丢失后重载）时把关节模式与播放头接回去
  e.setJointMode(jointCharacterId.value);
  e.selectJoint(jointBone.value);
  e.seek(store.playhead);
  syncView();
}

/**
 * 视口里的拖拽 / 走位结束：经 store 改数据（一步撤销）。store 不采纳时（只读、提案预览中、值没变）
 * 主动回灌一次，把对象拉回 store 里的样子 —— 场景没变的话 watch 不会触发。
 */
function commitTransform(id: string, transform: DirectorTransformPayload) {
  const accepted = store.commit(scene => directorSetTransform(scene, id, transform), { touched: [id] });
  if (!accepted && store.scene) engine.current()?.setScene(store.scene);
}

async function reload() {
  engine.detach();
  contextLost.value = false;
  viewportKey.value++;
  await nextTick();
  attach();
}

watch(() => store.scene, (scene) => {
  // 提案预览期间引擎投影的是建议场景：提案结束时由上面那个 watch 投影回来
  if (scene && !store.proposal) engine.current()?.setScene(scene);
  // 选中的对象被删了（撤销、远端更新）：取消选中
  if (selectedId.value && !directorFindObject(scene, selectedId.value)) selectObject(null);
});
watch(editable, on => engine.current()?.setEditable(on));
watch(jointCharacterId, (id) => {
  jointBone.value = null;
  engine.current()?.setJointMode(id);
});
watch(jointBone, bone => engine.current()?.selectJoint(bone));
// 播放头 → 引擎（拖标尺、点关键帧、挪关键帧）。播放中由引擎推进、经 frame 事件写回来，这里 seek 到同一帧是空操作
watch(() => store.playhead, frame => engine.current()?.seek(frame));
watch(() => store.timelineNotice, (notice) => {
  if (notice) ElMessage.warning(notice.text);
});
// 编辑器开着时 Agent / MCP / 同事改了场景、本地没有未保存的改动：store 已经自动载入最新并清空撤销栈，
// 这里说一句是谁改的（DR-F-155）。不说的话画面在用户眼前变了样、撤销也按不回去，而他不知道发生了什么。
// 按 seq 触发：同一个来源连着改两次，提示也要出现两次。
watch(() => store.remoteNotice?.seq, (seq) => {
  if (seq) ElMessage.info(directorRemoteNoticeText(store.remoteNotice));
});
// 换了选中的对象就退出关节模式；选中的不是角色时「姿势」页签也收起
watch(selectedId, () => {
  jointMode.value = false;
  if (tab.value === "pose" && selected.value?.kind !== "character") tab.value = "props";
});
/**
 * AI 提案的预览（DR-F-102）：有提案时引擎投影建议场景、加三色标记，属性面板清掉选中（面板显示的是当前场景，
 * 不是建议场景）；提案没了（应用 / 放弃 / 远端更新使它作废）就摘掉标记、投影回 store 里的当前场景。
 */
watch(() => store.proposal, (proposal) => {
  const e = engine.current();
  if (proposal) {
    aiOpen.value = true;
    selectObject(null);
    e?.setProposal(proposal.scene, proposal.changes);
    return;
  }
  e?.clearProposal();
  if (store.scene) e?.setScene(store.scene);
});
watch(() => store.saveState, (state) => {
  if (state === "conflict" || state === "offline" || state === "error") barOpen.value = true;
});
watch(() => props.upstreamImages, () => reconcilePanorama(), { deep: true });

// ---------------------------------------------------------------- 视角与叠加控件

function syncView() {
  const e = engine.current();
  if (!e) return;
  const view = e.getViewMode();
  viewMode.value = view.mode;
  viewCameraId.value = view.cameraId;
  refreshRects();
}

function setView(view: { mode: "director" | "camera"; cameraId: string | null }) {
  const e = engine.current();
  if (!e) return;
  e.setViewMode({ mode: view.mode, cameraId: view.cameraId ?? selectedCameraId() });
  syncView();
  if (viewMode.value !== "camera" && walking.value) walking.value = e.setWalk(false);
}

function selectedCameraId(): string | null {
  return selected.value?.kind === "camera" ? selected.value.id : null;
}

function refreshRects() {
  requestAnimationFrame(() => {
    frameRect.value = engine.current()?.getFrameRect() ?? null;
    const el = viewportEl.value;
    if (!el || !previewOn.value) {
      previewRect.value = null;
    } else {
      previewRect.value = {
        x: Math.max(0, el.clientWidth - PREVIEW_SIZE.width - 16),
        y: Math.max(0, el.clientHeight - PREVIEW_SIZE.height - 16),
        ...PREVIEW_SIZE
      };
    }
    engine.current()?.setPreviewRect(previewRect.value);
  });
}

function togglePreview() {
  previewOn.value = !previewOn.value;
  syncPreviewCamera();
  refreshRects();
}

/** 预览小窗显示输出机位；选中别的机位时显示它（DR-F-22）。 */
function syncPreviewCamera() {
  engine.current()?.setPreviewCamera(selectedCameraId());
}

function toggleWalk() {
  const e = engine.current();
  if (!e || store.readonly) return;
  walking.value = e.setWalk(!walking.value);
  if (!walking.value) e.clearWalkKeys();
}

function setMode(mode: DirectorTransformMode) {
  if (!editable.value) return;
  transformMode.value = engine.current()?.setTransformMode(mode) ?? mode;
}

function selectObject(id: string | null) {
  // 提案预览期间视口里是建议场景，属性面板却是当前场景：选中哪个都对不上，干脆不选（DR-AC-27）
  if (id && store.proposal) return;
  selectedId.value = id;
  engine.current()?.select(id);
  syncPreviewCamera();
  if (id) tab.value = "props";
}

/**
 * 视口里的一次姿势编辑结束（DRT-21）：骨骼覆盖、角色变换、脚底锁定一步提交（一步撤销）。
 * store 不采纳时（只读、提案预览中）回灌一次，把骨骼拉回 store 里的样子。
 */
function commitPose(event: { id: string; boneOverrides: Record<string, { x: number; y: number; z: number }>; transform?: Parameters<typeof directorSetTransform>[2]; footLock?: { left: boolean; right: boolean }; released?: Array<"left" | "right"> }) {
  const accepted = store.commit(scene => directorSetPose(scene, event.id, event), { touched: [event.id] });
  if (!accepted && store.scene) engine.current()?.setScene(store.scene);
  if (accepted && event.released?.length) {
    const feet = event.released.map(side => (side === "left" ? "左脚" : "右脚")).join("、");
    ElMessage.info(`${feet}已解锁：腿伸直也够不着原来的位置`);
  }
}

// ---------------------------------------------------------------- 时间轴（DRT-24）

function play() {
  engine.current()?.play();
}

function pause() {
  engine.current()?.pause();
}

function seek(frame: number) {
  store.playhead = Math.max(0, Math.round(frame));
}

/** ← / →：把选中的关键帧挪 1 帧（同轨有关键帧占着时说明，不合并）。 */
function nudgeKey(delta: -1 | 1) {
  const key = selectedKey.value;
  if (!key) return;
  let failure = "";
  const to = key.frame + delta;
  const accepted = store.commit((scene) => {
    const result = directorEditTimeline(scene, t => directorMoveKeyframe(t, key.objectId, key.channel, key.frame, to));
    if ("error" in result) {
      failure = directorTimelineErrorText(result);
      return scene;
    }
    return result.scene;
  });
  if (failure) ElMessage.warning(failure);
  if (accepted) {
    selectedKey.value = { ...key, frame: to };
    store.playhead = to;
  }
}

function toggleJointMode() {
  jointMode.value = !jointMode.value;
  tab.value = "pose";
}

/** 关节模式下的 Delete / 「重置这个关节」：只去掉这根骨骼的覆盖（回到姿势里的样子），不删角色。 */
function resetJoint() {
  const id = jointCharacterId.value;
  const bone = jointBone.value;
  if (!id || !bone) return;
  store.commit(scene => directorResetJoint(scene, id, bone), { touched: [id] });
}

/**
 * 展开 / 收起 AI 搭景条。⚠️ 自动发起只对「节点面板点了发送」那一次挂载有效：手动收起再展开时清掉标志，
 * 否则重新挂载的条会把那句描述再发一遍。
 */
function toggleAi(open = !aiOpen.value) {
  // 入口置灰时点不开（DR-AC-32）：按钮的 disabled 只拦得住鼠标，拦不住脚本与辅助技术派发的点击
  if (open && aiEntryReason.value) return;
  aiOpen.value = open;
  aiAutoStart.value = false;
}

/** 新对象放哪：角色、道具放在视口中心看向的地面上，机位用当前导演视角（DR-F-11、DR-F-26）。 */
function placement(): DirectorPlacement {
  const e = engine.current();
  return { ground: e?.groundPointAtViewCenter() ?? null, view: e?.getDirectorView() ?? null };
}

// ---------------------------------------------------------------- 机位动作（DR-F-26）

function limitText(error: DirectorLimitError): string {
  return error.what === "objects" ? `场景里最多 ${error.limit} 个对象` : "场景内容已到上限，请先删掉一些对象";
}

function aimCamera(targetId: string) {
  const e = engine.current();
  const camera = selected.value;
  if (!e || camera?.kind !== "camera") return;
  const center = e.getObjectCenter(targetId);
  const rotation = center ? directorLookAtRotation(camera.transform.position, center) : null;
  if (!rotation) return;
  store.commit(scene => directorSetTransform(scene, camera.id, { ...camera.transform, rotation }), { touched: [camera.id] });
}

function cameraFromView() {
  const e = engine.current();
  if (!e || !editable.value) return;
  const view = e.getDirectorView();
  const outcome: { id: string; limit: DirectorLimitError | null } = { id: "", limit: null };
  store.commit((scene: DirectorScene) => {
    const result = directorAddObject(scene, "camera");
    if ("error" in result) {
      outcome.limit = result;
      return scene;
    }
    outcome.id = result.object.id;
    return directorSetTransform(result.scene, result.object.id, view);
  });
  if (outcome.limit) ElMessage.warning(limitText(outcome.limit));
  else if (outcome.id) selectObject(outcome.id);
}

// ---------------------------------------------------------------- 键盘（DR-UX-03 / 04）

function onKeyCapture(event: KeyboardEvent) {
  const target = event.target;
  const inside = target instanceof Node && Boolean(rootEl.value?.contains(target));
  // Element Plus 的弹窗（确认框等）传送到了 body：放行，让它自己处理回车 / Esc（画布那边的布尔照样挡着）
  if (target instanceof Element && target.closest(".el-overlay, .el-message-box, .el-popper")) return;
  // 编辑器里的输入框：先放行到它自己，由根元素在冒泡阶段截断
  if (inside && isEditableTarget(target)) return;
  event.stopPropagation();
  if (smallScreen.value || !store.loaded) return;
  // 导出对话框开着时编辑器的快捷键都不响应（导出途中按空格播放、按 Delete 删对象都会把这次导出搅乱）；
  // 识别姿势的对话框同理（按 Delete 会删掉正要摆姿势的角色）
  if (previewOpen.value || poseImageId.value) return;
  const action = directorShortcutAction(event, {
    walking: walking.value,
    readonly: store.readonly,
    hasSelection: Boolean(selectedId.value),
    proposal: Boolean(store.proposal),
    jointMode: Boolean(jointCharacterId.value),
    jointSelected: Boolean(jointBone.value),
    keyframeSelected: Boolean(selectedKey.value)
  });
  if (!action) return;
  if (action.type !== "walk") event.preventDefault();
  dispatch(action);
}

/** 根元素冒泡阶段：编辑器里的按键（含输入框里的）一律不再往上传，画布收不到。 */
function onRootKey(event: KeyboardEvent) {
  event.stopPropagation();
}

function dispatch(action: DirectorShortcutAction) {
  const e = engine.current();
  switch (action.type) {
    case "walk":
      e?.walkKey(action.code, action.down);
      return;
    case "mode":
      setMode(action.mode);
      return;
    case "focus":
      if (selectedId.value) e?.focusObject(selectedId.value);
      return;
    case "reset-joint":
      resetJoint();
      return;
    case "play":
      if (store.playing) pause();
      else play();
      return;
    case "nudge-key":
      nudgeKey(action.delta);
      return;
    case "delete": {
      const id = selectedId.value;
      if (id && store.commit(scene => directorRemoveObject(scene, id))) selectObject(null);
      return;
    }
    case "duplicate":
      duplicateSelected();
      return;
    case "undo":
      store.undo();
      return;
    case "redo":
      store.redo();
      return;
    case "cancel":
      cancelCurrent();
  }
}

function duplicateSelected() {
  const id = selectedId.value;
  if (!id) return;
  const outcome: { id: string; limit: DirectorLimitError | null } = { id: "", limit: null };
  store.commit((scene: DirectorScene) => {
    const result = directorDuplicateObject(scene, id);
    if ("error" in result) {
      if (result.error === "limit") outcome.limit = result as DirectorLimitError;
      return scene;
    }
    outcome.id = result.object.id;
    return result.scene;
  });
  if (outcome.limit) ElMessage.warning(limitText(outcome.limit));
  else if (outcome.id) selectObject(outcome.id);
}

/** Esc：只取消当前操作，不关闭编辑器（DR-UX-02）。顺序：走位 → 视口里调全景 → 提案预览 → 选中的关节 → 关节模式。 */
function cancelCurrent() {
  const e = engine.current();
  if (walking.value && e) {
    walking.value = e.setWalk(false);
    e.clearWalkKeys();
  } else if (adjustPanorama.value) {
    endPanoramaAdjust(false);
  } else if (store.proposal) {
    store.discardProposal();
  } else if (jointBone.value) {
    jointBone.value = null;
  } else if (jointMode.value) {
    jointMode.value = false;
  }
}

function clearWalkKeys() {
  engine.current()?.clearWalkKeys();
}

function onVisibility() {
  if (document.visibilityState !== "visible") clearWalkKeys();
}

// ---------------------------------------------------------------- 全景（DR-F-61、dev/03 §6.6）

type Panorama = { sourceNodeId: string; mediaId: string; yaw?: number; radius?: number };

function currentPanorama(): Panorama | null {
  const background = store.scene?.background as { mode?: string; panorama?: Panorama } | undefined;
  return background?.mode === "panorama" && background.panorama ? background.panorama : null;
}

/**
 * 每次打开（以及上游变化时）按 `sourceNodeId` 重新解析全景：上游换了产出就跟着换（记一步）；
 * 连线已经断开就回落纯色并提示（DR-AC-10）。只读时不动数据。
 */
function reconcilePanorama() {
  const panorama = currentPanorama();
  if (!panorama || !store.loaded || !editable.value) return;
  const upstream = props.upstreamImages.find(item => item.sourceNodeId === panorama.sourceNodeId);
  if (!upstream) {
    store.commit(scene => directorPatchSettings(scene, "background", { mode: "color" }));
    ElMessage.warning("全景来源已断开，背景已换回纯色");
    return;
  }
  if (upstream.mediaId && upstream.mediaId !== panorama.mediaId) {
    store.commit(scene => directorPatchSettings(scene, "background", { panorama: { ...panorama, mediaId: upstream.mediaId } }));
  }
}

/** 视口里调全景：拖动改水平旋转、滚轮改半径，期间只投影不提交，松手（或滚轮停下）时提交一次。 */
let adjustDrag: { x: number; yaw: number } | null = null;
let adjustDraft: Partial<Panorama> = {};
let wheelTimer: ReturnType<typeof setTimeout> | undefined;

function previewPanorama(patch: Partial<Panorama>) {
  const panorama = currentPanorama();
  if (!panorama || !store.scene) return;
  adjustDraft = { ...adjustDraft, ...patch };
  const background = { ...(store.scene.background as object), panorama: { ...panorama, ...adjustDraft } };
  engine.current()?.setScene({ ...store.scene, background });
}

function endPanoramaAdjust(commit: boolean) {
  clearTimeout(wheelTimer);
  adjustDrag = null;
  const panorama = currentPanorama();
  const draft = adjustDraft;
  adjustDraft = {};
  if (commit && panorama && Object.keys(draft).length) {
    store.commit(scene => directorPatchSettings(scene, "background", { panorama: { ...panorama, ...draft } }));
  } else if (store.scene) {
    engine.current()?.setScene(store.scene);
  }
  if (!commit) adjustPanorama.value = false;
}

function onAdjustDown(event: PointerEvent) {
  const panorama = currentPanorama();
  if (!panorama) return;
  adjustDrag = { x: event.clientX, yaw: Number(panorama.yaw ?? 0) };
  (event.currentTarget as Element).setPointerCapture(event.pointerId);
}

function onAdjustMove(event: PointerEvent) {
  if (!adjustDrag) return;
  const yaw = ((adjustDrag.yaw + (event.clientX - adjustDrag.x) * 0.25) % 360 + 360) % 360;
  previewPanorama({ yaw: Math.round(yaw) });
}

function onAdjustUp() {
  if (adjustDrag) endPanoramaAdjust(true);
}

function onAdjustWheel(event: WheelEvent) {
  const panorama = currentPanorama();
  if (!panorama) return;
  const current = Number(adjustDraft.radius ?? panorama.radius ?? 50);
  previewPanorama({ radius: Math.min(200, Math.max(5, Math.round(current + Math.sign(event.deltaY) * 2))) });
  clearTimeout(wheelTimer);
  wheelTimer = setTimeout(() => endPanoramaAdjust(true), 400);
}

// ---------------------------------------------------------------- 截图与发布（DRT-17，dev/03 §7）

const currentCameraId = computed(() => directorCurrentCameraId({
  viewCameraId: viewMode.value === "camera" ? viewCameraId.value : null,
  selected: selected.value,
  outputCameraId: store.scene?.outputCameraId,
  cameraIds: cameras.value.map(camera => camera.id)
}));
/** 截图按钮为什么点不了；能点时为空串（DR-F-76：viewer 不能截图；提案预览中场景锁着）。 */
/** 「导出预演」为什么点不了（DR-F-129）：只读、提案预览中、没有机位、浏览器不能编码视频。 */
const previewBlocked = computed(() => {
  if (store.readonly) return readonlyText.value;
  if (store.proposal) return "请先应用或放弃 AI 提案";
  if (!cameras.value.length) return "场景里还没有机位";
  if (!directorCanRecordPreview()) return DIRECTOR_PREVIEW_UNSUPPORTED;
  return "";
});

function exportPreview(cameraId: string, signal: AbortSignal, onProgress: (done: number, total: number) => void) {
  const e = engine.current();
  if (!e) return Promise.reject(new Error("编辑器还没准备好"));
  return e.exportPreview({ cameraId, signal, onProgress });
}

const captureBlocked = computed(() => {
  if (store.readonly) return readonlyText.value;
  if (store.proposal) return "请先应用或放弃 AI 提案";
  if (!cameras.value.length) return "场景里还没有机位";
  if (!store.loaded) return "场景还在载入";
  return "";
});

/**
 * 截图（DR-F-70~74）：先存盘（存不上就不截，DR-F-73）→ 逐个机位离屏截图 → 带「刚存上的版本」上传 →
 * 记录插到截图条最前面 → 通知画布回写节点（输出机位的截图成为节点的输出与封面）→ toast 带「发布到画布」。
 * 上传撞上版本冲突（截图前刚好有人保存）：不留记录、进冲突流程，提示重新截图（DR-AC-16 / 18）。
 * `replaceNodeId`：「重新截图」时只替换这个发布节点（DR-F-84）。
 */
async function runCapture(ids: string[], options: { replaceNodeId?: string } = {}) {
  const e = engine.current();
  if (capturing.value || !ids.length || !e) return;
  if (captureBlocked.value) {
    ElMessage.warning(captureBlocked.value);
    return;
  }
  capturing.value = true;
  try {
    let version: number;
    try {
      version = await store.flush();
    } catch (cause) {
      barOpen.value = true;
      ElMessage.warning(`没有截图：${cause instanceof DirectorFlushError ? cause.message : "场景还没存上"}`);
      return;
    }
    const scene = store.scene as DirectorScene;
    // 截下来的是播放头这一帧的画面：机位快照与镜头文字也按这一帧（有关键帧的机位此刻不在基础位置上）
    const shown = directorDisplayedScene(scene, store.playhead);
    const records: MediaRecord[] = [];
    for (const id of ids) {
      const object = directorFindObject(shown, id);
      if (object?.kind !== "camera") continue;
      const shot = await e.capture(id);
      const form = directorCaptureForm({ blob: shot.blob, kind: "capture", cameraId: id, sceneVersion: version, camera: directorCameraSnapshot(object) });
      try {
        records.push(await useApi()<MediaRecord>(`/projects/${props.projectId}/director-scenes/${props.sceneId}/captures`, { method: "POST", body: form }));
      } catch (cause) {
        if (store.handleUploadConflict(cause)) {
          barOpen.value = true;
          ElMessage.warning("场景刚被修改，请重新截图");
        } else {
          ElMessage.error(directorFailureText(cause, "截图上传失败，请重试"));
        }
        break;
      }
    }
    if (!records.length) return;
    store.addRecords(records);
    // 镜头文字在这里算好带给画布（DRT-26）：画布只存摘要，不必加载镜头文字的算法与场景
    const shotTexts = Object.fromEntries(records.flatMap(r => (r.cameraId ? [[r.cameraId, directorShotText({ scene: shown, cameraId: r.cameraId })]] : [])));
    emit("captured", { records, scene, shotTexts, ...(options.replaceNodeId ? { replaceNodeId: options.replaceNodeId } : {}) });
    ElMessage({
      type: "success",
      duration: 6000,
      showClose: true,
      message: h("span", { class: "director-toast" }, [
        options.replaceNodeId ? "已重新截图，已替换画布上的节点" : `已截图 ${records.length} 张`,
        options.replaceNodeId ? null : h("button", { type: "button", class: "director-toast-action", onClick: () => publishRecords(records) }, "发布到画布")
      ])
    });
  } catch (cause) {
    if (cause instanceof DirectorCaptureError && cause.reason === "camera_missing") ElMessage.warning("该机位已删除");
    else ElMessage.error(cause instanceof DirectorCaptureError ? cause.message : "截图失败，请重试");
  } finally {
    capturing.value = false;
  }
}

/**
 * 截当前机位的控制图（DRT-27，DR-F-133）：深度图或骨架图。与截图同规则：先存盘、带最新版本上传（DRD-14），
 * 机位快照按播放头这一帧；深度图把近远写进快照（`camera.depth`）与文件名。骨架图画面里没有单人角色时不出；
 * 有群众时说明群众不出骨架（实例化渲染，没有逐人的骨骼）。
 */
async function runControlCapture(kind: "depth" | "pose") {
  const e = engine.current();
  const id = currentCameraId.value;
  if (capturing.value || !e || !id) return;
  if (captureBlocked.value) {
    ElMessage.warning(captureBlocked.value);
    return;
  }
  capturing.value = true;
  try {
    let version: number;
    try {
      version = await store.flush();
    } catch (cause) {
      barOpen.value = true;
      ElMessage.warning(`没有截取：${cause instanceof DirectorFlushError ? cause.message : "场景还没存上"}`);
      return;
    }
    const shown = directorDisplayedScene(store.scene as DirectorScene, store.playhead);
    const object = directorFindObject(shown, id);
    if (object?.kind !== "camera") return;
    const map = await e.captureControlMap(id, kind);
    if (kind === "pose" && !map.people?.length) {
      ElMessage.warning(map.crowds ? "画面里只有群众：群众不出骨架，没有可画的人" : "画面里没有角色，骨架图是空的，没有上传");
      return;
    }
    const snapshot = directorCameraSnapshot(object);
    const camera = kind === "depth" ? { ...snapshot, depth: { near: map.near, far: map.far } } : snapshot;
    const filename = kind === "depth" ? directorDepthFilename(object.name, map.near ?? 0, map.far ?? 0) : undefined;
    const form = directorCaptureForm({ blob: map.blob, kind, cameraId: id, sceneVersion: version, camera, ...(filename ? { filename } : {}) });
    let record: MediaRecord;
    try {
      record = await useApi()<MediaRecord>(`/projects/${props.projectId}/director-scenes/${props.sceneId}/captures`, { method: "POST", body: form });
    } catch (cause) {
      if (store.handleUploadConflict(cause)) {
        barOpen.value = true;
        ElMessage.warning("场景刚被修改，请重新截取");
      } else {
        ElMessage.error(directorFailureText(cause, "上传失败，请重试"));
      }
      return;
    }
    store.addRecords([record]);
    // 群众不出骨架（实例化渲染，没有逐人的骨骼）：写进这条提示里，不另弹一条一闪而过的
    const label = kind === "depth"
      ? `已截深度图（近 ${map.near?.toFixed(2)} 米、远 ${map.far?.toFixed(2)} 米）`
      : `已截骨架图（${map.people?.length ?? 0} 人${map.crowds ? "；群众不出骨架" : ""}）`;
    ElMessage({
      type: "success",
      duration: 6000,
      showClose: true,
      message: h("span", { class: "director-toast" }, [label, h("button", { type: "button", class: "director-toast-action", onClick: () => publishRecords([record]) }, "发布到画布")])
    });
  } catch (cause) {
    if (cause instanceof DirectorCaptureError && cause.reason === "camera_missing") ElMessage.warning("该机位已删除");
    else ElMessage.error(cause instanceof DirectorCaptureError ? cause.message : "截取失败，请重试");
  } finally {
    capturing.value = false;
  }
}

function captureCurrent() {
  if (currentCameraId.value) void runCapture([currentCameraId.value]);
}

function captureAll() {
  void runCapture(cameras.value.map(camera => camera.id));
}

/**
 * 一张截图的镜头文字（DRT-26）：机位按记录里的快照（截图那一刻的位置、朝向、焦距），画面里的对象按现在这一帧。
 * 快照里的机位后来删了也照样说得出来。
 */
function shotTextOfRecord(record: MediaRecord): string {
  const snap = record.camera as Record<string, unknown> | null;
  if (!record.cameraId || !snap || !store.scene) return "";
  const shown = directorDisplayedScene(store.scene, store.playhead);
  const camera = {
    id: record.cameraId, name: record.cameraName ?? "机位", kind: "camera", source: "user", visible: true, locked: false,
    transform: { position: snap.position, rotation: snap.rotation, scale: { x: 1, y: 1, z: 1 } },
    camera: { focalLengthMm: snap.focalLengthMm, sensorWidthMm: snap.sensorWidthMm, aspect: snap.aspect, roll: snap.roll }
  } as unknown as DirectorObject;
  return directorShotText({ scene: { ...shown, objects: [...(shown.objects ?? []).filter(o => o.id !== record.cameraId), camera] }, cameraId: record.cameraId });
}

/** 发布到画布（DR-F-80）：画布在导演台右侧依次建节点（CanvasBoard.onDirectorPublish）。 */
function publishRecords(records: MediaRecord[]) {
  if (store.readonly) {
    ElMessage.warning(readonlyText.value);
    return;
  }
  const shotTexts = Object.fromEntries(records.flatMap((record) => {
    const text = record.kind === "capture" ? shotTextOfRecord(record) : "";
    return text ? [[record.id, text]] : [];
  }));
  emit("publish", { records, shotTexts });
}

/**
 * 打开时带来的事：
 * - 「重新截图」（DR-F-84）：截同一个机位，机位已删除时说明并给「改用输出机位」。
 *   从发布节点来的只替换那个节点；从导演台节点来的（`nodeId` 是它自己）照常回写输出；
 * - 「搭景」（DRT-20）：展开 AI 条并自动发起一次。只读时不发（条里会说为什么）。
 */
async function runPendingAction() {
  const action = props.pendingAction;
  if (action?.type === "build") {
    aiAutoStart.value = !store.readonly;
    aiOpen.value = true;
    return;
  }
  if (action?.type !== "recapture" || !store.scene) return;
  const replaceNodeId = action.nodeId !== props.nodeId ? action.nodeId : undefined;
  const wanted = action.cameraId ?? store.scene.outputCameraId ?? null;
  if (wanted && directorFindObject(store.scene, wanted)?.kind === "camera") {
    await runCapture([wanted], { replaceNodeId });
    return;
  }
  const output = store.scene.outputCameraId;
  if (!output) {
    ElMessage.warning("该机位已删除，场景里也没有输出机位");
    return;
  }
  try {
    await ElMessageBox.confirm("该机位已删除。改用输出机位重新截图？", "重新截图", { confirmButtonText: "改用输出机位", cancelButtonText: "取消" });
  } catch {
    return;
  }
  await runCapture([output], { replaceNodeId });
}

// ---------------------------------------------------------------- 关闭（DR-F-154）

async function back() {
  if (closing.value) return;
  if (!store.loaded) {
    emit("close");
    return;
  }
  const blocker = directorCloseBlocker(store.saveState);
  if (blocker) {
    barOpen.value = true;
    ElMessage.warning(blocker);
    return;
  }
  // 没应用的 AI 方案不进场景：关掉就没了，先问一句（应用之后才算数，DR-F-102）
  if (store.proposal) {
    try {
      await ElMessageBox.confirm("AI 方案还没有应用，返回画布会放弃它。", "返回画布", { confirmButtonText: "放弃并返回", cancelButtonText: "继续预览" });
    } catch {
      return;
    }
    store.discardProposal();
  }
  closing.value = true;
  try {
    await store.flush();
    emit("close");
  } catch (cause) {
    barOpen.value = true;
    ElMessage.warning(cause instanceof DirectorFlushError ? cause.message : "场景还没存上，请先处理保存问题");
  } finally {
    closing.value = false;
  }
}

// ---------------------------------------------------------------- 挂载

let resizeObserver: ResizeObserver | undefined;
let opened = false;

function onWindowResize() {
  smallScreen.value = window.innerWidth < MIN_WIDTH;
  if (!smallScreen.value && !opened) {
    opened = true;
    void open();
  }
}

async function open() {
  loadError.value = "";
  // 编辑器打开时刷新一次 AI 搭景的可用性（DRT-20）：顶栏入口按它置灰，本小时次数也可能变了
  void buildConfig.refresh();
  try {
    await store.load(props.projectId, props.sceneId);
  } catch (cause) {
    const message = (cause as { message?: unknown })?.message;
    loadError.value = typeof message === "string" && message && message !== "请求失败" ? message : "场景载入失败，请检查网络后重试";
    return;
  }
  await nextTick();
  attach();
  reconcilePanorama();
  void engine.current()?.whenReady().then(runPendingAction);
  if (viewportEl.value) {
    resizeObserver = new ResizeObserver(() => refreshRects());
    resizeObserver.observe(viewportEl.value);
  }
}

/**
 * 场景的实时更新（DR-F-155）：只在编辑器打开期间订（画布页已经订了项目频道，这里只加一个监听）。
 * 放在这里而不是画布页：那样导演台 store 连同场景校验、时间轴会进画布页首屏的包（DRT-09 的包体积检查）。
 */
const realtime = useRealtimeStore();
let offSceneUpdates: (() => void) | undefined;

onMounted(() => {
  offSceneUpdates = realtime.on("director.scene_updated", frame => store.handleRemoteUpdate(frame));
  // 先拦键盘再载入：载入期间按下的 Backspace 也不能落到画布上
  window.addEventListener("keydown", onKeyCapture, true);
  window.addEventListener("keyup", onKeyCapture, true);
  window.addEventListener("blur", clearWalkKeys);
  document.addEventListener("visibilitychange", onVisibility);
  window.addEventListener("resize", onWindowResize);
  onWindowResize();
});

onBeforeUnmount(() => {
  offSceneUpdates?.();
  window.removeEventListener("keydown", onKeyCapture, true);
  window.removeEventListener("keyup", onKeyCapture, true);
  window.removeEventListener("blur", clearWalkKeys);
  window.removeEventListener("resize", onWindowResize);
  document.removeEventListener("visibilitychange", onVisibility);
  resizeObserver?.disconnect();
  clearTimeout(wheelTimer);
  engine.detach();
  poseDetector?.dispose();
  poseDetector = null;
  store.reset();
});
</script>

<template>
  <div ref="rootEl" class="director-studio" role="dialog" aria-modal="true" aria-label="3D 导演台" @keydown="onRootKey" @keyup="onRootKey">
    <StudioTopbar :title="nodeTitle || '3D 导演台'" :closing="closing" @back="back" @show-bar="barOpen = true">
      <template #actions>
        <button
          type="button"
          class="dr-btn"
          :class="{ primary: aiBarShown }"
          :disabled="Boolean(aiEntryReason)"
          :title="aiEntryReason || 'AI 搭景（免费）：用一句描述和参考图搭出场景，确认后才落进场景'"
          aria-label="AI 搭景"
          :aria-expanded="aiBarShown"
          data-testid="ai-build-entry"
          @click="toggleAi()"
        >
          <Sparkles :size="14" />AI 搭景
        </button>
        <span v-if="aiEntryReason && !store.readonly" class="director-topbar-note" role="note">{{ aiEntryReason }}</span>
        <button type="button" class="dr-btn" :disabled="Boolean(captureBlocked) || capturing" :title="captureBlocked || '截当前机位（机位视角下是正透过的机位，否则是选中或输出机位）'" aria-label="截当前机位" @click="captureCurrent">
          <CameraIcon :size="14" />截当前机位
        </button>
        <button type="button" class="dr-btn" :disabled="Boolean(captureBlocked) || capturing" :title="captureBlocked || '每个机位各截一张'" aria-label="截全部机位" @click="captureAll">
          <Aperture :size="14" />截全部机位
        </button>
        <button
          type="button"
          class="dr-btn"
          :disabled="Boolean(previewBlocked) || capturing"
          :title="previewBlocked || '按时间轴逐帧导出白模预演视频（mp4），可以发布成视频节点'"
          aria-label="导出预演"
          data-testid="preview-entry"
          @click="previewOpen = true"
        >
          <Clapperboard :size="14" />导出预演
        </button>
      </template>
    </StudioTopbar>

    <div v-if="loadError" class="director-studio-state" role="alert">
      <p>{{ loadError }}</p>
      <div class="dr-seg">
        <button type="button" class="dr-btn primary" @click="open">重试</button>
        <button type="button" class="dr-btn" @click="emit('close')">返回画布</button>
      </div>
    </div>
    <div v-else class="director-studio-body">
      <SceneTree :selected-id="selectedId" :readonly="!editable" :readonly-text="readonlyText || '请先应用或放弃 AI 提案'" :placement="placement" @select="selectObject" />

      <div class="director-studio-center">
      <main ref="viewportEl" :key="viewportKey" class="director-studio-viewport">
        <canvas ref="canvasEl" class="director-studio-canvas" />
        <div ref="labelsEl" class="director-studio-labels" />
        <ViewportChrome
          :view-mode="viewMode"
          :view-camera-id="viewCameraId"
          :cameras="cameras"
          :transform-mode="transformMode"
          :walking="walking"
          :preview-on="previewOn"
          :frame-rect="frameRect"
          :preview-rect="previewRect"
          :readonly="!editable"
          :readonly-text="readonlyText || '请先应用或放弃 AI 提案'"
          @set-view="setView"
          @reset-view="engine.current()?.resetView()"
          @toggle-preview="togglePreview"
          @toggle-walk="toggleWalk"
          @set-mode="setMode"
        />
        <div
          v-if="adjustPanorama"
          class="director-adjust"
          title="拖动转动全景，滚轮改半径；Esc 退出"
          @pointerdown="onAdjustDown"
          @pointermove="onAdjustMove"
          @pointerup="onAdjustUp"
          @pointercancel="onAdjustUp"
          @wheel.prevent="onAdjustWheel"
        ><span>拖动转动全景 · 滚轮改半径 · Esc 退出</span></div>
        <CaptureStrip
          v-if="store.loaded"
          :records="store.records"
          :render-fingerprint="store.renderFingerprint"
          :readonly="store.readonly"
          :readonly-text="readonlyText"
          :control-blocked="captureBlocked"
          :busy="capturing"
          @publish="publishRecords"
          @control="runControlCapture"
        />
        <AiBuildBar
          v-if="aiBarShown"
          :default-prompt="buildDefaults?.prompt ?? ''"
          :uploads="buildDefaults?.uploads ?? []"
          :upstream-images="upstreamImages"
          :auto-start="aiAutoStart"
          @close="toggleAi(false)"
        />
        <PreviewExportDialog
          v-if="previewOpen"
          :project-id="projectId"
          :scene-id="sceneId"
          :exporter="exportPreview"
          @close="previewOpen = false"
          @publish="record => publishRecords([record])"
        />
        <PoseFromImageDialog
          v-if="poseImageObject"
          :object="poseImageObject"
          :detector="poseDetectorOf"
          @close="poseImageId = null"
          @applied="onPoseApplied"
        />
        <DirectorConflictBar v-model:open="barOpen" />
        <StudioGuide v-if="store.loaded && !store.readonly" />
        <div v-if="!store.loaded" class="director-studio-loading" role="status">正在载入场景…</div>
        <div v-if="contextLost" class="director-studio-lost" role="alert">
          <p>图形环境已重置</p>
          <button type="button" class="dr-btn" title="重新加载编辑器" aria-label="重新加载编辑器" @click="reload">重新加载编辑器</button>
        </div>
      </main>
      <TimelinePanel
        v-if="store.loaded"
        v-model:selected-key="selectedKey"
        :selected-id="selectedId"
        :readonly="!editable"
        :readonly-text="readonlyText || '请先应用或放弃 AI 提案'"
        @seek="seek"
        @play="play"
        @pause="pause"
        @select-object="selectObject"
      />
      </div>

      <aside class="director-studio-side" aria-label="属性与场景设置">
        <nav class="dr-tabs" role="tablist">
          <button type="button" role="tab" :aria-selected="tab === 'props'" :class="{ active: tab === 'props' }" @click="tab = 'props'">属性</button>
          <button v-if="selected?.kind === 'character'" type="button" role="tab" :aria-selected="tab === 'pose'" :class="{ active: tab === 'pose' }" data-testid="pose-tab" @click="tab = 'pose'">姿势</button>
          <button type="button" role="tab" :aria-selected="tab === 'scene'" :class="{ active: tab === 'scene' }" @click="tab = 'scene'">场景</button>
        </nav>
        <div v-if="tab === 'props'" class="dr-panel">
          <p v-if="!selected" class="dr-empty">在左侧或视口里选中一个对象</p>
          <template v-else>
            <div class="dr-field"><span>名称</span><strong>{{ selected.name }}</strong></div>
            <p v-if="selectedTracked" class="dr-note" data-testid="autokey-note">已打关键帧：改动会写入第 {{ store.playhead }} 帧</p>
            <p v-if="selected.locked" class="dr-hint">已锁定：视口里不能点选、拖动，在左侧解锁后再改。</p>
            <TransformFields :object="selectedShown ?? selected" :readonly="!editable" />
            <CharacterProps v-if="selected.kind === 'character'" :object="selected" :readonly="!editable" @open-poses="tab = 'pose'" @pose-from-image="openPoseFromImage" />
            <CameraProps v-else-if="selected.kind === 'camera'" :object="selectedShown ?? selected" :readonly="!editable" @aim="aimCamera" @camera-from-view="cameraFromView" />
            <PropProps v-else-if="selected.kind === 'prop'" :object="selected" :readonly="!editable" />
            <CrowdProps v-else-if="selected.kind === 'crowd'" :object="selected" :readonly="!editable" />
          </template>
        </div>
        <div v-else-if="tab === 'pose' && selected?.kind === 'character'" class="dr-panel">
          <PosePanel
            :object="selected"
            :readonly="!editable"
            :readonly-text="readonlyText || '请先应用或放弃 AI 提案'"
            :joint-mode="Boolean(jointCharacterId)"
            :joint-bone="jointBone"
            @toggle-joint-mode="toggleJointMode"
            @select-joint="jointBone = $event"
            @reset-joint="resetJoint"
            @pose-from-image="openPoseFromImage"
          />
        </div>
        <SceneSettings v-else v-model:adjust-panorama="adjustPanorama" :upstream-images="upstreamImages" :readonly="!editable" />
      </aside>
    </div>
    <div v-if="smallScreen" class="director-studio-state director-studio-small" role="alert">
      <p>请在更大的屏幕上编辑 3D 导演台（窗口宽度至少 {{ MIN_WIDTH }} 像素）。</p>
      <button type="button" class="dr-btn" @click="back">返回画布</button>
    </div>
  </div>
</template>

<style scoped>
.director-studio {
  position: fixed;
  inset: 0;
  /* 高于脚本工作台（180）：两者不会同时打开，但层级要能说清谁在上面。 */
  z-index: 190;
  display: grid;
  grid-template-rows: 48px 1fr;
  background: var(--portal-bg);
  color: var(--portal-text);
}
.director-topbar-note { max-width: 220px; overflow: hidden; color: var(--portal-muted); font-size: 11px; text-overflow: ellipsis; white-space: nowrap; }
.director-studio-body { display: grid; grid-template-columns: 240px minmax(0, 1fr) 320px; min-height: 0; }
.director-studio-center { display: grid; grid-template-rows: minmax(0, 1fr) auto; min-width: 0; min-height: 0; }
.director-studio-viewport { position: relative; min-width: 0; min-height: 0; overflow: hidden; }
.director-studio-canvas { display: block; width: 100%; height: 100%; }
/* 位置与尺寸由引擎按画面矩形设置（机位视角下与画幅框重合）。 */
.director-studio-labels { position: absolute; left: 0; top: 0; pointer-events: none; }
.director-studio-side { display: grid; grid-template-rows: auto 1fr; min-height: 0; border-left: 1px solid var(--portal-line); background: var(--portal-panel); }
.director-studio-state,
.director-studio-lost,
.director-studio-loading {
  display: grid;
  place-content: center;
  justify-items: center;
  gap: 12px;
  color: var(--portal-text);
  text-align: center;
}
.director-studio-lost,
.director-studio-loading { position: absolute; inset: 0; z-index: 20; background: var(--portal-bg); }
.director-studio-small { position: absolute; inset: 48px 0 0; z-index: 40; background: var(--portal-bg); }
.director-adjust {
  position: absolute;
  inset: 0;
  z-index: 6;
  display: grid;
  align-items: end;
  justify-items: center;
  padding-bottom: 16px;
  cursor: ew-resize;
  touch-action: none;
}
.director-adjust span { padding: 6px 12px; border-radius: 999px; color: var(--portal-on-media); background: var(--portal-on-media-bg); font-size: 12px; }
</style>
