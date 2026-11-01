<script setup lang="ts">
/**
 * 智能剪辑的节点卡（SCT-23，SCD-47）：**只读**。封面、迷你时间轴、状态与两个角标、按需播放成片、「打开智能剪辑」。
 *
 * - 数据只从摘要来（`useSmartcuts`：封面、strip、最近一次导出、进行中的 AI 剪辑、两个过期标记），不取完整时间线；
 * - 封面是受保护媒体：经 useAuthenticatedMedia 取成 blob（卸载时释放），`<img src>` 直接引用一律 401；
 * - 播放点了才加载：有成片时取成片的预览版（短边 480），整页只有一个活的播放器（开剪辑台或开另一个节点卡的播放时停掉）；
 * - 迷你时间轴上点一段只把那一段的封面帧换到卡片上看（不播放）。
 * 卡片上没有编辑操作（SCK-17）：双击与「打开智能剪辑」都发 `open`。
 */
import { AudioWaveform, Clapperboard, LayoutPanelTop, Mic, Pause, Play, Scissors, Sparkles } from "@lucide/vue";
import type { JsonObject } from "~/types/canvas";
import { directorCanEdit } from "~/utils/directorRole";
import { smartcutClaimPlayer } from "~/smartcut/player";
import { smartcutMediaUrl } from "~/utils/smartcutPublish";
import { smartcutNodeState } from "~/utils/smartcutStale";
import { smartcutFormatTime } from "~/utils/smartcutTimeline";

const props = defineProps<{ nodeId: string; params: JsonObject; title: string }>();
const emit = defineEmits<{ open: [] }>();

const smartcuts = useSmartcuts();
const canvas = useCanvasStore();
const summary = computed(() => smartcuts.get(props.nodeId));
const nodeState = computed(() => smartcutNodeState(summary.value));
const covers = useAuthenticatedMedia();
const workspace = useWorkspaceStore();
/** AI 关着时（这一版只保留手动剪辑）不列四种剪辑方式：剪辑方式只对 AI 剪辑有意义。 */
const features = useSmartcutFeatures();

/**
 * 空节点（没有素材、没剪过、没导出过）：像脚本节点那样给一个引导 —— 说清要先连素材，并列出四种剪辑方式，
 * 点一下只是把「剪辑方式」预选好（写进节点参数，下方指令面板同步），不发请求、不打开剪辑台。
 */
const isEmpty = computed(() => nodeState.value.kind === "empty" && !nodeState.value.badges.length);
const MODE_ENTRIES = [
  { value: "narr", label: "解说成片", icon: AudioWaveform },
  { value: "board", label: "分镜成片", icon: LayoutPanelTop },
  { value: "talk", label: "口播精剪", icon: Mic },
  { value: "mix", label: "素材混剪", icon: Sparkles }
] as const;
const currentMode = computed(() => String(props.params.smartcut_mode ?? "mix"));
const canEdit = computed(() => directorCanEdit(workspace.current?.role));
function chooseMode(value: string) {
  if (!canEdit.value || value === currentMode.value) return;
  canvas.markTouched(props.nodeId);
  canvas.updateParams(props.nodeId, { ...props.params, smartcut_mode: value } as JsonObject);
}

/** 迷你时间轴上点中的那一段（只换封面帧，不播放）。 */
const peekIndex = ref<number | null>(null);
/**
 * 卡片上显示什么画面：点中的那一段 → 成片封面 → 时间线封面。都没有、而第一段是视频时，**直接显示那段视频**
 * （`frameVideoId`）：上传的视频不截封面帧（只有生成产出与成片截），不这样的话连上视频之后卡片上仍然只有一把剪刀。
 */
const display = computed<{ cover: string; video: string }>(() => {
  const s = summary.value;
  if (!s) return { cover: "", video: "" };
  const peek = peekIndex.value !== null ? s.strip[peekIndex.value] : undefined;
  if (peek) {
    if (peek.posterMediaId) return { cover: peek.posterMediaId, video: "" };
    if (peek.kind === "video" && peek.mediaId) return { cover: "", video: peek.mediaId };
  }
  const cover = s.latestRender?.outputPosterId ?? s.coverMediaId ?? "";
  if (cover) return { cover, video: "" };
  return { cover: "", video: s.strip.find(item => item.kind === "video" && item.mediaId)?.mediaId ?? "" };
});
const coverMediaId = computed(() => display.value.cover);
watch(coverMediaId, (id) => {
  if (id && !covers.urls.value[id]) void covers.appendAll([{ key: id, url: `${smartcutMediaUrl(id)}?w=640` }]);
}, { immediate: true });

