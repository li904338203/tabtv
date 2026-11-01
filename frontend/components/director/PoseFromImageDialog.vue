<script setup lang="ts">
/**
 * 从图片识别姿势（DRT-23，DR-F-110~114，DRK-13）：选一张人物照片（本地文件或画布上的图片）→ Worker 里识别 →
 * 原图上叠画识别出的骨架供确认 →「应用到角色」写成这个角色自己的姿势（记一步撤销）。
 *
 * - **照片不上传**：本地文件只在内存里读成 ImageBitmap 交给 Worker；画布上的图片按受保护媒体取回 blob。
 *   这个对话框不调任何写接口；
 * - 多人取 2D 包围盒最大的一个并说明；没人时说「没有识别到人物」、角色不变（DR-F-112）；
 * - 「左右镜像」只改换算，不重新识别（DR-F-113）；
 * - 第一次识别要下载约 23 MB 的识别文件，显示进度（DRR-13）。
 */
import { ScanSearch, X } from "@lucide/vue";
import type { DirectorPoseDelegate, DirectorPoseDetector } from "~/director/PoseDetector";
import { directorMentionCandidates } from "~/utils/directorCanvas";
import type { DirectorModelCode } from "~/utils/directorCatalog";
import {
  directorApplyDetectedPose,
  directorPickLargestPose,
  directorPoseFromLandmarks,
  type DirectorDetectedPose,
  type DirectorLandmark
} from "~/utils/directorPose";
import type { DirectorObject } from "~/utils/directorScene";

const props = defineProps<{ object: DirectorObject; detector: () => DirectorPoseDetector }>();
const emit = defineEmits<{ close: []; applied: [] }>();
const store = useDirectorStore();
const canvas = useCanvasStore();
const media = useAuthenticatedMedia();
const thumbs = useAuthenticatedMedia();

/** 开发与自查用：强制 CPU / GPU（默认 GPU，建不起来退回 CPU）。 */
const DELEGATE_KEY = "seeaitv:director-pose-delegate";

const source = ref<"file" | "canvas">("file");
const fileInput = ref<HTMLInputElement | null>(null);
const preview = ref<HTMLCanvasElement | null>(null);
const imageBlob = shallowRef<Blob | null>(null);
const imageName = ref("");
const pickedNodeId = ref("");
const phase = ref<"idle" | "loading" | "detecting" | "done">("idle");
const progress = ref({ loaded: 0, total: 0 });
const message = ref("");
const notice = ref("");
const detected = shallowRef<DirectorDetectedPose | null>(null);
const mirror = ref(false);
const delegate = ref<DirectorPoseDelegate | "">("");
let bitmap: ImageBitmap | null = null;

const candidates = computed(() => directorMentionCandidates(canvas.nodes, "", "", 40));
const model = computed(() => ((props.object.character as { model?: string } | undefined)?.model ?? "adult_male") as DirectorModelCode);
const result = computed(() => (detected.value ? directorPoseFromLandmarks(detected.value.worldLandmarks, { mirror: mirror.value, model: model.value }) : null));
const busy = computed(() => phase.value === "loading" || phase.value === "detecting");
const canApply = computed(() => !busy.value && Boolean(result.value && Object.keys(result.value.bones).length));
const progressText = computed(() => {
  const mb = (bytes: number) => (bytes / 1024 / 1024).toFixed(1);
  const { loaded, total } = progress.value;
  if (!total) return `正在下载识别模型（已下载 ${mb(loaded)} MB）`;
  return `正在下载识别模型 ${Math.min(100, Math.round((loaded / total) * 100))}%（第一次使用，约 ${mb(total)} MB）`;
});

watch(source, (value) => {
  if (value === "canvas") void thumbs.appendAll(candidates.value.map(item => ({ key: item.nodeId, url: `/api/v1/media/${item.mediaId}?w=160` })));
});

async function useBlob(blob: Blob, name: string) {
  message.value = "";
  notice.value = "";
  detected.value = null;
  phase.value = "idle";
  let next: ImageBitmap;
  try {
    next = await createImageBitmap(blob);
  } catch {
    message.value = "这张图打不开，请换一张（支持 PNG、JPEG、WebP）";
    return;
  }
  bitmap?.close();
  bitmap = next;
  imageBlob.value = blob;
  imageName.value = name;
  await nextTick();
  draw();
}

function onFile(event: Event) {
  const file = (event.target as HTMLInputElement).files?.[0];
  if (file) void useBlob(file, file.name);
}

async function pickCanvasImage(nodeId: string, mediaId: string, title: string) {
  pickedNodeId.value = nodeId;
  try {
    await useBlob(await media.blob(`/api/v1/media/${mediaId}`), title);
  } catch {
    message.value = "这张图取不到，请稍后再试";
  }
}

