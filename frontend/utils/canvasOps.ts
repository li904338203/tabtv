/**
 * 画布操作的前端重放（AGT-11）。
 *
 * 服务端（Agent）在最新版本上应用一批操作后推 `canvas.ops_applied`；打开着的画布据此
 * **本地重放**而不是整页重载（重载会闪、会丢本地未保存的改动）。
 *
 * ⚠️ 与 Go `canvas/domain.Apply` 是两份实现，**只靠共享 fixture 保持一致**
 *    （`api/fixtures/canvas-ops/`，Go 与 Vitest 读同一目录）。任何语义改动先改 fixture，再改两侧。
 * ⚠️ 写的是**落库形状**（扁平节点 `{id,type,title,position,size,params,result,agent,origin}`），
 *    不是 Vue Flow 运行时形状；store 再把差异增量合进 `nodes` / `edges`。
 * ⚠️ 只重放**已解析**的操作（`applied`）：ID 已由服务端分配、批内引用已替换。
 *    前端不分配 ID、不认识 `ref:xxx`。
 *
 * 纯函数：不碰 store、不发网络，输入文档永远不被修改。
 */
import type {
  CanvasOp,
  InverseOp,
  JsonObject,
  OpActor,
  StoredCanvasEdge,
  StoredCanvasNode
} from "~/types/canvas";
import { createdByActor, isUserTouched, ownedByActor } from "./canvasOrigin";

export const FRAME_NODE_TYPE = "frame";

export type ApplyOpsError = "not_owned" | "node_not_found" | "invalid";

export type ApplyOpsOptions = {
  /**
   * 远端重放用的宽松模式：这批操作**服务端已经接受**，本地与服务端的差别只可能来自
   * 本地还没保存的改动。此时
   *  - 作用于本地已删除节点的操作 → 跳过（用户的删除随下一次保存生效）；
   *  - 作用于本地已被用户改过（`agent.userTouched` / `origin.userTouched`）节点的 update / move → 跳过
   *    （用户的改动优先，这正是「用户改过 Agent / MCP 就不再改它」）；
   *  - 已存在的 ID 再次新增 → 跳过（重复事件）。
   * 其余所有权规则不再检查。严格模式（缺省）与 Go 同语义，任何一条不满足整批拒绝。
   */
  lenient?: boolean;
};

/** 本批挂到节点上的生成任务（link_generation）：重放之后由 generation store 接上跟踪。 */
export type LinkedGeneration = { nodeId: string; generationId: string };

export type ApplyOpsResult = {
  /** 新文档；出错时就是传进来的那份（未修改） */
  doc: JsonObject;
  /** 本批新建的节点 ID（含画框，按创建顺序） */
  nodeIds: string[];
  /** 本批真正改了节点 result 的 link_generation（同一任务已挂着、节点本地已删的不在内） */
  linked: LinkedGeneration[];
  /** 宽松模式下被跳过的操作条数 */
  skipped: number;
  error?: ApplyOpsError;
  message?: string;
};

type Doc = JsonObject & { nodes?: unknown; edges?: unknown };

