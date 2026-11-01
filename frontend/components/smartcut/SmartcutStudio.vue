<script setup lang="ts">
/**
 * 智能剪辑的剪辑台（SCT-20 外壳，SCT-21 预览，SCT-22 面板，SCT-24 导出）：全屏层。
 *
 * 布局（dev/03 §5.1）：顶栏 + 预览（中）+ 右侧面板（可收起）+ 工具条 + 多轨时间线；首次打开有三步引导；
 * 宽 < 1024 时盖一层提示，仍可只读查看。
 * 生命周期：挂载 → `store.load`（收起后再打开且还是这条时间线时只对一次账，不重新载入）→ 素材条 `listOnly` 同步；
 * 关闭 → 先存上再 `store.reset`；收起 → 先存上、保留 store（再打开秒开，SCD-52）。
 *
 * ⚠️ 只能经 `defineAsyncComponent(() => import("~/components/smartcut/SmartcutStudio.vue"))` 引用（SCD-41）：
 *    写成标签会把剪辑台打进画布首屏的包。
 * ⚠️ 键盘分两道（同导演台）：画布那边的布尔挡一道；这里在 window **捕获阶段**截断剪辑台打开期间的按键，
 *    画布在冒泡阶段永远收不到。输入框与片段块例外：先放行到它们自己（输入框要自己的退格，片段块要 ←→ 与回车），
 *    再由根元素在冒泡阶段截断（SC-AC-07）。
 * ⚠️ 焦点陷阱（SCD-53）：Tab 只在剪辑台里转；关闭后焦点回到打开它的那个元素（节点卡）。
 * AI 剪辑（SCT-31，dev/03 §5.5）：指令面板「发送」带来 `pendingAction: plan`，打开之后按节点上的要求剪一版；剪辑台里
 * 还有追加指令、按要求重剪、方案切换、撤销这轮 AI、素材变化的两个处理。交互在 `useSmartcutAi`，状态在 store。
 */
import "./smartcut.css";
import { ElMessage } from "element-plus";
import { SmartcutFlushError } from "~/stores/smartcut";
import { isEditableTarget } from "~/utils/canvasShortcuts";
import {
  smartcutAppendInputs,
  smartcutDeleteLeftAt,
  smartcutDeleteRightAt,
  smartcutDuplicateClip,
  smartcutFindClip,
  smartcutFitZoom,
  smartcutRemoveClips,
  smartcutSetClipSpeed,
  smartcutSplitAt,
  smartcutVideoClipAt
} from "~/utils/smartcutEdit";
import type { SmartcutEditResult } from "~/utils/smartcutContent";
import { smartcutTempId } from "~/utils/smartcutModel";
import { smartcutAddText } from "~/utils/smartcutPanels";
import type { SmartcutPendingAction } from "~/utils/smartcutCanvas";
import { smartcutClaimPlayer } from "~/smartcut/player";
import { smartcutChangedClips, smartcutRemoteNoticeText } from "~/utils/smartcutPlan";
import { smartcutShortcutAction, type SmartcutShortcutAction } from "~/utils/smartcutShortcuts";
import { smartcutCloseBlocker, smartcutMinStudioWidth, smartcutReadonlyText, type SmartcutTool } from "~/utils/smartcutStudio";
import { smartcutStepMs } from "~/utils/smartcutTimecode";
import { smartcutDefaultZoom, smartcutFormatTime, smartcutZoomLevels } from "~/utils/smartcutTimeline";
import { useSmartcutPreviewAssets } from "~/composables/useSmartcutPreviewAssets";
import { useSmartcutAi } from "~/composables/useSmartcutAi";
import type { components } from "@tabtv/api-client";
import SmartcutConflictBar from "./SmartcutConflictBar.vue";
import SmartcutGuide from "./SmartcutGuide.vue";
import SmartcutToolbar from "./SmartcutToolbar.vue";
import SmartcutTopbar from "./SmartcutTopbar.vue";
import ClipInspector from "./panels/ClipInspector.vue";
import HistoryPanel from "./panels/HistoryPanel.vue";
import SubtitleTrackEditor from "./panels/SubtitleTrackEditor.vue";
import TextEditor from "./panels/TextEditor.vue";
import PreviewStage from "./preview/PreviewStage.vue";
import TimelineView from "./timeline/TimelineView.vue";
import ExportPanel from "./panels/ExportPanel.vue";
import EditInstructionBar from "./panels/EditInstructionBar.vue";
import InputsChangedBar from "./panels/InputsChangedBar.vue";
import PlanNoticeBar from "./panels/PlanNoticeBar.vue";
import PlanResultCard from "./panels/PlanResultCard.vue";
import PlanStageBar from "./panels/PlanStageBar.vue";
import TranscribeDialog, { type SmartcutTranscribePreset } from "./dialogs/TranscribeDialog.vue";
import TranslateDialog from "./dialogs/TranslateDialog.vue";
import RegenerateForm from "./dialogs/RegenerateForm.vue";
import FillForm from "./dialogs/FillForm.vue";
import DesubtitleForm from "./dialogs/DesubtitleForm.vue";
import MusicForm from "./dialogs/MusicForm.vue";
import SfxForm from "./dialogs/SfxForm.vue";
import AudioGenPanel from "./panels/AudioGenPanel.vue";
import GenerationList from "./panels/GenerationList.vue";
import {
  smartcutClipSourceMs,
  smartcutClipToolReason,
  smartcutFillAfter,
  type SmartcutClipToolContext,
  type SmartcutClipToolOpen
} from "~/utils/smartcutClipGen";
import type { SmartcutStoreGeneration } from "~/stores/smartcut";

