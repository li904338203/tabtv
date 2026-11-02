<script setup lang="ts">
import type { components } from '~/types/legacy';
import type { components as ApiComponents } from "@tabtv/api-client";
import { ArrowUp, AudioLines, CircleAlert, Clapperboard, FileText, Image as ImageIcon, LoaderCircle, Maximize2, TextCursorInput, Video, X } from "@lucide/vue";
import { ElMessage } from "element-plus";
import CameraMoveChip from "./CameraMoveChip.vue";
import MaterialPlaza from "./MaterialPlaza.vue";
import DynamicControl from "./DynamicControl.vue";
import GenerationSizeControl, { type FormatParam } from "./GenerationSizeControl.vue";
import ImagePresetChip from "./ImagePresetChip.vue";
import ImagePresetControl from "./ImagePresetControl.vue";
import ReferenceResourceToken from "./ReferenceResourceToken.vue";
import ReferenceUploadButton from "./ReferenceUploadButton.vue";
import CanvasCharacterPicker from "./CanvasCharacterPicker.vue";
import CharacterReferenceToken from "./CharacterReferenceToken.vue";
import VideoModeControl from "./VideoModeControl.vue";
import VideoTopTools from "./VideoTopTools.vue";
import VoicePicker from "./VoicePicker.vue";
import type { CanvasNodeData, JsonObject } from "~/types/canvas";
import { VIDEO_MODE_LABELS, applyImagePreset, asObject, canvasMediaRole, canvasRoleCharacters, connectionRejection, upstreamNotReadyText, isGeneratorKind, isVideoModeAvailable, normalizeLegacyParams, presetCodeOf, ruleOf, clearImagePreset, shouldSpawnPresetNode, generationInputReady, generationInputs, mergeMaterialReferences, mergeObjects, modelReconcileRules, nextInputOrder, operationForNode, orderedTargetEdges, reconcileParams, referenceResources, videoModeForInputs, videoModeOptions, insertTextAt, upstreamShotTextOf, estimateRejectionText, type CanvasRoleCharacter } from "~/utils/canvas";
import type { MarketingPickedCharacter } from "~/utils/marketingLibrary";
import { canvasCameraMove, type MaterialPlazaType } from "~/utils/materialPlaza";
import { applyVoiceSelection } from "~/utils/voice";
import { generationQuantities, isVideoModeCode, nodeParamLayout, paramKey, upstreamDefaultOf } from "~/utils/paramLayout";
import { DIRECTOR_NODE_TYPE, directorSuggestionKey, directorSuggestions, type DirectorSuggestion, type DirectorSuggestionKind } from "~/utils/directorCanvas";

// ⚠️ 这个类型从 **生成的新契约** 取，不用 `types/legacy` 里那份老契约的抄本：
//    新契约才有 `group`（`main`/`format`/`advanced`），老契约是 `scope` —— 而参数
//    分区正是本次"界面错乱"的成因。继续用老类型的话，读对了字段反而编译不过。
type ParamConfig = ApiComponents["schemas"]["NodeParamConfig"];
// ⚠️ **复用 store 里那份**，不在这里再抄一遍：抄两份的话改了一处
//    另一处不会报错（两边各自自洽），而不一致的表现是
//    "预览报的价与实际扣的不一样"。
type GenerateRequest = Parameters<ReturnType<typeof useGenerationStore>["generate"]>[1];
type VoiceItem = components["schemas"]["VoiceResp"];
type PresetItem = components["schemas"]["PresetResp"];
type MaterialItem = ApiComponents["schemas"]["MaterialItem"];
// ⚠️ 取生成的新契约（DRB-1）：`types/legacy` 那份没有 `reason`。
type ValidateResponse = ApiComponents["schemas"]["CanvasConnectionValidateResponse"];

const props = defineProps<{ nodeId: string; data: CanvasNodeData }>();
const canvas = useCanvasStore();
const configStore = useCanvasConfigStore();
const generation = useGenerationStore();
const nodeConfig = computed(() => configStore.nodeType(props.data.nodeType));
const params = computed(() => nodeConfig.value?.params ?? []);
/**
 * 参数版式（顶部一行 / 尺寸弹窗 / 底部栏 / 高级项）—— 规则只在 `utils/paramLayout.ts`，
 * 组子项面板用的是同一份。
 *
 * ⚠️ `advanced` 组（反向提示词、随机种子）藏进「展开参数」：它们是少数人才调的，
 *    摆在主工具条上会把常用的模型、尺寸挤出可视区。
 */
const layout = computed(() => nodeParamLayout(props.data.nodeType, params.value));
const topParams = computed(() => layout.value.top);
const bottomParams = computed(() => layout.value.bottom);
const advancedParams = computed(() => layout.value.advanced);
const sizeDialogBefore = computed(() => layout.value.sizeBefore);
const advancedOpen = ref(false);
const formatParams = computed<FormatParam[]>(() => layout.value.size.map(item => ({
  code: item.code ?? "",
  name: item.name ?? item.code ?? "",
  config: controlConfig(item),
  value: props.data.params[paramKey(item.code)],
  defaultValue: item.defaultValue
})));
const modelOptions = ref<components["schemas"]["ModelSummaryResp"][]>([]);
const voicePickerOpen = ref(false);
/** 打开着的素材广场（视频节点：运镜 / 特效）；null 表示关着。 */
const plazaType = ref<MaterialPlazaType | null>(null);
/** 节点上选中的运镜（提示词框里那个标签）。 */
const cameraMove = computed(() => props.data.nodeType === "video" ? canvasCameraMove(props.data.params.camera) : undefined);
const rolePickerOpen = ref(false);
let estimateTimer: ReturnType<typeof setTimeout> | undefined;
let modelSyncVersion = 0;
let modelSyncReady = false;

