/**
 * 素材条解析（docs/agent/smartcut SC-F-10~18；与 Go 的 `canvas/domain.InboundSources` + 素材解析同一套规则，SCT-25）。
 *
 * 画布（存储形状：节点 `type` / `params` / `result`，连线 `order`）→ 智能剪辑节点的素材条：编号、类型、状态。
 * - **连线顺序即素材编号**：入边按 `order` 升序（相同按源节点 ID，与 Go 的 `Outline` 一致），从 1 编起；
 *   跳过触及文档 / 画框节点的连线与悬空边（不占编号）。
 * - **一次出多个产物**的上游按产物顺序拆成 `3-1`、`3-2`……，单个上游最多取前 9 个（SC-PARAM-20 / SCK-20）；
 *   素材条总共最多 50 条，超出的不进素材条、只报个数（SC-F-17）。
 * - 生成节点以画布上挂着的那次生成为准；`result` 只是 UI 快照（CLAUDE.md §5.4），真实状态与全部产物由
 *   `lookup` 提供（服务端回查 generation 表的结果）。不给 `lookup` 时退回快照：只能出一条，状态照快照。
 * - 没有生成 ID、但有媒体 ID 的（上传节点、复制出来的节点、成片 / 导演台发布节点）按媒体声明；
 *   归属与平台素材（`workspace_id = 0`）只有服务端判得了，经 `lookup.media` 报 `invalid`。
 *
 * ⚠️ 这里的结果只用于连线后的即时显示；素材条以 `GET .../inputs` 为准。规则改动先改
 *    `api/fixtures/smartcut/inputs/`（SCR-11），两边一起过。
 */
import type { components } from "@tabtv/api-client";

type SmartcutInput = components["schemas"]["SmartcutInput"];
type InputKind = SmartcutInput["kind"];
type AudioRole = NonNullable<SmartcutInput["audioRole"]>;

/** 存储形状的画布节点（`CanvasDocument.nodes` 的子集）。 */
export type SmartcutCanvasNode = {
  id: string;
  type: string;
  title?: string;
  params?: Record<string, unknown> | null;
  result?: {
    id?: string;
    status?: string;
    mediaId?: string;
    outputUrl?: string;
    outputType?: string;
    outputText?: string;
    error?: string;
    sourceGenerationId?: string;
  } | null;
};

/** 存储形状的连线（`CanvasDocument.edges` 的子集）。 */
export type SmartcutCanvasEdge = { id?: string; source: string; target: string; order?: number; operation?: string };

export type SmartcutCanvasDoc = { nodes?: readonly SmartcutCanvasNode[]; edges?: readonly SmartcutCanvasEdge[] };

/** 一条入边的上游（与 Go 的 `InboundSource` 对应）。 */
export type SmartcutInboundSource = {
  order: number;
  node: SmartcutCanvasNode;
};

/** 服务端回查得到的事实（fixture 与 store 都可以给）。 */
export type SmartcutInputsLookup = {
  /** 生成的真实状态与全部产物（按 idx）；`invalid` = 不是本项目的生成 / 归属不合法。查不到返回 undefined。 */
  generation?: (generationId: string) => {
    status: string;
    outputs?: ReadonlyArray<{ mediaId: string; durationMs?: number; hasAudio?: boolean; posterMediaId?: string }>;
    operation?: string;
    reason?: string;
  } | undefined;
  /** 媒体的归属与元数据；`invalid` = 别的工作区或平台素材。查不到返回 undefined（按快照放行）。 */
  media?: (mediaId: string) => {
    status: "ready" | "invalid";
    reason?: string;
    durationMs?: number;
    hasAudio?: boolean;
    posterMediaId?: string;
  } | undefined;
};

/** 单个上游最多取的产物数、素材条总条数（SC-PARAM-20）。 */
export const smartcutInputLimits = { outputsPerSource: 9, items: 50, textChars: 2000 } as const;

/** 连线不认这两类节点：文档与画框不是素材（与 Go 的 `InboundSources` 一致）。 */
const SKIPPED_NODE_TYPES = new Set(["document", "frame"]);

