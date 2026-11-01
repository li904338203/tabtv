<script setup lang="ts">
/**
 * 高清面板（docs/agent/imagetools IMGT-18，dev/02 §3.5；2026-10-31 方案变更为「高清重绘」）：模型、增强程度、服务端报价、发送。
 *
 * 高清就是让图片大模型做一次图生图重绘（`operation=img2img`，`params.tool=upscale`），
 * 提示词由服务端按增强程度（标准 / 精细 / 极致）拼。所以面板上没有服务商、没有倍数、也没有尺寸上限。
 *
 * props 约定（BaseNode 按它挂载，不要改）：
 * - `nodeId`：面板挂在哪个节点上（spawn 模式下就是源节点；node 模式下是高清子节点）；
 * - `sourceNodeId`：源图节点（spawn 模式 = nodeId；node 模式 = 连线 order=1 的上游，可以是另一个高清节点，IMG-F-57）；
 * - `mode`：`spawn` 发送 = 建子节点 + 立即生成；`node` 改动写进本节点 params、发送 = 对本节点生成（IMGD-22）。
 *
 * ⚠️ **没有提示词输入框**（IMG-F-58）：重绘的提示词完全由服务端按程度给，`imageToolRequest` 也会摘掉节点上残留的 `prompt`。
 * ⚠️ 模型来自 `/image-tools` 高清条目的 `models`（支持 img2img 的图片模型），不走图片节点的模型下拉。
 * ⚠️ 积分只显示服务端报价（= 模型 img2img 单价）；换模型 / 程度立刻重新报价。
 * ⚠️ 只能经 BaseNode 的 defineAsyncComponent 引入（IMG-N-06）。
 */
import { ChevronDown } from "@lucide/vue";
import { ElMessage } from "element-plus";
import ToolPanelShell from "./ToolPanelShell.vue";
import type { JsonObject } from "~/types/canvas";
import { generationInputReady, generationInputs } from "~/utils/canvas";
import { imageToolRequest, imageToolsConfigOf, type ImageToolUpscaleLevel, type ImageToolUpscaleOptions } from "~/utils/imageTools";
import {
  imageToolUpscaleLevels,
  imageToolUpscaleModelCode,
  imageToolUpscaleMoveLevel,
  imageToolUpscaleOptionsOf
} from "~/utils/imageToolUpscale";
import { useImageToolsStore } from "~/stores/imageTools";

const props = defineProps<{ nodeId: string; sourceNodeId: string; mode: "spawn" | "node" }>();
const emit = defineEmits<{ close: [] }>();

const canvas = useCanvasStore();
const configStore = useCanvasConfigStore();
const generation = useGenerationStore();
const imageTools = useImageToolsStore();

const config = computed(() => imageToolsConfigOf(configStore.config?.imageTools));
const levels = computed(() => imageToolUpscaleLevels(config.value.upscaleLevels));
const node = computed(() => canvas.nodes.find(item => item.id === props.nodeId));
/** 源图：源节点当前的产物（上游重新出图后跟着换）。 */
const sourceMediaId = computed(() => canvas.nodes.find(item => item.id === props.sourceNodeId)?.data.generation?.mediaId);

/**
 * 当前选项：node 模式读本节点 `params.toolOptions`（改动即写回、自动保存）；
 * spawn 模式只在面板里（IMG-UX-04：发送前不写画布，关掉就丢）。
 */
const draft = ref<Required<ImageToolUpscaleOptions>>(imageToolUpscaleOptionsOf(undefined));
const options = computed<Required<ImageToolUpscaleOptions>>(() => props.mode === "node"
  ? imageToolUpscaleOptionsOf(node.value?.data.params.toolOptions)
  : draft.value);

