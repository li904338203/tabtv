<script setup lang="ts">
/**
 * 视频节点的画面与底部控制条（2026-11-01 第二版，按用户给的参考图加回控制条；同日第一版把控制条和播放键整个去掉了）。
 *
 * - 鼠标移上去**静音**自动播放、移开暂停并回到开头（第一版的悬浮预览，保留）。
 *   ⚠️ 必须静音：浏览器只放行静音的自动播放，带声音的 play() 不在用户点击里会被拒。
 * - 鼠标在画面上、或节点选中时，底部出控制条：播放 / 暂停、当前时间、进度条（点按 / 拖动跳转）、总时长、声音、截帧。
 * - 动过控制条（播放、暂停、拖进度、开声音）就交给用户，像普通播放器：移开不再暂停、不再回到开头，播完停在结尾。
 *   等它停下来、鼠标也不在上面、节点也没选中，才回到悬浮预览（静音、回到开头）—— 不会有一个看不见的视频一直出声。
 * - 截帧：当前这一帧存成 PNG、上传成参考素材，在视频节点右侧建一个图片节点（与「添加资源」同一形态，一步撤销）。
 *
 * 按需加载（BaseNode 的 defineAsyncComponent）：画布页首屏包体积有上限（scripts/director-bundle-check.mjs）。
 * ⚠️ 控制条叠在拖动层（.node-media-drag-surface，z-index 2）之上，带 nodrag / nopan 并拦下 pointerdown / mousedown：
 *    否则按住进度条拖动的是整个节点，点一下按钮还会选中节点、弹出生成面板。
 * ⚠️ 视频地址是 stores/generation 带鉴权取回来再转成的 blob: 地址，同源 —— 画到 canvas 上不会被污染，截帧才取得到像素。
 */
import { Camera, LoaderCircle, Pause, Play, Volume2, VolumeX } from "@lucide/vue";
import { useVueFlow } from "@vue-flow/core";
import { ElMessage } from "element-plus";
import { formatVideoClock, frameCaptureNodePosition, imagePreviewHeight, videoSeekTime } from "~/utils/canvas";

const props = defineProps<{ nodeId: string; title: string; src: string; selected: boolean }>();
const emit = defineEmits<{ meta: [size: { width: number; height: number }] }>();

const canvas = useCanvasStore();
const { getNodes } = useVueFlow();
const { upload } = useReferenceUpload();
const { addResourceNodes } = useCanvasResourceNodes();

const videoEl = ref<HTMLVideoElement>();
const trackEl = ref<HTMLElement>();
const hover = ref(false);
/** 播放归谁管：`preview` 悬浮预览（静音、移开回到开头）；`user` 用户动过控制条，播放停在哪由用户决定。 */
const mode = ref<"preview" | "user">("preview");
const paused = ref(true);
const muted = ref(true);
const currentTime = ref(0);
const duration = ref(0);
const seeking = ref(false);
const capturing = ref(false);

const controlsVisible = computed(() => hover.value || props.selected || seeking.value || capturing.value
  || (mode.value === "user" && !paused.value));
const progress = computed(() => (duration.value > 0 ? Math.min(1, currentTime.value / duration.value) : 0));

function onMeta() {
  const video = videoEl.value;
  if (!video) return;
  syncDuration();
  if (video.videoWidth && video.videoHeight) emit("meta", { width: video.videoWidth, height: video.videoHeight });
}

function syncDuration() {
  const value = videoEl.value?.duration ?? 0;
  duration.value = Number.isFinite(value) && value > 0 ? value : 0;
}

function syncState() {
  const video = videoEl.value;
  if (!video) return;
  paused.value = video.paused;
  muted.value = video.muted;
  if (!seeking.value) currentTime.value = video.currentTime;
}