type Timeline = components["schemas"]["Timeline"];

const props = defineProps<{ projectId: string; timelineId: string; nodeId: string; nodeTitle: string; pendingAction?: SmartcutPendingAction }>();
const emit = defineEmits<{ close: []; collapse: []; publish: [render: components["schemas"]["RenderJob"]] }>();

const store = useSmartcutStore();
const realtime = useRealtimeStore();
const previews = useSmartcutPreviewAssets();

const rootEl = ref<HTMLElement | null>(null);
const timelineRef = ref<InstanceType<typeof TimelineView> | null>(null);
const previewRef = ref<InstanceType<typeof PreviewStage> | null>(null);
const loadError = ref("");
const closing = ref(false);
const barOpen = ref(true);
const sidePanel = ref<"history" | "generations" | "audio" | null>(null);
/** 从时间线接缝点进来时让片段面板定位到转场设置。 */
const inspectorFocus = ref<"transition" | null>(null);
const smallScreen = ref(false);
const zoom = ref(smartcutDefaultZoom);
const snap = ref(true);
/** 收起：卸载时保留 store（再打开秒开）。 */
let keepState = false;
/** 打开剪辑台之前有焦点的元素：关闭后还给它（SCD-53）。 */
let returnFocus: HTMLElement | null = null;

const editable = computed(() => store.loaded && !store.readonly && !smallScreen.value);
/**
 * 要调用模型的功能总开关（utils/smartcutFeatures）：这一版只保留手动剪辑 —— AI 剪辑条、规划结果、按新素材重新剪、
 * 识别字幕与翻译、补镜 / 重新生成 / 去字幕 / 音效 / 生成音频 / 生成记录都不出现，画布带来的「按要求剪一版」也不执行。
 */
const features = useSmartcutFeatures();
const disabledReason = computed(() => (store.readonly ? smartcutReadonlyText(store.readonlyReason) : smallScreen.value ? "屏幕太窄，只能查看" : ""));

// ---------------------------------------------------------------- AI 剪辑（SCT-31）

const smartcuts = useSmartcuts();
const ai = useSmartcutAi({
  nodeId: () => props.nodeId, editable: () => editable.value, collapse: () => leave("collapse"),
  openTranscribe: preset => openTranscribe(preset)
});

// ---------------------------------------------------------------- 识别字幕与翻译（SCT-36）

/** 识别确认框（null = 关着）。从字幕面板进来是做字幕，从 42217 的出路进来是口播 / 解说。 */
const transcribePreset = ref<SmartcutTranscribePreset | null>(null);
/** 翻译框的源字幕轨（null = 关着）。 */
const translateTrack = ref<string | null>(null);
function openTranscribe(preset: SmartcutTranscribePreset) {
  if (!editable.value) {
    ElMessage.warning(disabledReason.value || "现在不能识别");
    return;
  }
  translateTrack.value = null;
  transcribePreset.value = preset;
}
function openTranslate(trackId: string) {
  if (!editable.value) return;
  transcribePreset.value = null;
  translateTrack.value = trackId;
}
/** 素材变化：变了的段号（素材条的 `changed`）；摘要说有变化但没有片段用到变了的素材时也出条（可以按新素材重新剪）。 */
const changedPositions = computed(() => smartcutChangedClips(store.timeline, store.inputs));
const changedDismissed = ref(false);
// 替换之后素材不再「变了」，但被截短的段还要说完：有替换结果时条留着，用户关掉为止。
const showChanged = computed(() => !changedDismissed.value && (changedPositions.value.length > 0
  || Boolean(smartcuts.get(props.nodeId)?.inputsStale) || Boolean(ai.replaceNotice.value?.details.length)));
watch(() => store.inputsFingerprint, () => { changedDismissed.value = false; });
/** 结果卡上的出路与方案切换：AI 不能用（只读、进行中、被限流……）时置灰并说明。 */
const aiActionReason = computed(() => (store.planRunning ? "AI 剪辑进行中，请等它完成" : ai.planDisabledReason.value));
const undoReason = computed(() => {
  if (!store.aiRound) return "没有可撤销的 AI 剪辑";
  if (!editable.value) return disabledReason.value || "现在不能撤销";
  return store.planRunning ? "AI 剪辑进行中，请等它完成" : "";
});

