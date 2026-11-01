/**
 * 成片发布到画布（docs/agent/smartcut SC-F-77、SC-D-08、SCD-51）：纯函数，不碰网络、不碰 store。
 *
 * 发布节点 = 一个视频节点：`params.smartcutSource = {timelineId, renderId, mediaId, timelineVersion}` 记来源，
 * `generation` 是「已有产出」形态（照导演台 `directorPublishedNode`）。
 *
 * ⚠️ `generation` **没有 `id`**：有 `id` 的话下游会按 `generationId` 取输入，而那个 ID 不存在（导出不是生成）——
 *    下游一律按 `mediaId` 声明「参考」（`utils/canvas.generationInputs` 对成功、无 ID 的视频节点就是这么做的）。
 * ⚠️ 来源里的 `mediaId` 与「已有产出」的媒体 ID 必须是同一个：整图运行按 `smartcutSource.mediaId` 取静态源（SCT-09），
 *    下游受理按产出取 —— 两个不一样的话，同一个节点在两条路上是两条片子。
 * ⚠️ 重新导出后旧发布节点**不自动替换**，只在角标上说「剪辑已更新」（SCT-24）：用户可能正拿旧版去生成。
 */
import type { components } from "@tabtv/api-client";
import type { GenerationView, JsonObject } from "~/types/canvas";

type RenderJob = components["schemas"]["RenderJob"];

/** 发布节点 params.smartcutSource（SC-D-08）。 */
export type SmartcutSourceRef = {
  timelineId: string;
  renderId: string;
  mediaId: string;
  timelineVersion: number;
};

export type SmartcutPublishedNode = {
  nodeType: "video";
  title: string;
  params: JsonObject;
  generation: GenerationView;
};

const isId = (value: unknown): value is string => typeof value === "string" && /^\d+$/.test(value);

export function smartcutMediaUrl(mediaId: string): string {
  return `/api/v1/media/${mediaId}`;
}

/** 「已有产出」：成功、100%、视频，媒体 ID 与受保护地址；没有生成 ID。 */
export function smartcutPublishedGeneration(mediaId: string): GenerationView {
  return { status: "success", progress: 100, outputType: "video", mediaId, outputUrl: smartcutMediaUrl(mediaId) };
}

/**
 * 一次成功导出发布到画布后的节点内容。没有产出（还没导完、失败、产出 ID 缺失）时返回 null —— 不建一个裂的节点。
 * 标题「{智能剪辑名} 成片」；版本号放在来源角标上，不写进标题（用户会改标题，角标得一直对）。
 */
export function smartcutPublishedNode(
  render: Pick<RenderJob, "id" | "status" | "outputMediaId" | "timelineVersion">,
  source: { timelineId: string; title?: string }
): SmartcutPublishedNode | null {
  if (render.status !== "success" || !isId(render.outputMediaId) || !isId(source.timelineId)) return null;
  const ref: SmartcutSourceRef = {
    timelineId: source.timelineId,
    renderId: render.id,
    mediaId: render.outputMediaId,
    timelineVersion: render.timelineVersion
  };
  return {
    nodeType: "video",
    title: `${source.title?.trim() || "智能剪辑"} 成片`,
    params: { smartcutSource: ref },
    generation: smartcutPublishedGeneration(ref.mediaId)
  };
}

/** 发布节点上的来源；没有或形状不对时为 null（老数据、手改过的画布不该让节点卡崩掉）。 */
export function smartcutSourceOf(params: JsonObject | null | undefined): SmartcutSourceRef | null {
  const raw = params?.smartcutSource;
  if (!raw || typeof raw !== "object" || Array.isArray(raw)) return null;
  const source = raw as Record<string, unknown>;
  const version = source.timelineVersion;
  if (!isId(source.timelineId) || !isId(source.renderId) || !isId(source.mediaId)) return null;
  if (typeof version !== "number" || !Number.isInteger(version) || version < 1) return null;
  return { timelineId: source.timelineId, renderId: source.renderId, mediaId: source.mediaId, timelineVersion: version };
}

/** 发布节点的落点：在智能剪辑节点右侧一个身位，多次发布纵向依次排开（与导演台发布同一个间距）。 */
export function smartcutPublishPosition(source: { x: number; y: number }, index = 0): { x: number; y: number } {
  return { x: Math.round(source.x + 420), y: Math.round(source.y + Math.max(0, index) * 300) };
}

export type SmartcutSourceBadge = {
  text: string;
  /** 来源智能剪辑已删除（SC-UX-15）：角标不可点。 */
  missing: boolean;
  /** 时间线比发布时新：「剪辑已更新」。 */
  updated: boolean;
};

/**
 * 来源角标：「来自 {智能剪辑名} · v{版本}」；来源节点已删除时「来源智能剪辑已删除」；
 * 时间线比发布时新时补一句「剪辑已更新」。`current` 是来源节点当前的标题与时间线版本，节点不在画布上时传 null。
 */
export function smartcutSourceBadge(
  source: SmartcutSourceRef,
  current: { title?: string; version?: number } | null
): SmartcutSourceBadge {
  if (!current) return { text: "来源智能剪辑已删除", missing: true, updated: false };
  const base = `来自 ${current.title?.trim() || "智能剪辑"} · v${source.timelineVersion}`;
  const updated = typeof current.version === "number" && current.version > source.timelineVersion;
  return { text: updated ? `${base} · 剪辑已更新` : base, missing: false, updated };
}
