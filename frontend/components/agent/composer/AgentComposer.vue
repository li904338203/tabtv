<script setup lang="ts">
/**
 * 创造页 / 画布 Agent 面板共用的输入框（开发 03 §2）。创造页四种模式：**Agent（默认，AGT-43）**、
 * 视频 / 图片 / 音频直接生成（AGT-42）。记住上次的模式（本地存储）。
 *
 * - Agent 模式整块交给 `AgentModeComposer(context='create')`（技能、模型偏好、每轮单价、一键优化、
 *   `POST /agent/sessions {createProject}` → 跳画布并展开面板），模式下拉经插槽挂在它的左组；
 * - 直接生成的发送流程（开发 03 §7）：`POST /quick-generations`（一次请求建项目 + 建节点 + 受理）→ 跳画布，
 *   由 generation store 的 `restore()` 接着跟踪节点上的任务。
 * - 切换模式时**正文保留**（两边绑同一个 `text`）；技能 / 引用 / 附件各属各的模式。
 * - 未登录时点输入框 / 发送：正文、模式、技能、附件 mediaId 存 `sessionStorage('create-draft')` →
 *   `/login?redirect=/create` → 回来恢复一次（`utils/createDraft`，24 小时过期）。
 *
 * ⚠️ 价格一律来自 `POST /generations/estimate`（与受理同一条报价路径），页面不自己算。
 * ⚠️ 同一次「发送意图」（含网络重试）用同一个 `Idempotency-Key`：连点、重试只建一个项目；
 *    用户改了任何输入就是新的意图，换一个键。
 * ⚠️ 失败时**保留正文与附件**，在输入框下方显示服务端原话（断网说断网）。
 * ⚠️ 页面打开时**不写任何数据**：只读配置、模型、能力校验（报价接口）。
 *
 * `context='canvas'`（画布右侧 Agent 面板，AGT-29）：只有 Agent 模式，整块交给 `AgentModeComposer`
 * （技能、@ 引用、选中即引用、模型偏好、每轮单价、发送 / 停止）；下面这套直接生成的加载一律不跑。
 *
 * `context='marketing'`（营销页，MKT-T-14、MKT-F-10）：同样只有 Agent 模式、不显示模式下拉，整块交给
 * `AgentModeComposer(context='create', scene='marketing')` —— 建项目 + 会话、画布开关、一键优化、未登录去登录
 * 都与创造页 Agent 模式同一套；直接生成的加载、创造页草稿一律不跑（营销草稿由输入框自己管，键分开）。
 */
import type { components } from "~/types/legacy";
import type { components as ApiComponents } from "@tabtv/api-client";
import { ArrowUp, LoaderCircle, Plus } from "@lucide/vue";
import { ElMessage } from "element-plus";
import VoicePicker from "~/components/canvas/VoicePicker.vue";
import AgentModeComposer from "./AgentModeComposer.vue";
import AttachmentChips, { type ComposerAttachment } from "./AttachmentChips.vue";
import ComposerEditor from "./ComposerEditor.vue";
import ModeSelect from "./ModeSelect.vue";
import ModelPrefPopover, { type DirectPref } from "./ModelPrefPopover.vue";
import type { JsonObject } from "~/types/canvas";
import { asObject } from "~/utils/canvas";
import { sendGate, type QuoteState } from "~/utils/composerPrice";
import { paramKey } from "~/utils/paramLayout";
import { saveCreateDraft, takeCreateDraft } from "~/utils/createDraft";
import { loginPath } from "~/utils/safeRedirect";
import {
  MODE_LABELS,
  MODE_PLACEHOLDERS,
  QUICK_MODES,
  buildQuickRequest,
  quickCanvasPath,
  quickErrorText,
  quickOperation,
  quickOperationNote,
  quickQuantities,
  readLastMode,
  writeLastMode,
  type ComposerMode,
  type QuickMode
} from "~/utils/quickGenerate";
import { applyVoiceSelection } from "~/utils/voice";
import type { ComposerRef } from "~/utils/composerRefs";
import type { MarketingOption } from "~/utils/marketingOptions";