// ---------------------------------------------------------------- 片段级生成（SCT-40）

/** 打开着的片段级工具确认框（null = 关着）；`atMs` 是打开那一刻的播放头（配乐的起点）。 */
const clipTool = ref<(SmartcutClipToolOpen & { atMs: number }) | null>(null);
function openClipTool(open: SmartcutClipToolOpen) {
  if (!editable.value) {
    ElMessage.warning(disabledReason.value || "现在不能生成");
    return;
  }
  transcribePreset.value = null;
  translateTrack.value = null;
  clipTool.value = { ...open, atMs: store.playheadMs };
}
const configModels = computed(() => store.config?.models ?? {});
const fillReason = computed(() => smartcutClipToolReason("fill", { editable: editable.value, disabledReason: disabledReason.value,
  models: configModels.value.fill ?? undefined }));
const audioReason = computed(() => (editable.value ? "" : disabledReason.value || "现在不能编辑"));
function openFill() {
  openClipTool({ tool: "fill", afterClipId: smartcutFillAfter(store.timeline, store.playheadMs) });
}
const fillPositionText = computed(() => {
  const after = clipTool.value?.afterClipId ?? "";
  if (!after) return "插在最前面";
  const video = store.timeline?.tracks.find(t => t.kind === "video")?.clips ?? [];
  const i = video.findIndex(c => c.id === store.currentId(after));
  return i >= 0 ? `插在第 ${i + 1} 段之后（第 ${i + 2} 段之前）` : "插在选定的位置";
});
/** 一段取用的长度（素材毫秒）：去字幕与音效按它计价。 */
function sourceMsOf(clipId: string | undefined): number {
  const t = store.timeline;
  const c = t && clipId ? smartcutFindClip(t, store.currentId(clipId))?.clip : null;
  return c ? smartcutClipSourceMs(c) : 0;
}
/** 「生成音频」面板里音效按钮的上下文：选中的那一段（单选）。 */
const sfxContext = computed<SmartcutClipToolContext | null>(() => {
  const t = store.timeline;
  const sel = store.selection;
  if (!t || sel?.kind !== "clip" || sel.ids.length !== 1) return null;
  const ref = smartcutFindClip(t, sel.ids[0]!);
  if (!ref) return null;
  const input = store.inputs.find(i => i.sourceNodeId === ref.clip.sourceNodeId);
  return {
    editable: editable.value, disabledReason: disabledReason.value, trackKind: ref.track.kind,
    mediaKind: ref.track.kind === "audio" ? "audio" : input?.kind === "image" ? "image" : "video",
    clipMs: smartcutClipSourceMs(ref.clip), model: configModels.value.video2audio?.[0] ?? null,
    models: configModels.value.video2audio ?? undefined
  };
});
function openSfxFromPanel() {
  const id = store.selection?.ids[0];
  if (id) openClipTool({ tool: "sfx", clipId: id });
}
/** 生成记录里的「重试」：按那次的工具重新打开确认框（重新报价、确认后是新的一次扣费）。 */
function retryGeneration(g: SmartcutStoreGeneration) {
  const req = store.clipRequests[g.generationId];
  switch (g.kind) {
    case "regen":
      openClipTool({ tool: "regenerate", clipId: g.clipId, prompt: typeof req?.body.prompt === "string" ? req.body.prompt : "" });
      return;
    case "desubtitle":
      openClipTool({ tool: "desubtitle", clipId: g.clipId });
      return;
    case "sfx":
      openClipTool({ tool: "sfx", clipId: g.clipId });
      return;
    case "fill":
      openClipTool({ tool: "fill", afterClipId: req?.afterClipId ?? smartcutFillAfter(store.timeline, store.playheadMs) });
      return;
    case "music":
      openClipTool({ tool: "music" });
  }
}
/** 导出面板「第 N 段还在生成」的定位：选中那一段、播放头移过去、时间线滚到那里。 */
function locateClip(clipId: string) {
  const t = store.timeline;
  const ref = t ? smartcutFindClip(t, store.currentId(clipId)) : null;
  if (!ref) return;
  store.select({ kind: "clip", ids: [ref.clip.id] });
  store.setPlayhead(ref.clip.startMs);
  void nextTick(() => timelineRef.value?.reveal(ref.clip.startMs));
}
/** 从生成记录拖进时间线（落点 = 松手处）；放不下时说明原因。 */
async function placeGeneration(generationId: string, ms: number) {
  const reason = await store.placeGeneration(generationId, ms);
  if (reason) ElMessage.warning(reason);
  else ElMessage.success(`已放到 ${smartcutFormatTime(ms)}`);
}

