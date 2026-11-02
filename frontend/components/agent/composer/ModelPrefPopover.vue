<script setup lang="ts">
/**
 * 模型偏好浮层（设计 02 §4.5、设计 03 §2.3）：视频偏好 / 图片偏好 / 推理模型三页签。
 *
 * 样式照对标（「自定义模型｜创作偏好」）：触发器是「模型名 ｜ 创作偏好 ⌄」胶囊；浮层顶部分段页签，
 * 左栏「模型选择」（名称等宽字体 + 角标 + 灰色说明行，选中项左侧圆点高亮），右栏参数。
 * 浮层锚定在**整个输入框**上（不是触发器）：宽度跟输入框走，窄面板里自动变单栏，不会伸出可视区域。
 *
 * - ⚠️ **不可用的模型照样列出、置灰并写原因**（原因来自服务端能力校验的原话）—— 隐藏的话
 *   用户以为没有这个模型，而其实只差在设置里配一下；
 * - 推理模型页签只列 `agentCapable` 模型；它没有「智能匹配」（服务端没有默认推理模型）；
 * - 画幅 / 分辨率 / 时长的选项取自 `/canvas/config` 里视频节点的 `param_def`（与画布同源，不写死），
 *   另加「智能」= 不指定，由 Agent 按需求决定；
 * - 角标只显示目录里真有的（tabtv 没有会员门槛，目前没有角标），可用模型没有说明行（目录没有介绍字段），不编文案；
 * - 对标里的「生成模式 正常 / 自动」我们没有对应能力，不做假控件；
 * - 改动只影响**本会话**后续轮次（父组件走 `agent.patchSession`）；「设为默认」才写用户偏好（`agentPrefs`）。
 * - tabtv 不计价（TT-T-14）：不显示单价。
 *
 * **直接生成（`direct`，创造页）复用同一个胶囊与浮层**：
 * - 只出现当前模式那一个页签（视频 / 图片 / 音频），不出现与当前模式无关的页签；
 * - 左栏是该模式的模型（`/models`，不可用原因与能力校验同源），没有「智能匹配」—— 直接生成必须落到一个模型；
 * - 右栏是该模式的参数，**按 `param_def` 逐项渲染**（与画布节点同一份 `nodeParamLayout`，不写死选项）；
 *   音色走父组件的 `VoicePicker`；
 * - 操作码不在这里选（按参考图张数推断，见 `quickVideoOperation`），底部说清当前按哪种操作生成；
 * - 改动直接写回父组件的参数（`direct-update`），没有「设为默认」。
 */
import { Box, ChevronDown, ChevronRight, Info, Mic2, Sparkles } from "@lucide/vue";
import type { components as ApiComponents } from "@tabtv/api-client";
import DynamicControl from "~/components/canvas/DynamicControl.vue";
import type { OptionKind } from "~/stores/agentPrefs";
import type { JsonObject } from "~/types/canvas";
import { asObject } from "~/utils/canvas";
import type { ModelOption } from "~/utils/composerPrice";
import { directTriggerParts, modelBadges, modelOptionNote, prefTriggerParts, ratioIconBox } from "~/utils/composerUi";
import { controlOptions, currentValue, optionLabel, optionValue, type ControlOption } from "~/utils/controlOptions";
import { isVideoModeCode, nodeParamLayout, paramKey } from "~/utils/paramLayout";
import type { QuickMode } from "~/utils/quickGenerate";
import {
  prefModel,
  prefSection,
  resolveReasoningModel,
  withPrefFields,
  type AgentPrefsValue,
  type PrefKind
} from "~/utils/agentPrefs";

type ParamConfig = ApiComponents["schemas"]["NodeParamConfig"];

/** 直接生成（创造页）的数据：当前模式、该模式的参数与模型目录、底部那行操作说明。 */
export type DirectPref = {
  mode: QuickMode;
  params: JsonObject;
  models: ReadonlyArray<{ code?: string; name?: string; minMemberLevelId?: string | null }>;
  /** 底部说明（当前按哪种操作生成）。 */
  note?: string;
};