/** `/image-tools` 的高清条目：可用性、单价与可选模型。 */
const toolInfo = computed(() => imageTools.toolOf("upscale"));
const models = computed(() => toolInfo.value?.models ?? []);
/** spawn 模式下用户在面板里选的模型（node 模式存在节点 params.model 上）。 */
const spawnModel = ref("");
/** 发给受理的目录模型码：节点 / 面板上选过且仍可用就沿用，否则取列表第一个。 */
const modelCode = computed(() => imageToolUpscaleModelCode(props.mode === "node" ? node.value?.data.params.model : spawnModel.value, models.value));
const modelName = computed(() => models.value.find(item => item.code === modelCode.value)?.name ?? (modelCode.value || "暂无可用模型"));

/** 受理输入：node 模式是本节点的上游连线；spawn 模式由 `spawnToolNode` 建好子节点后自己组。 */
const inputs = computed(() => props.mode === "node" ? generationInputs(canvas.nodes, canvas.edges, props.nodeId) : []);
const sourceReady = computed(() => props.mode === "spawn"
  ? Boolean(sourceMediaId.value)
  : inputs.value.length > 0 && inputs.value.every(generationInputReady));

/** 编辑器自己的前置条件（排在权限与工具不可用之后，由外壳统一排序）。 */
const inputHint = computed(() => {
  if (!sourceReady.value) return "源图还没生成，先在上游节点出图";
  if (!modelCode.value) return "高清还没有可用的模型";
  return "";
});

const quoteKey = computed(() => `${props.nodeId}:upscale`);
const quote = computed(() => imageTools.quotes[quoteKey.value]);
const sending = ref(false);

function request() {
  return imageToolRequest({
    projectId: canvas.projectId,
    nodeId: props.nodeId,
    tool: "upscale",
    options: options.value,
    modelCode: modelCode.value,
    inputs: inputs.value,
    params: props.mode === "node" ? node.value?.data.params : {}
  });
}

function refreshQuote() {
  // 前置条件没满足（没源图 / 没模型）时不报价：原因已经由 inputHint 说清了
  imageTools.quote(quoteKey.value, sourceReady.value && modelCode.value ? request() : null);
}

function update(patch: Partial<ImageToolUpscaleOptions>) {
  const next = imageToolUpscaleOptionsOf({ ...options.value, ...patch });
  if (props.mode === "spawn") {
    draft.value = next;
    return;
  }
  if (!node.value) return;
  canvas.updateParams(props.nodeId, { ...node.value.data.params, toolOptions: { ...next } as JsonObject });
}

function selectModel(code: string) {
  if (props.mode === "spawn") {
    spawnModel.value = code;
    return;
  }
  if (!node.value || node.value.data.params.model === code) return;
  canvas.updateParams(props.nodeId, { ...node.value.data.params, model: code });
}

function selectLevel(level: ImageToolUpscaleLevel) {
  update({ level });
}

/** 分段按钮的方向键（IMG-N-08，radiogroup 的标准交互）：左右 / 上下切换并把焦点移过去，Home / End 到头尾。 */
function onLevelKeydown(event: KeyboardEvent) {
  const next = imageToolUpscaleMoveLevel(options.value.level, event.key, levels.value);
  if (!next) return;
  event.preventDefault();
  selectLevel(next);
  const group = event.currentTarget as HTMLElement | null;
  void nextTick(() => group?.querySelector<HTMLButtonElement>(`[data-level="${next}"]`)?.focus());
}

/** 节点上的目录模型码与列表对不上时写回（例如派生时工具列表还没取到、节点上没带 model）。 */
watch(modelCode, (code) => {
  if (props.mode === "spawn") {
    spawnModel.value = code;
    return;
  }
  if (!code || !node.value || node.value.data.params.model === code) return;
  canvas.updateParams(props.nodeId, { ...node.value.data.params, model: code });
});

/** 打开面板：取（缓存的）工具列表，拿到可用性与模型。失败不拦面板，外壳会按缺省显示。 */
onMounted(() => {
  imageTools.tools().catch(() => undefined);
});