// ---------------------------------------------------------------- 选中片段与工具条的上下文

/** 分割 / 删左右作用的片段：选中的那一段（单选时），否则播放头下画面轨的那一段。 */
const target = computed(() => {
  const t = store.timeline;
  if (!t) return null;
  const sel = store.selection;
  if (sel?.kind === "clip" && sel.ids.length === 1) return smartcutFindClip(t, sel.ids[0]!)?.clip ?? null;
  return smartcutVideoClipAt(t, store.playheadMs);
});
const playheadInTarget = computed(() => {
  const clip = target.value;
  return Boolean(clip && store.playheadMs > clip.startMs && store.playheadMs < clip.startMs + clip.durationMs);
});
const selectedClips = computed(() => (store.selection?.kind === "clip" ? store.selection.ids : []));
const targetSpeed = computed(() => {
  const ids = selectedClips.value;
  if (ids.length !== 1 || !store.timeline) return null;
  return smartcutFindClip(store.timeline, ids[0]!)?.clip.speed ?? 100;
});

// ---------------------------------------------------------------- 动作

/** 经纯函数改一次内容；失败时说清为什么（不改内容、不压撤销）。 */
function apply(mutate: (t: Timeline) => SmartcutEditResult<Timeline> | Timeline): boolean {
  let failure = "";
  const changed = store.commit((t) => {
    const result = mutate(t);
    if ("ok" in result && typeof result.ok === "boolean") {
      if (!result.ok) {
        failure = result.reason;
        return t;
      }
      return result.value;
    }
    return result as Timeline;
  });
  if (failure) ElMessage.warning(failure);
  return changed;
}

function runTool(tool: SmartcutTool | "undo" | "redo") {
  if (!editable.value && tool !== "undo" && tool !== "redo") return;
  const clip = target.value;
  const at = store.playheadMs;
  switch (tool) {
    case "undo":
      store.undo();
      return;
    case "redo":
      store.redo();
      return;
    case "split": {
      if (!clip) return;
      const newId = smartcutTempId();
      if (apply(t => smartcutSplitAt(t, clip.id, at, newId))) store.select({ kind: "clip", ids: [newId] });
      return;
    }
    case "delete-left":
      if (clip) apply(t => smartcutDeleteLeftAt(t, clip.id, at));
      return;
    case "delete-right":
      if (clip) apply(t => smartcutDeleteRightAt(t, clip.id, at));
      return;
    case "delete":
      removeSelection();
      return;
    case "duplicate": {
      const id = selectedClips.value[0];
      if (!id) return;
      const newId = smartcutTempId();
      if (apply(t => smartcutDuplicateClip(t, id, newId))) store.select({ kind: "clip", ids: [newId] });
    }
  }
}

function removeSelection() {
  const sel = store.selection;
  if (!sel || !editable.value) return;
  if (sel.kind === "clip") {
    if (store.commit(t => smartcutRemoveClips(t, sel.ids))) store.select(null);
    return;
  }
  const ids = new Set(sel.ids);
  const changed = store.commit(t => (sel.kind === "subtitle"
    ? ((t.subtitles ?? []).some(s => ids.has(s.id)) ? { ...t, subtitles: t.subtitles.filter(s => !ids.has(s.id)) } : t)
    : ((t.texts ?? []).some(x => ids.has(x.id)) ? { ...t, texts: (t.texts ?? []).filter(x => !ids.has(x.id)) } : t)));
  if (changed) store.select(null);
}

/** 右侧面板跟着选中走：片段 → 片段编辑；文字 → 文字编辑；其余（含字幕、什么都没选）→ 字幕轨编辑。 */
const sideView = computed<"history" | "generations" | "audio" | "clip" | "text" | "subtitles">(() => {
  if (sidePanel.value) return sidePanel.value;
  const sel = store.selection;
  if (sel?.kind === "clip" && sel.ids.length === 1) return "clip";
  if (sel?.kind === "text" && sel.ids.length === 1) return "text";
  return "subtitles";
});
// 选中片段时回到片段面板；「生成音频」面板要按选中的那一段判断音效能不能用，选中不关它
watch(() => store.selection, () => { if (sidePanel.value && sidePanel.value !== "audio" && store.selection) sidePanel.value = null; });

function addText() {
  if (!editable.value) return;
  const ids = { text: smartcutTempId(), track: smartcutTempId() };
  let failure = "";
  const changed = store.commit((t) => {
    const r = smartcutAddText(t, store.playheadMs, ids);
    if (!r.ok) {
      failure = r.reason;
      return t;
    }
    return r.value.timeline;
  });
  if (failure) ElMessage.warning(failure);
  if (changed) store.select({ kind: "text", ids: [ids.text] });
}