const incoming = computed(() => orderedTargetEdges(canvas.edges, props.nodeId));
/** 上游智能剪辑的成片（SCD-50）：摘要里最近一次成功导出的那一版。 */
const smartcuts = useSmartcuts();
const uploadedReferences = computed(() => referenceResources(props.data.params.reference));
/** 视频节点「角色库」选的角色：主图作为参考图，排在上传的参考之后。 */
const roleCharacters = computed(() => canvasRoleCharacters(props.data.params.roleLib));
const requestInputs = computed(() => {
  const inputs = generationInputs(canvas.nodes, canvas.edges, props.nodeId, smartcuts.outputs.value);
  // 上传的参考排在连线输入之后，角色排在上传之后，角标顺延；后端按 order 定角标，这里不能出现重号
  uploadedReferences.value.forEach((reference) => {
    inputs.push({
      // ⚠️ 受理侧认的是 **`kind`**（老后端 `type`）与 **`sourceNodeId`**
      //    （老后端 `nodeId`）。名字不对时服务端读到空 kind，
      //    报「输入类型非法」——指向输入内容，而真因是字段名。
      kind: reference.type,
      sourceNodeId: props.nodeId,
      order: inputs.length + 1,
      referenceId: reference.id
    });
  });
  roleCharacters.value.forEach((character) => {
    // 同一张图已经作为上传的参考在里面了就不再占一个角标（上游会白收两张一样的图）
    if (inputs.some(input => input.referenceId === character.mediaId)) return;
    inputs.push({ kind: "image", sourceNodeId: props.nodeId, order: inputs.length + 1, referenceId: character.mediaId });
  });
  return inputs;
});
const unresolvedInputs = computed(() => requestInputs.value.filter(input => !generationInputReady(input)));
const referencesReady = computed(() => unresolvedInputs.value.length === 0);
const referenceTypes = new Set(["text", "script", "image", "video", "audio"]);
const referenceItems = computed(() => incoming.value.map((edge, index) => {
  const source = canvas.nodes.find(node => node.id === edge.source);
  const nodeType = source?.data.nodeType ?? "text";
  const title = source?.data.title ?? "上游节点";
  return {
    edgeId: edge.id,
    nodeId: edge.source,
    // 导演台按图片显示（给下游的是截图，DRT-16）；按节点类型过滤的话它会从参考条上消失
    type: canvasMediaRole(nodeType),
    title,
    pendingTitle: nodeType === DIRECTOR_NODE_TYPE ? `${title} 还没有截图` : nodeType === "smartcut" ? `${title} 还没有导出成片` : `${title} 尚未生成`,
    order: edge.data?.order ?? index + 1,
    previewUrl: source?.data.generation?.previewUrl,
    ready: generationInputReady(requestInputs.value[index]!)
  };
}).filter(item => referenceTypes.has(item.type)));
const inputTypes = computed(() => requestInputs.value.map(input => input.kind));
/** 角色库一次最多挂几个角色：配置（`roleLib` 的 maxCount），不写死在这里。 */
const roleLibraryMax = computed(() => {
  const param = params.value.find(item => item.code === "roleLib");
  const value = Number(asObject(param?.config).maxCount ?? 4);
  return Number.isFinite(value) && value >= 1 ? Math.floor(value) : 4;
});
const pickedRoleCharacters = computed<MarketingPickedCharacter[]>(() => roleCharacters.value
  .map(character => ({ kind: character.kind, id: character.id, name: character.name, mainMediaId: character.mediaId })));
const operation = computed(() => {
  const node = canvas.nodes.find(item => item.id === props.nodeId);
  return node ? operationForNode(node, canvas.edges) : undefined;
});
const status = computed(() => props.data.generation?.status);
const running = computed(() => status.value === "queued" || status.value === "running");
const selectedVoiceId = computed(() => String(props.data.params.voiceId ?? props.data.params.voice ?? ""));
const hasRequiredVoice = computed(() => props.data.nodeType !== "audio" || Boolean(selectedVoiceId.value));
const canGenerate = computed(() => isGeneratorKind(nodeConfig.value?.kind) && Boolean(operation.value) && typeof props.data.params.model === "string" && Boolean(props.data.params.model) && hasRequiredVoice.value && referencesReady.value && !presetTextMissing.value);
const generateHint = computed(() => {
  if (!props.data.params.model) return "请先选择可用模型";
  if (!hasRequiredVoice.value) return "请先选择音色";
  if (!referencesReady.value) {
    return upstreamNotReadyText(canvas.nodes.find(node => node.id === unresolvedInputs.value[0]?.sourceNodeId));
  }
  if (presetTextMissing.value) return `「${selectedPreset.value?.name}」需要先写一段剧情描述`;
  return "开始生成";
});
const showPrompt = computed(() => ["text", "image", "video", "audio"].includes(props.data.nodeType));
const promptBox = ref<HTMLTextAreaElement | null>(null);
/**
 * 上游导演台带下来的镜头文字（DRT-26，DR-F-132）：只给一个「插入」入口，由用户点了才插，**不自动改提示词**。
 * 文字是截图那一刻存在节点参数里的摘要，这里不加载场景。
 */
