<script setup lang="ts">
import { ChevronLeft, ChevronRight, CircleAlert, CircleCheck, Copy, FileText, Film, Image as ImageIcon, LoaderCircle, MoreHorizontal, Plus, RectangleHorizontal, Save, Sparkles, Trash2, X } from "@lucide/vue";
import { ElMessage } from "element-plus";
import type { components } from '~/types/legacy';
import ScriptDescriptionRichEditor from "./ScriptDescriptionRichEditor.vue";
import type { GroupDetail, ScriptAsset, ScriptDetail, ScriptPromptType, ScriptShot } from "~/types/script";
import { asObject, liftLegacySize, mergeObjects } from "~/utils/canvas";
import { mergeMentionEntities, newCanvasNodeId, normalizeGroupDetail, shotsLeftOut, type ComposeResultResp } from "~/utils/scriptContract";
import {
  segmentScriptMentions,
  type ScriptMentionAsset,
  type ScriptMentionSegment,
  type ScriptMentionType
} from "~/utils/scriptMentions";

const props = defineProps<{
  scriptId: string;
  nodeId?: string;
  initialAction?: "storyboard" | "video";
}>();
const emit = defineEmits<{
  close: [];
  updated: [detail: ScriptDetail];
  "group-created": [group: GroupDetail];
}>();
const config = useCanvasConfigStore();
/** 脚本接口只走这一个入口（新契约，见 utils/scriptContract）。 */
const scripts = useScriptApi();
const media = useAuthenticatedMedia();
const detail = ref<ScriptDetail>();
const shots = ref<ScriptShot[]>([]);
const shotsBaseline = ref("[]");
const activeStep = ref(1);
const loading = ref(true);
const saving = ref(false);
const composing = ref<string[]>([]);
const batchBusy = ref(false);
const textModels = ref<components["schemas"]["ModelSummaryResp"][]>([]);
const imageModels = ref<components["schemas"]["ModelSummaryResp"][]>([]);
const assetImageModels = ref<components["schemas"]["ModelSummaryResp"][]>([]);
const videoModels = ref<components["schemas"]["ModelSummaryResp"][]>([]);
const selectedPromptShot = ref<ScriptShot>();
const imagePromptDraft = ref("");
const videoPromptDraft = ref("");
const promptSaving = ref(false);
const promptComposeMode = ref<"smart" | "template">("smart");
const selectedPromptTextModel = ref("");
const selectedImageModel = ref("");
const selectedAssetImageModel = ref("");
const selectedVideoModel = ref("");
const shotSearch = ref("");
const selectedShotIds = ref<string[]>([]);
const promptBatchDialogOpen = ref(false);
const promptBatchProgress = ref<{ done: number; total: number }>();
/**
 * 一键合成的并发度。各镜之间没有依赖 —— 每镜只用自己的画面描述、景别、光影与本镜资产，
 * 互不引用，所以可以并发。实测单镜 26~34 秒，串行 20 个镜头要等 10 分钟。
 * 不设上限则会把几十个请求同时砸向上游，触发限流反而更慢，取折中值。
 */
const PROMPT_COMPOSE_CONCURRENCY = 5;
const promptBatchSelectedIds = ref<string[]>([]);
const batchDialog = ref<"storyboard" | "video">();
const storyboardSettings = reactive({ ratio: "16:9", quality: "standard", clarity: "2K", count: 1 });
const videoSettings = reactive({ ratio: "16:9", clarity: "720P", audio: true });
const videoDurations = reactive<Record<string, number>>({});
const uploadingAssetId = ref("");
const assetPreviewUrls = ref<Record<string, string>>({});
const assetPreviewSources = ref<Record<string, string>>({});
const editingAssetId = ref("");
const assetNameDraft = ref("");
const assetDescriptionDraft = ref("");
const assetDescriptionInput = ref<HTMLTextAreaElement>();
const assetSaving = ref(false);
const assetGenerationDialogOpen = ref(false);
const selectedAssetGenerationIds = ref<string[]>([]);
const assetGenerationDrafts = ref<Record<string, string>>({});
const assetGenerationSettings = reactive<Record<"quality" | "clarity" | "ratio", string>>({
  quality: "standard",
  clarity: "2K",
  ratio: "2:1"
});
const assetModelOverrides = ref<Record<string, unknown>>({});
const editingDescriptionShotId = ref("");
const descriptionDraft = ref("");
const descriptionOriginal = ref("");
const descriptionEditorPosition = ref<Record<string, string>>({});
const descriptionEditor = ref<{ focusEnd: () => void }>();
let assetRefreshTimer: ReturnType<typeof setTimeout> | undefined;
let assetModelRequest = 0;

const current = computed(() => detail.value);
const filteredShots = computed(() => {
  const keyword = shotSearch.value.trim().toLowerCase();
  if (!keyword) return shots.value;
  return shots.value.filter(shot => [shot.shotNo, shot.description, shot.dialogue, shot.imagePrompt, shot.videoPrompt]
    .some(value => String(value ?? "").toLowerCase().includes(keyword)));
});
const shotsDirty = computed(() => shotDraftFingerprint(shots.value) !== shotsBaseline.value);
const promptBatchShots = computed(() => shots.value.filter(shot => !shot.id.startsWith("draft-")));
const allPromptBatchSelected = computed(() => promptBatchShots.value.length > 0
  && promptBatchShots.value.every(shot => promptBatchSelectedIds.value.includes(shot.id)));
/**
 * 一键合成的 dryRun（GAP-21）：服务端会跳过已就绪且没有改动的镜头，跳过几条要说出来。
 * tabtv 不计价（TT-T-14）：只显示跳过条数，不显示金额。
 */
const promptBatchQuote = ref<ComposeResultResp["quote"]>();
let promptQuoteTimer: ReturnType<typeof setTimeout> | undefined;
let promptQuoteRequest = 0;
const finalPromptsReady = computed(() => shots.value.filter(
  shot => shot.imagePromptStatus === "ready" && shot.videoPromptStatus === "ready"
).length);
const assetsReady = computed(() => (current.value?.assets ?? []).filter(asset => asset.status === "ready").length);
const assetsComplete = computed(() => assetsReady.value === (current.value?.assets.length ?? 0));
const assetsGenerating = computed(() => (current.value?.assets ?? []).some(asset => asset.status === "generating"));
const assetBoardReady = computed(() => Boolean(current.value?.assets.length) && assetsComplete.value);
const assetGroupExists = computed(() => current.value?.groups.some(group => group.groupType === "asset") ?? false);
const assetGenerationCandidates = computed(() => (current.value?.assets ?? [])
  .filter(asset => asset.status !== "ready" && asset.status !== "generating"));
const selectedAssetGenerationCount = computed(() => selectedAssetGenerationIds.value.length);
const allAssetGenerationSelected = computed(() => assetGenerationCandidates.value.length > 0
  && assetGenerationCandidates.value.every(asset => selectedAssetGenerationIds.value.includes(asset.id)));
const storyboardEligible = computed(() => shots.value.filter(shot => shot.imagePromptStatus === "ready" && Boolean(shot.imagePrompt)));
const videoEligible = computed(() => shots.value.filter(shot => shot.videoPromptStatus === "ready" && Boolean(shot.videoPrompt)));
const completedStepCount = computed(() => [1, 2, 3].filter(step => isStepComplete(step)).length);
const descriptionEditorShot = computed(() => shots.value.find(shot => shot.id === editingDescriptionShotId.value));
const mentionAssets = computed<ScriptMentionAsset[]>(() => {
  const byKey = new Map<string, ScriptMentionAsset>();
  for (const asset of current.value?.assets ?? []) {
    byKey.set(`${asset.assetType}:${asset.name.toLocaleLowerCase()}`, {
      id: asset.id,
      name: asset.name,
      assetType: asset.assetType
    });
  }
  for (const shot of shots.value) {
    // 分镜给出的实体（还没对账成资产时也在）：输入 @ 就该能选到它们（CD-78）
    for (const entity of shot.entities ?? []) {
      const key = `${entity.type}:${entity.name.toLocaleLowerCase()}`;
      if (!byKey.has(key)) byKey.set(key, { name: entity.name, assetType: entity.type });
    }
    for (const segment of segmentScriptMentions(shot.description, Array.from(byKey.values()))) {
      if (!segment.mention || !segment.name || !segment.type) continue;
      const key = `${segment.type}:${segment.name.toLocaleLowerCase()}`;
      if (!byKey.has(key)) byKey.set(key, { name: segment.name, assetType: segment.type });
    }
  }
  return Array.from(byKey.values());
});

const assetTypes = [
  { value: "character", label: "角色", placeholder: "生成或上传角色图" },
  { value: "scene", label: "场景", placeholder: "生成或上传场景图" },
  { value: "prop", label: "道具", placeholder: "生成或上传道具图" }
] as const;

const editingAsset = computed(() => (current.value?.assets ?? []).find(asset => asset.id === editingAssetId.value));
const editingAssetType = computed(() => assetTypeMeta(editingAsset.value?.assetType));
const assetSizeConfig = computed(() => {
  const base = config.nodeType("image")?.params?.find(param => param.code === "size")?.config;
  return mergeObjects(asObject(base), asObject(assetModelOverrides.value.size));
});

type AssetSettingOption = { label: string; value: string };

const assetQualityOptions = computed(() => assetSettingOptions("quality", [
  { label: "低画质", value: "low" },
  { label: "标准画质", value: "standard" },
  { label: "高画质", value: "high" }
]));
const assetClarityOptions = computed(() => assetSettingOptions("clarity", ["1K", "2K", "4K"]));
const assetRatioOptions = computed(() => assetSettingOptions("ratio", ["1:1", "1:2", "2:1", "9:16", "16:9", "3:4", "4:3"]));

function assetTypeMeta(type?: string) {
  return assetTypes.find(item => item.value === type) ?? assetTypes[0];
}

function assetsByType(type: string) {
  return (current.value?.assets ?? []).filter(asset => asset.assetType === type);
}

function generationAssetsByType(type: string) {
  return assetGenerationCandidates.value.filter(asset => asset.assetType === type);
}

function assetSettingSection(key: string) {
  const sections = Array.isArray(assetSizeConfig.value.sections)
    ? assetSizeConfig.value.sections.map(asObject)
    : [];
  const base = sections.find(section => section.key === key) ?? {};
  return mergeObjects(base, asObject(assetSizeConfig.value[key]));
}

function assetSettingOptions(key: string, fallback: Array<string | AssetSettingOption>) {
  const configured = assetSettingSection(key).options;
  const options = Array.isArray(configured) && configured.length ? configured : fallback;
  return options.map((option) => {
    if (typeof option === "object" && option !== null) {
      const item = asObject(option);
      const value = String(item.value ?? item.label ?? "");
      return { value, label: String(item.label ?? assetSettingLabel(value)) };
    }
    const value = String(option);
    return { value, label: assetSettingLabel(value) };
  }).filter(option => option.value);
}

function assetSettingLabel(value: string) {
  return ({
    low: "低画质",
    standard: "标准画质",
    high: "高画质",
    hd: "高画质",
    adaptive: "自适应"
  } as Record<string, string>)[value] ?? value;
}

function reconcileAssetGenerationSettings() {
  const entries: Array<["quality" | "clarity" | "ratio", AssetSettingOption[], string]> = [
    ["quality", assetQualityOptions.value, "standard"],
    ["clarity", assetClarityOptions.value, "2K"],
    ["ratio", assetRatioOptions.value, "2:1"]
  ];
  for (const [key, options, fallback] of entries) {
    if (options.some(option => option.value === assetGenerationSettings[key])) continue;
    const configuredDefault = String(assetSettingSection(key).default ?? fallback);
    assetGenerationSettings[key] = options.find(option => option.value === configuredDefault)?.value
      ?? options[0]?.value
      ?? fallback;
  }
}

function toggleAssetGeneration(assetId: string) {
  selectedAssetGenerationIds.value = selectedAssetGenerationIds.value.includes(assetId)
    ? selectedAssetGenerationIds.value.filter(id => id !== assetId)
    : [...selectedAssetGenerationIds.value, assetId];
}

function toggleAllAssetGeneration() {
  selectedAssetGenerationIds.value = allAssetGenerationSelected.value
    ? []
    : assetGenerationCandidates.value.map(asset => asset.id);
}

function openAssetGenerationDialog() {
  if (!assetGenerationCandidates.value.length) {
    ElMessage.info("当前资产均已就绪或正在生成");
    return;
  }
  selectedAssetGenerationIds.value = assetGenerationCandidates.value.map(asset => asset.id);
  assetGenerationDrafts.value = Object.fromEntries(
    assetGenerationCandidates.value.map(asset => [asset.id, asset.description ?? ""])
  );
  assetGenerationDialogOpen.value = true;
}

function closeAssetGenerationDialog() {
  if (batchBusy.value) return;
  assetGenerationDialogOpen.value = false;
}

function missingAssetsByType(type: string) {
  return assetsByType(type).filter(asset => asset.status !== "ready").length;
}

function openAssetEditor(asset: ScriptAsset) {
  editingAssetId.value = asset.id;
  assetNameDraft.value = asset.name;
  assetDescriptionDraft.value = asset.description ?? "";
  void nextTick(resizeAssetDescription);
}

/**
 * 描述现在含档案 + 构图规范 + 视觉风格，动辄七八百字。固定高度会把内容困在
 * 内层滚动条里，读一遍要在两层滚动之间来回切；改成随内容撑开，由弹窗整体滚动。
 */
function resizeAssetDescription() {
  const element = assetDescriptionInput.value;
  if (!element) return;
  element.style.height = "auto";
  element.style.height = `${element.scrollHeight}px`;
}

function closeAssetEditor() {
  if (assetSaving.value) return;
  editingAssetId.value = "";
  assetNameDraft.value = "";
  assetDescriptionDraft.value = "";
}

const assetFooterMessage = computed(() => {
  if (assetBoardReady.value) return "资产已生成，如再次生成将会覆盖之前的图片、场景和道具等资产";
  if (!current.value?.assets.length) return "尚未提取角色、场景和道具，请先从镜头表中提取资产";
  const counts = assetTypes.map(type => missingAssetsByType(type.value));
  return `检测到有 ${counts[0]} 个人物角色和 ${counts[1]} 个场景和 ${counts[2]} 个道具没有设定图，您可以手动上传或AI批量生成`;
});

function isStepComplete(step: number) {
  if (step === 1) {
    const total = current.value?.shotsProgress.total ?? 0;
    return total > 0 && (current.value?.shotsProgress.completed ?? 0) >= total;
  }
  if (step === 2) {
    const total = current.value?.assets.length ?? 0;
    return total > 0 && assetsReady.value >= total;
  }
  return shots.value.length > 0 && finalPromptsReady.value >= shots.value.length;
}

function stepProgressText(step: number) {
  if (step === 1) return `${current.value?.shotsProgress.completed ?? 0} 个镜头已就绪`;
  if (step === 2) {
    const total = current.value?.assets.length ?? 0;
    const remaining = Math.max(0, total - assetsReady.value);
    return `${assetsReady.value}/${total} 已生成${remaining ? `，还差 ${remaining} 个` : ""}`;
  }
  return `${finalPromptsReady.value}/${shots.value.length} 已合成`;
}

function mentionType(value?: string | null): ScriptMentionType {
  if (value === "character" || value === "scene" || value === "prop") return value;
  return "unknown";
}

function mentionClass(value?: string | null) {
  return `mention-${mentionType(value)}`;
}

function shotDescriptionSegments(shot: ScriptShot): ScriptMentionSegment[] {
  // 带上本镜的实体：描述里没写 @ 的同名字样也高亮（CD-78 之前生成的描述一个 @ 都没有）
  return segmentScriptMentions(shot.description, mentionAssets.value, shot.entities ?? []);
}

function openDescriptionEditor(shot: ScriptShot, event: MouseEvent) {
  const target = event.currentTarget as HTMLElement;
  const rect = target.getBoundingClientRect();
  const viewportWidth = window.innerWidth;
  const viewportHeight = window.innerHeight;
  const width = Math.min(720, Math.max(560, rect.width + 80), viewportWidth - 32);
  const estimatedHeight = 248;
  const left = Math.max(16, Math.min(rect.left, viewportWidth - width - 16));
  const below = rect.bottom + 8;
  const top = below + estimatedHeight <= viewportHeight - 16
    ? below
    : Math.max(16, rect.top - estimatedHeight - 8);

  editingDescriptionShotId.value = shot.id;
  descriptionDraft.value = shot.description;
  descriptionOriginal.value = shot.description;
  descriptionEditorPosition.value = { left: `${left}px`, top: `${top}px`, width: `${width}px` };
  void nextTick(() => descriptionEditor.value?.focusEnd());
}