watch(() => JSON.stringify([options.value, modelCode.value, sourceReady.value, inputs.value]), refreshQuote, { immediate: true });

onBeforeUnmount(() => imageTools.clearQuote(quoteKey.value));

async function send() {
  if (sending.value || inputHint.value) return;
  sending.value = true;
  try {
    if (props.mode === "spawn") {
      const result = await canvas.spawnToolNode(props.sourceNodeId, "upscale", { ...options.value }, {
        title: "高清",
        generate: true,
        model: modelCode.value
      });
      if (result.error) throw result.error;
      emit("close");
      return;
    }
    await generation.generate(props.nodeId, request());
    canvas.scheduleSave();
  } catch (cause) {
    ElMessage.error(cause instanceof Error && cause.message ? cause.message : "生成失败");
  } finally {
    sending.value = false;
  }
}
</script>

<template>
  <ToolPanelShell
    title="高清"
    tool="upscale"
    :quote="quote"
    :input-hint="inputHint"
    :sending="sending"
    :resettable="false"
    @close="emit('close')"
    @send="send"
  >
    <div class="upscale-form" data-testid="upscale-panel">
      <div class="upscale-row">
        <span id="upscale-model-label" class="upscale-label">模型</span>
        <label class="canvas-control control-select upscale-select" :class="{ 'is-empty': !models.length }" :title="modelName">
          <span class="control-select-value">{{ modelName }}</span>
          <ChevronDown :size="13" aria-hidden="true" />
          <select
            :value="modelCode"
            :disabled="!models.length"
            aria-labelledby="upscale-model-label"
            data-testid="upscale-model"
            @change="selectModel(($event.target as HTMLSelectElement).value)"
          >
            <option v-for="item in models" :key="item.code" :value="item.code">{{ item.name }}</option>
          </select>
        </label>
      </div>
      <div class="upscale-row">
        <span id="upscale-level-label" class="upscale-label">增强程度</span>
        <div class="upscale-levels" role="radiogroup" aria-labelledby="upscale-level-label" data-testid="upscale-levels" @keydown="onLevelKeydown">
          <button
            v-for="row in levels"
            :key="row.code"
            type="button"
            role="radio"
            :data-level="row.code"
            :aria-checked="row.code === options.level"
            :tabindex="row.code === options.level ? 0 : -1"
            :class="{ active: row.code === options.level }"
            @click="selectLevel(row.code)"
          >{{ row.name }}</button>
        </div>
      </div>
      <p class="upscale-note">用图片模型重绘出更清晰的细节，程度越高改动越多；输出尺寸由模型决定。</p>
    </div>
  </ToolPanelShell>
</template>

<style scoped>
.upscale-form { display: grid; gap: 10px; }
.upscale-row { min-height: 30px; display: flex; align-items: center; justify-content: space-between; gap: 12px; }
.upscale-label { flex: 0 0 auto; color: var(--cv-n70); font-size: 12px; }
.upscale-select { min-width: 160px; max-width: 220px; }
.upscale-select.is-empty { opacity: .6; }
.upscale-levels { display: inline-flex; gap: 3px; padding: 2px; border-radius: 7px; background: var(--cv-n14); }
.upscale-levels button { min-width: 52px; height: 26px; border: 0; border-radius: 5px; padding: 0 10px; color: var(--cv-n70); background: transparent; font-size: 12px; cursor: pointer; }
/* 悬停只提亮文字，不加描边：描边与选中的浅底太像，鼠标停在没选中的档位上看着像选中了两档。 */
.upscale-levels button:hover { color: var(--cv-n92); }
.upscale-levels button:focus-visible { color: var(--cv-n92); outline: 2px solid var(--cv-n70); outline-offset: -2px; }
.upscale-levels button.active { color: var(--cv-n92); background: var(--cv-n26); }
.upscale-note { margin: 0; color: var(--cv-n63); font-size: 11px; line-height: 1.5; }
</style>