type ModelSummary = components["schemas"]["ModelSummaryResp"];
type VoiceItem = components["schemas"]["VoiceResp"];
type QuickResult = {
  projectId: string;
  nodeId: string;
  generationId?: string;
  accepted: boolean;
  replayed: boolean;
  error?: { code: number; message: string };
};
type UploadResponse = { id?: string; kind?: string };
type MarketingConfig = ApiComponents["schemas"]["MarketingConfig"];
type AgentSkill = ApiComponents["schemas"]["AgentSkill"];

const props = withDefaults(defineProps<{
  context?: "create" | "canvas" | "marketing";
  projectId?: string;
  /** context='canvas' 时本页是否载入了画布；全屏对话页（AGT-44）为 false。 */
  withCanvas?: boolean;
  /** context='marketing'：`/marketing/config`。 */
  marketingConfig?: MarketingConfig | null;
  /** context='marketing'：配置的 `available`（Agent 关着时 false，只禁发送）。 */
  marketingAvailable?: boolean;
}>(), { context: "create", projectId: "", withCanvas: true, marketingConfig: null, marketingAvailable: true });
const emit = defineEmits<{ "open-ref": [ref: ComposerRef] }>();
const agentMode = ref<InstanceType<typeof AgentModeComposer>>();

const auth = useAuthStore();
const workspace = useWorkspaceStore();
const configStore = useCanvasConfigStore();

/** 输入框当前模式（默认 Agent）。 */
const composerMode = ref<ComposerMode>("agent");
/** 直接生成的模式（Agent 模式时停在上一次用过的直接生成模式，切回来参数还在）。 */
const mode = ref<QuickMode>("video");
const text = ref("");
const paramsByMode = reactive<Record<QuickMode, JsonObject>>({ video: {}, image: {}, audio: {} });
const modelsByMode = reactive<Record<QuickMode, ModelSummary[] | undefined>>({ video: undefined, image: undefined, audio: undefined });
const attachments = ref<Array<ComposerAttachment & { file: File }>>([]);
const quote = ref<QuoteState>({ loading: false });
const sending = ref(false);
const sendError = ref("");
const voiceOpen = ref(false);
const editor = ref<InstanceType<typeof ComposerEditor>>();
const fileInput = ref<HTMLInputElement>();
/** 当前这次「发送意图」的幂等键；输入一变就作废（见文件头）。 */
let pendingKey = "";
let quoteTimer: ReturnType<typeof setTimeout> | undefined;
let quoteSeq = 0;

const params = computed(() => paramsByMode[mode.value]);
const models = computed(() => modelsByMode[mode.value] ?? []);
const readOnly = computed(() => workspace.current?.role === "viewer");
const doneAttachments = computed(() => attachments.value.filter(item => item.status === "done" && item.referenceId));
const imageCount = computed(() => doneAttachments.value.length);
const uploading = computed(() => attachments.value.some(item => item.status === "uploading"));
/** 操作按参考图张数推断（创造页没有操作下拉，见 `quickVideoOperation`）；报价与发送同用这一个。 */
const operation = computed(() => quickOperation(mode.value, imageCount.value));
/** 中组「模型｜创作偏好」浮层的数据：当前模式、参数、模型目录、底部的操作说明。 */
const directPref = computed<DirectPref>(() => ({
  mode: mode.value,
  params: params.value,
  models: models.value,
  note: quickOperationNote(mode.value, imageCount.value)
}));
const hasContent = computed(() => Boolean(text.value.trim()) || doneAttachments.value.length > 0);

