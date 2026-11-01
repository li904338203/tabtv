<script setup lang="ts">
/**
 * 多角度编辑器（docs/agent/imagetools IMG-F-30~38，dev/02 §3.3）。
 *
 * props 约定（BaseNode 按它挂载，不要改）：
 * - `nodeId`：面板挂在哪个节点上（spawn 模式下就是源节点；node 模式下是工具子节点）；
 * - `sourceNodeId`：源图节点（spawn 模式 = nodeId；node 模式 = 连线 order=1 的上游）；
 * - `mode`：`spawn` 发送 = 建「多角度」子节点 + 立即生成（IMG-F-35）；`node` 改动写进本节点 params、
 *   发送 = 对本节点重新生成（IMG-F-38、IMGD-22）。
 * emit `close`：关闭面板。
 *
 * ⚠️ 只能经 BaseNode 的 defineAsyncComponent 引入（IMG-N-06）。
 * ⚠️ 不在前端拼提示词：只发 `{preset, horizontal, vertical, zoom, prompt?}`，鱼眼 / 倾斜的附加词由服务端按预设码加（IMG-F-36）。
 * ⚠️ 积分只显示服务端报价（IMG-UX-02），禁用理由由外壳统一排序。
 */
import { ElMessage } from "element-plus";
import ToolPanelShell from "./ToolPanelShell.vue";
import CameraSphere from "./CameraSphere.vue";
import { generationInputs, outputMediaIdOf } from "~/utils/canvas";
import {
  IMAGE_TOOL_MULTI_ANGLE_PROMPT_MAX,
  imageToolDefaultOptions,
  imageToolLabel,
  imageToolPresetToOptions,
  imageToolRequest,
  imageToolSetAngle,
  imageToolsConfigOf,
  imageToolZoomLabel,
  type ImageToolMultiAngleOptions,
  type ImageToolRequestInput
} from "~/utils/imageTools";
import {
  multiAngleHorizontalText,
  multiAngleOptionsOf,
  multiAnglePromptClamp,
  multiAnglePromptLength,
  multiAngleStateFromParams,
  multiAngleVerticalText,
  type MultiAngleEditorState
} from "~/utils/imageToolMultiAngle";
import { useImageToolsStore } from "~/stores/imageTools";
import type { JsonObject } from "~/types/canvas";

const TOOL = "multi_angle" as const;

const props = defineProps<{ nodeId: string; sourceNodeId: string; mode: "spawn" | "node" }>();
const emit = defineEmits<{ close: [] }>();

const canvas = useCanvasStore();
const configStore = useCanvasConfigStore();
const generation = useGenerationStore();
const imageTools = useImageToolsStore();

const config = computed(() => imageToolsConfigOf(configStore.config?.imageTools));
const presets = computed(() => config.value.multiAnglePresets);

const node = computed(() => canvas.nodes.find(item => item.id === props.nodeId));
const sourceNode = computed(() => canvas.nodes.find(item => item.id === props.sourceNodeId));
const sourceGeneration = computed(() => sourceNode.value?.data.generation);
/** 源图媒体 ID：源节点的产物（没有产物时不报价、不让发送）。 */
const sourceMediaId = computed(() => outputMediaIdOf(sourceGeneration.value));
/** 相机里的源图缩略：节点卡已经取好的 object URL，不再请求一次。 */
const thumbUrl = computed(() => {
  const view = sourceGeneration.value;
  return view?.previewUrl && view.outputType !== "video" && view.outputType !== "audio" ? view.previewUrl : "";
});

/** 编辑器状态：node 模式带上次的值（IMG-F-38），spawn 模式从默认值开始。 */
const state = ref<MultiAngleEditorState>(props.mode === "node"
  ? multiAngleStateFromParams(node.value?.data.params, config.value)
  : { options: imageToolDefaultOptions(TOOL, { config: config.value }), promptOn: false, promptText: "" });

const options = computed(() => state.value.options);
const wireOptions = computed(() => multiAngleOptionsOf(state.value));
const zoomLabel = computed(() => imageToolZoomLabel(options.value.zoom ?? 5, config.value.zoomBands));
const promptLength = computed(() => multiAnglePromptLength(state.value.promptText));

const toolInfo = computed(() => imageTools.toolOf(TOOL));
/** 模型：子节点上已经选定的优先，否则取 `/image-tools` 给的第一个（按 sort 排好的）。 */
const modelCode = computed(() => {
  const saved = props.mode === "node" ? node.value?.data.params.model : undefined;
  if (typeof saved === "string" && saved) return saved;
  return toolInfo.value?.models?.[0]?.code ?? "";
});

const quoteKey = computed(() => `${props.nodeId}:${TOOL}`);
const quote = computed(() => imageTools.quotes[quoteKey.value]);
const sending = ref(false);

/** 编辑器自己的前置条件（排在权限与工具不可用之后，由外壳统一排序）。 */
const inputHint = computed(() => {
  if (!sourceMediaId.value) return "源图还没有出图，先生成或上传一张图";
  if (toolInfo.value?.available && !modelCode.value) return "多角度还没有可用的模型，请联系管理员配置";
  return "";
});

