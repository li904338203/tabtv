<script setup lang="ts">
/**
 * 生成产出里的一段视频（design/03 R-5，按用户给的参考截图）。
 *
 * - 平时显示**封面帧**（Worker 截的 video_poster，十几 KB）；卡片按视频**真实比例**摆（宽高取自 `/media/{id}/info`，
 *   视频的宽高来自封面帧）。此前一律是 16:10 的空白横框，竖屏片子看不出长什么样。
 * - **鼠标悬浮**：静音播放，移开暂停并回到开头。整段视频**第一次悬浮才取**（30 秒的 480p 就有 17MB，列表里不预取），
 *   之后来回悬浮不再重取；取的时候显示转圈。
 * - 左下角常驻时长；悬浮时右下角出现白色胶囊「下载」「静音」（下载复用已取回的视频，不再下一遍）。点画面打开预览。
 * - 触屏没有悬浮：胶囊常驻，点画面打开预览播放。
 *
 * ⚠️ 受保护媒体一律 fetch → blob → objectURL（useAuthenticatedMedia），卸载时随作用域释放（CLAUDE.md §2.13）。
 */
import { Download, Film, LoaderCircle, Volume2, VolumeX } from "@lucide/vue";
import { ElMessage } from "element-plus";
import { mediaThumbUrl } from "~/utils/authorizedMedia";
import { extensionFor, safeFilename, saveBlob } from "~/utils/download";
import { outputVideoDuration, outputVideoTileSize } from "~/utils/outputVideo";

type MediaInfo = { posterMediaId?: string; width?: number; height?: number; durationMs?: number };

const props = defineProps<{ mediaId: string; label: string; filename: string }>();
const emit = defineEmits<{ open: [] }>();

const api = useApi();
const poster = useAuthenticatedMedia();
const clip = useAuthenticatedMedia();
const info = ref<MediaInfo>({});
/** 元信息没给宽高时，用封面 / 视频本身读到的尺寸兜底 —— 卡片比例不能一直停在默认的 16:9。 */
const natural = ref<{ width: number; height: number }>();
const hovering = ref(false);
const loading = ref(false);
const muted = ref(true);
const videoEl = ref<HTMLVideoElement>();

const size = computed(() => outputVideoTileSize(info.value.width || natural.value?.width, info.value.height || natural.value?.height));
const duration = computed(() => outputVideoDuration(info.value.durationMs));
const posterUrl = computed(() => poster.urls.value.poster);
const clipUrl = computed(() => clip.urls.value.clip);

watch(() => props.mediaId, async (id) => {
  info.value = {};
  natural.value = undefined;
  try {
    const got = await api<MediaInfo>(`/media/${id}/info`);
    if (props.mediaId !== id) return;
    info.value = got;
    if (got.posterMediaId) void poster.appendAll([{ key: "poster", url: mediaThumbUrl(got.posterMediaId, 640) }]);
  } catch {
    // 元信息取不到：照常显示占位，悬浮仍可播放、点开仍可预览。
  }
}, { immediate: true });

async function enter() {
  hovering.value = true;
  if (!clipUrl.value) {
    if (loading.value) return;
    loading.value = true;
    await clip.appendAll([{ key: "clip", url: `/media/${props.mediaId}` }]);
    loading.value = false;
    if (!clipUrl.value) return; // 取不到：留在封面上，点开预览会说明原因
  }
  await nextTick();
  if (hovering.value) void videoEl.value?.play().catch(() => {});
}

function leave() {
  hovering.value = false;
  const el = videoEl.value;
  if (!el) return;
  el.pause();
  el.currentTime = 0;
}

function toggleMute() {
  muted.value = !muted.value;
  const el = videoEl.value;
  if (!el) return;
  el.muted = muted.value;
  if (el.paused && hovering.value) void el.play().catch(() => {});
}

async function download() {
  try {
    const blob = clipUrl.value ? await (await fetch(clipUrl.value)).blob() : await clip.blob(`/media/${props.mediaId}`);
    saveBlob(blob, `${safeFilename(props.filename)}.${extensionFor(blob.type || "video/mp4")}`);
  } catch (cause) {
    ElMessage.error(cause instanceof Error ? cause.message : "下载没有成功");
  }
}