async function detect() {
  if (!imageBlob.value || busy.value) return;
  message.value = "";
  notice.value = "";
  detected.value = null;
  progress.value = { loaded: 0, total: 0 };
  const detector = props.detector();
  // 模型已经加载过（同一个编辑器会话里第二次识别）就没有下载这一步
  phase.value = detector.delegate ? "detecting" : "loading";
  const prefer = import.meta.client ? (localStorage.getItem(DELEGATE_KEY) as DirectorPoseDelegate | null) : null;
  try {
    const poses = await detector.detect(await createImageBitmap(imageBlob.value), {
      delegate: prefer === "CPU" || prefer === "GPU" ? prefer : undefined,
      onProgress: (loaded, total) => {
        progress.value = { loaded, total };
        if (total && loaded >= total) phase.value = "detecting";
      }
    });
    delegate.value = detector.delegate ?? "";
    const picked = directorPickLargestPose(poses);
    phase.value = "done";
    if (!picked) {
      message.value = "没有识别到人物";
      draw();
      return;
    }
    if (poses.length > 1) notice.value = "画面里有多个人，已取最大的一个；可以先裁剪";
    detected.value = picked;
    draw();
  } catch (cause) {
    phase.value = "idle";
    message.value = `识别失败：${cause instanceof Error && cause.message ? cause.message : "请重试"}`;
  }
}

/** BlazePose 的身体连线（脸只画点）。 */
const LINKS: ReadonlyArray<readonly [number, number]> = [
  [11, 12], [11, 13], [13, 15], [15, 19], [12, 14], [14, 16], [16, 20], [11, 23], [12, 24], [23, 24],
  [23, 25], [25, 27], [27, 31], [24, 26], [26, 28], [28, 32]
];

/** 原图等比放进预览框，识别出来的话叠画骨架（左侧暖色、右侧冷色，可见度低的点画成空心）。 */
function draw() {
  const el = preview.value;
  if (!el || !bitmap) return;
  const box = { w: el.clientWidth || 380, h: 280 };
  const scale = Math.min(box.w / bitmap.width, box.h / bitmap.height);
  const w = Math.max(1, Math.round(bitmap.width * scale));
  const h = Math.max(1, Math.round(bitmap.height * scale));
  const dpr = window.devicePixelRatio || 1;
  el.width = Math.round(w * dpr);
  el.height = Math.round(h * dpr);
  el.style.width = `${w}px`;
  el.style.height = `${h}px`;
  const ctx = el.getContext("2d");
  if (!ctx) return;
  ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
  ctx.drawImage(bitmap, 0, 0, w, h);
  const points = detected.value?.landmarks;
  if (!points) return;
  const at = (p: DirectorLandmark) => [p.x * w, p.y * h] as const;
  const side = (i: number) => (i === 0 ? "#f5f5f5" : i % 2 === 1 ? "#ff9f43" : "#4dabf7");
  ctx.lineWidth = 2.5;
  for (const [a, b] of LINKS) {
    const pa = points[a];
    const pb = points[b];
    if (!pa || !pb) continue;
    ctx.strokeStyle = a % 2 === 1 && b % 2 === 1 ? "#ff9f43" : a % 2 === 0 && b % 2 === 0 ? "#4dabf7" : "#f5f5f5";
    ctx.beginPath();
    ctx.moveTo(...at(pa));
    ctx.lineTo(...at(pb));
    ctx.stroke();
  }
  points.forEach((p, i) => {
    const [x, y] = at(p);
    ctx.beginPath();
    ctx.arc(x, y, i <= 10 ? 2.5 : 3.5, 0, Math.PI * 2);
    if ((p.visibility ?? 1) >= 0.5) {
      ctx.fillStyle = side(i);
      ctx.fill();
    } else {
      ctx.strokeStyle = side(i);
      ctx.lineWidth = 1.5;
      ctx.stroke();
    }
  });
}

function apply() {
  const bones = result.value?.bones;
  if (!canApply.value || !bones) return;
  const accepted = store.commit(scene => directorApplyDetectedPose(scene, props.object.id, bones), { touched: [props.object.id] });
  if (!accepted) {
    message.value = "没有应用：场景正在等待确认或你没有编辑权限";
    return;
  }
  emit("applied");
  emit("close");
}

function close() {
  if (busy.value) return;
  emit("close");
}

onBeforeUnmount(() => {
  bitmap?.close();
  bitmap = null;
});
</script>