const shotTexts = computed(() => (["image", "video"].includes(props.data.nodeType) ? upstreamShotTextOf(canvas.nodes, canvas.edges, props.nodeId) : []));

/** 插到光标处（没有光标时追加到末尾），然后把光标放到插入的文字后面。 */
function insertShotText(text: string) {
  const box = promptBox.value;
  const current = String(props.data.params.prompt ?? props.data.params.text ?? "");
  const focused = box && document.activeElement === box;
  const result = insertTextAt(current, text, focused ? box.selectionStart : null, focused ? box.selectionEnd : null);
  editParam("prompt", result.value);
  void nextTick(() => {
    if (!box) return;
    box.focus();
    box.setSelectionRange(result.caret, result.caret);
  });
}
const selectedPreset = computed(() => {
  if (props.data.nodeType !== "image") return undefined;
  const code = String(props.data.params.preset ?? "");
  if (!code) return undefined;
  return configStore.imagePresets.find(item => presetCodeOf(item) === code) as PresetItem | undefined;
});
const presetOptions = computed(() => asObject(props.data.params.presetOptions));
/** 选了预设就用预设自己的说明：它比通用文案准确得多，还顺带说清了要不要写描述。 */
const promptPlaceholder = computed(() => {
  if (selectedPreset.value?.description) return selectedPreset.value.description;
  return props.data.nodeType === "audio"
    ? "输入需要合成的文本"
    : "描述你想生成的内容，@引用已有素材";
});
/**
 * 连了 3D 导演台时的建议条（DR-F-93~95，判定在 utils/directorCanvas）：只提示、不改用户已选的参数。
 * 建议的值来自本节点类型的 `defaultByUpstream` 配置（DRD-45）—— 预设已下线、模式对当前输入不可用时不建议它：
 * 建议一个点了没用的东西比不建议更糟。
 */
const upstreamNodes = computed(() => incoming.value
  .map(edge => canvas.nodes.find(node => node.id === edge.source))
  .filter((node): node is NonNullable<typeof node> => Boolean(node)));
const directorDefaults = computed(() => {
  const presetCode = upstreamDefaultOf(params.value, "preset", DIRECTOR_NODE_TYPE);
  const preset = presetCode ? configStore.imagePresets.find(item => presetCodeOf(item) === presetCode) : undefined;
  const videoMode = upstreamDefaultOf(params.value, "video_mode", DIRECTOR_NODE_TYPE);
  return {
    ...(preset ? { presetCode, presetName: String(preset.name ?? presetCode) } : {}),
    ...(videoMode && isVideoModeAvailable(videoMode, inputTypes.value)
      ? { videoMode, videoModeName: VIDEO_MODE_LABELS[videoMode] ?? videoMode }
      : {})
  };
});
/** 关掉的建议条：只存内存与 sessionStorage，不写画布（关一下就产生一次画布保存不值）。 */
const dismissedTips = ref<ReadonlySet<DirectorSuggestionKind>>(new Set());
const directorTips = computed(() => directorSuggestions({ data: props.data }, upstreamNodes.value, directorDefaults.value)
  .filter(tip => !tip.dismissible || !dismissedTips.value.has(tip.kind)));

function tipStorage(): Pick<Storage, "getItem" | "setItem"> | undefined {
  try {
    return import.meta.client ? window.sessionStorage : undefined;
  } catch {
    return undefined;
  }
}

function loadDismissedTips() {
  const storage = tipStorage();
  const kinds: DirectorSuggestionKind[] = ["preset", "first_frame", "character"];
  dismissedTips.value = new Set(kinds.filter((kind) => {
    try {
      return storage?.getItem(directorSuggestionKey(canvas.projectId, props.nodeId, kind)) === "1";
    } catch {
      return false;
    }
  }));
}

function dismissDirectorTip(tip: DirectorSuggestion) {
  dismissedTips.value = new Set([...dismissedTips.value, tip.kind]);
  try {
    tipStorage()?.setItem(directorSuggestionKey(canvas.projectId, props.nodeId, tip.kind), "1");
  } catch {
    // 存不进去（隐私模式、配额满）只影响刷新之后：这次会话里照样关着
  }
}

function applyDirectorTip(tip: DirectorSuggestion) {
  const apply = tip.apply;
  if (!apply) return;
  canvas.markTouched(props.nodeId);
  if (apply.param === "preset") {
    const preset = configStore.imagePresets.find(item => presetCodeOf(item) === apply.value);
    if (!preset) return;
    // 写在本节点上、不派生新节点：建议的是「这个节点照导演台截图构图」，与在选择器里给已出的图挑加工玩法不是一回事
    canvas.updateParams(props.nodeId, applyImagePreset(props.data.params, preset));
    void refreshEstimate();
    return;
  }
  updateParam("mode", apply.value);
}

/** 故事板类预设必须先写剧情才有得可画，空描述提交只会烧积分换一张随机图。 */
const presetNeedsText = computed(() => selectedPreset.value?.inputMode === "text_required");
const presetTextMissing = computed(() =>
  presetNeedsText.value && !String(props.data.params.prompt ?? "").trim());

function referenceIcon(type: string) {
  return {
    image: ImageIcon,
    video: Video,
    audio: AudioLines
  }[type] ?? FileText;
}