function onMeta(event: Event) {
  const el = event.target as HTMLVideoElement;
  if (el.videoWidth && el.videoHeight) natural.value = { width: el.videoWidth, height: el.videoHeight };
}

function onPosterLoad(event: Event) {
  const img = event.target as HTMLImageElement;
  if (!natural.value && img.naturalWidth && img.naturalHeight) natural.value = { width: img.naturalWidth, height: img.naturalHeight };
}
</script>

<template>
  <div
    class="output-video"
    :style="{ width: `${size.width}px`, height: `${size.height}px` }"
    @mouseenter="enter"
    @mouseleave="leave"
  >
    <video
      v-if="clipUrl"
      ref="videoEl"
      class="output-video-media"
      :src="clipUrl"
      :poster="posterUrl"
      :muted="muted"
      playsinline
      loop
      preload="auto"
      @loadedmetadata="onMeta"
    />
    <img v-else-if="posterUrl" class="output-video-media" :src="posterUrl" :alt="label" @load="onPosterLoad">
    <span v-else class="output-video-blank" aria-hidden="true"><Film :size="22" /></span>
    <button type="button" class="output-video-open" :aria-label="`预览：${label}`" title="点击预览" @click="emit('open')" />
    <span v-if="loading" class="output-video-loading" aria-hidden="true"><LoaderCircle :size="18" class="spin" /></span>
    <span v-if="duration" class="output-video-duration">{{ duration }}</span>
    <div class="output-video-tools">
      <button type="button" title="下载" aria-label="下载" @click="download"><Download :size="14" /></button>
      <button type="button" :title="muted ? '打开声音' : '静音'" :aria-label="muted ? '打开声音' : '静音'" @click="toggleMute">
        <VolumeX v-if="muted" :size="14" />
        <Volume2 v-else :size="14" />
      </button>
    </div>
  </div>
</template>

<style scoped>
/* 叠在画面上的控件用媒体控件色（两套主题同值：画面本身不随主题变，控件也不该跟着翻转）。 */
.output-video {
  position: relative;
  overflow: hidden;
  border: 1px solid var(--canvas-line, var(--portal-line));
  border-radius: 10px;
  background: var(--portal-media-letterbox);
}
.output-video-media { display: block; width: 100%; height: 100%; object-fit: cover; }
.output-video-blank {
  display: grid;
  place-items: center;
  width: 100%;
  height: 100%;
  color: var(--canvas-muted, var(--portal-muted));
  background: var(--cv-n12, var(--portal-surface-2));
}
.output-video-open { position: absolute; inset: 0; z-index: 1; border: 0; padding: 0; background: transparent; cursor: zoom-in; }
.output-video-loading {
  position: absolute;
  inset: 0;
  z-index: 2;
  display: grid;
  place-items: center;
  color: var(--portal-on-media);
  pointer-events: none;
}
.output-video-duration {
  position: absolute;
  left: 8px;
  bottom: 8px;
  z-index: 2;
  border-radius: 6px;
  padding: 1px 6px;
  color: var(--portal-on-media);
  background: var(--portal-on-media-bg);
  font-size: 11px;
  line-height: 18px;
  pointer-events: none;
}
.output-video-tools {
  position: absolute;
  right: 8px;
  bottom: 8px;
  z-index: 2;
  display: flex;
  gap: 2px;
  border-radius: 999px;
  padding: 2px 4px;
  background: var(--portal-on-media);
  box-shadow: 0 2px 10px var(--portal-shadow);
  opacity: 0;
  pointer-events: none;
  transition: opacity .15s ease;
}
.output-video-tools button {
  width: 26px;
  height: 26px;
  display: grid;
  place-items: center;
  border: 0;
  border-radius: 50%;
  padding: 0;
  color: var(--portal-media-letterbox);
  background: transparent;
  cursor: pointer;
}
.output-video-tools button:hover { opacity: .6; }
/* 下载 / 静音悬浮时才出现（键盘聚焦到卡片里也出现）；触屏没有悬浮，常驻。 */
.output-video:hover .output-video-tools,
.output-video:focus-within .output-video-tools { opacity: 1; pointer-events: auto; }
@media (hover: none) {
  .output-video-tools { opacity: 1; pointer-events: auto; }
}
</style>
