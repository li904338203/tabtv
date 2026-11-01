<script setup lang="ts">
/**
 * 打光编辑器（docs/agent/imagetools IMGT-16，IMG-F-40~48，dev/02 §3.4）。
 *
 * props 约定（BaseNode 按它挂载，不要改）：
 * - `nodeId`：面板挂在哪个节点上（spawn 模式下就是源节点；node 模式下是工具子节点）；
 * - `sourceNodeId`：源图节点（spawn 模式 = nodeId；node 模式 = 连线 order=1 的上游）；
 * - `mode`：`spawn` 发送 = 建子节点 + 立即生成（IMG-F-45）；`node` 改动写进本节点 params、发送 = 对本节点重新生成（IMG-F-48）。
 * emit `close`：关闭面板。⚠️ 只能经 BaseNode 的 defineAsyncComponent 引入（IMG-N-06）。
 *
 * ⚠️ 不在前端拼提示词：只组装 `{azimuth, elevation, preset?, brightness, color, rim, smart}`，翻译在服务端（IMG-F-46）。
 * ⚠️ 智能模式打开时其余控件置灰、请求里**只发 `{smart:true}`**（IMG-F-43；`imageToolWireOptions` 负责剥）。
 *    界面上仍保留灰掉的值 —— 关掉智能模式时用户刚调好的方向不会丢。
 * ⚠️ 积分只显示服务端报价（ToolPanelShell），不在这里按单价估。
 */
import { ElMessage } from "element-plus";
import { CircleHelp } from "@lucide/vue";
import ToolPanelShell from "./ToolPanelShell.vue";
import LightSphere from "./LightSphere.vue";
import { generationInputs } from "~/utils/canvas";
import {
  imageToolDefaultOptions,
  imageToolLabel,
  imageToolRequest,
  imageToolsConfigOf,
  type ImageToolRelightOptions
} from "~/utils/imageTools";
import {
  imageToolRelightApplyPreset,
  imageToolRelightClampBrightness,
  imageToolRelightColorIndex,
  imageToolRelightColorLabel,
  imageToolRelightMatchPreset,
  imageToolRelightNormalizeColor,
  imageToolRelightOptionsOf,
  imageToolRelightWithDirection
} from "~/utils/imageToolRelight";
import { useImageToolsStore } from "~/stores/imageTools";

const props = defineProps<{ nodeId: string; sourceNodeId: string; mode: "spawn" | "node" }>();
const emit = defineEmits<{ close: [] }>();

const canvas = useCanvasStore();
const configStore = useCanvasConfigStore();
const generation = useGenerationStore();
const imageTools = useImageToolsStore();

const config = computed(() => imageToolsConfigOf(configStore.config?.imageTools));
const presets = computed(() => config.value.lightPresets);
const colors = computed(() => config.value.namedColors);
const defaults = computed(() => imageToolDefaultOptions("relight", { config: config.value }));
const title = computed(() => imageToolLabel(config.value, "relight"));

const node = computed(() => canvas.nodes.find(item => item.id === props.nodeId));
const sourceNode = computed(() => canvas.nodes.find(item => item.id === props.sourceNodeId));
/** 源图媒体：源节点的生成产物（spawn 模式源节点就是当前节点）。 */
const sourceMediaId = computed(() => sourceNode.value?.data.generation?.mediaId);
/** 球中间的源图小卡片：源节点上已经取好的 object URL（不再单独取一次图）。 */
const thumbnail = computed(() => sourceNode.value?.data.generation?.previewUrl ?? "");

/** node 模式回显节点上存着的选项；spawn 模式从默认值开始。 */
function initialOptions(): ImageToolRelightOptions {
  if (props.mode === "node") return imageToolRelightOptionsOf(node.value?.data.params.toolOptions, defaults.value, presets.value);
  return { ...defaults.value };
}
const options = ref<ImageToolRelightOptions>(initialOptions());
const smart = computed(() => options.value.smart === true);
const activePreset = computed(() => imageToolRelightMatchPreset(presets.value, options.value.azimuth ?? 0, options.value.elevation ?? 0));
const brightness = computed(() => options.value.brightness ?? 50);
const colorIndex = computed(() => imageToolRelightColorIndex(colors.value, options.value.color));
const colorLabel = computed(() => imageToolRelightColorLabel(colors.value, options.value.color));