async function saveDescriptionEditor(notify: boolean) {
  const shot = descriptionEditorShot.value;
  if (!shot) return;
  const nextDescription = descriptionDraft.value.trim();
  if (!nextDescription) {
    if (notify) {
      ElMessage.warning("画面描述不能为空");
      descriptionEditor.value?.focusEnd();
      return;
    }
    descriptionDraft.value = descriptionOriginal.value;
  }
  const value = descriptionDraft.value.trim();
  // ⚠️ 只看文字改没改：引用关系由服务端按实体对账给出（模型的描述里大多没有 @），
  //    拿「描述里有哪些 @」去比的话，打开再关上一次编辑器就会触发保存、把这一镜的提示词标成过期。
  const changed = value !== descriptionOriginal.value;
  if (changed) {
    shot.description = value;
    // 新写的 @名字 追加成实体，保存后对账时据此建资产、挂引用（只追加，不删模型给的实体）
    shot.entities = mergeMentionEntities(shot.entities ?? [], value, mentionAssets.value);
  }
  editingDescriptionShotId.value = "";
  descriptionOriginal.value = "";
  if (changed) await saveShots(notify);
}

function copy<T>(value: T): T {
  return JSON.parse(JSON.stringify(value)) as T;
}

function shotDraftFingerprint(value: ScriptShot[]) {
  return JSON.stringify(value.map(shot => ({
    id: shot.id,
    durationSec: Number(shot.durationSec) || 5,
    description: shot.description,
    shotSize: shot.shotSize,
    lighting: shot.lighting,
    dialogue: shot.dialogue,
    soundEffect: shot.soundEffect,
    cameraMove: shot.cameraMove,
    entities: shot.entities ?? []
  })));
}

function publish(next: ScriptDetail) {
  const selectedIds = new Set(selectedShotIds.value);
  const selectedShotNumbers = new Set(
    shots.value.filter(shot => selectedIds.has(shot.id)).map(shot => shot.shotNo)
  );
  const promptSelectedIds = new Set(promptBatchSelectedIds.value);
  const promptSelectedShotNumbers = new Set(
    shots.value.filter(shot => promptSelectedIds.has(shot.id)).map(shot => shot.shotNo)
  );
  detail.value = next;
  shots.value = copy(next.shots ?? []);
  shotsBaseline.value = shotDraftFingerprint(shots.value);
  selectedShotIds.value = shots.value
    .filter(shot => selectedIds.has(shot.id) || selectedShotNumbers.has(shot.shotNo))
    .map(shot => shot.id);
  promptBatchSelectedIds.value = shots.value
    .filter(shot => promptSelectedIds.has(shot.id) || promptSelectedShotNumbers.has(shot.shotNo))
    .map(shot => shot.id);
  if (!activeStep.value || activeStep.value < 1) activeStep.value = next.currentStep || 1;
  emit("updated", next);
}

function isProtectedMedia(url: string) {
  return url.startsWith("/api/");
}

function assetImageSource(asset: ScriptAsset) {
  if (!asset.imageUrl) return undefined;
  if (!isProtectedMedia(asset.imageUrl)) return asset.imageUrl;
  return assetPreviewUrls.value[asset.id];
}

function assetProgressValue(asset: ScriptAsset) {
  const progress = Number(asset.progress ?? 0);
  if (!Number.isFinite(progress)) return 0;
  return Math.min(100, Math.max(0, Math.round(progress)));
}

function assetProgressStyle(asset: ScriptAsset) {
  const progress = assetProgressValue(asset);
  return progress > 0 ? { width: `${progress}%` } : undefined;
}

function assetProgressLabel(asset: ScriptAsset) {
  const progress = assetProgressValue(asset);
  return progress > 0 ? `${progress}%` : "处理中";
}

function revokeAssetPreview(assetId: string) {
  const currentUrl = assetPreviewUrls.value[assetId];
  if (currentUrl?.startsWith("blob:")) URL.revokeObjectURL(currentUrl);
  assetPreviewUrls.value = Object.fromEntries(
    Object.entries(assetPreviewUrls.value).filter(([entryKey]) => entryKey !== assetId)
  );
  assetPreviewSources.value = Object.fromEntries(
    Object.entries(assetPreviewSources.value).filter(([entryKey]) => entryKey !== assetId)
  );
}

async function syncAssetPreviews() {
  if (!import.meta.client) return;
  const assets = current.value?.assets ?? [];
  const activeIds = new Set(assets.map(asset => asset.id));
  Object.keys(assetPreviewUrls.value).forEach((assetId) => {
    if (!activeIds.has(assetId)) revokeAssetPreview(assetId);
  });

  for (const asset of assets) {
    if (!asset.imageUrl || !isProtectedMedia(asset.imageUrl)) continue;
    const existing = assetPreviewUrls.value[asset.id];
    if (existing && assetPreviewSources.value[asset.id] === asset.imageUrl) continue;
    if (existing) revokeAssetPreview(asset.id);
    try {
      const response = await media.fetchMedia(asset.imageUrl);
      if (!response.ok) continue;
      assetPreviewUrls.value[asset.id] = URL.createObjectURL(await response.blob());
      assetPreviewSources.value[asset.id] = asset.imageUrl;
    } catch {
      // Asset metadata remains usable when a protected preview temporarily fails.
    }
  }
}

async function load(silent = false) {
  if (!silent) loading.value = true;
  try {
    const next = await scripts.loadWorkbench(props.scriptId, {
      name: detail.value?.name,
      references: detail.value?.input.references
    });
    publish(next);
    activeStep.value = Math.min(3, Math.max(1, next.currentStep || 1));
    if (next.sourceType === "manual" && !shots.value.length) addShot();
    if (!selectedShotIds.value.length) selectedShotIds.value = next.shots.map(shot => shot.id);
  } catch (cause) {
    ElMessage.error(cause instanceof Error ? cause.message : "脚本工作台加载失败");
  } finally {
    if (!silent) loading.value = false;
  }
}

async function loadModels() {
  try {
    await config.load();
    const [nextTextModels, nextImageModels, nextAssetModels, nextVideoModels] = await Promise.all([
      config.models("text", "storyboard_gen"),
      config.models("image", "text2img"),
      config.models("image", "asset_gen"),
      config.models("video", "text2video")
    ]);
    textModels.value = nextTextModels;
    imageModels.value = nextImageModels;
    assetImageModels.value = nextAssetModels;
    videoModels.value = nextVideoModels;
    selectedPromptTextModel.value ||= textModels.value[0]?.code ?? "";
    selectedImageModel.value ||= imageModels.value[0]?.code ?? "";
    selectedAssetImageModel.value ||= assetImageModels.value[0]?.code ?? "";
    selectedVideoModel.value ||= videoModels.value[0]?.code ?? "";
  } catch {
    // The workbench remains editable when the model catalog is temporarily unavailable.
  }
}

async function syncAssetModelOptions() {
  const request = ++assetModelRequest;
  if (!selectedAssetImageModel.value) {
    assetModelOverrides.value = {};
    reconcileAssetGenerationSettings();
    return;
  }
  try {
    const overrides = await config.overrides(selectedAssetImageModel.value);
    if (request !== assetModelRequest) return;
    assetModelOverrides.value = overrides;
  } catch {
    if (request !== assetModelRequest) return;
    assetModelOverrides.value = {};
  }
  reconcileAssetGenerationSettings();
}

async function setStep(step: number) {
  if (!current.value || step === activeStep.value) return;
  if (step > 1 && !shots.value.length) {
    ElMessage.warning("请先至少保存一个镜头");
    return;
  }
  if (step > 2 && !assetsComplete.value) {
    ElMessage.warning("请先准备好当前脚本引用的全部资产");
    return;
  }
  if ([1, 3].includes(activeStep.value) && shotsDirty.value && !(await saveShots(false))) return;
  try {
    if (step > 2 && current.value?.assets.length && !assetGroupExists.value) {
      // 把资产组放到画布上是顺手的事：没放上去不该挡住进入下一步
      await createAssetGroup(false).catch((cause: unknown) => {
        ElMessage.warning(`资产组没能放到画布上：${cause instanceof Error ? cause.message : "未知原因"}（不影响继续）`);
      });
    }
    const next = await scripts.patchStep(current.value!, step);
    activeStep.value = step;
    publish(next);
    if (step === 2 && needsReconcile(next)) await reconcileAssets();
  } catch (cause) {
    ElMessage.error(cause instanceof Error ? cause.message : "步骤切换失败");
  }
}

/**
 * 要不要对账：还没有资产，或镜头里有实体还没对应的资产（用户在描述里新写了 @某某）。
 * ⚠️ 不是每次进第 2 步都对：对账会把还没出图的资产描述改回模板（服务端规则），用户刚改的外观就没了。
 */
function needsReconcile(next: ScriptDetail) {
  if (!next.assets.length) return next.shots.some(shot => (shot.entities ?? []).length);
  const names = new Set(next.assets.map(asset => asset.name));
  return next.shots.some(shot => (shot.entities ?? []).some(entity => !names.has(entity.name)));
}

function newShot(): ScriptShot {
  const nextNo = shots.value.length + 1;
  return {
    id: `draft-${Date.now().toString(36)}`,
    shotNo: nextNo,
    durationSec: 5,
    description: "",
    shotSize: "中景",
    lighting: "",
    dialogue: "",
    soundEffect: "",
    cameraMove: "",
    imagePrompt: "",
    videoPrompt: "",
    imagePromptStatus: "pending",
    videoPromptStatus: "pending",
    imagePromptSource: null,
    videoPromptSource: null,
    contentVersion: 1,
    sort: shots.value.length,
    assetIds: []
  };
}

function addShot() {
  shots.value.push(newShot());
  selectedShotIds.value.push(shots.value.at(-1)!.id);
}

function removeShot(index: number) {
  if (shots.value.length <= 1) {
    ElMessage.warning("脚本至少保留一个镜头");
    return;
  }
  const removed = shots.value.splice(index, 1)[0];
  selectedShotIds.value = selectedShotIds.value.filter(id => id !== removed?.id);
  renumberShots();
}

function moveShot(index: number, offset: number) {
  const next = index + offset;
  if (next < 0 || next >= shots.value.length) return;
  const [item] = shots.value.splice(index, 1);
  if (item) shots.value.splice(next, 0, item);
  renumberShots();
}

function duplicateShot(index: number) {
  const source = shots.value[index];
  if (!source) return;
  const cloneShot = copy(source);
  cloneShot.id = `draft-${Date.now().toString(36)}`;
  cloneShot.imagePromptStatus = "pending";
  cloneShot.videoPromptStatus = "pending";
  cloneShot.imagePrompt = "";
  cloneShot.videoPrompt = "";
  shots.value.splice(index + 1, 0, cloneShot);
  selectedShotIds.value.push(cloneShot.id);
  renumberShots();
}

function renumberShots() {
  shots.value.forEach((shot, index) => {
    shot.shotNo = index + 1;
    shot.sort = index;
  });
}

async function saveShots(notify = true) {
  if (!current.value || saving.value || !shots.value.length) return false;
  saving.value = true;
  try {
    // ⚠️ 新后端没有整表保存：拆成逐镜的删 / 建 / 改 / 排序（见 useScriptApi.syncShots）
    const { plan } = await scripts.syncShots(props.scriptId, current.value.shots, shots.value);
    const entitiesChanged = plan.updates.some(update => "entities" in update.changes)
      || plan.creates.some(create => "entities" in create.rest);
    await load(true);
    // 新写的 @名字 要对一次账，资产与引用才跟得上（引用只由对账重绑）
    if (entitiesChanged) await reconcileAssets();
    if (notify) ElMessage.success("镜头表已保存");
    return true;
  } catch (cause) {
    // ⚠️ 逐镜保存没有整表事务：前面成功的几步已经落库。刷新成服务器上的样子，并把这件事说清楚
    const reason = cause instanceof Error ? cause.message : "未知原因";
    await load(true);
    ElMessage.error(`镜头表没有全部保存：${reason}。已刷新为服务器上的最新内容，未保存的修改请重新填写`);
    return false;
  } finally {
    saving.value = false;
  }
}

async function advance() {
  if (activeStep.value === 1 && shotsDirty.value && !(await saveShots())) return;
  await setStep(activeStep.value + 1);
}

async function reconcileAssets() {
  try {
    const result = await scripts.reconcileAssets(props.scriptId);
    // 对账会建资产、改引用：整份重取（引用关系在详情里，资产在资产列表里）
    await load(true);
    // 模型没给档案的实体退回了模板描述 —— 不是错误，但用户该知道该给哪几个补一句外观
    if (result.unmatched?.length) {
      ElMessage.info(`这些资产分镜里没有外观档案，已按模板写了描述，建议补一句外观：${result.unmatched.join("、")}`);
    }
  } catch (cause) {
    ElMessage.error(cause instanceof Error ? cause.message : "资产关系整理失败");
  }
}

function syncPromptDrafts(shot: ScriptShot) {
  imagePromptDraft.value = String(shot.imagePrompt ?? "");
  videoPromptDraft.value = String(shot.videoPrompt ?? "");
}

function openPrompt(shot: ScriptShot) {
  selectedPromptShot.value = shot;
  syncPromptDrafts(shot);
}

function closePrompt() {
  if (promptSaving.value || (selectedPromptShot.value && composing.value.includes(selectedPromptShot.value.id))) return;
  selectedPromptShot.value = undefined;
}

async function composePrompt(
  shot: ScriptShot,
  types: ScriptPromptType[] = ["image", "video"],
  mode = promptComposeMode.value,
  refresh = true,
  force = true
) {
  if (shotsDirty.value) {
    const shotNo = shot.shotNo;
    if (!(await saveShots(false))) return false;
    const latest = shots.value.find(item => item.shotNo === shotNo);
    if (!latest) return false;
    shot = latest;
  }
  const model = selectedPromptTextModel.value || textModels.value[0]?.code;
  if (mode === "smart" && !model) {
    ElMessage.warning("暂无可用文本模型");
    return false;
  }
  if (shot.id.startsWith("draft-")) {
    ElMessage.warning("请先保存新增镜头");
    return false;
  }
  if (composing.value.includes(shot.id)) return false;
  composing.value.push(shot.id);
  try {
    // ⚠️ 新后端没有逐镜的合成路径，是同一个批量端点带 shotIds；`template` 0 积分、`ai` 收费。
    //    单镜是用户显式点「合成 / 重新合成」：`force` 连已就绪的也重算（批量不 force，见 composeAll）。
    const result = await scripts.composePrompts(props.scriptId, {
      source: mode === "template" ? "template" : "ai",
      ...(mode === "template" ? {} : { modelCode: model }),
      shotIds: [shot.id],
      kinds: types,
      force
    });
    // 请求成功 ≠ 每一条都成功：失败的那几条各自退款，原因在逐项结果里
    const failed = result.items.filter(item => item.status === "failed");
    if (refresh) await load(true);
    if (failed.length) {
      const kinds = failed.map(item => (item.kind === "image" ? "分镜图" : "视频")).join("、");
      ElMessage.error(`第 ${shot.shotNo} 镜的${kinds}提示词合成失败：${failed[0]?.reason || "未知原因"}（已退款）`);
      return false;
    }
    if (selectedPromptShot.value?.id === shot.id) {
      const latest = shots.value.find(item => item.id === shot.id);
      if (latest) {
        selectedPromptShot.value = latest;
        syncPromptDrafts(latest);
      }
    }
    return true;
  } catch (cause) {
    ElMessage.error(cause instanceof Error ? cause.message : "提示词合成失败");
    return false;
  } finally {
    composing.value = composing.value.filter(id => id !== shot.id);
  }
}

async function openPromptBatchDialog() {
  if (shotsDirty.value && !(await saveShots(false))) return;
  if (!promptBatchShots.value.length) {
    ElMessage.warning("请先保存至少一个镜头");
    return;
  }
  const candidateIds = new Set(promptBatchShots.value.map(shot => shot.id));
  const retained = promptBatchSelectedIds.value.filter(id => candidateIds.has(id));
  promptBatchSelectedIds.value = retained.length ? retained : promptBatchShots.value.map(shot => shot.id);
  promptBatchDialogOpen.value = true;
}

