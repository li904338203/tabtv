<script setup lang="ts">
/**
 * 新建 / 编辑角色（MKT-T-39，营销 dev/05 §4.4、§5；MKT-F-103~108、MKT-UX-15/17，对标截图 63，**去掉可见范围**）。
 *
 * - 左：大图预览 + 「添加图片 / 设为主图 / 删除」+「n / 9」+ 缩略图条（首张即主图）；候选图区（生成结果，点选加入，满 9 张禁用）。
 *   上传处写明「请上传非真人形象…」（MKT-PARAM-25，不做自动识别）。
 * - 下：形象描述 + 画风（`/canvas/styles`，可不选）+ 参考图（上传）+ 模型（`/models?resultType=image`）+ 数量 +
 *   「AI 帮写」（`OptimizePromptButton purpose="character_appearance"`，与创造页一键优化同价同链路）+
 *   「生成角色 ✦N」—— N 来自 `POST /marketing/characters/generate {dryRun:true}`，输入变了 300ms 防抖重报。
 * - 右：角色名称（即时校验，禁用字符表取配置；重名 409 标红）、音色描述、上传音频（单段、可播放）+ 如实说明（MK-25）。
 * - 保存：新建 `POST` / 编辑 `PUT` 带 `version`；40902 说清是谁改的并保留输入。
 *
 * 生成的跟踪：受理回 `generationId` → 轮询 `GET /generations/{id}`（终态即停；耗尽只说「仍在处理」，GAP-14）+
 * 收 `generation.update` 事件立刻对账（事件只是信号，按 generationId 认，不按 projectId —— 这类任务的 projectId 是 "0"）。
 * 候选记 sessionStorage 24 小时（按角色分组），关掉再开仍可添加（MKT-UX-15：已扣费的结果不悄悄丢）。
 *
 * 规则全在 `utils/marketingLibrary.ts`（有单测）。
 * ⚠️ 打开只发 GET（详情、模型、画风、余额、已记录的生成状态）；报价（dryRun）只在写了描述之后才发，不扣费。
 * ⚠️ 本地选的文件用 object URL 预览，已有的受保护媒体走 `useAuthenticatedMedia` —— 两种都要释放。
 */
import { ElMessage, ElMessageBox } from "element-plus";
import { Check, ImagePlus, LoaderCircle, Music, Plus, RotateCw, Sparkles, Star, Trash2, TriangleAlert, Undo2, Upload, UserRound, X } from "@lucide/vue";
import OptimizePromptButton from "../OptimizePromptButton.vue";
import { mediaThumbUrl } from "~/utils/authorizedMedia";
import { apiFailure } from "~/utils/agentPanelBridge";
import { isTerminalStatus } from "~/utils/asyncState";
import { creditBalanceOf } from "~/utils/accountPanel";
import type { OptimizePriceView } from "~/utils/composerPrice";
import { workspaceTopic } from "~/utils/realtime";
import {
  MARKETING_CHARACTER_UPLOAD_NOTE,
  MARKETING_VOICE_NOTE,
  marketingAddCharGen,
  marketingAddImages,
  marketingAudioAllowed,
  marketingCandidatesOf,
  marketingCharacterCloseConfirmText,
  marketingCharacterFormIssue,
  marketingCharacterFormOf,
  marketingCharacterGenGate,
  marketingCharacterGenKey,
  marketingCharacterGenRequest,
  marketingCharacterInput,
  marketingCharacterNameFailure,
  marketingCharacterNameHint,
  marketingCharacterNameIssue,
  marketingClampGenCount,
  marketingConflictText,
  marketingDismissCandidate,
  marketingEmptyCharacterForm,
  marketingGenStatusText,
  marketingImageAccept,
  marketingImageAllowed,
  marketingImageGate,
  marketingImageHint,
  marketingIsQuotaError,
  marketingPatchImage,
  marketingReadCharGen,
  marketingRemoveImage,
  marketingRescopeCharGen,
  marketingSetMainImage,
  marketingTextCheck,
  marketingVisibleCandidates,
  marketingVoiceGate,
  marketingWriteCharGen,
  MARKETING_SEARCH_DEBOUNCE_MS,
  type MarketingCandidate,
  type MarketingCharacter,
  type MarketingCharacterForm,
  type MarketingCharacterGenAccepted,
  type MarketingCharacterGenQuote,
  type MarketingCharacterLimits,
  type MarketingCharGenEntry,
  type MarketingGenPhase,
  type MarketingImageItem
} from "~/utils/marketingLibrary";

const props = withDefaults(defineProps<{
  /** 编辑哪个角色；不传 = 新建。 */
  characterId?: string;
  limits: MarketingCharacterLimits;
  readOnly?: boolean;
  /** AI 帮写的价格（与创造页一键优化同一个 `optimizePriceView`）。 */
  optimizePrice: OptimizePriceView;
  /** AI 帮写用的推理模型（与 Agent 对话同一个）。 */
  optimizeModelCode?: string;
  /** 默认图片模型（Agent 偏好里的图片模型）；不在列表里就用第一个。 */
  defaultModelCode?: string;
}>(), { characterId: "", readOnly: false, optimizeModelCode: undefined, defaultModelCode: "" });

const emit = defineEmits<{
  close: [];
  saved: [character: MarketingCharacter, created: boolean];
  "quota-full": [];
}>();

type ImageModel = { code: string; name?: string };
type GenerationView = { id: string; status: string; outputs?: Array<{ index?: number; type?: string; mediaId?: string | null }> };
type GenTrack = { phase: MarketingGenPhase; candidates: MarketingCandidate[] };

const POLL_MS = 2500;
const POLL_MAX = 150;

const referenceUpload = useReferenceUpload();
const thumbs = useAuthenticatedMedia();
const configStore = useCanvasConfigStore();
const workspace = useWorkspaceStore();
const realtime = useRealtimeStore();
const isEdit = computed(() => Boolean(props.characterId));
const scope = ref(props.characterId || "new");

const form = ref<MarketingCharacterForm>(marketingEmptyCharacterForm());
const original = ref<MarketingCharacterForm>(marketingEmptyCharacterForm());
const version = ref(0);
const loading = ref(false);
const loadError = ref("");
const saving = ref(false);
const saveError = ref("");
const nameServerError = ref("");
const conflict = ref("");
const notice = ref("");
const current = ref(0);
const fileInput = ref<HTMLInputElement>();
const refInput = ref<HTMLInputElement>();
const voiceInput = ref<HTMLInputElement>();
/** localId → 本地预览 URL（刚选的图片 / 参考图 / 音频）。 */
const previews = ref<Record<string, string>>({});
/** localId → 文件（重试用）。 */
const files = new Map<string, File>();

