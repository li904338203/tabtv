import type { Edge, Node, ViewportTransform } from "@vue-flow/core";

export type JsonObject = Record<string, unknown>;

export type GenerationView = {
  id?: string;
  status?: string;
  progress?: number;
  /**
   * 产物的**工作区级媒体 ID**（产物端点的 `mediaId`）。
   *
   * ⚠️ 复制项目时服务端会把 `id` 解绑成 `sourceGenerationId`（GAP-16：生成 ID 只能在本项目里当输入），
   *    而媒体属于工作区、副本照样能用 —— 下游按它声明「参考」输入。复制只摘 `id`，这个字段跟着副本走。
   */
  mediaId?: string;
  /** 复制项目时服务端把原 `id` 改写到这里（GAP-16）：预览/列产物用它，但**不能**当生成输入。 */
  sourceGenerationId?: string;
  outputUrl?: string;
  outputType?: string;
  outputText?: string;
  previewUrl?: string;
  errorCode?: string;
  error?: string;
};

/**
 * 服务端（Agent，AGT-10）写在存储节点**顶层**的归属元数据。
 * 前端只负责原样往返（以及 AGT-11 的 `userTouched`），不解释其余字段。
 */
export type AgentMeta = {
  sessionId: string;
  stepId?: string;
  sourceDocId?: string;
  userTouched?: boolean;
};

/**
 * MCP（外部 AI 客户端，MCPT-09）建的节点在存储节点**顶层**写的来源字段。
 * 读取一律经 `utils/canvasOrigin.ts nodeOrigin`（与旧的 `agent` 字段统一解析）；
 * 前端只负责原样往返（以及 `userTouched`），未知键随对象一起保留。
 */
export type OriginMeta = {
  kind: string;
  grantId?: string;
  sessionId?: string;
  clientName?: string;
  userTouched?: boolean;
  [key: string]: unknown;
};

export type CanvasNodeData = {
  nodeType: string;
  title: string;
  params: JsonObject;
  modelOverrides?: JsonObject;
  generation?: GenerationView;
  agent?: AgentMeta;
  /** MCP 来源（MCPT-09）；与 `agent` 可能同时存在，解析见 `nodeOrigin`。 */
  origin?: OriginMeta;
  /**
   * 画框（`nodeType === "frame"`，AGT-11）框住的节点 ID。画框是纯视觉容器，
   * 与带执行与计费语义的组节点（group）严格区分；成员位置独立，解散只去掉框。
   */
  members?: string[];
  /**
   * 存储节点上前端**不认识**的键，原样保留、存盘时铺回去（§5.4「更新时保留未知字段」）。
   * ⚠️ 不要在业务代码里读写它：认识了的字段就该显式建模（像 `agent` 那样）。
   */
  stored?: JsonObject;
};

export type CanvasEdgeData = {
  order: number;
  operation?: string;
  /** 存储连线上前端不认识的键，原样往返（同 `CanvasNodeData.stored`）。 */
  stored?: JsonObject;
};

/** Vue Flow 节点类型：普通节点一律 `aitv`（按 `data.nodeType` 再分），画框是 `frame`（AGT-11）。 */
export type CanvasFlowNodeType = "aitv" | "frame";

export type CanvasNode = Omit<Node<CanvasNodeData, Record<string, never>, CanvasFlowNodeType>, "data"> & {
  data: CanvasNodeData;
};
export type CanvasEdge = Edge<CanvasEdgeData>;

export type StoredCanvasNode = {
  id: string;
  type: string;
  title?: string;
  position: { x: number; y: number };
  size?: { w: number; h: number };
  params?: JsonObject;
  result?: GenerationView;
  agent?: AgentMeta;
  origin?: OriginMeta;
  /** 未知字段：存储形状是开放的，前端不认识的键也要原样带回。 */
  [key: string]: unknown;
};

export type StoredCanvasEdge = {
  id: string;
  source: string;
  sourceHandle?: string;
  target: string;
  targetHandle?: string;
  order?: number;
  operation?: string;
  [key: string]: unknown;
};

export type CanvasDocument = {
  schemaVersion: number;
  viewport: ViewportTransform;
  nodes: StoredCanvasNode[];
  edges: StoredCanvasEdge[];
  /** 文档顶层的未知字段，同样原样往返。 */
  [key: string]: unknown;
};

export type CanvasSnapshot = {
  nodes: CanvasNode[];
  edges: CanvasEdge[];
};

// ---------------------------------------------------------------- 画布操作（AGT-10 / AGT-11）

export type CanvasOpKind = "add_node" | "update_node" | "move_node" | "add_edge" | "add_frame" | "set_frame_members" | "link_generation";

/**
 * 一条**已解析**的画布操作（契约 `CanvasOp`、事件 `canvas.ops_applied.ops`）：
 * 批内引用已换成真实 ID，add_* 带着最终落库的完整节点 / 连线 / 画框。前端只重放、不分配 ID。
 */
export type CanvasOp = {
  kind: CanvasOpKind | string;
  nodeId?: string;
  node?: JsonObject;
  patch?: JsonObject;
  edge?: JsonObject;
  frame?: JsonObject;
  position?: { x: number; y: number };
};

/**
 * 重放时的操作者；`sessionId` 只在 Agent 写入时有值、`grantId` 只在 MCP 写入时有值（所有权按它们判）。
 * `clientName` 只在事件里带（补取接口为 null），展示时回落到节点上的 `origin.clientName`。
 */
export type OpActor = {
  kind: "agent" | "user" | "mcp";
  userId?: string | null;
  sessionId?: string | null;
  grantId?: string | null;
  clientName?: string | null;
};

/** 一批服务端操作（来自事件或 `GET /projects/{id}/canvas/ops`）。 */
export type OpBatch = {
  /** 事件带、补取接口不带（补取本来就按项目查） */
  projectId?: string;
  batchId: string;
  fromVersion: number;
  toVersion: number;
  /** `truncated` 时事件不带 ops，必须走补取 */
  ops?: CanvasOp[];
  truncated?: boolean;
  actor: OpActor;
};

/**
 * 撤销一批远端操作用的逆操作（纯本地，不发给服务端）。由 `utils/canvasOps.inverseOf` 从
 * 批次前后两份文档算出来：删掉新增的、还原被改的**字段**（不是整节点回滚 ——
 * 那会把用户之后对同一节点的修改一并抹掉）。
 */
export type InverseOp =
  | { kind: "remove_node"; nodeId: string }
  | { kind: "remove_edge"; edgeId: string }
  | { kind: "restore_node"; node: StoredCanvasNode }
  | { kind: "restore_edge"; edge: StoredCanvasEdge }
  | {
    kind: "revert_fields";
    nodeId: string;
    set: JsonObject;
    unset: string[];
    params?: { set: JsonObject; unset: string[] };
  };

/** 撤销栈条目：普通整份快照，或一个远端批次（Agent 的一批操作 = 一步撤销）。 */
export type SnapshotUndoEntry = CanvasSnapshot & { kind: "snapshot" };
export type RemoteUndoEntry = { kind: "remote"; batchId: string; nodeIds: string[]; inverse: InverseOp[] };
export type UndoEntry = SnapshotUndoEntry | RemoteUndoEntry;
