/// <reference lib="webworker" />
/**
 * 姿势识别 Worker（DRT-23，DRD-41，DRK-13）：MediaPipe Pose Landmarker 跑在这里 —— 主线程跑会卡住编辑器，
 * 照片只以 ImageBitmap 的形式转进来，**不离开浏览器**。
 *
 * - 文件全部自托管在 `/director/mediapipe/`（`scripts/director-assets.mjs mediapipe`），**不访问 Google CDN**；
 * - 第一次收到识别请求时才下载（wasm 13.5 MB + 模型 9.4 MB），边下边回传进度（DRR-13）；
 *   wasm 由这里取回再交给 MediaPipe（blob 地址），这样进度里算得到它，也不会被取两遍；
 * - ⚠️ 这是 module worker：MediaPipe 在这里只能用模块版加载器（`forVisionTasks(base, true)`）。
 *   经典版靠 `importScripts`，module worker 里调它抛 TypeError，退回 `import()` 之后顶层的 `var ModuleFactory`
 *   不是全局变量，报的是「ModuleFactory not set」—— 看着像文件没下全；
 * - delegate 默认 GPU（Worker 里靠 OffscreenCanvas 的 WebGL2），建不起来退回 CPU；`ready` 消息说明实际用的哪个；
 * - 一个人回 33 个图像坐标点（0~1）与 33 个世界坐标点（米），`score` 取 33 点可见度的平均。
 */
import { FilesetResolver, PoseLandmarker, type Landmark, type NormalizedLandmark } from "@mediapipe/tasks-vision";

export type DirectorPoseDelegate = "GPU" | "CPU";
export type DirectorPoseWorkerRequest = { type: "detect"; id: number; bitmap: ImageBitmap; delegate?: DirectorPoseDelegate };
type Point = { x: number; y: number; z: number; visibility: number };
export type DirectorPoseWorkerMessage =
  | { type: "progress"; loaded: number; total: number }
  | { type: "ready"; delegate: DirectorPoseDelegate }
  | { type: "result"; id: number; poses: Array<{ landmarks: Point[]; worldLandmarks: Point[]; score: number }> }
  | { type: "error"; id: number; message: string };

const BASE = "/director/mediapipe";
const WASM_URL = `${BASE}/wasm/vision_wasm_module_internal.wasm`;
const MODEL_URL = `${BASE}/pose_landmarker_full.task`;

const scope = self as unknown as DedicatedWorkerGlobalScope;
const post = (message: DirectorPoseWorkerMessage) => scope.postMessage(message);

/**
 * ⚠️ MediaPipe 的加载器在 module worker 里见到 `self.import` 就把它当动态 import 函数来调；而 Nuxt 开发态往全局挂了
 *    `import = { meta: … }`（给 `import.meta.client` 之类做兼容），于是报「e3 is not a function」—— 看着像 MediaPipe 坏了。
 *    这里换成一个真的函数，`meta` 原样留着。
 */
const globalImport = scope as unknown as { import?: { meta?: unknown } };
globalImport.import = Object.assign((url: string) => import(/* @vite-ignore */ url), { meta: globalImport.import?.meta });

/**
 * ⚠️ MediaPipe 会把用量统计（任务类型、运行模式、平台、版本、耗时）每分钟 POST 到 `odml.pa.googleapis.com`，
 *    没有公开的开关（`enableLogging` 只在类型声明里）。照片不在里面，但识别全程不得访问外部服务（DR-N-11 / DRK-13）：
 *    这个 Worker 里只放行同源、blob: 与 data: 请求，其余一律拒绝 —— 它那边当成网络失败，记一次错误后就不再发。
 *    拒在这里而不是靠 CSP：本仓库的部署方式还没定，靠部署配置兜底的话少配一处就悄悄外发。
 */
const nativeFetch = scope.fetch.bind(scope);
scope.fetch = (input: RequestInfo | URL, init?: RequestInit) => {
  const raw = input instanceof Request ? input.url : String(input);
  const url = new URL(raw, scope.location.href);
  if (url.protocol !== "blob:" && url.protocol !== "data:" && url.origin !== scope.location.origin) {
    return Promise.reject(new TypeError(`姿势识别不访问外部地址：${url.origin}`));
  }
  return nativeFetch(input, init);
};

