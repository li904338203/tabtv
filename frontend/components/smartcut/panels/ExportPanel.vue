<script setup lang="ts">
/**
 * 导出（SCT-24，dev/03 §5.4，SC-F-70~78）：顶栏「导出 ▾」展开的面板 —— 选规格、免费预检、导出（带进度、可取消）、
 * 完成后用签名链接下载、发布到画布、手动提交作品。
 *
 * - 先存再导出：服务端按**已保存**的时间线冻结快照，本地没存上的那一刀导不进成片（store.flush）；
 * - 一次点击一个 `Idempotency-Key`，失败后的「重试」复用同一个键（SCB-3）；
 * - 进度以 `render.update` 事件为主、轮询兜底（`smartcutPollDelayMs`），`smartcutMergeJob` 不让晚到的旧状态把终态打回去；
 *   轮询耗尽**不等于失败**（GAP-14），只说「仍在处理中」；
 * - 下载只取签名链接再跳转（`window.location.assign`），**不把成片读进内存**（几百 MB 会撑爆标签页，SCK-10）；
 * - 导出**不自动登记作品**（SCK-9）：「提交作品」是显式操作，成功后说「已提交，等待审核」，不说「已发布」。
 */
import { ElMessage } from "element-plus";
import { ChevronDown, Download, LoaderCircle, Send, Share2, Square } from "@lucide/vue";
import type { components } from "@tabtv/api-client";
import { SmartcutFlushError } from "~/stores/smartcut";
import {
  smartcutDownloadable,
  smartcutIdempotencyKey,
  smartcutIsTerminal,
  smartcutIssueText,
  smartcutMergeJob,
  smartcutPollDelayMs,
  smartcutPollExhaustedText,
  smartcutPrecheckSummary,
  smartcutProgressText,
  smartcutRenderBlocker,
  smartcutRetryAfterOf,
  smartcutRetryAfterText,
  smartcutWatermarkNote
} from "~/utils/smartcutRender";
import { smartcutFormatTime } from "~/utils/smartcutTimeline";

type RenderJob = components["schemas"]["RenderJob"];
type RenderPrecheck = components["schemas"]["RenderPrecheck"];
type RenderDownload = components["schemas"]["RenderDownload"];

const props = defineProps<{ editable: boolean; disabledReason: string; nodeId: string }>();
const emit = defineEmits<{ publish: [render: RenderJob]; locate: [clipId: string] }>();
const store = useSmartcutStore();
const auth = useAuthStore();
const realtime = useRealtimeStore();
const canvas = useCanvasStore();

const MAX_POLLS = 60;
const open = ref(false);
const rootEl = ref<HTMLElement | null>(null);
const nodeParams = computed(() => canvas.nodes.find(node => node.id === props.nodeId)?.data.params ?? {});
const resolution = ref<"720p" | "1080p">(nodeParams.value.resolution === "720p" ? "720p" : "1080p");
const fps = ref<24 | 25 | 30>([24, 25, 30].includes(store.timeline?.fps ?? 0) ? (store.timeline!.fps as 24 | 25 | 30) : 30);
const precheck = ref<RenderPrecheck | null>(null);
const job = ref<RenderJob | null>(null);
const busy = ref<"" | "precheck" | "export" | "cancel" | "download" | "submit">("");
const failure = ref("");
const retryAfter = ref(0);
const exhausted = ref(false);
const submitted = ref(false);
let idemKey = "";
let pollTimer: ReturnType<typeof setTimeout> | undefined;
let retryTimer: ReturnType<typeof setInterval> | undefined;
let polls = 0;

const running = computed(() => Boolean(job.value && !smartcutIsTerminal(job.value.status)));
/** 只读成员看得到面板、点不动导出与下载，并知道为什么（导出与下载都要 export 权限）。 */
const denied = computed(() => (store.readonly ? "需要导出权限：你在该工作区是只读成员" : ""));
const generatingClips = computed(() => (store.timeline?.tracks ?? []).flatMap(t => t.clips ?? []).filter(c => c.pendingGenerationId).length);
/** 还在生成的画面片段（第几段）：导出面板逐段说出来，并能点击定位到那一段（SC-F-138、SC-AC-59）。 */
const generatingList = computed(() => {
  const video = store.timeline?.tracks.find(t => t.kind === "video")?.clips ?? [];
  return video.map((c, i) => ({ clipId: c.id, no: i + 1 })).filter((_, i) => Boolean(video[i]!.pendingGenerationId));
});
const blocker = computed(() => smartcutRenderBlocker(precheck.value, null, running.value, denied.value, {
  retryAfterSeconds: retryAfter.value || undefined,
  generatingClips: generatingClips.value
}));
const progressText = computed(() => (job.value ? smartcutProgressText(job.value) : ""));
const watermark = computed(() => smartcutWatermarkNote(job.value, Boolean(auth.user?.isAiWatermarkOff)));
const warnings = computed(() => job.value?.selfCheck?.warnings ?? []);