/** 参考上传的约束来自 `param_def`（图片节点的 reference：maxCount / accept / maxSize），不写死。 */
const referenceConfig = computed(() => {
  const param = configStore.nodeType("image")?.params?.find(item => item.code === "reference");
  return asObject(param?.config);
});
const maxAttachments = computed(() => {
  const value = Number(referenceConfig.value.maxCount ?? 1);
  return Number.isFinite(value) && value >= 1 ? Math.floor(value) : 1;
});
const acceptAttr = computed(() => {
  const accept = referenceConfig.value.accept;
  return Array.isArray(accept) ? accept.join(",") : typeof accept === "string" ? accept : "image/*";
});
const canAttach = computed(() => mode.value !== "audio");

const unavailable = computed(() => {
  const out: Partial<Record<QuickMode, string>> = {};
  for (const item of QUICK_MODES) {
    const list = modelsByMode[item];
    if (list && list.length === 0) out[item] = `暂无可用的${MODE_LABELS[item].replace("生成", "")}模型`;
  }
  return out;
});

const gate = computed(() => sendGate({
  readOnly: readOnly.value,
  sending: sending.value,
  uploading: uploading.value,
  hasContent: hasContent.value,
  hasModel: typeof params.value.model === "string" && Boolean(params.value.model),
  // 音色在 param_def 里是必填（与画布音频节点同一条：没选音色不让生成）。
  missingParam: mode.value === "audio" && !params.value.voiceId && !params.value.voice ? "请先选择音色" : undefined,
  modeUnavailable: unavailable.value[mode.value],
  quote: quote.value
}));
const placeholder = computed(() => readOnly.value
  ? "你在该工作区是只读，无法发起创作"
  : MODE_PLACEHOLDERS[mode.value]);

// ---------------------------------------------------------------- 加载（只读）

/** 按 `param_def` 的默认值补齐参数（与画布节点新建时同一条：未设置的才补）。 */
function applyDefaults(target: QuickMode) {
  const next = { ...paramsByMode[target] };
  for (const param of configStore.nodeType(target)?.params ?? []) {
    const key = paramKey(param.code);
    const value = (param as { defaultValue?: unknown }).defaultValue;
    if (key && next[key] === undefined && value !== undefined && value !== null && value !== "") next[key] = value;
  }
  paramsByMode[target] = next;
}

async function loadModels(target: QuickMode) {
  try {
    const list = await configStore.models(target);
    modelsByMode[target] = list;
    const current = paramsByMode[target].model;
    if (!list.some(model => model.code === current)) {
      paramsByMode[target] = { ...paramsByMode[target], model: list[0]?.code };
    }
  } catch {
    // 取不到模型不阻断：参数行没有模型可选，发送键会说「请先选择模型」。
    modelsByMode[target] = undefined;
  }
}

async function bootstrap() {
  if (!auth.isAuthenticated) return;
  try {
    await configStore.load();
  } catch {
    // 配置取不到时参数行为空，报价不会发起；不让整个输入框报错。
  }
  for (const item of QUICK_MODES) applyDefaults(item);
  await Promise.all(QUICK_MODES.map(loadModels));
  scheduleQuote();
}

// ---------------------------------------------------------------- 报价

function scheduleQuote() {
  clearTimeout(quoteTimer);
  quoteTimer = setTimeout(refreshQuote, 350);
}

async function refreshQuote() {
  const modelCode = params.value.model;
  // Agent 模式不报直接生成的价（它的价格是每轮单价，由 AgentModeComposer 自己取）。
  if (composerMode.value === "agent") return;
  if (!auth.isAuthenticated || typeof modelCode !== "string" || !modelCode) {
    quote.value = { loading: false };
    return;
  }
  const seq = ++quoteSeq;
  const quantities = quickQuantities(mode.value, params.value);
  quote.value = { ...quote.value, loading: true, error: undefined };
  try {
    await useApi()<unknown>("/generations/estimate", {
      method: "POST",
      body: {
        modelCode,
        operation: operation.value,
        ...quantities,
        text: text.value.trim(),
        inputs: imageCount.value ? { image: imageCount.value } : {}
      }
    });
    if (seq !== quoteSeq) return;
    quote.value = { loading: false };
  } catch (cause) {
    if (seq !== quoteSeq) return;
    quote.value = { loading: false, error: quickErrorText(cause, import.meta.client ? navigator.onLine : true) };
  }
}