// 生成
const styles = ref<Array<{ code?: string; name?: string }>>([]);
const models = ref<ImageModel[]>([]);
const modelsError = ref("");
const modelCode = ref("");
const count = ref(1);
const references = ref<MarketingImageItem[]>([]);
const quote = ref<MarketingCharacterGenQuote | null>(null);
const quoting = ref(false);
const quoteError = ref("");
const generating = ref(false);
const genError = ref("");
const balance = ref<number | undefined>();
const entries = ref<MarketingCharGenEntry[]>([]);
const tracks = ref<Record<string, GenTrack>>({});
/** AI 帮写替换前的原文（可撤销一次，MKT-F-108）。 */
const undoAppearance = ref<string | null>(null);

const imageGate = computed(() => marketingImageGate(form.value.images, props.limits.maxImages));
const issue = computed(() => marketingCharacterFormIssue(form.value, props.limits, props.readOnly));
const nameIssue = computed(() => (form.value.name ? marketingCharacterNameIssue(form.value.name, props.limits) : ""));
const nameError = computed(() => nameServerError.value || nameIssue.value);
const appearanceCheck = computed(() => marketingTextCheck(form.value.appearance, props.limits.appearance, "形象描述"));
const voiceTextCheck = computed(() => marketingTextCheck(form.value.voiceText, props.limits.voiceText, "音色描述"));
const voiceGate = computed(() => marketingVoiceGate(form.value.voice));
const canSave = computed(() => !issue.value && !nameServerError.value && !saving.value && !loading.value && !loadError.value);
const currentImage = computed(() => form.value.images[current.value]);
const doneRefs = computed(() => references.value.filter(item => item.status === "done" && item.mediaId).map(item => item.mediaId!));
const genInput = computed(() => ({
  modelCode: modelCode.value,
  description: form.value.appearance,
  styleCode: form.value.styleCode,
  referenceMediaIds: doneRefs.value,
  count: count.value
}));
const genKey = computed(() => marketingCharacterGenKey(genInput.value, props.limits.genMax));
const scopedEntries = computed(() => entries.value.filter(item => item.scope === scope.value));
const isGenerating = computed(() => generating.value || scopedEntries.value.some(item => {
  const phase = tracks.value[item.generationId]?.phase;
  return !phase || phase === "queued" || phase === "running";
}));
const candidates = computed(() => marketingVisibleCandidates(
  scopedEntries.value.flatMap(item => tracks.value[item.generationId]?.candidates ?? []),
  form.value.images.map(item => item.mediaId),
  scopedEntries.value.flatMap(item => item.dismissed)
));
const genGate = computed(() => marketingCharacterGenGate({
  readOnly: props.readOnly,
  generating: generating.value,
  description: form.value.appearance,
  appearanceLimit: props.limits.appearance,
  modelCode: modelCode.value,
  uploadingRefs: references.value.filter(item => item.status === "uploading").length,
  quoting: quoting.value,
  quoteError: quoteError.value,
  quote: quote.value,
  balance: balance.value
}));
const optimizeBlock = computed(() => (props.readOnly ? "你在该工作区是只读，不能使用 AI 帮写" : ""));
/** 最近一条生成的状态（列在候选区标题旁）。 */
const latestStatus = computed(() => {
  const last = scopedEntries.value[scopedEntries.value.length - 1];
  if (!last) return "";
  const track = tracks.value[last.generationId];
  if (!track) return marketingGenStatusText("queued");
  return marketingGenStatusText(track.phase, track.candidates.length);
});
const genCounts = computed(() => Array.from({ length: props.limits.genMax }, (_, i) => i + 1));

function localId(): string {
  return `c_${Date.now().toString(36)}_${Math.random().toString(36).slice(2, 8)}`;
}

// ---------------------------------------------------------------- 缩略图

function loadThumbs(ids: readonly (string | undefined)[], width: 320 | 640 = 640) {
  const wanted = ids.filter((id): id is string => Boolean(id)).map(id => ({ key: `${width}:${id}`, url: mediaThumbUrl(id, width) }));
  if (wanted.length) void thumbs.appendAll(wanted);
}

function imageSrc(item: MarketingImageItem | undefined, width: 320 | 640 = 640): string | undefined {
  if (!item) return undefined;
  return previews.value[item.localId] ?? (item.mediaId ? thumbs.urls.value[`${width}:${item.mediaId}`] : undefined);
}

function candidateSrc(item: MarketingCandidate): string | undefined {
  return thumbs.urls.value[`320:${item.mediaId}`];
}

const voiceSrc = computed(() => {
  const voice = form.value.voice;
  if (!voice) return undefined;
  return previews.value[voice.localId] ?? (voice.mediaId ? thumbs.urls.value[`voice:${voice.mediaId}`] : undefined);
});

watch(() => form.value.images.map(item => item.mediaId).join(","), () => {
  loadThumbs(form.value.images.filter(item => !previews.value[item.localId]).map(item => item.mediaId));
  // 缩略图条也要一份（同一档就够，640 在条里缩着显示）
});
watch(candidates, list => loadThumbs(list.map(item => item.mediaId), 320));
watch(() => form.value.voice?.mediaId, id => {
  if (id && form.value.voice && !previews.value[form.value.voice.localId]) void thumbs.appendAll([{ key: `voice:${id}`, url: `/media/${id}` }]);
});

// ---------------------------------------------------------------- 加载

function applyCharacter(character: MarketingCharacter) {
  const next = marketingCharacterFormOf(character, localId);
  form.value = next;
  original.value = { ...next, images: [...next.images] };
  version.value = character.version;
  current.value = 0;
  nameServerError.value = "";
}

async function load() {
  if (!props.characterId) return;
  loading.value = true;
  loadError.value = "";
  try {
    applyCharacter(await useApi()<MarketingCharacter>(`/marketing/characters/${encodeURIComponent(props.characterId)}`));
  } catch (cause) {
    const failure = apiFailure(cause);
    loadError.value = !navigator.onLine ? "网络已断开，恢复后再试" : ((cause as { statusCode?: number }).statusCode === 404 ? "这个角色已不存在" : failure.message);
  } finally {
    loading.value = false;
  }
}

async function loadModels() {
  try {
    const list = await useApi()<ImageModel[]>("/models", { query: { resultType: "image" } });
    models.value = (list ?? []).filter(item => item.code);
    modelCode.value = models.value.some(item => item.code === props.defaultModelCode) ? props.defaultModelCode : models.value[0]?.code ?? "";
    modelsError.value = models.value.length ? "" : "还没有可用的图片模型";
  } catch (cause) {
    modelsError.value = !navigator.onLine ? "网络已断开" : apiFailure(cause).message || "图片模型加载失败";
  }
}