function closePromptBatchDialog() {
  if (batchBusy.value) return;
  promptBatchDialogOpen.value = false;
}

function togglePromptBatchShot(shotId: string) {
  promptBatchSelectedIds.value = promptBatchSelectedIds.value.includes(shotId)
    ? promptBatchSelectedIds.value.filter(id => id !== shotId)
    : [...promptBatchSelectedIds.value, shotId];
}

function toggleAllPromptBatchShots() {
  promptBatchSelectedIds.value = allPromptBatchSelected.value
    ? []
    : promptBatchShots.value.map(shot => shot.id);
}

function schedulePromptQuote() {
  clearTimeout(promptQuoteTimer);
  promptQuoteRequest += 1;
  promptBatchQuote.value = undefined;
  if (!promptBatchDialogOpen.value || !promptBatchSelectedIds.value.length) return;
  if (promptComposeMode.value === "smart" && !selectedPromptTextModel.value) return;
  promptQuoteTimer = setTimeout(() => void refreshPromptQuote(), 250);
}

async function refreshPromptQuote() {
  const request = ++promptQuoteRequest;
  const template = promptComposeMode.value === "template";
  try {
    const result = await scripts.composePrompts(props.scriptId, {
      source: template ? "template" : "ai",
      ...(template ? {} : { modelCode: selectedPromptTextModel.value }),
      shotIds: promptBatchSelectedIds.value,
      force: false,
      dryRun: true
    });
    if (request === promptQuoteRequest) promptBatchQuote.value = result.quote;
  } catch {
    if (request === promptQuoteRequest) promptBatchQuote.value = undefined;
  }
}

async function composeAll(type?: ScriptPromptType) {
  const selectedIds = new Set(promptBatchSelectedIds.value);
  const targets = promptBatchShots.value.filter(shot => selectedIds.has(shot.id));
  if (!targets.length) {
    ElMessage.warning("请至少选择一个镜头");
    return;
  }
  if (batchBusy.value) return;
  // 先把脏数据存一次再并发：composePrompt 内部也会存，但并发触发多次 saveShots
  // 会互相撞乐观锁 —— 每次保存都把 script.version +1，后到的必然 40902
  if (shotsDirty.value && !(await saveShots(false))) return;
  batchBusy.value = true;
  let completed = 0;
  promptBatchProgress.value = { done: 0, total: targets.length };
  const types: ScriptPromptType[] = type ? [type] : ["image", "video"];
  try {
    // 共享队列 + 固定数量的消费者：某一镜特别慢时不会拖住整批，
    // 空闲的消费者继续取下一个（比按批切分的 Promise.all 更均衡）
    const queue = [...targets];
    // 每个请求在服务端会并发合成这一镜的几种词：按种类数折算，合计不超过上游的并发上限
    const concurrency = Math.max(1, Math.floor(PROMPT_COMPOSE_CONCURRENCY / types.length));
    const workers = Array.from(
      { length: Math.min(concurrency, queue.length) },
      async () => {
        for (let shot = queue.shift(); shot; shot = queue.shift()) {
          // ⚠️ 批量不 force：已就绪且上游没变的跳过 —— 重算要再花一次钱，而报价也是按跳过之后算的
          if (await composePrompt(shot, types, promptComposeMode.value, false, false)) completed += 1;
          promptBatchProgress.value = { done: (promptBatchProgress.value?.done ?? 0) + 1, total: targets.length };
        }
      }
    );
    await Promise.all(workers);
    await load(true);
    if (completed === targets.length) {
      promptBatchDialogOpen.value = false;
      ElMessage.success(`已合成 ${completed} 个镜头的最终提示词`);
    }
    else if (completed) ElMessage.warning(`已合成 ${completed}/${targets.length} 个镜头，请重试失败项`);
  } finally {
    batchBusy.value = false;
    promptBatchProgress.value = undefined;
  }
}

async function savePrompt() {
  const shot = selectedPromptShot.value;
  if (!current.value || !shot || promptSaving.value) return;
  promptSaving.value = true;
  try {
    const latest = shots.value.find(item => item.id === shot.id);
    if (!latest) return;
    // ⚠️ 新后端按种类各存一条（`PUT .../prompts/{kind}`），带镜头版本号；每存一条版本 +1，第二条要用新版本
    let working = latest;
    if (imagePromptDraft.value.trim() !== String(latest.imagePrompt ?? "").trim()) {
      working = await scripts.savePrompt(props.scriptId, working, "image", imagePromptDraft.value);
    }
    if (videoPromptDraft.value.trim() !== String(latest.videoPrompt ?? "").trim()) {
      working = await scripts.savePrompt(props.scriptId, working, "video", videoPromptDraft.value);
    }
    if (working === latest) {
      ElMessage.info("提示词没有改动");
      return;
    }
    await load(true);
    selectedPromptShot.value = shots.value.find(item => item.id === shot.id);
    if (selectedPromptShot.value) syncPromptDrafts(selectedPromptShot.value);
    ElMessage.success("提示词已保存");
  } catch (cause) {
    ElMessage.error(cause instanceof Error ? cause.message : "提示词保存失败");
  } finally {
    promptSaving.value = false;
  }
}

function openBatchDialog(type: "storyboard" | "video") {
  const eligible = type === "storyboard" ? storyboardEligible.value : videoEligible.value;
  if (!eligible.length) {
    ElMessage.warning(type === "storyboard" ? "请先合成分镜图提示词" : "请先合成视频提示词");
    return;
  }
  const eligibleIds = new Set(eligible.map(shot => shot.id));
  const retained = selectedShotIds.value.filter(id => eligibleIds.has(id));
  selectedShotIds.value = retained.length ? retained : eligible.map(shot => shot.id);
  for (const shot of eligible) videoDurations[shot.id] ||= shot.durationSec;
  batchDialog.value = type;
}

function toggleBatchShot(shotId: string) {
  selectedShotIds.value = selectedShotIds.value.includes(shotId)
    ? selectedShotIds.value.filter(id => id !== shotId)
    : [...selectedShotIds.value, shotId];
}

async function createStoryboardGroup() {
  if (!selectedShotIds.value.length) {
    ElMessage.warning("请至少选择一个镜头");
    return;
  }
  if (!selectedImageModel.value) {
    ElMessage.warning("暂无可用图片模型");
    return;
  }
  batchBusy.value = true;
  try {
    // ⚠️ 新后端只有一个建组端点；分镜图组**建组即提交**。比例与画质写进每个子项（清晰度从没发给过上游，不发）
    const resp = await scripts.createGroup(props.scriptId, {
      groupType: "storyboard_image",
      canvasNodeId: newCanvasNodeId(),
      name: "分镜图组",
      shotIds: selectedShotIds.value,
      modelCode: selectedImageModel.value,
      params: liftLegacySize({ size: { ratio: storyboardSettings.ratio, quality: storyboardSettings.quality } })
    });
    const group = normalizeGroupDetail(resp, { shots: shots.value, assets: current.value?.assets });
    const leftOut = shotsLeftOut(selectedShotIds.value, shots.value, group);
    emit("group-created", { ...group, modelCode: selectedImageModel.value });
    batchDialog.value = undefined;
    await load();
    if (leftOut.length) ElMessage.warning(`分镜图组已创建并提交；第 ${leftOut.join("、")} 镜没有加入：引用的资产还没有设定图，或提示词还没就绪`);
    else ElMessage.success("分镜图组已创建并提交");
  } catch (cause) {
    ElMessage.error(cause instanceof Error ? cause.message : "分镜图组创建失败");
  } finally {
    batchBusy.value = false;
  }
}

async function createAssetGroup(notify = true) {
  if (!current.value?.assets.length) {
    if (notify) ElMessage.info("当前脚本没有需要成组的资产");
    return undefined;
  }
  if (!assetsComplete.value) {
    if (notify) ElMessage.warning("全部资产就绪后才能创建资产组");
    return undefined;
  }
  batchBusy.value = true;
  try {
    const resp = await scripts.createGroup(props.scriptId, {
      groupType: "asset",
      canvasNodeId: newCanvasNodeId(),
      name: "资产组"
    });
    const group = normalizeGroupDetail(resp, { shots: shots.value, assets: current.value?.assets });
    emit("group-created", group);
    await load(true);
    if (notify) ElMessage.success("资产组已添加到画布");
    return group;
  } catch (cause) {
    if (notify) ElMessage.error(cause instanceof Error ? cause.message : "资产组创建失败");
    throw cause;
  } finally {
    batchBusy.value = false;
  }
}

async function createVideoGroup() {
  if (!selectedShotIds.value.length) {
    ElMessage.warning("请至少选择一个镜头");
    return;
  }
  if (!selectedVideoModel.value) {
    ElMessage.warning("暂无可用视频模型");
    return;
  }
  batchBusy.value = true;
  try {
    // 视频组**先落 draft**（一条几十积分，先让用户看清输入再花钱）；模型记在画布节点上，执行时带上
    const resp = await scripts.createGroup(props.scriptId, {
      groupType: "video_generator",
      canvasNodeId: newCanvasNodeId(),
      name: "视频生成器组",
      shotIds: selectedShotIds.value,
      params: { aspect_ratio: videoSettings.ratio, resolution: videoSettings.clarity.toLowerCase(), audio: videoSettings.audio },
      durations: Object.fromEntries(selectedShotIds.value.map(id => [id, videoDurations[id] ?? 5]))
    });
    const group = normalizeGroupDetail(resp, { shots: shots.value, assets: current.value?.assets });
    const leftOut = shotsLeftOut(selectedShotIds.value, shots.value, group);
    emit("group-created", { ...group, modelCode: selectedVideoModel.value });
    batchDialog.value = undefined;
    await load();
    if (leftOut.length) ElMessage.warning(`视频生成器组已创建为草稿；第 ${leftOut.join("、")} 镜没有加入：提示词还没就绪或输入规划不出来`);
    else ElMessage.success("视频生成器组已创建为草稿");
  } catch (cause) {
    ElMessage.error(cause instanceof Error ? cause.message : "视频组创建失败");
  } finally {
    batchBusy.value = false;
  }
}

async function uploadAsset(event: Event, asset: ScriptAsset) {
  const file = (event.target as HTMLInputElement).files?.[0];
  (event.target as HTMLInputElement).value = "";
  if (!file || uploadingAssetId.value) return;
  uploadingAssetId.value = asset.id;
  try {
    // ⚠️ 新后端没有「资产上传」这一条：先经媒体域上传（按真实文件头识别类型），再绑到资产上
    await scripts.uploadAssetImage({
      projectId: current.value?.projectId ?? "",
      nodeId: props.nodeId || current.value?.nodeId || "",
      asset,
      file
    });
    await load(true);
    ElMessage.success("资产图片已上传");
  } catch (cause) {
    ElMessage.error(cause instanceof Error ? cause.message : "资产上传失败");
  } finally {
    uploadingAssetId.value = "";
  }
}

async function saveAsset() {
  const asset = editingAsset.value;
  const name = assetNameDraft.value.trim();
  if (!asset || assetSaving.value) return;
  if (!name) {
    ElMessage.warning(`${editingAssetType.value.label}名称不能为空`);
    return;
  }
  assetSaving.value = true;
  try {
    const saved = await scripts.saveAsset(asset, { name, description: assetDescriptionDraft.value.trim() });
    await load(true);
    // 质量提示随响应回来、不拦截保存（占位名、描述里写了别人的名字……）：说出来，让用户自己判断
    const lint = saved.lints?.find(item => item.message)?.message;
    if (lint) ElMessage.warning(`${editingAssetType.value.label}信息已保存，有一处建议留意：${lint}`);
    else ElMessage.success(`${editingAssetType.value.label}信息已保存`);
    editingAssetId.value = "";
    assetNameDraft.value = "";
    assetDescriptionDraft.value = "";
  } catch (cause) {
    ElMessage.error(cause instanceof Error ? cause.message : "资产信息保存失败");
  } finally {
    assetSaving.value = false;
  }
}

async function saveAssetGenerationDrafts(assetIds: string[]) {
  let changed = false;
  for (const assetId of assetIds) {
    const asset = current.value?.assets.find(item => item.id === assetId);
    if (!asset) continue;
    const description = (assetGenerationDrafts.value[assetId] ?? "").trim();
    if (description === (asset.description ?? "").trim()) continue;
    await scripts.saveAsset(asset, { description });
    changed = true;
  }
  if (changed) await load(true);
}

async function generateAssets() {
  const eligibleIds = new Set(assetGenerationCandidates.value.map(asset => asset.id));
  const assetIds = selectedAssetGenerationIds.value.filter(assetId => eligibleIds.has(assetId));
  if (!assetIds.length) {
    ElMessage.warning("请至少选择一个需要生成的资产");
    return;
  }
  if (!selectedAssetImageModel.value) {
    ElMessage.warning("暂无支持资产生成的图片模型");
    return;
  }
  batchBusy.value = true;
  try {
    await saveAssetGenerationDrafts(assetIds);
    const items = await scripts.generateAssets(props.scriptId, {
      modelCode: selectedAssetImageModel.value,
      assetIds,
      // 只有画幅与画质会发出去（服务端白名单）；清晰度在老系统里也从没发给过上游
      params: liftLegacySize({ size: { ...assetGenerationSettings } })
    });
    assetGenerationDialogOpen.value = false;
    selectedAssetGenerationIds.value = [];
    await load(true);
    // 逐项结果：允许部分成功（单项失败不影响别的项）
    const submitted = items.filter(item => item.generationId && !item.skipped).length;
    const skipped = items.find(item => item.skipped && item.reason);
    if (submitted === assetIds.length) {
      ElMessage.success(`已提交 ${submitted} 个资产生成任务`);
    } else {
      ElMessage.warning(`已提交 ${submitted}/${assetIds.length} 个资产生成任务${skipped ? `；「${skipped.name}」：${skipped.reason}` : "，请稍后重试未提交的项"}`);
    }
  } catch (cause) {
    await load(true);
    ElMessage.error(cause instanceof Error ? cause.message : "资产批量生成失败");
  } finally {
    batchBusy.value = false;
    scheduleAssetRefresh();
  }
}

async function refreshGeneratingAssets() {
  assetRefreshTimer = undefined;
  if (!assetsGenerating.value || batchBusy.value) return;
  await load(true);
  scheduleAssetRefresh();
}

function scheduleAssetRefresh() {
  clearTimeout(assetRefreshTimer);
  assetRefreshTimer = undefined;
  if (!assetsGenerating.value || batchBusy.value) return;
  assetRefreshTimer = setTimeout(() => void refreshGeneratingAssets(), 1800);
}

function promptStatusLabel(status: string) {
  return ({ ready: "已生成", composing: "合成中", stale: "需更新", failed: "生成失败" } as Record<string, string>)[status] ?? "待生成";
}

function finalPromptStatus(shot: ScriptShot) {
  const statuses = [shot.imagePromptStatus, shot.videoPromptStatus];
  if (statuses.includes("composing")) return "composing";
  if (statuses.includes("failed")) return "failed";
  if (statuses.includes("stale")) return "stale";
  if (statuses.every(status => status === "ready")) return "ready";
  return "pending";
}

function shotPromptLabel(shot: ScriptShot) {
  return ({
    ready: "查看提示词",
    composing: "合成中",
    stale: "需更新",
    failed: "生成失败",
    pending: "待生成提示词"
  } as Record<string, string>)[finalPromptStatus(shot)];
}

function promptSourceLabel(source?: string | null) {
  return ({ ai: "智能合成", template: "自动拼接", manual: "人工修改" } as Record<string, string>)[source ?? ""] ?? "尚未合成";
}

function openShotPrompt(shot: ScriptShot) {
  openPrompt(shot);
}

onMounted(async () => {
  await Promise.all([load(), loadModels()]);
  if (props.initialAction) {
    activeStep.value = 3;
    await nextTick();
    openBatchDialog(props.initialAction);
  }
});

watch(
  () => current.value?.assets.map(asset => `${asset.id}:${asset.version}:${asset.imageUrl ?? ""}`).join("|") ?? "",
  () => void syncAssetPreviews()
);

