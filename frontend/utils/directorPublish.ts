/**
 * 3D 导演台的截图回写与发布（DRT-17，dev/03 §7.1~7.2）：纯函数，不碰网络、不碰 store。
 *
 * 两条路把截图交给下游（DRK-3）：
 * - **导演台节点的输出口**：截图后把输出机位的最新截图镜像进节点 params（`outputMediaId` / `cover` / `captures`），
 *   下游按 `outputMediaId` 取参考（utils/canvas.generationInputs）；
 * - **发布到画布**：每条记录建一个图片 / 视频节点，`generation` 是「已有产出」形态，`params.directorSource` 记来源。
 *
 * ⚠️ 镜像过来的都是**记录上的值**（截图那一刻的指纹、机位名、镜头文字）：记录不可变，镜像是安全的；
 *    节点卡判断过期、下游「插入镜头文字」因此都不必加载场景。
 * ⚠️ 发布节点的 `generation` **没有 `id`**：有 `id` 的话下游会按 `generationId` 取输入，而那个 ID 不存在
 *    （与复制项目时的 GAP-16 同类）—— 下游一律按 `mediaId` 声明「参考」。
 */
import type { components } from "@tabtv/api-client";
import type { GenerationView, JsonObject } from "~/types/canvas";
import type { DirectorScene } from "./directorScene";

/** 本地别名：stores/director 已经导出了同名类型，这里再导出的话 Nuxt 的自动导入会报重名。 */
type DirectorMediaRecord = components["schemas"]["DirectorMediaRecord"];
/** 能发布到画布的记录种类（泼溅文件只作场景背景，不发布）。 */
export type DirectorPublishedKind = "capture" | "depth" | "pose" | "preview";

/** 节点 params.captures 里的一条：每个机位一条最新截图的摘要（DR-F-74）。 */
export type DirectorCaptureSummary = {
  cameraId: string;
  cameraName: string;
  recordId: string;
  mediaId: string;
  width: number | null;
  height: number | null;
  renderFingerprint: string | null;
  /** 截图那一刻的镜头文字（DRT-26）；没有时不带。 */
  shotText?: string;
};

/** 发布节点 params.directorSource：来源导演台与那条记录。 */
export type DirectorSourceRef = {
  kind: DirectorPublishedKind;
  sceneId: string;
  recordId: string;
  mediaId: string;
  cameraId: string | null;
  cameraName: string | null;
  renderFingerprint: string | null;
  /** 截图那一刻的镜头文字（DRT-26）：下游「插入镜头文字」用它，手上没有场景数据也能插。没有时不带。 */
  shotText?: string;
};

/**
 * 打开编辑器时要它顺手做的事：
 * - `recapture`（DRT-17）重新截一张图。`nodeId` 是要替换产出的那个节点（导演台节点自己 = 只回写 captures；
 *   发布节点 = 截完只替换它）；`cameraId` 为 null 时截输出机位；
 * - `build`（DRT-20）按节点上的描述与参考图自动搭景一次（节点面板点了「发送」）。只发一次，提案仍要用户确认。
 */
export type DirectorPendingAction =
  | { type: "recapture"; nodeId: string; cameraId: string | null }
  | { type: "build" };

/** 编辑器截图之后发给画布的事件（DirectorStudio → CanvasBoard，dev/03 §7.1）。 */
export type DirectorCapturedEvent = {
  records: DirectorMediaRecord[];
  /** 截图那一刻的场景正文：回写 captures 要按它的机位顺序与输出机位。 */
  scene: DirectorScene;
  /** 「重新截图」时要替换产出的发布节点。 */
  replaceNodeId?: string;
  /** 每个机位截图那一刻的镜头文字（DRT-26，机位 ID → 文字）：由编辑器算好带过来，画布不必加载镜头文字的算法。 */
  shotTexts?: Record<string, string>;
};

/** 截图条 / toast 上的「发布到画布」（dev/03 §7.2）。`shotTexts` 按记录 ID 给每张图的镜头文字（DRT-26）。 */
export type DirectorPublishEvent = { records: DirectorMediaRecord[]; shotTexts?: Record<string, string> };

export function directorMediaUrl(mediaId: string): string {
  return `/api/v1/media/${mediaId}`;
}