// ---------------------------------------------------------------- 没有封面帧时直接显示视频

/** 原片超过它就不整段取回来（只为卡片上的一帧画面不值），等预览版做好。 */
const FRAME_ORIGINAL_MAX_BYTES = 30 * 1024 * 1024;
const frameSrc = ref("");
const frameEl = ref<HTMLVideoElement | null>(null);
const framePending = ref(false);
let frameSeq = 0;
let frameRetry: ReturnType<typeof setTimeout> | undefined;
function clearFrame() {
  clearTimeout(frameRetry);
  frameRetry = undefined;
  if (frameSrc.value) URL.revokeObjectURL(frameSrc.value);
  frameSrc.value = "";
  framePending.value = false;
}
/**
 * 取那段视频的预览版（短边 480，SCB 的预览素材）；还在做时隔几秒再问（最多约 3 分钟）。没有预览版、原片又不大时取原片。
 * 取不到不打扰：卡片退回剪刀占位。
 */
async function loadFrame(id: string, attempt = 0) {
  const seq = ++frameSeq;
  framePending.value = true;
  try {
    const assets = await useApi()<{ status?: Record<string, string>; preview?: { mediaId?: string } | null }>(`/media/${id}/preview-assets`).catch(() => null);
    if (seq !== frameSeq) return;
    let source = assets?.preview?.mediaId ?? "";
    if (!source) {
      const state = assets?.status?.preview_proxy;
      if ((state === "queued" || state === "running") && attempt < 20) {
        frameRetry = setTimeout(() => void loadFrame(id, attempt + 1), 9000);
        return;
      }
      const info = await useApi()<{ sizeBytes?: number }>(`/media/${id}/info`).catch(() => null);
      if (seq !== frameSeq) return;
      if (!info?.sizeBytes || info.sizeBytes > FRAME_ORIGINAL_MAX_BYTES) {
        framePending.value = false;
        return;
      }
      source = id;
    }
    const blob = await covers.blob(smartcutMediaUrl(source));
    if (seq !== frameSeq) return;
    frameSrc.value = URL.createObjectURL(blob);
    framePending.value = false;
  } catch {
    if (seq === frameSeq) framePending.value = false;
  }
}
watch(() => display.value.video, (id) => {
  frameSeq++;
  clearFrame();
  if (id) void loadFrame(id);
}, { immediate: true });
const framePlaying = ref(false);
/** 整页只有一个活的播放器（开剪辑台、或播另一个节点卡时停掉这里）。 */
let releaseFrame: (() => void) | null = null;
function toggleFrame() {
  const el = frameEl.value;
  if (!el) return;
  if (el.paused) {
    releaseFrame?.();
    releaseFrame = smartcutClaimPlayer(() => {
      el.pause();
      framePlaying.value = false;
    });
    el.muted = false;
    void el.play().catch(() => undefined);
    framePlaying.value = true;
  } else {
    el.pause();
    framePlaying.value = false;
    releaseFrame?.();
    releaseFrame = null;
  }
}
const coverSrc = computed(() => (coverMediaId.value ? covers.urls.value[coverMediaId.value] ?? "" : ""));

const totalMs = computed(() => Math.max(1, summary.value?.effectiveMs ?? 0));
const lanes = computed(() => {
  const strip = summary.value?.strip ?? [];
  const pct = (ms: number) => `${(ms / totalMs.value) * 100}%`;
  const item = (s: (typeof strip)[number], index: number) => ({ index, kind: s.kind, left: pct(s.startMs), width: pct(Math.max(s.durationMs, totalMs.value / 200)) });
  return {
    picture: strip.map(item).filter(s => s.kind !== "audio"),
    audio: strip.map(item).filter(s => s.kind === "audio")
  };
});

// ---------------------------------------------------------------- 播放成片（按需）

const outputMediaId = computed(() => (summary.value?.latestRender?.status === "success" ? summary.value.latestRender.outputMediaId ?? "" : ""));
const videoEl = ref<HTMLVideoElement | null>(null);
const videoSrc = ref("");
const playing = ref(false);
const loadingVideo = ref(false);
const playError = ref("");
let release: (() => void) | null = null;

function stopPlayback() {
  videoEl.value?.pause();
  playing.value = false;
  if (videoSrc.value) URL.revokeObjectURL(videoSrc.value);
  videoSrc.value = "";
  release?.();
  release = null;
}

