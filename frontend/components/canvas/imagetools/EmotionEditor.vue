<script setup lang="ts">
/**
 * 情绪调节（docs/agent/imagetools IMGT-17，IMG-F-10~22，dev/02 §3.2）。
 *
 * props 约定（BaseNode 按它挂载）：
 * - `nodeId`：面板挂在哪个节点上（spawn 模式下就是源节点；node 模式下是工具子节点）；
 * - `sourceNodeId`：源图节点（spawn 模式 = nodeId；node 模式 = 连线 order=1 的上游）；
 * - `mode`：`spawn` 发送 = 建子节点 + 立即生成；`node` 改动写进本节点 params、发送 = 对本节点重新生成（IMGD-22）。
 * emit `close`：关闭面板。
 * ⚠️ 只能经 BaseNode 的 defineAsyncComponent 引入（IMG-N-06）。
 *
 * 方案（2026-10-31 变更）：**只有手动框选**，没有自动人物识别。服务端把每个框裁出来交给图片模型（img2img）
 * 改表情，再羽化贴回原图 —— 输出就是原图分辨率，所以没有分辨率选项，只有张数（1~4）。
 *
 * 布局：头部（张数 / 积分 / 发送，外壳给）→ 角色条（脸部小图 chip + 「框选添加」）→ 正文：左边大面板
 * 有两个视图「画面」（源图 + 角色框 + 框选，IMG-F-12）与「示意」（25 张示意头像，随选中格切换，IMG-F-16），
 * 右边 5×5 坐标盘 → 底部「情绪定位 {名}」。
 * 打开时默认在「画面」页签并处于框选状态（没有识别可等，第一步就是框人）；在坐标盘上选格子自动切到「示意」
 * （选情绪时要看的是表情），点「框选添加」切回「画面」。
 *
 * ⚠️ 只发被调整过的角色（IMG-F-15）：存进节点的 `toolOptions.roles` 也只有它们，整图运行按节点参数执行时
 *    不会把框了但没调整的人也改掉。
 */
import { ElMessage } from "element-plus";
import { ChevronDown, ScanFace, SquareDashed, X } from "@lucide/vue";
import ToolPanelShell from "./ToolPanelShell.vue";
import EmotionGrid from "./EmotionGrid.vue";
import EmotionFaceCanvas from "./EmotionFaceCanvas.vue";
import type { JsonObject } from "~/types/canvas";
import { generationInputs, outputMediaIdOf } from "~/utils/canvas";
import {
  imageToolEmotionAt,
  imageToolEmotionImage,
  imageToolLabel,
  imageToolRequest,
  imageToolRoleLabels,
  imageToolRoleLimitReason,
  imageToolsConfigOf,
  type ImageToolPixelBox
} from "~/utils/imageTools";
import {
  imageToolEmotionAddRole,
  imageToolEmotionFaceCrop,
  imageToolEmotionHeadFallback,
  imageToolEmotionInputHint,
  imageToolEmotionOptions,
  imageToolEmotionOrdered,
  imageToolEmotionPickSelected,
  imageToolEmotionQuoteOptions,
  imageToolEmotionRemoveRole,
  imageToolEmotionRestore,
  imageToolEmotionSetRole,
  imageToolEmotionSourceChanged,
  type ImageToolEmotionState
} from "~/utils/imageToolEmotion";
import { useImageToolsStore } from "~/stores/imageTools";

const TOOL = "expression_edit" as const;

const props = defineProps<{ nodeId: string; sourceNodeId: string; mode: "spawn" | "node" }>();
const emit = defineEmits<{ close: [] }>();

const canvas = useCanvasStore();
const generation = useGenerationStore();
const configStore = useCanvasConfigStore();
const imageTools = useImageToolsStore();
const media = useAuthenticatedMedia();

const config = computed(() => imageToolsConfigOf(configStore.config?.imageTools));
const title = computed(() => imageToolLabel(config.value, TOOL).name);
const maxRoles = computed(() => config.value.maxRoles);
const sourceNode = computed(() => canvas.nodes.find(node => node.id === props.sourceNodeId));
const targetNode = computed(() => canvas.nodes.find(node => node.id === props.nodeId));
const sourceMediaId = computed(() => outputMediaIdOf(sourceNode.value?.data.generation) ?? "");
const quoteKey = computed(() => `${props.nodeId}:${TOOL}`);

