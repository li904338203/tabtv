<script setup lang="ts">
import type { components } from '~/types/legacy';
import { ArrowUp, FileText, Image as ImageIcon, Languages, LoaderCircle, Maximize2, Minimize2, Sparkles, X } from "@lucide/vue";
import { ElMessage } from "element-plus";
import type { CanvasNodeData } from "~/types/canvas";
import type { ScriptDetail, ScriptEntryMode, ScriptSourceType } from "~/types/script";
import { orderedTargetEdges, scriptCanGenerateFromPanel, scriptCanvasReferences, scriptEntryMode as resolveScriptEntryMode, scriptHasStoryboard, scriptReferencesForEntryMode, scriptRequestReferences, scriptTranslatedInput } from "~/utils/canvas";
import { scriptStoryInput } from "~/utils/scriptContract";

const props = defineProps<{ nodeId: string; data: CanvasNodeData }>();
const canvas = useCanvasStore();
const scripts = useScriptApi();
const config = useCanvasConfigStore();
const inputText = ref(String(props.data.params.sourceText ?? ""));
const model = ref(String(props.data.params.model ?? ""));
const translatePrompt = ref(Boolean(props.data.params.translatePrompt));
const modelOptions = ref<components["schemas"]["ModelSummaryResp"][]>([]);
const submitting = ref(false);
// 服务端报告的在途状态：刷新页面后本地 submitting 会丢，靠它恢复"生成中"
const serverRunning = ref(false);
const loading = computed(() => submitting.value || serverRunning.value);
const expanded = ref(false);
let runningTimer: ReturnType<typeof setInterval> | undefined;

const existingScriptId = computed(() => String(props.data.params.scriptId ?? ""));
const hasStoryboard = computed(() => scriptHasStoryboard(props.data.params));
const sourceType = computed<ScriptSourceType | "">(() => {
  const value = props.data.params.sourceType;
  if (value === "script" || value === "character" || value === "manual") return value;
  return existingScriptId.value ? "manual" : "";
});
const references = computed(() => scriptCanvasReferences(canvas.nodes, canvas.edges, props.nodeId));
const entryMode = computed(() => resolveScriptEntryMode(
  props.data.params,
  references.value.map(reference => reference.type)
));
const relevantReferences = computed(() => scriptReferencesForEntryMode(references.value, entryMode.value));
/**
 * 引用文本的摘要。只有连线和图标时，用户无法确认"引用的到底是哪段内容"——
 * 之前上游内容被静默丢弃就没人察觉，正是因为界面上看不见它。
 */
const REFERENCE_EXCERPT_CHARS = 140;
const referenceTextPreviews = computed(() => references.value
  .filter(reference => reference.type === "text" && reference.text)
  .map(reference => {
    const text = String(reference.text ?? "").trim();
    return {
      nodeId: reference.nodeId,
      title: reference.title ?? "文本节点",
      length: text.length,
      excerpt: text.length > REFERENCE_EXCERPT_CHARS
        ? `${text.slice(0, REFERENCE_EXCERPT_CHARS)}…`
        : text
    };
  }));
