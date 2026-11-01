<script setup lang="ts">
/**
 * 导出预演（DRT-25，DR-F-125~129，DR-UX-11）：选机位（默认输出机位）→ 逐帧导出（进度 + 取消）→ 先存盘再上传 → 发布到画布。
 *
 * - 时长取时间轴的总时长，分辨率短边 720（`directorPreviewSize`），预计大小按固定码率估；
 * - 取消：编码器丢掉、不产出文件、不上传、不留记录；
 * - 超过 50 MiB 不上传，提示降低分辨率或缩短时长（DR-F-126）；
 * - 上传前 `store.flush()`，带刚存上的版本（DRD-14）；40902 进冲突流程，与截图同一套；
 * - 浏览器不支持 WebCodecs 的 H.264 编码时说明「请使用新版 Chrome / Edge」（DR-F-129）。
 */
import type { components } from "@tabtv/api-client";
import { directorCameraSnapshot } from "~/director/capture";
import type { DirectorPreviewResult } from "~/director/engine/Recorder";
import { DirectorFlushError } from "~/stores/director";
import { directorDisplayedObject, directorTimelineOf } from "~/utils/directorAnimation";
import { DIRECTOR_PREVIEW_FPS, DIRECTOR_PREVIEW_MAX_BYTES, directorPreviewEstimateBytes, directorPreviewSize } from "~/utils/directorCamera";
import { directorFailureText } from "~/utils/directorCanvas";
import { DIRECTOR_OBJECT_DEFAULTS } from "~/utils/directorCatalog";

type MediaRecord = components["schemas"]["DirectorMediaRecord"];
type Exporter = (cameraId: string, signal: AbortSignal, onProgress: (done: number, total: number) => void) => Promise<DirectorPreviewResult>;

const props = defineProps<{ projectId: string; sceneId: string; exporter: Exporter }>();
const emit = defineEmits<{ close: []; publish: [record: MediaRecord] }>();
const store = useDirectorStore();

const cameras = computed(() => (store.scene?.objects ?? []).filter(o => o.kind === "camera"));
const cameraId = ref(store.scene?.outputCameraId ?? cameras.value[0]?.id ?? "");
const phase = ref<"idle" | "rendering" | "uploading" | "done">("idle");
const progress = ref({ done: 0, total: 0 });
const message = ref("");
const record = ref<MediaRecord | null>(null);
let controller: AbortController | null = null;

const frames = computed(() => directorTimelineOf(store.scene).durationFrames);
const aspect = computed(() => {
  const camera = cameras.value.find(c => c.id === cameraId.value);
  const value = (camera?.camera as { aspect?: unknown } | undefined)?.aspect;
  return typeof value === "string" ? value : DIRECTOR_OBJECT_DEFAULTS.camera.aspect;
});
const size = computed(() => directorPreviewSize(aspect.value));
const estimate = computed(() => directorPreviewEstimateBytes(size.value.width, size.value.height, frames.value));
const megabytes = (bytes: number) => `${(bytes / 1024 / 1024).toFixed(1)} MB`;
const busy = computed(() => phase.value === "rendering" || phase.value === "uploading");

async function start() {
  if (busy.value || !cameraId.value) return;
  message.value = "";
  record.value = null;
  controller = new AbortController();
  phase.value = "rendering";
  progress.value = { done: 0, total: frames.value };
  let result: DirectorPreviewResult;
  try {
    result = await props.exporter(cameraId.value, controller.signal, (done, total) => {
      progress.value = { done, total };
    });
  } catch (cause) {
    phase.value = "idle";
    message.value = cause instanceof Error && cause.message ? `导出失败：${cause.message}` : "导出失败，请重试";
    return;
  }
  if (!result.supported) {
    phase.value = "idle";
    message.value = result.reason;
    return;
  }
  if (result.canceled) {
    phase.value = "idle";
    message.value = "已取消导出，没有上传";
    return;
  }
  if (result.blob.size > DIRECTOR_PREVIEW_MAX_BYTES) {
    phase.value = "idle";
    message.value = `导出的视频有 ${megabytes(result.blob.size)}，超过 50 MB：请降低分辨率或缩短时长，没有上传`;
    return;
  }
  await upload(result.blob, result.durationMs);
}