function removeReference(edgeId: string) {
  canvas.snapshot();
  const remaining = canvas.edges.filter(edge => edge.id !== edgeId);
  const orderById = new Map(orderedTargetEdges(remaining, props.nodeId).map((edge, index) => [edge.id, index + 1]));
  canvas.edges = remaining.map(edge => edge.target === props.nodeId
    ? { ...edge, data: { ...edge.data, order: orderById.get(edge.id) ?? edge.data?.order ?? 1 } }
    : edge);
  canvas.scheduleSave();
}

/**
 * 大列表懒加载：`param_def.config.domainFrom` 指向一个目录，选项**不内嵌在配置里**
 * （模型/音色/素材动辄上百条，塞进 `/canvas/config` 会让每次开画布都拖一份全量）。
 *
 * ⚠️ 老前端只认 `model` 一个 code、按 code 写死；新后端把它统一成了 `domainFrom`。
 *    不接这条的表现是：模式、风格、特效、预设的选项**恒为空** ——
 *    控件渲染出一个光秃秃的标签，看起来像"这个功能还没做"。
 */
function optionsFromDomain(domain: string, config: JsonObject): unknown[] | undefined {
  if (domain === "models") {
    return modelOptions.value.map(model => ({
      label: `${model.name ?? model.code}${model.isVip ? " · VIP" : ""}`,
      value: model.code
    }));
  }
  if (domain === "videoModes") {
    return configStore.videoModes.map(code => ({ label: VIDEO_MODE_LABELS[code] ?? code, value: code }));
  }
  if (domain === "materials") {
    const type = String(config.materialType ?? "style");
    return (configStore.materialLists[type === "effect" ? "effect" : "style"] ?? [])
      .map(item => ({ label: item.name ?? item.code, value: item.code }));
  }
  return undefined;
}

function controlConfig(param: ParamConfig) {
  const base = asObject(param.config);
  let value = base;
  const override = asObject(props.data.modelOverrides?.[param.code ?? ""]);
  value = mergeObjects(value, override);
  const domain = typeof value.domainFrom === "string" ? value.domainFrom : "";
  // ⚠️ `model` 这条保留按 code 判：老后端的模型参数没有 domainFrom。
  const resolved = optionsFromDomain(domain || (param.code === "model" ? "models" : ""), value);
  if (resolved) value.options = resolved;
  // ⚠️ 视频模式要按**当前实际连入的输入**算可用性（首尾帧要恰好 2 张图…），
  //    所以拿到目录之后还要再过一道，而不是直接把目录当选项。
  if (isVideoModeParam(param) && Array.isArray(value.options)) {
    value.options = videoModeOptions(base.options ?? value.options, value.options, inputTypes.value);
  }
  return value;
}

function isVideoModeParam(param: ParamConfig) {
  return isVideoModeCode(param.code);
}

function updateParam(code: string, value: unknown) {
  canvas.updateParams(props.nodeId, { ...props.data.params, [code]: value });
  if (code === "model" && typeof value === "string") {
    void applyModelOverrides(value, requiredOperation());
  }
}

/**
 * 用户在面板上亲手改参数（AGT-11）：Agent 建的节点被用户改过之后，Agent 不再改它。
 * ⚠️ 只给模板里的用户操作用 —— 挂载时补默认值、切模型时对齐参数也走 updateParam，
 *    那些不是用户的修改，标成「改过」的话用户只是点开一下 Agent 的节点就把它锁住了。
 */
function editParam(code: string, value: unknown) {
  canvas.markTouched(props.nodeId);
  updateParam(code, value);
}

/**
 * 视频节点的生成模式跟着输入走（连线 + 上传的参考 + 角色）。
 * 挂了角色时优先「全能参考」：角色图是参考，不是首帧 —— 只有一张图时按通用规则会落成图生视频，
 * 那样角色就成了视频的第一帧。
 */
function videoModeWith(references: { type: string }[], characters: CanvasRoleCharacter[]) {
  const types = [
    ...incoming.value.map(edge => sourceType(edge.source)),
    ...references.map(reference => reference.type),
    ...characters.map(() => "image")
  ];
  const current = String(props.data.params.mode || "text2video");
  if (characters.length && isVideoModeAvailable("ref2video", types)) return "ref2video";
  return videoModeForInputs(current, types);
}

function updateUploadedReference(value: unknown) {
  canvas.markTouched(props.nodeId);
  const references = referenceResources(value);
  const next = { ...props.data.params };
  if (references.length) next.reference = references;
  else delete next.reference;
  if (props.data.nodeType === "video") next.mode = videoModeWith(references, roleCharacters.value);
  canvas.updateParams(props.nodeId, next);
}

/**
 * 从节点上拿掉一份上传的参考。
 *
 * ⚠️ 只拿掉引用、不删文件：参考就是工作区素材库里的一份媒体（新后端没有单独的「参考」记录可删），
 *    它可能还被别的节点、别的生成用着；要删去素材库删（那里有引用保护）。
 *    此前这里调 `DELETE /media/references/{id}` —— 新后端没有这条路由，每次移除都弹一条「删除失败」。
 */
function removeUploadedReference(referenceId: string) {
  updateUploadedReference(uploadedReferences.value.filter(item => item.id !== referenceId));
}

/** 角色库弹窗「添加」：节点上的角色整体换成这次的选择（顺序即选择顺序）。 */
function updateRoleCharacters(characters: CanvasRoleCharacter[]) {
  canvas.markTouched(props.nodeId);
  const next = { ...props.data.params };
  if (characters.length) next.roleLib = characters;
  else delete next.roleLib;
  if (props.data.nodeType === "video") next.mode = videoModeWith(uploadedReferences.value, characters);
  canvas.updateParams(props.nodeId, next);
}