function causeText(cause: unknown, fallback: string): string {
  if (cause instanceof SmartcutFlushError) return cause.message;
  const message = (cause as { message?: unknown })?.message;
  return typeof message === "string" && message && message !== "请求失败" ? message : fallback;
}

/** 规格变了：之前的预检与幂等键都作废（同一把键对应的是另一份规格）。 */
watch([resolution, fps], () => {
  precheck.value = null;
  idemKey = "";
});
/** 时间线改了：预检作废（它是按改之前的样子算的）。 */
watch(() => store.serverVersion, () => { if (!running.value) precheck.value = null; });

async function runPrecheck() {
  if (busy.value || !store.loaded) return;
  busy.value = "precheck";
  failure.value = "";
  try {
    // 预检按服务端的时间线算：本地没存上的改动它看不见
    if (!store.readonly) await store.flush();
    precheck.value = await useApi()<RenderPrecheck>(`/timelines/${store.timelineId}/renders`, {
      method: "POST", body: { dryRun: true, resolution: resolution.value, fps: fps.value }
    });
  } catch (cause) {
    failure.value = causeText(cause, "预检失败，请检查网络后重试");
  } finally {
    busy.value = "";
  }
}

async function startExport() {
  if (busy.value || blocker.value) return;
  busy.value = "export";
  failure.value = "";
  exhausted.value = false;
  submitted.value = false;
  try {
    await store.flush();
    idemKey ||= smartcutIdempotencyKey();
    const res = await useApi()<RenderPrecheck & { render?: RenderJob }>(`/timelines/${store.timelineId}/renders`, {
      method: "POST", headers: { "Idempotency-Key": idemKey }, body: { resolution: resolution.value, fps: fps.value }
    });
    if (res.render) {
      job.value = smartcutMergeJob(job.value, res.render);
      idemKey = "";
      track();
    } else {
      precheck.value = res;
    }
  } catch (cause) {
    const secs = smartcutRetryAfterOf(cause);
    if (secs) startRetryCountdown(secs);
    failure.value = secs ? `导出太频繁，${smartcutRetryAfterText(secs)}` : causeText(cause, "导出没有发起，请重试");
  } finally {
    busy.value = "";
  }
}

function startRetryCountdown(seconds: number) {
  clearInterval(retryTimer);
  retryAfter.value = seconds;
  retryTimer = setInterval(() => {
    retryAfter.value = Math.max(0, retryAfter.value - 1);
    if (!retryAfter.value) clearInterval(retryTimer);
  }, 1000);
}

// ---------------------------------------------------------------- 进度：事件为主、轮询兜底

let offRender: (() => void) | undefined;
function track() {
  clearTimeout(pollTimer);
  polls = 0;
  offRender ??= realtime.on("render.update", (frame) => {
    const current = job.value;
    if (!current || String(frame.renderJobId ?? "") !== current.id) return;
    const output = frame.output as { mediaId?: string } | null | undefined;
    const err = frame.error as { message?: string } | null | undefined;
    job.value = smartcutMergeJob(current, {
      ...current,
      status: frame.status as RenderJob["status"],
      progress: Number(frame.progress) || 0,
      ...(typeof frame.stage === "string" ? { stage: frame.stage as RenderJob["stage"] } : {}),
      ...(output?.mediaId ? { outputMediaId: output.mediaId } : {}),
      ...(err?.message ? { errorMessage: err.message } : {})
    });
    // 到了终态再取一次完整的任务：自检结果、水印、成片地址只在任务详情里
    if (job.value && smartcutIsTerminal(job.value.status)) void poll(true);
  });
  schedulePoll();
}

function schedulePoll() {
  clearTimeout(pollTimer);
  if (!job.value || smartcutIsTerminal(job.value.status)) return;
  if (polls >= MAX_POLLS) {
    exhausted.value = true;
    return;
  }
  pollTimer = setTimeout(() => { void poll(false); }, smartcutPollDelayMs(polls));
  polls += 1;
}

async function poll(final: boolean) {
  const current = job.value;
  if (!current) return;
  try {
    const fresh = await useApi()<RenderJob>(`/renders/${current.id}`);
    job.value = final && smartcutIsTerminal(fresh.status) ? fresh : smartcutMergeJob(job.value, fresh);
    exhausted.value = false;
  } catch {
    // 一次网络抖动不清空眼前的进度
  }
  if (!final) schedulePoll();
}