const props = withDefaults(defineProps<{
  /** 当前生效的偏好（用户默认 < 会话级）。直接生成时不用。 */
  prefs?: AgentPrefsValue;
  /** 直接生成模式（创造页）；不给就是 Agent 偏好。 */
  direct?: DirectPref;
  disabled?: boolean;
  /** 浮层向上弹（输入框贴底时）。 */
  up?: boolean;
  /**
   * 触发器只显示一个模型图标（画布 Agent 面板，2026-11-01 按参考图）：悬停出「模型」（`data-tip`，样式在输入框工具栏里），
   * 当前模型与创作偏好写在 aria-label 里；点开的浮层与胶囊时完全一样。
   */
  iconOnly?: boolean;
}>(), { prefs: () => ({}), direct: undefined });
const emit = defineEmits<{
  change: [prefs: AgentPrefsValue];
  "save-default": [prefs: AgentPrefsValue];
  /** 直接生成：改一个参数（含 `model`）。 */
  "direct-update": [code: string, value: unknown];
  /** 直接生成：打开音色选择。 */
  "open-voice": [];
}>();

const prefsStore = useAgentPrefsStore();
const configStore = useCanvasConfigStore();
const open = ref(false);
const tab = ref<OptionKind>(props.direct?.mode ?? "video");
const root = ref<HTMLElement>();
const popover = ref<HTMLElement>();
const ratioRow = ref<HTMLElement>();
const customDuration = ref("");
/** 用户点了「自定义时长」但还没填数：只在本地展开输入，不改偏好。 */
const customOpen = ref(false);
const ratioMore = ref(false);

const AGENT_TABS: Array<{ kind: OptionKind; label: string }> = [
  { kind: "video", label: "视频偏好" },
  { kind: "image", label: "图片偏好" },
  { kind: "reasoning", label: "推理模型" }
];
const DIRECT_TAB_LABELS: Record<QuickMode, string> = { video: "视频偏好", image: "图片偏好", audio: "音频偏好" };
/** ⚠️ 直接生成只给当前模式那一个页签：切到别的页签选模型不会生效，出现了只会误导。 */
const TABS = computed(() => props.direct
  ? [{ kind: props.direct.mode as OptionKind, label: DIRECT_TAB_LABELS[props.direct.mode] }]
  : AGENT_TABS);
watch(() => props.direct?.mode, mode => {
  if (mode) tab.value = mode;
});

/**
 * 直接生成的模型：目录取父组件给的（与发送同一份 `/models`），单价与不可用原因取 store 的报价结果；
 * 报价还没回来的按可用列出、说明行写「报价中」。
 */
const directOptions = computed<ModelOption[]>(() => {
  const direct = props.direct;
  if (!direct) return [];
  return direct.models
    .filter(model => model.code)
    .map(model => prefsStore.option(direct.mode, model.code)
      ?? { code: model.code!, name: model.name || model.code!, available: true, badges: modelBadges(model) });
});
const options = computed<ModelOption[]>(() => props.direct ? directOptions.value : prefsStore.options(tab.value));
const loadError = computed(() => prefsStore.optionErrors[tab.value]);
const directModel = computed(() => {
  const value = props.direct?.params.model;
  return typeof value === "string" && value ? value : undefined;
});
const trigger = computed(() => props.direct
  ? directTriggerParts(directModel.value, directOptions.value)
  : prefTriggerParts(props.prefs, prefsStore.options("video")));
const selected = computed(() => {
  if (props.direct) return directModel.value;
  return tab.value === "reasoning"
    ? resolveReasoningModel(props.prefs, options.value)
    : prefModel(props.prefs, tab.value as PrefKind);
});
const video = computed(() => prefSection(props.prefs, "video"));
/** 页签上的小圆点：这一类偏好已自定义（选了模型）。直接生成只有一个页签，不需要。 */
const customized = computed<Partial<Record<OptionKind, boolean>>>(() => props.direct
  ? {}
  : {
    video: Boolean(prefModel(props.prefs, "video")),
    image: Boolean(prefModel(props.prefs, "image")),
    reasoning: false
  });

// ---------------------------------------------------------------- 直接生成的参数（param_def 驱动）

/** 直接生成里没有落点的参数（同原参数行）：正文、参考（`＋`）、依赖画布其他节点的、模型（左栏）、操作（自动判定）。 */
const DIRECT_EXCLUDED = ["reference", "preset", "mark", "effectLib", "roleLib", "camera", "model", "voice"];
/** 右栏次序：比例 → 画质 → 分辨率 → 时长 → 数量；认不出的排后面、保持后端次序。 */
const DIRECT_ORDER = ["aspect_ratio", "quality", "resolution", "duration", "count"];
/** 标题只管怎么叫；后端的名字作兜底（后台加一项不必改前端）。 */
const DIRECT_TITLES: Record<string, string> = {
  aspect_ratio: "画面比例",
  quality: "画质",
  resolution: "分辨率",
  duration: "时长",
  count: "生成数量"
};