function selectRoleCharacters(picked: MarketingPickedCharacter[]) {
  rolePickerOpen.value = false;
  // 没有图的角色（脚本资产还没出图）当不了参考：说出来，不悄悄丢掉
  const missing = picked.filter(item => !item.mainMediaId).map(item => item.name);
  if (missing.length) ElMessage.warning(`${missing.join("、")} 还没有图，不能作为参考`);
  updateRoleCharacters(canvasRoleCharacters(picked.map(item => ({
    kind: item.kind, id: item.id, name: item.name, mediaId: item.mainMediaId ?? ""
  }))));
}

function removeRoleCharacter(character: CanvasRoleCharacter) {
  updateRoleCharacters(roleCharacters.value.filter(item => !(item.kind === character.kind && item.id === character.id)));
}

/** 角色在库里被删了 / 改了（在弹窗里操作的）：节点上正挂着它的话跟着拿掉 / 换名字和主图。 */
function onRoleCharacterDeleted(id: string) {
  const left = roleCharacters.value.filter(item => !(item.kind === "marketing_character" && item.id === id));
  if (left.length !== roleCharacters.value.length) updateRoleCharacters(left);
}
function onRoleCharacterUpdated(character: MarketingPickedCharacter) {
  const index = roleCharacters.value.findIndex(item => item.kind === character.kind && item.id === character.id);
  if (index < 0) return;
  const next = [...roleCharacters.value];
  if (!character.mainMediaId) {
    next.splice(index, 1);
  } else {
    next[index] = { ...next[index]!, name: character.name, mediaId: character.mainMediaId };
  }
  updateRoleCharacters(next);
}

function controlValue(param: ParamConfig) {
  if (param.code === "voice") return props.data.params.voiceName;
  const current = props.data.params[paramKey(param.code)];
  if (current !== undefined && current !== null && current !== "") return current;
  // ⚠️ 新节点的 params 是空的，而选中态要靠默认值撑起来。
  //    老后端把默认值放在 `config.default`，新后端放在顶层 **`defaultValue`** ——
  //    只读一处的表现是"新建的图片节点画幅一个都没高亮"，看起来像没配默认值。
  return (param as { defaultValue?: unknown }).defaultValue ?? asObject(param.config).default;
}

function handleControlAction(action: string) {
  if (action === "openVoicePicker" && props.data.nodeType === "audio") {
    voicePickerOpen.value = true;
  } else if (action === "openEffectPicker" && props.data.nodeType === "video") {
    plazaType.value = "effect";
  } else if (action === "openCameraPicker" && props.data.nodeType === "video") {
    plazaType.value = "camera";
  } else if (action === "openRolePicker" && props.data.nodeType === "video") {
    rolePickerOpen.value = true;
  }
}

function sourceType(nodeId: string) {
  return canvas.nodes.find(node => node.id === nodeId)?.data.nodeType ?? "text";
}

async function validateVideoInputs(mode: string, sources: Array<{ sourceType: string; order: number }>) {
  return useApi()<ValidateResponse>("/canvas/connections/validate", {
    method: "POST",
    body: { targetType: "video", mode, inputs: sources }
  });
}

/**
 * 选一个特效：连一个特效素材节点到这个视频节点（已经连着特效的话换掉它的素材），服务端按素材码拼词。
 * ⚠️ 素材节点要存 **`materialCode`**（生成输入按它声明 `kind=material`，见 utils/canvas.generationInputs）；
 *    此前存的是老后端的 `effectId`，新后端的条目没有 id，于是选了也什么都不发生。
 */
async function selectVideoEffect(item: MaterialItem) {
  canvas.markTouched(props.nodeId);
  if (!item.code) return;
  const effectParams = {
    materialCode: item.code,
    name: item.name,
    cover: item.coverUrl ?? "",
    category: item.category ?? "",
    commercial: item.isCommercial
  };
  const existingEdge = incoming.value.find(edge => sourceType(edge.source) === "effect");
  const existingNode = existingEdge ? canvas.nodes.find(node => node.id === existingEdge.source) : undefined;
  try {
    let validation: ValidateResponse | undefined;
    if (!existingNode) {
      validation = await validateVideoInputs(String(props.data.params.mode || "text2video"), [
        ...incoming.value.map(edge => ({ sourceType: sourceType(edge.source), order: edge.data?.order ?? 1 })),
        { sourceType: "effect", order: nextInputOrder(canvas.edges, props.nodeId) }
      ]);
      // DRB-1：`valid:false` 是 HTTP 200，不会进 catch —— 不看它的话特效节点照样建出来、连线照样落下
      const rejection = connectionRejection(validation);
      if (rejection) {
        ElMessage.error(rejection);
        return;
      }
    }
    canvas.snapshot();
    if (existingNode) {
      existingNode.data.title = item.name ?? "特效";
      existingNode.data.params = effectParams;
    } else {
      const target = canvas.nodes.find(node => node.id === props.nodeId);
      const order = nextInputOrder(canvas.edges, props.nodeId);
      const effectNodeId = canvas.addNode("effect", item.name ?? "特效", effectParams, {
        position: target ? { x: target.position.x - 390, y: target.position.y + 40 } : undefined,
        recordHistory: false,
        persist: false
      });
      canvas.edges.push({
        id: `e_${effectNodeId}_${props.nodeId}_${Date.now().toString(36)}`,
        source: effectNodeId,
        sourceHandle: "out",
        target: props.nodeId,
        targetHandle: "in",
        animated: true,
        data: { order, operation: validation?.operation }
      });
    }
    updateParam("effectLib", item.name ?? "特效");
    canvas.selectedNodeId = props.nodeId;
    plazaType.value = null;
  } catch (cause) {
    ElMessage.error(cause instanceof Error ? cause.message : "特效应用失败");
  }
}

