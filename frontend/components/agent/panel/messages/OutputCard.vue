<script setup lang="ts">
/**
 * 生成产出（设计 03 §3.5）。
 *
 * - 生成中：预计时间 + 「取消 ✦x」（写明退回多少）。与画布节点上的取消是**同一个接口**
 *   （`POST /generations/{id}/cancel`），退款只发生一次。超过预计时间说「仍在处理」，不说失败（GAP-14）。
 * - 完成：缩略图**全部列出**（多产出不只给第一张，GO-40）；点击在面板内预览，「在画布中查看」定位节点。
 * - 失败：原因 + 「重试」—— 重试交给 Agent（重新报价、走额度检查），这里不绕过额度直接重跑。
 *
 * ⚠️ 受保护媒体一律 fetch → blob → objectURL（`useAuthenticatedMedia`），卸载 / 换产出时 `release()`；
 *    列表只取 `?w=320` 的缩略图（原图一张就上 MB）。视频不预取（缩略图端点对视频原样返回整片），
 *    显示占位，点开再取。
 */
import { Download, Film, Image as ImageIcon, LoaderCircle, Music, X } from "@lucide/vue";
import { ElMessage, ElMessageBox } from "element-plus";
import { credits, etaText, type OutputItem, type StepOutputs } from "~/utils/agentMessages";
import { mediaThumbUrl } from "~/utils/authorizedMedia";
import { saveBlob, extensionFor, safeFilename } from "~/utils/download";
import { AGENT_PANEL_BRIDGE, NOOP_BRIDGE, apiFailure } from "~/utils/agentPanelBridge";
import OutputVideoTile from "./OutputVideoTile.vue";

const props = defineProps<{
  outputs: StepOutputs;
  title: string;
  startedAt?: string;
  /** 预计耗时（秒）；不知道就不说预计时间 —— 不猜。 */
  estimateSeconds?: number;
  /** 不能操作（只读 / 非发起人）时的原因；空 = 可操作。 */
  readOnlyReason?: string;
}>();
const emit = defineEmits<{ retry: [] }>();

const agent = useAgentStore();
const canvas = useCanvasStore();
const generation = useGenerationStore();
const bridge = inject(AGENT_PANEL_BRIDGE, NOOP_BRIDGE);
const thumbs = useAuthenticatedMedia();
const preview = useOutputPreview();
const cancelling = ref(false);
const now = ref(Date.now());
let clock: ReturnType<typeof setInterval> | undefined;

const thumbKeys = computed(() => props.outputs.items.filter(item => item.type === "image" && item.mediaId).map(item => `${item.idx}:${item.mediaId}`).join("|"));

watch(thumbKeys, () => {
  void thumbs.loadAll(props.outputs.items
    .filter(item => item.type === "image" && item.mediaId)
    .map(item => ({ key: String(item.idx), url: mediaThumbUrl(item.mediaId) })));
}, { immediate: true });

watch(() => props.outputs.state, (state) => {
  clearInterval(clock);
  if (state === "generating") clock = setInterval(() => { now.value = Date.now(); }, 15_000);
}, { immediate: true });
onBeforeUnmount(() => {
  clearInterval(clock);
  thumbs.release();
});

const eta = computed(() => etaText(props.startedAt, props.estimateSeconds, now.value));
// 画布没载入（全屏对话页）时一律按「不在本页画布上」：取消走直连接口，定位交给桥（跳画布页）
const nodeOnCanvas = computed(() => Boolean(bridge.canvasLoaded && props.outputs.nodeId && canvas.nodes.some(node => node.id === props.outputs.nodeId)));

/** 视频产出交给 OutputVideoTile：封面帧 + 真实比例 + 悬浮播放（design/03 R-5）。 */
function isVideo(item: OutputItem) {
  return item.type === "video" && Boolean(item.mediaId);
}
const hasVideo = computed(() => props.outputs.items.some(isVideo));

function iconFor(item: OutputItem) {
  return item.type === "video" ? Film : item.type === "audio" ? Music : ImageIcon;
}

async function cancel() {
  const id = props.outputs.generationId;
  if (!id || cancelling.value) return;
  try {
    await ElMessageBox.confirm(`取消后会退回 ${credits(props.outputs.credits)}，已经在跑的这一次就不会再有产出。`, "取消这次生成", {
      confirmButtonText: "取消生成",
      cancelButtonText: "继续等",
      closeOnClickModal: false
    });
  } catch {
    return;
  }
  cancelling.value = true;
  try {
    // 节点在画布上：走 generation store（同一个接口，顺带把节点上的状态改成「已取消」）
    if (props.outputs.nodeId && nodeOnCanvas.value) await generation.cancel(props.outputs.nodeId, id);
    else await useApi()(`/generations/${id}/cancel`, { method: "POST" });
    ElMessage.success("已取消，积分会按原批次退回");
  } catch (cause) {
    ElMessage.error(apiFailure(cause).message);
  } finally {
    cancelling.value = false;
    void agent.reconcile();
  }
}