// 播放中且控制条看得见时逐帧读进度：timeupdate 一秒只来四次，进度点会一跳一跳的
let frame = 0;
function tick() {
  const video = videoEl.value;
  if (video && !seeking.value) currentTime.value = video.currentTime;
  frame = requestAnimationFrame(tick);
}
watch(() => controlsVisible.value && !paused.value, (running) => {
  cancelAnimationFrame(frame);
  frame = running ? requestAnimationFrame(tick) : 0;
});

/** 回到悬浮预览的静止态：暂停、静音、回到开头（卡片静止时显示第一帧，与封面一致）。 */
function rewind() {
  const video = videoEl.value;
  if (!video) return;
  video.pause();
  video.muted = true;
  if (video.currentTime) video.currentTime = 0;
}

function onEnter() {
  hover.value = true;
  const video = videoEl.value;
  if (!video || mode.value === "user") return;
  video.muted = true;
  void video.play().catch(() => {
    // 浏览器仍拦下（极少见）：停在第一帧，不报错
  });
}

function onLeave() {
  hover.value = false;
  if (mode.value === "preview") rewind();
}

// 用户接管之后：停下来了、鼠标不在上面、节点也没选中 —— 这一轮看完了，回到悬浮预览
watch([hover, () => props.selected, paused, seeking], () => {
  if (mode.value !== "user" || !paused.value || hover.value || props.selected || seeking.value) return;
  mode.value = "preview";
  rewind();
});

// 换了一份产出（重新生成）：从悬浮预览重新开始
watch(() => props.src, () => {
  mode.value = "preview";
  paused.value = true;
  currentTime.value = 0;
  duration.value = 0;
  if (videoEl.value) videoEl.value.muted = true;
});

function togglePlay() {
  const video = videoEl.value;
  if (!video) return;
  mode.value = "user";
  if (video.paused || video.ended) void video.play().catch(syncState);
  else video.pause();
}

function toggleMute() {
  const video = videoEl.value;
  if (!video) return;
  mode.value = "user";
  video.muted = !video.muted;
}

function seekTo(clientX: number) {
  const video = videoEl.value;
  const track = trackEl.value;
  if (!video || !track || !duration.value) return;
  const time = videoSeekTime(clientX, track.getBoundingClientRect(), duration.value);
  currentTime.value = time;
  video.currentTime = time;
}

function onSeekStart(event: PointerEvent) {
  if (event.button !== 0 || !duration.value) return;
  mode.value = "user";
  seeking.value = true;
  trackEl.value?.setPointerCapture(event.pointerId);
  seekTo(event.clientX);
}

function onSeekMove(event: PointerEvent) {
  if (seeking.value) seekTo(event.clientX);
}

function onSeekEnd(event: PointerEvent) {
  if (!seeking.value) return;
  seeking.value = false;
  if (trackEl.value?.hasPointerCapture(event.pointerId)) trackEl.value.releasePointerCapture(event.pointerId);
}

/** 键盘：进度条聚焦时左右方向键各跳 5 秒。 */
function onSeekKey(event: KeyboardEvent) {
  const video = videoEl.value;
  const step = event.key === "ArrowLeft" ? -5 : event.key === "ArrowRight" ? 5 : 0;
  if (!video || !step || !duration.value) return;
  event.preventDefault();
  mode.value = "user";
  video.currentTime = Math.min(duration.value, Math.max(0, video.currentTime + step));
  currentTime.value = video.currentTime;
}

/** 等当前位置的画面解码出来：只读了元数据（preload="metadata"）的视频要先请求一次当前位置，浏览器才会去解码这一帧。 */
function waitForFrame(video: HTMLVideoElement, timeoutMs: number): Promise<void> {
  if (video.readyState >= HTMLMediaElement.HAVE_CURRENT_DATA) return Promise.resolve();
  return new Promise<void>((resolve, reject) => {
    let timer = 0;
    const finish = (ok: boolean) => {
      window.clearTimeout(timer);
      video.removeEventListener("loadeddata", onReady);
      video.removeEventListener("seeked", onReady);
      if (ok) resolve();
      else reject(new Error("画面还没加载出来，请稍后再试"));
    };
    const onReady = () => finish(true);
    timer = window.setTimeout(() => finish(false), timeoutMs);
    video.addEventListener("loadeddata", onReady);
    video.addEventListener("seeked", onReady);
    // 原地跳一下（跳到当前位置）：浏览器因此去取这一帧并解码
    const position = video.currentTime;
    video.currentTime = position;
  });
}