async function loadBalance() {
  try {
    // 契约是 `{ balance, grants }`（此前读老结构的 total，恒为 undefined）
    balance.value = creditBalanceOf(await useApi()<unknown>("/credits"));
  } catch {
    balance.value = undefined;
  }
}

// ---------------------------------------------------------------- 图片

function pickImages() {
  if (props.readOnly || !imageGate.value.ok) return;
  fileInput.value?.click();
}

function onImages(event: Event) {
  const input = event.target as HTMLInputElement;
  const picked = Array.from(input.files ?? []);
  input.value = "";
  const allowed: Array<{ item: MarketingImageItem; file: File }> = [];
  const refused: string[] = [];
  for (const file of picked) {
    if (!marketingImageAllowed(file.type, props.limits.mimes)) refused.push(file.name);
    else allowed.push({ item: { localId: localId(), status: "uploading", name: file.name }, file });
  }
  const result = marketingAddImages(form.value.images, allowed.map(entry => entry.item), props.limits.maxImages);
  notice.value = [
    refused.length ? `${refused.join("、")}：${marketingImageHint({ mimes: props.limits.mimes, maxImages: props.limits.maxImages }).split("，")[0]}` : "",
    result.notice
  ].filter(Boolean).join("；");
  const firstNew = form.value.images.length;
  form.value = { ...form.value, images: result.items };
  if (result.added.length) current.value = firstNew;
  const added = new Set(result.added.map(item => item.localId));
  for (const { item, file } of allowed) {
    if (!added.has(item.localId)) continue;
    files.set(item.localId, file);
    previews.value = { ...previews.value, [item.localId]: URL.createObjectURL(file) };
    void uploadImage(item.localId);
  }
}

async function uploadImage(id: string) {
  const file = files.get(id);
  if (!file) return;
  form.value = { ...form.value, images: marketingPatchImage(form.value.images, id, { status: "uploading", error: undefined }) };
  try {
    const mediaId = await referenceUpload.upload(file, { mediaType: "image" });
    form.value = { ...form.value, images: marketingPatchImage(form.value.images, id, { status: "done", mediaId, error: undefined }) };
  } catch (cause) {
    form.value = { ...form.value, images: marketingPatchImage(form.value.images, id, { status: "failed", error: uploadErrorText(cause) }) };
  }
}

function uploadErrorText(cause: unknown): string {
  return !navigator.onLine ? "网络已断开，恢复后点重试" : (cause as { message?: string })?.message || "上传没有成功，请重试";
}

function releasePreview(id: string) {
  files.delete(id);
  const url = previews.value[id];
  if (!url) return;
  URL.revokeObjectURL(url);
  const { [id]: _drop, ...rest } = previews.value;
  previews.value = rest;
}

function removeCurrent() {
  const item = currentImage.value;
  if (props.readOnly || !item) return;
  form.value = { ...form.value, images: marketingRemoveImage(form.value.images, item.localId) };
  releasePreview(item.localId);
  current.value = Math.min(current.value, Math.max(0, form.value.images.length - 1));
}

function makeCurrentMain() {
  if (props.readOnly || current.value <= 0) return;
  form.value = { ...form.value, images: marketingSetMainImage(form.value.images, current.value) };
  current.value = 0;
}

function retryImage(item: MarketingImageItem) {
  if (files.has(item.localId)) void uploadImage(item.localId);
}

/** 候选图 → 角色图片（满 9 张不加）。 */
function addCandidate(item: MarketingCandidate) {
  if (props.readOnly || !imageGate.value.ok) return;
  const result = marketingAddImages(form.value.images, [{ localId: localId(), mediaId: item.mediaId, status: "done", name: "生成图" }], props.limits.maxImages);
  if (!result.added.length) return;
  form.value = { ...form.value, images: result.items };
  current.value = form.value.images.length - 1;
}

function dismissCandidate(item: MarketingCandidate) {
  entries.value = marketingDismissCandidate(entries.value, item.generationId, item.mediaId);
  persistEntries();
}

// ---------------------------------------------------------------- 参考图（生成用，不存进角色）

function pickReferences() {
  if (props.readOnly) return;
  refInput.value?.click();
}

function onReferences(event: Event) {
  const input = event.target as HTMLInputElement;
  const picked = Array.from(input.files ?? []).filter(file => marketingImageAllowed(file.type, props.limits.mimes));
  input.value = "";
  const result = marketingAddImages(references.value, picked.map(file => ({ localId: localId(), status: "uploading" as const, name: file.name })), props.limits.maxImages);
  if (result.notice) ElMessage.warning(`参考图${result.notice}`);
  references.value = result.items;
  result.added.forEach((item, i) => {
    const file = picked[i]!;
    files.set(item.localId, file);
    previews.value = { ...previews.value, [item.localId]: URL.createObjectURL(file) };
    void (async () => {
      try {
        const mediaId = await referenceUpload.upload(file, { mediaType: "image" });
        references.value = marketingPatchImage(references.value, item.localId, { status: "done", mediaId });
      } catch (cause) {
        references.value = marketingPatchImage(references.value, item.localId, { status: "failed", error: uploadErrorText(cause) });
      }
    })();
  });
}

function removeReference(item: MarketingImageItem) {
  references.value = marketingRemoveImage(references.value, item.localId);
  releasePreview(item.localId);
}

// ---------------------------------------------------------------- 音频（单段，只存不发，MK-25）

function pickVoice() {
  if (props.readOnly || !voiceGate.value.ok) return;
  voiceInput.value?.click();
}

function onVoice(event: Event) {
  const input = event.target as HTMLInputElement;
  const file = input.files?.[0];
  input.value = "";
  if (!file || form.value.voice) return;
  if (!marketingAudioAllowed(file.type)) {
    ElMessage.error(`${file.name}：只能上传音频（wav / mp3 / ogg）`);
    return;
  }
  const id = localId();
  files.set(id, file);
  previews.value = { ...previews.value, [id]: URL.createObjectURL(file) };
  form.value = { ...form.value, voice: { localId: id, status: "uploading", name: file.name } };
  void uploadVoice(id);
}

async function uploadVoice(id: string) {
  const file = files.get(id);
  if (!file) return;
  form.value = { ...form.value, voice: form.value.voice?.localId === id ? { ...form.value.voice, status: "uploading", error: undefined } : form.value.voice };
  try {
    const mediaId = await referenceUpload.upload(file, { mediaType: "audio" });
    if (form.value.voice?.localId === id) form.value = { ...form.value, voice: { ...form.value.voice, status: "done", mediaId } };
  } catch (cause) {
    if (form.value.voice?.localId === id) form.value = { ...form.value, voice: { ...form.value.voice, status: "failed", error: uploadErrorText(cause) } };
  }
}