function patch(next: Partial<ImageToolRelightOptions>) {
  options.value = { ...options.value, ...next };
}

function setDirection(direction: { azimuth: number; elevation: number }) {
  if (smart.value) return;
  options.value = imageToolRelightWithDirection(options.value, presets.value, direction);
}

function applyPreset(code: string) {
  if (smart.value) return;
  options.value = imageToolRelightApplyPreset(options.value, presets.value, code);
}

function setBrightness(value: unknown) {
  patch({ brightness: imageToolRelightClampBrightness(value, brightness.value) });
}

/** 百分比输入框：失焦 / 回车时才规范化（打字过程中清空不该立刻跳回 50）。 */
const brightnessText = ref(String(brightness.value));
watch(brightness, (value) => {
  brightnessText.value = String(value);
});
function commitBrightnessText() {
  setBrightness(brightnessText.value);
  brightnessText.value = String(brightness.value);
}

function setColor(value: string | null) {
  patch({ color: imageToolRelightNormalizeColor(value) });
}

function reset() {
  options.value = { ...defaults.value };
  colorOpen.value = false;
}

// ---------------------------------------------------------------------------
// 颜色弹层
// ---------------------------------------------------------------------------

const colorOpen = ref(false);
/** 自定义取色器的初值：当前颜色，没有颜色时给暖白（取色器不收空值）。 */
const pickerValue = computed(() => imageToolRelightNormalizeColor(options.value.color) ?? "#fff1dc");

function toggleColor() {
  if (!smart.value) colorOpen.value = !colorOpen.value;
}

watch(smart, (value) => {
  if (value) colorOpen.value = false;
});

// ---------------------------------------------------------------------------
// node 模式：改动写回节点 params（IMG-F-48）
// ---------------------------------------------------------------------------

const toolInfo = computed(() => imageTools.toolOf("relight"));
/** 模型：节点上已选且仍可用的优先，否则取 `/image-tools` 里这个工具的第一个模型。 */
const modelCode = computed(() => {
  const models = toolInfo.value?.models ?? [];
  const own = props.mode === "node" ? String(node.value?.data.params.model ?? "") : "";
  if (own && models.some(item => item.code === own)) return own;
  return models[0]?.code ?? "";
});

watch(() => JSON.stringify(options.value), () => {
  if (props.mode !== "node" || !node.value) return;
  const params = node.value.data.params;
  if (JSON.stringify(params.toolOptions) === JSON.stringify(options.value) && (!modelCode.value || params.model === modelCode.value)) return;
  canvas.updateParams(props.nodeId, {
    ...params,
    toolOptions: JSON.parse(JSON.stringify(options.value)),
    ...(modelCode.value ? { model: modelCode.value } : {})
  });
});

/** 节点上的选项被别处改了（撤销、采纳远端版本）：跟着回显。自己写回去的那一次 JSON 相同，不会来回震荡。 */
watch(() => JSON.stringify(node.value?.data.params.toolOptions ?? null), (raw) => {
  if (props.mode !== "node" || raw === JSON.stringify(options.value)) return;
  options.value = imageToolRelightOptionsOf(node.value?.data.params.toolOptions, defaults.value, presets.value);
});

// ---------------------------------------------------------------------------
// 报价与发送
// ---------------------------------------------------------------------------

const quoteKey = computed(() => `${props.nodeId}:relight`);
const toolsError = ref("");

function buildRequest(withInputs: boolean) {
  if (!modelCode.value || !sourceMediaId.value) return null;
  return imageToolRequest({
    projectId: canvas.projectId,
    nodeId: props.nodeId,
    tool: "relight",
    options: options.value,
    modelCode: modelCode.value,
    inputs: withInputs ? generationInputs(canvas.nodes, canvas.edges, props.nodeId) : [],
    params: props.mode === "node" ? node.value?.data.params : undefined
  });
}