/** 当前这一帧编码成 PNG（视频的原始分辨率）。 */
async function frameBlob(video: HTMLVideoElement): Promise<Blob> {
  await waitForFrame(video, 3000);
  const { videoWidth: width, videoHeight: height } = video;
  if (!width || !height) throw new Error("画面还没加载出来，请稍后再试");
  const surface = document.createElement("canvas");
  surface.width = width;
  surface.height = height;
  const context = surface.getContext("2d");
  if (!context) throw new Error("浏览器无法生成图片");
  context.drawImage(video, 0, 0, width, height);
  const blob = await new Promise<Blob | null>(resolve => surface.toBlob(resolve, "image/png"));
  if (!blob) throw new Error("浏览器无法生成图片");
  return blob;
}

async function captureFrame() {
  const video = videoEl.value;
  if (!video || capturing.value) return;
  capturing.value = true;
  const clock = formatVideoClock(video.currentTime);
  const title = `${props.title} 截帧 ${clock}`;
  try {
    const blob = await frameBlob(video);
    const fileName = `${title.replace(/[\\/:*?"<>|\s]+/g, "-")}.png`;
    const mediaId = await upload(new File([blob], fileName, { type: "image/png" }), { mediaType: "image", projectId: canvas.projectId });
    const source = canvas.nodes.find(node => node.id === props.nodeId);
    const occupied = getNodes.value.map(node => ({ x: node.position.x, y: node.position.y, width: node.dimensions.width, height: node.dimensions.height }));
    // 新图片节点的大小按这一帧的比例估：卡片宽 390，标题栏约 70（与图片节点的预览高度同一套算法）
    const size = { width: 390, height: 70 + imagePreviewHeight(366, video.videoWidth, video.videoHeight) };
    const position = source ? frameCaptureNodePosition(source.position, occupied, size) : undefined;
    // addNode 会把选中切到新节点：切回原来的选中，不打断用户手上的事（比如接着截下一帧）。
    // ⚠️ 在 await 之前切回：建节点是 addResourceNodes 里同步的那一段，同一个 tick 里切回去，新节点不会闪一下选中态（弹出生成面板）
    const selectedBefore = canvas.selectedNodeId;
    const created = addResourceNodes([{ type: "image", mediaId, title }], { position });
    canvas.selectedNodeId = selectedBefore;
    await created;
    ElMessage.success(`已截取 ${clock} 的画面，放在视频节点右侧`);
  } catch (cause) {
    ElMessage.error(cause instanceof Error && cause.message ? `截帧失败：${cause.message}` : "截帧失败，请稍后再试");
  } finally {
    capturing.value = false;
  }
}

onBeforeUnmount(() => cancelAnimationFrame(frame));
</script>