function removeVoice() {
  if (props.readOnly || !form.value.voice) return;
  releasePreview(form.value.voice.localId);
  form.value = { ...form.value, voice: null };
}

// ---------------------------------------------------------------- AI 帮写

function onOptimized(text: string) {
  undoAppearance.value = form.value.appearance;
  form.value = { ...form.value, appearance: text };
  void loadBalance();
}

function undoOptimize() {
  if (undoAppearance.value === null) return;
  form.value = { ...form.value, appearance: undoAppearance.value };
  undoAppearance.value = null;
}

// ---------------------------------------------------------------- 报价（dryRun，300ms 防抖）

let quoteTimer: ReturnType<typeof setTimeout> | undefined;
let quoteSeq = 0;

async function requestQuote() {
  const seq = ++quoteSeq;
  quoting.value = true;
  quoteError.value = "";
  try {
    const result = await useApi()<MarketingCharacterGenQuote>("/marketing/characters/generate", {
      method: "POST",
      body: marketingCharacterGenRequest(genInput.value, props.limits.genMax, true)
    });
    if (seq !== quoteSeq) return;
    quote.value = result;
  } catch (cause) {
    if (seq !== quoteSeq) return;
    quote.value = null;
    quoteError.value = !navigator.onLine ? "网络已断开，恢复后自动重新报价" : `报价失败：${apiFailure(cause).message}`;
  } finally {
    if (seq === quoteSeq) quoting.value = false;
  }
}

watch(genKey, () => {
  clearTimeout(quoteTimer);
  quote.value = null;
  quoteError.value = "";
  const ready = form.value.appearance.trim() && modelCode.value
    && !references.value.some(item => item.status === "uploading")
    && !appearanceCheck.value.error;
  if (!ready) {
    quoteSeq += 1;
    quoting.value = false;
    return;
  }
  quoting.value = true;
  quoteTimer = setTimeout(() => void requestQuote(), MARKETING_SEARCH_DEBOUNCE_MS);
});

// ---------------------------------------------------------------- 生成 + 跟踪

function idempotencyKey(): string {
  return globalThis.crypto?.randomUUID?.() ?? `${Date.now().toString(36)}-${Math.random().toString(36).slice(2)}`;
}

async function generate() {
  if (genGate.value.disabled) return;
  generating.value = true;
  genError.value = "";
  try {
    const accepted = await useApi()<MarketingCharacterGenAccepted>("/marketing/characters/generate", {
      method: "POST",
      headers: { "Idempotency-Key": idempotencyKey() },
      body: marketingCharacterGenRequest(genInput.value, props.limits.genMax, false)
    });
    entries.value = marketingAddCharGen(entries.value, scope.value, String(accepted.generationId), Date.now());
    persistEntries();
    tracks.value = { ...tracks.value, [String(accepted.generationId)]: { phase: "queued", candidates: [] } };
    track(String(accepted.generationId));
  } catch (cause) {
    genError.value = !navigator.onLine ? "网络已断开，这次没有发出去" : apiFailure(cause).message;
  } finally {
    generating.value = false;
    void loadBalance();
  }
}

const timers = new Map<string, ReturnType<typeof setTimeout>>();
const attempts = new Map<string, number>();
const inflight = new Set<string>();
let releaseTopic: (() => void) | undefined;
let offEvent: (() => void) | undefined;
let disposed = false;

function persistEntries() {
  if (import.meta.client) marketingWriteCharGen(sessionStorage, entries.value);
}

/** 对一次账：取状态，到终态就停；成功把图片产物列为候选。 */
async function reconcile(id: string) {
  if (inflight.has(id) || disposed) return;
  inflight.add(id);
  try {
    const view = await useApi()<GenerationView>(`/generations/${encodeURIComponent(id)}`);
    if (disposed) return;
    const phase = (["queued", "running", "success", "failed", "cancelled"].includes(view.status) ? view.status : "running") as MarketingGenPhase;
    tracks.value = { ...tracks.value, [id]: { phase, candidates: phase === "success" ? marketingCandidatesOf(id, view.outputs ?? []) : [] } };
    if (isTerminalStatus(view.status)) {
      stopPolling(id);
      if (phase !== "success") void loadBalance();
    }
  } catch (cause) {
    // 任务查不到了（换了工作区 / 被清理）：不再跟踪。其余错误（断网）下一轮再试。
    if ((cause as { statusCode?: number }).statusCode === 404) {
      stopPolling(id);
      entries.value = entries.value.filter(item => item.generationId !== id);
      persistEntries();
    }
  } finally {
    inflight.delete(id);
  }
}

function stopPolling(id: string) {
  clearTimeout(timers.get(id));
  timers.delete(id);
}

function schedule(id: string) {
  const n = (attempts.get(id) ?? 0) + 1;
  attempts.set(id, n);
  if (n > POLL_MAX) {
    // ⚠️ 浏览器不再问了 ≠ 任务失败（GAP-14）：只说「仍在处理」。
    const currentTrack = tracks.value[id];
    if (!currentTrack || !isTerminalStatus(currentTrack.phase)) tracks.value = { ...tracks.value, [id]: { phase: "unknown", candidates: [] } };
    return;
  }
  timers.set(id, setTimeout(async () => {
    await reconcile(id);
    if (!disposed && timers.has(id)) schedule(id);
  }, POLL_MS));
}

function track(id: string) {
  ensureRealtime();
  void reconcile(id).then(() => {
    const phase = tracks.value[id]?.phase;
    if (!disposed && (!phase || !isTerminalStatus(phase))) schedule(id);
  });
}

/** 收 `generation.update` 当对账信号（只认自己在跟踪的 generationId）。 */
function ensureRealtime() {
  if (offEvent || !workspace.currentId) return;
  offEvent = realtime.on("generation.update", frame => {
    const id = typeof frame.generationId === "string" ? frame.generationId : "";
    if (id && timers.has(id)) void reconcile(id);
  });
  releaseTopic = realtime.subscribe(workspaceTopic(workspace.currentId));
  realtime.connect();
}

function restoreTracking() {
  if (!import.meta.client) return;
  entries.value = marketingReadCharGen(sessionStorage, Date.now());
  persistEntries();
  for (const item of scopedEntries.value) track(item.generationId);
}

// ---------------------------------------------------------------- 保存 / 重新加载 / 关闭

watch(() => form.value.name, () => {
  nameServerError.value = "";
});