const directNodeParams = computed<ParamConfig[]>(() => props.direct ? configStore.nodeType(props.direct.mode)?.params ?? [] : []);
const directParams = computed<ParamConfig[]>(() => {
  if (!props.direct) return [];
  const layout = nodeParamLayout(props.direct.mode, directNodeParams.value, { exclude: DIRECT_EXCLUDED });
  const rank = (code: string | undefined) => {
    const index = DIRECT_ORDER.indexOf(code ?? "");
    return index < 0 ? DIRECT_ORDER.length : index;
  };
  return [...layout.size, ...layout.bottom]
    .filter(item => !isVideoModeCode(item.code))
    .map((item, index) => ({ item, index }))
    .sort((a, b) => rank(a.item.code) - rank(b.item.code) || a.index - b.index)
    .map(entry => entry.item);
});
const directHasVoice = computed(() => directNodeParams.value.some(item => item.code === "voice"));
const voiceLabel = computed(() => String(props.direct?.params.voiceName ?? "") || "选择音色");

function paramConfig(param: ParamConfig): Record<string, unknown> {
  return asObject(param.config);
}
function paramChoicesOf(param: ParamConfig): ControlOption[] {
  const config = paramConfig(param);
  return config.domainFrom ? [] : controlOptions(config);
}
function paramRaw(param: ParamConfig): unknown {
  const current = props.direct?.params[paramKey(param.code)];
  if (current !== undefined && current !== null && current !== "") return current;
  return (param as { defaultValue?: unknown }).defaultValue ?? paramConfig(param).default;
}
function paramCurrent(param: ParamConfig): string {
  return currentValue(paramRaw(param));
}
function setDirect(param: ParamConfig, value: unknown) {
  emit("direct-update", paramKey(param.code), value);
}
function openVoice() {
  open.value = false;
  emit("open-voice");
}

type Choice = { value: string; label: string };

function paramChoices(code: string): Choice[] {
  const param = configStore.nodeType("video")?.params?.find(item => item.code === code);
  const list = asObject(param?.config).options;
  if (!Array.isArray(list)) return [];
  return list
    .map(item => asObject(item))
    .filter(item => item.value !== undefined && item.value !== null)
    .map(item => ({ value: String(item.value), label: String(item.label ?? item.value) }));
}

const ratioChoices = computed(() => paramChoices("aspect_ratio"));
const resolutionChoices = computed(() => paramChoices("resolution"));
const durationChoices = computed(() => paramChoices("duration"));
const durationSet = computed(() => {
  const value = video.value.duration;
  return value !== undefined && value !== null && value !== "";
});
const durationIsCustom = computed(() => durationSet.value && !durationChoices.value.some(item => item.value === String(video.value.duration)));
const showCustomDuration = computed(() => customOpen.value || durationSet.value);

function updateRatioMore() {
  const el = ratioRow.value;
  ratioMore.value = Boolean(el && el.scrollLeft + el.clientWidth < el.scrollWidth - 2);
}

function scrollRatios() {
  ratioRow.value?.scrollBy({ left: 160, behavior: "smooth" });
}

function toggle() {
  if (props.disabled) return;
  open.value = !open.value;
  if (open.value) {
    void configStore.load().catch(() => undefined);
    void prefsStore.loadOptions(tab.value);
    if (props.direct) tab.value = props.direct.mode;
    customDuration.value = durationIsCustom.value ? String(video.value.duration) : "";
    customOpen.value = false;
    nextTick(updateRatioMore);
  }
}

watch(tab, kind => {
  if (open.value) void prefsStore.loadOptions(kind);
  nextTick(updateRatioMore);
});
watch([ratioChoices, directParams], () => nextTick(updateRatioMore));

function pickModel(code: string | undefined) {
  if (code) {
    const option = options.value.find(item => item.code === code);
    if (option && !option.available) return;
  }
  if (props.direct) {
    if (code) emit("direct-update", "model", code);
    return;
  }
  emit("change", withPrefFields(props.prefs, tab.value as PrefKind, { model: code }));
}

function bindRatioRow(el: unknown) {
  ratioRow.value = el instanceof HTMLElement ? el : undefined;
}

function setVideo(field: "ratio" | "resolution" | "duration", value: string | number | undefined) {
  emit("change", withPrefFields(props.prefs, "video", { [field]: value }));
}