// 参数 / 附件 / 模型 / 模式变化时重新报价（正文只影响按字数计价的音频，一并防抖）。
watch(
  () => JSON.stringify([mode.value, params.value, imageCount.value, text.value.length]),
  () => {
    pendingKey = "";
    scheduleQuote();
  }
);
watch(text, () => { pendingKey = ""; });

// ---------------------------------------------------------------- 交互

function setMode(next: ComposerMode) {
  const changed = next !== composerMode.value;
  composerMode.value = next;
  if (next !== "agent") mode.value = next;
  if (import.meta.client) writeLastMode(localStorage, next);
  sendError.value = "";
  if (changed && next !== "agent") scheduleQuote();
}

/** 快捷入口：切到 Agent 并插入技能胶囊（null = 不指定技能）。 */
async function applySkill(skill: { code: string; name: string } | null) {
  setMode("agent");
  await nextTick();
  agentMode.value?.setSkill(skill);
  agentMode.value?.focus();
}

function updateParam(code: string, value: unknown) {
  paramsByMode[mode.value] = { ...paramsByMode[mode.value], [code]: value };
}

function selectVoice(voice: VoiceItem) {
  paramsByMode.audio = applyVoiceSelection(paramsByMode.audio, voice);
  voiceOpen.value = false;
}

function focus() {
  if (props.context !== "create" || composerMode.value === "agent") agentMode.value?.focus();
  else editor.value?.focus();
}

/** 把一段话放进输入框、不发送（Agent 面板空会话的推荐词）：替换正文并聚焦，用户自己点发送。 */
function fillText(value: string) {
  if (props.context !== "create" || composerMode.value === "agent") {
    agentMode.value?.fillText(value);
    return;
  }
  text.value = value;
  editor.value?.focus();
}

/** 画布：外部加一个节点引用（右键「问 Agent」）。 */
function addNodeRef(nodeId: string) {
  agentMode.value?.addNodeRef(nodeId);
}

/** 画布：外部加一份媒体引用（侧栏资产「添加到 Agent」）。 */
function addMediaRef(item: { id: string; label: string; thumbMediaId?: string; hint?: string }) {
  agentMode.value?.addRef({ kind: "media", ...item });
}

/** 未登录：存下已输入的内容，去登录，回来恢复（设计 02 §8）。 */
async function requireLogin() {
  if (auth.isAuthenticated) return false;
  if (import.meta.client) {
    saveCreateDraft(sessionStorage, {
      mode: mode.value,
      text: text.value,
      mediaIds: doneAttachments.value.map(item => item.referenceId!)
    });
  }
  await navigateTo(loginPath("/create"));
  return true;
}

function pickFiles() {
  if (readOnly.value || !canAttach.value) return;
  fileInput.value?.click();
}

async function uploadOne(item: ComposerAttachment & { file: File }) {
  item.status = "uploading";
  item.error = undefined;
  try {
    const body = new FormData();
    body.append("file", item.file);
    body.append("mediaType", "image");
    const response = await useApi()<UploadResponse>("/media/references", { method: "POST", body });
    if (!response.id) throw new Error("上传接口响应缺少 ID");
    item.referenceId = String(response.id);
    item.status = "done";
  } catch (cause) {
    item.status = "failed";
    item.error = quickErrorText(cause, navigator.onLine);
  }
}