// —— 编辑器状态 ————————————————————————————————————————————————
const state = reactive<ImageToolEmotionState>({ count: 1, roles: [] });
const selectedId = ref("");
const view = ref<"head" | "image">("image");
const drawing = ref(false);
/** node 模式：上游换了图，旧的人物框已作废（IMG-F-21）。 */
const sourceChanged = ref(false);
const sending = ref(false);
const notice = ref("");
let manualSeq = 0;

const ordered = computed(() => imageToolEmotionOrdered(state.roles));
const labels = computed(() => imageToolRoleLabels(state.roles));
const selected = computed(() => state.roles.find(role => role.id === selectedId.value));
const current = computed(() => ({ arousal: selected.value?.arousal ?? 0, warmth: selected.value?.warmth ?? 0 }));
const currentEmotion = computed(() => imageToolEmotionAt(config.value.emotions, current.value.arousal, current.value.warmth));
const limitReason = computed(() => imageToolRoleLimitReason(state.roles.length, maxRoles.value));
const inputHint = computed(() => {
  if (!sourceMediaId.value) return "源图还没有生成，先在上游节点出图";
  return imageToolEmotionInputHint(state.roles);
});

// —— 模型：节点上存了就用，否则取 `/image-tools` 里这个工具的第一个模型 ——————————————
const toolModel = ref("");
const model = computed(() => {
  const stored = props.mode === "node" ? targetNode.value?.data.params?.model : undefined;
  return typeof stored === "string" && stored ? stored : toolModel.value;
});

// —— 源图（受保护媒体，取 640 缩略图；框的换算一律按原图尺寸）——————————————————————
const natural = ref({ width: 0, height: 0 });
const imageUrl = computed(() => media.urls.value.source ?? "");
const faces = ref<Record<string, string>>({});
let faceImage: HTMLImageElement | null = null;

async function loadSource(mediaId: string) {
  media.release();
  faces.value = {};
  faceImage = null;
  if (!mediaId) return;
  await media.appendAll([{ key: "source", url: `/api/v1/media/${encodeURIComponent(mediaId)}?w=640` }]);
}

/** 脸部小图：在已取到的源图上用 <canvas> 裁（IMGD-29，不再请求服务端）。 */
function cropFaces() {
  const img = faceImage;
  if (!img || !img.naturalWidth || typeof document === "undefined") return;
  const next: Record<string, string> = {};
  for (const role of state.roles) {
    const crop = imageToolEmotionFaceCrop(role.box, natural.value, { width: img.naturalWidth, height: img.naturalHeight });
    if (!crop) continue;
    const cell = document.createElement("canvas");
    cell.width = 48;
    cell.height = 48;
    const ctx = cell.getContext("2d");
    if (!ctx) continue;
    ctx.drawImage(img, crop.sx, crop.sy, crop.side, crop.side, 0, 0, 48, 48);
    try {
      next[role.id] = cell.toDataURL("image/jpeg", 0.82);
    } catch {
      // 画布被污染（理论上不会：object URL 同源）就不显示小图，退回占位图标
    }
  }
  faces.value = next;
}

watch(imageUrl, (url) => {
  faceImage = null;
  if (!url || typeof Image === "undefined") return;
  const img = new Image();
  img.onload = () => {
    if (media.urls.value.source !== url) return;
    faceImage = img;
    cropFaces();
  };
  img.src = url;
});
watch(() => [JSON.stringify(state.roles.map(role => [role.id, role.box])), natural.value.width, natural.value.height], cropFaces);