/** 运镜广场里选一个：节点里只存 code 与名字（提示词框里显示成标签），提示词由服务端拼。 */
function selectCameraMove(item: MaterialItem) {
  plazaType.value = null;
  canvas.markTouched(props.nodeId);
  canvas.updateParams(props.nodeId, { ...props.data.params, camera: { code: item.code, name: item.name } });
}

function removeCameraMove() {
  canvas.markTouched(props.nodeId);
  const next = { ...props.data.params };
  delete next.camera;
  canvas.updateParams(props.nodeId, next);
}

function onPlazaSelect(item: MaterialItem) {
  if (plazaType.value === "camera") selectCameraMove(item);
  else if (plazaType.value === "effect") void selectVideoEffect(item);
}

function selectVoice(voice: VoiceItem) {
  canvas.markTouched(props.nodeId);
  canvas.updateParams(props.nodeId, applyVoiceSelection(props.data.params, voice));
  voicePickerOpen.value = false;
  void refreshEstimate();
}

function selectImagePreset(preset: PresetItem) {
  canvas.markTouched(props.nodeId);
  const node = canvas.nodes.find(item => item.id === props.nodeId);
  if (node && shouldSpawnPresetNode(node)) {
    canvas.addPresetNode(props.nodeId, preset, imageToImageOperation());
    return;
  }
  canvas.updateParams(props.nodeId, applyImagePreset(props.data.params, preset));
  void refreshEstimate();
}

/** 以配置下发的连线规则为准，规则缺失时才退回内置值。 */
function imageToImageOperation() {
  const rule = (configStore.config?.connectionRules ?? []).find(
    (item) => {
      // ⚠️ 走 ruleOf 归一：直接读 item.source 的话新后端下恒为 undefined，
      //    "图生图可用"永远判成 false。
      const r = ruleOf(item);
      return r.source === "image" && r.target === "image" && r.allowed;
    });
  return String(rule?.operation ?? "img2img");
}

function updatePresetOptions(value: JsonObject) {
  canvas.markTouched(props.nodeId);
  canvas.updateParams(props.nodeId, { ...props.data.params, presetOptions: value });
  void refreshEstimate();
}

function clearPreset() {
  canvas.markTouched(props.nodeId);
  canvas.updateParams(props.nodeId, clearImagePreset(props.data.params));
  void refreshEstimate();
}

function requiredOperation() {
  if (props.data.nodeType === "video") {
    return videoModeForInputs(
      String(props.data.params.mode || "text2video"),
      inputTypes.value
    );
  }
  return operation.value;
}

async function applyModelOverrides(modelCode: string, preserveOperation?: string) {
  const overrides = await configStore.overrides(modelCode) as JsonObject;
  canvas.updateModelOverrides(props.nodeId, overrides);
  const next = reconcileParams(props.data.params, modelReconcileRules(params.value, overrides));
  if (props.data.nodeType === "video" && preserveOperation) next.mode = preserveOperation;
  canvas.updateParams(props.nodeId, next);
}

async function syncModelsForOperation() {
  const syncVersion = ++modelSyncVersion;
  const expectedOperation = requiredOperation();
  const options = await configStore.models(props.data.nodeType, expectedOperation);
  if (syncVersion !== modelSyncVersion) return;
  modelOptions.value = options;

  const currentModel = typeof props.data.params.model === "string"
    ? props.data.params.model
    : "";
  const selected = options.find(model => model.code === currentModel) ?? options[0];
  const next = { ...props.data.params };
  if (props.data.nodeType === "video" && expectedOperation) next.mode = expectedOperation;
  if (!selected?.code) {
    delete next.model;
    canvas.updateModelOverrides(props.nodeId, {});
    canvas.updateParams(props.nodeId, next);
    return;
  }

  next.model = selected.code;
  const overrides = await configStore.overrides(selected.code) as JsonObject;
  if (syncVersion !== modelSyncVersion) return;
  const reconciled = reconcileParams(next, modelReconcileRules(params.value, overrides));
  if (props.data.nodeType === "video" && expectedOperation) reconciled.mode = expectedOperation;
  canvas.updateModelOverrides(props.nodeId, overrides);
  canvas.updateParams(props.nodeId, reconciled);
}

function generationRequest(): GenerateRequest | null {
  if (!operation.value) return null;
  const model = props.data.params.model;
  if (typeof model !== "string" || !model) return null;
  if (!hasRequiredVoice.value) return null;
  if (!referencesReady.value) return null;
  return {
    projectId: canvas.projectId,
    nodeId: props.nodeId,
    operation: operation.value,
    // ⚠️ 字段名是 **`modelCode`**（老后端 `model`）。写成 `model` 的话
    //    服务端收到空模型码，报的是「模型不存在」——指向模型配置，
    //    而真因是字段名。
    modelCode: model,
    params: mergeMaterialReferences(props.data.params, canvas.nodes, canvas.edges, props.nodeId),
    inputs: requestInputs.value,
    ...generationQuantities(props.data.nodeType, props.data.params)
  };
}

