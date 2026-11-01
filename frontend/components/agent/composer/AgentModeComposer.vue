<script setup lang="ts">
/**
 * Agent 模式的输入框本体（AGT-29，设计 02 §4 / 设计 03 §2.3）。由 `AgentComposer` 在
 * `context='canvas'`（画布右侧面板）时挂出来；创造页的 Agent 模式（AGT-43）复用同一个组件。
 *
 * 组成：正文（开头可带一个技能胶囊）+ 引用 / 附件胶囊 + 底栏（＋ 附件、@、技能、模型偏好、每轮单价、发送 / 停止）。
 *
 * ⚠️ 引用以胶囊对象为准，发出去的 `refs` 只有 `{kind,id}`（AG-03、GO-27），不带预览 URL。
 * ⚠️ 「选中即引用」：画布选中的节点自动成为胶囊；取消选中只移除**自动加入**的，手动 `@` 的保留。
 * ⚠️ 禁用理由给最根本的那一条（`utils/agentState.composerAvailability`：权限 > 已取消 > 卡片提示 > 余额）。
 * ⚠️ 运行中（模型正在输出）发送键变停止；**挂起等待时照样能发**（等生成时可以追问别的事）。
 * ⚠️ 发送失败**保留正文、胶囊与附件**，原话显示在输入框下方（断网说断网）。
 * ⚠️ 打开页面不写任何数据：只读偏好、模型目录与报价；写只发生在用户点发送 / 改偏好 / 设为默认时。
 *
 * `context='create'`（创造页，AGT-43）：没有当前会话 —— 发送 = `POST /agent/sessions {createProject}`
 * 一次建项目 + 会话 + 首条消息，再跳 `/video/canvas/{projectId}?session={sessionId}`（面板自动展开）。
 * 与画布的差别：不读 agent store 的当前会话（那是上一次打开的画布留下的）；正文右上角有「一键优化」；
 * 左组多一个模式下拉（插槽 `mode`）；未登录时点输入框 / 发送 → 存草稿去登录（`utils/createDraft`）。
 * 右组多一个「画布」开关（AGT-44，设计 02 §12）：开 → 发送后进画布页（面板展开）；关 → 进全屏对话页
 * `/video/chat/{projectId}?session=`。默认开、本地记住；直接生成模式不渲染本组件，自然没有这个开关。
 *
 * `withCanvas=false`（全屏对话页，AGT-44）：仍是会话输入框（读 agent store 的当前会话），但这一页
 * 没有载入画布 —— 不做「选中即引用」、`@` 不列画布节点（画布 store 里可能是上一个画布留下的）。
 *
 * `scene='marketing'`（营销页，MKT-T-14，营销 dev/03 §3）：在 `context='create'` 的基础上**只加不改** ——
 * 左侧槽位区（`MarketingSlots`）、正文开头四维胶囊、底栏技能之后三个选项按钮（`MarketingSelectorBar`
 * + `MarketingOptionPicker`）、技能弹层只列营销技能、营销草稿与 `loginPath("/marketing")`；
 * `@`、`＋`、模型偏好、画布开关（同一个本地键）、价格（同一个 `agentPriceView` + `PricePill`）、一键优化、失败保留不变。
 * 联动规则全在 `utils/marketingComposer`（纯函数），这里只负责把它的结果落到正文 / 偏好 / 槽位上。
 * 发送（MKT-T-24，营销 dev/03 §3.5）：一次 `POST /agent/sessions {scene:"marketing", marketing, refs(带 slot/order)}`
 * 建项目 + 营销会话 + 首条消息（请求体见 `utils/marketingRequest`），成功清营销草稿、按画布开关跳画布页或全屏对话页；
 * 失败保留全部输入，40001 `details.slot` 标红槽位、42215 标红胶囊并写进选项弹层的 `serverBlocks`。
 */
import { ArrowUp, AtSign, ChevronDown, LoaderCircle, Plus, Square, Zap } from "@lucide/vue";
import { agentLandingPath, readCanvasToggle, writeCanvasToggle } from "~/utils/agentChat";
import { ElMessage, ElMessageBox } from "element-plus";
import OptimizePromptButton from "./OptimizePromptButton.vue";
import AttachmentChips, { type ComposerAttachment } from "./AttachmentChips.vue";
import ComposerEditor from "./ComposerEditor.vue";
import MentionPicker from "./MentionPicker.vue";
import ModelPrefPopover from "./ModelPrefPopover.vue";
import PricePill from "./PricePill.vue";
import SkillPicker, { type AgentSkill } from "./SkillPicker.vue";
import { composerAvailability, composerSubmitAction, sendButtonMode } from "~/utils/agentState";
import { effectivePrefs as resolvePrefs, prefModel, resolveReasoningModel, withPrefFields, type AgentPrefsValue } from "~/utils/agentPrefs";
import { canSubmitContent, type ComposerChip, type ComposerChipDim, type Trigger } from "~/utils/composerKeys";
import { pickerPlacement, type PickerPlacement } from "~/utils/pickerPlacement";
import { agentPriceView, optimizePriceView } from "~/utils/composerPrice";
import { creditBalanceOf } from "~/utils/accountPanel";
import { saveCreateDraft } from "~/utils/createDraft";
import { loginPath } from "~/utils/safeRedirect";
import { mediaThumbUrl } from "~/utils/authorizedMedia";
import { referenceMediaType } from "~/utils/referenceUpload";
import {
  addManualRef,
  nodeCandidates,
  nodeRefSource,
  removeRef,
  serializeRefs,
  syncSelectedRefs,
  type ComposerRef
} from "~/utils/composerRefs";
import { buildAgentSessionRequest, quickErrorText } from "~/utils/quickGenerate";
import type { AgentCreateSessionRequest } from "~/stores/agent";
import type { components } from "@tabtv/api-client";
import MarketingSlots from "./marketing/MarketingSlots.vue";
import MarketingSelectorBar from "./marketing/MarketingSelectorBar.vue";
import MarketingOptionPicker from "./marketing/MarketingOptionPicker.vue";
import ProductPickerDialog from "./marketing/ProductPickerDialog.vue";
import CharacterPickerDialog from "./marketing/CharacterPickerDialog.vue";
import {
  applyCreative,
  applyPick,
  applySkill as applySkillToState,
  conflictText,
  dropChip,
  initialComposerState,
  marketingSendGate,
  type MarketingComposerState,
  type PickDim,
  type SlotKey,
  type SwitchResult
} from "~/utils/marketingComposer";
import {
  emptySlotsValue,
  marketingSelectedProductId,
  marketingSetProduct,
  marketingSyncProduct,
  slotsFailedText,
  SLOT_KEYS,
  type MarketingPickedProduct,
  type MarketingSlotsValue
} from "~/utils/marketingSlots";
import {
  MARKETING_CHARACTER_REMOVED_TEXT,
  MARKETING_PRODUCT_REMOVED_TEXT,
  marketingCharacterLimits,
  marketingCharacterSlotItems,
  marketingPickedFromSlot,
  marketingProductLimits,
  marketingSyncCharacter,
  type MarketingPickedCharacter
} from "~/utils/marketingLibrary";
import { currentVideoModel, type MarketingOption, type ServerOptionBlock } from "~/utils/marketingOptions";
import {
  MARKETING_DRAFT_DEBOUNCE_MS,
  composerStateFromDraft,
  marketingDraftOf,
  pendingUploadsText,
  readMarketingDraft,
  saveMarketingDraft,
  slotsValueFromDraft,
  clearMarketingDraft,
  MARKETING_LEGACY_DRAFT_TEXT
} from "~/utils/marketingDraft";
import { buildMarketingSessionRequest, marketingSendFailure, marketingSendFingerprint, type MarketingRequestInput } from "~/utils/marketingRequest";
import { isBatchSkill } from "~/utils/skillEntries";

type Attachment = ComposerAttachment & { file: File; mediaType: "image" | "video" | "audio" };
type MarketingConfig = components["schemas"]["MarketingConfig"];

const props = withDefaults(defineProps<{
  context?: "create" | "canvas";
  projectId?: string;
  /** 本页载入了画布（画布面板）；全屏对话页为 false：不读画布 store。 */
  withCanvas?: boolean;
  /** 场景：不传 = 通用（创造页、画布面板）；`marketing` = 营销页（只与 context='create' 搭配）。 */
  scene?: "marketing";
  /** 营销页配置（`/marketing/config`）。 */
  marketingConfig?: MarketingConfig | null;
  /** 营销页能不能发送（配置的 `available`；Agent 总开关关着时为 false）。 */
  marketingAvailable?: boolean;
}>(), { context: "canvas", projectId: "", withCanvas: true, scene: undefined, marketingConfig: null, marketingAvailable: true });
/** 点了某个引用胶囊（面板据此在画布上定位节点）。 */
const emit = defineEmits<{ "open-ref": [ref: ComposerRef] }>();

const agent = useAgentStore();
const prefsStore = useAgentPrefsStore();
const workspace = useWorkspaceStore();
const canvas = useCanvasStore();
const auth = useAuthStore();