<template>
  <div class="pose-image-mask" role="dialog" aria-modal="true" aria-label="从图片识别姿势" data-testid="pose-from-image-dialog" :data-delegate="delegate || undefined">
    <section class="pose-image">
      <header>
        <strong><ScanSearch :size="14" />从图片识别姿势 · {{ object.name }}</strong>
        <button type="button" class="dr-btn" :disabled="busy" aria-label="关闭" @click="close"><X :size="13" /></button>
      </header>
      <div class="dr-seg" role="tablist" aria-label="图片来源">
        <button type="button" :class="{ active: source === 'file' }" :disabled="busy" @click="source = 'file'">本地图片</button>
        <button type="button" :class="{ active: source === 'canvas' }" :disabled="busy" @click="source = 'canvas'">画布上的图片</button>
      </div>
      <div v-if="source === 'file'" class="pose-image-file">
        <button type="button" class="dr-btn" :disabled="busy" data-testid="pose-image-choose" @click="fileInput?.click()">选择图片…</button>
        <span class="dr-hint">{{ imageName || "照片只在浏览器里识别，不会上传" }}</span>
        <input ref="fileInput" type="file" accept="image/*" hidden data-testid="pose-image-file" @change="onFile">
      </div>
      <div v-else class="pose-image-canvas">
        <p v-if="!candidates.length" class="dr-hint">画布上还没有出过图的图片节点</p>
        <button
          v-for="item in candidates"
          :key="item.nodeId"
          type="button"
          class="pose-image-thumb"
          :class="{ active: pickedNodeId === item.nodeId }"
          :disabled="busy"
          :title="item.title"
          data-testid="pose-image-canvas-item"
          @click="pickCanvasImage(item.nodeId, item.mediaId, item.title)"
        >
          <img v-if="thumbs.urls.value[item.nodeId]" :src="thumbs.urls.value[item.nodeId]" :alt="item.title">
          <span>{{ item.title }}</span>
        </button>
      </div>
      <div class="pose-image-preview" :class="{ empty: !imageBlob }">
        <canvas v-show="imageBlob" ref="preview" data-testid="pose-image-preview" />
        <span v-if="!imageBlob" class="dr-hint">选一张能看清全身的人物照片：站姿、动作越完整，识别越准</span>
      </div>
      <label class="pose-image-mirror">
        <input v-model="mirror" type="checkbox" data-testid="pose-image-mirror">左右镜像（自拍照常是镜像的）
      </label>
      <p v-if="phase === 'loading'" class="dr-hint" role="status" data-testid="pose-image-progress">{{ progressText }}</p>
      <p v-else-if="phase === 'detecting'" class="dr-hint" role="status">正在识别…</p>
      <p v-if="message" class="pose-image-message" role="alert" data-testid="pose-image-message">{{ message }}</p>
      <p v-if="notice" class="pose-image-notice" role="status" data-testid="pose-image-notice">{{ notice }}</p>
      <p v-for="warning in result?.warnings ?? []" :key="warning" class="pose-image-notice" role="status">{{ warning }}</p>
      <p class="dr-hint">识别结果只当初值：单张照片看不出前后深浅，应用后可以在「姿势」页签用关节模式继续微调。</p>
      <footer>
        <button type="button" class="dr-btn" :disabled="!imageBlob || busy" data-testid="pose-image-detect" @click="detect">识别</button>
        <button type="button" class="dr-btn primary" :disabled="!canApply" data-testid="pose-image-apply" @click="apply">应用到角色</button>
      </footer>
    </section>
  </div>
</template>

<style scoped>
.pose-image-mask { position: absolute; inset: 0; z-index: 30; display: grid; place-items: center; background: rgb(0 0 0 / 35%); }
.pose-image { display: grid; gap: 10px; width: min(440px, calc(100% - 32px)); max-height: calc(100% - 32px); overflow: auto; padding: 14px 16px; border: 1px solid var(--portal-line-strong); border-radius: 10px; color: var(--portal-text); background: var(--portal-elevated); box-shadow: 0 18px 48px var(--portal-shadow-strong); font-size: 12px; }
.pose-image header { display: flex; align-items: center; justify-content: space-between; }
.pose-image header strong { display: inline-flex; align-items: center; gap: 6px; }
.pose-image-file { display: flex; align-items: center; gap: 8px; min-width: 0; }
.pose-image-file .dr-hint { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.pose-image-canvas { display: grid; grid-template-columns: repeat(4, 1fr); gap: 6px; max-height: 150px; overflow: auto; }
.pose-image-thumb { display: grid; gap: 2px; padding: 3px; border: 1px solid var(--portal-line); border-radius: 6px; color: var(--portal-text-soft); background: transparent; font-size: 11px; cursor: pointer; }
.pose-image-thumb.active { border-color: var(--portal-text); }
.pose-image-thumb img { width: 100%; aspect-ratio: 1; object-fit: cover; border-radius: 4px; }
.pose-image-thumb span { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.pose-image-preview { display: grid; place-items: center; min-height: 120px; border: 1px dashed var(--portal-line); border-radius: 8px; padding: 6px; }
.pose-image-preview canvas { max-width: 100%; border-radius: 4px; }
.pose-image-mirror { display: inline-flex; align-items: center; gap: 6px; }
.pose-image-message { margin: 0; color: var(--portal-danger); }
.pose-image-notice { margin: 0; color: var(--portal-warning, var(--portal-text-soft)); }
.pose-image footer { display: flex; justify-content: flex-end; gap: 8px; }
</style>