function text(value: unknown): string {
  return typeof value === "string" ? value : "";
}

function numberOrNull(value: unknown): number | null {
  return typeof value === "number" && Number.isFinite(value) ? value : null;
}

/** 读节点上的 `captures`：形状不对的条目丢掉（老数据、手改过的画布不该让节点卡崩掉）。 */
export function directorCaptureSummaries(params: JsonObject): DirectorCaptureSummary[] {
  if (!Array.isArray(params.captures)) return [];
  return params.captures.flatMap((item): DirectorCaptureSummary[] => {
    if (!item || typeof item !== "object" || Array.isArray(item)) return [];
    const entry = item as Record<string, unknown>;
    const cameraId = text(entry.cameraId);
    const mediaId = text(entry.mediaId);
    if (!cameraId || !/^\d+$/.test(mediaId)) return [];
    return [{
      cameraId,
      cameraName: text(entry.cameraName),
      recordId: text(entry.recordId),
      mediaId,
      width: numberOrNull(entry.width),
      height: numberOrNull(entry.height),
      renderFingerprint: typeof entry.renderFingerprint === "string" ? entry.renderFingerprint : null,
      ...(typeof entry.shotText === "string" && entry.shotText ? { shotText: entry.shotText } : {})
    }];
  });
}

/** 发布节点上的来源；没有或形状不对时为 null。 */
export function directorSourceOf(params: JsonObject): DirectorSourceRef | null {
  const raw = params.directorSource;
  if (!raw || typeof raw !== "object" || Array.isArray(raw)) return null;
  const source = raw as Record<string, unknown>;
  const kind = source.kind;
  if (kind !== "capture" && kind !== "depth" && kind !== "pose" && kind !== "preview") return null;
  const mediaId = text(source.mediaId);
  if (!/^\d+$/.test(mediaId)) return null;
  return {
    kind,
    sceneId: text(source.sceneId),
    recordId: text(source.recordId),
    mediaId,
    cameraId: text(source.cameraId) || null,
    cameraName: text(source.cameraName) || null,
    renderFingerprint: typeof source.renderFingerprint === "string" ? source.renderFingerprint : null,
    ...(text(source.shotText).trim() ? { shotText: text(source.shotText).trim() } : {})
  };
}

/** 场景里的机位，按正文里的顺序（`captures` 也按这个顺序排，DR-AC-17）。 */
function camerasOf(scene: DirectorScene): Array<{ id: string; name: string }> {
  return (scene.objects ?? [])
    .filter(object => object.kind === "camera")
    .map(object => ({ id: object.id, name: object.name }));
}

/**
 * 截图之后回写导演台节点的 params（dev/03 §7.1 第 4 步）：
 * - `captures`：每个机位一条最新截图的摘要，按场景里机位的顺序；这批没截到的机位沿用原来那条，
 *   场景里已经没有的机位去掉；同一机位这批截了多张时取最晚的一张；
 * - 截到了**输出机位**：`outputMediaId` 与 `cover` 换成它；截的是其他机位时这两项不动。
 * 只认 `kind = capture` 的记录（深度图、骨架图不是「这个机位的截图」）。这批一条都没有时原样返回。
 */
export function directorMirrorParams(
  params: JsonObject,
  records: readonly DirectorMediaRecord[],
  scene: DirectorScene,
  options: { shotTextOf?: (cameraId: string) => string | undefined } = {}
): JsonObject {
  const latest = new Map<string, DirectorMediaRecord>();
  for (const record of records) {
    if (record.kind !== "capture" || !record.cameraId) continue;
    const current = latest.get(record.cameraId);
    if (!current || record.createdAt >= current.createdAt) latest.set(record.cameraId, record);
  }
  if (!latest.size) return params;
  const existing = new Map(directorCaptureSummaries(params).map(entry => [entry.cameraId, entry]));
  const captures = camerasOf(scene).flatMap((camera): DirectorCaptureSummary[] => {
    const record = latest.get(camera.id);
    if (!record) {
      const old = existing.get(camera.id);
      return old ? [old] : [];
    }
    const shotText = options.shotTextOf?.(camera.id);
    return [{
      cameraId: camera.id,
      cameraName: record.cameraName ?? camera.name,
      recordId: record.id,
      mediaId: record.mediaId,
      width: record.width,
      height: record.height,
      renderFingerprint: record.renderFingerprint,
      ...(shotText ? { shotText } : {})
    }];
  });
  const next: JsonObject = { ...params, captures };
  const output = scene.outputCameraId ? latest.get(scene.outputCameraId) : undefined;
  if (output) {
    next.outputMediaId = output.mediaId;
    next.cover = directorMediaUrl(output.mediaId);
  }
  return next;
}