async function upload(blob: Blob, durationMs: number) {
  phase.value = "uploading";
  let version: number;
  try {
    version = await store.flush();
  } catch (cause) {
    phase.value = "idle";
    message.value = `没有上传：${cause instanceof DirectorFlushError ? cause.message : "场景还没存上"}`;
    return;
  }
  // 机位快照取第 0 帧（片头）的样子：有关键帧的机位一路在动，快照记的是起点
  const camera = store.scene ? directorDisplayedObject(store.scene, cameraId.value, 0) : undefined;
  const form = new FormData();
  form.append("file", blob, "preview.mp4");
  form.append("cameraId", cameraId.value);
  form.append("sceneVersion", String(version));
  form.append("durationMs", String(durationMs));
  form.append("fps", String(DIRECTOR_PREVIEW_FPS));
  if (camera) form.append("camera", JSON.stringify(directorCameraSnapshot(camera)));
  try {
    const saved = await useApi()<MediaRecord>(`/projects/${props.projectId}/director-scenes/${props.sceneId}/previews`, { method: "POST", body: form });
    store.addRecords([saved]);
    record.value = saved;
    phase.value = "done";
  } catch (cause) {
    phase.value = "idle";
    message.value = store.handleUploadConflict(cause) ? "场景刚被修改，请重新导出" : directorFailureText(cause, "上传失败，请重试");
  }
}

function cancel() {
  controller?.abort();
}

function close() {
  if (busy.value) return;
  emit("close");
}

onBeforeUnmount(() => controller?.abort());
</script>

<template>
  <div class="preview-export-mask" role="dialog" aria-modal="true" aria-label="导出预演" data-testid="preview-export">
    <section class="preview-export">
      <header>
        <strong>导出预演</strong>
        <button type="button" class="dr-btn" :disabled="busy" aria-label="关闭" @click="close">关闭</button>
      </header>
      <div class="dr-field">
        <span>机位</span>
        <select v-model="cameraId" class="dr-select" :disabled="busy" aria-label="导出哪个机位">
          <option v-for="camera in cameras" :key="camera.id" :value="camera.id">{{ camera.name }}{{ camera.id === store.scene?.outputCameraId ? "（输出机位）" : "" }}</option>
        </select>
      </div>
      <dl class="preview-export-facts">
        <dt>时长</dt><dd data-testid="preview-duration">{{ (frames / DIRECTOR_PREVIEW_FPS).toFixed(1) }} 秒 · {{ frames }} 帧 · {{ DIRECTOR_PREVIEW_FPS }} fps</dd>
        <dt>分辨率</dt><dd data-testid="preview-size">{{ size.width }} × {{ size.height }}</dd>
        <!-- 按目标码率估的是上界：白模画面大多静止，实际常小一个数量级（3 秒实测 0.23 MB） -->
        <dt>预计大小</dt><dd data-testid="preview-estimate">不超过约 {{ megabytes(estimate) }}（上限 50 MB）</dd>
      </dl>
      <p class="dr-hint">逐帧离屏渲染，与播放的快慢无关；画面里没有网格、手柄这些辅助元素。</p>
      <div v-if="phase === 'rendering'" class="preview-export-progress" role="status">
        <progress :value="progress.done" :max="progress.total || 1" aria-label="导出进度" />
        <span>{{ progress.done }} / {{ progress.total }} 帧</span>
        <button type="button" class="dr-btn" data-testid="preview-cancel" @click="cancel">取消</button>
      </div>
      <p v-else-if="phase === 'uploading'" class="dr-hint" role="status">正在上传…</p>
      <p v-if="message" class="preview-export-message" role="alert">{{ message }}</p>
      <div v-if="phase === 'done' && record" class="preview-export-done" role="status">
        <span>已导出并保存预演（{{ ((record.durationMs ?? 0) / 1000).toFixed(1) }} 秒）</span>
        <button type="button" class="dr-btn primary" data-testid="preview-publish" @click="emit('publish', record); emit('close')">发布到画布</button>
      </div>
      <footer v-if="phase === 'idle' || phase === 'done'">
        <button type="button" class="dr-btn primary" :disabled="!cameraId" data-testid="preview-start" @click="start">{{ phase === "done" ? "再导一次" : "开始导出" }}</button>
      </footer>
    </section>
  </div>
</template>

<style scoped>
.preview-export-mask { position: absolute; inset: 0; z-index: 30; display: grid; place-items: center; background: rgb(0 0 0 / 35%); }
.preview-export { display: grid; gap: 10px; width: min(420px, calc(100% - 32px)); padding: 14px 16px; border: 1px solid var(--portal-line-strong); border-radius: 10px; color: var(--portal-text); background: var(--portal-elevated); box-shadow: 0 18px 48px var(--portal-shadow-strong); font-size: 12px; }
.preview-export header { display: flex; align-items: center; justify-content: space-between; }
.preview-export-facts { display: grid; grid-template-columns: auto 1fr; gap: 4px 12px; margin: 0; }
.preview-export-facts dt { color: var(--portal-muted); }
.preview-export-facts dd { margin: 0; font-variant-numeric: tabular-nums; }
.preview-export-progress { display: flex; align-items: center; gap: 8px; }
.preview-export-progress progress { flex: 1; }
.preview-export-message { margin: 0; color: var(--portal-danger); }
.preview-export-done { display: flex; align-items: center; justify-content: space-between; gap: 8px; }
.preview-export footer { display: flex; justify-content: flex-end; }
</style>