async function togglePlay() {
  if (playing.value) {
    stopPlayback();
    return;
  }
  const id = outputMediaId.value;
  if (!id || loadingVideo.value) return;
  release = smartcutClaimPlayer(stopPlayback);
  loadingVideo.value = true;
  playError.value = "";
  try {
    const assets = await useApi()<{ preview?: { mediaId?: string } | null }>(`/media/${id}/preview-assets`).catch(() => null);
    // 预览版没好时退回成片本身（成片是我们自己导出的，最大 1080p，可以接受）
    const blob = await covers.blob(smartcutMediaUrl(assets?.preview?.mediaId ?? id));
    if (!release) return;
    videoSrc.value = URL.createObjectURL(blob);
    playing.value = true;
    await nextTick();
    await videoEl.value?.play().catch(() => undefined);
  } catch {
    playError.value = "成片取不到，请稍后再试";
    stopPlayback();
  } finally {
    loadingVideo.value = false;
  }
}

onBeforeUnmount(() => {
  stopPlayback();
  frameSeq++;
  clearFrame();
  releaseFrame?.();
});

// ---------------------------------------------------------------- 同步时机（SCD-49）

/** 卡片挂载时按已保存的画布同步一次（没剪过时把新素材铺上轨道）；失败不打扰，打开剪辑台时还会再同步。 */
onMounted(async () => {
  await smartcuts.load(canvas.projectId);
  if (summary.value && canvas.projectId) void smartcuts.sync(canvas.projectId, props.nodeId, "autofill").catch(() => undefined);
});
</script>

<template>
  <!-- ⚠️ `.stop`：双击冒泡到画布的话 Vue Flow 会顺手把视口放大一级 -->
  <div v-if="isEmpty" class="smartcut-node-body smartcut-node-empty" title="双击打开智能剪辑" data-testid="smartcut-node-body" @dblclick.stop="emit('open')">
    <Scissors :size="40" stroke-width="1.35" class="smartcut-empty-mark" aria-hidden="true" />
    <p class="smartcut-empty-text">空空如也，请先连接视频、图片或音频节点</p>
    <div v-if="features.ai" class="smartcut-empty-modes nodrag nopan" @pointerdown.stop @mousedown.stop>
      <span>尝试：</span>
      <button
        v-for="entry in MODE_ENTRIES"
        :key="entry.value"
        type="button"
        class="nodrag nopan"
        :class="{ active: entry.value === currentMode }"
        :disabled="!canEdit"
        :aria-pressed="entry.value === currentMode"
        :title="canEdit ? `剪辑方式选「${entry.label}」` : '需要编辑权限：你在该工作区是只读成员'"
        @click.stop="chooseMode(entry.value)"
      ><component :is="entry.icon" :size="16" />{{ entry.label }}</button>
    </div>
  </div>
  <div v-else class="smartcut-node-body" title="双击打开智能剪辑" data-testid="smartcut-node-body" @dblclick.stop="emit('open')">
    <div class="smartcut-node-cover">
      <video v-if="videoSrc" ref="videoEl" :src="videoSrc" playsinline class="smartcut-node-video" @ended="stopPlayback" />
      <img v-else-if="coverSrc" :src="coverSrc" :alt="`${title} 的封面`" :draggable="false">
      <!-- 没有封面帧（上传的视频）：直接显示那段视频的第一帧，点播放按钮播放 -->
      <video
        v-else-if="frameSrc"
        ref="frameEl"
        :src="`${frameSrc}#t=0.1`"
        muted
        playsinline
        preload="metadata"
        class="smartcut-node-video"
        data-testid="smartcut-node-frame"
        @ended="framePlaying = false"
        @pause="framePlaying = false"
      />
      <div v-else class="smartcut-node-placeholder" aria-hidden="true">
        <Scissors :size="34" stroke-width="1.3" /><small v-if="framePending">画面准备中…</small>
      </div>
      <button
        v-if="outputMediaId"
        type="button"
        class="smartcut-node-play nodrag"
        :disabled="loadingVideo"
        :title="playing ? '停止播放' : '播放成片'"
        :aria-label="playing ? '停止播放' : '播放成片'"
        @mousedown.stop
        @click.stop="togglePlay"
      >
        <Pause v-if="playing" :size="16" /><Play v-else :size="16" fill="currentColor" />
      </button>
      <button
        v-if="!outputMediaId && frameSrc && !videoSrc"
        type="button"
        class="smartcut-node-play nodrag"
        :title="framePlaying ? '暂停' : '播放素材'"
        :aria-label="framePlaying ? '暂停' : '播放素材'"
        @mousedown.stop
        @click.stop="toggleFrame"
      >
        <Pause v-if="framePlaying" :size="16" /><Play v-else :size="16" fill="currentColor" />
      </button>
      <button type="button" class="smartcut-node-open nodrag" data-testid="smartcut-open" @mousedown.stop @click.stop="emit('open')">
        <Clapperboard :size="15" />打开智能剪辑
      </button>
      <!-- 状态与时长浮在画面左上，迷你时间轴贴着画面底边：卡片下方不再另起两行（用户反馈 2026-10-29） -->
      <div v-if="summary && summary.strip.length > 1" class="smartcut-node-strip" role="img" :aria-label="`迷你时间轴：${summary.strip.length} 段`">
        <div class="lane">
          <button
            v-for="s in lanes.picture"
            :key="`p-${s.index}`"
            type="button"
            class="seg nodrag"
            :class="s.kind"
            :style="{ left: s.left, width: s.width }"
            :title="`第 ${s.index + 1} 段（点一下在卡片上看它的画面）`"
            :aria-label="`第 ${s.index + 1} 段`"
            @mousedown.stop
            @click.stop="peekIndex = peekIndex === s.index ? null : s.index"
          />
        </div>
        <div v-if="lanes.audio.length" class="lane audio">
          <span v-for="s in lanes.audio" :key="`a-${s.index}`" class="seg audio" :style="{ left: s.left, width: s.width }" />
        </div>
      </div>
      <footer class="smartcut-node-meta" aria-live="polite">
        <span class="state" :class="`state-${nodeState.kind}`" data-testid="smartcut-node-state">{{ nodeState.label }}</span>
        <span v-if="summary?.effectiveMs" class="duration">{{ smartcutFormatTime(summary.effectiveMs) }}</span>
        <span v-for="badge in nodeState.badges" :key="badge.key" class="badge" :data-badge="badge.key">{{ badge.label }}</span>
        <span v-if="playError" class="badge">{{ playError }}</span>
      </footer>
    </div>
    <div v-if="nodeState.kind === 'exporting'" class="smartcut-node-progress" :style="{ width: `${nodeState.progress ?? 0}%` }" aria-hidden="true" />
    <!-- AI 剪辑中：阶段写在状态里（同步素材 / 理解素材 / 规划 / 装配），这条只表示「在动」，不冒充进度 -->
    <div v-else-if="nodeState.kind === 'ai'" class="smartcut-node-progress ai" aria-hidden="true" />
  </div>