async function save() {
  if (!canSave.value) return;
  saving.value = true;
  saveError.value = "";
  try {
    const body = marketingCharacterInput(form.value);
    const character = isEdit.value
      ? await useApi()<MarketingCharacter>(`/marketing/characters/${encodeURIComponent(props.characterId)}`, { method: "PUT", body: { ...body, version: version.value } })
      : await useApi()<MarketingCharacter>("/marketing/characters", { method: "POST", body });
    conflict.value = "";
    original.value = form.value;
    if (!isEdit.value) {
      entries.value = marketingRescopeCharGen(entries.value, "new", character.id);
      persistEntries();
      scope.value = character.id;
    }
    emit("saved", character, !isEdit.value);
  } catch (cause) {
    if (!navigator.onLine) {
      saveError.value = "网络已断开，恢复后再保存";
      return;
    }
    const failure = apiFailure(cause);
    if (failure.code === 40902) {
      conflict.value = marketingConflictText(failure.details, "角色");
      return;
    }
    if (marketingIsQuotaError(failure)) emit("quota-full");
    const nameFailure = marketingCharacterNameFailure(failure, form.value.name);
    if (nameFailure) nameServerError.value = nameFailure;
    else saveError.value = failure.message;
  } finally {
    saving.value = false;
  }
}

async function reload() {
  try {
    await ElMessageBox.confirm("重新加载会用最新内容替换你当前的输入。", "重新加载？", { confirmButtonText: "重新加载", cancelButtonText: "取消" });
  } catch {
    return;
  }
  conflict.value = "";
  for (const item of form.value.images) releasePreview(item.localId);
  if (form.value.voice) releasePreview(form.value.voice.localId);
  await load();
}

async function requestClose() {
  if (saving.value) return;
  const text = props.readOnly ? "" : marketingCharacterCloseConfirmText(form.value, original.value, {
    generating: isGenerating.value,
    pendingCandidates: candidates.value.length
  });
  if (text) {
    try {
      await ElMessageBox.confirm(text, "关闭新建角色？", { confirmButtonText: "关闭", cancelButtonText: "继续编辑" });
    } catch {
      return;
    }
  }
  emit("close");
}

function onKeydown(event: KeyboardEvent) {
  if (event.key !== "Escape") return;
  // 角色选择弹窗也在听 Esc：这里先处理并拦下，否则一次 Esc 把两个弹窗都关了。
  event.stopImmediatePropagation();
  void requestClose();
}

onMounted(async () => {
  window.addEventListener("keydown", onKeydown, true);
  void load();
  void loadModels();
  void loadBalance();
  restoreTracking();
  styles.value = await configStore.materials("style");
});

onBeforeUnmount(() => {
  disposed = true;
  window.removeEventListener("keydown", onKeydown, true);
  clearTimeout(quoteTimer);
  for (const id of [...timers.keys()]) stopPolling(id);
  offEvent?.();
  releaseTopic?.();
  if (releaseTopic && realtime.activeTopics().length === 0) realtime.disconnect();
  for (const url of Object.values(previews.value)) URL.revokeObjectURL(url);
  previews.value = {};
  files.clear();
});

watch(count, value => {
  const clamped = marketingClampGenCount(value, props.limits.genMax);
  if (clamped !== value) count.value = clamped;
});

defineExpose({ requestClose });
</script>