function isObject(value: unknown): value is JsonObject {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

function clone<T>(value: T): T {
  return JSON.parse(JSON.stringify(value)) as T;
}

class OpFailure extends Error {
  constructor(readonly code: ApplyOpsError, message: string) {
    super(message);
  }
}

const fail = (code: ApplyOpsError, message: string): never => {
  throw new OpFailure(code, message);
};

/** RFC 7386 JSON Merge Patch：对象逐键递归合并，null 删键，其余替换（与 Go mergePatch 一致）。 */
export function mergePatch(target: unknown, patch: unknown): unknown {
  if (!isObject(patch)) return clone(patch);
  const out: JsonObject = isObject(target) ? { ...target } : {};
  for (const [key, value] of Object.entries(patch)) {
    if (value === null) Reflect.deleteProperty(out, key);
    else out[key] = mergePatch(out[key], value);
  }
  return out;
}

/**
 * 节点是否归某个 Agent 会话所有（与 Go OwnedBy 一致）。是 `ownedByActor` 的 agent 特例，保留给既有调用点；
 * 来源字段的解析只在 `utils/canvasOrigin.ts`（MCPT-09）。
 */
export function ownedBy(node: JsonObject, sessionId: string | null | undefined): boolean {
  return ownedByActor(node, { kind: "agent", sessionId });
}

function userTouched(node: JsonObject): boolean {
  return isUserTouched(node);
}

class ReplayState {
  readonly nodes: JsonObject[];
  edges: JsonObject[] | undefined;
  readonly byId = new Map<string, JsonObject>();
  readonly created: string[] = [];
  readonly linked: LinkedGeneration[] = [];
  skipped = 0;

  constructor(readonly doc: Doc, readonly actor: OpActor, readonly lenient: boolean) {
    if (doc.nodes !== undefined && doc.nodes !== null && !Array.isArray(doc.nodes)) fail("invalid", "nodes 必须是数组");
    if (doc.edges !== undefined && doc.edges !== null && !Array.isArray(doc.edges)) fail("invalid", "edges 必须是数组");
    this.nodes = Array.isArray(doc.nodes) ? doc.nodes as JsonObject[] : [];
    this.edges = Array.isArray(doc.edges) ? doc.edges as JsonObject[] : undefined;
    for (const node of this.nodes) {
      if (!isObject(node)) fail("invalid", "节点必须是对象");
      if (typeof node.id === "string" && node.id) this.byId.set(node.id, node);
    }
  }

  apply(op: CanvasOp) {
    switch (op.kind) {
      case "add_node": return this.addNode(op.node, false);
      case "add_frame": return this.addNode(op.frame, true);
      case "update_node": return this.updateNode(op);
      case "move_node": return this.moveNode(op);
      case "add_edge": return this.addEdge(op);
      case "set_frame_members": return this.setFrameMembers(op);
      case "link_generation": return this.linkGeneration(op);
      default: return fail("invalid", `未知的操作种类 ${String(op.kind)}`);
    }
  }

  private skip() {
    this.skipped += 1;
  }

  private addNode(raw: unknown, frame: boolean) {
    if (!isObject(raw)) return fail("invalid", frame ? "缺少 frame" : "缺少 node");
    const node = clone(raw);
    const id = typeof node.id === "string" ? node.id : "";
    if (!id) return fail("invalid", "重放的新建项必须带服务端分配的 id");
    if (frame) node.type = FRAME_NODE_TYPE;
    else if (typeof node.type !== "string" || !node.type) return fail("invalid", "node.type 必填");
    if (this.byId.has(id)) {
      if (this.lenient) return this.skip();
      return fail("invalid", `节点 ID 重复: ${id}`);
    }
    this.nodes.push(node);
    this.byId.set(id, node);
    this.created.push(id);
  }

  /** 解析被操作的节点；`requireOwned` 时在严格模式下校验所有权。返回 undefined 表示宽松模式下跳过。 */
  private target(nodeId: string | undefined, requireOwned: boolean): JsonObject | undefined {
    const node = nodeId ? this.byId.get(nodeId) : undefined;
    if (!node) {
      if (this.lenient) return undefined;
      return fail("node_not_found", `节点不存在: ${nodeId ?? ""}`);
    }
    if (this.lenient) return node;
    // user 不受所有权限制；agent / mcp 只能改本主体建且用户没动过的节点（两者互不可改）
    if (requireOwned && this.actor.kind !== "user" && !ownedByActor(node, this.actor)) {
      return fail("not_owned", `不能修改非本会话 / 本授权创建或用户改过的节点: ${nodeId}`);
    }
    return node;
  }

  private updateNode(op: CanvasOp) {
    const node = this.target(op.nodeId, true);
    if (!node || (this.lenient && userTouched(node))) return this.skip();
    const patch = op.patch;
    if (!isObject(patch) || Object.keys(patch).length === 0) return fail("invalid", "patch 为空");
    for (const [key, value] of Object.entries(patch)) {
      if (key === "title") {
        if (typeof value !== "string") return fail("invalid", "patch.title 必须是字符串");
      } else if (key === "params") {
        if (!isObject(value)) return fail("invalid", "patch.params 必须是对象");
      } else {
        return fail("invalid", `update_node 只允许修改 title 与 params，不允许 ${key}`);
      }
    }
    if ("title" in patch) node.title = patch.title;
    if ("params" in patch) node.params = mergePatch(node.params, patch.params);
  }

  private moveNode(op: CanvasOp) {
    const node = this.target(op.nodeId, true);
    if (!node || (this.lenient && userTouched(node))) return this.skip();
    const position = op.position;
    if (!position || typeof position.x !== "number" || typeof position.y !== "number") {
      return fail("invalid", "move_node 缺少 position");
    }
    node.position = { x: position.x, y: position.y };
  }

  private addEdge(op: CanvasOp) {
    if (!isObject(op.edge)) return fail("invalid", "缺少 edge");
    const edge = clone(op.edge);
    const id = typeof edge.id === "string" ? edge.id : "";
    if (!id) return fail("invalid", "重放的连线必须带服务端分配的 id");
    if (this.edges?.some(item => item.id === id)) {
      if (this.lenient) return this.skip();
      return fail("invalid", `连线 ID 重复: ${id}`);
    }
    const source = typeof edge.source === "string" ? edge.source : "";
    const target = typeof edge.target === "string" ? edge.target : "";
    if (!source || !target) return fail("invalid", "edge 缺少 source 或 target");
    const sourceNode = this.target(source, false);
    // 连线**进入**一个节点等于改了它的输入，所以目标要过所有权（与 Go 一致）
    const targetNode = this.target(target, true);
    if (!sourceNode || !targetNode) return this.skip();
    if (sourceNode.type === FRAME_NODE_TYPE || targetNode.type === FRAME_NODE_TYPE) {
      return fail("invalid", "画框是纯视觉容器，不能连线");
    }
    if (typeof edge.sourceHandle !== "string" || !edge.sourceHandle) edge.sourceHandle = "out";
    if (typeof edge.targetHandle !== "string" || !edge.targetHandle) edge.targetHandle = "in";
    if (edge.order === undefined || edge.order === null) edge.order = this.nextOrder(target);
    if (!this.edges) this.edges = [];
    this.edges.push(edge);
  }

  /**
   * 把已受理的生成挂到节点上（与 Go `linkGeneration` 同语义）：result 整体替换为
   * `{id, status:"queued", progress:0}`；同一个 generationId 已挂着时不动。
   *
   * ⚠️ 只看来源主体（会话 / 授权）、不看 userTouched：它只写「这里有一次生成」这个事实，不碰用户改过的内容；
   *    宽松模式下也照样重放（跳过的话节点一直显示成空的，而任务已经扣了钱）。
   */
  private linkGeneration(op: CanvasOp) {
    const node = op.nodeId ? this.byId.get(op.nodeId) : undefined;
    if (!node) {
      if (this.lenient) return this.skip();
      return fail("node_not_found", `节点不存在: ${op.nodeId ?? ""}`);
    }
    if (!this.lenient && this.actor.kind !== "user" && !createdByActor(node, this.actor)) {
      return fail("not_owned", `只能把任务挂到本会话 / 本授权创建的节点上: ${op.nodeId}`);
    }
    if (node.type === FRAME_NODE_TYPE) return fail("invalid", "画框不能挂生成任务");
    const patch = op.patch;
    if (!isObject(patch)) return fail("invalid", "缺少 patch");
    for (const key of Object.keys(patch)) {
      if (key !== "generationId") return fail("invalid", `link_generation 只允许 generationId，不允许 ${key}`);
    }
    const generationId = patch.generationId;
    if (typeof generationId !== "string" || !/^[1-9]\d{0,18}$/.test(generationId)) {
      return fail("invalid", "patch.generationId 必须是字符串形式的正整数");
    }
    if (isObject(node.result) && node.result.id === generationId) return;
    node.result = { id: generationId, status: "queued", progress: 0 };
    this.linked.push({ nodeId: String(node.id), generationId });
  }

  private setFrameMembers(op: CanvasOp) {
    const node = this.target(op.nodeId, true);
    if (!node) return this.skip();
    if (node.type !== FRAME_NODE_TYPE) return fail("invalid", `节点 ${op.nodeId} 不是画框`);
    const members = isObject(op.frame) ? op.frame.members : undefined;
    if (members !== undefined && members !== null && !Array.isArray(members)) return fail("invalid", "members 必须是数组");
    const out: string[] = [];
    for (const member of (members ?? []) as unknown[]) {
      if (typeof member !== "string" || !member) return fail("invalid", "members 的每一项必须是节点 ID 字符串");
      if (!this.byId.has(member)) {
        // 宽松模式：本地已删的成员不再框住它（画框只是视觉容器）
        if (this.lenient) continue;
        return fail("node_not_found", `节点不存在: ${member}`);
      }
      if (!out.includes(member)) out.push(member);
    }
    node.members = out;
  }

  private nextOrder(target: string): number {
    let max = 0;
    for (const edge of this.edges ?? []) {
      if (edge.target !== target) continue;
      const order = Number(edge.order);
      if (Number.isInteger(order) && order > max) max = order;
    }
    return max + 1;
  }
}

/**
 * 在存储形状的文档上按序应用一批已解析的操作。一批要么全部生效、要么一个都不生效：
 * 出错时返回原文档（未修改）与错误种类。
 */
export function applyOps(doc: unknown, ops: CanvasOp[], actor: OpActor, options: ApplyOpsOptions = {}): ApplyOpsResult {
  const original = isObject(doc) ? doc : {};
  if (!Array.isArray(ops) || ops.length === 0) {
    return { doc: original, nodeIds: [], linked: [], skipped: 0, error: "invalid", message: "操作为空" };
  }
  try {
    const next = clone(original) as Doc;
    const state = new ReplayState(next, actor, options.lenient === true);
    for (const op of ops) {
      if (!isObject(op)) fail("invalid", "操作必须是对象");
      state.apply(op);
    }
    next.nodes = state.nodes;
    if (state.edges) next.edges = state.edges;
    return { doc: next, nodeIds: state.created, linked: state.linked, skipped: state.skipped };
  } catch (cause) {
    if (cause instanceof OpFailure) return { doc: original, nodeIds: [], linked: [], skipped: 0, error: cause.code, message: cause.message };
    throw cause;
  }
}

// ---------------------------------------------------------------- 差异与逆操作

function nodeList(doc: unknown): StoredCanvasNode[] {
  return isObject(doc) && Array.isArray(doc.nodes) ? doc.nodes as StoredCanvasNode[] : [];
}

function edgeList(doc: unknown): StoredCanvasEdge[] {
  return isObject(doc) && Array.isArray(doc.edges) ? doc.edges as StoredCanvasEdge[] : [];
}

function same(a: unknown, b: unknown): boolean {
  return JSON.stringify(a) === JSON.stringify(b);
}

function byId<T extends { id: string }>(items: T[]): Map<string, T> {
  return new Map(items.filter(item => item && typeof item.id === "string").map(item => [item.id, item]));
}

/** 两份文档之间节点 / 连线的增删改（按 ID；改 = 序列化后不同）。 */
export type DocumentDiff = {
  addedNodes: StoredCanvasNode[];
  removedNodeIds: string[];
  changedNodes: StoredCanvasNode[];
  addedEdges: StoredCanvasEdge[];
  removedEdgeIds: string[];
  changedEdges: StoredCanvasEdge[];
};

export function diffDocuments(before: unknown, after: unknown): DocumentDiff {
  const beforeNodes = byId(nodeList(before));
  const afterNodes = byId(nodeList(after));
  const beforeEdges = byId(edgeList(before));
  const afterEdges = byId(edgeList(after));
  const diff: DocumentDiff = { addedNodes: [], removedNodeIds: [], changedNodes: [], addedEdges: [], removedEdgeIds: [], changedEdges: [] };
  for (const [id, node] of afterNodes) {
    const prev = beforeNodes.get(id);
    if (!prev) diff.addedNodes.push(node);
    else if (!same(prev, node)) diff.changedNodes.push(node);
  }
  for (const id of beforeNodes.keys()) if (!afterNodes.has(id)) diff.removedNodeIds.push(id);
  for (const [id, edge] of afterEdges) {
    const prev = beforeEdges.get(id);
    if (!prev) diff.addedEdges.push(edge);
    else if (!same(prev, edge)) diff.changedEdges.push(edge);
  }
  for (const id of beforeEdges.keys()) if (!afterEdges.has(id)) diff.removedEdgeIds.push(id);
  return diff;
}

/** 浅层逐键比较：before 里有而值不同 → set 回去；before 里没有 → unset。 */
function keyDiff(before: JsonObject, after: JsonObject, skip: Set<string>) {
  const set: JsonObject = {};
  const unset: string[] = [];
  for (const key of new Set([...Object.keys(before), ...Object.keys(after)])) {
    if (skip.has(key) || same(before[key], after[key])) continue;
    if (key in before && before[key] !== undefined) set[key] = clone(before[key]);
    else unset.push(key);
  }
  return { set, unset };
}

/**
 * 生成撤销一批操作用的逆操作：删掉新增的节点 / 连线、把被改节点的**被改字段**还原。
 *
 * ⚠️ 字段级而不是整节点回滚：撤销栈里远端批次之前的快照已作废，但批次之后用户对**其他字段**
 *    的修改（比如生成结果回写）不该被一起抹掉。
 */
export function inverseOf(before: unknown, after: unknown): InverseOp[] {
  const diff = diffDocuments(before, after);
  const beforeNodes = byId(nodeList(before));
  const beforeEdges = byId(edgeList(before));
  const inverse: InverseOp[] = [];
  for (const edge of diff.addedEdges) inverse.push({ kind: "remove_edge", edgeId: edge.id });
  for (const node of diff.addedNodes) inverse.push({ kind: "remove_node", nodeId: node.id });
  for (const id of diff.removedNodeIds) inverse.push({ kind: "restore_node", node: clone(beforeNodes.get(id)!) });
  for (const node of diff.changedNodes) {
    const prev = beforeNodes.get(node.id)! as JsonObject;
    const fields = keyDiff(prev, node as JsonObject, new Set(["id", "params"]));
    const op: InverseOp = { kind: "revert_fields", nodeId: node.id, set: fields.set, unset: fields.unset };
    if (!same(prev.params, node.params)) {
      // params 逐键还原（批次只改了其中几个键）；形状本身变了才整体替换
      if (isObject(prev.params) && isObject(node.params)) op.params = keyDiff(prev.params, node.params, new Set());
      else if (prev.params === undefined) op.unset.push("params");
      else op.set.params = clone(prev.params);
    }
    inverse.push(op);
  }
  for (const id of diff.removedEdgeIds) inverse.push({ kind: "restore_edge", edge: clone(beforeEdges.get(id)!) });
  for (const edge of diff.changedEdges) inverse.push({ kind: "restore_edge", edge: clone(beforeEdges.get(edge.id)!) });
  return inverse;
}

/**
 * 应用逆操作（宽松：目标已不存在就跳过）。删节点时一并删掉连到它的线、并从画框成员里摘掉 ——
 * 否则留下悬空连线，下一次保存会被服务端校验拒绝。
 */
export function applyInverse(doc: unknown, inverse: InverseOp[]): JsonObject {
  const next = clone(isObject(doc) ? doc : {}) as Doc;
  let nodes = Array.isArray(next.nodes) ? next.nodes as JsonObject[] : [];
  let edges = Array.isArray(next.edges) ? next.edges as JsonObject[] : undefined;
  const removedNodes = new Set<string>();
  for (const op of inverse) {
    switch (op.kind) {
      case "remove_edge":
        edges = edges?.filter(edge => edge.id !== op.edgeId);
        break;
      case "remove_node":
        removedNodes.add(op.nodeId);
        nodes = nodes.filter(node => node.id !== op.nodeId);
        break;
      case "restore_node":
        if (!nodes.some(node => node.id === op.node.id)) nodes.push(clone(op.node) as JsonObject);
        break;
      case "restore_edge": {
        const list = edges ?? (edges = []);
        const index = list.findIndex(edge => edge.id === op.edge.id);
        if (index >= 0) list[index] = clone(op.edge) as JsonObject;
        else list.push(clone(op.edge) as JsonObject);
        break;
      }
      case "revert_fields": {
        const node = nodes.find(item => item.id === op.nodeId);
        if (!node) break;
        for (const key of op.unset) Reflect.deleteProperty(node, key);
        Object.assign(node, clone(op.set));
        if (op.params) {
          const params: JsonObject = isObject(node.params) ? { ...node.params } : {};
          for (const key of op.params.unset) Reflect.deleteProperty(params, key);
          Object.assign(params, clone(op.params.set));
          node.params = params;
        }
        break;
      }
    }
  }
  if (removedNodes.size) {
    edges = edges?.filter(edge => !removedNodes.has(String(edge.source)) && !removedNodes.has(String(edge.target)));
    for (const node of nodes) {
      if (node.type === FRAME_NODE_TYPE && Array.isArray(node.members)) {
        node.members = (node.members as unknown[]).filter(member => !removedNodes.has(String(member)));
      }
    }
  }
  next.nodes = nodes;
  if (edges) next.edges = edges;
  return next;
}
