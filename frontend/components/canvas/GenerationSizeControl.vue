<script setup lang="ts">
/**
 * 「尺寸」弹窗 —— 老系统图片/视频节点底部栏的那个（画质 · 比例 · 时长 · 数量收进一个下拉）。
 *
 * ⚠️ 标记与样式类名照老系统原样保留，外观不变；区别在数据：
 *    老后端是一个 `size` 对象（`size.ratio / size.quality / size.clarity`），
 *    新后端是**平铺的参数**（`aspect_ratio` / `quality` / `count` / `duration` / `resolution`，
 *    都在「规格」组里）。所以这里按后端下发的「规格」组**逐项**渲染 —— 配置驱动，
 *    后台加一项不必改前端；认不出的参数按普通分段按钮渲染，不丢、也不崩。
 *
 * ⚠️ 此前这几项是**平铺在底部栏上**的（画幅 5 个按钮 + 数量 3 个按钮挤在一行），
 *    与老系统「点开才出现」的弹窗不一样，也把常用的模型、发送挤出了可视区。
 *
 * 可选项随**所选模型**变（型号覆盖，`GET /models/{code}/params`）：Seedance 2.5 是 4~30 秒、
 * 480P~1080P、六种画幅，2.0 Fast 只有 720P。连续的整数档位（时长）画成滑块；
 * 被其他项约束住的选项置灰（1080P 下只有三种画幅），改动让另一项失效时一并校正。
 */
import { ChevronDown, RectangleHorizontal } from "@lucide/vue";
import {
  controlOptions,
  currentValue,
  optionDisabled,
  optionLabel,
  optionValue,
  optionsSliderRange,
  restrictedOptionValues,
  restrictionFixes,
  type ControlOption
} from "~/utils/controlOptions";

export type FormatParam = {
  code: string;
  name: string;
  config: Record<string, unknown>;
  /** 当前值；没有时取 defaultValue。 */
  value: unknown;
  defaultValue?: unknown;
};

const props = defineProps<{ nodeType: string; params: FormatParam[] }>();
const emit = defineEmits<{ update: [code: string, value: string] }>();

const root = ref<HTMLElement>();
const open = ref(false);

/** 每一项在弹窗里的标题 —— 与老系统一致；后端的名字（「画幅」「分辨率」）只作兜底。 */
const TITLES: Record<string, string> = {
  quality: "画质",
  aspect_ratio: "比例",
  resolution: "清晰度",
  duration: "视频时长",
  count: "生成数量",
  // 智能剪辑（SmartcutPromptPanel）
  target_duration: "目标时长",
  plan_variants: "方案数"
};

/** 按钮摘要里需要补字才看得懂的值（「自动」单独出现不知道指什么）。 */
const SUMMARY_LABELS: Record<string, Record<string, string>> = { target_duration: { auto: "自动时长" } };

function current(param: FormatParam): string {
  // ⚠️ 走 currentValue 而不是 String()：老数据里的值可能是整个选项对象，String() 出来是「[object Object]」。
  return currentValue(param.value ?? param.defaultValue);
}
function optionsOf(param: FormatParam): ControlOption[] {
  return controlOptions(param.config);
}
function labelOf(param: FormatParam): string {
  const value = current(param);
  const special = SUMMARY_LABELS[param.code]?.[value];
  if (special) return special;
  const hit = optionsOf(param).find(option => optionValue(option) === value);
  return hit ? optionLabel(hit) : value;
}

/** 比例网格的列数：五种以内保持老系统的五列，六种（Seedance）一行排完，再多才换行。 */
function ratioColumns(param: FormatParam): number {
  return Math.min(6, Math.max(5, optionsOf(param).length));
}

/** 比例的小图示：按宽高比画一个框，「自适应」不画。 */
function ratioStyle(option: ControlOption) {
  const ratio = optionValue(option);
  if (ratio === "adaptive") return {};
  const [width, height] = ratio.split(":").map(Number);
  if (!width || !height) return {};
  const scale = 18 / Math.max(width, height);
  return { width: `${Math.max(6, width * scale)}px`, height: `${Math.max(6, height * scale)}px` };
}

/**
 * 老弹窗的两种次序（与参数的 sort 无关）：
 *   弹窗里自上而下：画质 → 清晰度 → 比例 → 时长 → 数量
 *   按钮上的摘要：  比例 · 画质 · 清晰度 · 时长 · 数量（「16:9 · 标准画质 · 1 张」）
 * 认不出的参数排在后面、保持后端给的次序 —— 后台加一项不必改前端。
 */
const SECTION_ORDER = ["quality", "resolution", "aspect_ratio", "duration", "target_duration", "count", "plan_variants"];
const SUMMARY_ORDER = ["aspect_ratio", "quality", "resolution", "duration", "target_duration", "count", "plan_variants"];
function ordered(order: string[]) {
  const rank = (code: string) => {
    const index = order.indexOf(code);
    return index < 0 ? order.length : index;
  };
  return props.params
    .map((param, index) => ({ param, index }))
    .sort((a, b) => rank(a.param.code) - rank(b.param.code) || a.index - b.index)
    .map(entry => entry.param);
}
const sections = computed(() => ordered(SECTION_ORDER));

/** 触发按钮上的摘要 —— 不点开也知道现在是什么设置。 */
const summary = computed(() => ordered(SUMMARY_ORDER)
  .map(labelOf)
  .filter(Boolean)
  .join(" · ") || "尺寸");