/** 报价被拒的原因（DRT-25，DR-F-128）：例如预演视频连到不收参考视频的模型。原样显示，不猜 */
const estimateRejection = ref("");

async function refreshEstimate() {
  const request = generationRequest();
  if (!request) {
    estimateRejection.value = "";
    return;
  }
  try {
    await generation.estimate(props.nodeId, request);
    estimateRejection.value = "";
  } catch (cause) {
    generation.estimates[props.nodeId] = 0;
    estimateRejection.value = estimateRejectionText(cause);
  }
}

async function generate() {
  const request = generationRequest();
  if (!request || !canGenerate.value) return;
  try {
    await generation.generate(props.nodeId, request);
    canvas.scheduleSave();
  } catch (cause) {
    ElMessage.error(cause instanceof Error ? cause.message : "生成失败");
  }
}

watch(() => JSON.stringify([props.data.params, incoming.value]), () => {
  clearTimeout(estimateTimer);
  estimateTimer = setTimeout(refreshEstimate, 350);
}, { deep: true });

watch(() => JSON.stringify([operation.value, inputTypes.value]), () => {
  if (modelSyncReady) void syncModelsForOperation();
});

onMounted(async () => {
  loadDismissedTips();
  if (nodeConfig.value?.kind === "material" || ["script", "group"].includes(props.data.nodeType)) return;
  // 老形态参数（老前端的 `size` 对象、误写的 `video_mode`）先归一，**再**补默认值 ——
  // 顺序反过来的话，老节点里存着的画幅会被默认的 1:1 抢先占住。
  const normalized = normalizeLegacyParams(props.data.params);
  if (normalized !== props.data.params) canvas.updateParams(props.nodeId, normalized);
  for (const param of params.value) {
    const key = paramKey(param.code);
    if (key && props.data.params[key] === undefined && param.defaultValue !== undefined) {
      updateParam(key, param.defaultValue);
    }
  }
  // `domainFrom: "materials"` 的目录要先拉回来，否则风格/特效控件永远是空的。
  // ⚠️ 失败不阻断：store 里已经吞掉异常并缓存空数组，素材没配是正常状态。
  await Promise.all([...new Set(params.value
    .filter(param => asObject(param.config).domainFrom === "materials")
    .map(param => String(asObject(param.config).materialType ?? "style")))]
    .map(type => configStore.materials(type)));
  await syncModelsForOperation();
  modelSyncReady = true;
  await refreshEstimate();
});
</script>