export type DirectorPublishedNode = {
  nodeType: "image" | "video";
  title: string;
  params: JsonObject;
  generation: GenerationView;
};

/**
 * 控制图的发布节点注明类型（DR-F-134）：目前接入的模型都不把深度图 / 骨架图当控制条件，下游只当普通参考图用。
 * 截图与预演没有注明（为空串）。
 */
export function directorKindNote(kind: DirectorPublishedKind): string {
  if (kind === "depth") return "深度图（当参考图使用）：近白远黑";
  if (kind === "pose") return "骨架图（当参考图使用）：OpenPose 18 点，被挡住的关节照常画";
  return "";
}

const KIND_TITLES: Record<DirectorPublishedKind, string> = {
  capture: "截图",
  depth: "深度图",
  pose: "骨架图",
  preview: "预演"
};

function generationOf(record: Pick<DirectorMediaRecord, "kind" | "mediaId">): GenerationView {
  return {
    status: "success",
    progress: 100,
    outputType: record.kind === "preview" ? "video" : "image",
    mediaId: record.mediaId,
    outputUrl: directorMediaUrl(record.mediaId)
  };
}

function sourceOf(record: DirectorMediaRecord, kind: DirectorPublishedKind, shotText?: string): DirectorSourceRef {
  return {
    kind,
    sceneId: record.sceneId,
    recordId: record.id,
    mediaId: record.mediaId,
    cameraId: record.cameraId,
    cameraName: record.cameraName,
    renderFingerprint: record.renderFingerprint,
    ...(shotText ? { shotText } : {})
  };
}

/**
 * 一条记录发布到画布后的节点内容（dev/03 §7.2）：截图 / 深度图 / 骨架图建图片节点，预演建视频节点；
 * 标题「{机位名} 截图」，深度图与骨架图注明类型。泼溅文件不发布（返回 null）。
 */
export function directorPublishedNode(record: DirectorMediaRecord, shotText?: string): DirectorPublishedNode | null {
  if (record.kind === "splat") return null;
  const kind = record.kind;
  const camera = record.cameraName?.trim() || "机位";
  return {
    nodeType: kind === "preview" ? "video" : "image",
    title: `${camera} ${KIND_TITLES[kind]}`,
    params: { directorSource: sourceOf(record, kind, shotText) },
    generation: generationOf(record)
  };
}

/**
 * 「重新截图」之后**只替换这个节点**的产出与来源（DR-F-84）：其余参数（用户写的提示词等）原样保留；
 * `generation` 整个换掉（旧的 `previewUrl` 指着旧图，留着会让节点显示旧截图）。
 */
export function directorReplacePublished(
  data: { params: JsonObject; generation?: GenerationView },
  record: DirectorMediaRecord,
  shotText?: string
): { params: JsonObject; generation: GenerationView } {
  const kind: DirectorPublishedKind = record.kind === "splat" ? "capture" : record.kind;
  return {
    params: { ...data.params, directorSource: sourceOf(record, kind, shotText) },
    generation: generationOf(record)
  };
}

/** 「转为普通图片 / 视频节点」：去掉来源，节点恢复普通行为（参数面板重新出现）。其余参数不动。 */
export function directorConvertToPlain(params: JsonObject): JsonObject {
  const { directorSource: _directorSource, ...rest } = params;
  return rest;
}

/** 发布节点的落点：在来源导演台右侧一个身位，纵向依次排开（与预设派生节点同一个横向间距）。 */
export function directorPublishPositions(source: { x: number; y: number }, count: number): Array<{ x: number; y: number }> {
  return Array.from({ length: Math.max(0, count) }, (_, index) => ({
    x: Math.round(source.x + 420),
    y: Math.round(source.y + index * 300)
  }));
}