async function cancel() {
  const current = job.value;
  if (!current || busy.value) return;
  busy.value = "cancel";
  try {
    const res = await useApi()<RenderJob>(`/renders/${current.id}/cancel`, { method: "POST" });
    job.value = smartcutMergeJob(job.value, res);
  } catch (cause) {
    ElMessage.warning(causeText(cause, "取消失败，请稍后再试"));
  } finally {
    busy.value = "";
  }
}

// ---------------------------------------------------------------- 完成之后：下载、发布、提交作品

async function download() {
  const current = job.value;
  if (!current || !smartcutDownloadable(current) || busy.value || denied.value) return;
  busy.value = "download";
  try {
    // 签名链接 5 分钟有效：每次点都现取一个，不缓存
    const res = await useApi()<RenderDownload>(`/renders/${current.id}/download`);
    window.location.assign(res.url);
  } catch (cause) {
    ElMessage.warning(causeText(cause, "下载链接取不到，请稍后再试"));
  } finally {
    busy.value = "";
  }
}

function publish() {
  if (job.value && smartcutDownloadable(job.value)) emit("publish", job.value);
}

async function submitWork() {
  const current = job.value;
  if (!current || busy.value || store.readonly) return;
  busy.value = "submit";
  try {
    await useApi()(`/works`, { method: "POST", body: { sourceType: "render", sourceId: current.id, title: canvas.nodes.find(n => n.id === props.nodeId)?.data.title ?? store.timeline?.name } });
    submitted.value = true;
    ElMessage.success("已提交，等待审核");
  } catch (cause) {
    ElMessage.warning(causeText(cause, "提交失败，请稍后再试"));
  } finally {
    busy.value = "";
  }
}

// ---------------------------------------------------------------- 打开与关闭

/** 第一次展开时取最近一次导出：还在跑的接着跟，已完成的直接给下载等入口。 */
let loadedLatest = false;
async function toggle() {
  open.value = !open.value;
  if (!open.value || loadedLatest || !store.timelineId) return;
  loadedLatest = true;
  try {
    const list = await useApi()<RenderJob[]>(`/timelines/${store.timelineId}/renders?limit=1`);
    const latest = list[0];
    if (latest && !job.value) {
      job.value = latest;
      if (!smartcutIsTerminal(latest.status)) track();
    }
  } catch {
    // 取不到历史不打扰：面板照常能预检、导出
  }
}

function onDocPointer(event: PointerEvent) {
  if (open.value && rootEl.value && event.target instanceof Node && !rootEl.value.contains(event.target)) open.value = false;
}
onMounted(() => document.addEventListener("pointerdown", onDocPointer, true));
onBeforeUnmount(() => {
  document.removeEventListener("pointerdown", onDocPointer, true);
  clearTimeout(pollTimer);
  clearInterval(retryTimer);
  offRender?.();
});

const durationText = computed(() => smartcutFormatTime(precheck.value?.durationMs ?? store.timeline?.effectiveMs ?? 0));
</script>