function editTransition(clipId: string) {
  store.select({ kind: "clip", ids: [clipId] });
  inspectorFocus.value = "transition";
  void nextTick(() => {
    document.querySelector<HTMLElement>('[data-testid="smartcut-transition-editor"] select')?.focus();
    inspectorFocus.value = null;
  });
}

function setSpeed(speed: number) {
  const id = selectedClips.value[0];
  if (!id || !editable.value) return;
  apply(t => smartcutSetClipSpeed(t, id, speed));
}

function fitWidth() {
  const width = timelineRef.value?.laneViewport ?? 800;
  zoom.value = smartcutFitZoom(store.timeline?.effectiveMs ?? 0, width, smartcutZoomLevels);
}

function step(unit: "frame" | "second", delta: number) {
  const t = store.timeline;
  if (!t) return;
  previewRef.value?.pause();
  store.setPlayhead(smartcutStepMs(store.playheadMs, t.fps, unit, delta, t.effectiveMs));
}

function dispatch(action: SmartcutShortcutAction) {
  switch (action) {
    case "play":
      previewRef.value?.toggle();
      return;
    case "frame-back":
      step("frame", -1);
      return;
    case "frame-forward":
      step("frame", 1);
      return;
    case "second-back":
      step("second", -1);
      return;
    case "second-forward":
      step("second", 1);
      return;
    case "split":
      runTool("split");
      return;
    case "delete":
      runTool("delete");
      return;
    case "undo":
      store.undo();
      return;
    case "redo":
      store.redo();
      return;
    case "duplicate":
      runTool("duplicate");
  }
}

/** Esc：只取消当前操作（拖动、裁切、变换），不关闭剪辑台（SC-UX-03）；什么都没在进行时清掉选中。 */
function cancelCurrent() {
  if (clipTool.value) {
    clipTool.value = null;
    return;
  }
  if (transcribePreset.value || translateTrack.value) {
    transcribePreset.value = null;
    translateTrack.value = null;
    return;
  }
  if (timelineRef.value?.cancel()) return;
  if (previewRef.value?.cancel()) return;
  if (store.selection) store.select(null);
}

// ---------------------------------------------------------------- 键盘（SC-UX-03、SC-AC-07、SC-AC-70）

function onKeyCapture(event: KeyboardEvent) {
  const target = event.target;
  const inside = target instanceof Node && Boolean(rootEl.value?.contains(target));
  // Element Plus 的弹窗（确认框等）传送到了 body：放行，让它自己处理回车 / Esc（画布那边的布尔照样挡着）
  if (target instanceof Element && target.closest(".el-overlay, .el-message-box, .el-popper")) return;
  // 剪辑台里的输入框：先放行到它自己，由根元素在冒泡阶段截断
  if (inside && isEditableTarget(target)) return;
  const onClip = inside && target instanceof Element && Boolean(target.closest("[data-clip-block]"));
  if (event.key === "Tab") {
    // 焦点陷阱：焦点不在剪辑台里（点到了空白处、页面刚打开）时按 Tab 先回到剪辑台，不让它跑到画布上
    if (event.type === "keydown") {
      if (inside) trapFocus(event);
      else {
        event.preventDefault();
        event.stopPropagation();
        rootEl.value?.focus();
      }
    }
    return;
  }
  if (!onClip) event.stopPropagation();
  if (event.type !== "keydown" || !store.loaded) return;
  if (event.key === "Escape") {
    event.preventDefault();
    cancelCurrent();
    return;
  }
  const action = smartcutShortcutAction(event, {
    readonly: !editable.value,
    hasSelection: Boolean(store.selection?.ids.length),
    clipListFocused: onClip
  });
  if (!action) return;
  event.preventDefault();
  if (onClip) event.stopPropagation();
  dispatch(action);
}

/** 根元素冒泡阶段：剪辑台里的按键（含输入框、片段块里的）一律不再往上传，画布收不到。 */
function onRootKey(event: KeyboardEvent) {
  event.stopPropagation();
}

const FOCUSABLE = "button:not([disabled]), [href], input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex='-1'])";

function trapFocus(event: KeyboardEvent) {
  const root = rootEl.value;
  if (!root) return;
  const items = [...root.querySelectorAll<HTMLElement>(FOCUSABLE)].filter(el => el.offsetParent !== null || el === document.activeElement);
  if (!items.length) {
    event.preventDefault();
    root.focus();
    return;
  }
  const first = items[0]!;
  const last = items[items.length - 1]!;
  const active = document.activeElement as HTMLElement | null;
  if (event.shiftKey && (active === first || active === root)) {
    event.preventDefault();
    last.focus();
  } else if (!event.shiftKey && active === last) {
    event.preventDefault();
    first.focus();
  }
}

// ---------------------------------------------------------------- 打开、收起、关闭