<template>
  <Teleport to="body">
    <div class="portal-overlay character-editor-overlay" @click.self="requestClose">
      <form class="character-editor" role="dialog" aria-modal="true" :aria-label="isEdit ? '编辑角色' : '新建角色'" @submit.prevent="save">
        <header class="ce-head">
          <h2>{{ isEdit ? "编辑角色" : "新建角色" }}</h2>
          <button type="button" class="ce-close" title="关闭（Esc）" aria-label="关闭" @click="requestClose"><X :size="15" /></button>
        </header>

        <p v-if="readOnly" class="ce-banner">你在该工作区是只读，不能新增、编辑角色或生成图片</p>
        <p v-if="loadError" class="ce-banner danger">{{ loadError }}</p>
        <div v-if="conflict" class="ce-banner warn" role="alert">
          <span>{{ conflict }}</span>
          <button type="button" class="ce-link" @click="reload">重新加载</button>
        </div>

        <div v-if="loading" class="ce-loading"><LoaderCircle :size="18" class="spin" />加载中…</div>
        <div v-else class="ce-body">
          <!-- 左：图片 + 生成 -->
          <section class="ce-left">
            <div class="ce-stage">
              <div class="ce-preview" :class="{ empty: !currentImage }">
                <img v-if="imageSrc(currentImage)" :src="imageSrc(currentImage)" :alt="form.name || '角色图片'" draggable="false">
                <UserRound v-else :size="64" class="ce-placeholder" />
                <span v-if="currentImage && current === 0" class="ce-main">主图</span>
                <span v-if="currentImage?.status === 'uploading'" class="ce-overlay"><LoaderCircle :size="22" class="spin" /></span>
                <button v-else-if="currentImage?.status === 'failed'" type="button" class="ce-overlay failed" :title="currentImage.error" @click="retryImage(currentImage)">
                  <TriangleAlert :size="16" /><RotateCw :size="16" />重新上传
                </button>
              </div>
              <div class="ce-tools">
                <input ref="fileInput" type="file" class="ce-file" multiple :accept="marketingImageAccept(limits.mimes)" tabindex="-1" @change="onImages">
                <button type="button" class="ce-pill" :disabled="readOnly || !imageGate.ok" :title="readOnly ? '你在该工作区是只读' : imageGate.reason || '添加图片'" @click="pickImages"><Plus :size="13" />添加图片</button>
                <button type="button" class="ce-pill" :disabled="readOnly || !currentImage || current === 0 || currentImage.status !== 'done'" @click="makeCurrentMain"><Star :size="13" />设为主图</button>
                <button type="button" class="ce-pill" :disabled="readOnly || !currentImage" @click="removeCurrent"><Trash2 :size="13" />删除</button>
                <span class="ce-count" :class="{ full: !imageGate.ok }">{{ form.images.length }} / {{ limits.maxImages }}</span>
              </div>
              <ul v-if="form.images.length" class="ce-strip" aria-label="角色图片">
                <li v-for="(item, index) in form.images" :key="item.localId">
                  <button type="button" class="ce-thumb" :class="[item.status, { on: index === current }]" :title="item.error || (index === 0 ? '主图' : `第 ${index + 1} 张`)" @click="current = index">
                    <img v-if="imageSrc(item)" :src="imageSrc(item)" alt="" draggable="false">
                    <LoaderCircle v-if="item.status === 'uploading'" :size="12" class="spin ce-thumb-icon" />
                    <TriangleAlert v-else-if="item.status === 'failed'" :size="12" class="ce-thumb-icon" />
                  </button>
                </li>
              </ul>
              <p class="ce-hint">{{ MARKETING_CHARACTER_UPLOAD_NOTE }}；{{ marketingImageHint({ mimes: limits.mimes, maxImages: limits.maxImages }) }}，第一张为主图</p>
              <p v-if="notice" class="ce-hint warn">{{ notice }}</p>
            </div>

            <div v-if="scopedEntries.length" class="ce-candidates">
              <div class="ce-candidates-head">
                <strong>生成结果</strong>
                <span>{{ latestStatus }}</span>
              </div>
              <ul v-if="candidates.length" class="ce-candidate-grid">
                <li v-for="item in candidates" :key="item.mediaId" class="ce-candidate">
                  <button type="button" :disabled="readOnly || !imageGate.ok" :title="imageGate.ok ? '加入角色图片' : imageGate.reason" @click="addCandidate(item)">
                    <img v-if="candidateSrc(item)" :src="candidateSrc(item)" alt="生成的候选图" draggable="false">
                    <span class="ce-candidate-add"><Check :size="12" />加入</span>
                  </button>
                  <button type="button" class="ce-candidate-dismiss" title="不要这张" aria-label="不要这张" @click="dismissCandidate(item)"><X :size="11" /></button>
                </li>
              </ul>
            </div>

            <div class="ce-gen">
              <textarea
                v-model="form.appearance"
                class="ce-appearance"
                rows="4"
                placeholder="输入角色的具体形象描述来生成图片，如五官、表情、服饰等"
                :disabled="readOnly"
                :aria-invalid="Boolean(appearanceCheck.error)"
                aria-label="形象描述"
              />
              <div class="ce-gen-meta">
                <small :class="{ over: appearanceCheck.error }">{{ appearanceCheck.counter }}</small>
                <button v-if="undoAppearance !== null" type="button" class="ce-link" @click="undoOptimize"><Undo2 :size="12" />撤销帮写</button>
              </div>
              <ul v-if="references.length" class="ce-refs" aria-label="参考图">
                <li v-for="item in references" :key="item.localId" class="ce-ref" :class="item.status" :title="item.error || item.name">
                  <img v-if="previews[item.localId]" :src="previews[item.localId]" alt="" draggable="false">
                  <LoaderCircle v-if="item.status === 'uploading'" :size="12" class="spin ce-thumb-icon" />
                  <TriangleAlert v-else-if="item.status === 'failed'" :size="12" class="ce-thumb-icon" />
                  <button type="button" class="ce-ref-remove" aria-label="移除参考图" @click="removeReference(item)"><X :size="10" /></button>
                </li>
              </ul>
              <div class="ce-gen-bar">
                <select v-model="form.styleCode" class="ce-pill ce-select" aria-label="画风" :disabled="readOnly">
                  <option value="">不使用画风</option>
                  <option v-for="style in styles" :key="style.code" :value="style.code">{{ style.name || style.code }}</option>
                </select>
                <input ref="refInput" type="file" class="ce-file" multiple :accept="marketingImageAccept(limits.mimes)" tabindex="-1" @change="onReferences">
                <button type="button" class="ce-pill" :disabled="readOnly" title="参考图只用于这次生成，不会存进角色" @click="pickReferences"><ImagePlus :size="13" />参考图<template v-if="references.length"> {{ references.length }}</template></button>
                <OptimizePromptButton
                  :text="form.appearance"
                  :model-code="optimizeModelCode"
                  :price="optimizePrice"
                  :block-reason="optimizeBlock"
                  purpose="character_appearance"
                  label="AI 帮写"
                  placement="top"
                  @replace="onOptimized"
                  @charged="loadBalance"
                />
                <span class="ce-spacer" />
                <select v-model="modelCode" class="ce-pill ce-select" aria-label="图片模型" :disabled="readOnly || !models.length" :title="modelsError">
                  <option v-if="!models.length" value="">{{ modelsError || "加载模型…" }}</option>
                  <option v-for="model in models" :key="model.code" :value="model.code">{{ model.name || model.code }}</option>
                </select>
                <select v-model.number="count" class="ce-pill ce-select ce-count-select" aria-label="生成张数" :disabled="readOnly">
                  <option v-for="n in genCounts" :key="n" :value="n">{{ n }} 张</option>
                </select>
                <button type="button" class="ce-generate" :disabled="genGate.disabled" :title="genGate.reason || '按报价扣费，失败全额退回'" @click="generate">
                  <LoaderCircle v-if="generating || (quoting && form.appearance.trim())" :size="13" class="spin" /><Sparkles v-else :size="13" />{{ genGate.label }}
                </button>
              </div>
              <p v-if="genGate.disabled && genGate.reason && !readOnly && form.appearance.trim()" class="ce-hint" :class="{ warn: Boolean(quote?.blocked) || genGate.reason.startsWith('余额不足') }">{{ genGate.reason }}</p>
              <p v-if="genError" class="ce-hint warn" role="alert">{{ genError }}</p>
            </div>
          </section>

          <!-- 右：名称 + 音色 -->
          <section class="ce-right">
            <label class="ce-field">
              <span class="ce-label">角色名称<em>*</em></span>
              <input v-model="form.name" type="text" placeholder="新角色" :disabled="readOnly" :aria-invalid="Boolean(nameError)">
              <small class="ce-hint" :class="{ warn: nameError }">{{ nameError || marketingCharacterNameHint(limits) }}</small>
            </label>
            <div class="ce-field">
              <span class="ce-label">音色</span>
              <div class="ce-voice-box">
                <textarea v-model="form.voiceText" rows="4" placeholder="描述角色音色，如年龄、语气、说话风格和语言" :disabled="readOnly" :aria-invalid="Boolean(voiceTextCheck.error)" aria-label="音色描述" />
                <small :class="{ over: voiceTextCheck.error }">{{ voiceTextCheck.error || `最多 ${limits.voiceText} 个字符（${voiceTextCheck.counter}）` }}</small>
              </div>
              <input ref="voiceInput" type="file" class="ce-file" accept="audio/*" tabindex="-1" @change="onVoice">
              <div v-if="form.voice" class="ce-voice" :class="form.voice.status">
                <Music :size="14" />
                <span class="ce-voice-name" :title="form.voice.name">{{ form.voice.name }}</span>
                <LoaderCircle v-if="form.voice.status === 'uploading'" :size="13" class="spin" />
                <button v-else-if="form.voice.status === 'failed'" type="button" class="ce-link" :title="form.voice.error" @click="uploadVoice(form.voice.localId)"><RotateCw :size="12" />重试</button>
                <button type="button" class="ce-icon" :disabled="readOnly" aria-label="删除音频" title="删除音频" @click="removeVoice"><Trash2 :size="13" /></button>
                <audio v-if="voiceSrc" :src="voiceSrc" controls preload="none" />
              </div>
              <button type="button" class="ce-upload-voice" :disabled="readOnly || !voiceGate.ok" :title="readOnly ? '你在该工作区是只读' : voiceGate.reason || '上传音频'" @click="pickVoice">
                <Upload :size="14" />上传音频
                <small v-if="!voiceGate.ok">{{ voiceGate.reason }}</small>
              </button>
              <p class="ce-hint">{{ MARKETING_VOICE_NOTE }}</p>
            </div>
          </section>
        </div>

        <p v-if="saveError" class="ce-error" role="alert">{{ saveError }}</p>
        <footer class="ce-foot">
          <span v-if="issue && !loading && !loadError && !readOnly" class="ce-issue">{{ issue }}</span>
          <button type="button" class="ce-btn" @click="requestClose">取消</button>
          <button type="submit" class="ce-btn primary" :disabled="!canSave" :title="issue || nameServerError">
            <LoaderCircle v-if="saving" :size="14" class="spin" />保存
          </button>
        </footer>
      </form>
    </div>
  </Teleport>