function setOptions(next: ImageToolMultiAngleOptions) {
  state.value = { ...state.value, options: next };
}

function choosePreset(code: string) {
  setOptions(imageToolPresetToOptions(presets.value, code, options.value));
}

function setAngle(key: "horizontal" | "vertical" | "zoom", event: Event) {
  setOptions(imageToolSetAngle(options.value, key, Number((event.target as HTMLInputElement).value)));
}

function togglePrompt() {
  state.value = { ...state.value, promptOn: !state.value.promptOn };
}

function onPromptInput(event: Event) {
  const target = event.target as HTMLTextAreaElement;
  const clamped = multiAnglePromptClamp(target.value);
  if (clamped !== target.value) target.value = clamped;
  state.value = { ...state.value, promptText: clamped };
}

/** 重置 = 0°、0°、景别 5、自定义、关提示词（IMG-F-34）。 */
function reset() {
  state.value = { options: imageToolDefaultOptions(TOOL, { config: config.value }), promptOn: false, promptText: "" };
}

/** 受理 / 报价的输入：node 模式按本节点的连线；spawn 模式子节点还不存在，按源节点的产物声明一张图。 */
function requestInputs(): ImageToolRequestInput[] {
  if (props.mode === "node") return generationInputs(canvas.nodes, canvas.edges, props.nodeId);
  const view = sourceGeneration.value;
  const generationId = view?.status === "success" && view.id ? String(view.id) : undefined;
  return [{
    sourceNodeId: props.sourceNodeId,
    kind: "image",
    order: 1,
    ...(generationId ? { generationId } : sourceMediaId.value ? { referenceId: sourceMediaId.value } : {})
  }];
}

function buildRequest() {
  if (!modelCode.value || !sourceMediaId.value) return null;
  return imageToolRequest({
    projectId: canvas.projectId,
    nodeId: props.nodeId,
    tool: TOOL,
    options: wireOptions.value,
    modelCode: modelCode.value,
    inputs: requestInputs(),
    params: props.mode === "node" ? node.value?.data.params : undefined
  });
}

function refreshQuote() {
  imageTools.quote(quoteKey.value, buildRequest());
}

watch(() => JSON.stringify([wireOptions.value, modelCode.value, sourceMediaId.value]), refreshQuote);

/** node 模式：改动写进本节点 params（IMGD-22），下次打开带着这次的值。 */
watch(wireOptions, (value) => {
  if (props.mode !== "node" || !node.value) return;
  const params = node.value.data.params;
  if (JSON.stringify(params.toolOptions ?? null) === JSON.stringify(value)) return;
  canvas.updateParams(props.nodeId, { ...params, toolOptions: value as JsonObject });
});

async function send() {
  if (sending.value || !modelCode.value) return;
  sending.value = true;
  try {
    if (props.mode === "spawn") {
      const result = await canvas.spawnToolNode(props.sourceNodeId, TOOL, wireOptions.value, {
        title: imageToolLabel(config.value, TOOL).spawn || "多角度",
        generate: true,
        model: modelCode.value
      });
      if (!result.nodeId) {
        ElMessage.error("源图节点不存在，无法派生");
        return;
      }
      if (result.error) ElMessage.error(result.error instanceof Error ? result.error.message : "生成失败");
      emit("close");
      return;
    }
    const request = buildRequest();
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
  await imageTools.tools().catch(() => undefined);
  refreshQuote();
});

onBeforeUnmount(() => imageTools.clearQuote(quoteKey.value));
</script>