async function onFiles(event: Event) {
  const input = event.target as HTMLInputElement;
  const picked = Array.from(input.files ?? []);
  input.value = "";
  if (!picked.length) return;
  if (await requireLogin()) return;
  const room = maxAttachments.value - attachments.value.length;
  if (room <= 0) {
    ElMessage.warning(`最多 ${maxAttachments.value} 张参考图`);
    return;
  }
  if (picked.length > room) ElMessage.warning(`最多 ${maxAttachments.value} 张参考图，超出的 ${picked.length - room} 张已忽略`);
  const maxSize = Number(referenceConfig.value.maxSize ?? 10 * 1024 * 1024);
  for (const file of picked.slice(0, room)) {
    if (!file.type.startsWith("image/")) {
      ElMessage.error(`${file.name}：目前只支持图片`);
      continue;
    }
    if (Number.isFinite(maxSize) && file.size > maxSize) {
      ElMessage.error(`${file.name}：超过 ${Math.round(maxSize / 1024 / 1024)}MB`);
      continue;
    }
    const item = reactive({
      key: `${Date.now().toString(36)}_${Math.random().toString(36).slice(2, 7)}`,
      name: file.name,
      status: "uploading" as const,
      previewUrl: URL.createObjectURL(file),
      file
    }) as ComposerAttachment & { file: File };
    attachments.value.push(item);
    void uploadOne(item);
  }
}

function removeAttachment(key: string) {
  const item = attachments.value.find(entry => entry.key === key);
  if (item?.previewUrl) URL.revokeObjectURL(item.previewUrl);
  attachments.value = attachments.value.filter(entry => entry.key !== key);
}

function retryAttachment(key: string) {
  const item = attachments.value.find(entry => entry.key === key);
  if (item) void uploadOne(item);
}

async function send() {
  if (await requireLogin()) return;
  if (gate.value.disabled || sending.value) return;
  const modelCode = String(params.value.model ?? "");
  sending.value = true;
  sendError.value = "";
  // ⚠️ 键在第一次点发送时生成，网络重试复用同一个；输入一变（见上面的 watch）就作废。
  if (!pendingKey) pendingKey = crypto.randomUUID();
  try {
    const body = buildQuickRequest({
      mode: mode.value,
      modelCode,
      text: text.value,
      params: params.value,
      attachments: doneAttachments.value.map(item => ({ referenceId: item.referenceId!, name: item.name }))
    });
    const result = await useApi()<QuickResult>("/quick-generations", {
      method: "POST",
      headers: { "Idempotency-Key": pendingKey },
      body
    });
    pendingKey = "";
    writeLastMode(localStorage, mode.value);
    if (!result.accepted && result.error?.message) {
      // 项目与节点已经建好，节点上写着同一句话；跳过去用户可以直接重试。
      ElMessage.warning(`项目已创建，但生成没有发起：${result.error.message}`);
    }
    await navigateTo(quickCanvasPath(result.projectId));
  } catch (cause) {
    // ⚠️ 保留正文与附件，原话显示在输入框下方。
    sendError.value = quickErrorText(cause, navigator.onLine);
  } finally {
    sending.value = false;
  }
}

onMounted(() => {
  // 画布面板、营销页只有 Agent 模式：直接生成的配置 / 模型 / 报价都不取，也不碰创造页草稿。
  if (props.context === "canvas" || props.context === "marketing") return;
  const last = readLastMode(localStorage);
  composerMode.value = last;
  if (last !== "agent") mode.value = last;
  // 未登录时存下的草稿：登录回来恢复一次（取出即清）。未登录时不动它（还没去登录 / 登录没成功）。
  if (auth.isAuthenticated) {
    const draft = takeCreateDraft(sessionStorage);
    if (draft) {
      composerMode.value = draft.mode;
      if (draft.mode !== "agent") mode.value = draft.mode;
      text.value = draft.text;
      if (draft.mode === "agent" && (draft.skill || draft.mediaIds.length)) {
        void nextTick(() => agentMode.value?.restoreDraft({ skill: draft.skill, mediaIds: draft.mediaIds }));
      } else if (draft.mediaIds.length) {
        // 直接生成的附件是带本地预览的上传项，草稿只有 ID 恢复不出来：说清楚，让用户重新添加。
        ElMessage.info("登录前添加的参考图需要重新添加");
      }
    }
  }
  void bootstrap();
});