function smartDuration() {
  customOpen.value = false;
  customDuration.value = "";
  setVideo("duration", undefined);
}

function applyCustomDuration() {
  const seconds = Number.parseInt(customDuration.value, 10);
  setVideo("duration", Number.isFinite(seconds) && seconds > 0 ? seconds : undefined);
}

function isActive(field: "ratio" | "resolution" | "duration", value: string | undefined) {
  const current = video.value[field];
  if (value === undefined) return current === undefined || current === null || current === "";
  return String(current) === value;
}

function onOutside(event: PointerEvent) {
  if (!open.value || !(event.target instanceof Node)) return;
  // 浮层锚在输入框上、不在触发器里：两处都算「里面」。
  if (root.value?.contains(event.target) || popover.value?.contains(event.target)) return;
  open.value = false;
}
function onKey(event: KeyboardEvent) {
  if (event.key === "Escape") open.value = false;
}
/** 从外部打开（全屏对话页顶栏的「偏好」按钮，AGT-44）；已开着就不动。 */
function show() {
  if (!open.value) toggle();
}
defineExpose({ show });

onMounted(() => document.addEventListener("pointerdown", onOutside, true));
onBeforeUnmount(() => document.removeEventListener("pointerdown", onOutside, true));
</script>

<template>
  <div ref="root" class="model-pref" :class="{ up }" @keydown="onKey">
    <button
      v-if="iconOnly"
      type="button"
      class="model-pref-trigger icon-only"
      :class="{ open, custom: trigger.custom }"
      :disabled="disabled"
      :aria-expanded="open"
      :aria-label="`模型：${trigger.model}｜${trigger.tail}`"
      data-tip="模型"
      @click="toggle"
    >
      <Box :size="16" class="model-pref-icon" />
    </button>
    <button
      v-else
      type="button"
      class="model-pref-trigger"
      :class="{ open, custom: trigger.custom }"
      :disabled="disabled"
      :aria-expanded="open"
      :title="`${trigger.model}｜${trigger.tail}`"
      @click="toggle"
    >
      <Sparkles :size="14" class="model-pref-icon" />
      <span class="model-pref-model">{{ trigger.model }}</span>
      <span class="model-pref-sep" aria-hidden="true" />
      <span class="model-pref-tail">{{ trigger.tail }}</span>
      <ChevronDown :size="13" class="model-pref-chevron" />
    </button>
    <div v-if="open" ref="popover" class="model-pref-popover" :class="{ direct: Boolean(direct) }" role="dialog" aria-label="模型与创作偏好">
      <div class="model-pref-inner">
        <nav class="model-pref-tabs" role="tablist">
          <button
            v-for="item in TABS"
            :key="item.kind"
            type="button"
            role="tab"
            :aria-selected="tab === item.kind"
            :class="{ active: tab === item.kind }"
            @click="tab = item.kind"
          >
            {{ item.label }}<i v-if="customized[item.kind]" class="model-pref-tab-dot" aria-label="已自定义" />
          </button>
        </nav>
        <div class="model-pref-body" :class="{ single: !direct && tab === 'image' }">
          <section class="model-pref-col">
            <p class="model-pref-label">模型选择</p>
            <ul class="model-pref-list" aria-label="模型选择">
              <li v-if="!direct && tab !== 'reasoning'">
                <button type="button" class="model-pref-item" :class="{ active: !selected }" :aria-pressed="!selected" @click="pickModel(undefined)">
                  <span class="model-pref-dot" />
                  <span class="model-pref-item-main">
                    <span class="model-pref-item-head"><span class="model-pref-name sans">智能匹配模型</span></span>
                    <span class="model-pref-note">根据需求自动选择，确认规划时会明确告诉你用了哪个</span>
                  </span>
                </button>
              </li>
              <li v-if="!options.length" class="model-pref-empty">
                {{ direct ? '当前没有可用的模型' : (loadError || (tab === 'reasoning' ? '还没有支持 Agent 的推理模型' : '加载中…')) }}
              </li>
              <li v-for="option in options" :key="option.code">
                <button
                  type="button"
                  class="model-pref-item"
                  :class="{ active: selected === option.code, unavailable: !option.available }"
                  :disabled="!option.available"
                  :aria-pressed="selected === option.code"
                  :title="option.available ? option.name : `不可用：${option.reason}`"
                  @click="pickModel(option.code)"
                >
                  <span class="model-pref-dot" />
                  <span class="model-pref-item-main">
                    <span class="model-pref-item-head">
                      <span class="model-pref-name">{{ option.name }}</span>
                      <em v-for="badge in option.badges ?? []" :key="badge" class="model-pref-badge">{{ badge }}</em>
                    </span>
                    <span v-if="modelOptionNote(option, tab === 'reasoning')" class="model-pref-note">{{ modelOptionNote(option, tab === 'reasoning') }}</span>
                  </span>
                </button>
              </li>
            </ul>
          </section>
          <section v-if="direct" class="model-pref-col model-pref-params">
            <template v-if="direct.mode === 'audio' && directHasVoice">
              <p class="model-pref-label">音色</p>
              <button type="button" class="model-pref-voice" :title="voiceLabel" @click="openVoice">
                <Mic2 :size="14" /><span>{{ voiceLabel }}</span><ChevronRight :size="13" />
              </button>
            </template>
            <template v-for="param in directParams" :key="param.code">
              <template v-if="paramChoicesOf(param).length">
                <p class="model-pref-label">{{ DIRECT_TITLES[param.code ?? ''] ?? param.name ?? param.code }}</p>
                <div v-if="param.code === 'aspect_ratio'" class="model-pref-ratio-wrap">
                  <div :ref="bindRatioRow" class="model-pref-ratios" @scroll.passive="updateRatioMore">
                    <button
                      v-for="option in paramChoicesOf(param)"
                      :key="optionValue(option)"
                      type="button"
                      :class="{ active: paramCurrent(param) === optionValue(option) }"
                      @click="setDirect(param, optionValue(option))"
                    >
                      <i class="model-pref-ratio-box" :style="{ width: `${ratioIconBox(optionValue(option)).width}px`, height: `${ratioIconBox(optionValue(option)).height}px` }" />
                      <span>{{ optionLabel(option) }}</span>
                    </button>
                  </div>
                  <button v-if="ratioMore" type="button" class="model-pref-ratio-more" aria-label="更多比例" @click="scrollRatios"><ChevronRight :size="14" /></button>
                </div>
                <div v-else class="model-pref-seg wrap">
                  <button
                    v-for="option in paramChoicesOf(param)"
                    :key="optionValue(option)"
                    type="button"
                    :class="{ active: paramCurrent(param) === optionValue(option) }"
                    @click="setDirect(param, optionValue(option))"
                  >{{ optionLabel(option) }}</button>
                </div>
              </template>
              <template v-else-if="param.control === 'slider'">
                <p class="model-pref-label">{{ param.name ?? param.code }} <b class="model-pref-value">{{ paramCurrent(param) }}</b></p>
                <input
                  class="model-pref-range"
                  type="range"
                  :min="Number(paramConfig(param).min ?? 0)"
                  :max="Number(paramConfig(param).max ?? 100)"
                  :step="Number(paramConfig(param).step ?? 1)"
                  :value="Number(paramRaw(param) ?? 0)"
                  :aria-label="param.name ?? param.code"
                  @input="setDirect(param, Number(($event.target as HTMLInputElement).value))"
                >
              </template>
              <div v-else class="model-pref-generic">
                <DynamicControl
                  :control="param.control ?? ''"
                  :name="param.name ?? param.code ?? ''"
                  :config="paramConfig(param)"
                  :model-value="paramRaw(param)"
                  @update:model-value="setDirect(param, $event)"
                />
              </div>
            </template>
            <p v-if="!directParams.length && !(direct.mode === 'audio' && directHasVoice)" class="model-pref-explain">这个模式没有可调的参数。</p>
          </section>
          <section v-else-if="tab === 'video'" class="model-pref-col model-pref-params">
            <template v-if="ratioChoices.length">
              <p class="model-pref-label">画面比例 <span class="model-pref-tip" title="「智能」= 不指定，由 Agent 按需求决定"><Info :size="12" /></span></p>
              <div class="model-pref-ratio-wrap">
                <div ref="ratioRow" class="model-pref-ratios" @scroll.passive="updateRatioMore">
                  <button type="button" :class="{ active: isActive('ratio', undefined) }" @click="setVideo('ratio', undefined)">
                    <Sparkles :size="14" />
                    <span>智能</span>
                  </button>
                  <button v-for="item in ratioChoices" :key="item.value" type="button" :class="{ active: isActive('ratio', item.value) }" @click="setVideo('ratio', item.value)">
                    <i class="model-pref-ratio-box" :style="{ width: `${ratioIconBox(item.value).width}px`, height: `${ratioIconBox(item.value).height}px` }" />
                    <span>{{ item.label }}</span>
                  </button>
                </div>
                <button v-if="ratioMore" type="button" class="model-pref-ratio-more" aria-label="更多比例" @click="scrollRatios"><ChevronRight :size="14" /></button>
              </div>
            </template>
            <template v-if="resolutionChoices.length">
              <p class="model-pref-label">分辨率</p>
              <div class="model-pref-seg">
                <button type="button" :class="{ active: isActive('resolution', undefined) }" @click="setVideo('resolution', undefined)">智能</button>
                <button v-for="item in resolutionChoices" :key="item.value" type="button" :class="{ active: isActive('resolution', item.value) }" @click="setVideo('resolution', item.value)">{{ item.label }}</button>
              </div>
            </template>
            <p class="model-pref-label">时长 <span class="model-pref-tip" title="「智能时长」= 不指定，由 Agent 按剧情决定"><Info :size="12" /></span></p>
            <div class="model-pref-seg two">
              <button type="button" :class="{ active: !showCustomDuration }" @click="smartDuration">智能时长</button>
              <button type="button" :class="{ active: showCustomDuration }" @click="customOpen = true">自定义时长</button>
            </div>
            <div v-if="showCustomDuration" class="model-pref-duration">
              <button
                v-for="item in durationChoices"
                :key="item.value"
                type="button"
                class="model-pref-chip"
                :class="{ active: isActive('duration', item.value) }"
                @click="setVideo('duration', Number(item.value))"
              >{{ item.label }}</button>
              <label class="model-pref-custom" :class="{ active: durationIsCustom }">
                <input v-model="customDuration" type="number" min="1" max="600" inputmode="numeric" placeholder="秒数" aria-label="自定义时长（秒）" @change="applyCustomDuration">
                秒
              </label>
            </div>
          </section>
          <section v-else-if="tab === 'reasoning'" class="model-pref-col model-pref-params">
            <p class="model-pref-label">说明</p>
            <p class="model-pref-explain">推理模型负责理解你的需求、规划分镜并调度生成模型，按轮计费；图片与视频的生成费用在确认规划时另给总价，确认前不会扣费。</p>
          </section>
        </div>
        <footer v-if="direct" class="model-pref-foot">
          <span>{{ direct.note || '改动只影响这一次创作' }}</span>
        </footer>
        <footer v-else class="model-pref-foot">
          <span>改动只影响本会话后续轮次</span>
          <button type="button" :disabled="prefsStore.saving" @click="emit('save-default', prefs)">设为默认</button>
        </footer>
      </div>
    </div>
  </div>