const sourceReady = computed(() => {
  if (entryMode.value === "direct" || entryMode.value === "manual") return true;
  if (entryMode.value === "text_reference") {
    return relevantReferences.value.length >= 1 && relevantReferences.value.every(reference => reference.ready);
  }
  if (entryMode.value === "character_reference") {
    return relevantReferences.value.length >= 2 && relevantReferences.value.slice(0, 2).every(reference => reference.ready);
  }
  return false;
});
const canGenerate = computed(() => scriptCanGenerateFromPanel(
  entryMode.value,
  model.value,
  sourceReady.value,
  inputText.value
));
const modeHint = computed(() => {
  if (!entryMode.value) {
    if (!inputText.value.trim()) return "请输入剧情描述";
    if (!model.value) return "请选择文本模型";
    return "生成分镜脚本";
  }
  if (hasStoryboard.value) return "已有分镜，可重新生成或打开脚本工作台";
  if (existingScriptId.value) return entryMode.value === "manual" ? "脚本已创建，请在工作台添加镜头" : "脚本已创建，输入提示词后生成分镜";
  if (entryMode.value === "direct") return inputText.value.trim() ? "剧情已就绪" : "请输入剧情描述";
  if (entryMode.value === "text_reference") return sourceReady.value ? "剧本文本已就绪" : "请先完成上游文本节点";
  if (entryMode.value === "character_reference") return sourceReady.value ? "角色参考已就绪" : "请先完成两个上游角色节点";
  return "直接进入空白镜头表";
});
const promptPlaceholder = computed(() => {
  if (!entryMode.value || entryMode.value === "direct") return "描述剧本片段、故事，为你生成分镜脚本";
  if (entryMode.value === "character_reference") return "描述两个角色参与的故事、场景和剧情走向";
  if (entryMode.value === "text_reference") return "补充剧本类型、节奏、时长或分镜要求";
  return "可在脚本工作台中直接编写镜头表";
});
const submitLabel = computed(() => hasStoryboard.value ? "重新生成分镜脚本" : "生成分镜脚本");
const submitTitle = computed(() => {
  if (loading.value) return "分镜生成中，请稍候";
  return canGenerate.value ? submitLabel.value : modeHint.value;
});

function effectiveInputText() {
  return scriptTranslatedInput(inputText.value, translatePrompt.value);
}

async function loadModels() {
  try {
    modelOptions.value = await config.models("text", "storyboard_gen");
    if (!modelOptions.value.some(item => item.code === model.value)) model.value = modelOptions.value[0]?.code ?? "";
    if (model.value && props.data.params.model !== model.value) persistDraft();
  } catch {
    modelOptions.value = [];
  }
}

function persistDraft() {
  canvas.updateParams(props.nodeId, {
    ...props.data.params,
    sourceText: inputText.value,
    model: model.value,
    translatePrompt: translatePrompt.value
  });
}

function toggleTranslation() {
  translatePrompt.value = !translatePrompt.value;
  persistDraft();
}