watch(
  () => [
    current.value?.assets.map(asset => `${asset.id}:${asset.status}:${asset.version}:${asset.imageUrl ?? ""}`).join("|") ?? "",
    batchBusy.value
  ],
  scheduleAssetRefresh,
  { immediate: true }
);

watch(selectedAssetImageModel, () => void syncAssetModelOptions());

watch(
  () => JSON.stringify([promptBatchDialogOpen.value, promptBatchSelectedIds.value, promptComposeMode.value, selectedPromptTextModel.value]),
  schedulePromptQuote
);


onBeforeUnmount(() => {
  clearTimeout(assetRefreshTimer);
  clearTimeout(promptQuoteTimer);
  promptQuoteRequest += 1;
  assetModelRequest += 1;
  Object.keys(assetPreviewUrls.value).forEach(revokeAssetPreview);
});
</script>

<template>
  <div class="script-studio-overlay" role="dialog" aria-modal="true" aria-label="脚本工作台">
    <section class="script-studio-shell">
      <header class="script-studio-header">
        <div class="script-header-spacer" aria-hidden="true" />
        <div class="script-step-bar">
          <button v-for="step in [1, 2, 3]" :key="step" type="button" :class="{ active: activeStep === step, complete: isStepComplete(step) }" @click="setStep(step)"><span>{{ step }}</span><strong>{{ ["确认镜头", "准备资产", "合成提示词"][step - 1] }}</strong><small>{{ stepProgressText(step) }}</small></button>
        </div>
        <div class="script-header-meta"><span>{{ completedStepCount }}/3 完成后可批量生视频</span><button class="script-studio-close" type="button" title="关闭" @click="emit('close')"><X :size="20" /></button></div>
      </header>

      <div v-if="loading" class="script-studio-loading"><LoaderCircle :size="28" class="spin" /><strong>正在恢复脚本工作台</strong></div>
      <main v-else class="script-studio-body">
        <section v-if="activeStep === 1 || activeStep === 3" class="script-step-content shots-step" :class="{ 'prompts-step': activeStep === 3 }">
          <div class="shot-table-wrap" :class="{ 'prompt-stage-table': activeStep === 3 }">
            <table class="shot-table">
              <thead><tr><th class="shot-number-column">镜号</th><th class="shot-duration-column">时长</th><th class="shot-description">画面描述</th><th class="shot-size-column">景别</th><th class="shot-lighting-column">光影氛围</th><th class="shot-dialogue-column">对白 · 旁白</th><th class="shot-sound-column">音效</th><th class="shot-camera-column">运镜</th><th class="shot-prompt-column" :class="{ 'is-active': activeStep === 3 }">最终提示词</th><th class="shot-action">操作</th></tr></thead>
              <tbody>
                <tr v-for="(shot, index) in filteredShots" :key="shot.id">
                  <td class="shot-number"><span>{{ shot.shotNo }}</span></td>
                  <td><input v-model.number="shot.durationSec" class="shot-duration" type="number" min="1" max="60"><small>s</small></td>
                  <td class="shot-description" :class="{ 'is-editing': editingDescriptionShotId === shot.id }">
                    <button class="shot-description-display" type="button" title="编辑画面描述" @click="openDescriptionEditor(shot, $event)">
                      <template v-if="shot.description">
                        <template v-for="(segment, segmentIndex) in shotDescriptionSegments(shot)" :key="`${shot.id}-${segmentIndex}`">
                          <mark v-if="segment.mention" class="script-mention" :class="mentionClass(segment.type)">{{ segment.text }}</mark>
                          <span v-else>{{ segment.text }}</span>
                        </template>
                      </template>
                      <span v-else class="shot-description-empty">点击填写画面描述</span>
                    </button>
                  </td>
                  <td><select v-model="shot.shotSize"><option value="远景">远景</option><option value="全景">全景</option><option value="中景">中景</option><option value="近景">近景</option><option value="特写">特写</option></select></td>
                  <td><textarea v-model="shot.lighting" rows="3" placeholder="光线、色调" /></td>
                  <td><textarea v-model="shot.dialogue" rows="3" placeholder="对白或旁白" /></td>
                  <td><textarea v-model="shot.soundEffect" rows="3" placeholder="现场音效" /></td>
                  <td><textarea v-model="shot.cameraMove" rows="3" placeholder="推拉摇移" /></td>
                  <td class="shot-final-prompt"><button type="button" :class="`status-${finalPromptStatus(shot)}`" @click="openShotPrompt(shot)"><LoaderCircle v-if="composing.includes(shot.id)" :size="13" class="spin" /><span v-else>{{ shotPromptLabel(shot) }}</span></button></td>
                  <td class="shot-action">
                    <details>
                      <summary title="镜头操作"><MoreHorizontal :size="17" /></summary>
                      <div class="shot-action-menu"><button type="button" :disabled="index === 0" @click="moveShot(index, -1)"><ChevronLeft :size="14" />上移</button><button type="button" :disabled="index === shots.length - 1" @click="moveShot(index, 1)"><ChevronRight :size="14" />下移</button><button type="button" @click="duplicateShot(index)"><Copy :size="14" />复制</button><button type="button" @click="removeShot(index)"><Trash2 :size="14" />删除</button></div>
                    </details>
                  </td>
                </tr>
              </tbody>
            </table>
            <div v-if="!shots.length" class="script-empty-table"><FileText :size="28" /><strong>还没有镜头</strong><button type="button" @click="addShot">添加第一个镜头</button></div>
          </div>
        </section>

        <section v-else-if="activeStep === 2" class="script-step-content assets-step">
          <div class="asset-style-banner">
            <span>全局风格</span>
            <p>{{ current?.globalStyle || "尚未设定，进入提示词合成时自动补全" }}</p>
          </div>
          <div class="script-asset-sections">
            <section v-for="type in assetTypes" :key="type.value" class="script-asset-section">
              <h3>{{ type.label }}</h3>
              <div class="script-asset-grid">
                <article
                  v-for="asset in assetsByType(type.value)"
                  :key="asset.id"
                  class="script-asset-item"
                  role="button"
                  tabindex="0"
                  :aria-label="`编辑${type.label} ${asset.name}`"
                  @click="openAssetEditor(asset)"
                  @keydown.enter.prevent="openAssetEditor(asset)"
                  @keydown.space.prevent="openAssetEditor(asset)"
                >
                  <div
                    class="script-asset-card"
                    :class="{ generating: asset.status === 'generating', failed: asset.status === 'failed' }"
                    :aria-busy="asset.status === 'generating'"
                  >
                    <img
                      v-if="assetImageSource(asset)"
                      :class="{ 'is-generating': asset.status === 'generating' }"
                      :src="assetImageSource(asset)"
                      :alt="asset.name"
                    >
                    <div v-if="asset.status === 'generating'" class="script-asset-generating" aria-live="polite">
                      <span class="script-asset-generating-icon"><LoaderCircle :size="19" class="spin" /></span>
                      <strong>正在生成设定图</strong>
                      <small>AI 正在绘制{{ type.label }}资产</small>
                      <div
                        class="script-asset-progress"
                        :class="{ indeterminate: assetProgressValue(asset) === 0 }"
                        role="progressbar"
                        aria-valuemin="0"
                        aria-valuemax="100"
                        :aria-valuenow="assetProgressValue(asset) || undefined"
                        :aria-valuetext="assetProgressLabel(asset)"
                      >
                        <span class="script-asset-progress-track"><span class="script-asset-progress-fill" :style="assetProgressStyle(asset)" /></span>
                        <output>{{ assetProgressLabel(asset) }}</output>
                      </div>
                    </div>
                    <div v-else-if="!assetImageSource(asset)" class="script-asset-placeholder">
                      <span>{{ asset.status === "failed" ? "生成失败，可重新生成或上传" : type.placeholder }}</span>
                    </div>
                    <label
                      class="script-asset-upload"
                      :class="{ disabled: asset.status === 'generating' }"
                      :title="asset.status === 'generating' ? '资产生成中' : `上传或替换${type.label}图`"
                      @click.stop
                    >
                      <MoreHorizontal :size="18" />
                      <input type="file" accept="image/*" :disabled="uploadingAssetId === asset.id || asset.status === 'generating'" @change="uploadAsset($event, asset)">
                    </label>
                  </div>
                  <div class="script-asset-meta">
                    <strong>{{ asset.name }}</strong>
                    <p><span>名称：{{ asset.name }}</span><span>描述：{{ asset.description || "暂无描述" }}</span></p>
                  </div>
                </article>
                <button class="script-asset-add" type="button" :title="`从镜头重新提取并补充${type.label}`" :disabled="batchBusy" @click="reconcileAssets">
                  <Plus :size="26" />
                  <span>新增</span>
                </button>
              </div>
            </section>
          </div>
        </section>

      </main>

      <footer v-if="activeStep === 2" class="script-studio-footer asset-studio-footer">
        <div class="asset-footer-notice" :class="{ complete: assetBoardReady }">
          <CircleCheck v-if="assetBoardReady" :size="17" />
          <CircleAlert v-else :size="17" />
          <span>{{ assetFooterMessage }}</span>
        </div>
        <div class="script-footer-actions">
          <button v-if="!current?.assets.length" type="button" class="footer-primary" :disabled="batchBusy" @click="reconcileAssets">提取镜头资产</button>
          <button v-else-if="!assetBoardReady" type="button" class="footer-primary" :disabled="batchBusy || !assetGenerationCandidates.length" @click="openAssetGenerationDialog">
            <LoaderCircle v-if="batchBusy || (assetsGenerating && !assetGenerationCandidates.length)" :size="15" class="spin" />
            {{ assetsGenerating && !assetGenerationCandidates.length ? "资产生成中" : "一键生成所有资产" }}
          </button>
          <button v-else type="button" class="footer-primary" :disabled="saving" @click="advance">下一步：合成提示词<ChevronRight :size="15" /></button>
        </div>
      </footer>
      <footer v-else class="script-studio-footer">
        <div>
          <button type="button" class="footer-add-shot" @click="addShot"><Plus :size="18" />添加镜头</button>
        </div>
        <div class="script-footer-actions">
          <button v-if="activeStep < 3" type="button" class="footer-primary" :disabled="saving || !shots.length" @click="advance">下一步：{{ ["准备资产", "合成提示词"][activeStep - 1] }}<ChevronRight :size="15" /></button>
          <template v-else>
            <button type="button" :disabled="saving || !shotsDirty" @click="saveShots()"><LoaderCircle v-if="saving" :size="15" class="spin" /><Save v-else :size="15" />保存镜头表</button>
            <button type="button" class="footer-primary" :disabled="batchBusy || Boolean(composing.length) || !promptBatchShots.length" @click="openPromptBatchDialog"><Sparkles :size="15" />一键合成全部提示词</button>
          </template>
        </div>
      </footer>
    </section>

    <div v-if="promptBatchDialogOpen" class="prompt-batch-backdrop" @click="closePromptBatchDialog" />
    <section
      v-if="promptBatchDialogOpen"
      class="prompt-batch-dialog"
      role="dialog"
      aria-modal="true"
      aria-label="合成最终提示词"
      @keydown.esc.stop.prevent="closePromptBatchDialog"
    >
      <header>
        <div><h2>合成最终提示词</h2><CircleAlert :size="17" /></div>
        <button type="button" title="关闭" :disabled="batchBusy" @click="closePromptBatchDialog"><X :size="26" /></button>
      </header>
      <div class="prompt-batch-list">
        <article
          v-for="shot in promptBatchShots"
          :key="shot.id"
          :class="{ selected: promptBatchSelectedIds.includes(shot.id) }"
          @click="togglePromptBatchShot(shot.id)"
        >
          <input
            type="checkbox"
            :checked="promptBatchSelectedIds.includes(shot.id)"
            :aria-label="`选择镜头 ${shot.shotNo}`"
            @click.stop
            @change="togglePromptBatchShot(shot.id)"
          >
          <strong>镜头{{ shot.shotNo }}</strong>
          <p>{{ shot.description || "尚未填写画面描述" }}</p>
          <span :class="`status-${finalPromptStatus(shot)}`">{{ promptStatusLabel(finalPromptStatus(shot)) }}</span>
          <button type="button" @click.stop="openShotPrompt(shot)">详情</button>
        </article>
      </div>
      <footer>
        <label class="prompt-batch-select-all">
          <input type="checkbox" :checked="allPromptBatchSelected" @change="toggleAllPromptBatchShots">
          <span>全选镜头</span>
          <small>已选 {{ promptBatchSelectedIds.length }}/{{ promptBatchShots.length }}</small>
        </label>
        <label class="prompt-batch-model">
          <Sparkles :size="19" />
          <select v-model="selectedPromptTextModel" :disabled="promptComposeMode === 'template'">
            <option value="" disabled>选择文本模型</option>
            <option v-for="model in textModels" :key="model.code" :value="model.code">{{ model.name ?? model.code }}</option>
          </select>
        </label>
        <div class="prompt-batch-modes">
          <label><input v-model="promptComposeMode" type="radio" value="smart">智能合成</label>
          <label><input v-model="promptComposeMode" type="radio" value="template">自动拼接</label>
        </div>
        <span class="prompt-batch-credits" :title="promptBatchQuote?.skipCount ? `${promptBatchQuote.skipCount} 条已就绪且没有改动，会跳过` : undefined"><small v-if="promptBatchQuote?.skipCount">（跳过 {{ promptBatchQuote.skipCount }} 条已就绪）</small></span>
        <button
          type="button"
          class="prompt-batch-submit"
          :disabled="batchBusy || !promptBatchSelectedIds.length || (promptComposeMode === 'smart' && !selectedPromptTextModel)"
          @click="composeAll()"
        >
          <LoaderCircle v-if="batchBusy" :size="17" class="spin" />
          <Sparkles v-else :size="17" />
          {{ batchBusy
            ? (promptBatchProgress ? `正在合成 ${promptBatchProgress.done}/${promptBatchProgress.total}` : "正在合成")
            : `确认合成 (${promptBatchSelectedIds.length})` }}
        </button>
      </footer>
    </section>

    <div v-if="assetGenerationDialogOpen" class="asset-generation-backdrop" @click="closeAssetGenerationDialog" />
    <section
      v-if="assetGenerationDialogOpen"
      class="asset-generation-dialog"
      role="dialog"
      aria-modal="true"
      aria-label="一键生成所有资产"
    >
      <header>
        <h2>一键生成所有资产</h2>
        <button type="button" title="关闭" :disabled="batchBusy" @click="closeAssetGenerationDialog"><X :size="26" /></button>
      </header>
      <div class="asset-generation-list">
        <section v-for="type in assetTypes" v-show="generationAssetsByType(type.value).length" :key="type.value" class="asset-generation-section">
          <h3>{{ type.label }} ({{ generationAssetsByType(type.value).length }})</h3>
          <article
            v-for="asset in generationAssetsByType(type.value)"
            :key="asset.id"
            :class="{ selected: selectedAssetGenerationIds.includes(asset.id) }"
          >
            <label class="asset-generation-heading">
              <input type="checkbox" :checked="selectedAssetGenerationIds.includes(asset.id)" @change="toggleAssetGeneration(asset.id)">
              <strong>{{ asset.name }}</strong>
              <span>{{ type.label }}</span>
            </label>
            <textarea
              v-model="assetGenerationDrafts[asset.id]"
              rows="3"
              maxlength="4000"
              :aria-label="`${asset.name}描述`"
              :placeholder="`输入${type.label}的完整设定描述`"
            />
          </article>
        </section>
      </div>
      <footer>
        <label class="asset-generation-select-all">
          <input type="checkbox" :checked="allAssetGenerationSelected" @change="toggleAllAssetGeneration">
          <span>已选 {{ selectedAssetGenerationCount }}/{{ assetGenerationCandidates.length }}</span>
        </label>
        <label class="asset-generation-control">
          <ImageIcon :size="19" />
          <select v-model="selectedAssetImageModel" aria-label="图片模型">
            <option value="" disabled>选择图片模型</option>
            <option v-for="model in assetImageModels" :key="model.code" :value="model.code">{{ model.name ?? model.code }}</option>
          </select>
        </label>
        <label class="asset-generation-control">
          <Sparkles :size="18" />
          <select v-model="assetGenerationSettings.quality" aria-label="画质">
            <option v-for="option in assetQualityOptions" :key="option.value" :value="option.value">{{ option.label }}</option>
          </select>
        </label>
        <!-- 清晰度不再给选：图片的清晰度从没发给过上游（新后端只收画幅与画质），选了等于没选 -->
        <label class="asset-generation-control">
          <RectangleHorizontal :size="19" />
          <select v-model="assetGenerationSettings.ratio" aria-label="比例">
            <option v-for="option in assetRatioOptions" :key="option.value" :value="option.value">{{ option.label }}</option>
          </select>
        </label>
        <!-- 原「预计消耗积分」的位置（TT-T-14 删除金额）：保留空占位把提交键推到右侧，布局不变 -->
        <span class="asset-generation-credits" aria-hidden="true" />
        <button
          class="asset-generation-submit"
          type="button"
          :disabled="batchBusy || !selectedAssetGenerationCount || !selectedAssetImageModel"
          @click="generateAssets"
        >
          <LoaderCircle v-if="batchBusy" :size="17" class="spin" />
          <span>{{ batchBusy ? "提交中" : `生成(${selectedAssetGenerationCount})` }}</span>
        </button>
      </footer>
    </section>

    <div v-if="editingAsset" class="asset-editor-backdrop" @click="closeAssetEditor" />
    <aside
      v-if="editingAsset"
      class="asset-editor-drawer"
      role="dialog"
      aria-modal="true"
      :aria-label="`编辑${editingAssetType.label}`"
      @keydown.esc.stop.prevent="closeAssetEditor"
    >
      <header>
        <h2>编辑{{ editingAssetType.label }}</h2>
        <button type="button" title="关闭" :disabled="assetSaving" @click="closeAssetEditor"><X :size="22" /></button>
      </header>
      <div class="asset-editor-body">
        <section class="asset-editor-visual-section">
          <h3>{{ editingAssetType.label }}形象</h3>
          <div
            class="asset-editor-image"
            :class="{ generating: editingAsset.status === 'generating', failed: editingAsset.status === 'failed' }"
            :aria-busy="editingAsset.status === 'generating'"
          >
            <img
              v-if="assetImageSource(editingAsset)"
              :class="{ 'is-generating': editingAsset.status === 'generating' }"
              :src="assetImageSource(editingAsset)"
              :alt="editingAsset.name"
            >
            <div v-if="editingAsset.status === 'generating'" class="script-asset-generating asset-editor-generating" aria-live="polite">
              <span class="script-asset-generating-icon"><LoaderCircle :size="25" class="spin" /></span>
              <strong>正在生成设定图</strong>
              <small>完成后将自动刷新</small>
              <div
                class="script-asset-progress"
                :class="{ indeterminate: assetProgressValue(editingAsset) === 0 }"
                role="progressbar"
                aria-valuemin="0"
                aria-valuemax="100"
                :aria-valuenow="assetProgressValue(editingAsset) || undefined"
                :aria-valuetext="assetProgressLabel(editingAsset)"
              >
                <span class="script-asset-progress-track"><span class="script-asset-progress-fill" :style="assetProgressStyle(editingAsset)" /></span>
                <output>{{ assetProgressLabel(editingAsset) }}</output>
              </div>
            </div>
            <label v-else-if="!assetImageSource(editingAsset)" class="asset-editor-empty-upload">
              <Plus :size="31" />
              <span>{{ editingAssetType.placeholder }}</span>
              <input type="file" accept="image/*" :disabled="uploadingAssetId === editingAsset.id" @change="uploadAsset($event, editingAsset)">
            </label>
            <label
              class="asset-editor-image-menu"
              :class="{ disabled: editingAsset.status === 'generating' }"
              :title="editingAsset.status === 'generating' ? '资产生成中' : `上传或替换${editingAssetType.label}图`"
              @click.stop
            >
              <LoaderCircle v-if="uploadingAssetId === editingAsset.id" :size="17" class="spin" />
              <MoreHorizontal v-else :size="20" />
              <input type="file" accept="image/*" :disabled="Boolean(uploadingAssetId) || editingAsset.status === 'generating'" @change="uploadAsset($event, editingAsset)">
            </label>
          </div>
        </section>
        <label class="asset-editor-field">
          <span>{{ editingAssetType.label }}名称</span>
          <input v-model="assetNameDraft" type="text" maxlength="80" :placeholder="`输入${editingAssetType.label}名称`">
        </label>
        <label class="asset-editor-field asset-editor-description">
          <span>{{ editingAssetType.label }}描述</span>
          <textarea
            ref="assetDescriptionInput"
            v-model="assetDescriptionDraft"
            maxlength="4000"
            :placeholder="`输入完整的${editingAssetType.label}设定描述`"
            @input="resizeAssetDescription"
          />
          <small>{{ assetDescriptionDraft.length }}/4000</small>
        </label>
      </div>
      <footer>
        <button type="button" :disabled="assetSaving" @click="closeAssetEditor">取消</button>
        <button type="button" class="asset-editor-save" :disabled="assetSaving || !assetNameDraft.trim()" @click="saveAsset">
          <LoaderCircle v-if="assetSaving" :size="15" class="spin" />
          <Save v-else :size="15" />
          保存
        </button>
      </footer>
    </aside>

    <div v-if="descriptionEditorShot" class="description-editor-dismiss" @mousedown="saveDescriptionEditor(false)" />
    <section
      v-if="descriptionEditorShot"
      class="description-editor-popover"
      :style="descriptionEditorPosition"
      role="dialog"
      aria-label="编辑画面描述"
      @mousedown.stop
    >
      <ScriptDescriptionRichEditor
        ref="descriptionEditor"
        v-model="descriptionDraft"
        class="description-rich-editor"
        :assets="mentionAssets"
        @dismiss="saveDescriptionEditor(false)"
        @submit="saveDescriptionEditor(true)"
      />
      <footer>
        <span>输入 @ 选择资产；失焦自动保存</span>
        <button type="button" :disabled="saving || !descriptionDraft.trim()" @click="saveDescriptionEditor(true)">保存</button>
      </footer>
    </section>

    <div v-if="selectedPromptShot" class="prompt-detail-backdrop" @click="closePrompt" />
    <section
      v-if="selectedPromptShot"
      class="prompt-detail-dialog"
      role="dialog"
      aria-modal="true"
      :aria-label="`第 ${selectedPromptShot.shotNo} 镜最终提示词`"
      @keydown.esc.stop.prevent="closePrompt"
    >
      <header>
        <h2>第 {{ selectedPromptShot.shotNo }} 镜：最终提示词</h2>
        <button type="button" title="关闭" @click="closePrompt"><X :size="22" /></button>
      </header>
      <div class="prompt-detail-body">
        <section class="prompt-editor-section">
          <header>
            <div><strong>分镜提示词</strong><span :class="`status-${selectedPromptShot.imagePromptStatus}`"><CircleCheck v-if="selectedPromptShot.imagePromptStatus === 'ready'" :size="13" />{{ promptStatusLabel(selectedPromptShot.imagePromptStatus) }}</span></div>
            <small>{{ promptSourceLabel(selectedPromptShot.imagePromptSource) }}</small>
          </header>
          <ScriptDescriptionRichEditor
            v-model="imagePromptDraft"
            class="prompt-rich-editor"
            label="分镜提示词"
            placeholder="合成后将在这里显示分镜图提示词，输入 @ 引用人物、场景或道具"
            :assets="mentionAssets"
            @submit="savePrompt"
          />
        </section>
        <section class="prompt-editor-section">
          <header>
            <div><strong>视频运动提示词</strong><span :class="`status-${selectedPromptShot.videoPromptStatus}`"><CircleCheck v-if="selectedPromptShot.videoPromptStatus === 'ready'" :size="13" />{{ promptStatusLabel(selectedPromptShot.videoPromptStatus) }}</span></div>
            <small>{{ promptSourceLabel(selectedPromptShot.videoPromptSource) }}</small>
          </header>
          <ScriptDescriptionRichEditor
            v-model="videoPromptDraft"
            class="prompt-rich-editor"
            label="视频运动提示词"
            placeholder="合成后将在这里显示镜头运动、主体动作与参考素材规则，输入 @ 引用人物、场景或道具"
            :assets="mentionAssets"
            @submit="savePrompt"
          />
        </section>
      </div>
      <footer>
        <label class="prompt-model-select">
          <Sparkles :size="18" />
          <select v-model="selectedPromptTextModel" :disabled="promptComposeMode === 'template'">
            <option value="" disabled>选择文本模型</option>
            <option v-for="model in textModels" :key="model.code" :value="model.code">{{ model.name ?? model.code }}</option>
          </select>
        </label>
        <div class="prompt-compose-options">
          <label><input v-model="promptComposeMode" type="radio" value="smart">智能合成</label>
          <label><input v-model="promptComposeMode" type="radio" value="template">自动拼接</label>
        </div>
        <button type="button" class="prompt-save-button" :disabled="promptSaving || composing.includes(selectedPromptShot.id)" @click="savePrompt"><Save :size="15" />保存修改</button>
        <button type="button" class="prompt-compose-button" :disabled="promptSaving || composing.includes(selectedPromptShot.id)" @click="composePrompt(selectedPromptShot)"><LoaderCircle v-if="composing.includes(selectedPromptShot.id)" :size="16" class="spin" /><Sparkles v-else :size="16" />{{ finalPromptStatus(selectedPromptShot) === 'pending' ? '合成提示词' : '重新合成提示词' }}</button>
      </footer>
    </section>

    <div v-if="batchDialog" class="batch-dialog-backdrop" @click="batchDialog = undefined" />
    <section
      v-if="batchDialog"
      class="script-batch-dialog"
      role="dialog"
      aria-modal="true"
      :aria-label="batchDialog === 'video' ? '批量生视频' : '批量生成分镜图'"
    >
      <header>
        <div><span>{{ batchDialog === 'video' ? 'VIDEO GENERATOR GROUP' : 'STORYBOARD IMAGE GROUP' }}</span><h2>{{ batchDialog === 'video' ? '批量生视频' : '批量生成分镜图' }}</h2></div>
        <button type="button" title="关闭" @click="batchDialog = undefined"><X :size="19" /></button>
      </header>
      <p class="batch-dialog-note">{{ batchDialog === 'video' ? '优先使用同镜头已生成的分镜图作为首帧，并保留角色、场景和道具上下文。' : '每个镜头独立生成一张首帧图，失败项可在画布组节点中单独重试。' }}</p>
      <div class="batch-shot-list">
        <article
          v-for="shot in (batchDialog === 'video' ? videoEligible : storyboardEligible)"
          :key="shot.id"
          :class="{ selected: selectedShotIds.includes(shot.id) }"
          @click="toggleBatchShot(shot.id)"
        >
          <input type="checkbox" :checked="selectedShotIds.includes(shot.id)" @click.stop @change="toggleBatchShot(shot.id)">
          <div><strong>镜头 {{ String(shot.shotNo).padStart(2, '0') }}</strong><p>{{ batchDialog === 'video' ? shot.videoPrompt : shot.imagePrompt }}</p><small>{{ shot.assetIds.length }} 个资产引用 · {{ shot.shotSize || '未设景别' }}</small></div>
          <label v-if="batchDialog === 'video'" @click.stop>时长<input v-model.number="videoDurations[shot.id]" type="number" min="1" max="60">s</label>
        </article>
      </div>
      <footer v-if="batchDialog === 'storyboard'">
        <label>图片模型<select v-model="selectedImageModel"><option v-for="item in imageModels" :key="item.code" :value="item.code">{{ item.name ?? item.code }}</option></select></label>
        <label>比例<select v-model="storyboardSettings.ratio"><option>16:9</option><option>9:16</option><option>1:1</option></select></label>
        <label>画质<select v-model="storyboardSettings.quality"><option value="standard">标准</option><option value="high">高</option></select></label>
        <button type="button" :disabled="batchBusy || !selectedShotIds.length" @click="createStoryboardGroup"><LoaderCircle v-if="batchBusy" :size="15" class="spin" /><ImageIcon v-else :size="15" />确认并执行（{{ selectedShotIds.length }}）</button>
      </footer>
      <footer v-else>
        <label>视频模型<select v-model="selectedVideoModel"><option v-for="item in videoModels" :key="item.code" :value="item.code">{{ item.name ?? item.code }}</option></select></label>
        <label>比例<select v-model="videoSettings.ratio"><option>16:9</option><option>9:16</option><option>1:1</option></select></label>
        <label>清晰度<select v-model="videoSettings.clarity"><option>720P</option><option>1080P</option></select></label>
        <label class="batch-audio"><input v-model="videoSettings.audio" type="checkbox">音频</label>
        <button type="button" :disabled="batchBusy || !selectedShotIds.length" @click="createVideoGroup"><LoaderCircle v-if="batchBusy" :size="15" class="spin" /><Film v-else :size="15" />确认并创建视频生成器组（{{ selectedShotIds.length }}）</button>
      </footer>
    </section>
  </div>