onBeforeUnmount(() => {
  clearTimeout(quoteTimer);
  for (const item of attachments.value) if (item.previewUrl) URL.revokeObjectURL(item.previewUrl);
});

/** 打开模型偏好浮层（全屏对话页顶栏，AGT-44）。 */
function openPrefs() {
  agentMode.value?.openPrefs();
}

/** 营销页芯片：选中技能（插胶囊 + 联动），不发送。 */
function applyMarketingSkill(skill: AgentSkill) {
  return agentMode.value?.applySkill(skill);
}

/** 营销页：填入一个创意（与模板墙同一条路）。 */
function applyCreative(option: MarketingOption) {
  return agentMode.value?.applyCreative(option);
}

/** 营销页芯片的「全部技能 →」。 */
function openSkills() {
  return agentMode.value?.openSkills();
}

defineExpose({ setMode, applySkill, focus, fillText, addNodeRef, addMediaRef, openPrefs, applyMarketingSkill, applyCreative, openSkills });
</script>

<template>
  <AgentModeComposer
    v-if="context === 'canvas'"
    ref="agentMode"
    context="canvas"
    :project-id="projectId"
    :with-canvas="withCanvas"
    @open-ref="item => emit('open-ref', item)"
  />
  <div v-else-if="context === 'marketing'" class="agent-composer-shell marketing-shell">
    <AgentModeComposer
      ref="agentMode"
      context="create"
      scene="marketing"
      :marketing-config="marketingConfig"
      :marketing-available="marketingAvailable"
    />
  </div>
  <div v-else-if="composerMode === 'agent'" class="agent-composer-shell">
    <AgentModeComposer ref="agentMode" v-model:text="text" context="create">
      <template #mode>
        <ModeSelect :model-value="composerMode" :unavailable="unavailable" @update:model-value="setMode" />
      </template>
    </AgentModeComposer>
  </div>
  <section v-else class="agent-composer" :class="{ readonly: readOnly }" aria-label="创作输入框">
    <ComposerEditor
      ref="editor"
      v-model="text"
      :placeholder="placeholder"
      :disabled="readOnly || sending"
      @submit="send"
      @focus="requireLogin"
    />
    <AttachmentChips :items="attachments" :disabled="sending" @remove="removeAttachment" @retry="retryAttachment" />
    <div class="composer-bar">
      <div class="composer-group left">
        <button
          type="button"
          class="composer-icon"
          :disabled="readOnly || sending || !canAttach || attachments.length >= maxAttachments"
          :title="!canAttach ? '音频生成不需要参考图' : `添加参考图（最多 ${maxAttachments} 张）`"
          aria-label="添加参考图"
          @click="pickFiles"
        ><Plus :size="18" /></button>
        <input ref="fileInput" type="file" class="composer-file" :accept="acceptAttr" multiple @change="onFiles">
        <ModeSelect :model-value="composerMode" :unavailable="unavailable" :disabled="sending" @update:model-value="setMode" />
      </div>
      <div class="composer-group center">
        <ModelPrefPopover
          v-if="auth.isAuthenticated"
          class="composer-params"
          :direct="directPref"
          :disabled="readOnly || sending"
          @direct-update="updateParam"
          @open-voice="voiceOpen = true"
        />
      </div>
      <div class="composer-group right">
        <button
          type="button"
          class="composer-submit"
          :disabled="auth.isAuthenticated && gate.disabled"
          :title="auth.isAuthenticated ? (gate.reason || '发送') : '登录后发送'"
          :aria-label="gate.reason || '发送'"
          @click="send"
        >
          <LoaderCircle v-if="sending" :size="17" class="spin" />
          <ArrowUp v-else :size="18" />
        </button>
      </div>
    </div>
    <p v-if="readOnly" class="composer-note">你在该工作区是只读，无法发起创作</p>
    <p v-else-if="sendError" class="composer-error" role="alert">{{ sendError }}</p>
    <p v-else-if="auth.isAuthenticated && gate.disabled && gate.reason && hasContent && !sending" class="composer-note">{{ gate.reason }}</p>
    <VoicePicker v-model="voiceOpen" :selected-id="String(paramsByMode.audio.voiceId ?? '')" @select="selectVoice" />
  </section>