// —— 打开：还原节点上的框（node 模式），进入「画面」框选 ——————————————————————————————————
async function open(mediaId: string, initial: boolean) {
  const params = targetNode.value?.data.params as Record<string, unknown> | undefined;
  if (props.mode === "node" && initial) {
    const changed = imageToolEmotionSourceChanged(params, mediaId);
    sourceChanged.value = changed;
    const restored = imageToolEmotionRestore(changed ? { ...(params?.toolOptions as object), roles: [] } : params?.toolOptions);
    state.count = restored.count;
    state.roles = restored.roles;
  } else {
    // 上游换了图（面板开着时）：旧框全部作废
    if (!initial && props.mode === "node") sourceChanged.value = true;
    state.roles = [];
  }
  selectedId.value = imageToolEmotionPickSelected(state.roles, "");
  notice.value = "";
  // 默认就在「画面」里框人（还有名额时直接进入框选）
  view.value = "image";
  drawing.value = Boolean(mediaId) && !imageToolRoleLimitReason(state.roles.length, maxRoles.value);
  natural.value = { width: 0, height: 0 };
  if (!mediaId) return;
  const cachedSize = imageTools.sizeOf(mediaId);
  if (cachedSize?.width) natural.value = { ...cachedSize };
  void loadSource(mediaId);
  void imageTools.tools()
    .then((list) => {
      // ⚠️ 只认编辑器条目（presetGroup=editor），同 store.toolOf
      toolModel.value = list.find(item => item.code === TOOL && item.presetGroup === "editor")?.models?.[0]?.code ?? "";
    })
    .catch(() => { toolModel.value = ""; });
  // 原图像素尺寸：框的「显示 ↔ 原图」换算全靠它；取不到时画面里会说明暂时不能框选
  const size = await imageTools.sourceSize(mediaId);
  if (size.width && size.height && mediaId === sourceMediaId.value) natural.value = { ...size };
}

watch(sourceMediaId, (value, previous) => void open(value, previous === undefined), { immediate: true });

// —— node 模式：改动写回节点 params（IMGD-22）——————————————————————————————————————
function persist() {
  if (props.mode !== "node" || !targetNode.value || !sourceMediaId.value) return;
  const params: JsonObject = {
    ...(targetNode.value.data.params as JsonObject),
    tool: TOOL,
    toolOptions: imageToolEmotionOptions(sourceMediaId.value, state) as unknown as JsonObject,
    toolSourceMediaId: sourceMediaId.value,
    ...(model.value ? { model: model.value } : {})
  };
  canvas.updateParams(props.nodeId, params);
}

// —— 交互 ——————————————————————————————————————————————————————————————————
function selectRole(id: string) {
  selectedId.value = id;
}

function pickEmotion(value: { arousal: number; warmth: number }) {
  if (!selected.value) return;
  state.roles = imageToolEmotionSetRole(state.roles, selected.value.id, value.arousal, value.warmth);
  view.value = "head";
  persist();
}

function startManual() {
  if (limitReason.value) {
    notice.value = limitReason.value;
    return;
  }
  notice.value = "";
  view.value = "image";
  drawing.value = !drawing.value;
}

function addManual(box: ImageToolPixelBox) {
  manualSeq += 1;
  const result = imageToolEmotionAddRole(state.roles, box, `${Date.now().toString(36)}-${manualSeq}`, maxRoles.value);
  if (result.error) {
    notice.value = result.error;
    drawing.value = false;
    return;
  }
  state.roles = result.roles;
  selectedId.value = result.id ?? selectedId.value;
  drawing.value = false;
  notice.value = "";
  persist();
}

function onDrawRejected() {
  notice.value = "框太小了（每边至少 24 像素），再拖大一点";
}

function removeRole(id: string) {
  state.roles = imageToolEmotionRemoveRole(state.roles, id);
  selectedId.value = imageToolEmotionPickSelected(state.roles, selectedId.value === id ? "" : selectedId.value);
  notice.value = "";
  persist();
}

function onChipKeydown(event: KeyboardEvent, id: string) {
  if (event.key === "Delete" || event.key === "Backspace") {
    event.preventDefault();
    event.stopPropagation();
    removeRole(id);
  }
}

function setCount(value: string) {
  state.count = Math.min(4, Math.max(1, Number(value) || 1));
  persist();
}

/** 重置参数：张数回默认，所有角色回到「未调整」，框保留（框是用户亲手画的，重置不该让他重画一遍）。 */
function reset() {
  state.count = 1;
  state.roles = state.roles.map(role => ({ ...role, arousal: 0, warmth: 0, touched: false }));
  notice.value = "";
  persist();
}