</template>

<style scoped>
.smartcut-node-body { position: relative; display: flex; flex-direction: column; gap: 6px; height: 100%; padding: 8px; }
.smartcut-node-empty { min-height: 300px; align-items: center; justify-content: center; gap: 14px; padding: 28px 32px; color: var(--cv-n63, var(--canvas-muted)); }
.smartcut-empty-mark { color: var(--cv-n74, var(--canvas-muted)); }
.smartcut-empty-text { margin: 0; font-size: 12px; text-align: center; }
.smartcut-empty-modes { align-self: stretch; display: grid; gap: 2px; margin-top: 6px; font-size: 11px; }
.smartcut-empty-modes > span { margin-bottom: 4px; color: var(--cv-n63, var(--canvas-muted)); }
.smartcut-empty-modes button {
  min-height: 32px; display: flex; align-items: center; gap: 9px; border: 0; border-radius: 7px; padding: 0 12px;
  color: var(--cv-n87, var(--canvas-text)); background: transparent; font-size: 12px; text-align: left; cursor: pointer;
}
.smartcut-empty-modes button svg { color: var(--cv-n74, var(--canvas-muted)); }
.smartcut-empty-modes button:hover:not(:disabled), .smartcut-empty-modes button:focus-visible { background: var(--cv-n22, var(--canvas-line)); outline: none; }
.smartcut-empty-modes button.active { color: var(--cv-n96, var(--canvas-text)); background: var(--cv-n22, var(--canvas-line)); }
.smartcut-empty-modes button.active svg { color: var(--canvas-cyan); }
.smartcut-empty-modes button:disabled { cursor: not-allowed; opacity: 0.6; }
.smartcut-node-cover { position: relative; flex: 1; min-height: 160px; overflow: hidden; border-radius: 8px; background: var(--cv-n10, var(--canvas-bg)); }
.smartcut-node-cover img, .smartcut-node-video { width: 100%; height: 100%; object-fit: contain; background: #000; }
.smartcut-node-placeholder { display: grid; place-items: center; align-content: center; gap: 6px; height: 100%; color: var(--canvas-muted); }
.smartcut-node-placeholder small { font-size: 11px; }
.smartcut-node-play {
  position: absolute; z-index: 2; left: 10px; bottom: 14px; display: grid; place-items: center; width: 30px; height: 30px;
  border: 0; border-radius: 50%; color: var(--portal-on-media); background: var(--portal-on-media-bg); cursor: pointer;
}
.smartcut-node-open {
  position: absolute; z-index: 2; right: 10px; bottom: 14px; display: inline-flex; align-items: center; gap: 4px; height: 28px; padding: 0 10px;
  border: 1px solid var(--canvas-line); border-radius: 6px; color: var(--cv-n90, var(--canvas-text)); background: var(--canvas-panel, var(--portal-elevated)); font-size: 12px; cursor: pointer;
}
/* 画面底部一层渐变，按钮与时间轴压在上面也看得清 */
.smartcut-node-cover::after {
  content: ""; position: absolute; inset: auto 0 0; height: 64px; pointer-events: none;
  background: linear-gradient(to top, rgba(0, 0, 0, .45), rgba(0, 0, 0, 0));
}
/* 状态与时长：浮在画面左上的半透明胶囊 */
.smartcut-node-meta {
  position: absolute; z-index: 2; top: 10px; left: 10px; right: 72px; display: flex; flex-wrap: wrap; align-items: center; gap: 4px;
  pointer-events: none;
}
.smartcut-node-meta > span {
  display: inline-flex; align-items: center; gap: 5px; height: 20px; padding: 0 8px; border-radius: 999px;
  color: #fff; background: rgba(15, 16, 18, .58); backdrop-filter: blur(6px); -webkit-backdrop-filter: blur(6px);
  font-size: 11px; line-height: 1; white-space: nowrap;
}
.smartcut-node-meta .state::before { content: ""; width: 6px; height: 6px; border-radius: 50%; background: #9aa3ad; }
.smartcut-node-meta .state-laid::before, .smartcut-node-meta .state-edited::before { background: #5ec8d8; }
.smartcut-node-meta .state-exported::before { background: #4cc38a; }
.smartcut-node-meta .state-exporting::before, .smartcut-node-meta .state-ai::before { background: #5ec8d8; animation: smartcut-node-pulse 1.2s ease-in-out infinite; }
.smartcut-node-meta .state-failed::before { background: #f06a6a; }
.smartcut-node-meta .duration { font-variant-numeric: tabular-nums; }
.smartcut-node-meta .badge { color: #3b2a05; background: rgba(245, 190, 80, .92); }
/* 迷你时间轴：贴着画面底边的细条，悬停时加粗方便点 */
/* 平时不显示（只有一段时就是一整条线，没有信息量还显得多余，用户反馈 2026-10-30）；鼠标移到画面上才淡入，方便点某一段看画面 */
.smartcut-node-strip { position: absolute; z-index: 1; left: 0; right: 0; bottom: 0; display: grid; gap: 1px; opacity: 0; transition: opacity 150ms ease; }
.smartcut-node-cover:hover .smartcut-node-strip, .smartcut-node-strip:focus-within { opacity: 1; }
.smartcut-node-strip .lane { position: relative; height: 4px; background: rgba(255, 255, 255, .16); transition: height 120ms ease; }
.smartcut-node-strip .lane.audio { height: 2px; }
.smartcut-node-strip:hover .lane { height: 8px; }
.smartcut-node-strip:hover .lane.audio { height: 3px; }
.smartcut-node-strip .seg { position: absolute; top: 0; bottom: 0; padding: 0; border: 0; border-right: 1px solid rgba(0, 0, 0, .5); background: rgba(94, 200, 216, .9); cursor: pointer; }
.smartcut-node-strip .seg:hover { background: #8fe3ef; }
.smartcut-node-strip .seg.image { background: rgba(232, 184, 76, .9); }
.smartcut-node-strip .seg.audio { background: rgba(76, 195, 138, .85); cursor: default; }
@keyframes smartcut-node-pulse { 50% { opacity: .35; } }
.smartcut-node-progress { position: absolute; left: 0; bottom: 0; height: 2px; background: var(--canvas-cyan); }
.smartcut-node-progress.ai { width: 30%; animation: smartcut-node-ai 1.4s ease-in-out infinite; }
@keyframes smartcut-node-ai { 0% { left: 0; } 50% { left: 70%; } 100% { left: 0; } }
@media (prefers-reduced-motion: reduce) {
  .smartcut-node-progress.ai { left: 0; width: 100%; opacity: 0.6; animation: none; }
  .smartcut-node-meta .state::before { animation: none; }
}
</style>