function locate() {
  if (!props.outputs.nodeId || !bridge.locate([props.outputs.nodeId])) ElMessage.info("对应的节点已经不在画布上了");
}

async function open(item: OutputItem) {
  if (!props.outputs.generationId) return;
  await preview.open({
    generationId: props.outputs.generationId,
    title: props.title,
    subtitle: props.outputs.modelCode,
    type: item.type,
    filename: safeFilename(props.title || `agent-${props.outputs.generationId}`)
  });
  const position = preview.outputs.value.findIndex((output, index) => (output.index ?? index) === item.idx);
  if (position > 0) await preview.show(position);
}

async function download(item: OutputItem) {
  if (!item.mediaId) return;
  try {
    const blob = await thumbs.blob(`/media/${item.mediaId}`);
    saveBlob(blob, `${safeFilename(`${props.title || "agent"}-${item.idx + 1}`)}.${extensionFor(blob.type)}`);
  } catch (cause) {
    ElMessage.error(cause instanceof Error ? cause.message : "下载没有成功");
  }
}
</script>

<template>
  <div class="agent-output" :class="`is-${outputs.state}`">
    <div v-if="outputs.state === 'generating'" class="agent-output-pending">
      <LoaderCircle :size="14" class="spin" />
      <span>{{ eta }}</span>
      <button v-if="!readOnlyReason" type="button" class="agent-link-button" :disabled="cancelling" @click="cancel">
        <X :size="12" />取消 {{ credits(outputs.credits) }}
      </button>
    </div>

    <div v-else-if="outputs.state === 'failed' || outputs.state === 'cancelled'" class="agent-output-failed">
      <span>{{ outputs.state === 'cancelled' ? '已取消' : '生成没有成功' }}<template v-if="outputs.error">：{{ outputs.error }}</template></span>
      <button v-if="outputs.state === 'failed' && !readOnlyReason" type="button" class="agent-link-button" title="交给 Agent 重新报价后再试（会走额度检查）" @click="emit('retry')">重试</button>
    </div>

    <template v-else>
      <div class="agent-output-grid" :class="{ 'is-single': outputs.items.length === 1, 'has-video': hasVideo }">
        <figure v-for="item in outputs.items" :key="item.idx" class="agent-output-item" :class="{ 'is-video': isVideo(item) }">
          <OutputVideoTile
            v-if="isVideo(item)"
            :media-id="item.mediaId ?? ''"
            :label="`${title} 第 ${item.idx + 1} 项`"
            :filename="`${title || 'agent'}-${item.idx + 1}`"
            @open="open(item)"
          />
          <button v-else type="button" class="agent-output-thumb" :title="item.type === 'text' ? item.text : '点击预览'" :disabled="item.type === 'text'" @click="open(item)">
            <img v-if="thumbs.urls.value[String(item.idx)]" :src="thumbs.urls.value[String(item.idx)]" :alt="`${title} 第 ${item.idx + 1} 项`">
            <span v-else-if="item.type === 'text'" class="agent-output-text">{{ item.text }}</span>
            <span v-else class="agent-output-placeholder"><component :is="iconFor(item)" :size="20" /></span>
          </button>
          <!-- 视频卡片自带下载；单个视频时不再在下方重复「video」与下载按钮，多个产出时仍标「第 N 项」。 -->
          <figcaption v-if="!isVideo(item) || outputs.items.length > 1">
            <span>{{ outputs.items.length > 1 ? `第 ${item.idx + 1} 项` : item.type }}</span>
            <button v-if="item.mediaId && !isVideo(item)" type="button" title="下载" @click="download(item)"><Download :size="12" /></button>
          </figcaption>
        </figure>
        <p v-if="!outputs.items.length" class="agent-muted">任务已完成，但没有返回产物</p>
      </div>
      <div class="agent-output-actions">
        <button v-if="outputs.nodeId" type="button" class="agent-link-button" @click="locate">在画布中查看</button>
        <span v-if="outputs.credits" class="agent-muted">{{ credits(outputs.credits) }}</span>
      </div>
    </template>

    <Teleport to="body">
      <MediaPreviewDialog
        v-if="preview.opened.value"
        :type="preview.type.value"
        :src="preview.currentSrc.value"
        :title="preview.request.value?.title"
        :subtitle="preview.request.value?.subtitle"
        :loading="preview.loading.value"
        :error="preview.error.value"
        :index="preview.index.value"
        :count="preview.outputs.value.length"
        @close="preview.close"
        @prev="preview.step(-1)"
        @next="preview.step(1)"
        @retry="preview.retry"
      >
        <template #actions>
          <span class="media-kind">{{ preview.type.value }}</span>
          <button type="button" class="product-primary-command" :disabled="!preview.currentSrc.value || preview.loading.value" @click="preview.download">下载</button>
        </template>
      </MediaPreviewDialog>
    </Teleport>
  </div>
</template>