<template>
  <ToolPanelShell
    title="多角度编辑器"
    :tool="TOOL"
    :quote="quote"
    :input-hint="inputHint"
    :sending="sending"
    :send-label="mode === 'spawn' ? '生成多角度' : '重新生成'"
    class="multi-angle-editor"
    @close="emit('close')"
    @reset="reset"
    @send="send"
  >
    <div class="ma-presets" role="radiogroup" aria-label="视角预设">
      <button
        v-for="preset in presets"
        :key="preset.code"
        type="button"
        role="radio"
        class="ma-preset"
        :class="{ active: options.preset === preset.code }"
        :aria-checked="options.preset === preset.code"
        :data-preset="preset.code"
        @click="choosePreset(preset.code)"
      >{{ preset.name }}</button>
    </div>
    <div class="ma-main">
      <CameraSphere :model-value="options" :thumb-url="thumbUrl" @update:model-value="setOptions" />
      <div class="ma-controls">
        <label class="ma-slider">
          <span class="ma-slider-head"><span>水平环绕</span><output :title="multiAngleHorizontalText(options.horizontal ?? 0)">{{ options.horizontal ?? 0 }}°</output></span>
          <input
            type="range"
            min="0"
            max="360"
            step="1"
            :value="options.horizontal ?? 0"
            :aria-valuetext="`${options.horizontal ?? 0}°，${multiAngleHorizontalText(options.horizontal ?? 0)}`"
            data-testid="ma-horizontal"
            @input="setAngle('horizontal', $event)"
          >
        </label>
        <label class="ma-slider">
          <span class="ma-slider-head"><span>垂直俯仰</span><output :title="multiAngleVerticalText(options.vertical ?? 0)">{{ options.vertical ?? 0 }}°</output></span>
          <input
            type="range"
            min="-30"
            max="90"
            step="1"
            :value="options.vertical ?? 0"
            :aria-valuetext="`${options.vertical ?? 0}°，${multiAngleVerticalText(options.vertical ?? 0)}`"
            data-testid="ma-vertical"
            @input="setAngle('vertical', $event)"
          >
        </label>
        <label class="ma-slider">
          <span class="ma-slider-head"><span>景别缩放</span><output>{{ zoomLabel }}</output></span>
          <input
            type="range"
            min="0"
            max="10"
            step="1"
            :value="options.zoom ?? 5"
            :aria-valuetext="`${options.zoom ?? 5}，${zoomLabel}`"
            data-testid="ma-zoom"
            @input="setAngle('zoom', $event)"
          >
        </label>
        <div class="ma-prompt-row">
          <span id="ma-prompt-label">提示词</span>
          <button
            type="button"
            role="switch"
            class="ma-switch"
            :class="{ on: state.promptOn }"
            :aria-checked="state.promptOn"
            aria-labelledby="ma-prompt-label"
            data-testid="ma-prompt-switch"
            @click="togglePrompt"
          ><span class="ma-switch-dot" /></button>
        </div>
        <div v-if="state.promptOn" class="ma-prompt">
          <textarea
            :value="state.promptText"
            rows="3"
            :maxlength="IMAGE_TOOL_MULTI_ANGLE_PROMPT_MAX * 2"
            placeholder="补充描述（可选），例如：保持人物服装不变"
            aria-label="补充提示词"
            data-testid="ma-prompt"
            @input="onPromptInput"
          />
          <span class="ma-prompt-count">{{ promptLength }}/{{ IMAGE_TOOL_MULTI_ANGLE_PROMPT_MAX }}</span>
        </div>
      </div>
    </div>
  </ToolPanelShell>
</template>

<style scoped>
.ma-presets { display: flex; flex-wrap: wrap; gap: 6px; margin-bottom: 10px; }
.ma-preset { height: 26px; border: 1px solid var(--cv-n24); border-radius: 6px; padding: 0 10px; color: var(--cv-n70); background: var(--cv-n16); font-size: 12px; cursor: pointer; }
.ma-preset:hover,
.ma-preset:focus-visible { color: var(--cv-n92); border-color: var(--cv-n42); outline: none; }
.ma-preset.active { color: var(--cv-n92); border-color: var(--cv-n57); background: var(--cv-n24); }
.ma-main { display: flex; gap: 16px; align-items: flex-start; }
.ma-controls { min-width: 220px; display: flex; flex: 1 1 auto; flex-direction: column; gap: 12px; padding-top: 4px; }
.ma-slider { display: flex; flex-direction: column; gap: 6px; }
.ma-slider-head { display: flex; align-items: center; justify-content: space-between; color: var(--cv-n70); font-size: 12px; }
.ma-slider-head output { color: var(--cv-n92); font-variant-numeric: tabular-nums; }
.ma-slider input[type="range"] { width: 100%; margin: 0; accent-color: var(--cv-n86); cursor: pointer; }
.ma-slider input[type="range"]:focus-visible { outline: 2px solid var(--cv-cyan-52a50); outline-offset: 2px; }
.ma-prompt-row { display: flex; align-items: center; justify-content: space-between; color: var(--cv-n70); font-size: 12px; }
.ma-switch { position: relative; width: 32px; height: 18px; flex: none; border: 0; border-radius: 9px; padding: 0; background: var(--cv-n28); cursor: pointer; transition: background 0.15s; }
.ma-switch.on { background: var(--cv-cyan-60); }
.ma-switch:focus-visible { outline: 2px solid var(--cv-cyan-52a50); outline-offset: 2px; }
.ma-switch-dot { position: absolute; top: 2px; left: 2px; width: 14px; height: 14px; border-radius: 50%; background: var(--cv-n92); transition: transform 0.15s; }
.ma-switch.on .ma-switch-dot { transform: translateX(14px); }
.ma-prompt { position: relative; }
.ma-prompt textarea { width: 100%; box-sizing: border-box; resize: none; border: 1px solid var(--cv-n24); border-radius: 6px; padding: 6px 8px 18px; color: var(--cv-n92); background: var(--cv-n12); font: inherit; font-size: 12px; line-height: 1.5; }
.ma-prompt textarea:focus { border-color: var(--cv-n42); outline: none; }
.ma-prompt-count { position: absolute; right: 8px; bottom: 5px; color: var(--cv-n57); font-size: 10px; }
</style>