async function open() {
  loadError.value = "";
  try {
    if (store.loaded && store.timelineId === props.timelineId && store.projectId === props.projectId) {
      await store.reconcile();
    } else {
      await store.load(props.projectId, props.timelineId);
    }
  } catch (cause) {
    const message = (cause as { message?: unknown })?.message;
    loadError.value = typeof message === "string" && message && message !== "请求失败" ? message : "时间线载入失败，请检查网络后重试";
    return;
  }
  // 剪辑台开着时只更新素材条、不动轨道（SCD-49）：失败不打扰，素材条留着上一次的。
  const synced = store.sync("listOnly").catch(() => null);
  await nextTick();
  if (!timelineRef.value?.focusFirst()) rootEl.value?.focus();
  await runPendingAction(synced);
}

/** 打开时顺手做的事：AI 剪辑（指令面板的「发送」，按节点上的要求剪一版）、「追加到末尾」（SC-F-14）。 */
async function runPendingAction(synced: Promise<unknown>) {
  const action = props.pendingAction;
  if (!action || !editable.value) return;
  if (action.type === "plan") {
    if (!features.ai) return;
    // 先等素材条同步完：服务端也会先同步一次，但界面上的「没有素材」要按最新的素材条判
    await synced;
    await ai.plan(ai.nodeRequest());
    return;
  }
  if (action.type !== "append-unused") return;
  await synced;
  let count = 0;
  store.commit((t) => {
    const result = smartcutAppendInputs(t, store.inputs, smartcutTempId);
    count = result.appended;
    return result.timeline;
  });
  ElMessage[count ? "success" : "info"](count ? `已把 ${count} 段新素材追加到末尾` : "没有可以追加的素材（还在生成、或时长未知）");
}

function notifyLeave(kind: "close" | "collapse") {
  if (kind === "close") emit("close");
  else emit("collapse");
}

async function leave(kind: "close" | "collapse") {
  if (closing.value) return;
  if (!store.loaded) {
    notifyLeave(kind);
    return;
  }
  const blocker = smartcutCloseBlocker(store.saveState);
  if (blocker) {
    barOpen.value = true;
    ElMessage.warning(blocker);
    return;
  }
  closing.value = true;
  try {
    previewRef.value?.pause();
    if (!store.readonly) await store.flush();
    keepState = kind === "collapse";
    notifyLeave(kind);
  } catch (cause) {
    barOpen.value = true;
    ElMessage.warning(cause instanceof SmartcutFlushError ? cause.message : "时间线还没存上，请先处理保存问题");
  } finally {
    closing.value = false;
  }
}

function onWindowResize() {
  smallScreen.value = window.innerWidth < smartcutMinStudioWidth;
}

watch(() => store.remoteNotice?.seq, () => {
  const text = smartcutRemoteNoticeText(store.remoteNotice);
  if (text) ElMessage.info(text);
});
watch(() => store.saveState, (state) => {
  if (state === "conflict" || state === "offline" || state === "error") barOpen.value = true;
});

let offTimeline: (() => void) | undefined;
let offSmartcut: (() => void) | undefined;
let offGeneration: (() => void) | undefined;
let offReconnected: (() => void) | undefined;

let releasePlayer: (() => void) | undefined;

onMounted(() => {
  returnFocus = document.activeElement instanceof HTMLElement ? document.activeElement : null;
  // 整页只有一个活的播放器：剪辑台一打开，节点卡上正在播的成片停掉（SCR-4）
  releasePlayer = smartcutClaimPlayer(() => previewRef.value?.pause());
  // 时间线的实时更新只在剪辑台开着时听（画布页已经订了项目频道，这里只加监听）
  offTimeline = realtime.on("timeline.updated", frame => store.handleRemoteUpdate(frame));
  // 片段级生成的进度：按 generationId 认领（智能剪辑名下的任务不挂画布节点，画布的生成 store 认领不到）
  offGeneration = realtime.on("generation.update", frame => store.handleGenerationFrame(frame));
  // 断线期间错过的推送不会补发：重连后取一次生成记录对账
  offReconnected = realtime.onReconnected(() => { void store.refreshGenerations().catch(() => undefined); });
  offSmartcut = realtime.on("smartcut.updated", (frame) => {
    store.handleSmartcutFrame(frame);
    const analysis = frame.analysis as { mediaId?: unknown; status?: unknown } | null | undefined;
    if (analysis && typeof analysis.mediaId === "string" && analysis.status === "success") void previews.refresh(analysis.mediaId);
  });
  // 先拦键盘再载入：载入期间按下的 Backspace 也不能落到画布上
  window.addEventListener("keydown", onKeyCapture, true);
  window.addEventListener("keyup", onKeyCapture, true);
  window.addEventListener("resize", onWindowResize);
  onWindowResize();
  void open();
});