/** 报价只看结构化选项（智能模式下灰掉的值不影响请求，`imageToolRequest` 已剥掉，不会白白重报）。 */
watch(() => JSON.stringify([buildRequest(false)?.params.toolOptions ?? null, modelCode.value, sourceMediaId.value]), () => {
  imageTools.quote(quoteKey.value, buildRequest(false));
}, { immediate: true });

const inputHint = computed(() => {
  if (!sourceMediaId.value) return props.mode === "node" ? "上游图片还没有生成结果" : "这张图还没有生成结果";
  if (toolsError.value) return toolsError.value;
  if (toolInfo.value && toolInfo.value.available !== false && !modelCode.value) return "该工具还没有可用的模型";
  return "";
});

const sending = ref(false);

async function send() {
  if (sending.value || !modelCode.value) return;
  sending.value = true;
  try {
    if (props.mode === "spawn") {
      const result = await canvas.spawnToolNode(props.sourceNodeId, "relight", JSON.parse(JSON.stringify(options.value)), {
        title: title.value.spawn || "打光",
        generate: true,
        model: modelCode.value
      });
      if (result.error) ElMessage.error(result.error instanceof Error ? result.error.message : "生成失败");
      if (result.nodeId) emit("close");
      return;
    }
    const request = buildRequest(true);
    if (!request) return;
    await generation.generate(props.nodeId, request);
    canvas.scheduleSave();
  } catch (cause) {
    ElMessage.error(cause instanceof Error ? cause.message : "生成失败");
  } finally {
    sending.value = false;
  }
}

onMounted(async () => {
  try {
    await imageTools.tools();
    toolsError.value = "";
  } catch (cause) {
    toolsError.value = cause instanceof Error && cause.message ? cause.message : "工具列表加载失败";
  }
});

onBeforeUnmount(() => imageTools.clearQuote(quoteKey.value));

const HELP = {
  smart: "打开后由模型按画面内容自行补光；亮度、颜色、方向与轮廓光都不再生效",
  brightness: "主光源的强弱：0% 很暗，100% 很亮",
  color: "光的颜色。「无」= 保留原图的色温",
  rim: "在主体边缘加一圈轮廓光，把人物从背景里勾出来"
};
</script>