<template>
  <div ref="rootEl" class="sc-export" data-testid="smartcut-export">
    <button type="button" class="sc-btn" :class="{ active: open || running }" :aria-expanded="open" aria-haspopup="dialog" aria-label="导出" title="导出成片（免费）" @click="toggle">
      <LoaderCircle v-if="running" :size="14" class="sc-spin" /><ChevronDown v-else :size="14" />{{ running ? progressText : "导出" }}
    </button>
    <section v-if="open" class="sc-export-panel" role="dialog" aria-label="导出成片">
      <div class="sc-section">
        <h3>规格</h3>
        <div class="sc-field">
          <span>分辨率</span>
          <div class="sc-seg" role="radiogroup" aria-label="分辨率">
            <button v-for="r in ['720p', '1080p'] as const" :key="r" type="button" role="radio" class="sc-btn" :class="{ active: resolution === r }" :aria-checked="resolution === r" :disabled="running" @click="resolution = r">{{ r }}</button>
          </div>
        </div>
        <div class="sc-field">
          <span>帧率</span>
          <div class="sc-seg" role="radiogroup" aria-label="帧率">
            <button v-for="f in [24, 25, 30] as const" :key="f" type="button" role="radio" class="sc-btn" :class="{ active: fps === f }" :aria-checked="fps === f" :disabled="running" @click="fps = f">{{ f }}fps</button>
          </div>
        </div>
      </div>

      <div class="sc-section">
        <h3>预检（免费）</h3>
        <button type="button" class="sc-btn" :disabled="Boolean(busy) || running" data-testid="smartcut-precheck" @click="runPrecheck">
          <LoaderCircle v-if="busy === 'precheck'" :size="13" class="sc-spin" />{{ precheck ? "重新预检" : "预检" }}
        </button>
        <p v-if="precheck" class="sc-note" data-testid="smartcut-precheck-summary">{{ smartcutPrecheckSummary(precheck, durationText) }}</p>
        <ul v-if="precheck?.issues.length" class="sc-export-issues" role="note" data-testid="smartcut-precheck-issues">
          <li v-for="(issue, i) in precheck.issues" :key="i">{{ smartcutIssueText(issue) }}</li>
        </ul>
      </div>

      <div class="sc-section">
        <h3>导出（免费）</h3>
        <button
          type="button"
          class="sc-btn primary"
          :disabled="Boolean(blocker) || Boolean(busy)"
          :title="blocker || '导出成片'"
          :aria-label="blocker ? `导出：${blocker}` : '导出成片'"
          data-testid="smartcut-start-export"
          @click="startExport"
        >
          <LoaderCircle v-if="busy === 'export'" :size="13" class="sc-spin" />导出
        </button>
        <p v-if="blocker && !running" class="sc-note">{{ blocker }}</p>
        <ul v-if="generatingList.length && !running" class="sc-export-issues" role="note" data-testid="smartcut-export-generating">
          <li v-for="g in generatingList" :key="g.clipId">
            第 {{ g.no }} 段还在生成
            <button type="button" class="sc-btn" data-testid="smartcut-export-locate" @click="open = false; emit('locate', g.clipId)">定位</button>
          </li>
        </ul>
        <p v-if="failure" class="sc-error" role="alert">{{ failure }}</p>
      </div>

      <div v-if="job" class="sc-section" data-testid="smartcut-render-job" :data-status="job.status">
        <h3>这次导出</h3>
        <p class="sc-note">{{ progressText }}<template v-if="job.spec"> · {{ job.spec.resolution ?? "" }} {{ job.spec.fps ? `${job.spec.fps}fps` : "" }}</template></p>
        <div v-if="running" class="sc-export-bar" aria-hidden="true"><span :style="{ width: `${job.progress}%` }" /></div>
        <p v-if="exhausted" class="sc-note">{{ smartcutPollExhaustedText }}</p>
        <button v-if="running" type="button" class="sc-btn danger" :disabled="busy === 'cancel'" data-testid="smartcut-cancel-export" @click="cancel"><Square :size="13" />取消导出</button>
        <template v-if="smartcutDownloadable(job)">
          <ul v-if="warnings.length" class="sc-export-issues warn" role="note"><li v-for="w in warnings" :key="w">{{ w }}</li></ul>
          <p v-if="watermark" class="sc-note">{{ watermark }}</p>
          <div class="sc-seg">
            <button type="button" class="sc-btn primary" :disabled="Boolean(denied) || busy === 'download'" :title="denied || '下载成片（链接 5 分钟有效）'" data-testid="smartcut-download" @click="download"><Download :size="13" />下载</button>
            <button type="button" class="sc-btn" :disabled="Boolean(denied)" :title="denied || '在画布上放一个成片视频节点'" data-testid="smartcut-publish" @click="publish"><Share2 :size="13" />发布到画布</button>
            <button type="button" class="sc-btn" :disabled="store.readonly || submitted || busy === 'submit'" :title="submitted ? '已提交，等待审核' : '提交到作品库（需要审核）'" data-testid="smartcut-submit-work" @click="submitWork"><Send :size="13" />{{ submitted ? "已提交" : "提交作品" }}</button>
          </div>
        </template>
      </div>
    </section>
  </div>
</template>

<style scoped>
.sc-export { position: relative; }
.sc-export-panel {
  position: absolute;
  top: calc(100% + 6px);
  right: 0;
  z-index: 50;
  display: grid;
  gap: 14px;
  width: 340px;
  padding: 12px;
  border: 1px solid var(--portal-line-strong);
  border-radius: 10px;
  color: var(--portal-text);
  background: var(--portal-elevated);
  box-shadow: 0 10px 30px var(--portal-shadow);
  font-size: 12px;
}
.sc-export-issues { margin: 0; padding-left: 16px; color: var(--portal-danger); line-height: 1.6; }
.sc-export-issues.warn { color: var(--portal-gold); }
.sc-export-bar { height: 4px; overflow: hidden; border-radius: 999px; background: var(--portal-surface-2); }
.sc-export-bar span { display: block; height: 100%; background: var(--portal-cyan); transition: width 0.3s; }
@media (prefers-reduced-motion: reduce) { .sc-export-bar span { transition: none; } }
</style>
