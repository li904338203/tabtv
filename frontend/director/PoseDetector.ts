/**
 * 姿势识别的主线程一侧（DRT-23）：按需建 Worker、按请求号对应结果、转发下载进度；编辑器关闭时 `dispose()` 终止 Worker。
 *
 * Worker 第一次识别时才建（`new Worker(new URL(...))` 由 Vite 单独打包，MediaPipe 只在 Worker 的包里，
 * 不进编辑器的主包）。模型加载一次，同一个编辑器会话里后续识别直接用。
 */
import type { DirectorDetectedPose } from "~/utils/directorPose";
import type { DirectorPoseDelegate, DirectorPoseWorkerMessage, DirectorPoseWorkerRequest } from "./workers/pose.worker";

export type { DirectorPoseDelegate } from "./workers/pose.worker";

export class DirectorPoseDetector {
  private worker: Worker | null = null;
  private seq = 0;
  private readonly pending = new Map<number, { resolve: (poses: DirectorDetectedPose[]) => void; reject: (error: Error) => void }>();
  private progress: ((loaded: number, total: number) => void) | null = null;
  /** 实际用上的 delegate（第一次识别之后才有）。 */
  delegate: DirectorPoseDelegate | null = null;

  /** 识别一张图（ImageBitmap 会被转进 Worker，调用方之后不能再用它）。 */
  detect(bitmap: ImageBitmap, options: { delegate?: DirectorPoseDelegate; onProgress?: (loaded: number, total: number) => void } = {}): Promise<DirectorDetectedPose[]> {
    const worker = this.ensure();
    this.progress = options.onProgress ?? null;
    const id = ++this.seq;
    return new Promise((resolve, reject) => {
      this.pending.set(id, { resolve, reject });
      const request: DirectorPoseWorkerRequest = { type: "detect", id, bitmap, delegate: options.delegate };
      worker.postMessage(request, [bitmap]);
    });
  }

  dispose(): void {
    this.worker?.terminate();
    this.worker = null;
    for (const { reject } of this.pending.values()) reject(new Error("编辑器已关闭"));
    this.pending.clear();
  }

  private ensure(): Worker {
    if (this.worker) return this.worker;
    const worker = new Worker(new URL("./workers/pose.worker.ts", import.meta.url), { type: "module", name: "director-pose" });
    worker.onmessage = (event: MessageEvent<DirectorPoseWorkerMessage>) => {
      const message = event.data;
      if (message.type === "progress") this.progress?.(message.loaded, message.total);
      else if (message.type === "ready") this.delegate = message.delegate;
      else {
        const waiter = this.pending.get(message.id);
        this.pending.delete(message.id);
        if (message.type === "result") waiter?.resolve(message.poses);
        else waiter?.reject(new Error(message.message));
      }
    };
    worker.onerror = (event) => {
      event.preventDefault();
      for (const { reject } of this.pending.values()) reject(new Error(event.message || "识别失败"));
      this.pending.clear();
      this.worker?.terminate();
      this.worker = null;
    };
    this.worker = worker;
    return worker;
  }
}