<template>
  <div class="node-video-output nowheel" data-testid="node-video" @mouseenter="onEnter" @mouseleave="onLeave">
    <video
      ref="videoEl"
      class="nodrag"
      :src="src"
      :loop="mode === 'preview'"
      muted
      playsinline
      preload="metadata"
      :draggable="false"
      @loadedmetadata="onMeta"
      @durationchange="syncDuration"
      @play="syncState"
      @pause="syncState"
      @ended="syncState"
      @seeked="syncState"
      @timeupdate="syncState"
      @volumechange="syncState"
    />
    <span class="node-media-drag-surface" aria-hidden="true" />
    <!-- ⚠️ `.stop`：双击冒泡到画布的话 Vue Flow 会顺手把视口放大一级 -->
    <div
      v-show="controlsVisible"
      class="video-node-controls nodrag nopan"
      data-testid="node-video-controls"
      @pointerdown.stop
      @mousedown.stop
      @dblclick.stop
    >
      <button type="button" class="video-node-button" :title="paused ? '播放' : '暂停'" :aria-label="paused ? '播放' : '暂停'" @click.stop="togglePlay">
        <Play v-if="paused" :size="15" fill="currentColor" />
        <Pause v-else :size="15" fill="currentColor" />
      </button>
      <span class="video-node-time">{{ formatVideoClock(currentTime) }}</span>
      <div
        ref="trackEl"
        class="video-node-track"
        role="slider"
        tabindex="0"
        aria-label="播放进度"
        :aria-valuemin="0"
        :aria-valuemax="Math.round(duration)"
        :aria-valuenow="Math.round(currentTime)"
        :aria-valuetext="formatVideoClock(currentTime)"
        @pointerdown="onSeekStart"
        @pointermove="onSeekMove"
        @pointerup="onSeekEnd"
        @pointercancel="onSeekEnd"
        @lostpointercapture="onSeekEnd"
        @keydown="onSeekKey"
      >
        <i class="video-node-track-fill" :style="{ width: `${progress * 100}%` }" />
        <i class="video-node-track-knob" :style="{ left: `${progress * 100}%` }" />
      </div>
      <span class="video-node-time">{{ formatVideoClock(duration) }}</span>
      <button type="button" class="video-node-button" :title="muted ? '打开声音' : '静音'" :aria-label="muted ? '打开声音' : '静音'" @click.stop="toggleMute">
        <VolumeX v-if="muted" :size="16" />
        <Volume2 v-else :size="16" />
      </button>
      <button
        type="button"
        class="video-node-button"
        :disabled="capturing"
        :title="capturing ? '正在截取…' : '截取当前画面，放到右侧的图片节点'"
        aria-label="截取当前画面"
        @click.stop="captureFrame"
      >
        <LoaderCircle v-if="capturing" :size="15" class="spin" />
        <Camera v-else :size="16" />
      </button>
    </div>
  </div>
</template>

<style scoped>
.video-node-controls {
  position: absolute;
  left: 0;
  right: 0;
  bottom: 0;
  z-index: 3;
  display: flex;
  align-items: center;
  gap: 4px;
  padding: 18px 8px 6px;
  color: var(--portal-on-media);
  background: linear-gradient(to top, var(--portal-on-media-bg), transparent);
  font-size: 11px;
  font-variant-numeric: tabular-nums;
  cursor: default;
  user-select: none;
}
.video-node-button {
  flex: none;
  width: 26px;
  height: 26px;
  display: grid;
  place-items: center;
  padding: 0;
  border: 0;
  border-radius: 6px;
  color: inherit;
  background: transparent;
  cursor: pointer;
}
.video-node-button:hover { background: var(--portal-on-media-line); }
.video-node-button:disabled { cursor: progress; }
.video-node-time { flex: none; min-width: 28px; text-align: center; }
/* 轨道本身是 16 高的透明点击区，中间画一条 3 高的线：线太细手点不中 */
.video-node-track { position: relative; flex: 1; min-width: 36px; height: 16px; margin: 0 4px; cursor: pointer; touch-action: none; }
.video-node-track::before,
.video-node-track-fill { content: ""; position: absolute; left: 0; top: 50%; height: 3px; border-radius: 2px; transform: translateY(-50%); }
.video-node-track::before { right: 0; background: var(--portal-on-media-line); }
.video-node-track-fill { background: var(--portal-on-media); }
.video-node-track-knob { position: absolute; top: 50%; width: 10px; height: 10px; border-radius: 50%; background: var(--portal-on-media); box-shadow: 0 1px 3px var(--cv-shade-30); transform: translate(-50%, -50%); }
.video-node-track:focus-visible { outline: 1px solid var(--portal-on-media); outline-offset: 2px; border-radius: 4px; }
</style>