<template>
  <section v-if="nodeConfig?.kind !== 'material' && !['script', 'group'].includes(data.nodeType)" class="node-param-panel nodrag nowheel">
    <div v-for="tip in directorTips" :key="tip.kind" class="director-tip" :data-tip="tip.kind" role="note">
      <Clapperboard :size="13" />
      <span>{{ tip.text }}</span>
      <button v-if="tip.apply" type="button" class="director-tip-apply" @click="applyDirectorTip(tip)">{{ tip.apply.label }}</button>
      <button v-if="tip.dismissible" type="button" class="director-tip-close" title="不再提示" aria-label="不再提示" @click="dismissDirectorTip(tip)"><X :size="11" /></button>
    </div>
    <div class="node-top-controls">
      <VideoTopTools v-if="data.nodeType === 'video'" :controls="topParams" :params="data.params" :project-id="canvas.projectId" :node-id="nodeId" @update="(code, value) => code === 'reference' ? updateUploadedReference(value) : editParam(code, value)" @action="handleControlAction" />
      <template v-else>
        <!--
          ⚠️ 参考图必须走 `ReferenceUploadButton`（真上传到项目并回引用 ID），
             **不能**落到通用的 `UploadControl` —— 后者只 emit `{name,size}`，
             一个字节都没传上去，而界面上它看起来上传成功了。
        -->
        <template v-for="param in topParams" :key="param.code">
          <ReferenceUploadButton
            v-if="param.code === 'reference'"
            :project-id="canvas.projectId"
            :node-id="nodeId"
            :model-value="controlValue(param)"
            :config="controlConfig(param)"
            :name="param.name ?? '参考'"
            @update:model-value="updateUploadedReference"
          />
          <DynamicControl v-else :control="param.control ?? ''" :name="param.name ?? param.code ?? ''" :config="controlConfig(param)" :model-value="controlValue(param)" @update:model-value="editParam(paramKey(param.code), $event)" @action="handleControlAction" />
        </template>
      </template>
      <button
        v-if="advancedParams.length"
        class="node-expand"
        type="button"
        :class="{ active: advancedOpen }"
        :title="advancedOpen ? '收起高级参数' : '展开高级参数'"
        :aria-expanded="advancedOpen"
        @click="advancedOpen = !advancedOpen"
      ><Maximize2 :size="15" /></button>
    </div>
    <!--
      ⚠️ `group === 'advanced'` 的参数（负向提示词、随机种子）**不进主工具条**：
         它们是少数人才调的，摆在常用的画幅/时长旁边只会把后者挤出可视区。
    -->
    <div v-if="advancedOpen && advancedParams.length" class="node-advanced-controls">
      <DynamicControl
        v-for="param in advancedParams"
        :key="param.code"
        :control="param.control ?? ''"
        :name="param.name ?? param.code ?? ''"
        :config="controlConfig(param)"
        :model-value="controlValue(param)"
        @update:model-value="editParam(paramKey(param.code), $event)"
        @action="handleControlAction"
      />
    </div>
    <div v-if="referenceItems.length || uploadedReferences.length || roleCharacters.length" class="node-reference-strip" aria-label="节点参考资源">
      <div v-for="reference in referenceItems" :key="reference.edgeId" class="node-reference-token" :class="{ pending: !reference.ready }">
        <button class="node-reference-open" type="button" :title="reference.ready ? reference.title : reference.pendingTitle" @click="canvas.selectedNodeId = reference.nodeId">
          <span class="node-reference-order">{{ reference.order }}</span>
          <img v-if="reference.type === 'image' && reference.previewUrl" :src="reference.previewUrl" :alt="reference.title">
          <component :is="referenceIcon(reference.type)" v-else :size="23" />
        </button>
        <button class="node-reference-remove" type="button" title="移除引用" @click="removeReference(reference.edgeId)"><X :size="11" /></button>
      </div>
      <ReferenceResourceToken
        v-for="(reference, index) in uploadedReferences"
        :key="reference.id"
        :value="reference"
        :order="referenceItems.length + index + 1"
        @remove="removeUploadedReference(reference.id)"
      />
      <CharacterReferenceToken
        v-for="(character, index) in roleCharacters"
        :key="`${character.kind}:${character.id}`"
        :character="character"
        :order="referenceItems.length + uploadedReferences.length + index + 1"
        @remove="removeRoleCharacter(character)"
      />
    </div>
    <ImagePresetChip
      v-if="selectedPreset"
      :preset="selectedPreset"
      :options="presetOptions"
      @update="updatePresetOptions"
      @clear="clearPreset"
    />
    <textarea v-if="showPrompt" ref="promptBox" :value="String(data.params.prompt ?? data.params.text ?? '')" :placeholder="promptPlaceholder" @input="editParam(data.nodeType === 'audio' ? 'text' : 'prompt', ($event.target as HTMLTextAreaElement).value)" />
    <div v-if="showPrompt && shotTexts.length" class="node-shot-texts" data-testid="insert-shot-text">
      <button
        v-for="item in shotTexts"
        :key="item.nodeId"
        type="button"
        class="node-shot-text"
        :title="`插入「${item.text}」（来自 ${item.title}）`"
        @mousedown.prevent
        @click="insertShotText(item.text)"
      ><TextCursorInput :size="12" />插入镜头文字<small>{{ shotTexts.length > 1 ? item.title : item.text }}</small></button>
    </div>
    <!-- 选中的运镜以标签显示在提示词框里（对标产品同款）；词由服务端拼，标签不往提示词里写字 -->
    <div v-if="cameraMove" class="node-prompt-tags">
      <CameraMoveChip :name="cameraMove.name" @open="plazaType = 'camera'" @remove="removeCameraMove" />
    </div>
    <p v-if="estimateRejection" class="node-quote-rejection" role="alert" data-testid="quote-rejection"><CircleAlert :size="13" />{{ estimateRejection }}</p>
    <div class="node-bottom-controls">
      <div class="node-control-scroll">
        <template v-for="param in bottomParams" :key="param.code">
          <!-- 「尺寸」弹窗（画质 · 比例 · 数量 / 清晰度 · 比例 · 时长）：老系统里点开才出现，不平铺 -->
          <GenerationSizeControl
            v-if="param.code === sizeDialogBefore"
            :node-type="data.nodeType"
            :params="formatParams"
            @update="editParam"
          />
          <VideoModeControl
            v-if="data.nodeType === 'video' && isVideoModeParam(param)"
            :model-value="controlValue(param)"
            :config="controlConfig(param)"
            @update:model-value="canvas.markTouched(nodeId); updateParam('mode', $event)"
          />
          <ImagePresetControl
            v-else-if="data.nodeType === 'image' && param.code === 'preset'"
            :presets="configStore.imagePresets"
            :selected="String(data.params.preset ?? '')"
            @select="selectImagePreset"
          />
          <DynamicControl v-else :control="param.control ?? ''" :name="param.name ?? param.code ?? ''" :config="controlConfig(param)" :model-value="controlValue(param)" @update:model-value="editParam(paramKey(param.code), $event)" @action="handleControlAction" />
        </template>
        <GenerationSizeControl
          v-if="!sizeDialogBefore"
          :node-type="data.nodeType"
          :params="formatParams"
          @update="editParam"
        />
      </div>
      <!-- 原「预计积分」角标的位置（TT-T-14 删除金额）：保留空占位把发送键推到右侧，布局不变 -->
      <span class="node-estimate" aria-hidden="true" />
      <button class="node-generate" type="button" :disabled="!canGenerate || running" :title="generateHint" @click="generate">
        <LoaderCircle v-if="running" :size="17" class="spin" />
        <ArrowUp v-else :size="18" />
      </button>
    </div>
  </section>
  <VoicePicker v-model="voicePickerOpen" :selected-id="selectedVoiceId" @select="selectVoice" />
  <MaterialPlaza
    :model-value="plazaType !== null"
    :type="plazaType ?? 'camera'"
    :selected-code="plazaType === 'camera' ? cameraMove?.code : undefined"
    @update:model-value="open => { if (!open) plazaType = null; }"
    @select="onPlazaSelect"
  />
  <CanvasCharacterPicker
    v-if="rolePickerOpen"
    :selected="pickedRoleCharacters"
    :max-refs="roleLibraryMax"
    @close="rolePickerOpen = false"
    @pick="selectRoleCharacters"
    @deleted="onRoleCharacterDeleted"
    @updated="onRoleCharacterUpdated"
  />
</template>