onBeforeUnmount(() => {
  offTimeline?.();
  offSmartcut?.();
  offGeneration?.();
  offReconnected?.();
  window.removeEventListener("keydown", onKeyCapture, true);
  window.removeEventListener("keyup", onKeyCapture, true);
  window.removeEventListener("resize", onWindowResize);
  previews.stopPolling();
  releasePlayer?.();
  if (!keepState) store.reset();
  if (returnFocus?.isConnected) returnFocus.focus();
});
</script>

<template>
  <div
    ref="rootEl"
    class="smartcut-studio"
    role="dialog"
    aria-modal="true"
    aria-label="智能剪辑"
    tabindex="-1"
    data-testid="smartcut-studio"
    @keydown="onRootKey"
    @keyup="onRootKey"
  >
    <SmartcutTopbar
      :title="nodeTitle || '智能剪辑'"
      :closing="closing"
      :history-open="sidePanel === 'history'"
      :small-screen="smallScreen"
      @close="leave('close')"
      @collapse="leave('collapse')"
      @show-bar="barOpen = true"
      @toggle-history="sidePanel = sidePanel === 'history' ? null : 'history'"
    >
      <template #export>
        <ExportPanel v-if="store.loaded" :editable="editable" :disabled-reason="disabledReason" :node-id="nodeId" @publish="render => emit('publish', render)" @locate="locateClip" />
      </template>
    </SmartcutTopbar>

    <div v-if="loadError" class="smartcut-state" role="alert">
      <p>{{ loadError }}</p>
      <div class="sc-seg">
        <button type="button" class="sc-btn primary" @click="open">重试</button>
        <button type="button" class="sc-btn" @click="emit('close')">关闭</button>
      </div>
    </div>
    <div v-else-if="!store.loaded" class="smartcut-state" role="status"><p>时间线载入中…</p></div>
    <div v-else class="smartcut-body">
      <div class="smartcut-ai-strip">
        <PlanStageBar v-if="store.planRunning && features.ai" :running="store.planRunning" />
        <PlanNoticeBar
          v-if="ai.failure.value && features.ai"
          :failure="ai.failure.value"
          :can-force="Boolean(store.planConflict)"
          :disabled-reason="store.planRunning ? 'AI 剪辑进行中，请等它完成' : editable ? '' : disabledReason"
          :retry-after="ai.retryAfter.value"
          @out="ai.resolveOut"
          @force="ai.forceApply"
          @keep="ai.keepTheirs"
          @dismiss="ai.dismissFailure"
        />
        <InputsChangedBar
          v-if="showChanged"
          :positions="changedPositions"
          :disabled-reason="store.planRunning ? 'AI 剪辑进行中，请等它完成' : editable ? '' : disabledReason"
          :replan-disabled-reason="ai.planDisabledReason.value"
          :allow-replan="features.ai"
          :notice="ai.replaceNotice.value"
          @replace="ai.replaceChanged"
          @replan="ai.replan"
          @dismiss="changedDismissed = true; ai.dismissReplace()"
        />
      </div>
      <div class="smartcut-upper">
        <div v-if="features.ai && ai.resultOpen.value && store.plan" class="smartcut-result-anchor">
          <PlanResultCard
            :plan="store.plan"
            :disabled-reason="aiActionReason"
            :undo-disabled-reason="undoReason"
            @undo="ai.undoRound"
            @close="ai.closeResult"
            @out="ai.resolveOut"
            @select="ai.chooseVariant"
          />
        </div>
        <PreviewStage ref="previewRef" class="smartcut-preview" :editable="editable" />
        <aside class="smartcut-side" :data-side-view="sideView">
          <HistoryPanel v-if="sideView === 'history'" @close="sidePanel = null" @undo-ai="ai.undoRound" />
          <GenerationList v-else-if="sideView === 'generations'" :editable="editable" :disabled-reason="disabledReason" @close="sidePanel = null" @retry="retryGeneration" />
          <AudioGenPanel
            v-else-if="sideView === 'audio'"
            :editable="editable"
            :disabled-reason="disabledReason"
            :sfx-context="sfxContext"
            @close="sidePanel = null"
            @music="openClipTool({ tool: 'music' })"
            @sfx="openSfxFromPanel"
          />
          <ClipInspector
            v-else-if="sideView === 'clip'"
            :key="store.selection?.ids[0]"
            :clip-id="store.selection!.ids[0]!"
            :editable="editable"
            :disabled-reason="disabledReason"
            :focus="inspectorFocus"
            @clip-tool="openClipTool"
          />
          <TextEditor v-else-if="sideView === 'text'" :key="store.selection?.ids[0]" :text-id="store.selection!.ids[0]!" :editable="editable" :disabled-reason="disabledReason" />
          <SubtitleTrackEditor
            v-else
            :focus-id="store.selection?.kind === 'subtitle' ? store.selection.ids[0] : null"
            :editable="editable"
            :disabled-reason="disabledReason"
            @transcribe="openTranscribe({ purpose: 'subtitles', subtitles: true })"
            @translate="openTranslate"
          />
        </aside>
      </div>
      <EditInstructionBar
        v-if="features.ai"
        :edit-disabled-reason="ai.editDisabledReason.value"
        :plan-disabled-reason="ai.planDisabledReason.value"
        :submit="ai.edit"
        @plan="ai.plan(ai.nodeRequest())"
      />
      <SmartcutToolbar
        v-model:zoom="zoom"
        v-model:snap="snap"
        :target-speed="targetSpeed"
        :has-selection="Boolean(store.selection?.ids.length)"
        :playhead-in-target="playheadInTarget"
        :view-only="smallScreen"
        :fill-reason="fillReason"
        :audio-reason="audioReason"
        :side-panel="sidePanel"
        :ai-tools="features.ai"
        @tool="runTool"
        @fill="openFill"
        @audio-gen="sidePanel = sidePanel === 'audio' ? null : 'audio'"
        @generations="sidePanel = sidePanel === 'generations' ? null : 'generations'"
        @speed="setSpeed"
        @play="previewRef?.toggle()"
        @fit="fitWidth"
        @add-text="addText"
      />
      <TimelineView
        ref="timelineRef"
        class="smartcut-timeline"
        :zoom="zoom"
        :snap="snap"
        :editable="editable"
        :disabled-reason="disabledReason"
        @edit-transition="editTransition"
        @place-generation="placeGeneration"
      />
    </div>

    <TranscribeDialog v-if="transcribePreset" :preset="transcribePreset" @close="transcribePreset = null" />
    <TranslateDialog v-if="translateTrack !== null" :source-track-id="translateTrack" @close="translateTrack = null" />
    <template v-if="clipTool">
      <RegenerateForm
        v-if="clipTool.tool === 'regenerate'"
        :clip-id="clipTool.clipId ?? ''"
        :initial-prompt="clipTool.prompt ?? ''"
        :model-name="clipTool.modelName ?? ''"
        @close="clipTool = null"
      />
      <FillForm v-else-if="clipTool.tool === 'fill'" :after-clip-id="clipTool.afterClipId ?? ''" :position-text="fillPositionText" @close="clipTool = null" />
      <DesubtitleForm v-else-if="clipTool.tool === 'desubtitle'" :clip-id="clipTool.clipId ?? ''" :source-ms="sourceMsOf(clipTool.clipId)" @close="clipTool = null" />
      <MusicForm v-else-if="clipTool.tool === 'music'" :at-ms="clipTool.atMs" :effective-ms="store.timeline?.effectiveMs ?? 0" @close="clipTool = null" />
      <SfxForm v-else-if="clipTool.tool === 'sfx'" :clip-id="clipTool.clipId ?? ''" :source-ms="sourceMsOf(clipTool.clipId)" @close="clipTool = null" />
    </template>
    <SmartcutConflictBar v-model:open="barOpen" />
    <SmartcutGuide v-if="store.loaded && !smallScreen" />
    <div v-if="smallScreen" class="smartcut-small" role="note">剪辑台需要更宽的屏幕（至少 {{ smartcutMinStudioWidth }} 像素），现在只能查看。</div>
  </div>