// —— 示意头像：优先 `imageToolEmotionImage`（配置下发的地址），取不到时退回程序化生成的 SVG ——————
const headMissing = ref(false);
const headSrc = computed(() => {
  const { arousal, warmth } = current.value;
  if (headMissing.value) return imageToolEmotionHeadFallback(arousal, warmth);
  return currentEmotion.value?.image || imageToolEmotionImage(arousal, warmth);
});
function onHeadError() {
  if (!headMissing.value) headMissing.value = true;
}

// —— 报价（服务端，IMG-F-74）—————————————————————————————————————————————————
watch(
  () => JSON.stringify([sourceMediaId.value, model.value, state.count, state.roles]),
  () => {
    const options = imageToolEmotionQuoteOptions(sourceMediaId.value, state);
    if (!options || !model.value) {
      imageTools.quote(quoteKey.value, null);
      return;
    }
    const request = imageToolRequest({
      projectId: canvas.projectId,
      nodeId: props.nodeId,
      tool: TOOL,
      options,
      modelCode: model.value,
      inputs: []
    });
    imageTools.quote(quoteKey.value, request);
  },
  { immediate: true }
);
const quote = computed(() => imageTools.quotes[quoteKey.value]);
onBeforeUnmount(() => imageTools.clearQuote(quoteKey.value));

// —— 发送 ——————————————————————————————————————————————————————————————————
async function send() {
  if (sending.value || inputHint.value || !sourceMediaId.value) return;
  if (!model.value) {
    ElMessage.error("该工具还没有可用的模型");
    return;
  }
  const options = imageToolEmotionOptions(sourceMediaId.value, state);
  sending.value = true;
  try {
    if (props.mode === "spawn") {
      const result = await canvas.spawnToolNode(props.sourceNodeId, TOOL, options, {
        title: imageToolLabel(config.value, TOOL).spawn,
        generate: true,
        model: model.value
      });
      if (result.error) ElMessage.error(result.error instanceof Error && result.error.message ? result.error.message : "生成失败");
      emit("close");
      return;
    }
    const node = targetNode.value;
    if (!node) return;
    const params: JsonObject = {
      ...(node.data.params as JsonObject),
      tool: TOOL,
      toolOptions: options as unknown as JsonObject,
      toolSourceMediaId: sourceMediaId.value,
      model: model.value
    };
    canvas.updateParams(props.nodeId, params);
    const request = imageToolRequest({
      projectId: canvas.projectId,
      nodeId: props.nodeId,
      tool: TOOL,
      options,
      modelCode: model.value,
      inputs: generationInputs(canvas.nodes, canvas.edges, props.nodeId),
      params
    });
    await generation.generate(props.nodeId, request);
    sourceChanged.value = false;
    canvas.scheduleSave();
  } catch (cause) {
    ElMessage.error(cause instanceof Error && cause.message ? cause.message : "生成失败");
  } finally {
    sending.value = false;
  }
}

const statusText = computed(() => sourceMediaId.value ? "" : "源图还没有生成，先在上游节点出图");
</script>