</template>

<style scoped>
.script-studio-overlay {
  --studio-bg: #0b0b0c;
  --studio-panel: #161617;
  --studio-header: #202021;
  --studio-line: #343436;
  --studio-text: #dedee0;
  --studio-muted: #8b8b90;
  --studio-accent: #3bb8c9;
  position: fixed;
  inset: 0;
  z-index: 180;
  display: grid;
  place-items: stretch;
  padding: 0;
  background: var(--studio-bg);
  color: var(--studio-text);
}
.script-studio-shell { width: 100%; height: 100%; min-width: 0; display: grid; grid-template-rows: 74px minmax(0, 1fr) 60px; overflow: hidden; border: 0; border-radius: 0; background: var(--studio-bg); box-shadow: none; }
.script-studio-header { display: grid; grid-template-columns: minmax(160px,1fr) minmax(620px,760px) minmax(240px,1fr); align-items: center; gap: 24px; padding: 0 20px; background: var(--studio-bg); }
.script-header-spacer { min-width: 0; }
.script-header-meta { justify-self: end; display: flex; align-items: center; gap: 18px; color: #c9c9cb; font-size: 12px; white-space: nowrap; }
.script-studio-close { width: 32px; height: 32px; display: grid; place-items: center; border: 0; border-radius: 4px; color: #8d8d91; background: transparent; }
.script-studio-close:hover { color: #f2f2f3; background: #202021; }
.eyebrow { color: var(--studio-muted); font-size: 9px; letter-spacing: .12em; }
.script-step-bar { width: 100%; min-width: 0; display: grid; grid-template-columns: repeat(3, minmax(0,1fr)); gap: 52px; }
.script-step-bar button { min-width: 0; position: relative; display: grid; grid-template-columns: 36px minmax(0,1fr); grid-template-rows: auto auto; align-items: center; column-gap: 10px; border: 0; border-radius: 7px; padding: 8px 12px; color: #8d8d92; background: transparent; text-align: left; }
.script-step-bar button:not(:last-child)::after { content: ""; width: 44px; height: 1px; position: absolute; left: calc(100% + 4px); top: 50%; background: #303033; pointer-events: none; }
.script-step-bar button span { width: 34px; height: 34px; grid-row: 1 / 3; display: grid; place-items: center; border: 2px solid #4c4c50; border-radius: 50%; color: #c9c9cc; font-size: 14px; font-weight: 650; }
.script-step-bar button strong { align-self: end; color: inherit; font-size: 13px; font-weight: 520; }
.script-step-bar button small { align-self: start; overflow: hidden; color: #68686d; font-size: 10px; text-overflow: ellipsis; white-space: nowrap; }
.script-step-bar button.active { color: #f0f0f1; background: #242425; }
.script-step-bar button.complete span { border-color: #f0f0f1; color: #141415; background: #f0f0f1; }
.script-studio-body { min-height: 0; overflow: hidden; background: #0e0e0f; }
.script-studio-loading { height: 100%; display: grid; place-items: center; align-content: center; gap: 12px; color: #969aa2; font-size: 12px; }
.script-step-content { height: 100%; min-height: 0; display: grid; grid-template-rows: auto auto minmax(0,1fr); gap: 14px; padding: 18px 24px 14px; overflow: hidden; }
.shots-step { grid-template-rows: minmax(0,1fr); gap: 0; padding: 0 20px 10px; }
.script-content-toolbar { display: flex; justify-content: space-between; align-items: end; gap: 24px; }
.script-content-toolbar h2 { margin: 6px 0 3px; font-size: 22px; font-weight: 620; }
.script-content-toolbar p { margin: 0; color: #858991; font-size: 11px; }
.script-toolbar-actions, .prompt-batch-actions { display: flex; align-items: center; gap: 7px; flex-wrap: wrap; }
.script-toolbar-actions button, .prompt-batch-actions button, .prompt-batch-actions select, .prompt-mode-select, .script-footer-actions button { min-height: 32px; display: inline-flex; align-items: center; gap: 6px; border: 1px solid #3a3a3d; border-radius: 6px; padding: 0 10px; color: #c8c8cb; background: #242425; font-size: 10px; }
.asset-model-select { min-height: 32px; border: 1px solid #3a3a3d; border-radius: 6px; outline: 0; padding: 0 8px; color: #c8c8cb; background: #242425; font-size: 10px; }.asset-model-select option { color: #222; }
.script-toolbar-actions button:hover, .prompt-batch-actions button:hover { color: #fff; border-color: #77777b; }
.script-toolbar-actions button:disabled, .prompt-batch-actions button:disabled { opacity: .45; cursor: wait; }
.script-global-style, .asset-style-banner { display: flex; align-items: center; gap: 10px; min-height: 42px; padding: 0 13px; border: 1px solid rgba(53,194,210,.18); border-radius: 7px; color: #50c7d7; background: rgba(53,194,210,.06); }
.script-global-style label { display: flex; align-items: center; gap: 10px; flex: 1; color: #81858c; font-size: 10px; }
.script-global-style input { min-width: 0; flex: 1; border: 0; outline: 0; color: #dce0e3; background: transparent; font-size: 11px; }
.shot-table-wrap, .prompt-table-wrap { min-height: 0; overflow: auto; border: 1px solid var(--studio-line); border-radius: 8px; background: var(--studio-panel); scrollbar-color: #8b8b90 #242426; }
.shot-table, .prompt-table { width: 100%; min-width: 1680px; border-collapse: collapse; table-layout: fixed; }
.shot-table th, .prompt-table th { height: 50px; box-sizing: border-box; position: sticky; top: 0; z-index: 2; border-right: 1px solid var(--studio-line); border-bottom: 1px solid var(--studio-line); padding: 0 12px; color: #8f8f94; background: var(--studio-header); font-size: 12px; font-weight: 520; text-align: left; }
.shot-table td, .prompt-table td { height: 74px; box-sizing: border-box; border-right: 1px solid var(--studio-line); border-bottom: 1px solid var(--studio-line); padding: 7px 9px; color: #d0d0d2; font-size: 12px; line-height: 1.48; vertical-align: middle; }
.shot-table th:last-child, .shot-table td:last-child, .prompt-table th:last-child, .prompt-table td:last-child { border-right: 0; }
.shot-table tbody tr:hover { background: rgba(255,255,255,.018); }
.shot-number-column { width: 54px; }.shot-duration-column { width: 70px; }.shot-table .shot-description { width: 28%; }.shot-size-column { width: 82px; }.shot-lighting-column { width: 11%; }.shot-dialogue-column { width: 18%; }.shot-sound-column { width: 11%; }.shot-camera-column { width: 11%; }.shot-prompt-column { width: 112px; }.shot-table .shot-action { width: 56px; }
.prompt-batch-bar input[type="checkbox"] { accent-color: #eeeef0; }
.shot-table input:not([type="checkbox"]), .shot-table textarea, .shot-table select { width: 100%; min-height: 28px; box-sizing: border-box; border: 1px solid transparent; border-radius: 3px; outline: 0; padding: 4px 5px; color: #d8d8da; background: transparent; font: inherit; line-height: 1.45; resize: none; }
.shot-table textarea { min-height: 58px; }
.shot-table input:not([type="checkbox"]):focus, .shot-table textarea:focus, .shot-table select:focus { border-color: #66666a; background: #222224; }.shot-table select option, .prompt-batch-actions select option { color: #222; }
.shot-duration { width: 43px !important; display: inline-block; }.shot-table td > small { color: #707075; }.shot-number { text-align: center; }.shot-number span { color: #b8b8bc; font-weight: 520; }.prompt-shot-no { color: var(--studio-accent); font-weight: 620; }
.shot-description-display { width: 100%; max-height: 58px; display: -webkit-box; overflow: hidden; border: 0; padding: 3px 5px; color: #d8d8da; background: transparent; font: inherit; line-height: 1.5; text-align: left; overflow-wrap: anywhere; cursor: text; -webkit-box-orient: vertical; -webkit-line-clamp: 3; }
.shot-description.is-editing { position: relative; z-index: 1; outline: 2px solid #eeeeef; outline-offset: -2px; }.shot-description-display:hover { background: #212123; }.shot-description-display:focus-visible { outline: 1px solid #66666a; outline-offset: -1px; }.shot-description-empty { color: #68686d; }
.script-mention { border: 0; border-radius: 3px; padding: 0 2px; color: inherit; background: transparent; font: inherit; }.script-mention.mention-character { color: #4fd0e4; background: rgba(79,208,228,.1); }.script-mention.mention-scene { color: #7bcf96; background: rgba(123,207,150,.1); }.script-mention.mention-prop { color: #e3b45f; background: rgba(227,180,95,.1); }.script-mention.mention-unknown { color: #b89be8; background: rgba(184,155,232,.1); }
.shot-final-prompt { text-align: center; }.shot-final-prompt button { border: 0; padding: 5px 2px; color: #98989d; background: transparent; font: inherit; white-space: nowrap; }.shot-final-prompt button:not(:disabled):hover { color: #ededee; }.shot-final-prompt button:disabled { cursor: default; }
.shot-action { position: relative; text-align: center; }.shot-action details { position: relative; display: inline-block; }.shot-action summary { width: 28px; height: 28px; display: grid; place-items: center; border-radius: 4px; color: #8f8f94; cursor: pointer; list-style: none; }.shot-action summary::-webkit-details-marker { display: none; }.shot-action summary:hover, .shot-action details[open] summary { color: #fff; background: #29292b; }.shot-action-menu { width: 106px; position: absolute; z-index: 5; right: 0; top: 32px; display: grid; overflow: hidden; border: 1px solid #3b3b3e; border-radius: 6px; background: #202022; box-shadow: 0 12px 28px rgba(0,0,0,.5); }.shot-action-menu button { min-height: 34px; display: flex; align-items: center; gap: 8px; border: 0; padding: 0 11px; color: #c8c8cb; background: transparent; font-size: 11px; text-align: left; }.shot-action-menu button:hover { color: #fff; background: #303033; }.shot-action-menu button:disabled { opacity: .3; }
.icon-button { width: 25px; height: 25px; display: grid; place-items: center; border: 0; border-radius: 4px; color: #92969e; background: transparent; }.icon-button:hover { color: #fff; background: #33353a; }
.script-empty-table, .script-empty-state { min-height: 230px; display: grid; place-items: center; align-content: center; gap: 10px; color: #777b83; font-size: 11px; }.script-empty-table button, .script-empty-state button { min-height: 30px; border: 1px solid rgba(53,194,210,.35); border-radius: 5px; padding: 0 12px; color: #48c5d5; background: rgba(53,194,210,.08); font-size: 10px; }
.assets-step {
  grid-template-rows: auto minmax(0, 1fr);
  gap: 14px;
  padding: 8px 18px 12px;
  background: #09090a;
}
.asset-style-banner {
  min-width: 0;
  min-height: 26px;
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 0;
  border: 0;
  border-radius: 0;
  background: transparent;
}
.asset-style-banner > span {
  flex: none;
  border-radius: 3px;
  padding: 3px 7px;
  color: #42c8d8;
  background: rgba(31, 154, 172, .18);
  font-size: 11px;
  line-height: 1.2;
}
.asset-style-banner p {
  overflow: hidden;
  margin: 0;
  color: #9a9a9f;
  font-size: 12px;
  line-height: 1.4;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.script-asset-sections {
  min-height: 0;
  overflow: auto;
  display: grid;
  align-content: start;
  gap: 26px;
  padding: 0 14px 20px 0;
  scrollbar-color: #8b8b90 transparent;
}
.script-asset-section h3 {
  margin: 0 0 10px;
  color: #d8d8da;
  font-size: 13px;
  font-weight: 540;
}
.script-asset-grid {
  display: flex;
  align-items: flex-start;
  flex-wrap: wrap;
  gap: 12px;
}
.script-asset-item {
  width: 296px;
  min-width: 0;
  border-radius: 4px;
  outline: 0;
  cursor: pointer;
}
.script-asset-item:focus-visible .script-asset-card,
.script-asset-item:hover .script-asset-card { border-color: #77777c; }
.script-asset-card {
  width: 100%;
  aspect-ratio: 16 / 9;
  box-sizing: border-box;
  position: relative;
  display: grid;
  place-items: center;
  overflow: hidden;
  border: 1px dashed #48484c;
  border-radius: 4px;
  color: #68686d;
  background: #101011;
}
.script-asset-card:has(img) {
  border-style: solid;
  border-color: #2f2f32;
  background: #0d0d0e;
}
.script-asset-card.generating {
  border-style: solid;
  border-color: rgba(65, 200, 216, .52);
  background: #111718;
  box-shadow: inset 0 0 22px rgba(65, 200, 216, .04);
}
.script-asset-card.generating::before {
  content: "";
  width: 62%;
  height: 180%;
  position: absolute;
  z-index: 1;
  top: -40%;
  left: -75%;
  background: linear-gradient(90deg, transparent, rgba(79, 215, 230, .13), transparent);
  transform: skewX(-18deg);
  animation: asset-card-sheen 1.8s ease-in-out infinite;
  pointer-events: none;
}
.script-asset-card.failed { border-color: rgba(220, 103, 110, .58); }
.script-asset-card > img {
  width: 100%;
  height: 100%;
  display: block;
  object-fit: contain;
  background: #0b0b0c;
}
.script-asset-card > img.is-generating,
.asset-editor-image > img.is-generating {
  opacity: .28;
  filter: saturate(.45) brightness(.72);
}
.script-asset-generating {
  position: absolute;
  z-index: 2;
  inset: 0;
  display: grid;
  place-content: center;
  justify-items: center;
  gap: 5px;
  color: #dfeff1;
  text-align: center;
  pointer-events: none;
}
.script-asset-generating-icon {
  width: 34px;
  height: 34px;
  display: grid;
  place-items: center;
  border: 1px solid rgba(83, 208, 222, .34);
  border-radius: 50%;
  color: #55d1df;
  background: rgba(23, 58, 62, .58);
  box-shadow: 0 0 18px rgba(65, 200, 216, .12);
}
.script-asset-generating strong {
  font-size: 11px;
  font-weight: 620;
}
.script-asset-generating small {
  color: #7f9ea2;
  font-size: 9px;
}
.script-asset-progress {
  width: min(154px, 76%);
  display: grid;
  grid-template-columns: minmax(0, 1fr) auto;
  align-items: center;
  gap: 7px;
  margin-top: 6px;
}
.script-asset-progress-track {
  height: 4px;
  overflow: hidden;
  border-radius: 999px;
  background: rgba(107, 139, 144, .22);
  box-shadow: inset 0 0 0 1px rgba(100, 191, 201, .08);
}
.script-asset-progress-fill {
  width: 0;
  height: 100%;
  display: block;
  border-radius: inherit;
  background: #4fd0df;
  box-shadow: 0 0 8px rgba(79, 208, 223, .48);
  transition: width .35s ease;
}
.script-asset-progress output {
  min-width: 28px;
  color: #8fb4b9;
  font-size: 9px;
  line-height: 1;
  text-align: right;
}
.script-asset-progress.indeterminate .script-asset-progress-fill {
  width: 38%;
  animation: asset-progress-indeterminate 1.35s ease-in-out infinite;
}
.script-asset-placeholder {
  display: inline-flex;
  align-items: center;
  gap: 7px;
  color: #66666b;
  font-size: 10px;
}
.script-asset-card.failed .script-asset-placeholder { color: #c36a70; }
.script-asset-upload {
  width: 25px;
  height: 25px;
  position: absolute;
  right: 7px;
  top: 7px;
  display: grid;
  place-items: center;
  border-radius: 4px;
  color: #e3e3e5;
  background: rgba(6, 6, 7, .88);
  cursor: pointer;
}
.script-asset-upload:hover { background: #262628; }
.script-asset-upload.disabled { z-index: 3; opacity: .42; cursor: wait; }
.script-asset-upload input { display: none; }

@keyframes asset-card-sheen {
  0% { left: -75%; }
  58%, 100% { left: 118%; }
}
@keyframes asset-progress-indeterminate {
  0% { transform: translateX(-110%); }
  55% { transform: translateX(115%); }
  100% { transform: translateX(265%); }
}
.script-asset-meta {
  min-width: 0;
  display: grid;
  gap: 4px;
  padding: 8px 0 0;
}
.script-asset-meta strong {
  overflow: hidden;
  color: #e0e0e2;
  font-size: 12px;
  font-weight: 540;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.script-asset-meta p {
  min-width: 0;
  display: flex;
  gap: 5px;
  overflow: hidden;
  margin: 0;
  color: #6f6f74;
  font-size: 9px;
  line-height: 1.4;
  white-space: nowrap;
}
.script-asset-meta p span {
  overflow: hidden;
  text-overflow: ellipsis;
}
.script-asset-meta p span:first-child { flex: none; max-width: 42%; }
.script-asset-meta p span:last-child { flex: 1; }
.script-asset-add {
  width: 166px;
  aspect-ratio: 1;
  display: grid;
  place-items: center;
  align-content: center;
  gap: 7px;
  border: 1px dashed #454549;
  border-radius: 4px;
  color: #66666b;
  background: #101011;
  font-size: 10px;
}
.script-asset-add:hover:not(:disabled) {
  color: #a6a6aa;
  border-color: #747478;
  background: #141415;
}
.script-asset-add:disabled { opacity: .45; cursor: wait; }
.asset-footer-notice {
  min-width: 0;
  display: flex;
  align-items: center;
  gap: 8px;
  color: #9e9ea2;
}
.asset-footer-notice svg { flex: none; color: #e38a18; }
.asset-footer-notice.complete svg { color: #26b878; }
.asset-footer-notice span {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.prompts-step { grid-template-rows: auto auto minmax(0,1fr) auto; }.prompt-mode-select { padding-left: 8px; }.prompt-mode-select span { color: #767a82; }.prompt-mode-select select { border: 0; outline: 0; color: #d8dbdf; background: transparent; font: inherit; }.prompt-batch-bar { display: flex; align-items: center; gap: 16px; min-height: 42px; padding: 0 12px; border: 1px solid rgba(255,255,255,.1); border-radius: 7px; color: #8d9198; background: #202125; font-size: 10px; }.prompt-batch-bar > label { display: inline-flex; align-items: center; gap: 6px; white-space: nowrap; }.prompt-batch-actions { margin-left: auto; }.prompt-batch-actions select { outline: 0; padding-right: 7px; }.prompt-table { min-width: 980px; }.prompt-table th:nth-child(1), .prompt-table td:nth-child(1) { width: 72px; }.prompt-table th:nth-child(2), .prompt-table td:nth-child(2) { width: 23%; }.prompt-table th:nth-child(5), .prompt-table td:nth-child(5) { width: 46px; }.prompt-table td { vertical-align: middle; }.prompt-table td:first-child { display: grid; gap: 3px; }.prompt-table td:first-child small { color: #777b83; }.prompt-status { min-height: 25px; display: inline-flex; align-items: center; gap: 5px; border: 1px solid rgba(255,255,255,.12); border-radius: 5px; padding: 0 7px; color: #a4a8af; background: #292a2e; font-size: 9px; }.prompt-status.status-ready { color: #63cfb1; border-color: rgba(99,207,177,.28); }.prompt-status.status-failed { color: #ed7c83; border-color: rgba(237,124,131,.3); }.prompt-preview { max-width: 390px; max-height: 47px; overflow: auto; margin: 5px 0 0; color: #7f838b; font-size: 9px; line-height: 1.45; }.prompt-description { color: #b8bbc0 !important; }.script-group-list { display: flex; align-items: center; gap: 8px; min-height: 34px; color: #7e828a; font-size: 10px; }.script-group-list button { display: inline-flex; gap: 7px; border: 1px solid rgba(255,255,255,.1); border-radius: 5px; padding: 6px 9px; color: #d3d6da; background: #25262a; font-size: 9px; }.script-group-list button strong { color: #858991; font-weight: 400; }
.script-studio-footer { display: flex; align-items: center; justify-content: space-between; gap: 18px; padding: 0 22px; border-top: 1px solid var(--studio-line); background: #171718; color: #8e8e93; font-size: 11px; }.script-studio-footer > div:first-child { display: flex; align-items: center; gap: 8px; }.script-studio-footer small { margin-left: 8px; color: #65656a; }.footer-add-shot { min-height: 36px; display: inline-flex; align-items: center; gap: 8px; border: 0; padding: 0 10px; color: #d8d8da; background: transparent; font-size: 12px; }.footer-add-shot:hover { color: #fff; }.footer-status-dot { width: 7px; height: 7px; border-radius: 50%; background: #8d8d91; }.script-footer-actions { display: flex; gap: 7px; }.script-footer-actions button { color: #bfc2c7; }.script-footer-actions .footer-primary, .prompt-detail-actions .footer-primary { min-height: 38px; color: #171719; border-color: #f0f0f1; background: #f0f0f1; font-size: 12px; font-weight: 600; }.script-footer-actions .footer-primary:hover, .prompt-detail-actions .footer-primary:hover { background: #fff; }.prompt-detail-drawer { width: min(560px, calc(100% - 40px)); height: min(580px, calc(100% - 120px)); position: absolute; right: 32px; top: 102px; display: grid; grid-template-rows: 70px 44px minmax(0,1fr) 60px; overflow: hidden; border: 1px solid rgba(255,255,255,.2); border-radius: 8px; background: #242529; box-shadow: 0 22px 65px rgba(0,0,0,.65); }.prompt-detail-drawer header { display: flex; align-items: center; justify-content: space-between; padding: 0 18px; border-bottom: 1px solid rgba(255,255,255,.1); }.prompt-detail-drawer header span { color: #49c5d5; font-size: 9px; }.prompt-detail-drawer h3 { margin: 5px 0 0; font-size: 15px; }.prompt-detail-drawer header button { border: 0; color: #9da1a8; background: transparent; }.prompt-detail-tabs { display: flex; gap: 4px; padding: 7px 12px; border-bottom: 1px solid rgba(255,255,255,.08); }.prompt-detail-tabs button { display: inline-flex; align-items: center; gap: 5px; border: 0; border-radius: 5px; padding: 0 9px; color: #8f939a; background: transparent; font-size: 10px; }.prompt-detail-tabs button.active { color: #ebedef; background: #34363b; }.prompt-detail-drawer > textarea { min-height: 0; border: 0; outline: 0; padding: 16px; color: #e2e4e7; background: #202125; font: 12px/1.65 inherit; resize: none; }.prompt-detail-actions { display: flex; justify-content: flex-end; gap: 7px; align-items: center; padding: 0 14px; border-top: 1px solid rgba(255,255,255,.1); }.prompt-detail-actions button { min-height: 31px; display: inline-flex; align-items: center; gap: 6px; border: 1px solid rgba(255,255,255,.12); border-radius: 5px; padding: 0 9px; color: #c9ccd0; background: #2d2f34; font-size: 10px; }
.script-studio-footer.asset-studio-footer { background: #1b1b1c; }
.asset-generation-backdrop {
  position: absolute;
  z-index: 14;
  inset: 0;
  background: rgba(0, 0, 0, .78);
}
.asset-generation-dialog {
  position: absolute;
  z-index: 15;
  inset: 8px 16px;
  display: grid;
  grid-template-rows: 94px minmax(0, 1fr) 104px;
  overflow: hidden;
  border: 1px solid #444448;
  border-radius: 8px;
  color: #d9d9dc;
  background: #171718;
  box-shadow: 0 28px 90px rgba(0, 0, 0, .72);
  color-scheme: dark;
}
.asset-generation-dialog > header {
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding: 0 34px;
}
.asset-generation-dialog > header h2 {
  margin: 0;
  color: #eeeeef;
  font-size: 24px;
  font-weight: 650;
}
.asset-generation-dialog > header button {
  width: 42px;
  height: 42px;
  display: grid;
  place-items: center;
  border: 0;
  border-radius: 5px;
  color: #b6b6ba;
  background: transparent;
}
.asset-generation-dialog > header button:hover:not(:disabled) { color: #fff; background: #242426; }
.asset-generation-list {
  min-height: 0;
  display: grid;
  align-content: start;
  gap: 28px;
  overflow: auto;
  padding: 14px 34px 34px;
  scrollbar-color: #8d959e transparent;
}
.asset-generation-section {
  display: grid;
  gap: 10px;
}
.asset-generation-section h3 {
  margin: 0 0 2px;
  color: #8f8f94;
  font-size: 16px;
  font-weight: 560;
}
.asset-generation-section article {
  min-width: 0;
  display: grid;
  gap: 7px;
  border: 1px solid #39393c;
  border-radius: 7px;
  padding: 14px 18px 15px;
  background: #19191a;
}
.asset-generation-section article.selected { border-color: #48484c; }
.asset-generation-heading {
  min-width: 0;
  display: flex;
  align-items: center;
  gap: 11px;
  color: #dedee0;
  cursor: pointer;
}
.asset-generation-heading input,
.asset-generation-select-all input {
  width: 20px;
  height: 20px;
  flex: none;
  margin: 0;
  accent-color: #f1f1f2;
}
.asset-generation-heading strong {
  overflow: hidden;
  font-size: 16px;
  font-weight: 590;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.asset-generation-heading span {
  flex: none;
  border-radius: 4px;
  padding: 3px 8px;
  color: #a3a3a8;
  background: #343436;
  font-size: 12px;
}
.asset-generation-section textarea {
  width: calc(100% - 43px);
  min-height: 84px;
  box-sizing: border-box;
  margin-left: 43px;
  border: 1px solid #3b3b3e;
  border-radius: 5px;
  outline: 0;
  padding: 10px 12px;
  color: #bcbcc0;
  background: #1d1d1e;
  font: 14px/1.55 inherit;
  resize: vertical;
  scrollbar-color: #929aa4 transparent;
}
.asset-generation-section textarea:focus { border-color: #68686c; color: #e0e0e2; }
.asset-generation-dialog > footer {
  min-width: 0;
  display: flex;
  align-items: center;
  gap: 24px;
  overflow-x: auto;
  padding: 0 32px;
  border-top: 1px solid #333336;
  background: #181819;
  scrollbar-width: thin;
}
.asset-generation-select-all,
.asset-generation-control,
.asset-generation-credits {
  flex: none;
  display: inline-flex;
  align-items: center;
  gap: 9px;
  color: #d5d5d8;
  font-size: 15px;
  white-space: nowrap;
}
.asset-generation-select-all { cursor: pointer; }
.asset-generation-control { color: #f0f0f1; }
.asset-generation-control select {
  min-width: 0;
  max-width: 210px;
  border: 0;
  outline: 0;
  padding: 4px 20px 4px 0;
  color: #e1e1e3;
  background: transparent;
  font: inherit;
  font-size: 15px;
  cursor: pointer;
}
.asset-generation-control select option { color: #e1e1e3; background: #272729; }
.asset-generation-credits {
  margin-left: auto;
  color: #b0b0b4;
  font-size: 17px;
}
.asset-generation-submit {
  min-width: 120px;
  min-height: 52px;
  flex: none;
  display: inline-flex;
  align-items: center;
  justify-content: center;
  gap: 8px;
  border: 1px solid #f0f0f1;
  border-radius: 8px;
  padding: 0 18px;
  color: #171719;
  background: #f0f0f1;
  font-size: 16px;
  font-weight: 620;
}
.asset-generation-submit:hover:not(:disabled) { background: #fff; }
.asset-generation-dialog button:disabled { opacity: .45; cursor: default; }
.asset-editor-backdrop {
  position: absolute;
  z-index: 12;
  inset: 0;
  background: rgba(0, 0, 0, .78);
}
.asset-editor-drawer {
  width: min(600px, 40vw);
  min-width: 480px;
  position: absolute;
  z-index: 13;
  inset: 0 0 0 auto;
  display: grid;
  grid-template-rows: 52px minmax(0, 1fr) 64px;
  overflow: hidden;
  border-left: 1px solid #303033;
  color: #d9d9db;
  background: #202021;
  box-shadow: -24px 0 64px rgba(0, 0, 0, .42);
  color-scheme: dark;
}
.asset-editor-drawer > header {
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding: 0 18px;
  border-bottom: 1px solid #363638;
}
.asset-editor-drawer > header h2 {
  margin: 0;
  color: #e5e5e7;
  font-size: 15px;
  font-weight: 600;
}
.asset-editor-drawer > header button {
  width: 34px;
  height: 34px;
  display: grid;
  place-items: center;
  border: 0;
  border-radius: 4px;
  color: #c5c5c8;
  background: transparent;
}
.asset-editor-drawer > header button:hover:not(:disabled) { color: #fff; background: #2d2d2f; }
.asset-editor-body {
  min-height: 0;
  display: grid;
  align-content: start;
  gap: 20px;
  overflow: auto;
  padding: 22px 18px 30px;
  scrollbar-color: #737378 transparent;
}
.asset-editor-visual-section {
  display: grid;
  gap: 10px;
}
.asset-editor-visual-section h3,
.asset-editor-field > span {
  margin: 0;
  color: #dadadd;
  font-size: 13px;
  font-weight: 560;
}
.asset-editor-image {
  width: 100%;
  aspect-ratio: 3 / 2;
  position: relative;
  display: grid;
  place-items: center;
  overflow: hidden;
  border: 1px dashed #505054;
  border-radius: 5px;
  background: #29292a;
}
.asset-editor-image:has(img) { border-style: solid; border-color: #3d3d40; background: #111112; }
.asset-editor-image.generating {
  border-style: solid;
  border-color: rgba(65, 200, 216, .52);
  background: #111718;
}
.asset-editor-image.failed { border-color: rgba(220, 103, 110, .65); }
.asset-editor-image > img {
  width: 100%;
  height: 100%;
  display: block;
  object-fit: contain;
}
.asset-editor-empty-upload {
  display: grid;
  place-items: center;
  gap: 10px;
  color: #77777c;
  font-size: 11px;
  cursor: pointer;
}
.asset-editor-empty-upload:hover { color: #aaaab0; }
.asset-editor-empty-upload input,
.asset-editor-image-menu input { display: none; }
.asset-editor-image-menu {
  width: 31px;
  height: 31px;
  position: absolute;
  right: 9px;
  top: 9px;
  display: grid;
  place-items: center;
  border-radius: 4px;
  color: #e8e8ea;
  background: rgba(7, 7, 8, .88);
  cursor: pointer;
}
.asset-editor-image-menu:hover { background: #111113; }
.asset-editor-image-menu.disabled { z-index: 3; opacity: .42; cursor: wait; }
.asset-editor-generating {
  gap: 9px;
  background: radial-gradient(circle at center, rgba(37, 91, 97, .16), transparent 48%);
}
.asset-editor-generating .script-asset-generating-icon { width: 46px; height: 46px; }
.asset-editor-generating strong { font-size: 14px; }
.asset-editor-generating small { font-size: 11px; }
.asset-editor-field {
  min-width: 0;
  display: grid;
  gap: 10px;
}
.asset-editor-field input,
.asset-editor-field textarea {
  width: 100%;
  box-sizing: border-box;
  border: 1px solid transparent;
  border-radius: 5px;
  outline: 0;
  color: #e2e2e4;
  background: #303031;
  font: 12px/1.65 inherit;
}
.asset-editor-field input {
  height: 40px;
  padding: 0 12px;
}
.asset-editor-field textarea {
  min-height: 280px;
  padding: 13px 12px 28px;
  /* 高度由 resizeAssetDescription 按内容设定，这里禁掉内层滚动条与手动拖拽 */
  overflow: hidden;
  resize: none;
}
.asset-editor-field input:focus,
.asset-editor-field textarea:focus { border-color: #66666b; background: #323234; }
.asset-editor-description { position: relative; }
.asset-editor-description small {
  position: absolute;
  right: 11px;
  bottom: 8px;
  color: #76767b;
  font-size: 9px;
  pointer-events: none;
}
.asset-editor-drawer > footer {
  display: flex;
  align-items: center;
  justify-content: flex-end;
  gap: 9px;
  padding: 0 18px;
  border-top: 1px solid #363638;
  background: #1d1d1e;
}
.asset-editor-drawer > footer button {
  min-width: 82px;
  min-height: 38px;
  display: inline-flex;
  align-items: center;
  justify-content: center;
  gap: 7px;
  border: 1px solid #49494d;
  border-radius: 5px;
  color: #cacacd;
  background: #2b2b2d;
  font-size: 12px;
}
.asset-editor-drawer > footer button:hover:not(:disabled) { color: #fff; background: #343436; }
.asset-editor-drawer > footer .asset-editor-save {
  border-color: #ededee;
  color: #18181a;
  background: #ededee;
  font-weight: 600;
}
.asset-editor-drawer > footer .asset-editor-save:hover:not(:disabled) { color: #111; background: #fff; }
.asset-editor-drawer button:disabled { opacity: .45; cursor: default; }
.description-editor-dismiss { position: fixed; z-index: 8; inset: 0; background: transparent; }
.description-editor-popover { position: fixed; z-index: 9; overflow: hidden; border: 1px solid #4b4b4f; border-radius: 12px; background: #171718; box-shadow: 0 22px 54px rgba(0,0,0,.66); }
.description-rich-editor { height: 172px; position: relative; margin: 14px 14px 0; overflow: hidden; border-radius: 7px; background: #252526; }
.description-editor-popover > footer { min-height: 58px; display: flex; align-items: center; justify-content: space-between; gap: 16px; padding: 0 16px 0 20px; color: #85858a; font-size: 12px; }.description-editor-popover > footer button { min-width: 96px; min-height: 40px; border: 0; border-radius: 8px; color: #1b1b1d; background: #f1f1f2; font-size: 14px; font-weight: 600; }.description-editor-popover > footer button:hover { background: #fff; }.description-editor-popover > footer button:disabled { opacity: .42; cursor: default; }
.shots-step.prompts-step { grid-template-rows: minmax(0, 1fr); gap: 0; padding: 0 20px 10px; }
.prompt-stage-table .shot-prompt-column.is-active { color: #9ce8f1; background: #173137; }
.prompt-stage-table .shot-table tbody tr { background: #171718; }
.prompt-stage-table .shot-table tbody tr:hover { background: #1a1a1c; }
.shot-readonly { display: block; max-height: 58px; overflow: hidden; margin: 0; color: #cfcfd2; line-height: 1.5; overflow-wrap: anywhere; }
.shot-duration-readonly { text-align: center; white-space: nowrap; }
.shot-description-readonly { cursor: default; }
.shot-description-readonly:hover { background: transparent; }
.shot-final-prompt button { display: inline-flex; align-items: center; justify-content: center; gap: 5px; }
.shot-final-prompt button.status-ready { color: #c9c9cc; }
.shot-final-prompt button.status-composing { color: #5ed4e3; }
.shot-final-prompt button.status-stale { color: #d8a354; }
.shot-final-prompt button.status-failed { color: #e27078; }
.prompt-batch-backdrop { position: absolute; z-index: 10; inset: 0; background: rgba(0, 0, 0, .82); }
.prompt-batch-dialog {
  width: min(1560px, calc(100% - 48px));
  height: min(900px, calc(100% - 40px));
  position: absolute;
  z-index: 11;
  left: 50%;
  top: 50%;
  display: grid;
  grid-template-rows: 86px minmax(0, 1fr) 96px;
  overflow: hidden;
  border: 1px solid #3c3c3f;
  border-radius: 12px;
  color: #dedee0;
  background: #19191a;
  box-shadow: 0 34px 100px rgba(0, 0, 0, .82);
  transform: translate(-50%, -50%);
}
.prompt-batch-dialog > header { display: flex; align-items: center; justify-content: space-between; padding: 0 32px; border-bottom: 1px solid #343436; }
.prompt-batch-dialog > header > div { display: flex; align-items: center; gap: 10px; }
.prompt-batch-dialog > header h2 { margin: 0; color: #f2f2f3; font-size: 24px; font-weight: 620; }
.prompt-batch-dialog > header svg { color: #8d8d92; }
.prompt-batch-dialog > header button { width: 42px; height: 42px; display: grid; place-items: center; border: 0; border-radius: 6px; color: #a4a4a8; background: transparent; }
.prompt-batch-dialog > header button:hover:not(:disabled) { color: #fff; background: #29292b; }
.prompt-batch-list { min-height: 0; overflow: auto; padding: 34px; scrollbar-color: #8c9299 transparent; }
.prompt-batch-list article { min-height: 82px; display: grid; grid-template-columns: 28px 92px minmax(0, 1fr) 92px 70px; align-items: center; gap: 16px; border: 1px solid #3b3b3e; border-bottom: 0; padding: 0 22px; color: #b6b6ba; background: #1d1d1e; cursor: pointer; }
.prompt-batch-list article:first-child { border-radius: 9px 9px 0 0; }
.prompt-batch-list article:last-child { border-bottom: 1px solid #3b3b3e; border-radius: 0 0 9px 9px; }
.prompt-batch-list article:only-child { border-radius: 9px; }
.prompt-batch-list article.selected { background: #202021; }
.prompt-batch-list article > input { width: 24px; height: 24px; margin: 0; accent-color: #f0f0f1; cursor: pointer; }
.prompt-batch-list article > strong { color: #efeff0; font-size: 15px; font-weight: 560; white-space: nowrap; }
.prompt-batch-list article > p { min-width: 0; overflow: hidden; margin: 0; color: #9b9b9f; font-size: 14px; line-height: 1.6; text-overflow: ellipsis; white-space: nowrap; }
.prompt-batch-list article > span { justify-self: end; border-radius: 4px; padding: 5px 8px; color: #838388; background: #29292b; font-size: 10px; white-space: nowrap; }
.prompt-batch-list article > span.status-ready { color: #50c9a8; background: rgba(72, 201, 164, .09); }
.prompt-batch-list article > span.status-stale { color: #d6a15b; background: rgba(214, 161, 91, .09); }
.prompt-batch-list article > span.status-failed { color: #e4777e; background: rgba(228, 119, 126, .09); }
.prompt-batch-list article > button { border: 0; padding: 10px 0; color: #a8a8ac; background: transparent; font-size: 14px; }
.prompt-batch-list article > button:hover { color: #fff; }
.prompt-batch-dialog > footer { min-width: 0; display: flex; align-items: center; gap: 26px; padding: 0 30px; border-top: 1px solid #343436; background: #171718; }
.prompt-batch-select-all, .prompt-batch-model, .prompt-batch-modes label { display: inline-flex; align-items: center; gap: 9px; white-space: nowrap; }
.prompt-batch-select-all { color: #e1e1e3; font-size: 14px; }
.prompt-batch-select-all input { width: 23px; height: 23px; accent-color: #f0f0f1; }
.prompt-batch-select-all small { color: #77777c; font-size: 12px; }
.prompt-batch-model { min-width: 0; color: #ededee; }
.prompt-batch-model select { max-width: 230px; border: 0; outline: 0; color: #dedee0; background: transparent; font-size: 14px; }
.prompt-batch-model select option { color: #222; }
.prompt-batch-modes { display: flex; align-items: center; gap: 18px; margin-left: auto; color: #a4a4a8; font-size: 13px; }
.prompt-batch-modes input { width: 18px; height: 18px; accent-color: #ededee; }
.prompt-batch-credits { display: inline-flex; align-items: center; gap: 6px; color: #a1a1a5; font-size: 13px; }
.prompt-batch-submit { min-height: 50px; display: inline-flex; align-items: center; justify-content: center; gap: 8px; border: 1px solid #f0f0f1; border-radius: 8px; padding: 0 22px; color: #181819; background: #f0f0f1; font-size: 14px; font-weight: 620; white-space: nowrap; }
.prompt-batch-submit:hover:not(:disabled) { background: #fff; }
.prompt-batch-submit:disabled { opacity: .38; cursor: not-allowed; }
.prompt-detail-backdrop { position: absolute; z-index: 20; inset: 0; background: rgba(0, 0, 0, .76); }
.prompt-detail-dialog {
  width: min(820px, calc(100% - 40px));
  height: min(760px, calc(100% - 56px));
  position: absolute;
  z-index: 21;
  left: 50%;
  top: 50%;
  display: grid;
  grid-template-rows: 68px minmax(0, 1fr) 74px;
  overflow: hidden;
  border: 1px solid #414144;
  border-radius: 8px;
  color: #dedee0;
  background: #181819;
  box-shadow: 0 30px 86px rgba(0, 0, 0, .76);
  transform: translate(-50%, -50%);
}
.prompt-detail-dialog > header { display: flex; align-items: center; justify-content: space-between; padding: 0 20px; border-bottom: 1px solid #333335; }
.prompt-detail-dialog > header h2 { margin: 0; color: #eeeeef; font-size: 17px; font-weight: 620; }
.prompt-detail-dialog > header button { width: 34px; height: 34px; display: grid; place-items: center; border: 0; border-radius: 4px; color: #99999e; background: transparent; }
.prompt-detail-dialog > header button:hover { color: #fff; background: #28282a; }
.prompt-detail-body { min-height: 0; display: grid; grid-template-rows: minmax(180px, .8fr) minmax(240px, 1.2fr); gap: 16px; overflow: auto; padding: 18px 20px; }
.prompt-editor-section { min-height: 0; display: grid; grid-template-rows: 32px minmax(0, 1fr); gap: 8px; }
.prompt-editor-section > header { display: flex; align-items: center; justify-content: space-between; gap: 12px; }
.prompt-editor-section > header > div { display: flex; align-items: center; gap: 9px; }
.prompt-editor-section > header strong { color: #d8d8db; font-size: 12px; font-weight: 560; }
.prompt-editor-section > header span { min-height: 22px; display: inline-flex; align-items: center; gap: 4px; border-radius: 4px; padding: 0 7px; color: #8d8d92; background: #272729; font-size: 9px; }
.prompt-editor-section > header span.status-ready { color: #48c9a4; background: rgba(72, 201, 164, .1); }
.prompt-editor-section > header span.status-stale { color: #d7a052; background: rgba(215, 160, 82, .1); }
.prompt-editor-section > header span.status-failed { color: #e27078; background: rgba(226, 112, 120, .1); }
.prompt-editor-section > header small { color: #747479; font-size: 9px; }
/* 提示词框与画面描述共用富文本编辑器：@实体要高亮、要能唤出资产选择。
   这里只覆写外观，行为全在 ScriptDescriptionRichEditor 里。 */
.prompt-editor-section .prompt-rich-editor { min-height: 0; border: 1px solid #343437; border-radius: 7px; background: #111112; }
.prompt-editor-section .prompt-rich-editor:focus-within { border-color: #626267; background: #131314; }
.prompt-editor-section .prompt-rich-editor :deep(.script-description-editor-input) { padding: 14px 15px; color: #dedee0; font: 12px/1.7 inherit; scrollbar-width: thin; scrollbar-color: #4a4a4e transparent; }
.prompt-editor-section .prompt-rich-editor :deep(.script-description-editor-input)::-webkit-scrollbar { width: 6px; }
.prompt-editor-section .prompt-rich-editor :deep(.script-description-editor-input)::-webkit-scrollbar-thumb { border-radius: 3px; background: #4a4a4e; }
.prompt-detail-dialog > footer { min-width: 0; display: flex; align-items: center; gap: 10px; padding: 0 18px; border-top: 1px solid #333335; background: #151516; }
.prompt-model-select { min-width: 0; display: inline-flex; align-items: center; gap: 7px; color: #ededee; }
.prompt-model-select select { max-width: 190px; border: 0; outline: 0; color: #dedee0; background: transparent; font-size: 11px; }
.prompt-model-select option { color: #222; }
.prompt-compose-options { display: flex; align-items: center; gap: 10px; margin-left: auto; color: #8d8d92; font-size: 10px; }
.prompt-compose-options label, .prompt-compose-options span { display: inline-flex; align-items: center; gap: 5px; white-space: nowrap; }
.prompt-compose-options input { accent-color: #dcdcdf; }
.prompt-compose-options span { color: #9a9a9f; }
.prompt-save-button, .prompt-compose-button { min-height: 38px; display: inline-flex; align-items: center; justify-content: center; gap: 6px; border: 1px solid #444448; border-radius: 6px; padding: 0 13px; color: #c9c9cc; background: #28282a; font-size: 11px; }
.prompt-compose-button { border-color: #ededee; color: #171719; background: #ededee; font-weight: 600; }
.prompt-save-button:hover:not(:disabled) { color: #fff; background: #333335; }
.prompt-compose-button:hover:not(:disabled) { background: #fff; }
.prompt-save-button:disabled, .prompt-compose-button:disabled { opacity: .42; cursor: wait; }
.script-batch-dialog { width: min(980px, calc(100% - 80px)); max-height: min(760px, calc(100% - 90px)); position: absolute; z-index: 3; left: 50%; top: 50%; display: grid; grid-template-rows: 76px auto minmax(0,1fr) auto; overflow: hidden; border: 1px solid rgba(255,255,255,.18); border-radius: 9px; background: #1c1d20; box-shadow: 0 30px 90px rgba(0,0,0,.72); transform: translate(-50%, -50%); }
.batch-dialog-backdrop { position: absolute; z-index: 2; inset: 0; background: rgba(4,5,6,.72); }
.script-batch-dialog > header { display: flex; align-items: center; justify-content: space-between; padding: 0 22px; border-bottom: 1px solid rgba(255,255,255,.1); background: #18191c; }.script-batch-dialog > header span { color: #48c5d5; font-size: 8px; letter-spacing: .14em; }.script-batch-dialog > header h2 { margin: 5px 0 0; font-size: 18px; }.script-batch-dialog > header button { width: 30px; height: 30px; display: grid; place-items: center; border: 0; color: #a8acb2; background: transparent; }
.batch-dialog-note { margin: 14px 20px 8px; border-radius: 6px; padding: 11px 13px; color: #a9adb4 !important; background: #25262a; font-size: 10px !important; }.batch-shot-list { min-height: 0; display: grid; align-content: start; gap: 7px; overflow: auto; padding: 8px 20px 16px; }.batch-shot-list article { display: grid; grid-template-columns: 20px minmax(0,1fr) 90px; align-items: center; gap: 11px; border: 1px solid rgba(255,255,255,.1); border-radius: 7px; padding: 11px 12px; color: #c9ccd0; background: #202125; cursor: pointer; }.batch-shot-list article.selected { border-color: rgba(53,194,210,.55); background: rgba(53,194,210,.07); }.batch-shot-list article > input { accent-color: #35c2d2; }.batch-shot-list article strong { font-size: 11px; }.batch-shot-list article p { overflow: hidden; margin: 5px 0 3px; color: #8e9299; font-size: 9px; line-height: 1.4; text-overflow: ellipsis; white-space: nowrap; }.batch-shot-list article small { color: #656a72; font-size: 8px; }.batch-shot-list article > label { display: inline-flex; align-items: center; gap: 4px; color: #8c9098; font-size: 9px; }.batch-shot-list article > label input { width: 50px; border: 1px solid rgba(255,255,255,.13); border-radius: 4px; outline: 0; padding: 5px; color: #e2e4e5; background: #292a2e; }
.script-batch-dialog > footer { display: flex; align-items: center; gap: 10px; flex-wrap: wrap; padding: 13px 20px; border-top: 1px solid rgba(255,255,255,.1); background: #18191c; }.script-batch-dialog > footer label { display: inline-flex; align-items: center; gap: 6px; color: #83878f; font-size: 9px; }.script-batch-dialog > footer select { min-height: 31px; border: 1px solid rgba(255,255,255,.13); border-radius: 5px; outline: 0; padding: 0 8px; color: #d4d7da; background: #28292d; font-size: 9px; }.script-batch-dialog > footer select option { color: #222; }.script-batch-dialog > footer button { min-height: 35px; display: inline-flex; align-items: center; gap: 6px; margin-left: auto; border: 1px solid #eef0f1; border-radius: 6px; padding: 0 15px; color: #161719; background: #eef0f1; font-size: 10px; font-weight: 600; }.script-batch-dialog > footer button:disabled { opacity: .45; }.batch-audio input { accent-color: #35c2d2; }
@media (max-width: 1280px) {
  .script-studio-overlay { padding: 0; }
  .script-studio-shell { width: 100%; height: 100%; border-radius: 0; }
  .script-step-bar button { min-width: 145px; }
  .script-content-toolbar { align-items: start; flex-direction: column; }
  .prompt-batch-bar { align-items: start; flex-wrap: wrap; padding: 8px 12px; }
  .prompt-batch-actions { margin-left: 0; }
  .prompt-batch-dialog { width: calc(100% - 24px); height: calc(100% - 24px); grid-template-rows: 72px minmax(0, 1fr) auto; }
  .prompt-batch-list { padding: 20px; }
  .prompt-batch-list article { grid-template-columns: 24px 78px minmax(0, 1fr) 82px 56px; gap: 12px; padding-inline: 16px; }
  .prompt-batch-dialog > footer { flex-wrap: wrap; gap: 12px 20px; padding: 14px 20px; }
  .prompt-batch-modes { margin-left: 0; }
  .prompt-batch-submit { margin-left: auto; }
  .asset-generation-dialog > footer { gap: 14px; padding-inline: 18px; }
  .asset-generation-control select { max-width: 150px; font-size: 13px; }
}
@media (max-width: 720px) {
  .assets-step { padding-inline: 12px; }
  .script-asset-sections { padding-right: 4px; }
  .script-asset-item { width: min(296px, calc(100vw - 28px)); }
  .script-asset-add { width: min(166px, calc(50vw - 20px)); }
  .asset-studio-footer { gap: 10px; padding-inline: 12px; }
  .asset-footer-notice span { white-space: normal; }
  .asset-editor-drawer { width: 100%; min-width: 0; }
  .asset-editor-body { padding-inline: 14px; }
  .asset-generation-dialog { inset: 0; grid-template-rows: 70px minmax(0, 1fr) auto; border: 0; border-radius: 0; }
  .asset-generation-dialog > header { padding: 0 16px; }
  .asset-generation-dialog > header h2 { font-size: 20px; }
  .asset-generation-list { gap: 22px; padding: 8px 12px 24px; }
  .asset-generation-section article { padding: 12px; }
  .asset-generation-section textarea { width: 100%; margin-left: 0; }
  .asset-generation-dialog > footer { flex-wrap: wrap; gap: 12px 16px; overflow: visible; padding: 14px 16px; }
  .asset-generation-credits { margin-left: 0; }
  .asset-generation-submit { min-height: 44px; margin-left: auto; }
  .prompt-detail-dialog { width: 100%; height: 100%; border: 0; border-radius: 0; }
  .prompt-detail-dialog > footer { align-items: stretch; flex-wrap: wrap; padding-block: 10px; }
  .prompt-compose-options { order: 3; width: 100%; margin-left: 0; }
  .prompt-batch-dialog { inset: 0; width: 100%; height: 100%; border: 0; border-radius: 0; transform: none; }
  .prompt-batch-dialog > header { padding-inline: 16px; }
  .prompt-batch-dialog > header h2 { font-size: 19px; }
  .prompt-batch-list { padding: 12px; }
  .prompt-batch-list article { min-height: 92px; grid-template-columns: 24px minmax(0, 1fr) 56px; grid-template-areas: "check title detail" "check copy copy" "check status status"; gap: 5px 10px; padding: 12px; }
  .prompt-batch-list article > input { grid-area: check; }
  .prompt-batch-list article > strong { grid-area: title; }
  .prompt-batch-list article > p { grid-area: copy; }
  .prompt-batch-list article > span { grid-area: status; justify-self: start; }
  .prompt-batch-list article > button { grid-area: detail; }
  .prompt-batch-dialog > footer { align-items: stretch; gap: 10px 14px; padding: 12px; }
  .prompt-batch-select-all { width: 100%; }
  .prompt-batch-model { flex: 1; }
  .prompt-batch-model select { width: 100%; }
  .prompt-batch-modes { order: 3; width: 100%; }
  .prompt-batch-credits { margin-left: auto; }
  .prompt-batch-submit { min-height: 44px; }
}
@media (prefers-reduced-motion: reduce) {
  .script-asset-card.generating::before,
  .script-asset-progress.indeterminate .script-asset-progress-fill,
  .script-asset-generating .spin { animation: none; }
}
</style>