</template>

<style scoped>
.smartcut-studio {
  position: fixed;
  inset: 0;
  z-index: 2000;
  display: grid;
  grid-template-rows: auto minmax(0, 1fr);
  color: var(--portal-text);
  background: var(--portal-bg);
}
.smartcut-studio:focus { outline: none; }
.smartcut-body { display: grid; grid-template-rows: auto minmax(160px, 1fr) auto auto minmax(180px, 38%); min-height: 0; }
.smartcut-ai-strip { display: grid; min-width: 0; }
.smartcut-upper { position: relative; display: flex; min-height: 0; }
.smartcut-result-anchor { position: absolute; top: 10px; right: 332px; z-index: 30; display: flex; max-height: calc(100% - 20px); }
.smartcut-preview { flex: 1; }
.smartcut-side { width: 320px; flex-shrink: 0; min-height: 0; overflow: auto; border-left: 1px solid var(--portal-line); background: var(--portal-panel); }
.smartcut-timeline { min-height: 0; }
.smartcut-state { display: grid; place-content: center; gap: 12px; color: var(--portal-text-soft); text-align: center; }
.smartcut-small {
  position: absolute;
  top: 56px;
  left: 50%;
  z-index: 40;
  padding: 8px 14px;
  border: 1px solid var(--portal-gold-line);
  border-radius: 8px;
  color: var(--portal-text);
  background: var(--portal-elevated);
  font-size: 12px;
  transform: translateX(-50%);
}
</style>