<template>
  <ToolPanelShell
    :title="title"
    :tool="TOOL"
    :quote="quote"
    :input-hint="inputHint"
    :sending="sending"
    class="emotion-editor"
    @close="emit('close')"
    @reset="reset"
    @send="send"
  >
    <template #header>
      <label class="emotion-select" title="一次出几张">
        <span class="sr-only">张数</span>
        <select :value="String(state.count)" aria-label="张数" @change="setCount(($event.target as HTMLSelectElement).value)">
          <option v-for="n in 4" :key="n" :value="String(n)">{{ n }}张</option>
        </select>
        <ChevronDown :size="12" aria-hidden="true" />
      </label>
    </template>

    <div class="emotion-roles" role="toolbar" aria-label="角色">
      <div
        v-for="role in ordered"
        :key="role.id"
        class="emotion-role"
        :class="{ 'is-selected': role.id === selectedId, 'is-touched': role.touched }"
      >
        <button
          type="button"
          class="emotion-role-main"
          :aria-pressed="role.id === selectedId"
          :title="role.touched ? `${labels[role.id]}：${imageToolEmotionAt(config.emotions, role.arousal, role.warmth)?.name ?? ''}` : `${labels[role.id]}：未调整（不会被修改）`"
          :data-testid="`emotion-role-${role.id}`"
          @click="selectRole(role.id)"
          @keydown="onChipKeydown($event, role.id)"
        >
          <img v-if="faces[role.id]" :src="faces[role.id]" alt="" class="emotion-role-face">
          <span v-else class="emotion-role-face"><ScanFace :size="13" /></span>
          <span>{{ labels[role.id] }}</span>
          <span v-if="role.touched" class="emotion-role-dot" aria-label="已调整" />
        </button>
        <button type="button" class="emotion-role-remove" :aria-label="`删除${labels[role.id]}`" title="删除这个角色" @click="removeRole(role.id)">
          <X :size="11" />
        </button>
      </div>
      <button
        type="button"
        class="emotion-manual"
        :class="{ 'is-active': drawing }"
        :aria-pressed="drawing"
        :disabled="Boolean(limitReason) || !sourceMediaId"
        :title="limitReason || (drawing ? '取消框选' : '在画面上拖出一个框，添加一个角色')"
        data-testid="emotion-manual-add"
        @click="startManual"
      >
        <SquareDashed :size="13" />{{ drawing ? "取消框选" : "框选添加" }}
      </button>
    </div>

    <p v-if="sourceChanged" class="emotion-status is-warning" role="status" data-testid="emotion-source-changed">
      上游图片已变化，请重新框选人物（旧的框已清掉）
    </p>
    <p v-if="statusText" class="emotion-status" role="status" data-testid="emotion-status">{{ statusText }}</p>
    <p v-if="notice" class="emotion-status" role="status">{{ notice }}</p>

    <div class="emotion-body">
      <div class="emotion-stage">
        <div class="emotion-stage-tabs" role="tablist" aria-label="左侧视图">
          <button type="button" role="tab" :aria-selected="view === 'image'" :class="{ 'is-active': view === 'image' }" data-testid="emotion-tab-image" @click="view = 'image'">画面</button>
          <button type="button" role="tab" :aria-selected="view === 'head'" :class="{ 'is-active': view === 'head' }" data-testid="emotion-tab-head" @click="view = 'head'; drawing = false">示意</button>
        </div>
        <div v-show="view === 'head'" class="emotion-head" role="tabpanel">
          <img :src="headSrc" :alt="`示意：${currentEmotion?.name ?? ''}`" loading="lazy" draggable="false" @error="onHeadError">
        </div>
        <EmotionFaceCanvas
          v-show="view === 'image'"
          role="tabpanel"
          :src="imageUrl"
          :natural="natural"
          :roles="state.roles"
          :labels="labels"
          :selected-id="selectedId"
          :drawing="drawing"
          @draw="addManual"
          @draw-rejected="onDrawRejected"
          @select="selectRole"
          @remove="removeRole"
        />
      </div>
      <div class="emotion-pad">
        <EmotionGrid
          :emotions="config.emotions"
          :arousal="current.arousal"
          :warmth="current.warmth"
          :disabled="!selected"
          :label="selected ? `${labels[selected.id]} 的情绪` : '情绪坐标'"
          @select="pickEmotion"
        />
      </div>
    </div>

    <template #footer>
      <p class="emotion-position" aria-live="polite">
        <span>情绪定位</span>
        <strong>{{ selected ? (currentEmotion?.name ?? "—") : "—" }}</strong>
        <em v-if="selected && !selected.touched">（未调整）</em>
      </p>
    </template>
  </ToolPanelShell>
</template>

<style scoped>
.emotion-select { position: relative; display: inline-flex; align-items: center; height: 28px; border-radius: 6px; color: var(--cv-n88); background: var(--cv-n20); }
.emotion-select:focus-within { box-shadow: 0 0 0 2px var(--cv-n70); }
.emotion-select select { height: 28px; appearance: none; border: 0; padding: 0 22px 0 10px; color: inherit; background: transparent; font-size: 12px; cursor: pointer; outline: none; }
.emotion-select select option { color: var(--cv-n92); background: var(--cv-n15); }
.emotion-select > svg { position: absolute; right: 7px; color: var(--cv-n63); pointer-events: none; }
.sr-only { position: absolute; width: 1px; height: 1px; overflow: hidden; clip: rect(0 0 0 0); white-space: nowrap; }