</template>

<style scoped>
.character-editor-overlay { z-index: 90; }
.character-editor {
  width: min(1120px, 100%);
  max-height: calc(100vh - 48px);
  overflow-y: auto;
  display: grid;
  gap: 14px;
  border: 1px solid var(--portal-line-strong);
  border-radius: 20px;
  padding: 20px 22px;
  color: var(--portal-text);
  background: var(--portal-elevated);
  box-shadow: 0 28px 70px var(--portal-shadow-strong);
}
.ce-head { display: flex; align-items: center; justify-content: space-between; }
.ce-head h2 { margin: 0; font-size: 16px; }
.ce-close {
  width: 28px;
  height: 28px;
  display: grid;
  place-items: center;
  border: 0;
  border-radius: 50%;
  color: var(--portal-muted);
  background: var(--portal-surface-2);
  cursor: pointer;
}
.ce-close:hover { color: var(--portal-text); background: var(--portal-hover); }
.ce-banner {
  margin: 0;
  display: flex;
  align-items: center;
  gap: 10px;
  border-radius: 8px;
  padding: 8px 10px;
  color: var(--portal-text-soft);
  background: var(--portal-surface-2);
  font-size: 12px;
}
.ce-banner.warn { background: var(--portal-gold-soft); }
.ce-banner.danger { color: var(--portal-danger); background: var(--portal-danger-soft); }
.ce-link {
  display: inline-flex;
  align-items: center;
  gap: 3px;
  margin-left: auto;
  border: 0;
  padding: 0;
  color: var(--portal-cyan);
  background: transparent;
  font: inherit;
  font-size: 12px;
  cursor: pointer;
  white-space: nowrap;
}
.ce-loading { display: flex; align-items: center; justify-content: center; gap: 8px; min-height: 240px; color: var(--portal-muted); }
.ce-body { display: grid; grid-template-columns: minmax(0, 1fr) 300px; gap: 20px; min-width: 0; }
.ce-left { display: grid; gap: 12px; min-width: 0; }
.ce-stage { display: grid; gap: 10px; border-radius: 16px; padding: 12px; background: var(--portal-surface); }
.ce-preview {
  position: relative;
  height: 280px;
  display: grid;
  place-items: center;
  overflow: hidden;
  border-radius: 12px;
  background: var(--portal-surface-2);
}
.ce-preview img { width: 100%; height: 100%; object-fit: contain; }
.ce-placeholder { color: var(--portal-faint); }
.ce-main {
  position: absolute;
  top: 8px;
  left: 8px;
  border-radius: 4px;
  padding: 0 6px;
  color: var(--portal-on-media);
  background: var(--portal-on-media-bg);
  font-size: 11px;
  line-height: 18px;
}
.ce-overlay {
  position: absolute;
  inset: 0;
  display: flex;
  align-items: center;
  justify-content: center;
  gap: 6px;
  border: 0;
  color: var(--portal-on-media);
  background: var(--portal-scrim-soft);
  font: inherit;
  font-size: 12px;
}
.ce-overlay.failed { background: var(--portal-scrim); cursor: pointer; }
.ce-tools { display: flex; flex-wrap: wrap; align-items: center; gap: 8px; }
.ce-count { color: var(--portal-muted); font-size: 12px; font-variant-numeric: tabular-nums; }
.ce-count.full { color: var(--portal-gold); }
.ce-file { display: none; }
.ce-pill {
  height: 32px;
  display: inline-flex;
  align-items: center;
  gap: 4px;
  border: 1px solid var(--portal-line-strong);
  border-radius: 999px;
  padding: 0 12px;
  color: var(--portal-text);
  background: transparent;
  font: inherit;
  font-size: 12px;
  font-weight: 600;
  cursor: pointer;
}
.ce-pill:hover:not(:disabled) { background: var(--portal-hover); }
.ce-pill:disabled { cursor: not-allowed; opacity: .5; }
.ce-select { max-width: 160px; padding-right: 8px; }
.ce-count-select { max-width: 76px; }
.ce-strip { display: flex; flex-wrap: wrap; gap: 6px; margin: 0; padding: 0; list-style: none; }
.ce-thumb {
  position: relative;
  width: 48px;
  height: 48px;
  display: grid;
  place-items: center;
  overflow: hidden;
  border: 2px solid transparent;
  border-radius: 8px;
  padding: 0;
  color: var(--portal-on-media);
  background: var(--portal-surface-2);
  cursor: pointer;
}
.ce-thumb.on { border-color: var(--portal-cyan); }
.ce-thumb.failed { border-color: var(--portal-danger-line); }
.ce-thumb img { width: 100%; height: 100%; object-fit: cover; }
.ce-thumb-icon { position: absolute; }
.ce-hint { margin: 0; color: var(--portal-muted); font-size: 12px; line-height: 18px; }
.ce-hint.warn { color: var(--portal-danger); }
.ce-candidates { display: grid; gap: 8px; border-radius: 14px; padding: 10px 12px; background: var(--portal-surface); }
.ce-candidates-head { display: flex; align-items: baseline; gap: 10px; font-size: 12px; }
.ce-candidates-head span { color: var(--portal-muted); }
.ce-candidate-grid { display: flex; flex-wrap: wrap; gap: 8px; margin: 0; padding: 0; list-style: none; }
.ce-candidate { position: relative; }
.ce-candidate > button:first-child {
  position: relative;
  width: 88px;
  height: 88px;
  overflow: hidden;
  border: 1px solid var(--portal-line);
  border-radius: 10px;
  padding: 0;
  background: var(--portal-surface-2);
  cursor: pointer;
}
.ce-candidate > button:first-child:disabled { cursor: not-allowed; opacity: .55; }
.ce-candidate img { width: 100%; height: 100%; object-fit: cover; }
.ce-candidate-add {
  position: absolute;
  inset: auto 0 0 0;
  display: flex;
  align-items: center;
  justify-content: center;
  gap: 3px;
  padding: 3px 0;
  color: var(--portal-on-media);
  background: var(--portal-on-media-bg);
  font-size: 11px;
  opacity: 0;
  transition: opacity .12s;
}
.ce-candidate:hover .ce-candidate-add,
.ce-candidate:focus-within .ce-candidate-add { opacity: 1; }
@media (hover: none) { .ce-candidate-add { opacity: 1; } }
.ce-candidate-dismiss,
.ce-ref-remove {
  position: absolute;
  top: 4px;
  right: 4px;
  width: 18px;
  height: 18px;
  display: grid;
  place-items: center;
  border: 0;
  border-radius: 50%;
  padding: 0;
  color: var(--portal-on-media);
  background: var(--portal-on-media-bg);
  cursor: pointer;
}
.ce-gen { display: grid; gap: 8px; border-radius: 16px; padding: 12px; background: var(--portal-surface); }
.ce-appearance,
.ce-voice-box textarea,
.ce-field input {
  width: 100%;
  box-sizing: border-box;
  border: 1px solid var(--portal-line);
  border-radius: 12px;
  padding: 10px 12px;
  color: var(--portal-text);
  background: var(--portal-input-bg);
  font: inherit;
  font-size: 13px;
  resize: vertical;
}
.ce-field input { border-radius: 999px; }
.ce-appearance::placeholder,
.ce-voice-box textarea::placeholder,
.ce-field input::placeholder { color: var(--portal-placeholder); }
.ce-appearance:focus,
.ce-voice-box textarea:focus,
.ce-field input:focus { outline: none; border-color: var(--portal-cyan-line); }
[aria-invalid="true"] { border-color: var(--portal-danger-line) !important; }
.ce-appearance:disabled,
.ce-voice-box textarea:disabled,
.ce-field input:disabled { opacity: .7; cursor: not-allowed; }
.ce-gen-meta { display: flex; align-items: center; gap: 10px; }
.ce-gen-meta small,
.ce-voice-box small { color: var(--portal-faint); font-size: 11px; font-variant-numeric: tabular-nums; }
.ce-gen-meta small.over,
.ce-voice-box small.over { color: var(--portal-danger); }
.ce-refs { display: flex; flex-wrap: wrap; gap: 6px; margin: 0; padding: 0; list-style: none; }
.ce-ref {
  position: relative;
  width: 44px;
  height: 44px;
  display: grid;
  place-items: center;
  overflow: hidden;
  border: 1px solid var(--portal-line);
  border-radius: 8px;
  color: var(--portal-on-media);
  background: var(--portal-surface-2);
}
.ce-ref.failed { border-color: var(--portal-danger-line); }
.ce-ref img { width: 100%; height: 100%; object-fit: cover; }
.ce-ref-remove { top: 2px; right: 2px; width: 14px; height: 14px; }
.ce-gen-bar { display: flex; flex-wrap: wrap; align-items: center; gap: 8px; }
.ce-spacer { flex: 1; }
.ce-generate {
  height: 32px;
  display: inline-flex;
  align-items: center;
  gap: 5px;
  border: 0;
  border-radius: 999px;
  padding: 0 14px;
  color: var(--portal-mark-text);
  background: var(--portal-mark-bg);
  font: inherit;
  font-size: 12px;
  font-weight: 600;
  cursor: pointer;
}
.ce-generate:hover:not(:disabled) { opacity: .9; }
.ce-generate:disabled { cursor: not-allowed; color: var(--portal-disabled-text); background: var(--portal-disabled-bg); }
.ce-right { display: grid; align-content: start; gap: 18px; min-width: 0; }
.ce-field { display: grid; gap: 8px; min-width: 0; }
.ce-label { font-size: 13px; font-weight: 600; }
.ce-label em { color: var(--portal-danger); font-style: normal; }
.ce-voice-box { display: grid; gap: 6px; border-radius: 14px; padding: 10px; background: var(--portal-surface); }
.ce-voice {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 8px;
  border: 1px solid var(--portal-line);
  border-radius: 12px;
  padding: 8px 10px;
  font-size: 12px;
}
.ce-voice.failed { border-color: var(--portal-danger-line); }
.ce-voice-name { min-width: 0; flex: 1; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.ce-voice audio { width: 100%; height: 32px; }
.ce-icon {
  width: 24px;
  height: 24px;
  display: grid;
  place-items: center;
  border: 0;
  border-radius: 6px;
  padding: 0;
  color: var(--portal-muted);
  background: transparent;
  cursor: pointer;
}
.ce-icon:hover:not(:disabled) { color: var(--portal-text); background: var(--portal-hover); }
.ce-icon:disabled { cursor: not-allowed; opacity: .5; }
.ce-upload-voice {
  min-height: 56px;
  display: grid;
  place-items: center;
  align-content: center;
  gap: 4px;
  border: 1px dashed var(--portal-line-strong);
  border-radius: 14px;
  color: var(--portal-text);
  background: var(--portal-surface);
  font: inherit;
  font-size: 13px;
  font-weight: 600;
  cursor: pointer;
}
.ce-upload-voice:hover:not(:disabled) { background: var(--portal-hover); }
.ce-upload-voice:disabled { cursor: not-allowed; opacity: .55; }
.ce-upload-voice small { color: var(--portal-muted); font-size: 11px; font-weight: 400; }
.ce-error { margin: 0; color: var(--portal-danger); font-size: 12px; }
.ce-foot { display: flex; align-items: center; justify-content: flex-end; gap: 10px; border-top: 1px solid var(--portal-line); padding-top: 14px; }
.ce-issue { margin-right: auto; color: var(--portal-muted); font-size: 12px; }
.ce-btn {
  min-width: 72px;
  height: 34px;
  display: inline-flex;
  align-items: center;
  justify-content: center;
  gap: 6px;
  border: 1px solid var(--portal-line-strong);
  border-radius: 999px;
  padding: 0 16px;
  color: var(--portal-text);
  background: transparent;
  font: inherit;
  font-size: 13px;
  font-weight: 600;
  cursor: pointer;
}
.ce-btn:hover:not(:disabled) { background: var(--portal-hover); }
.ce-btn.primary { border-color: transparent; color: var(--portal-mark-text); background: var(--portal-mark-bg); }
.ce-btn.primary:hover:not(:disabled) { opacity: .9; }
.ce-btn:disabled { cursor: not-allowed; color: var(--portal-disabled-text); background: var(--portal-disabled-bg); border-color: transparent; }
.spin { animation: ce-spin 1s linear infinite; }
@keyframes ce-spin { to { transform: rotate(360deg); } }
@media (max-width: 860px) {
  .character-editor { padding: 16px; border-radius: 16px; }
  .ce-body { grid-template-columns: 1fr; }
  .ce-preview { height: 220px; }
}
</style>
