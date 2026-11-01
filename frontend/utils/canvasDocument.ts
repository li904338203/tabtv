/**
 * 画布文档「落库形状 ↔ Vue Flow 运行时形状」的互转（AGT-07）。
 *
 * ⚠️ 两头都必须**保留未知字段**（CLAUDE.md §5.4、GO-35）：
 *    存储节点 / 存储连线 / 文档顶层上前端不认识的键，读入时原样收进 `stored` / `extras`，
 *    写出时先铺回去、再写已知键。按固定字段重建的话，服务端写入的 `agent` 元数据
 *    （AGT-10）会被用户的下一次自动保存静默抹掉 —— 而这**不报任何错**。
 *
 * 纯函数、不碰 store，单测见 `canvasDocument.test.ts`（往返逐字一致）。
 */
import type {
  AgentMeta,
  CanvasEdge,
  CanvasNode,
  JsonObject,
  OriginMeta,
  StoredCanvasEdge,
  StoredCanvasNode
} from "~/types/canvas";

/** 存储节点上前端认识的键；其余一律进 `data.stored`。 */
const NODE_KEYS = new Set(["id", "type", "title", "position", "size", "params", "result", "agent", "origin"]);
/**
 * 画框（`type:"frame"`，AGT-11）多认一个 `members`。只对画框认：普通节点上的同名键是未知字段，原样往返。
 * ⚠️ 画框与组节点（group，带执行与计费语义）严格区分，不复用 group。
 */
const FRAME_KEYS = new Set([...NODE_KEYS, "members"]);
export const FRAME_TYPE = "frame";
/** 画框原本没有 params 时的标记（只在内存里，写出时摘掉），保证往返不凭空多出 `params: {}`。 */
const FRAME_NO_PARAMS = "__frameNoParams";

/** 存储连线上前端认识的键；其余一律进 `data.stored`。 */
const EDGE_KEYS = new Set(["id", "source", "sourceHandle", "target", "targetHandle", "order", "operation"]);
/** 文档顶层前端认识的键；其余一律进 `extras`。 */
const DOCUMENT_KEYS = new Set(["schemaVersion", "viewport", "nodes", "edges"]);