.emotion-roles { display: flex; flex-wrap: wrap; align-items: center; gap: 6px; margin-bottom: 8px; }
.emotion-role { display: inline-flex; align-items: center; height: 30px; border: 1px solid var(--cv-n24); border-radius: 15px; background: var(--cv-n18); }
.emotion-role.is-selected { border-color: var(--cv-n88); background: var(--cv-n22); }
.emotion-role-main { height: 100%; display: inline-flex; align-items: center; gap: 6px; border: 0; padding: 0 4px 0 3px; color: var(--cv-n88); background: transparent; font-size: 12px; cursor: pointer; }
.emotion-role-main:focus-visible,
.emotion-role-remove:focus-visible,
.emotion-manual:focus-visible,
.emotion-stage-tabs button:focus-visible { outline: 2px solid var(--cv-n88); outline-offset: 1px; border-radius: 12px; }
.emotion-role-face { width: 24px; height: 24px; display: grid; place-items: center; flex: none; border-radius: 50%; object-fit: cover; color: var(--cv-n63); background: var(--cv-n24); }
.emotion-role-dot { width: 6px; height: 6px; border-radius: 50%; background: var(--cv-n100); }
.emotion-role-remove { width: 22px; height: 22px; display: grid; place-items: center; margin-right: 3px; border: 0; border-radius: 50%; padding: 0; color: var(--cv-n63); background: transparent; cursor: pointer; }
.emotion-role-remove:hover { color: var(--cv-n92); background: var(--cv-n24); }
.emotion-manual { height: 30px; display: inline-flex; align-items: center; gap: 5px; border: 1px dashed var(--cv-n33); border-radius: 15px; padding: 0 11px; color: var(--cv-n78); background: transparent; font-size: 12px; cursor: pointer; }
.emotion-manual:hover:not(:disabled),
.emotion-manual.is-active { color: var(--cv-n92); border-color: var(--cv-n70); background: var(--cv-n20); }
.emotion-manual:disabled { opacity: .45; cursor: not-allowed; }

.emotion-status { display: flex; align-items: center; gap: 8px; margin: 0 0 8px; color: var(--cv-n63); font-size: 11px; line-height: 1.45; }
.emotion-status.is-warning { padding: 6px 9px; border: 1px solid var(--cv-gold-64a18); border-radius: 6px; color: var(--cv-gold-64); background: var(--cv-gold-64a6); }

.emotion-body { display: grid; grid-template-columns: minmax(0, 1fr) 260px; gap: 10px; height: 284px; }
.emotion-stage { position: relative; min-width: 0; height: 100%; border-radius: 12px; background: var(--cv-n12); overflow: hidden; }
.emotion-stage-tabs { position: absolute; z-index: 2; top: 8px; left: 8px; display: inline-flex; gap: 2px; padding: 2px; border-radius: 7px; background: var(--cv-n15); }
.emotion-stage-tabs button { height: 22px; border: 0; border-radius: 5px; padding: 0 9px; color: var(--cv-n63); background: transparent; font-size: 11px; cursor: pointer; }
.emotion-stage-tabs button.is-active { color: var(--cv-n92); background: var(--cv-n24); }
/* ⚠️ 绝对定位铺满舞台：放在网格里按百分比算高度的话，图片按原始尺寸撑开，头像下半截被舞台的 overflow 裁掉。 */
/* ⚠️ 图片给**明确的宽高** + contain：放在网格里只写百分比 max-height 的话不生效（网格行高由内容撑开），
   图片按宽度铺满、头像下半截被舞台的 overflow 裁掉。上边留出页签的位置。 */
.emotion-head { position: absolute; inset: 0; }
.emotion-head img { position: absolute; top: 36px; left: 12px; width: calc(100% - 24px); height: calc(100% - 48px); object-fit: contain; user-select: none; }
.emotion-pad { min-width: 0; height: 100%; }
.emotion-pad :deep(.emotion-grid) { background: var(--cv-n12); }

.emotion-position { display: inline-flex; flex: 0 0 auto; align-items: baseline; gap: 10px; margin: 0; color: var(--cv-n63); font-size: 12px; }
.emotion-position strong { color: var(--cv-n92); font-size: 13px; font-weight: 600; }
.emotion-position em { color: var(--cv-n54); font-size: 11px; font-style: normal; }
</style>