</template>

<style scoped>
/* ⚠️ 不设 position：浮层相对最近的定位祖先（输入框卡片）摆放，宽度跟输入框走 */
.model-pref { min-width: 0; max-width: 100%; }
.model-pref-trigger {
  max-width: 100%;
  height: 30px;
  display: inline-flex;
  align-items: center;
  gap: 6px;
  border: 0;
  border-radius: 999px;
  padding: 0 10px;
  color: var(--portal-text-soft);
  background: var(--portal-surface-2);
  font: inherit;
  font-size: 12px;
  cursor: pointer;
}
.model-pref-trigger:hover:not(:disabled), .model-pref-trigger.open { color: var(--portal-text); background: var(--portal-surface-3); }
.model-pref-trigger.custom .model-pref-icon { color: var(--portal-text); }
.model-pref-trigger:disabled { color: var(--portal-disabled-text); cursor: not-allowed; }
.model-pref-icon { flex: none; }
.model-pref-model { min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.model-pref-tail { flex: none; white-space: nowrap; }
.model-pref-sep { flex: none; width: 1px; height: 12px; background: var(--portal-line-strong); }
.model-pref-chevron { flex: none; color: var(--portal-muted); transition: transform .15s ease; }
.model-pref-trigger.open .model-pref-chevron { transform: rotate(180deg); }
/* 只显示图标：与输入框左下角的 ＋ / @ 同一种 30px 圆形图标按钮；选过模型（custom）时图标亮一档 */
.model-pref-trigger.icon-only { width: 30px; justify-content: center; border-radius: 50%; padding: 0; background: transparent; }
.model-pref-trigger.icon-only:hover:not(:disabled), .model-pref-trigger.icon-only.open { color: var(--portal-text); background: var(--portal-hover); }
.model-pref-trigger.icon-only:disabled { background: transparent; }

.model-pref-popover {
  position: absolute;
  top: calc(100% + 8px);
  left: 0;
  right: 0;
  max-width: 760px;
  margin: 0 auto;
  z-index: 90;
  container: modelpref / inline-size;
  border: 1px solid var(--portal-line-strong);
  border-radius: 16px;
  color: var(--portal-text);
  background: var(--portal-elevated);
  box-shadow: 0 18px 48px var(--portal-shadow-strong);
  font-size: 12px;
  text-align: left;
}
.model-pref.up .model-pref-popover { top: auto; bottom: calc(100% + 8px); }
.model-pref-inner { display: flex; flex-direction: column; max-height: min(480px, 64vh); }

.model-pref-tabs {
  flex: none;
  align-self: flex-start;
  display: inline-flex;
  gap: 2px;
  margin: 12px 12px 0;
  padding: 3px;
  border-radius: 10px;
  background: var(--portal-surface-2);
  max-width: calc(100% - 24px);
  overflow-x: auto;
}
.model-pref-tabs button {
  position: relative;
  border: 0;
  border-radius: 8px;
  padding: 5px 14px;
  color: var(--portal-text-soft);
  background: transparent;
  font: inherit;
  white-space: nowrap;
  cursor: pointer;
}
.model-pref-tabs button:hover { color: var(--portal-text); }
.model-pref-tabs button.active { color: var(--portal-text); background: var(--portal-surface-3); }
.model-pref-tab-dot { position: absolute; top: 5px; right: 6px; width: 4px; height: 4px; border-radius: 50%; background: var(--portal-text); }

/* 窄（画布面板）：单栏，参数排在模型列表下面；宽：左右两栏 */
.model-pref-body { min-height: 0; display: grid; grid-template-columns: minmax(0, 1fr); overflow-y: auto; }
.model-pref-col { min-width: 0; padding: 12px 14px; }
.model-pref-params { border-top: 1px solid var(--portal-line-soft); }
@container modelpref (min-width: 520px) {
  .model-pref-body:not(.single) { grid-template-columns: minmax(0, 1fr) minmax(0, 1fr); overflow: hidden; }
  .model-pref-body:not(.single) .model-pref-col { overflow-y: auto; }
  .model-pref-params { border-top: 0; border-left: 1px solid var(--portal-line-soft); padding-left: 18px; }
}

.model-pref-label { display: flex; align-items: center; gap: 4px; margin: 14px 0 8px; color: var(--portal-muted); }
.model-pref-col > .model-pref-label:first-child { margin-top: 0; }
.model-pref-tip { display: inline-grid; color: var(--portal-faint); cursor: help; }

.model-pref-list { margin: 0; padding: 0; list-style: none; display: grid; gap: 2px; }
.model-pref-item {
  width: 100%;
  display: flex;
  align-items: flex-start;
  gap: 10px;
  border: 0;
  border-radius: 10px;
  padding: 9px 10px;
  color: var(--portal-text-soft);
  background: transparent;
  font: inherit;
  text-align: left;
  cursor: pointer;
}
.model-pref-item:hover:not(:disabled) { color: var(--portal-text); background: var(--portal-hover); }
.model-pref-item.active { color: var(--portal-text); background: var(--portal-surface-2); }
.model-pref-item.unavailable { color: var(--portal-disabled-text); cursor: not-allowed; }
.model-pref-dot { flex: none; width: 6px; height: 6px; margin-top: 7px; border-radius: 50%; background: var(--portal-line-strong); }
.model-pref-item.active .model-pref-dot { background: var(--portal-text); box-shadow: 0 0 0 3px var(--portal-active); }
.model-pref-item-main { min-width: 0; flex: 1; display: grid; gap: 3px; }
.model-pref-item-head { min-width: 0; display: flex; flex-wrap: wrap; align-items: center; gap: 6px; }
.model-pref-name {
  min-width: 0;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  font-family: var(--font-mono, ui-monospace, SFMono-Regular, Menlo, Consolas, monospace);
  font-size: 13px;
  font-weight: 600;
}
.model-pref-name.sans { font-family: inherit; }
.model-pref-badge {
  border-radius: 5px;
  padding: 0 6px;
  color: var(--portal-gold);
  background: var(--portal-gold-soft);
  font-size: 10px;
  font-style: normal;
  line-height: 17px;
}
.model-pref-item.unavailable .model-pref-badge { color: var(--portal-disabled-text); background: var(--portal-surface-2); }
.model-pref-note { color: var(--portal-muted); font-size: 11px; line-height: 16px; }
.model-pref-item.unavailable .model-pref-note { color: var(--portal-disabled-text); }
.model-pref-empty { padding: 8px 10px; color: var(--portal-muted); }

.model-pref-ratio-wrap { position: relative; }
.model-pref-ratios {
  display: flex;
  gap: 4px;
  padding: 4px;
  border-radius: 12px;
  background: var(--portal-surface-2);
  overflow-x: auto;
  scrollbar-width: none;
}
.model-pref-ratios::-webkit-scrollbar { display: none; }
.model-pref-ratios button {
  flex: none;
  min-width: 56px;
  height: 56px;
  display: grid;
  place-items: center;
  align-content: center;
  gap: 6px;
  border: 0;
  border-radius: 9px;
  color: var(--portal-text-soft);
  background: transparent;
  font: inherit;
  font-size: 11px;
  cursor: pointer;
}
.model-pref-ratios button:hover { color: var(--portal-text); }
.model-pref-ratios button.active { color: var(--portal-text); background: var(--portal-surface-3); }
.model-pref-ratio-box { display: block; border: 1.5px solid currentColor; border-radius: 2px; }
.model-pref-ratio-more {
  position: absolute;
  top: 50%;
  right: 6px;
  width: 22px;
  height: 22px;
  display: grid;
  place-items: center;
  transform: translateY(-50%);
  border: 0;
  border-radius: 50%;
  color: var(--portal-text);
  background: var(--portal-surface-3);
  box-shadow: 0 2px 8px var(--portal-shadow);
  cursor: pointer;
}

.model-pref-seg { display: flex; gap: 4px; padding: 4px; border-radius: 12px; background: var(--portal-surface-2); }
.model-pref-seg button {
  flex: 1 1 0;
  min-width: 0;
  height: 30px;
  border: 0;
  border-radius: 9px;
  padding: 0 8px;
  color: var(--portal-text-soft);
  background: transparent;
  font: inherit;
  white-space: nowrap;
  cursor: pointer;
}
.model-pref-seg button:hover { color: var(--portal-text); }
.model-pref-seg button.active { color: var(--portal-text); background: var(--portal-surface-3); }
.model-pref-duration { display: flex; flex-wrap: wrap; align-items: center; gap: 6px; margin-top: 8px; }
.model-pref-chip, .model-pref-custom {
  height: 28px;
  display: inline-flex;
  align-items: center;
  gap: 4px;
  border: 1px solid var(--portal-line);
  border-radius: 8px;
  padding: 0 10px;
  color: var(--portal-text-soft);
  background: transparent;
  font: inherit;
  cursor: pointer;
}
.model-pref-chip.active, .model-pref-custom.active { border-color: var(--portal-text-soft); color: var(--portal-text); background: var(--portal-active); }
.model-pref-custom { cursor: text; }
.model-pref-custom input { width: 52px; border: 0; outline: 0; color: var(--portal-text); background: transparent; font: inherit; }
.model-pref-custom input::placeholder { color: var(--portal-placeholder); }
.model-pref-seg.wrap { flex-wrap: wrap; }
.model-pref-seg.wrap button { flex: 1 0 56px; }
.model-pref-voice {
  width: 100%;
  height: 34px;
  display: flex;
  align-items: center;
  gap: 8px;
  border: 0;
  border-radius: 10px;
  padding: 0 10px;
  color: var(--portal-text);
  background: var(--portal-surface-2);
  font: inherit;
  cursor: pointer;
}
.model-pref-voice:hover { background: var(--portal-surface-3); }
.model-pref-voice span { min-width: 0; flex: 1; overflow: hidden; text-align: left; text-overflow: ellipsis; white-space: nowrap; }
.model-pref-value { color: var(--portal-text); font-weight: 600; }
.model-pref-range { width: 100%; accent-color: var(--portal-text); }
.model-pref-generic { min-width: 0; }
.model-pref-explain { margin: 0; color: var(--portal-text-soft); line-height: 19px; }

.model-pref-foot {
  flex: none;
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 8px;
  padding: 10px 14px;
  border-top: 1px solid var(--portal-line-soft);
  color: var(--portal-muted);
}
.model-pref-foot button {
  height: 26px;
  border: 1px solid var(--portal-line-strong);
  border-radius: 999px;
  padding: 0 12px;
  color: var(--portal-text);
  background: transparent;
  font: inherit;
  white-space: nowrap;
  cursor: pointer;
}
.model-pref-foot button:disabled { opacity: .5; cursor: not-allowed; }
</style>