const GENERATION_FAILED = new Set(["failed", "cancelled"]);

const digits = (value: unknown): string => (typeof value === "string" && /^\d+$/.test(value) ? value : "");

function paramsOf(node: SmartcutCanvasNode): Record<string, unknown> {
  return node.params && typeof node.params === "object" ? node.params : {};
}

/** 产物的媒体 ID：结果里存了就用；没存的从 `outputUrl`（`/api/v1/media/{id}`）取（同 `utils/canvas.outputMediaIdOf`）。 */
function resultMediaId(node: SmartcutCanvasNode): string {
  const result = node.result;
  if (!result) return "";
  if (digits(result.mediaId)) return result.mediaId!;
  return /^\/api\/v1\/media\/(\d+)(?:[?#].*)?$/.exec(result.outputUrl ?? "")?.[1] ?? "";
}

/** 发布节点上的静态来源：导演台发布（`directorSource`）与成片发布（`smartcutSource`）。 */
function staticMediaId(node: SmartcutCanvasNode): string {
  const params = paramsOf(node);
  for (const key of ["directorSource", "smartcutSource"]) {
    const source = params[key];
    if (source && typeof source === "object" && !Array.isArray(source)) {
      const id = digits((source as Record<string, unknown>).mediaId);
      if (id) return id;
    }
  }
  return "";
}

/** 指向某个节点的入边，按 `order` 升序（相同按源节点 ID）；跳过触及文档 / 画框的连线与悬空边。 */
function inboundEdges(doc: SmartcutCanvasDoc, nodeId: string): Array<{ edge: SmartcutCanvasEdge; order: number; node: SmartcutCanvasNode }> {
  const nodes = new Map((doc.nodes ?? []).map(node => [node.id, node]));
  return (doc.edges ?? [])
    .filter(edge => edge.target === nodeId && edge.source)
    .map(edge => ({ edge, order: typeof edge.order === "number" ? edge.order : 0, node: nodes.get(edge.source) }))
    .filter((item): item is { edge: SmartcutCanvasEdge; order: number; node: SmartcutCanvasNode } =>
      !!item.node && !SKIPPED_NODE_TYPES.has(item.node.type))
    .sort((a, b) => a.order - b.order || (a.node.id < b.node.id ? -1 : a.node.id > b.node.id ? 1 : 0));
}

/**
 * 智能剪辑节点的入边，按 `order` 升序（相同按源节点 ID）。跳过触及文档 / 画框的连线与悬空边。
 */
export function smartcutInboundSources(doc: SmartcutCanvasDoc, nodeId: string): SmartcutInboundSource[] {
  return inboundEdges(doc, nodeId).map(({ order, node }) => ({ order, node }));
}

/** 节点作为素材时的类型；不认的类型为 null。导演台给的是截图（图片），脚本与视频组是镜头组。 */
export function smartcutInputKindOf(node: Pick<SmartcutCanvasNode, "type" | "params">): InputKind | null {
  switch (node.type) {
    case "video":
    case "image":
    case "audio":
    case "text":
      return node.type;
    case "director":
      return "image";
    case "script":
      return "shotGroup";
    case "group":
      return paramsOf(node as SmartcutCanvasNode).groupType === "video_generator" ? "shotGroup" : null;
    default:
      return null;
  }
}

/**
 * 音频的默认用途（SC-F-11）：来自文字转语音（`text2audio`）的默认人声，其余（上传、配乐、音效）默认配乐。
 * 操作码以服务端回查为准；没有时按画布推断：有生成的音频节点，取它第一条入边的操作，缺省就是 `text2audio`
 * （与 `utils/canvas.operationForNode` 的默认一致）。
 */
export function smartcutDefaultAudioRole(doc: SmartcutCanvasDoc, node: SmartcutCanvasNode, operation?: string): AudioRole {
  if (operation) return operation === "text2audio" ? "voice" : "music";
  const generated = Boolean(node.result?.id || node.result?.sourceGenerationId);
  if (!generated) return "music";
  const first = inboundEdges(doc, node.id)[0];
  return (first?.edge.operation || "text2audio") === "text2audio" ? "voice" : "music";
}

function truncateText(text: string): string {
  const chars = [...text];
  return chars.length > smartcutInputLimits.textChars ? chars.slice(0, smartcutInputLimits.textChars).join("") : text;
}

type Draft = Omit<SmartcutInput, "no" | "used">;

/** 一个上游 → 一条或多条素材（多产物时多条）。 */
function resolveSource(doc: SmartcutCanvasDoc, node: SmartcutCanvasNode, lookup: SmartcutInputsLookup): Draft[] {
  const params = paramsOf(node);
  const base = { sourceNodeId: node.id, sourceNodeType: node.type, ...(node.title ? { title: node.title } : {}) };
  const kind = smartcutInputKindOf(node);
  if (!kind) {
    const outputType = node.result?.outputType;
    const fallback: InputKind = outputType === "image" || outputType === "audio" || outputType === "text" ? outputType : "video";
    return [{ ...base, kind: fallback, status: "invalid", reason: `不支持的输入：${node.type}` }];
  }

  if (kind === "shotGroup") {
    const id = node.type === "script" ? String(params.scriptId ?? "") : String(params.groupId ?? "");
    if (id) return [{ ...base, kind, status: "ready" }];
    return [{ ...base, kind, status: "pending", reason: node.type === "script" ? "脚本还没有分镜" : "视频组还没建好" }];
  }

  const resultId = typeof node.result?.id === "string" ? node.result.id : "";
  const snapshotStatus = node.result?.status ?? "";

  if (kind === "text") {
    // 有成功的生成时，下游拿到的是生成的产出（与 `utils/canvas.generationInputs` 一致）；否则取节点上写的文字。
    const outputText = node.result?.outputText?.trim() ?? "";
    if (resultId && snapshotStatus === "success" && outputText) return [{ ...base, kind, status: "ready", text: truncateText(outputText) }];
    const value = params.text ?? params.prompt;
    if (typeof value === "string" && value.trim()) return [{ ...base, kind, status: "ready", text: truncateText(value.trim()) }];
    if (resultId && GENERATION_FAILED.has(snapshotStatus)) return [{ ...base, kind, status: "failed", reason: node.result?.error || "上游生成失败" }];
    return [{ ...base, kind, status: "pending", reason: resultId ? "上游还在生成" : "文本还是空的" }];
  }

  const role = (operation?: string) => (kind === "audio" ? { audioRole: smartcutDefaultAudioRole(doc, node, operation) } : {});

  // 导演台节点：给下游的是输出机位的最新截图（params.outputMediaId）。
  if (node.type === "director") {
    const id = digits(params.outputMediaId);
    return id ? [withMedia({ ...base, kind, status: "ready", mediaId: id }, lookup)] : [{ ...base, kind, status: "pending", reason: "请先在导演台截图" }];
  }

  if (resultId) {
    const fact = lookup.generation?.(resultId);
    const status = fact?.status ?? snapshotStatus;
    if (status === "invalid") return [{ ...base, kind, status: "invalid", reason: fact?.reason || "这个生成不属于本项目" }];
    if (GENERATION_FAILED.has(status)) return [{ ...base, kind, status: "failed", reason: fact?.reason || node.result?.error || "上游生成失败" }];
    // 排队、运行中与认不出的状态一律当「还在生成」：认不出的状态不能当成功，也不该当失败去吓用户。
    if (status !== "success") return [{ ...base, kind, status: "pending", reason: "上游还在生成" }];
    if (fact?.outputs && fact.outputs.length === 0) return [{ ...base, kind, status: "failed", reason: "上游任务完成但没有产物" }];
    const outputs = fact?.outputs ?? (resultMediaId(node) ? [{ mediaId: resultMediaId(node) }] : []);
    if (outputs.length === 0) return [{ ...base, kind, status: "ready", ...role(fact?.operation) }];
    return outputs.slice(0, smartcutInputLimits.outputsPerSource).map(output => ({
      ...base,
      kind,
      status: "ready" as const,
      mediaId: output.mediaId,
      ...(output.durationMs !== undefined ? { durationMs: output.durationMs } : {}),
      ...(output.hasAudio !== undefined ? { hasAudio: output.hasAudio } : {}),
      ...(output.posterMediaId ? { posterMediaId: output.posterMediaId } : {}),
      ...role(fact?.operation)
    }));
  }

  // 没有生成 ID：上传节点、复制出来的节点、发布节点 —— 按媒体声明。
  const mediaId = (!snapshotStatus || snapshotStatus === "success" ? resultMediaId(node) : "") || staticMediaId(node);
  if (mediaId) return [withMedia({ ...base, kind, status: "ready", mediaId, ...role() }, lookup)];
  if (snapshotStatus && GENERATION_FAILED.has(snapshotStatus)) return [{ ...base, kind, status: "failed", reason: node.result?.error || "上传失败" }];
  return [{ ...base, kind, status: "pending", reason: snapshotStatus ? "还在上传" : "还没有产出" }];
}

/** 按媒体声明的素材：服务端说归属不合法时记 invalid，不进轨。 */
function withMedia(draft: Draft, lookup: SmartcutInputsLookup): Draft {
  const fact = draft.mediaId ? lookup.media?.(draft.mediaId) : undefined;
  if (!fact) return draft;
  if (fact.status === "invalid") {
    const { audioRole: _role, ...rest } = draft;
    return { ...rest, status: "invalid", reason: fact.reason || "素材不属于本工作区" };
  }
  return {
    ...draft,
    ...(fact.durationMs !== undefined ? { durationMs: fact.durationMs } : {}),
    ...(fact.hasAudio !== undefined ? { hasAudio: fact.hasAudio } : {}),
    ...(fact.posterMediaId ? { posterMediaId: fact.posterMediaId } : {})
  };
}

/** 时间线里用到素材的片段（判 `used`）。 */
export type SmartcutUsedClip = { sourceNodeId?: string | null; mediaId?: string };

/**
 * 素材条：编号、类型、状态（`SmartcutInput` 的形状）。`omitted` 是超出 50 条、没进素材条的条数（要说出来）。
 */
export function smartcutResolveInputs(
  doc: SmartcutCanvasDoc,
  nodeId: string,
  options: { lookup?: SmartcutInputsLookup; clips?: readonly SmartcutUsedClip[] } = {}
): { items: SmartcutInput[]; omitted: number } {
  const lookup = options.lookup ?? {};
  const clips = options.clips ?? [];
  const all: SmartcutInput[] = [];
  smartcutInboundSources(doc, nodeId).forEach((source, index) => {
    const drafts = resolveSource(doc, source.node, lookup);
    drafts.forEach((draft, k) => {
      const no = drafts.length > 1 ? `${index + 1}-${k + 1}` : String(index + 1);
      const used = clips.some(clip => clip.sourceNodeId === draft.sourceNodeId && (!draft.mediaId || clip.mediaId === draft.mediaId));
      all.push({ no, ...draft, used });
    });
  });
  return { items: all.slice(0, smartcutInputLimits.items), omitted: Math.max(0, all.length - smartcutInputLimits.items) };
}

/**
 * 「来源已断开」的片段（SC-F-15）：片段记着来源节点，而那个节点已不在入边里。片段本身保留、不删。
 * 没有来源节点的片段（剪辑台里直接加的）不算。
 */
export function smartcutDisconnectedClipIds(
  tracks: ReadonlyArray<{ clips?: ReadonlyArray<{ id: string; sourceNodeId?: string | null }> }>,
  inbound: readonly SmartcutInboundSource[]
): string[] {
  const connected = new Set(inbound.map(source => source.node.id));
  return tracks.flatMap(track => (track.clips ?? [])
    .filter(clip => clip.sourceNodeId && !connected.has(clip.sourceNodeId))
    .map(clip => clip.id));
}

/** 「有 N 段新素材未使用」（SC-F-14）：就绪、能上轨（视频 / 图片 / 音频）、还没被用到的条数。 */
export function smartcutUnusedCount(items: readonly Pick<SmartcutInput, "kind" | "status" | "used">[]): number {
  return items.filter(item => item.status === "ready" && !item.used && (item.kind === "video" || item.kind === "image" || item.kind === "audio")).length;
}