<template>
  <ToolPanelShell
    title="打光效果"
    tool="relight"
    :quote="imageTools.quotes[quoteKey]"
    :input-hint="inputHint"
    :sending="sending"
    :send-label="mode === 'spawn' ? '发送：生成打光子节点' : '重新生成'"
    @close="emit('close')"
    @reset="reset"
    @send="send"
  >
    <div class="relight">
      <div class="relight-stage">
        <LightSphere
          :azimuth="options.azimuth ?? 0"
          :elevation="options.elevation ?? 0"
          :thumbnail="thumbnail"
          :color="smart ? null : options.color ?? null"
          :disabled="smart"
          @update="setDirection"
        />
      </div>

      <div class="relight-controls">
        <div class="relight-row relight-global">
          <span class="relight-label">全局</span>
          <div class="relight-switch-label">
            <span>智能模式</span>
            <span class="relight-help" :title="HELP.smart" aria-hidden="true"><CircleHelp :size="12" /></span>
            <button
              type="button"
              role="switch"
              class="relight-switch"
              :aria-checked="smart"
              :aria-label="`智能模式：${HELP.smart}`"
              @click="patch({ smart: !smart })"
            ><i /></button>
          </div>
        </div>

        <fieldset class="relight-fields" :disabled="smart" :aria-disabled="smart">
          <div class="relight-row relight-block">
            <span class="relight-label">亮度<span class="relight-help" :title="HELP.brightness"><CircleHelp :size="12" /></span></span>
            <div class="relight-brightness">
              <input
                type="range"
                min="0"
                max="100"
                step="1"
                :value="brightness"
                aria-label="亮度"
                :aria-valuetext="`${brightness}%`"
                @input="setBrightness(($event.target as HTMLInputElement).value)"
              >
              <label class="relight-percent">
                <input
                  v-model="brightnessText"
                  type="text"
                  inputmode="numeric"
                  aria-label="亮度百分比"
                  @blur="commitBrightnessText"
                  @keydown.enter.prevent="commitBrightnessText"
                >
                <span>%</span>
              </label>
            </div>
          </div>

          <div class="relight-row relight-block relight-color-row">
            <span class="relight-label">颜色<span class="relight-help" :title="HELP.color"><CircleHelp :size="12" /></span></span>
            <button
              type="button"
              class="relight-swatch relight-swatch-current"
              :class="{ none: !options.color }"
              :style="options.color ? { background: options.color } : undefined"
              :title="colorLabel"
              :aria-label="`颜色：${colorLabel}`"
              :aria-expanded="colorOpen"
              @click="toggleColor"
            />
            <span class="relight-color-name">{{ colorLabel }}</span>
            <div v-if="colorOpen" class="relight-palette" role="dialog" aria-label="选择光色" @keydown.esc.stop.prevent="colorOpen = false">
              <button
                type="button"
                class="relight-swatch none"
                :class="{ active: !options.color }"
                title="无（保留原色温）"
                aria-label="无（保留原色温）"
                :aria-pressed="!options.color"
                @click="setColor(null)"
              />
              <button
                v-for="(item, index) in colors"
                :key="item.hex"
                type="button"
                class="relight-swatch"
                :class="{ active: colorIndex === index }"
                :style="{ background: item.hex }"
                :title="item.name"
                :aria-label="item.name"
                :aria-pressed="colorIndex === index"
                @click="setColor(item.hex)"
              />
              <label class="relight-swatch relight-custom" :class="{ active: Boolean(options.color) && colorIndex < 0 }" title="自定义颜色">
                <input
                  type="color"
                  :value="pickerValue"
                  aria-label="自定义颜色"
                  @input="setColor(($event.target as HTMLInputElement).value)"
                >
              </label>
            </div>
          </div>

          <div class="relight-row relight-block">
            <span class="relight-label">主光源</span>
            <div class="relight-presets" role="group" aria-label="主光源方向">
              <button
                v-for="item in presets"
                :key="item.code"
                type="button"
                :class="{ active: activePreset === item.code }"
                :aria-pressed="activePreset === item.code"
                @click="applyPreset(item.code)"
              >
                {{ item.name }}
              </button>
            </div>
            <p v-if="!activePreset && !smart" class="relight-custom-dir">自定义方向：方位 {{ options.azimuth }}°，仰角 {{ options.elevation }}°</p>
          </div>

          <div class="relight-row">
            <span class="relight-label">轮廓光<span class="relight-help" :title="HELP.rim"><CircleHelp :size="12" /></span></span>
            <button
              type="button"
              role="switch"
              class="relight-switch"
              :aria-checked="options.rim === true"
              :disabled="smart"
              aria-label="轮廓光"
              @click="patch({ rim: !options.rim })"
            ><i /></button>
          </div>
        </fieldset>
      </div>
    </div>
  </ToolPanelShell>
</template>

<style scoped>
.relight { display: grid; grid-template-columns: minmax(0, 1fr) 280px; gap: 14px; }
.relight-stage { display: flex; align-items: center; justify-content: center; padding: 10px 8px; border-radius: 8px; background: var(--cv-n20); }
.relight-controls { display: flex; flex-direction: column; gap: 10px; min-width: 0; }
.relight-fields { display: flex; flex-direction: column; gap: 12px; margin: 0; padding: 0; border: 0; min-width: 0; }
.relight-fields:disabled { opacity: .45; }
.relight-row { display: flex; align-items: center; justify-content: space-between; gap: 8px; }
.relight-block { flex-wrap: wrap; justify-content: flex-start; }
.relight-block > .relight-label { flex: 0 0 100%; }
.relight-label { display: inline-flex; align-items: center; gap: 4px; color: var(--cv-n70); font-size: 12px; }
.relight-global { padding-bottom: 8px; border-bottom: 1px solid var(--canvas-line); }
.relight-global > .relight-label { color: var(--cv-n92); font-weight: 600; }
.relight-switch-label { display: inline-flex; align-items: center; gap: 6px; color: var(--cv-n80); font-size: 12px; }
.relight-help { display: inline-flex; color: var(--cv-n54); cursor: help; }