function isObject(value: unknown): value is JsonObject {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

function cloneValue<T>(value: T): T {
  return JSON.parse(JSON.stringify(value)) as T;
}

/** 取出 `known` 之外的键（深拷贝）；一个都没有时返回 undefined，免得每个节点都背一个空对象。 */
export function unknownKeys(value: unknown, known: Set<string>): JsonObject | undefined {
  if (!isObject(value)) return undefined;
  const rest: JsonObject = {};
  let found = false;
  for (const [key, item] of Object.entries(value)) {
    if (known.has(key) || item === undefined) continue;
    rest[key] = item;
    found = true;
  }
  return found ? cloneValue(rest) : undefined;
}

/** 文档顶层的未知键。 */
export function documentExtras(value: unknown): JsonObject {
  return unknownKeys(value, DOCUMENT_KEYS) ?? {};
}

/** 只认对象形状的 agent；坏值当没有（不让一个脏字段把整张画布弄得打不开）。 */
function agentOf(value: unknown): AgentMeta | undefined {
  return isObject(value) ? cloneValue(value) as AgentMeta : undefined;
}

/**
 * 同理只认对象形状的 origin（MCPT-09）。⚠️ 原样深拷贝、不改写：里面将来多出的键也要往返；
 * 也**不**把旧的 `agent` 迁成 `origin`（读取统一走 `nodeOrigin`）。
 * ⚠️ 坏值（非对象）不认领，但会作为未知键进 `stored` 原样带回 —— 与 `agent` 不同的是
 *    origin 是新字段，脏值更可能来自将来的形状变化，丢掉它不如留着。
 */
function originOf(value: unknown): OriginMeta | undefined {
  return isObject(value) ? cloneValue(value) as OriginMeta : undefined;
}

/** 来源字段（agent / origin）在运行时 data 上的形状；两个都可能有。 */
function originFields(node: StoredCanvasNode): { agent?: AgentMeta; origin?: OriginMeta } {
  const agent = agentOf(node.agent);
  const origin = originOf(node.origin);
  return { ...(agent ? { agent } : {}), ...(origin ? { origin } : {}) };
}

/** 存储节点上 known 之外的键；origin 不是对象时它不被认领，按未知键原样往返。 */
function storedExtras(node: StoredCanvasNode, known: Set<string>): JsonObject | undefined {
  const rest = unknownKeys(node, known) ?? {};
  if (node.origin !== undefined && !isObject(node.origin)) rest.origin = cloneValue(node.origin);
  return Object.keys(rest).length ? rest : undefined;
}

export type NodeHydrateOptions = {
  /** 节点在文档里的序号，仅用于给缺标题的历史节点补一个默认标题 */
  index: number;
  /** 宽度：由 store 按节点类型的最小宽度算好传进来（规则留在 store，这里不复制一份） */
  width: number;
  params: JsonObject;
};

export function storedToCanvasNode(node: StoredCanvasNode, options: NodeHydrateOptions): CanvasNode {
  if (node.type === FRAME_TYPE) return storedToFrameNode(node, options);
  const stored = storedExtras(node, NODE_KEYS);
  const source = originFields(node);
  return {
    id: node.id,
    type: "aitv",
    position: node.position,
    width: options.width,
    data: {
      nodeType: node.type,
      title: node.title ?? `${node.type} 节点 ${options.index + 1}`,
      params: options.params,
      generation: node.result,
      ...source,
      ...(stored ? { stored } : {})
    }
  };
}

/**
 * 画框 → Vue Flow `type:"frame"`。宽高都取存下来的值（画框的大小就是它框住的范围，
 * 不能像普通节点那样按最小宽度撑开、高度归一成 180）。
 */
function storedToFrameNode(node: StoredCanvasNode, options: NodeHydrateOptions): CanvasNode {
  const stored = storedExtras(node, FRAME_KEYS);
  const source = originFields(node);
  const members = Array.isArray(node.members) ? node.members.filter((item): item is string => typeof item === "string") : [];
  const extra: JsonObject = { ...(stored ?? {}) };
  if (node.params === undefined) extra[FRAME_NO_PARAMS] = true;
  return {
    id: node.id,
    type: "frame",
    position: node.position,
    width: Number(node.size?.w) || options.width,
    height: Number(node.size?.h) || 400,
    // 画在其他节点下面：画框只是视觉容器
    zIndex: -1,
    data: {
      nodeType: FRAME_TYPE,
      title: node.title ?? "画框",
      params: options.params,
      members,
      ...source,
      ...(Object.keys(extra).length ? { stored: extra } : {})
    }
  };
}

/**
 * 运行时节点 → 存储节点。`size` 由调用方给（默认宽度规则在 store 里）。
 *
 * ⚠️ 先铺未知键再写已知键：已知键永远以运行时为准，未知键原样带回。
 * ⚠️ `result` 照旧剥掉 `previewUrl` / `outputText`：它们是 blob URL 与正文缓存，不该落库。
 */
export function canvasNodeToStored(node: CanvasNode, size: { w: number; h: number }): StoredCanvasNode {
  const data = node.data;
  if (data.nodeType === FRAME_TYPE) return frameToStored(node, size);
  const stored: StoredCanvasNode = {
    ...(data.stored ? cloneValue(data.stored) : {}),
    id: node.id,
    type: data.nodeType,
    title: data.title,
    position: cloneValue(node.position),
    size,
    params: cloneValue(data.params),
    result: data.generation
      ? { ...cloneValue(data.generation), previewUrl: undefined, outputText: undefined }
      : undefined
  };
  if (data.agent) stored.agent = cloneValue(data.agent);
  if (data.origin) stored.origin = cloneValue(data.origin);
  return stored;
}

function frameToStored(node: CanvasNode, size: { w: number; h: number }): StoredCanvasNode {
  const data = node.data;
  const { [FRAME_NO_PARAMS]: noParams, ...rest } = data.stored ?? {};
  const stored: StoredCanvasNode = {
    ...cloneValue(rest),
    id: node.id,
    type: FRAME_TYPE,
    title: data.title,
    position: cloneValue(node.position),
    size,
    members: [...(data.members ?? [])]
  };
  // 原本没有 params 且至今没人写过的话保持没有；有内容就照常写出
  if (!(noParams === true && Object.keys(data.params ?? {}).length === 0)) stored.params = cloneValue(data.params);
  if (data.agent) stored.agent = cloneValue(data.agent);
  if (data.origin) stored.origin = cloneValue(data.origin);
  return stored;
}

export function storedToCanvasEdge(edge: StoredCanvasEdge): CanvasEdge {
  const stored = unknownKeys(edge, EDGE_KEYS);
  return {
    id: edge.id,
    source: edge.source,
    sourceHandle: edge.sourceHandle ?? "out",
    target: edge.target,
    targetHandle: edge.targetHandle ?? "in",
    animated: true,
    data: { order: edge.order ?? 1, operation: edge.operation, ...(stored ? { stored } : {}) }
  };
}

export function canvasEdgeToStored(edge: CanvasEdge): StoredCanvasEdge {
  return {
    ...(edge.data?.stored ? cloneValue(edge.data.stored) : {}),
    id: edge.id,
    source: edge.source,
    sourceHandle: edge.sourceHandle ?? "out",
    target: edge.target,
    targetHandle: edge.targetHandle ?? "in",
    order: edge.data?.order ?? 1,
    operation: edge.data?.operation
  };
}
