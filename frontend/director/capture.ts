/**
 * 截图上传的请求体（DRT-17，契约 `uploadDirectorCapture`）。纯函数，随编辑器的异步 chunk 加载。
 *
 * - `sceneVersion` 必须是**刚存上**的那个版本（`store.flush()` 的返回值）：服务端先核版本再收文件，
 *   不一致回 409、文件不上传（DRD-14、DR-AC-18）；
 * - `camera` 是截图那一刻的机位参数快照：机位之后改名、改焦距、被删掉，记录仍说得清当时是哪一个。
 */
import { DIRECTOR_OBJECT_DEFAULTS } from "~/utils/directorCatalog";
import type { DirectorObject } from "~/utils/directorScene";

export type DirectorCaptureKind = "capture" | "depth" | "pose";

/** 机位参数快照：焦距、传感器、画幅、裁剪、横滚与位置朝向（世界坐标，度）。 */
export function directorCameraSnapshot(camera: DirectorObject): Record<string, unknown> {
  const params = { ...DIRECTOR_OBJECT_DEFAULTS.camera, ...((camera.camera ?? {}) as Record<string, unknown>) };
  return {
    name: camera.name,
    focalLengthMm: params.focalLengthMm,
    sensorWidthMm: params.sensorWidthMm,
    aspect: params.aspect,
    near: params.near,
    far: params.far,
    roll: params.roll,
    position: { ...camera.transform.position },
    rotation: { ...camera.transform.rotation }
  };
}

export function directorCaptureForm(input: {
  blob: Blob;
  kind: DirectorCaptureKind;
  cameraId: string;
  sceneVersion: number;
  camera: Record<string, unknown>;
  /** 文件名（深度图把近远写进去，DRT-27）；不给时按类型。 */
  filename?: string;
}): FormData {
  const body = new FormData();
  body.append("file", input.blob, input.filename ?? `${input.kind}.png`);
  body.append("kind", input.kind);
  body.append("cameraId", input.cameraId);
  body.append("sceneVersion", String(input.sceneVersion));
  body.append("camera", JSON.stringify(input.camera));
  return body;
}

/**
 * 深度图的文件名（DR-F-133）：把近远（米，两位小数）写进去，下游拿到文件就知道编码 —— 如「机位1-depth-near0.80-far12.40.png」。
 * 机位名里的空白去掉，文件系统不认的字符换成下划线。
 */
export function directorDepthFilename(cameraName: string, near: number, far: number): string {
  const name = (cameraName.replace(/\s+/g, "") || "机位").replace(/[\\/:*?"<>|]/g, "_");
  return `${name}-depth-near${near.toFixed(2)}-far${far.toFixed(2)}.png`;
}

/**
 * 「截当前机位」截哪一个：机位视角下正透过的那个 → 选中的机位 → 输出机位 → 第一个机位。一个都没有时为 null。
 */
export function directorCurrentCameraId(input: {
  viewCameraId: string | null;
  selected: DirectorObject | undefined;
  outputCameraId: string | null | undefined;
  cameraIds: readonly string[];
}): string | null {
  const exists = (id: string | null | undefined): id is string => Boolean(id) && input.cameraIds.includes(id as string);
  if (exists(input.viewCameraId)) return input.viewCameraId;
  if (input.selected?.kind === "camera" && exists(input.selected.id)) return input.selected.id;
  if (exists(input.outputCameraId)) return input.outputCameraId;
  return input.cameraIds[0] ?? null;
}