/** 正文。创造页由父组件绑定（切换模式时保留已输入的内容）；画布面板不绑，就是本地状态。 */
const text = defineModel<string>("text", { default: "" });
const refs = ref<ComposerRef[]>([]);
const attachments = ref<Attachment[]>([]);
const skill = ref<{ code: string; name: string } | null>(null);
const trigger = ref<Trigger | null>(null);
/** 由按钮打开的候选（没有正文里的触发字）。 */
const manualPicker = ref<"/" | "@" | null>(null);
const sendError = ref("");
/** 还没有会话时（新会话草稿）对偏好的改动；建会话时随首条消息一起发。 */
const draftPrefs = ref<AgentPrefsValue>({});
/** 余额；没取到为 undefined —— **不猜**（generation store 取失败时记 0，拿它判「余额不足」会误拦）。 */
const balance = ref<number | undefined>();
const editor = ref<InstanceType<typeof ComposerEditor>>();
const skillPicker = ref<InstanceType<typeof SkillPicker>>();
const mentionPicker = ref<InstanceType<typeof MentionPicker>>();
const fileInput = ref<HTMLInputElement>();
const thumbs = useAuthenticatedMedia();

const isCanvas = computed(() => props.context === "canvas");
const isCreate = computed(() => props.context === "create");
/** 画布节点相关的能力（选中即引用、@ 画布节点）只在真的载入了画布时才有。 */
const canvasNodes = computed(() => isCanvas.value && props.withCanvas);
/**
 * 技能与模型收成工具栏左组里的两个图标（2026-11-01 按用户给的参考图，画布 Agent 面板）：
 * 「＋ @ 模型 Skill」一行到底，悬停才说明是什么；此前是「技能 ⌄」「模型名｜创作偏好 ⌄」两个胶囊，窄面板里还单独占一行。
 * 创造页与营销页的输入框宽，仍用胶囊（营销还要在同一组里放创意 / Hook / 风格）。
 */
const iconTools = computed(() => isCanvas.value);
/** 创造页「画布」开关：开 = 发送后进画布页；关 = 进全屏对话页。挂载后才读本地（SSR 时没有 localStorage）。 */
const canvasOn = ref(true);
function setCanvasOn(value: boolean) {
  canvasOn.value = value;
  if (import.meta.client) writeCanvasToggle(localStorage, value);
}
const prefPopover = ref<InstanceType<typeof ModelPrefPopover>>();
/** 创造页未登录：输入框照样能点（点了去登录），价格、模型都不取。 */
const guest = computed(() => isCreate.value && !auth.isAuthenticated);
/** 创造页永远是「新会话」：store 里的当前会话是上一次打开的画布留下的，与这里无关。 */
const current = computed(() => (isCanvas.value ? agent.current : null));
/** 创造页建会话（建项目）进行中。画布面板用 store 的 sending。 */
const creating = ref(false);
const busy = computed(() => (isCanvas.value ? agent.sending : creating.value));
const role = computed(() => workspace.current?.role ?? null);

// ---------------------------------------------------------------- 营销场景的状态（MKT-T-14）

const isMarketing = computed(() => props.scene === "marketing");
/** 胶囊、预填、槽位声明、联动标记（`utils/marketingComposer` 的状态）。正文与偏好以本组件的 `text` / 生效偏好为准，调用前同步进去。 */
const mk = ref<MarketingComposerState>(initialComposerState(props.marketingConfig?.slots ?? []));
/** 槽位内容（`MarketingSlots` 的 v-model）。 */
const slotsValue = ref<MarketingSlotsValue>(emptySlotsValue());
const slotsUploading = ref(false);
const slotsRef = ref<InstanceType<typeof MarketingSlots>>();
const selectorBar = ref<InstanceType<typeof MarketingSelectorBar>>();
/** 当前打开的创意 / Hook / 风格弹层。 */
const optionPicker = ref<PickDim | null>(null);
/** 选择栏收成「选项」按钮（窄屏）：这时本组件自己的「技能」按钮隐藏，技能在全屏面板里选。 */
const selectorNarrow = ref(false);
/** 服务端 42215 回过的选项（建会话失败时写入；key = 选项 code）：选项弹层据此置灰并写原因。 */
const serverBlocks = ref<Record<string, ServerOptionBlock>>({});
/** 建会话回 40001 `details.slot` 时标红的槽位；该槽内容一变就撤掉（用户已经在改了）。 */
const invalidSlot = ref<SlotKey | null>(null);
/** 建会话回 42215 时标红的胶囊（维度 + code）；那一维换了 / 删了就撤掉。 */
const invalidChip = ref<{ dim: PickDim; code: string } | null>(null);
const marketingPriceExtra = computed(() => {
  if (!isMarketing.value) return {};
  const code = mk.value.chips.find(chip => chip.dim === "skill")?.code;
  const max = props.marketingConfig?.maxVariants;
  // 是不是批量读技能目录的 isBatch（MKT-T-32），不按 code 认
  return isBatchSkill(code, prefsStore.sceneSkills.marketing) && max ? { variants: { max } } : {};
});

/** 生效的偏好：会话级（没有会话时是草稿）的完整快照；空的才用用户默认（见 utils/agentPrefs.effectivePrefs）。 */
const effectivePrefs = computed<AgentPrefsValue>(() => resolvePrefs(
  prefsStore.prefs,
  current.value ? current.value.session.prefs : draftPrefs.value
));
const reasoningOptions = computed(() => prefsStore.options("reasoning"));
const reasoningCode = computed(() => resolveReasoningModel(effectivePrefs.value, reasoningOptions.value));
const price = computed(() => agentPriceView({
  // 解析不出可用的推理模型时，拿所选（或第一个）不可用的那个来说明原因。
  reasoning: prefsStore.option("reasoning", reasoningCode.value)
    ?? prefsStore.option("reasoning", prefModel(effectivePrefs.value, "reasoning"))
    ?? reasoningOptions.value[0],
  video: prefsStore.option("video", prefModel(effectivePrefs.value, "video")),
  image: prefsStore.option("image", prefModel(effectivePrefs.value, "image"))
}, marketingPriceExtra.value));
const reasoningLoading = computed(() => !prefsStore.optionsByKind.reasoning && !prefsStore.optionErrors.reasoning);

const availability = computed(() => {
  if (guest.value) return { canSend: true, reason: "", hint: "" } as ReturnType<typeof composerAvailability>;
  const value = composerAvailability({
    role: role.value,
    isInitiator: current.value ? current.value.isInitiator : true,
    state: current.value?.state,
    pendingCardId: current.value?.pendingCardId,
    balance: balance.value,
    turnPrice: prefsStore.option("reasoning", reasoningCode.value)?.price
  });
  // 创造页的只读说法照设计 02 §8（这里还没有「对话」）。
  if (isCreate.value && role.value === "viewer") return { ...value, reason: "你在该工作区是只读，无法发起创作" };
  return value;
});
/** 权限 / 会话已结束：连正文都不让写（写了也发不出去）。余额不足只禁发送键。 */
const locked = computed(() => !availability.value.canSend && availability.value.shortfall === undefined);
const mode = computed(() => (isCanvas.value ? sendButtonMode(current.value?.state, agent.isStreaming) : "send"));
const uploading = computed(() => attachments.value.some(item => item.status === "uploading"));
const doneAttachments = computed(() => attachments.value.filter(item => item.status === "done" && item.referenceId));
const hasContent = computed(() => Boolean(text.value.trim()) || refs.value.length > 0 || doneAttachments.value.length > 0
  // 营销页：只选了商品 / 只选了胶囊也算写了东西（发不发得出去由槽位与选项的要求去判）。
  || (isMarketing.value && (mk.value.chips.length > 0 || SLOT_KEYS.some(key => slotsValue.value.items[key].length > 0))));
const canStop = computed(() => Boolean(current.value)
  && !current.value!.stopRequested
  && (current.value!.isInitiator || role.value === "owner" || role.value === "admin"));

/** 发送键不能点的原因（悬停）；能点为空串。 */
const blockReason = computed(() => {
  // 未登录：发送键可点，点了存草稿去登录。
  if (guest.value) return hasContent.value ? "" : "先写下想让 Agent 做的事";
  if (!availability.value.canSend) return availability.value.reason;
  if (busy.value) return "正在发送";
  if (uploading.value) return "附件上传中，完成后再发送";
  if (reasoningLoading.value) return "正在读取推理模型";
  if (price.value.error) return price.value.error;
  if (!hasContent.value) return "先写下想让 Agent 做的事";
  return "";
});

const placeholder = computed(() => {
  if (locked.value) return availability.value.reason;
  if (isMarketing.value) return "添加商品，写下核心卖点或创意方向，让 Agent 为你补全生成方案";
  // 画布面板不放占位提示（2026-11-01 按用户要求，设计 03 R-16）：@ 引用、/ 技能的用法在输入框上方轮播的小技巧里
  if (isCanvas.value) return "";
  return "描述你想做的视频，比如：三打白骨精，悟空视角，15 秒竖屏";
});
/**
 * 输入框右下角的每轮单价「✦N/轮」：画布面板不显示（2026-11-01 按用户要求，设计 03 R-16）。
 * 余额不够时照样有「去充值」和输入框下面那行原因；创造页、营销页照旧显示。
 */
const showPrice = computed(() => !isCanvas.value);

const pickerKind = computed<"/" | "@" | null>(() => trigger.value?.kind ?? manualPicker.value);