/** 两个文件一起下，按已收到的字节报进度；服务端没给长度时总数报 0（界面显示已下载多少）。 */
async function download(urls: string[]): Promise<Uint8Array[]> {
  const responses = await Promise.all(urls.map(url => fetch(url)));
  responses.forEach((response, i) => {
    if (!response.ok) throw new Error(`识别模型下载失败（${urls[i]}：HTTP ${response.status}）`);
  });
  const sizes = responses.map(r => Number(r.headers.get("content-length")) || 0);
  const total = sizes.every(Boolean) ? sizes.reduce((a, b) => a + b, 0) : 0;
  const loaded = responses.map(() => 0);
  return Promise.all(responses.map(async (response, i) => {
    const reader = response.body?.getReader();
    if (!reader) return new Uint8Array(await response.arrayBuffer());
    const chunks: Uint8Array[] = [];
    for (;;) {
      const { done, value } = await reader.read();
      if (done) break;
      chunks.push(value);
      loaded[i] = (loaded[i] ?? 0) + value.byteLength;
      post({ type: "progress", loaded: loaded.reduce((a, b) => a + b, 0), total });
    }
    const out = new Uint8Array(chunks.reduce((n, c) => n + c.byteLength, 0));
    let offset = 0;
    for (const chunk of chunks) {
      out.set(chunk, offset);
      offset += chunk.byteLength;
    }
    return out;
  }));
}

let loading: Promise<{ task: PoseLandmarker; delegate: DirectorPoseDelegate }> | null = null;

async function load(prefer: DirectorPoseDelegate) {
  const [wasm, model] = await download([WASM_URL, MODEL_URL]);
  const fileset = await FilesetResolver.forVisionTasks(`${BASE}/wasm`, true);
  const wasmBinaryPath = URL.createObjectURL(new Blob([wasm as Uint8Array<ArrayBuffer>], { type: "application/wasm" }));
  const create = (delegate: DirectorPoseDelegate) => PoseLandmarker.createFromOptions(
    { ...fileset, wasmBinaryPath },
    { baseOptions: { modelAssetBuffer: model, delegate }, runningMode: "IMAGE", numPoses: 3 }
  );
  try {
    if (prefer === "GPU") {
      try {
        return { task: await create("GPU"), delegate: "GPU" as const };
      } catch {
        // Worker 里没有可用的 WebGL2（驱动、OffscreenCanvas）：退回 CPU，慢一些但结果相同
      }
    }
    return { task: await create("CPU"), delegate: "CPU" as const };
  } finally {
    // 建好（或彻底失败）之后 wasm 已经编译进模块，这个地址不再用：不释放的话 13.5 MB 的字节一直挂在内存里
    URL.revokeObjectURL(wasmBinaryPath);
  }
}

const plain = (points: Array<Landmark | NormalizedLandmark>): Point[] =>
  points.map(p => ({ x: p.x, y: p.y, z: p.z, visibility: typeof p.visibility === "number" ? p.visibility : 1 }));

scope.onmessage = async (event: MessageEvent<DirectorPoseWorkerRequest>) => {
  const request = event.data;
  if (request?.type !== "detect") return;
  try {
    if (!loading) {
      loading = load(request.delegate ?? "GPU");
      loading.catch(() => {
        loading = null;
      });
    }
    const { task, delegate } = await loading;
    post({ type: "ready", delegate });
    const result = task.detect(request.bitmap);
    const poses = result.landmarks.map((landmarks, i) => {
      const image = plain(landmarks);
      return { landmarks: image, worldLandmarks: plain(result.worldLandmarks[i] ?? []), score: image.reduce((n, p) => n + p.visibility, 0) / (image.length || 1) };
    });
    post({ type: "result", id: request.id, poses });
  } catch (cause) {
    post({ type: "error", id: request.id, message: cause instanceof Error && cause.message ? cause.message : "识别失败" });
  } finally {
    request.bitmap.close();
  }
};
