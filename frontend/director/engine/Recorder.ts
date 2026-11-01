/**
 * 预演视频（DRT-25，DR-F-125 / 126 / 129，DRD-42）：**离线逐帧渲染 + WebCodecs 编码**，mediabunny 封装成 mp4。
 *
 * - 帧号决定一切：第 f 帧先跳到 f（`renderFrame` 里 seek + 等素模、群众、背景），再离屏渲染、交给编码器，
 *   时间戳 = f ÷ fps。不看墙钟，两次导出逐帧一致（DR-N-07）。⚠️ 不用 MediaRecorder：实时录制会掉帧、结果不可复现；
 * - 帧在流里推给编码器，不在内存里攒：15 秒 × 30 帧 × 1280×720 的 RGBA 约 1.2 GB，攒着会把标签页撑爆；
 *   `CanvasSource.add` 的 Promise 就是背压，等它再画下一帧；
 * - 每帧检查 `signal.aborted`：取消时丢掉编码器与输出，不产出文件（调用方据此不上传、不留记录）；
 * - mediabunny 第一次导出时才加载（dev/03 §2.1；MPL-2.0 只约束到文件级，这里只调用、不改它）；
 * - 不支持：没有 `VideoEncoder`，或这个尺寸 / 码率编不了 H.264（`canEncodeVideo("avc")`）—— 返回 `supported: false`，
 *   界面置灰并说明「请使用新版 Chrome / Edge」（DR-F-129）。
 */
import { directorPreviewBitrate } from "~/utils/directorCamera";

export type DirectorPreviewResult =
  | { supported: false; reason: string }
  | { supported: true; canceled: true; frames: number }
  | { supported: true; canceled: false; blob: Blob; frames: number; width: number; height: number; durationMs: number };

export const DIRECTOR_PREVIEW_UNSUPPORTED = "这个浏览器不能导出视频（需要 WebCodecs 的 H.264 编码），请使用新版 Chrome / Edge";

/** 不加载 mediabunny 的快速判断：连 `VideoEncoder` 都没有就不用往下试（按钮据此置灰）。 */
export function directorCanRecordPreview(): boolean {
  return typeof globalThis.VideoEncoder === "function";
}

/** 渲染第 `frame` 帧，返回自上而下的 RGBA 像素（`width × height × 4`）。 */
export type DirectorFrameRenderer = (frame: number) => Promise<Uint8ClampedArray<ArrayBuffer>>;

export async function directorRecordPreview(options: {
  width: number;
  height: number;
  fps: number;
  frames: number;
  renderFrame: DirectorFrameRenderer;
  signal?: AbortSignal;
  onProgress?: (done: number, total: number) => void;
}): Promise<DirectorPreviewResult> {
  const { width, height, fps, frames } = options;
  if (!directorCanRecordPreview()) return { supported: false, reason: DIRECTOR_PREVIEW_UNSUPPORTED };
  const mb = await import("mediabunny");
  const bitrate = directorPreviewBitrate(width, height);
  if (!(await mb.canEncodeVideo("avc", { width, height, bitrate }))) return { supported: false, reason: DIRECTOR_PREVIEW_UNSUPPORTED };

  const canvas = document.createElement("canvas");
  canvas.width = width;
  canvas.height = height;
  const ctx = canvas.getContext("2d");
  if (!ctx) return { supported: false, reason: "浏览器无法创建画布" };
  const target = new mb.BufferTarget();
  const output = new mb.Output({ format: new mb.Mp4OutputFormat({ fastStart: "in-memory" }), target });
  // 每秒一个关键帧：下游拖进度条时好定位，体积代价对白模画面很小
  const source = new mb.CanvasSource(canvas, { codec: "avc", bitrate, keyFrameInterval: 1 });
  output.addVideoTrack(source, { frameRate: fps });
  const release = () => {
    canvas.width = 0;
    canvas.height = 0;
  };
  try {
    await output.start();
    for (let f = 0; f < frames; f++) {
      if (options.signal?.aborted) {
        await output.cancel();
        return { supported: true, canceled: true, frames: f };
      }
      const pixels = await options.renderFrame(f);
      ctx.putImageData(new ImageData(pixels, width, height), 0, 0);
      await source.add(f / fps, 1 / fps);
      options.onProgress?.(f + 1, frames);
    }
    if (options.signal?.aborted) {
      await output.cancel();
      return { supported: true, canceled: true, frames };
    }
    await output.finalize();
  } catch (err) {
    if (output.state !== "finalized" && output.state !== "canceled") await output.cancel().catch(() => undefined);
    throw err;
  } finally {
    release();
  }
  const buffer = target.buffer;
  if (!buffer) throw new Error("视频封装失败");
  return { supported: true, canceled: false, blob: new Blob([buffer], { type: "video/mp4" }), frames, width, height, durationMs: Math.round((frames * 1000) / fps) };
}