// 技能 / @ 弹层的摆放：打开时量一次输入框的位置，上方放不下就向下弹（见 utils/pickerPlacement）。
// 上界取顶栏下沿 —— 只按视口顶部算的话，弹层会钻到固定顶栏下面被裁掉。
const PICKER_PREFERRED = 300;
const composerBox = ref<HTMLElement>();
const pickerPlace = ref<PickerPlacement>({ side: "top", maxHeight: PICKER_PREFERRED });
function measurePicker() {
  const box = composerBox.value;
  if (!box || typeof window === "undefined") return;
  const rect = box.getBoundingClientRect();
  const topbar = document.querySelector(".portal-topbar")?.getBoundingClientRect().bottom ?? 0;
  pickerPlace.value = pickerPlacement({
    above: rect.top - Math.max(0, topbar),
    below: window.innerHeight - rect.bottom,
    preferred: PICKER_PREFERRED,
  });
}
const pickerStyle = computed(() => ({ "--picker-max": `${pickerPlace.value.maxHeight}px` }));
const pickerClass = computed(() => `picker-${pickerPlace.value.side}`);
watch(pickerKind, (kind) => {
  if (typeof window === "undefined") return;
  if (kind) {
    void nextTick(measurePicker);
    window.addEventListener("resize", measurePicker);
    window.addEventListener("scroll", measurePicker, true);
  } else {
    window.removeEventListener("resize", measurePicker);
    window.removeEventListener("scroll", measurePicker, true);
  }
});
onBeforeUnmount(() => {
  if (typeof window === "undefined") return;
  window.removeEventListener("resize", measurePicker);
  window.removeEventListener("scroll", measurePicker, true);
});
const mentionNodes = computed(() => (canvasNodes.value ? nodeCandidates(canvas.nodes, trigger.value?.kind === "@" ? trigger.value.query : "") : []));
const loadedSkillCodes = computed(() => current.value?.session.skillCodes ?? []);

/** 一键优化的价格：与对话同一个推理模型、同一个每轮单价。 */
const optimizePrice = computed(() => optimizePriceView(
  prefsStore.option("reasoning", reasoningCode.value)
    ?? prefsStore.option("reasoning", prefModel(effectivePrefs.value, "reasoning"))
    ?? reasoningOptions.value[0]
));
const optimizeBlock = computed(() => {
  if (guest.value) return "";
  if (locked.value) return availability.value.reason;
  if (busy.value) return "正在发送";
  if (reasoningLoading.value) return "正在读取推理模型";
  return "";
});

// ---------------------------------------------------------------- 选中即引用

function lookupNode(id: string) {
  if (!canvasNodes.value) return undefined;
  const node = canvas.nodes.find(item => item.id === id);
  return node ? nodeRefSource(node) : undefined;
}

watch(() => (canvasNodes.value ? [...canvas.selectedNodeIds] : []), ids => {
  refs.value = syncSelectedRefs(refs.value, ids, lookupNode);
}, { immediate: true });

watch(refs, list => {
  void thumbs.appendAll(list
    .filter(item => item.thumbMediaId)
    .map(item => ({ key: item.thumbMediaId!, url: mediaThumbUrl(item.thumbMediaId, 160) })));
});

async function loadBalance() {
  if (guest.value) return;
  try {
    // 契约是 `{ balance, grants }`（此前读老结构的 total，恒为 undefined，「积分不足」永远判不出来）
    balance.value = creditBalanceOf(await useApi()<unknown>("/credits"));
  } catch {
    balance.value = undefined;
  }
}

// 一轮跑完（回到空闲 / 等确认 / 失败）时余额可能变了：重新取一次（只读）。
watch(() => current.value?.state, (next, prev) => {
  if (prev && next !== prev && (next === "idle" || next === "failed" || next === "awaiting_approval" || next === "awaiting_input")) void loadBalance();
});

// 换会话：上一条没发出去的错误不再挂着。
watch(() => agent.currentId, () => {
  sendError.value = "";
});

// ---------------------------------------------------------------- 候选

function onTrigger(value: Trigger | null) {
  trigger.value = value;
  if (value) manualPicker.value = null;
}

function closePicker() {
  trigger.value = null;
  manualPicker.value = null;
}

function onPickerKey(key: "up" | "down" | "confirm" | "close") {
  const picker = pickerKind.value === "/" ? skillPicker.value : mentionPicker.value;
  if (key === "close") closePicker();
  else if (key === "confirm") picker?.confirm();
  else picker?.move(key === "down" ? 1 : -1);
}

function clearTriggerText() {
  if (trigger.value) editor.value?.replaceTrigger(trigger.value.start);
}

function selectSkill(item: AgentSkill) {
  clearTriggerText();
  if (isMarketing.value) {
    // 营销：技能带预填、槽位与模型绑定（MKT-T-11 的 applySkill），要清已添加内容时先问。
    closePicker();
    void chooseMarketingSkill(item).then(() => editor.value?.focus());
    return;
  }
  skill.value = { code: item.code, name: item.name };
  closePicker();
  editor.value?.focus();
}

function selectMention(item: Omit<ComposerRef, "auto">) {
  clearTriggerText();
  refs.value = addManualRef(refs.value, item);
  closePicker();
  editor.value?.focus();
}

async function openPicker(kind: "/" | "@") {
  if (await requireLogin()) return;
  if (locked.value) return;
  trigger.value = null;
  optionPicker.value = null;
  manualPicker.value = manualPicker.value === kind ? null : kind;
}

// ---------------------------------------------------------------- 附件

const referenceUpload = useReferenceUpload();

async function uploadOne(item: Attachment) {
  item.status = "uploading";
  item.error = undefined;
  try {
    item.referenceId = await referenceUpload.upload(item.file, { mediaType: item.mediaType, projectId: props.projectId || undefined });
    item.status = "done";
  } catch (cause) {
    item.status = "failed";
    item.error = quickErrorText(cause, navigator.onLine);
  }
}

async function pickFiles() {
  if (await requireLogin()) return;
  if (locked.value) return;
  fileInput.value?.click();
}