.relight-switch { position: relative; width: 32px; height: 18px; flex: none; border: 0; border-radius: 9px; padding: 0; background: var(--cv-n30); cursor: pointer; transition: background .15s; }
.relight-switch > i { position: absolute; top: 2px; left: 2px; width: 14px; height: 14px; border-radius: 50%; background: var(--cv-n96); transition: transform .15s; }
.relight-switch[aria-checked="true"] { background: var(--canvas-cyan); }
.relight-switch[aria-checked="true"] > i { transform: translateX(14px); }
.relight-switch:disabled { cursor: not-allowed; }
.relight-switch:focus-visible,
.relight-presets > button:focus-visible,
.relight-swatch:focus-visible,
.relight-custom:focus-within { outline: 2px solid var(--canvas-cyan); outline-offset: 1px; }

.relight-brightness { display: flex; flex: 1 1 auto; align-items: center; gap: 10px; }
.relight-brightness > input[type="range"] { flex: 1 1 auto; min-width: 0; accent-color: var(--canvas-cyan); }
.relight-percent { display: inline-flex; align-items: center; width: 62px; height: 26px; border: 1px solid var(--canvas-line-strong); border-radius: 6px; padding: 0 6px; background: var(--cv-n12); color: var(--cv-n70); }
.relight-percent > input { width: 100%; min-width: 0; border: 0; padding: 0; color: var(--cv-n92); background: transparent; font-size: 12px; text-align: right; outline: none; }

.relight-color-row { position: relative; align-items: center; }
.relight-color-name { color: var(--cv-n63); font-size: 11px; }
.relight-swatch { position: relative; width: 22px; height: 22px; flex: none; border: 1px solid var(--canvas-line-strong); border-radius: 5px; padding: 0; cursor: pointer; }
.relight-swatch.active { box-shadow: 0 0 0 2px var(--cv-n15), 0 0 0 3px var(--canvas-cyan); }
/* 「无」= 白底红斜线 */
.relight-swatch.none { background: linear-gradient(to top right, transparent calc(50% - 1px), var(--canvas-danger) calc(50% - 1px), var(--canvas-danger) calc(50% + 1px), transparent calc(50% + 1px)), #fff; }
.relight-swatch-current { width: 26px; height: 26px; }
.relight-swatch:disabled { cursor: not-allowed; }
.relight-palette { position: absolute; z-index: 3; top: calc(100% + 6px); left: 0; display: grid; grid-template-columns: repeat(6, 22px); gap: 7px; padding: 10px; border: 1px solid var(--canvas-line-strong); border-radius: 8px; background: var(--cv-n14); box-shadow: 0 12px 28px var(--cv-shade-42); }
.relight-custom { overflow: hidden; background: conic-gradient(#ff3b30, #ffd23f, #34c759, #24c8db, #2f6bff, #8e5cff, #ff2d95, #ff3b30); }
.relight-custom > input { position: absolute; inset: 0; width: 100%; height: 100%; border: 0; padding: 0; opacity: 0; cursor: pointer; }

.relight-presets { display: grid; flex: 1 1 100%; grid-template-columns: repeat(3, minmax(0, 1fr)); gap: 6px; }
.relight-presets > button { height: 28px; border: 1px solid var(--canvas-line); border-radius: 6px; color: var(--cv-n70); background: var(--cv-n20); font-size: 12px; cursor: pointer; }
.relight-presets > button:hover:not(:disabled) { color: var(--cv-n92); background: var(--cv-n22); }
.relight-presets > button.active { border-color: var(--canvas-cyan); color: var(--cv-n92); background: var(--cv-cyan-52a15); }
.relight-presets > button:disabled { cursor: not-allowed; }
.relight-custom-dir { flex: 1 1 100%; margin: 0; color: var(--cv-n54); font-size: 11px; }
</style>