/** 弹窗里各项的当前值（判参数之间的约束用）。 */
const currentValues = computed<Record<string, string>>(() =>
  Object.fromEntries(props.params.map(param => [param.code, current(param)])));

/**
 * 选不了的项：配置里标了 disabled，或被其他项约束住（型号覆盖的 `restrictBy`，
 * 如某些视频型号在 1080p 下只有 16:9 / 9:16 / 1:1）—— 置灰而不是藏起来，
 * 藏起来的话用户会以为这个模型根本没有 21:9。
 */
function blocked(param: FormatParam, option: ControlOption): boolean {
  if (optionDisabled(option)) return true;
  const allowed = restrictedOptionValues(param.config, currentValues.value);
  return Boolean(allowed && !allowed.has(optionValue(option)));
}
function blockedHint(param: FormatParam, option: ControlOption): string | undefined {
  return blocked(param, option) ? `当前设置下不可选：${optionLabel(option)}` : undefined;
}

function choose(param: FormatParam, value: string) {
  emit("update", param.code, value);
  // 这一项改了之后，被它约束的其他项若已失效就跟着校正（例如切到 1080p 时把 21:9 改回 16:9），
  // 否则按钮上还显示 21:9，要到提交后才被上游拒绝（那时已经扣过费）。
  const configs = Object.fromEntries(props.params.map(item => [item.code, item.config]));
  for (const [code, next] of Object.entries(restrictionFixes({ ...currentValues.value, [param.code]: value }, configs))) {
    emit("update", code, next);
  }
}
function pick(param: FormatParam, option: ControlOption) {
  if (blocked(param, option)) return;
  choose(param, optionValue(option));
}

/**
 * 连续整数的档位（时长 4~30 秒这类）画成滑块，老系统这里本来就是滑块；
 * 档位少或不连续的仍是按钮。判定只看选项本身（见 optionsSliderRange）。
 */
function sliderOf(param: FormatParam) {
  return optionsSliderRange(optionsOf(param));
}
/** 拖动中的值：松手才提交（拖一下就提交的话，每过一档都要存一次画布、报一次价）。 */
const dragging = ref<Record<string, number>>({});
function sliderValue(param: FormatParam): number {
  const range = sliderOf(param);
  const value = dragging.value[param.code] ?? Number(current(param));
  if (!range || !Number.isFinite(value)) return range?.min ?? 0;
  return Math.min(range.max, Math.max(range.min, value));
}
function sliderLabel(param: FormatParam): string {
  const value = String(sliderValue(param));
  const hit = optionsOf(param).find(option => optionValue(option) === value);
  return hit ? optionLabel(hit) : value;
}
function onSlide(param: FormatParam, event: Event) {
  dragging.value = { ...dragging.value, [param.code]: Number((event.target as HTMLInputElement).value) };
}
function onSlideEnd(param: FormatParam, event: Event) {
  const value = (event.target as HTMLInputElement).value;
  dragging.value = Object.fromEntries(Object.entries(dragging.value).filter(([code]) => code !== param.code));
  if (value !== current(param)) choose(param, value);
}

function onOutside(event: PointerEvent) {
  if (root.value && event.target instanceof Node && !root.value.contains(event.target)) open.value = false;
}
onMounted(() => document.addEventListener("pointerdown", onOutside));
onBeforeUnmount(() => document.removeEventListener("pointerdown", onOutside));
</script>

<template>
  <div v-if="params.length" ref="root" class="canvas-control control-dialog generation-size-control">
    <button class="generation-size-trigger" type="button" :title="summary" :aria-expanded="open" @click.stop="open = !open">
      <RectangleHorizontal :size="15" /><span>{{ summary }}</span><ChevronDown :size="13" />
    </button>
    <div v-if="open" class="generation-size-popover nodrag nowheel">
      <section v-for="param in sections" :key="param.code" class="size-choice-section">
        <strong>{{ TITLES[param.code] ?? param.name }}</strong>
        <!-- 比例用带图示的网格（老系统同款）；连续的整数档位（时长）是滑块；其余是分段按钮 -->
        <div v-if="param.code === 'aspect_ratio'" class="size-ratio-grid" :style="{ '--ratio-columns': ratioColumns(param) }">
          <button
            v-for="option in optionsOf(param)"
            :key="optionValue(option)"
            type="button"
            :class="{ active: current(param) === optionValue(option) }"
            :disabled="blocked(param, option)"
            :title="blockedHint(param, option)"
            @click="pick(param, option)"
          ><i :style="ratioStyle(option)" /><span>{{ optionLabel(option) }}</span></button>
        </div>
        <label v-else-if="sliderOf(param)" class="size-duration">
          <input
            type="range"
            :min="sliderOf(param)?.min"
            :max="sliderOf(param)?.max"
            :step="sliderOf(param)?.step"
            :value="sliderValue(param)"
            :aria-label="TITLES[param.code] ?? param.name"
            @input="onSlide(param, $event)"
            @change="onSlideEnd(param, $event)"
          >
          <span>{{ sliderLabel(param) }}</span>
        </label>
        <div v-else class="size-choice-row">
          <button
            v-for="option in optionsOf(param)"
            :key="optionValue(option)"
            type="button"
            :class="{ active: current(param) === optionValue(option) }"
            :disabled="blocked(param, option)"
            :title="blockedHint(param, option)"
            @click="pick(param, option)"
          >{{ optionLabel(option) }}</button>
        </div>
      </section>
    </div>
  </div>
</template>