function selectReference(nodeId: string) {
  canvas.selectedNodeId = nodeId;
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

function referenceIcon(type: string) {
  return type === "image" ? ImageIcon : FileText;
}

function syncCanvasScript(
  detail: ScriptDetail,
  references: ReturnType<typeof scriptRequestReferences>,
  mode: ScriptEntryMode
) {
  canvas.updateParams(props.nodeId, {
    ...props.data.params,
    scriptId: detail.scriptId,
    sourceType: detail.sourceType,
    scriptEntryMode: mode,
    sourceText: inputText.value,
    sourceReferences: references,
    model: model.value,
    scriptStatus: detail.status,
    scriptStep: detail.currentStep,
    scriptVersion: detail.version,
    scriptShotCount: detail.shots.length,
    scriptPromptReady: detail.promptsProgress.completed,
    scriptImagePromptReady: detail.shots.filter(shot => shot.imagePromptStatus === "ready").length,
    scriptVideoPromptReady: detail.shots.filter(shot => shot.videoPromptStatus === "ready").length
  });
}

/** 轮询服务端在途状态：生成中时每 3 秒一次，结束即停表。 */
function syncRunningState(running: boolean) {
  serverRunning.value = running;
  if (running && !runningTimer) {
    runningTimer = setInterval(() => void pollRunning(), 3000);
  } else if (!running && runningTimer) {
    clearInterval(runningTimer);
    runningTimer = undefined;
  }
}

async function pollRunning() {
  if (!existingScriptId.value) return syncRunningState(false);
  try {
    const detail = await scripts.load(existingScriptId.value, { name: props.data.title });
    syncRunningState(Boolean(detail.storyboardRunning));
    // 生成完成：把最新镜头数等同步到画布，无需用户手动刷新
    if (!detail.storyboardRunning) applyScriptDetail(detail);
  } catch {
    // 接口暂时不可用时保持现状，下一次轮询再试
  }
}

/** 把服务端详情合并进画布参数（仅在有变化时写，避免无谓的保存请求）。 */
function applyScriptDetail(detail: ScriptDetail) {
  const next = {
    ...props.data.params,
    scriptId: detail.scriptId,
    sourceType: detail.sourceType,
    scriptEntryMode: resolveScriptEntryMode(
      { ...props.data.params, sourceType: detail.sourceType },
      references.value.map(reference => reference.type)
    ),
    scriptStatus: detail.status,
    scriptStep: detail.currentStep,
    scriptVersion: detail.version,
    scriptShotCount: detail.shots.length,
    scriptPromptReady: detail.promptsProgress.completed,
    scriptImagePromptReady: detail.shots.filter(shot => shot.imagePromptStatus === "ready").length,
    scriptVideoPromptReady: detail.shots.filter(shot => shot.videoPromptStatus === "ready").length,
    ...(detail.input.text && !props.data.params.sourceText ? { sourceText: detail.input.text } : {})
  };
  if (JSON.stringify(next) !== JSON.stringify(props.data.params)) canvas.updateParams(props.nodeId, next);
}

async function hydrateExistingScript() {
  if (!existingScriptId.value) return;
  try {
    const detail = await scripts.load(existingScriptId.value, { name: props.data.title });
    syncRunningState(Boolean(detail.storyboardRunning));
    if (!inputText.value && detail.input.text) inputText.value = detail.input.text;
    applyScriptDetail(detail);
  } catch {
    // Keep the local draft usable while the script API is temporarily unavailable.
  }
}

async function start() {
  if (loading.value) return;
  persistDraft();
  const requestEntryMode: ScriptEntryMode = entryMode.value || "direct";
  const requestSourceType: ScriptSourceType = sourceType.value || "script";
  if (requestEntryMode === "direct") {
    if (!inputText.value.trim()) {
      ElMessage.warning("请输入剧情描述");
      return;
    }
    if (!model.value) {
      ElMessage.warning("请选择文本模型");
      return;
    }
  }
  if (requestEntryMode === "manual") {
    ElMessage.info(hasStoryboard.value ? "请通过节点中的按钮打开脚本工作台" : "请在初次进入时打开脚本工作台并添加镜头");
    return;
  }
  if (requestEntryMode !== "direct" && !sourceReady.value) {
    ElMessage.warning(modeHint.value);
    return;
  }
  if (requestEntryMode === "character_reference" && !inputText.value.trim()) {
    ElMessage.warning("请输入包含两个角色的剧情描述");
    return;
  }
  if (!model.value) {
    ElMessage.warning("请选择文本模型");
    return;
  }
  submitting.value = true;
  try {
    const input = {
      text: effectiveInputText() || undefined,
      assetIds: [],
      // 校验用 relevantReferences（按模式），请求带全部已连接上游 —— 用户连了线就是想让它参与
      references: scriptRequestReferences(references.value)
    };
    const hadStoryboard = hasStoryboard.value;
    // 剧情存在脚本上（rawInput），上游引用走补充上下文（extraContext）—— 分法见 utils/scriptContract
    const story = scriptStoryInput(requestEntryMode, input.text ?? "", input.references);
    const extras = { name: props.data.title, references: input.references };
    let current: ScriptDetail;
    if (!existingScriptId.value) {
      current = await scripts.create({
        projectId: canvas.projectId,
        nodeId: props.nodeId,
        sourceType: requestSourceType,
        rawInput: story.rawInput,
        ...extras
      });
      canvas.updateParams(props.nodeId, {
        ...props.data.params,
        scriptId: current.scriptId,
        sourceType: requestSourceType,
        scriptEntryMode: requestEntryMode,
        sourceText: inputText.value,
        sourceReferences: input.references,
        model: model.value,
        scriptStatus: current.status,
        scriptStep: current.currentStep,
        scriptVersion: current.version,
        scriptShotCount: current.shots.length,
        scriptPromptReady: current.promptsProgress.completed
      });
    } else {
      current = await scripts.load(existingScriptId.value, extras);
    }
    // ⚠️ 生成读的是脚本上存的剧情：变了先写进去（同一节点重建时回的是已有那条，剧情可能是旧的）
    current = await scripts.saveStory(current, story.rawInput);
    const detail = await scripts.generateStoryboard(current.scriptId, model.value, story.extraContext, extras);
    syncCanvasScript(detail, input.references, requestEntryMode);
    ElMessage.success(hadStoryboard ? "分镜脚本已重新生成" : "分镜脚本生成成功");
  } catch (cause) {
    ElMessage.error(cause instanceof Error ? cause.message : "脚本节点创建失败");
  } finally {
    submitting.value = false;
    // 请求已返回，服务端不该再有 running 记录；同步一次避免残留
    serverRunning.value = false;
  }
}

watch(() => props.data.params, (params) => {
  if (typeof params.sourceText === "string" && params.sourceText !== inputText.value) inputText.value = params.sourceText;
  if (typeof params.model === "string" && params.model !== model.value) model.value = params.model;
  if (typeof params.translatePrompt === "boolean" && params.translatePrompt !== translatePrompt.value) translatePrompt.value = params.translatePrompt;
}, { deep: true });

onMounted(async () => {
  await loadModels();
  await hydrateExistingScript();
});

onBeforeUnmount(() => {
  if (runningTimer) clearInterval(runningTimer);
});
</script>

<template>
  <section class="script-node-panel nodrag nowheel" :class="{ empty: !sourceType, expanded }" @mousedown.stop>
    <button class="script-panel-expand" type="button" :title="expanded ? '收起输入框' : '展开输入框'" :aria-label="expanded ? '收起输入框' : '展开输入框'" @click.stop="expanded = !expanded"><Minimize2 v-if="expanded" :size="15" /><Maximize2 v-else :size="15" /></button>
    <div v-if="references.length" class="node-reference-strip script-reference-strip" aria-label="脚本上游引用">
      <div v-for="reference in references" :key="reference.edgeId" class="node-reference-token" :class="{ pending: !reference.ready }">
        <button class="node-reference-open" type="button" :title="reference.ready ? reference.title ?? '上游节点' : `${reference.title ?? '上游节点'} 尚未完成`" @click="selectReference(reference.nodeId)">
          <span class="node-reference-order">{{ reference.order }}</span>
          <img v-if="reference.type === 'image' && reference.previewUrl" :src="reference.previewUrl" :alt="reference.title ?? '角色参考'">
          <component :is="referenceIcon(reference.type)" v-else :size="23" />
        </button>
        <button v-if="!hasStoryboard" class="node-reference-remove" type="button" title="移除引用" @click="removeReference(reference.edgeId)"><X :size="11" /></button>
      </div>
    </div>
    <div v-if="referenceTextPreviews.length" class="script-reference-preview" aria-label="引用的文本内容">
      <article v-for="preview in referenceTextPreviews" :key="preview.nodeId">
        <header><FileText :size="12" /><span>{{ preview.title }}</span><small>{{ preview.length }} 字</small></header>
        <p>{{ preview.excerpt }}</p>
      </article>
    </div>
    <textarea v-model="inputText" :placeholder="promptPlaceholder" @blur="persistDraft" />
    <div class="script-entry-footer">
      <label class="script-model-select" title="文本模型">
        <Sparkles :size="20" />
        <select v-model="model" aria-label="文本模型" @change="persistDraft()">
          <option value="" disabled>选择模型</option>
          <option v-for="item in modelOptions" :key="item.code" :value="item.code">{{ item.name ?? item.code }}</option>
        </select>
      </label>
      <div class="script-entry-actions">
        <button class="script-translate" :class="{ active: translatePrompt }" type="button" :aria-pressed="translatePrompt" title="翻译并优化提示词" @click="toggleTranslation"><Languages :size="20" /></button>
        <button v-if="entryMode !== 'manual'" class="script-entry-submit" type="button" :disabled="loading || !canGenerate" :title="submitTitle" :aria-label="submitTitle" :aria-busy="loading" @click="start">
          <LoaderCircle v-if="loading" :size="18" class="spin" />
          <ArrowUp v-else :size="22" />
        </button>
      </div>
      <span class="script-source-warning" aria-live="polite">{{ entryMode === 'manual' || !sourceReady ? modeHint : '' }}</span>
    </div>
  </section>
</template>