</template>

<style scoped>
/*
 * 统一输入框（照对标改版）：大圆角深色卡片 + 下方三组工具栏。直接生成模式下中组是一个
 * 「模型｜创作偏好」胶囊（与画布 Agent 面板同一个 `ModelPrefPopover`），点开是该模式的模型与参数；
 * 浮层锚在输入框卡片上（本元素 position: relative），宽度跟输入框走。
 * 宽度按输入框自身判断（容器查询）：窄时中组换到第一行。
 */
.agent-composer {
  position: relative;
  z-index: 2;
  width: min(880px, 100%);
  margin: 28px auto 0;
  container: composer / inline-size;
  border: 1px solid var(--portal-line);
  border-radius: 22px;
  padding: 16px 16px 12px 18px;
  background: var(--portal-surface);
  box-shadow: 0 10px 30px var(--portal-shadow);
  text-align: left;
  transition: border-color .15s ease;
}
.agent-composer:focus-within { border-color: var(--portal-line-strong); }
/* Agent 模式：卡片样式在 AgentModeComposer 里（与画布面板同一个组件），这里只管宽度与位置 */
.agent-composer-shell { position: relative; z-index: 2; width: min(880px, 100%); margin: 28px auto 0; }
/* 营销页：宽度与位置由页面的挂载位决定（槽位区要更宽的输入框） */
.agent-composer-shell.marketing-shell { width: 100%; margin: 0; }
.agent-composer :deep(.composer-editor) { min-height: 88px; }
.composer-bar {
  display: grid;
  grid-template-columns: minmax(max-content, 1fr) auto minmax(max-content, 1fr);
  grid-template-areas: "left center right";
  align-items: center;
  gap: 8px;
  margin-top: 12px;
}
.composer-group { min-width: 0; display: flex; align-items: center; gap: 6px; }
.composer-group.left { grid-area: left; }
.composer-group.center { grid-area: center; justify-content: center; }
.composer-group.right { grid-area: right; justify-content: flex-end; }
@container composer (max-width: 640px) {
  .composer-bar {
    grid-template-columns: auto minmax(0, 1fr);
    grid-template-areas: "center center" "left right";
    row-gap: 8px;
  }
  .composer-group.center { justify-content: flex-start; }
}
.composer-params { min-width: 0; }
.composer-icon {
  width: 30px;
  height: 30px;
  display: grid;
  place-items: center;
  border: 0;
  border-radius: 50%;
  color: var(--portal-text-soft);
  background: transparent;
  cursor: pointer;
}
.composer-icon:hover:not(:disabled) { color: var(--portal-text); background: var(--portal-hover); }
.composer-icon:disabled { color: var(--portal-disabled-text); cursor: not-allowed; }
.composer-file { display: none; }
.composer-divider { width: 1px; height: 14px; background: var(--portal-line-strong); }
.composer-submit {
  flex: none;
  width: 34px;
  height: 34px;
  display: grid;
  place-items: center;
  border: 0;
  border-radius: 50%;
  /* 与创造页输入框同一套中性主按钮（AgentModeComposer.vue）。 */
  color: var(--portal-mark-text);
  background: var(--portal-mark-bg);
  cursor: pointer;
}
.composer-submit:disabled { color: var(--portal-disabled-text); background: var(--portal-disabled-bg); cursor: not-allowed; }
.composer-error, .composer-note { margin: 8px 0 0; font-size: 12px; }
.composer-error { color: var(--portal-danger); }
.composer-note { color: var(--portal-muted); }
</style>