function onFiles(event: Event) {
  const input = event.target as HTMLInputElement;
  const picked = Array.from(input.files ?? []);
  input.value = "";
  for (const file of picked) {
    const mediaType = referenceMediaType(file);
    if (!mediaType) {
      ElMessage.error(`${file.name}：只支持图片、视频与音频`);
      continue;
    }
    const item = reactive({
      key: `${Date.now().toString(36)}_${Math.random().toString(36).slice(2, 7)}`,
      name: file.name,
      status: "uploading" as const,
      previewUrl: mediaType === "image" ? URL.createObjectURL(file) : undefined,
      file,
      mediaType
    }) as Attachment;
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

// ---------------------------------------------------------------- 偏好

async function changePrefs(next: AgentPrefsValue) {
  if (!current.value) {
    // 新会话草稿：只记下差异，建会话时随首条消息发出。
    draftPrefs.value = next;
    return;
  }
  try {
    await agent.patchSession({ prefs: next });
  } catch (cause) {
    ElMessage.error(quickErrorText(cause, navigator.onLine));
  }
}

async function saveDefault(next: AgentPrefsValue) {
  try {
    await prefsStore.saveDefault(next);
    ElMessage.success("已设为默认偏好，之后新建的会话都会用它");
  } catch (cause) {
    ElMessage.error(quickErrorText(cause, navigator.onLine));
  }
}

/**
 * 这次发送要带的偏好。会话偏好里没有可用的推理模型时，把解析出来的那一个写进去：服务端虽然会兜底
 * （按会员等级报得出价的第一个，04 文档 P-13），但按钮上显示的模型与价格要和实际用的一致。新会话带完整的生效偏好。
 */
function prefsForSend(): AgentPrefsValue | undefined {
  const code = reasoningCode.value;
  const base = effectivePrefs.value;
  if (!current.value) return code ? withPrefFields(base, "reasoning", { model: code }) : base;
  const session = current.value.session.prefs ?? {};
  // 会话级偏好是完整快照：空的（别处建的会话）或推理模型对不上时，整份写回生效的偏好。
  if (code && (Object.keys(session).length === 0 || prefModel(session, "reasoning") !== code)) {
    return withPrefFields(base, "reasoning", { model: code });
  }
  return undefined;
}

// ---------------------------------------------------------------- 未登录（创造页）

/** 这次输入里引用到的素材 ID（草稿只存 ID，GO-27）。 */
function draftMediaIds(): string[] {
  return [
    ...refs.value.filter(item => item.kind === "media").map(item => item.id),
    ...doneAttachments.value.map(item => item.referenceId!)
  ];
}

/** 未登录：存下已输入的内容去登录，回来恢复（设计 02 §8）。返回 true 表示已跳走。 */
async function requireLogin(): Promise<boolean> {
  if (!guest.value) return false;
  if (isMarketing.value) {
    // 营销页：存营销草稿（键与创造页分开），登录后回到营销页。
    saveMarketingDraftNow();
    await navigateTo(loginPath("/marketing"));
    return true;
  }
  if (import.meta.client) {
    saveCreateDraft(sessionStorage, { mode: "agent", text: text.value, ...(skill.value ? { skill: skill.value } : {}), mediaIds: draftMediaIds() });
  }
  await navigateTo(loginPath("/create"));
  return true;
}

async function optimizeGuard() {
  return !(await requireLogin());
}

/** 一键优化：用户选了「替换」。 */
function replaceText(value: string) {
  text.value = value;
  editor.value?.focus();
}

/**
 * 外部把一段话放进输入框、**不发送**（画布 Agent 面板空会话的推荐词，2026-11-01 按用户要求）：
 * 替换正文并聚焦（光标落在末尾），用户看过、改过再自己点发送。引用胶囊、附件、所选技能都不动。
 */
function fillText(value: string) {
  if (locked.value) return;
  replaceText(value);
}

function onEditorFocus() {
  if (guest.value) void requireLogin();
}

// ---------------------------------------------------------------- 发送 / 停止

/** 创造页这次发送意图的幂等键：连点、网络重试只建一个项目；内容一变就是新的意图（见下面的 watch）。 */
let createKey = "";
watch(() => JSON.stringify([text.value, skill.value?.code, refs.value.map(item => `${item.kind}:${item.id}`),
  doneAttachments.value.map(item => item.referenceId), draftPrefs.value]), () => {
  createKey = "";
});

/** 创造页发送：一次请求建项目 + 会话 + 首条消息（开发 03 §7），成功后跳画布。 */
async function submitCreate(outgoing: ReturnType<typeof serializeRefs>) {
  if (creating.value) return;
  creating.value = true;
  sendError.value = "";
  if (!createKey) createKey = globalThis.crypto?.randomUUID?.() ?? `${Date.now().toString(36)}-${Math.random().toString(36).slice(2)}`;
  try {
    const body = buildAgentSessionRequest({
      text: text.value,
      refs: outgoing,
      ...(skill.value ? { skillCode: skill.value.code } : {}),
      prefs: prefsForSend()
    });
    const result = await agent.createSession(body as AgentCreateSessionRequest, createKey);
    createKey = "";
    await navigateTo(agentLandingPath(result.projectId, result.sessionId, canvasOn.value));
  } catch (cause) {
    // ⚠️ 正文、技能、引用与附件都留着；原话放在输入框下方（断网说断网）。
    sendError.value = quickErrorText(cause, navigator.onLine);
    void loadBalance();
  } finally {
    creating.value = false;
  }
}

/** 这次营销发送要带的普通引用（附件与 `@`，只有 `{kind,id}`）。 */
function plainOutgoing() {
  return serializeRefs([
    ...refs.value,
    ...doneAttachments.value.map(item => ({ kind: "media" as const, id: item.referenceId!, label: item.name, auto: false }))
  ]);
}

function marketingRequestInput(): MarketingRequestInput {
  return { text: text.value, chips: mk.value.chips, slots: slotsValue.value, decls: mk.value.slots, refs: plainOutgoing(), prefs: prefsForSend() };
}

/** 营销发送意图的指纹与幂等键：内容不变的连点 / 重试复用同一个键（只建一个项目），内容一变换新键。 */
let marketingKey: { fingerprint: string; key: string } | null = null;

/**
 * 营销页发送（MKT-T-24，dev/03 §3.5）：一次请求建项目 + 营销会话 + 首条消息。
 * 成功 → 清营销草稿（并停掉之后的草稿写入，否则卸载时又把它写回去）→ 按画布开关跳画布页 / 全屏对话页。
 * 失败 → 输入全部保留；40001 `details.slot` 标红槽位、42215 标红胶囊并写进选项弹层。
 */
async function submitMarketing() {
  if (creating.value || marketingGate.value.disabled) return;
  creating.value = true;
  sendError.value = "";
  invalidSlot.value = null;
  invalidChip.value = null;
  const input = marketingRequestInput();
  const fingerprint = marketingSendFingerprint(input);
  if (marketingKey?.fingerprint !== fingerprint) {
    marketingKey = { fingerprint, key: globalThis.crypto?.randomUUID?.() ?? `${Date.now().toString(36)}-${Math.random().toString(36).slice(2)}` };
  }
  try {
    const result = await agent.createSession(buildMarketingSessionRequest(input), marketingKey.key);
    marketingKey = null;
    marketingDraftReady.value = false;
    clearTimeout(marketingDraftTimer);
    if (import.meta.client) clearMarketingDraft(sessionStorage);
    await navigateTo(agentLandingPath(result.projectId, result.sessionId, canvasOn.value));
  } catch (cause) {
    const failure = marketingSendFailure(cause, navigator.onLine);
    sendError.value = failure.text;
    if (failure.slot) invalidSlot.value = failure.slot;
    if (failure.option) {
      invalidChip.value = { dim: failure.option.dim, code: failure.option.code };
      serverBlocks.value = { ...serverBlocks.value, [failure.option.code]: failure.option.block };
    }
    void loadBalance();
  } finally {
    creating.value = false;
  }
}

// 用户动了被标红的那一处：撤掉标红（错误原话留着，直到下一次发送）。
watch(() => (invalidSlot.value ? slotsValue.value.items[invalidSlot.value] : null), (next, prev) => {
  if (prev && next !== prev) invalidSlot.value = null;
});
watch(() => mk.value.chips, chips => {
  const bad = invalidChip.value;
  if (bad && chips.find(chip => chip.dim === bad.dim)?.code !== bad.code) invalidChip.value = null;
});

async function submit() {
  // 运行中按回车不再停止（04 文档 P-07）：有内容就排队为下一轮，停止只认停止键（它直接调 stopRun）。
  if (composerSubmitAction(mode.value, hasContent.value) === "none") return;
  if (await requireLogin()) return;
  if (isMarketing.value) {
    await submitMarketing();
    return;
  }
  if (blockReason.value) return;
  if (!canSubmitContent({ hasText: Boolean(text.value.trim()), hasRefs: refs.value.length > 0 || doneAttachments.value.length > 0, uploading: uploading.value })) return;
  sendError.value = "";
  // ⚠️ 只有 {kind,id}：胶囊上的标题、缩略图都不出门。上传完成的附件以素材引用发出。
  const outgoing = serializeRefs([
    ...refs.value,
    ...doneAttachments.value.map(item => ({ kind: "media" as const, id: item.referenceId!, label: item.name, auto: false }))
  ]);
  if (isCreate.value) {
    await submitCreate(outgoing);
    return;
  }
  const prefs = prefsForSend();
  try {
    await agent.send(text.value, outgoing, {
      ...(skill.value ? { skillCode: skill.value.code } : {}),
      ...(prefs ? { prefs } : {})
    });
    // 发出去了才清：正文、技能、手动引用与附件。自动胶囊对应的节点还选着，保留。
    text.value = "";
    skill.value = null;
    refs.value = refs.value.filter(item => item.auto);
    for (const item of attachments.value) if (item.previewUrl) URL.revokeObjectURL(item.previewUrl);
    attachments.value = [];
    draftPrefs.value = {};
    closePicker();
    void loadBalance();
  } catch (cause) {
    // ⚠️ 正文、胶囊、附件都留着；原话放在输入框下方。
    sendError.value = quickErrorText(cause, navigator.onLine);
  }
}

async function stopRun() {
  if (!canStop.value) return;
  try {
    await agent.stop();
  } catch (cause) {
    ElMessage.error(quickErrorText(cause, navigator.onLine));
  }
}


// ---------------------------------------------------------------- 营销：胶囊与联动（MKT-T-14）

/** 调联动纯函数前的输入：正文、偏好取此刻的真实值（用户可能刚打过字、刚改过偏好）。 */
function marketingInput(): MarketingComposerState {
  return { ...mk.value, text: text.value, prefs: effectivePrefs.value };
}

/** 落下联动的结果：胶囊 / 槽位声明进状态，正文写回，偏好有变化才写进草稿偏好，技能胶囊同步给发送用的 `skill`。 */
function commitMarketing(next: MarketingComposerState, before: MarketingComposerState) {
  mk.value = next;
  if (next.text !== text.value) text.value = next.text;
  if (next.prefs !== before.prefs) draftPrefs.value = next.prefs;
  const chip = next.chips.find(item => item.dim === "skill");
  skill.value = chip ? { code: chip.code, name: chip.name } : null;
}

/**
 * 跑一次会切槽位的联动（选技能 / 删技能）：要清掉已添加的内容时先问一次，确认后才清（MKT-T-11 的 `needsConfirm`）。
 * 返回是否生效。
 */
async function runSwitch(run: (state: MarketingComposerState, confirmed: boolean) => SwitchResult): Promise<boolean> {
  let before = marketingInput();
  let result = run(before, false);
  if (result.needsConfirm?.length) {
    try {
      await ElMessageBox.confirm(conflictText(result.needsConfirm), "清空已添加的内容？", {
        confirmButtonText: "继续",
        cancelButtonText: "取消",
        closeOnClickModal: false
      });
    } catch {
      return false;
    }
    before = marketingInput();
    result = run(before, true);
    if (result.needsConfirm?.length) return false;
  }
  commitMarketing(result.state, before);
  if (result.clearSlots.length) slotsRef.value?.clear(result.clearSlots);
  return true;
}

function chooseMarketingSkill(item: AgentSkill): Promise<boolean> {
  return runSwitch((state, confirmed) => applySkillToState(state, item, { confirmed }));
}

/** 选创意（弹层、模板墙同一条路）：默认技能按 code 从营销技能目录里找；未登录取不到目录就只填创意（B-5）。 */
async function applyCreativeOption(option: MarketingOption) {
  let defaultSkill: AgentSkill | null = null;
  if (option.defaultSkillCode && !guest.value) {
    try {
      const list = await prefsStore.loadSkills(false, "marketing");
      defaultSkill = list.find(item => item.code === option.defaultSkillCode) ?? null;
    } catch {
      defaultSkill = null;
    }
  }
  const before = marketingInput();
  const result = applyCreative(before, option, defaultSkill);
  commitMarketing(result.state, before);
  // 自动带入放弃时说一声：用户看到创意胶囊却没有技能胶囊，会以为点漏了。其余结果胶囊本身已经说明了。
  if (result.autoSkill === "skipped_conflict" && defaultSkill) {
    ElMessage.info(`没有自动带入技能「${defaultSkill.name}」：它放不下已添加的内容，需要的话可手动选择`);
  }
}

async function pickMarketingOption(option: MarketingOption) {
  if (option.kind === "creative") {
    await applyCreativeOption(option);
  } else {
    const before = marketingInput();
    commitMarketing(applyPick(before, option.kind as PickDim, option), before);
  }
  focusQuietly();
}

function toggleOptionPicker(dim: PickDim) {
  if (locked.value) return;
  closePicker();
  optionPicker.value = optionPicker.value === dim ? null : dim;
}

function onOptionSelect(option: MarketingOption) {
  optionPicker.value = null;
  void pickMarketingOption(option);
}

/** 未登录时不聚焦正文：聚焦会触发「去登录」，而填入本身不需要登录（模板墙，MKT-T-15）。 */
function focusQuietly() {
  if (!guest.value) void nextTick(() => editor.value?.focus());
}

const marketingSelected = (dim: PickDim) => mk.value.chips.find(chip => chip.dim === dim)?.code ?? null;
/** 选项可用性的上下文（MKT-T-13 `optionBlock`）：当前视频模型、所选商品的图片张数、服务端结论。 */
const optionCtx = computed(() => ({
  videoModel: currentVideoModel(prefModel(effectivePrefs.value, "video"), prefsStore.options("video")),
  productCount: slotsValue.value.items.product.reduce((sum, item) => sum + (item.status === "done" ? (item.imageCount ?? 1) : 0), 0),
  // 素材裂变：源视频本身就是商品视频，「需要商品图」的选项算满足（04 文档 P-47）。
  sourceVideo: slotsValue.value.items.source_video.some(item => item.status === "done"),
  serverBlocks: serverBlocks.value
}));

function onSlotsFilled(filled: MarketingComposerState["filled"]) {
  mk.value = { ...mk.value, filled };
}

/** 未登录点槽位：先去登录（上传要登录），草稿照存。捕获阶段拦下，槽位组件里的按钮收不到这次点击。 */
function onSlotsClickCapture(event: Event) {
  if (!guest.value) return;
  event.preventDefault();
  event.stopPropagation();
  void requireLogin();
}

/**
 * 营销发送键的理由（MKT-F-17、MKT-AC-04）：最根本的那一条在前 ——
 * Agent 关着 > 只读 > 上传中 > 必填槽「请选择商品」> 选项的输入要求 > 上传失败 > Agent 模式原有判定。
 */
const marketingGate = computed(() => {
  if (!props.marketingAvailable) return { disabled: true, reason: "营销 Agent 暂时无法发送" };
  const extra = slotsFailedText(slotsValue.value) || blockReason.value;
  return marketingSendGate({ ...mk.value, text: text.value }, {
    readOnly: role.value === "viewer",
    uploading: slotsUploading.value || uploading.value,
    base: { disabled: Boolean(extra), reason: extra }
  });
});
/** 发送键实际用的理由：通用场景就是 `blockReason`（一字不差），营销场景走上面的闸。 */
const sendBlock = computed(() => (isMarketing.value && !guest.value ? marketingGate.value.reason : blockReason.value));
/**
 * 输入框下方那一行说明：只说**用户自己能补的**营销要求（缺商品、选项要求、上传失败），
 * 其余原因在发送键的悬停里。
 */
const marketingNote = computed(() => {
  if (!isMarketing.value || guest.value || locked.value) return "";
  const gate = marketingGate.value as { disabled: boolean; reason: string; code?: string };
  if (!gate.disabled) return "";
  if (gate.code === "slot_required" || gate.code === "pick_requirement") return gate.reason;
  const fixable = slotsFailedText(slotsValue.value);
  return fixable && gate.reason === fixable ? gate.reason : "";
});

// ---------------------------------------------------------------- 营销：草稿（MKT-UX-11）

/** 恢复完草稿之后才开始写 —— 否则挂载那一刻的空状态会先把草稿删掉。 */
const marketingDraftReady = ref(false);
let marketingDraftTimer: ReturnType<typeof setTimeout> | undefined;

function saveMarketingDraftNow() {
  clearTimeout(marketingDraftTimer);
  if (!import.meta.client || !isMarketing.value || !marketingDraftReady.value) return;
  saveMarketingDraft(sessionStorage, marketingDraftOf({ state: mk.value, text: text.value, slots: slotsValue.value, prefs: draftPrefs.value }));
}

watch(() => (isMarketing.value && marketingDraftReady.value
  ? [text.value, mk.value.chips, mk.value.lastPrefill, mk.value.suppressAutoSkill, mk.value.pickRequirements, mk.value.skillPrefs, slotsValue.value, draftPrefs.value]
  : null), value => {
  if (!value) return;
  clearTimeout(marketingDraftTimer);
  marketingDraftTimer = setTimeout(saveMarketingDraftNow, MARKETING_DRAFT_DEBOUNCE_MS);
}, { deep: true });

function draftLocalId(): string {
  return `d_${Date.now().toString(36)}_${Math.random().toString(36).slice(2, 8)}`;
}

/**
 * 进页面恢复草稿（刷新、登录回跳）。技能的槽位声明草稿里没有（只存 code + name），按营销技能目录重算；
 * 素材**按工作区重新校验归属**（`GET /media/{id}/info`，只读）—— 草稿可能是换账号 / 换工作区之前存的。
 */
async function restoreMarketingDraft() {
  const draft = import.meta.client ? readMarketingDraft(sessionStorage) : undefined;
  if (!draft) return;
  const base = initialComposerState(props.marketingConfig?.slots ?? []);
  const before = marketingInput();
  commitMarketing({ ...composerStateFromDraft(base, draft), filled: mk.value.filled, prefs: before.prefs }, before);
  draftPrefs.value = draft.prefs;
  slotsValue.value = slotsValueFromDraft(draft, draftLocalId);
  // 旧结构的草稿（商品图 / 名称卖点 / 上传的人物图）已被丢弃：说一次，不静默丢（MKT-F-133）。
  if (draft.droppedLegacy) ElMessage.warning(MARKETING_LEGACY_DRAFT_TEXT);
  if (draft.pendingUploads) ElMessage.warning(pendingUploadsText(draft.pendingUploads));
  if (guest.value) return;

  const skillChip = draft.chips.find(chip => chip.dim === "skill");
  if (skillChip) {
    try {
      const found = (await prefsStore.loadSkills(false, "marketing")).find(item => item.code === skillChip.code);
      if (found?.inputSlots.length) mk.value = { ...mk.value, slots: found.inputSlots };
    } catch {
      // 目录取不到：槽位按场景默认，发送时服务端仍会按技能校验。
    }
  }

  const mediaIds = SLOT_KEYS.flatMap(key => slotsValue.value.items[key]).filter(item => item.kind === "media" && item.id).map(item => item.id!);
  const dropped = new Set<string>();
  await Promise.all(mediaIds.map(async id => {
    try {
      await useApi()(`/media/${encodeURIComponent(id)}/info`);
    } catch {
      dropped.add(id);
    }
  }));
  await refreshDraftProduct();
  if (dropped.size) {
    const current = slotsValue.value;
    slotsValue.value = {
      ...current,
      items: Object.fromEntries(SLOT_KEYS.map(key => [key, current.items[key].filter(item => !(item.kind === "media" && item.id && dropped.has(item.id)))])) as MarketingSlotsValue["items"]
    };
    ElMessage.warning(`有 ${dropped.size} 个素材已不可用，已从草稿中移除`);
  }
}

/**
 * 草稿里的商品按当前工作区再取一次（只读）：已被删除 / 换了工作区的从卡片移除并说明；还在的同步名称、主图与张数
 * （草稿存的是当时的快照，别人可能改过）。取不到（断网、旧后端）就原样留着 —— 发送时服务端还会校验归属。
 */
async function refreshDraftProduct() {
  const id = marketingSelectedProductId(slotsValue.value);
  if (!id) return;
  try {
    const product = await useApi()<MarketingPickedProduct>(`/marketing/products/${encodeURIComponent(id)}`);
    slotsValue.value = marketingSyncProduct(slotsValue.value, { kind: "updated", product }).value;
  } catch (cause) {
    if ((cause as { statusCode?: number }).statusCode !== 404) return;
    slotsValue.value = marketingSyncProduct(slotsValue.value, { kind: "deleted", id }).value;
    ElMessage.warning("草稿里的商品已不存在，请重新选择");
  }
}

// ---------------------------------------------------------------- 营销：商品 / 角色弹窗（MKT-T-38）

const productPickerOpen = ref(false);
const productLimits = computed(() => marketingProductLimits(props.marketingConfig));
const slotLimits = computed(() => ({
  maxProductsPerSession: props.marketingConfig?.maxProductsPerSession ?? null,
  maxCharacterRefs: props.marketingConfig?.maxCharacterRefs ?? null
}));

/** 点了商品 / 角色卡：打开对应的选择弹窗（MKT-T-38 / MKT-T-39）。 */
function onOpenPicker(slot: SlotKey) {
  if (guest.value) {
    void requireLogin();
    return;
  }
  closePicker();
  optionPicker.value = null;
  if (slot === "product") productPickerOpen.value = true;
  else if (slot === "character") characterPickerOpen.value = true;
}

// ---------------------------------------------------------------- 营销：角色弹窗（MKT-T-39）

const characterPickerOpen = ref(false);
const characterLimits = computed(() => marketingCharacterLimits(props.marketingConfig));
const pickedCharacters = computed(() => marketingPickedFromSlot(slotsValue.value.items.character));

/** 弹窗「添加」：角色槽整体换成这次的选择（库角色写 `marketing_character`、脚本资产写 `asset`）。 */
function onCharacterPick(picked: MarketingPickedCharacter[]) {
  characterPickerOpen.value = false;
  slotsValue.value = { ...slotsValue.value, items: { ...slotsValue.value.items, character: marketingCharacterSlotItems(picked, draftLocalId) } };
}

function onCharacterChanged(change: { kind: "updated"; character: MarketingPickedCharacter } | { kind: "deleted"; id: string }) {
  const result = marketingSyncCharacter(slotsValue.value.items.character, change);
  if (!result.changed) return;
  slotsValue.value = { ...slotsValue.value, items: { ...slotsValue.value.items, character: result.items } };
  if (result.removed) ElMessage.info(MARKETING_CHARACTER_REMOVED_TEXT);
}

function onProductPick(product: MarketingPickedProduct | null) {
  productPickerOpen.value = false;
  slotsValue.value = marketingSetProduct(slotsValue.value, product, draftLocalId());
}

function onProductChanged(change: { kind: "updated"; product: MarketingPickedProduct } | { kind: "deleted"; id: string }) {
  const result = marketingSyncProduct(slotsValue.value, change);
  if (!result.changed) return;
  slotsValue.value = result.value;
  if (result.removed) ElMessage.info(MARKETING_PRODUCT_REMOVED_TEXT);
}

// 配置重新取到（页面「重试」）时更新场景默认槽位；没有技能时当前槽位跟着走。
watch(() => props.marketingConfig?.slots, slots => {
  if (!slots) return;
  const hasSkill = mk.value.chips.some(chip => chip.dim === "skill");
  mk.value = { ...mk.value, defaultSlots: slots, ...(hasSkill ? {} : { slots }) };
});

/** 模板墙 → 输入框的填入通道（MKT-T-15）；只填入、不发送。恢复草稿之后才注册，否则草稿会把刚填的创意盖掉。 */
const marketingFill = useMarketingFill();
let stopMarketingFill: (() => void) | undefined;

function onPageHide() {
  saveMarketingDraftNow();
}

// ---------------------------------------------------------------- 对外

function focus() {
  editor.value?.focus();
}

/** 外部加一个引用（如画布右键「问 Agent」）：手动胶囊，取消选中不会带走它。 */
function addRef(item: Omit<ComposerRef, "auto">) {
  refs.value = addManualRef(refs.value, item);
}

/** 按节点 ID 加引用（查不到的节点忽略）。 */
function addNodeRef(nodeId: string) {
  const source = lookupNode(nodeId);
  if (source) addRef({ kind: "node", id: source.id, label: source.label, thumbMediaId: source.thumbMediaId, hint: source.hint });
}

function removeChip(item: ComposerRef) {
  refs.value = removeRef(refs.value, item);
}

/** 输入框开头的胶囊：通用场景只有技能一维（MKT-T-11；创造页 / 画布面板行为与单技能时代一致）；营销场景四维。 */
const editorChips = computed<ComposerChip[]>(() => {
  if (isMarketing.value) return mk.value.chips;
  return skill.value ? [{ dim: "skill", code: skill.value.code, name: skill.value.name }] : [];
});

function onRemoveChip(dim: ComposerChipDim) {
  if (isMarketing.value) {
    void runSwitch((state, confirmed) => dropChip(state, dim, { confirmed }));
    return;
  }
  if (dim === "skill") skill.value = null;
}

/** 选一个技能（创造页快捷入口）；null = 不指定技能。 */
function setSkill(value: { code: string; name: string } | null) {
  skill.value = value;
}

/**
 * 登录回来恢复草稿（创造页）：技能照原样；素材**按工作区重新校验归属**（`GET /media/{id}/info`，只读），
 * 校验不过的丢弃并提示 —— 草稿是登录前存的，换了账号 / 工作区之后那些 ID 不一定还属于他。
 */
async function restoreDraft(draft: { skill?: { code: string; name: string }; mediaIds: string[] }) {
  if (draft.skill) skill.value = draft.skill;
  let dropped = 0;
  for (const id of draft.mediaIds) {
    try {
      const info = await useApi()<{ id?: string; kind?: string }>(`/media/${encodeURIComponent(id)}/info`);
      refs.value = addManualRef(refs.value, { kind: "media", id, label: "素材", thumbMediaId: info.kind === "image" ? id : undefined, hint: info.kind });
    } catch {
      dropped += 1;
    }
  }
  if (dropped) ElMessage.warning(`有 ${dropped} 个附件已不可用，已从草稿中移除`);
}

// 还没有可用的推理模型时（多半是页面打开时后台还没配好），用户一开始输入就重取一次（5 秒节流），
// 而不是让发送键一直灰到刷新页面。只读请求，不改任何数据。
let lastReasoningRetry = 0;
watch(text, () => {
  if (guest.value) return;
  if (reasoningOptions.value.some(option => option.available)) return;
  const now = Date.now();
  if (now - lastReasoningRetry < 5000) return;
  lastReasoningRetry = now;
  void prefsStore.loadOptions("reasoning");
});

onMounted(() => {
  if (isCreate.value) canvasOn.value = readCanvasToggle(localStorage);
  if (isMarketing.value) {
    // 同步部分（正文、胶囊、槽位项）在第一个 await 之前就落下了：此后才开始写草稿、才接模板墙的填入
    // （先接的话草稿会把刚填的创意盖掉）。技能槽位与素材归属的异步校验结束后照常经防抖写回。
    void restoreMarketingDraft();
    marketingDraftReady.value = true;
    stopMarketingFill = marketingFill.onFill(option => {
      void applyCreativeOption(option).then(focusQuietly);
    });
    window.addEventListener("pagehide", onPageHide);
  }
  // 创造页未登录：什么都不取（取了也是 401）。
  if (guest.value) return;
  // 只读：偏好、推理模型（含每轮单价）。视频 / 图片目录等打开偏好浮层或用户选过时再取。
  void prefsStore.load();
  void prefsStore.loadOptions("reasoning");
  void loadBalance();
  if (prefModel(effectivePrefs.value, "video")) void prefsStore.loadOptions("video");
  if (prefModel(effectivePrefs.value, "image")) void prefsStore.loadOptions("image");
});

watch(() => [prefModel(effectivePrefs.value, "video"), prefModel(effectivePrefs.value, "image")], ([videoModel, imageModel]) => {
  if (guest.value) return;
  if (videoModel) void prefsStore.loadOptions("video");
  if (imageModel) void prefsStore.loadOptions("image");
});

onBeforeUnmount(() => {
  for (const item of attachments.value) if (item.previewUrl) URL.revokeObjectURL(item.previewUrl);
  if (isMarketing.value) {
    // 离开营销页：防抖里还没写的那一次立刻写掉。
    saveMarketingDraftNow();
    stopMarketingFill?.();
    window.removeEventListener("pagehide", onPageHide);
  }
});

/** 打开模型偏好浮层（全屏对话页顶栏的「偏好」按钮）。 */
function openPrefs() {
  prefPopover.value?.show();
}

/** 营销页芯片：选中技能（插胶囊 + 联动），不发送（MKT-F-21）。 */
async function applySkill(item: AgentSkill) {
  if (await requireLogin()) return;
  if (locked.value) return;
  closePicker();
  optionPicker.value = null;
  if (await chooseMarketingSkill(item)) editor.value?.focus();
}

/** 营销页模板墙以外的入口填入创意（与 `useMarketingFill` 同一条路）。 */
async function applyCreativeFromPage(option: MarketingOption) {
  if (locked.value) return;
  await applyCreativeOption(option);
  focusQuietly();
}

/** 营销页芯片的「全部技能 →」：打开技能弹层（「全部」分类）；窄屏打开选项面板的技能页签。 */
async function openSkills() {
  if (await requireLogin()) return;
  if (locked.value) return;
  optionPicker.value = null;
  if (selectorNarrow.value) {
    selectorBar.value?.openPanel("skill");
    return;
  }
  trigger.value = null;
  manualPicker.value = "/";
}

defineExpose({ focus, fillText, addRef, addNodeRef, setSkill, restoreDraft, openPrefs, applySkill, applyCreative: applyCreativeFromPage, openSkills });
</script>

<template>
  <section class="agent-mode-composer" :class="{ locked, canvas: isCanvas, marketing: isMarketing }" aria-label="与 Agent 对话">
    <p v-if="availability.hint && !locked" class="composer-hint" role="status">{{ availability.hint }}</p>
    <div ref="composerBox" class="composer-box" :class="{ 'with-optimize': isCreate }">
      <OptimizePromptButton
        v-if="isCreate"
        class="composer-optimize"
        :text="text"
        :model-code="reasoningCode"
        :price="optimizePrice"
        :block-reason="optimizeBlock"
        :guard="optimizeGuard"
        @replace="replaceText"
        @charged="loadBalance"
      />
      <SkillPicker
        v-if="pickerKind === '/'"
        ref="skillPicker"
        :class="pickerClass"
        :style="pickerStyle"
        :query="trigger?.kind === '/' ? trigger.query : undefined"
        :loaded-codes="loadedSkillCodes"
        :scene="isMarketing ? 'marketing' : undefined"
        @select="selectSkill"
        @close="closePicker"
      />
      <MentionPicker
        v-else-if="pickerKind === '@'"
        ref="mentionPicker"
        :class="pickerClass"
        :style="pickerStyle"
        :query="trigger?.kind === '@' ? trigger.query : ''"
        :nodes="mentionNodes"
        @select="selectMention"
        @close="closePicker"
      />
      <!-- 营销：创意 / Hook / 风格弹层（与技能 / @ 弹层同一个锚点，以整个输入框为宽） -->
      <MarketingOptionPicker
        v-if="isMarketing && optionPicker && marketingConfig"
        :key="optionPicker"
        :kind="optionPicker"
        :categories="marketingConfig.categories[optionPicker]"
        :model-filters="marketingConfig.creativeModelFilters"
        :ctx="optionCtx"
        :selected-code="marketingSelected(optionPicker)"
        @select="onOptionSelect"
        @close="optionPicker = null"
      />
      <!-- 营销：槽位区在正文左侧（窄时移到正文上方）；通用场景这两层 display: contents，不影响布局 -->
      <div class="composer-body" :class="{ 'with-slots': isMarketing }">
        <div v-if="isMarketing" class="composer-slots" :class="invalidSlot ? `invalid-${invalidSlot}` : ''" @click.capture="onSlotsClickCapture">
          <MarketingSlots
            ref="slotsRef"
            v-model="slotsValue"
            :decls="mk.slots"
            :limits="slotLimits"
            :disabled="locked"
            :project-id="projectId"
            @filled="onSlotsFilled"
            @uploading="value => slotsUploading = value"
            @open-picker="onOpenPicker"
          />
          <ProductPickerDialog
            v-if="productPickerOpen"
            :selected-id="marketingSelectedProductId(slotsValue)"
            :limits="productLimits"
            :read-only="role === 'viewer'"
            @close="productPickerOpen = false"
            @pick="onProductPick"
            @deleted="id => onProductChanged({ kind: 'deleted', id })"
            @updated="product => onProductChanged({ kind: 'updated', product })"
          />
          <CharacterPickerDialog
            v-if="characterPickerOpen"
            :selected="pickedCharacters"
            :limits="characterLimits"
            :read-only="role === 'viewer'"
            :optimize-price="optimizePrice"
            :optimize-model-code="reasoningCode"
            :default-model-code="prefModel(effectivePrefs, 'image') ?? ''"
            @close="characterPickerOpen = false"
            @pick="onCharacterPick"
            @deleted="id => onCharacterChanged({ kind: 'deleted', id })"
            @updated="character => onCharacterChanged({ kind: 'updated', character })"
          />
        </div>
        <div class="composer-main" :class="invalidChip ? `invalid-${invalidChip.dim}` : ''">
      <ComposerEditor
        ref="editor"
        v-model="text"
        :placeholder="placeholder"
        :disabled="locked"
        :chips="editorChips"
        :picker-open="Boolean(pickerKind)"
        triggers
        @submit="submit"
        @trigger="onTrigger"
        @picker-key="onPickerKey"
        @remove-chip="onRemoveChip"
        @focus="onEditorFocus"
      />
      <AttachmentChips
        :items="attachments"
        :refs="refs"
        :thumbs="thumbs.urls.value"
        :disabled="busy"
        @remove="removeAttachment"
        @retry="retryAttachment"
        @remove-ref="removeChip"
        @open-ref="item => emit('open-ref', item)"
      />
        </div>
      </div>
      <div class="composer-bar" :class="{ compact: iconTools }">
        <div class="composer-group left">
          <button type="button" class="composer-icon" :disabled="locked" title="上传图片 / 视频 / 音频" aria-label="上传" @click="pickFiles"><Plus :size="18" /></button>
          <input ref="fileInput" type="file" class="composer-file" accept="image/*,video/*,audio/*" multiple @change="onFiles">
          <button type="button" class="composer-icon" :class="{ active: pickerKind === '@' }" :disabled="locked" :title="canvasNodes ? '引用画布节点、素材或角色资产' : '引用素材或角色资产'" aria-label="引用" @click="openPicker('@')"><AtSign :size="16" /></button>
          <!-- 画布面板：模型、Skill 两个图标（悬停说明见样式里的 [data-tip]）；点开的浮层与胶囊时完全一样 -->
          <template v-if="iconTools">
            <ModelPrefPopover v-if="!guest" ref="prefPopover" icon-only :prefs="effectivePrefs" :disabled="locked" :up="isCanvas" @change="changePrefs" @save-default="saveDefault" />
            <button
              type="button"
              class="composer-icon"
              :class="{ active: skill || pickerKind === '/' }"
              :disabled="locked"
              data-tip="Skill"
              :aria-label="skill ? `技能：${skill.name}` : '选择技能（可不选）'"
              :aria-expanded="pickerKind === '/'"
              @click="openPicker('/')"
            ><Zap :size="16" /></button>
          </template>
          <!-- 创造页：模式下拉（Agent / 视频 / 图片 / 音频） -->
          <slot name="mode" />
        </div>
        <div v-if="!iconTools" class="composer-group center">
          <button
            v-if="!(isMarketing && selectorNarrow)"
            type="button"
            class="composer-pill"
            :class="{ active: skill || pickerKind === '/' }"
            :disabled="locked"
            :title="skill ? `技能：${skill.name}` : '选择技能（可不选）'"
            @click="openPicker('/')"
          >
            <Zap :size="14" />
            <span class="composer-pill-text">技能</span>
            <ChevronDown :size="13" class="composer-pill-chevron" />
          </button>
          <!-- 营销：技能之后「创意 / Hook / 风格」；窄屏收成一个「选项」按钮 + 全屏面板（含技能页签） -->
          <MarketingSelectorBar
            v-if="isMarketing"
            ref="selectorBar"
            :chips="mk.chips"
            :open="optionPicker"
            :disabled="locked"
            @toggle="toggleOptionPicker"
            @narrow="value => selectorNarrow = value"
          >
            <template #panel="{ dim, close }">
              <SkillPicker
                v-if="dim === 'skill'"
                embedded
                scene="marketing"
                @select="item => { close(); void chooseMarketingSkill(item); }"
                @close="close"
              />
              <MarketingOptionPicker
                v-else-if="marketingConfig"
                :key="dim"
                embedded
                :kind="dim"
                :categories="marketingConfig.categories[dim]"
                :model-filters="marketingConfig.creativeModelFilters"
                :ctx="optionCtx"
                :selected-code="marketingSelected(dim)"
                @select="option => { close(); void pickMarketingOption(option); }"
                @close="close"
              />
            </template>
          </MarketingSelectorBar>
          <ModelPrefPopover v-if="!guest" ref="prefPopover" :prefs="effectivePrefs" :disabled="locked" :up="isCanvas" @change="changePrefs" @save-default="saveDefault" />
        </div>
        <div class="composer-group right">
          <button
            v-if="isCreate"
            type="button"
            role="switch"
            class="composer-canvas-toggle"
            :class="{ on: canvasOn }"
            :aria-checked="canvasOn"
            :title="canvasOn ? '发送后打开画布（Agent 面板在右侧）' : '发送后进入全屏对话，不打开画布'"
            @click="setCanvasOn(!canvasOn)"
          >
            <span class="composer-switch" aria-hidden="true"><i /></span>
            <span>画布</span>
          </button>
          <span v-if="isCreate" class="composer-divider" aria-hidden="true" />
          <template v-if="!guest">
            <PricePill v-if="showPrice" :agent="price" :up="isCanvas" />
            <NuxtLink v-if="availability.shortfall" to="/credits" class="composer-recharge">去充值</NuxtLink>
            <span v-if="showPrice || availability.shortfall" class="composer-divider" aria-hidden="true" />
          </template>
          <button
            v-if="mode === 'stop'"
            type="button"
            class="composer-submit stop"
            :disabled="!canStop"
            :title="current?.stopRequested ? '已请求停止，正在收尾' : (canStop ? '停止' : '只有发起人或管理员能停止')"
            aria-label="停止"
            @click="stopRun"
          ><Square :size="12" fill="currentColor" /></button>
          <button
            v-else
            type="button"
            class="composer-submit"
            :disabled="Boolean(sendBlock)"
            :title="guest ? '登录后发送' : (sendBlock || '发送')"
            :aria-label="sendBlock || '发送'"
            @click="submit"
          >
            <LoaderCircle v-if="busy" :size="16" class="spin" />
            <ArrowUp v-else :size="17" />
          </button>
        </div>
      </div>
    </div>
    <p v-if="sendError" class="composer-error" role="alert">{{ sendError }}</p>
    <p v-else-if="locked" class="composer-note">{{ availability.reason }}</p>
    <p v-else-if="availability.shortfall" class="composer-note">{{ availability.reason }}</p>
    <p v-else-if="marketingNote" class="composer-note">{{ marketingNote }}</p>
  </section>
</template>

<style scoped>
/*
 * 统一输入框（照对标改版）：大圆角深色卡片，上方多行正文，下方一行三组工具栏 ——
 * 左「＋ @」、中「技能 ⌄」「模型｜创作偏好 ⌄」居中、右「✦价格 ｜ 发送」。
 * 宽度按**输入框自身**判断（容器查询），画布右侧面板（320~640px）里中组换到第一行、
 * 再窄时收起胶囊文字，不靠视口宽度。
 */
.agent-mode-composer { position: relative; z-index: 2; width: 100%; text-align: left; container: composer / inline-size; }
.composer-box {
  position: relative;
  border: 1px solid var(--portal-line);
  border-radius: 20px;
  padding: 14px 14px 10px 16px;
  background: var(--portal-surface);
  transition: border-color .15s ease;
}
.agent-mode-composer.canvas .composer-box { border-radius: 16px; padding: 12px 10px 8px 12px; }
/* 创造页：与直接生成模式同一张大卡片（AgentComposer 的 .agent-composer） */
.agent-mode-composer:not(.canvas) .composer-box {
  border-radius: 22px;
  padding: 16px 16px 12px 18px;
  box-shadow: 0 10px 30px var(--portal-shadow);
}
.agent-mode-composer:not(.canvas) .composer-box :deep(.composer-editor) { min-height: 88px; }
/* 右上角「一键优化」：正文给它让出位置 */
.composer-optimize { position: absolute; top: 10px; right: 10px; z-index: 3; }
.composer-box.with-optimize :deep(.composer-editor-wrap) { padding-right: 30px; }
.composer-box:focus-within { border-color: var(--portal-line-strong); }
.composer-hint { margin: 0 0 6px; color: var(--portal-gold); font-size: 12px; }
.composer-bar {
  display: grid;
  /* 技能与模型选择紧跟在左组之后（2026-10-25 按用户要求靠左；此前左右两列等宽、中组被挤在正中），价格与发送贴右。 */
  grid-template-columns: max-content minmax(0, 1fr) max-content;
  grid-template-areas: "left center right";
  align-items: center;
  gap: 8px;
  margin-top: 10px;
}
.composer-group { min-width: 0; display: flex; align-items: center; gap: 4px; }
.composer-group.left { grid-area: left; }
.composer-group.center { grid-area: center; justify-content: flex-start; gap: 6px; }
.composer-group.right { grid-area: right; justify-content: flex-end; gap: 6px; }
/* 窄：中组占第一行，左右组在第二行两端 */
@container composer (max-width: 560px) {
  .composer-bar {
    grid-template-columns: auto minmax(0, 1fr);
    grid-template-areas: "center center" "left right";
    row-gap: 6px;
  }
  .composer-group.center { justify-content: flex-start; }
}
/* 更窄：胶囊只留图标 + 模型名，「技能」「创作偏好」字样收起（悬停仍有完整说明） */
@container composer (max-width: 360px) {
  .composer-pill-text, .composer-group.center :deep(.model-pref-tail), .composer-group.center :deep(.model-pref-sep) { display: none; }
}
/* 画布面板（iconTools）：技能、模型是左组里的图标，没有中组 —— 不论多窄都是一行，左「＋ @ 模型 Skill」、右「✦价格 ｜ 发送」 */
.composer-bar.compact { display: flex; justify-content: space-between; }
/*
 * 图标的悬停说明（2026-11-01 按参考图）：鼠标移上去立刻在图标正上方出一个小标签，不等系统 title 的延迟。
 * 浮层打开着（aria-expanded）时不出，免得压在浮层边上。
 */
.composer-bar :deep([data-tip]) { position: relative; }
.composer-bar :deep([data-tip]:hover:not(:disabled)::after),
.composer-bar :deep([data-tip]:focus-visible::after) {
  content: attr(data-tip);
  position: absolute;
  left: 50%;
  bottom: calc(100% + 8px);
  z-index: 5;
  border: 1px solid var(--portal-line);
  border-radius: 6px;
  padding: 3px 8px;
  color: var(--portal-text);
  background: var(--portal-elevated);
  box-shadow: 0 4px 14px var(--portal-shadow);
  font-size: 12px;
  line-height: 1.5;
  white-space: nowrap;
  transform: translateX(-50%);
  pointer-events: none;
}
.composer-bar :deep([data-tip][aria-expanded="true"]::after) { display: none; }
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
.composer-icon.active, .composer-icon:hover:not(:disabled) { color: var(--portal-text); background: var(--portal-hover); }
.composer-icon:disabled { color: var(--portal-disabled-text); cursor: not-allowed; }
.composer-pill {
  flex: none;
  height: 30px;
  display: inline-flex;
  align-items: center;
  gap: 5px;
  border: 0;
  border-radius: 999px;
  padding: 0 10px;
  color: var(--portal-text-soft);
  background: var(--portal-surface-2);
  font: inherit;
  font-size: 12px;
  white-space: nowrap;
  cursor: pointer;
}
.composer-pill:hover:not(:disabled) { color: var(--portal-text); background: var(--portal-surface-3); }
.composer-pill.active { color: var(--portal-text); background: var(--portal-active); }
.composer-pill:disabled { color: var(--portal-disabled-text); cursor: not-allowed; }
.composer-pill-chevron { color: var(--portal-muted); }
.composer-group.center :deep(.model-pref) { min-width: 0; }
.composer-file { display: none; }
.composer-divider { width: 1px; height: 14px; background: var(--portal-line-strong); }
.composer-submit {
  flex: none;
  width: 32px;
  height: 32px;
  display: grid;
  place-items: center;
  border: 0;
  border-radius: 50%;
  /* 主操作用中性的「反转色」（暗色下白底黑箭头、亮色下黑底白箭头），与营销弹窗里的主按钮同一套（2026-10-25）。 */
  color: var(--portal-mark-text);
  background: var(--portal-mark-bg);
  cursor: pointer;
}
.composer-submit.stop { background: var(--portal-text-soft); }
.composer-submit:disabled { color: var(--portal-disabled-text); background: var(--portal-disabled-bg); cursor: not-allowed; }
/* 「画布」开关（对标截图 #26：小号滑块 + 文字） */
.composer-canvas-toggle {
  flex: none;
  height: 28px;
  display: inline-flex;
  align-items: center;
  gap: 6px;
  border: 0;
  border-radius: 999px;
  padding: 0 6px;
  color: var(--portal-text-soft);
  background: transparent;
  font: inherit;
  font-size: 12px;
  white-space: nowrap;
  cursor: pointer;
}
.composer-canvas-toggle:hover { color: var(--portal-text); }
.composer-switch {
  position: relative;
  width: 26px;
  height: 15px;
  border-radius: 999px;
  background: var(--portal-surface-3);
  box-shadow: inset 0 0 0 1px var(--portal-line-strong);
  transition: background .15s ease;
}
.composer-switch i {
  position: absolute;
  top: 2px;
  left: 2px;
  width: 11px;
  height: 11px;
  border-radius: 50%;
  background: var(--portal-text-soft);
  transition: transform .15s ease, background .15s ease;
}
.composer-canvas-toggle.on { color: var(--portal-text); }
.composer-canvas-toggle.on .composer-switch { background: var(--portal-mark-bg); box-shadow: none; }
.composer-canvas-toggle.on .composer-switch i { transform: translateX(11px); background: var(--portal-mark-text); }
.composer-recharge { color: var(--portal-gold); font-size: 12px; text-decoration: none; white-space: nowrap; }
.composer-recharge:hover { text-decoration: underline; }
.composer-error, .composer-note { margin: 6px 2px 0; font-size: 12px; }
/*
 * 营销（MKT-T-14 / MKT-T-38）：方形卡片区在正文左侧，输入框窄于 640px 时移到正文上方（卡片仍横排）。
 * 通用场景下这两层包裹是 display: contents —— 创造页 / 画布面板的布局一像素不变。
 */
.composer-body:not(.with-slots), .composer-body:not(.with-slots) > .composer-main { display: contents; }
/* 方形卡片区宽度由卡片自己决定（MKT-UX-13，两张 96px / 三张 84px），正文占其余。 */
.composer-body.with-slots { display: grid; grid-template-columns: max-content minmax(0, 1fr); gap: 14px; align-items: start; }
.composer-body.with-slots > .composer-main { min-width: 0; }
@container composer (max-width: 639px) {
  .composer-body.with-slots { grid-template-columns: minmax(0, 1fr); gap: 10px; }
}
.agent-mode-composer.marketing .composer-group.center { flex-wrap: wrap; }
.composer-error { color: var(--portal-danger); }
/*
 * 营销发送失败的标红（MKT-T-24）：40001 `details.slot` → 那个槽位卡；42215 → 那一维的胶囊与选择按钮。
 * 只加描边不改布局；子组件的类名（`.slot-card.slot-*`、`.composer-skill.dim-*`）是它们自己的。
 */
.composer-slots.invalid-product :deep(.slot-card.slot-product),
.composer-slots.invalid-character :deep(.slot-card.slot-character),
.composer-slots.invalid-source_video :deep(.slot-card.slot-source_video) {
  outline: 1px solid var(--portal-danger);
  outline-offset: 2px;
  border-radius: 12px;
}
.composer-main.invalid-creative :deep(.composer-skill.dim-creative),
.composer-main.invalid-hook :deep(.composer-skill.dim-hook),
.composer-main.invalid-style :deep(.composer-skill.dim-style) {
  color: var(--portal-danger);
  box-shadow: inset 0 0 0 1px var(--portal-danger);
}
.composer-note { color: var(--portal-muted); }
</style>
