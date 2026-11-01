import type { components } from '~/types/legacy';
import type {
  CanvasDocument,
  CanvasEdge,
  CanvasNode,
  CanvasOp,
  CanvasSnapshot,
  GenerationView,
  JsonObject,
  OpActor,
  OpBatch,
  RemoteUndoEntry,
  StoredCanvasEdge,
  StoredCanvasNode,
  UndoEntry
} from "~/types/canvas";
import { canvasSaveState, templateApplyState } from "~/utils/asyncState";
import { asObject, cloneJson, detachScriptSourceParams, generationInputs, newNodePosition, nextInputOrder, normalizeEdgeOrders, presetNodeParams, presetNodePosition, resetScriptEntryParams, scriptEntryResetPlan } from "~/utils/canvas";
import { FRAME_TYPE, canvasEdgeToStored, canvasNodeToStored, documentExtras, storedToCanvasEdge, storedToCanvasNode } from "~/utils/canvasDocument";
import { conflictDetails, readDraft, removeDraft, writeDraft, type ConflictDetails } from "~/utils/canvasConflict";
import { applyInverse, applyOps, diffDocuments, inverseOf, type DocumentDiff, type LinkedGeneration } from "~/utils/canvasOps";
import { nodeOrigin, withTouched } from "~/utils/canvasOrigin";
import { IMAGE_TOOL_OPERATION, imageToolRequest, imageToolSpawnPosition, type ImageToolCode, type ImageToolOptions } from "~/utils/imageTools";

type ProjectResponse = components["schemas"]["ProjectResp"];
type SaveResponse = components["schemas"]["SaveCanvasResp"];
/** `GET /projects/{id}/canvas/state`：只回版本与最后编辑者，不回画布内容。 */
type CanvasStateResponse = { version: number; updatedAt?: string; lastEditorId?: string; lastEditorName?: string; bySelf?: boolean };

function clone<T>(value: T): T {
  return cloneJson(toRaw(value));
}

function blankDocument(): CanvasDocument {
  return {
    schemaVersion: 1,
    viewport: { x: 0, y: 0, zoom: 1 },
    nodes: [],
    edges: []
  };
}

/** 用大卡片铺开展示的组类型，与 BaseNode 的 mediaCollectionGroup 保持一致 */
const MEDIA_COLLECTION_GROUPS = ["asset", "storyboard_image", "video_generator"];

function nodeDefaultWidth(nodeType: string, params: JsonObject = {}) {
  // 大卡片布局的三种组同宽：媒体框按节点宽度自适应，宽度不一致会在右侧留出空白
  if (nodeType === "group" && MEDIA_COLLECTION_GROUPS.includes(String(params.groupType ?? ""))) return 368;
  if (nodeType === "group") return 460;
  if (["video", "script"].includes(nodeType)) return 390;
  return 330;
}

/**
 * 未测量过的节点落库时的兜底高度。⚠️ 与 Go 的 `canvas/domain.DefaultNodeSize` 同值：Agent 建的节点要和
 * 用户点出来的长得一样，共享 fixture `api/fixtures/canvas-ops/addnode-shape.json` 钉住两边。
 * 3D 导演台的卡片放一张 16:9 封面，比普通节点高一截（DRT-05）。
 */
function nodeDefaultHeight(nodeType: string) {
  // 导演台与智能剪辑的卡片都放一张 16:9 封面（与 Go 的 DefaultNodeSize 同值，addnode-shape fixture 钉住两边）
  return nodeType === "director" || nodeType === "smartcut" ? 240 : 180;
}

function hydratedNodeWidth(nodeType: string, params: JsonObject, savedWidth?: number) {
  const minimum = nodeDefaultWidth(nodeType, params);
  if (nodeType === "group" && MEDIA_COLLECTION_GROUPS.includes(String(params.groupType ?? ""))) {
    return minimum;
  }
  return Math.max(savedWidth ?? 0, minimum);
}

type AddNodeOptions = {
  /**
   * 指定节点 ID（DRT-16）：要先拿到 ID 去建节点背后的对象（3D 导演台的场景以 `canvasNodeId` 绑定节点），
   * 再加节点。必须来自 `newNodeId()`，与本 store 自己生成的 ID 同一个生成器。
   */
  id?: string;
  position?: { x: number; y: number };
  recordHistory?: boolean;
  persist?: boolean;
  /** 标题直接用给定文案，不追加"节点 N"。预设派生的节点要显示预设名本身。 */
  exactTitle?: boolean;
};

export type AddEdgeOptions = {
  /** 显式 ID（组节点的连线用确定性 ID 去重）；缺省自动生成 */
  id?: string;
  /** 缺省接在目标节点已有输入的最后（`nextInputOrder`） */
  order?: number;
  operation?: string;
  sourceHandle?: string;
  targetHandle?: string;
  recordHistory?: boolean;
  persist?: boolean;
};

/** `GET /projects/{id}/canvas/ops` 的响应（契约 listCanvasOps）。 */
type CanvasOpsResponse = {
  currentVersion: number;
  needsReload: boolean;
  batches: Array<{ batchId?: unknown; fromVersion?: unknown; toVersion?: unknown; actor?: unknown; ops?: unknown }>;
};

/**
 * 补取的结果：`applied` 已追平（可能 0 批）· `reload` 链条断了（中间有真人整份保存 / 批次过期），
 * 只能整页载入 · `error` 请求失败（本地什么都没动）· `deferred` 正忙（保存在途等），稍后自动再补。
 */
export type CatchUpResult = "applied" | "reload" | "error" | "deferred";

/** 最近一次重放的远端批次（右下角提示用）。 */
export type RemoteBatchNotice = {
  batchId: string;
  /** 本批新建的节点（含画框） */
  nodeIds: string[];
  /** 本批改动的已有节点数 */
  changedCount: number;
  actorKind: "agent" | "user" | "mcp";
  /**
   * MCP 写入时的客户端名（MCPT-09）：事件里带；补取接口不带时回落到本批新建 / 改动节点上的
   * `origin.clientName`；都没有时为 undefined（提示里说「外部 AI 客户端」，不编造）。
   */
  clientName?: string;
  /** 自增序号：同一批被重复通知时也能让提示重新出现 */
  seq: number;
};

/** 补取接口说「需要整页载入」时冲突条上的说明。 */
export const RELOAD_REQUIRED_MESSAGE = "画布有其他人的改动，需要载入最新版本";

/** 保存撞上「中间只有 Agent / MCP 改过」的冲突时，自动补取并重存的最多次数；超过就交给用户。 */
const AGENT_CONFLICT_RETRIES = 3;

/**
 * 冲突时可以「补取 → 重放 → 自动重存」的最后编辑者种类：机器写入（Agent / MCP，dev/01 R9）。
 * ⚠️ mcp 必须在内：MCP 写入时最后编辑者是用户本人，不放宽的话会弹「你在另一个窗口修改过」，
 *    用户点「以我为准」就把 MCP 刚建的节点整份覆盖（MCP-F-60）。
 */
const MACHINE_EDITOR_KINDS = new Set(["agent", "mcp"]);

/** 新节点描边高亮持续多久（毫秒）。 */
const HIGHLIGHT_MS = 1200;

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

function optionalId(value: unknown): string | null {
  if (typeof value === "string" && value) return value;
  if (typeof value === "number" && Number.isFinite(value)) return String(value);
  return null;
}

/**
 * 把事件帧 / 补取接口里的一批解析成 `OpBatch`；形状不对返回 null（事件只是信号，坏帧直接丢）。
 * ⚠️ 事件的 actor 叫 `agentSessionId` / `mcpGrantId`，重放用的 actor 叫 `sessionId` / `grantId`，在这里统一。
 * ⚠️ 未知的 kind 仍按 agent 处理（与此前一致）：它只用于宽松重放，所有权由服务端判过了。
 */
export function normalizeOpBatch(raw: unknown): OpBatch | null {
  if (!isRecord(raw)) return null;
  const fromVersion = Number(raw.fromVersion);
  const toVersion = Number(raw.toVersion);
  if (!Number.isInteger(fromVersion) || !Number.isInteger(toVersion) || toVersion <= fromVersion) return null;
  const actor = isRecord(raw.actor) ? raw.actor : {};
  return {
    projectId: optionalId(raw.projectId) ?? undefined,
    batchId: typeof raw.batchId === "string" ? raw.batchId : `${fromVersion}-${toVersion}`,
    fromVersion,
    toVersion,
    ops: Array.isArray(raw.ops) ? raw.ops as CanvasOp[] : undefined,
    truncated: raw.truncated === true,
    actor: {
      kind: actor.kind === "user" ? "user" : actor.kind === "mcp" ? "mcp" : "agent",
      userId: optionalId(actor.userId),
      sessionId: optionalId(actor.agentSessionId ?? actor.sessionId),
      grantId: optionalId(actor.mcpGrantId ?? actor.grantId),
      clientName: typeof actor.clientName === "string" && actor.clientName ? actor.clientName : null
    } satisfies OpActor
  };
}

/**
 * MCP 批次的客户端名：事件里带的优先；补取接口不带，回落到本批新建 / 改动的、同授权节点上的
 * `origin.clientName`（MCPT-09）。都没有返回 undefined —— 不编造。
 */
function batchClientName(batch: OpBatch, diff: DocumentDiff): string | undefined {
  if (batch.actor.clientName) return batch.actor.clientName;
  for (const node of [...diff.addedNodes, ...diff.changedNodes]) {
    const origin = nodeOrigin(node);
    if (origin?.kind === "mcp" && origin.clientName && (!batch.actor.grantId || origin.grantId === batch.actor.grantId)) {
      return origin.clientName;
    }
  }
  return undefined;
}

/** 冲突态下三个动作的落点。`conflict` = 又撞上了别人的保存，回到冲突态（不自动循环重试）。 */
export type ConflictResolution = "resolved" | "conflict";

export const useCanvasStore = defineStore("canvas", () => {
  const projectId = ref("");
  const projectName = ref("未命名");
  const version = ref(1);
  const nodes = ref<CanvasNode[]>([]);
  const edges = ref<CanvasEdge[]>([]);
  const viewport = ref({ x: 0, y: 0, zoom: 1 });
  const selectedNodeId = ref<string | null>(null);
  /** 画布上当前框选 / 多选的节点（由 CanvasBoard 从 Vue Flow 同步进来）。`selectedNodeId` 仍是单选面板用的那一个。 */
  const selectedNodeIds = ref<string[]>([]);
  const saveState = ref<"saved" | "saving" | "conflict" | "error">("saved");
  /** 最近一次冲突的详情（谁、什么时候）；不在冲突态时为 null。 */
  const conflict = ref<ConflictDetails | null>(null);
  /** 本标签页里有这个项目的本地草稿（采纳对方版本后可恢复 / 丢弃）。 */
  const draftAvailable = ref(false);
  /** 冲突动作进行中：期间自动保存一律不发（否则会用旧版本号再撞一次 409）。 */
  const resolving = ref(false);
  /**
   * 撤销栈。普通条目是整份快照；远端批次（Agent 的一批操作）是一个 `remote` 条目，存逆操作。
   * ⚠️ 远端批次到达时清掉它之前的普通快照：那些快照回到的是「Agent 写入之前的世界」，
   *    Ctrl+Z 会静默删掉 Agent 的节点（§5.16）。
   */
  const undoStack = ref<UndoEntry[]>([]);
  const redoStack = ref<CanvasSnapshot[]>([]);
  const hydrated = ref(false);
  /** 文档顶层前端不认识的键，原样往返（§5.4）。 */
  let extraTopLevel: JsonObject = {};
  let saveTimer: ReturnType<typeof setTimeout> | undefined;
  /**
   * 远端批次挂上生成任务时的回调（generation store 注册）。
   * ⚠️ 用回调而不是在这里直接调 generation store：后者本身依赖本 store，互相 import 会成环。
   */
  const generationLinkedListeners = new Set<(item: LinkedGeneration) => void>();
  function onGenerationLinked(listener: (item: LinkedGeneration) => void): () => void {
    generationLinkedListeners.add(listener);
    return () => generationLinkedListeners.delete(listener);
  }
  /** 最近一次重放的远端批次（CanvasOpsToast 盯着它）。 */
  const lastRemoteBatch = ref<RemoteBatchNotice | null>(null);
  /** 刚由远端新增、正在描边高亮的节点。 */
  const highlightedNodeIds = ref<string[]>([]);
  let highlightTimer: ReturnType<typeof setTimeout> | undefined;
  let noticeSeq = 0;
  /** 一个 PUT 正在路上（期间不重放远端批次：版本号要等它回来才知道）。 */
  let saveInFlight = false;
  /** 保存在途时又被要求保存：回来后补一次。 */
  let saveAgain = false;
  /** 正在补取操作日志。 */
  let catchUpPromise: Promise<CatchUpResult> | null = null;
  /** 忙的时候到达的远端批次 / 补取请求，忙完再处理。 */
  let deferredBatches: OpBatch[] = [];
  let deferredResync = false;

  function hydrateNodes(document: CanvasDocument): CanvasNode[] {
    return document.nodes.map((node, index) => hydrateNode(node, index));
  }

  function hydrateNode(node: StoredCanvasNode, index: number): CanvasNode {
    const params = asObject(node.params);
    const width = node.type === FRAME_TYPE
      ? Number(node.size?.w) || 640
      : hydratedNodeWidth(node.type, params, node.size?.w);
    return storedToCanvasNode(node, { index, params, width });
  }

  /**
   * 用服务端的项目整体替换本地状态。
   *
   * ⚠️ 撤销栈在这里清空：栈里存的是整份文档的快照，采纳远端内容之后还能撤销的话，
   *    一次 Ctrl+Z 就回到了同事改动之前的世界（§5.16）。
   */
  function hydrateProject(project: ProjectResponse, fallbackId: string) {
    const document = normalizeDocument(project.canvasData);
    projectId.value = project.id ?? fallbackId;
    projectName.value = project.name ?? "未命名";
    version.value = project.version ?? 1;
    viewport.value = document.viewport;
    extraTopLevel = documentExtras(project.canvasData);
    nodes.value = hydrateNodes(document);
    edges.value = document.edges.map(storedToCanvasEdge);
    selectedNodeIds.value = [];
    undoStack.value = [];
    redoStack.value = [];
    saveState.value = "saved";
    conflict.value = null;
    lastRemoteBatch.value = null;
    // 只读不写：本标签页里若留着这个项目的草稿（比如冲突后刷新过页面），让冲突条给出恢复入口
    draftAvailable.value = Boolean(readDraft(projectId.value));
    hydrated.value = true;
  }

  async function load(id: string) {
    hydrated.value = false;
    const project = await useApi()<ProjectResponse>(`/projects/${id}`);
    hydrateProject(project, id);
  }

  function normalizeDocument(value: unknown): CanvasDocument {
    const candidate = asObject(value);
    const fallback = blankDocument();
    return {
      schemaVersion: Number(candidate.schemaVersion) || fallback.schemaVersion,
      viewport: {
        x: Number(asObject(candidate.viewport).x) || 0,
        y: Number(asObject(candidate.viewport).y) || 0,
        zoom: Number(asObject(candidate.viewport).zoom) || 1
      },
      nodes: Array.isArray(candidate.nodes) ? candidate.nodes as CanvasDocument["nodes"] : [],
      edges: Array.isArray(candidate.edges) ? candidate.edges as CanvasDocument["edges"] : []
    };
  }

  /**
   * 落库形状。⚠️ 先铺未知键再写已知键：节点 / 连线 / 顶层上前端不认识的字段
   * （比如 Agent 写入的 `agent` 元数据）必须原样带回，否则下一次自动保存就把它们抹掉。
   */
  function document(): CanvasDocument {
    return {
      ...clone(extraTopLevel),
      schemaVersion: 1,
      viewport: clone(viewport.value),
      nodes: nodes.value.map(node => canvasNodeToStored(toRaw(node), {
        w: Number(node.width) || nodeDefaultWidth(node.data.nodeType, node.data.params),
        h: Number(node.height) || nodeDefaultHeight(node.data.nodeType)
      })),
      edges: edges.value.map(edge => canvasEdgeToStored(toRaw(edge)))
    };
  }

  /**
   * 重命名项目。
   *
   * <p>走 PATCH 而不是 PUT：PUT 那条是保存画布数据的，带乐观锁 version，
   * 改个名字不该跟画布内容的版本冲突搅在一起。
   */
  async function rename(next: string) {
    const name = next.trim();
    if (!projectId.value || !name || name === projectName.value) return;
    const previous = projectName.value;
    // 先落本地再发请求：改名是低风险操作，让输入框立刻定下来，失败再回滚
    projectName.value = name;
    try {
      await useApi()(`/projects/${projectId.value}`, { method: "PATCH", body: { name } });
    } catch (cause) {
      projectName.value = previous;
      throw cause;
    }
  }

  /** 把当前本地文档存成草稿；写不进去返回 false。 */
  function persistDraft() {
    return writeDraft({
      projectId: projectId.value,
      baseVersion: version.value,
      savedAt: new Date().toISOString(),
      document: document()
    });
  }

  /**
   * 进冲突态：记下详情，并**立刻**把本地文档存成草稿 —— 用户随后可能直接关页面。
   * ⚠️ 不触碰本地 nodes/edges（FLOW-09、FE-03）。
   */
  function enterConflict(cause: unknown) {
    conflict.value = conflictDetails(cause);
    saveState.value = "conflict";
    persistDraft();
  }

  /** 补取说链条断了：进冲突态（不自动重载 —— 本地可能有未保存的改动），说明换成「需要载入最新版本」。 */
  function enterReloadConflict() {
    conflict.value = { currentUpdatedAt: "", currentEditorId: "", currentEditorName: "", bySelf: false, message: RELOAD_REQUIRED_MESSAGE };
    saveState.value = "conflict";
    persistDraft();
  }

  async function save() {
    if (!hydrated.value || !projectId.value || resolving.value) return;
    // 同一时刻只发一个 PUT：两个并发 PUT 带同一个版本号，后到的那个必然撞上前一个
    if (saveInFlight) {
      saveAgain = true;
      return;
    }
    saveInFlight = true;
    try {
      // 正在补取远端批次：等它追平版本号再发，否则这一次必然 409
      if (catchUpPromise) await catchUpPromise;
      if (saveState.value === "conflict") {
        // 冲突态下不发请求（必然再 409），但继续把新改动刷进草稿，免得草稿停在进冲突那一刻
        persistDraft();
        return;
      }
      saveState.value = "saving";
      for (let attempt = 0; ; attempt += 1) {
        try {
          // ⚠️ 新后端把画布保存**单独开了一条路**：`PUT /projects/{id}/canvas`。
          //    老系统是 `PUT /projects/{id}`（连同项目一起存）——
          //    打到老路径上新后端回 **405**，而 405 看起来像"方法写错了"，
          //    不像"端点搬家了"。
          //
          //    读**不用改**：`GET /projects/{id}` 的响应里就带 `canvasData`。
          //    冲突码也一致（40902 VERSION_CONFLICT），所以下面的冲突处理原样保留。
          const response = await useApi()<SaveResponse>(`/projects/${projectId.value}/canvas`, {
            method: "PUT",
            body: { canvasData: document(), version: version.value }
          });
          version.value = response.version ?? version.value + 1;
          saveState.value = "saved";
          return;
        } catch (cause) {
          // ⚠️ 冲突与普通失败的处置完全不同，判定抽在 utils/asyncState 里单测。
          //    两个态**都不触碰本地 nodes/edges** —— 静默丢弃用户刚画的东西是这条
          //    规则要防的唯一后果（FLOW-09、FE-03）。
          if (canvasSaveState(cause) !== "conflict") {
            saveState.value = "error";
            return;
          }
          // ⚠️ 中间那次写入是 Agent / MCP 的画布操作（AGT-11、MCPT-09）：补取 → 本地重放（本地改动叠在上面）→
          //    立即重存，用户无感。是真人写的、或补取发现链条断了 / 重试用完 → 冲突态（AGT-07）。
          const kind = conflictDetails(cause).editorKind;
          if (!kind || !MACHINE_EDITOR_KINDS.has(kind) || attempt >= AGENT_CONFLICT_RETRIES) {
            enterConflict(cause);
            return;
          }
          const result = await startCatchUp();
          if (result === "reload") {
            enterReloadConflict();
            return;
          }
          if (result !== "applied") {
            enterConflict(cause);
            return;
          }
        }
      }
    } finally {
      saveInFlight = false;
      if (saveAgain) {
        saveAgain = false;
        scheduleSave();
      }
      drainDeferred();
    }
  }

  function cancelPendingSave() {
    clearTimeout(saveTimer);
    saveTimer = undefined;
  }

  /**
   * 处理保存冲突（AG-P3）。
   *
   * - `adopt` 采纳对方版本：先确认草稿写进去了，再载入服务端最新版本。草稿写不进去时**拒绝**
   *   （否则就是静默丢弃用户的改动）。
   * - `overwrite` 以我为准：问 `canvas/state` 要当前版本号，用它重存本地文档。期间又有人存了
   *   → 回到冲突态并刷新详情，**不自动循环重试**。
   *
   * ⚠️ 两条路都不先清本地再发请求：请求失败时本地内容原样还在。
   */
  async function resolveConflict(mode: "adopt" | "overwrite"): Promise<ConflictResolution> {
    if (saveState.value !== "conflict" || resolving.value || !projectId.value) return "conflict";
    cancelPendingSave();
    resolving.value = true;
    const id = projectId.value;
    try {
      if (mode === "adopt") {
        if (!persistDraft()) {
          throw new Error("浏览器无法保存本地草稿（存储空间不足或被禁用），为避免丢失你的改动，暂不能采纳对方版本");
        }
        const project = await useApi()<ProjectResponse>(`/projects/${id}`);
        hydrateProject(project, id);
        return "resolved";
      }
      const state = await useApi()<CanvasStateResponse>(`/projects/${id}/canvas/state`);
      const sent = document();
      try {
        const response = await useApi()<SaveResponse>(`/projects/${id}/canvas`, {
          method: "PUT",
          body: { canvasData: sent, version: state.version }
        });
        version.value = response.version ?? state.version + 1;
        conflict.value = null;
        saveState.value = "saved";
        removeDraft(id);
        draftAvailable.value = false;
        // 请求在途时用户又改了东西：补存一次（这次用的是新版本号）
        if (JSON.stringify(document()) !== JSON.stringify(sent)) scheduleSave();
        return "resolved";
      } catch (cause) {
        if (canvasSaveState(cause) === "conflict") {
          enterConflict(cause);
          return "conflict";
        }
        throw cause;
      }
    } finally {
      resolving.value = false;
      drainDeferred();
    }
  }

  /**
   * 用草稿替换本地画布，再按当前版本保存（撤销栈里留一步，可撤回）。
   *
   * ⚠️ 这会用草稿覆盖刚载入的远端内容 —— 冲突条上要说清楚；冲突态下不提供（先选采纳或以我为准）。
   */
  async function restoreDraft(): Promise<boolean> {
    if (!hydrated.value || saveState.value === "conflict" || resolving.value) return false;
    const draft = readDraft(projectId.value);
    if (!draft) {
      draftAvailable.value = false;
      return false;
    }
    cancelPendingSave();
    snapshot();
    const restored = normalizeDocument(draft.document);
    nodes.value = hydrateNodes(restored);
    edges.value = restored.edges.map(storedToCanvasEdge);
    selectedNodeIds.value = [];
    selectedNodeId.value = null;
    await save();
    if (saveState.value === "saved") {
      removeDraft(projectId.value);
      draftAvailable.value = false;
    }
    return true;
  }

  function discardDraft() {
    removeDraft(projectId.value);
    draftAvailable.value = false;
  }

  /**
   * 对**空画布**套用工作流模板（画布上那四张快捷卡片）—— 照老系统：
   * `POST /projects/{id}/template {templateCode, version}`，服务端把模板画布写进来、回整个项目。
   *
   * ⚠️ 走乐观锁：套模板就是一次画布保存，不带 version 的话会盖掉另一个标签页刚画的东西。
   *    发之前先撤掉排队中的自动保存 —— 它要是抢先落库，版本号一变，这次套用必然 40902。
   * ⚠️ 失败时本地一律不动（画布本来就是空的）；只有「本地看到的是旧的」才进冲突态
   *    （判定见 `templateApplyState`）。
   */
  async function applyTemplate(templateCode: string) {
    if (!hydrated.value || !projectId.value || nodes.value.length || edges.value.length) {
      throw new Error("仅空画布可以应用工作流模板");
    }
    const hadPendingSave = saveTimer !== undefined;
    clearTimeout(saveTimer);
    saveTimer = undefined;
    try {
      const project = await useApi()<ProjectResponse>(`/projects/${projectId.value}/template`, {
        method: "POST",
        body: { templateCode, version: version.value }
      });
      hydrateProject(project, projectId.value);
    } catch (cause) {
      const state = templateApplyState(cause);
      // 画布本来就是空的，不存草稿（存了的话"恢复草稿"会把别人画的东西清成空画布）
      if (state) {
        conflict.value = conflictDetails(cause);
        saveState.value = state;
      }
      else if (hadPendingSave) scheduleSave();
      throw cause;
    }
  }

  function scheduleSave() {
    if (!hydrated.value) return;
    clearTimeout(saveTimer);
    saveTimer = setTimeout(() => {
      saveTimer = undefined;
      void save();
    }, 700);
  }

  function pushUndo(entry: UndoEntry) {
    undoStack.value.push(entry);
    if (undoStack.value.length > 40) undoStack.value.shift();
  }

  function snapshot() {
    pushUndo({ kind: "snapshot", nodes: clone(nodes.value), edges: clone(edges.value) });
    redoStack.value = [];
  }

  function undo() {
    const previous = undoStack.value.pop();
    if (!previous) return;
    if (previous.kind === "remote") {
      revertRemote(previous);
      return;
    }
    redoStack.value.push({ nodes: clone(nodes.value), edges: clone(edges.value) });
    nodes.value = previous.nodes;
    edges.value = previous.edges;
    scheduleSave();
  }

  function redo() {
    const next = redoStack.value.pop();
    if (!next) return;
    pushUndo({ kind: "snapshot", nodes: clone(nodes.value), edges: clone(edges.value) });
    nodes.value = next.nodes;
    edges.value = next.edges;
    scheduleSave();
  }

  // ---------------------------------------------------------------- 远端批次（AGT-11）

  function inConflict() {
    return saveState.value === "conflict";
  }

  /** 正在做会改变版本号的事：远端批次与补取都要等它做完（否则会拿着过时的版本号判「不连续」）。 */
  function busy() {
    return saveInFlight || resolving.value || catchUpPromise !== null;
  }

  /**
   * 把一份新的落库文档**增量**合进运行时状态：新增的 push、被改的原地改字段、其余对象引用不变
   * （Vue Flow 据此只重绘变了的节点 —— 整体替换数组会让整张画布闪一下）。
   */
  function mergeDocument(previous: CanvasDocument, next: JsonObject) {
    const diff = diffDocuments(previous, next);
    if (diff.removedNodeIds.length) {
      const removed = new Set(diff.removedNodeIds);
      nodes.value = nodes.value.filter(node => !removed.has(node.id));
      selectedNodeIds.value = selectedNodeIds.value.filter(id => !removed.has(id));
      if (selectedNodeId.value && removed.has(selectedNodeId.value)) selectedNodeId.value = null;
    }
    const previousById = new Map(previous.nodes.map(node => [node.id, node]));
    for (const stored of diff.changedNodes) {
      const node = nodes.value.find(item => item.id === stored.id);
      if (!node) continue;
      const index = nodes.value.indexOf(node);
      const fresh = hydrateNode(cloneJson(stored), index);
      const before = previousById.get(stored.id);
      if (JSON.stringify(before?.position) !== JSON.stringify(stored.position)) node.position = fresh.position;
      if (fresh.type === "frame") {
        node.width = fresh.width;
        node.height = fresh.height;
      }
      node.data.title = fresh.data.title;
      node.data.params = fresh.data.params;
      node.data.members = fresh.data.members;
      node.data.agent = fresh.data.agent;
      node.data.origin = fresh.data.origin;
      node.data.stored = fresh.data.stored;
      // 生成结果不归批次管；只有逆操作恢复整节点时才可能变
      if (JSON.stringify(before?.result) !== JSON.stringify(stored.result)) node.data.generation = fresh.data.generation;
    }
    for (const stored of diff.addedNodes) {
      nodes.value.push(hydrateNode(cloneJson(stored), nodes.value.length));
    }
    if (diff.removedEdgeIds.length) {
      const removed = new Set(diff.removedEdgeIds);
      edges.value = edges.value.filter(edge => !removed.has(edge.id));
    }
    for (const stored of diff.changedEdges) {
      const index = edges.value.findIndex(edge => edge.id === stored.id);
      if (index >= 0) edges.value[index] = storedToCanvasEdge(cloneJson(stored));
    }
    for (const stored of diff.addedEdges) edges.value.push(storedToCanvasEdge(cloneJson(stored) as StoredCanvasEdge));
  }

  function highlight(ids: string[]) {
    if (!ids.length) return;
    highlightedNodeIds.value = [...new Set([...highlightedNodeIds.value, ...ids])];
    clearTimeout(highlightTimer);
    highlightTimer = setTimeout(() => {
      highlightedNodeIds.value = [];
    }, HIGHLIGHT_MS);
  }

  /**
   * 在本地重放一批（调用方已确认 `fromVersion === version`）。失败返回 false（本地不动）。
   *
   * ⚠️ 不保存：重放之后本地就是服务端的那一版（加上本地本来就没保存的改动，它们自有排队中的保存）。
   *    打开页面 / 收到事件就写库的话，会产生用户什么都没做的版本。
   */
  function applyBatch(batch: OpBatch): boolean {
    if (!batch.ops?.length) return false;
    const previous = document();
    const result = applyOps(previous, batch.ops, batch.actor, { lenient: true });
    if (result.error) return false;
    mergeDocument(previous, result.doc);
    version.value = batch.toVersion;
    const diff = diffDocuments(previous, result.doc);
    // 撤销栈：作废之前的普通快照（它们回到的是 Agent 写入之前的世界）
    undoStack.value = undoStack.value.filter(item => item.kind === "remote");
    redoStack.value = [];
    // ⚠️ 只挂任务（link_generation）的批次**不压撤销条目、不弹提示**：它记的是「节点上已经有一次
    //    生成」这个事实，撤销它不会取消任务（钱照扣），只会让节点又显示成空的 —— 正是要修的缺陷。
    const onlyLinks = batch.ops.every(op => op.kind === "link_generation");
    if (!onlyLinks) {
      const entry: RemoteUndoEntry = { kind: "remote", batchId: batch.batchId, nodeIds: result.nodeIds, inverse: inverseOf(previous, result.doc) };
      pushUndo(entry);
      noticeSeq += 1;
      lastRemoteBatch.value = {
        batchId: batch.batchId,
        nodeIds: result.nodeIds,
        changedCount: diff.changedNodes.length,
        actorKind: batch.actor.kind,
        clientName: batch.actor.kind === "mcp" ? batchClientName(batch, diff) : undefined,
        seq: noticeSeq
      };
      highlight(result.nodeIds);
    }
    // Agent 受理的任务（link_generation）：交给 generation store 接上跟踪，节点立刻显示排队 / 生成中。
    for (const item of result.linked) for (const listener of generationLinkedListeners) listener(item);
    // 跳过了几条（本地已删 / 用户刚改过的节点）：本地以用户为准，补一次保存让服务端跟上
    if (result.skipped > 0) scheduleSave();
    return true;
  }

  /**
   * 收到 `canvas.ops_applied`（或补取到的一批）。事件只是对账信号：
   * 别的项目、重复 / 过期的批次一律丢弃；版本不连续或事件被截断时改走补取。
   */
  function applyRemoteOps(raw: unknown) {
    const batch = normalizeOpBatch(raw);
    if (!batch || !hydrated.value || !projectId.value) return;
    if (batch.projectId && batch.projectId !== projectId.value) return;
    if (busy()) {
      deferredBatches.push(batch);
      return;
    }
    // 冲突态下本地版本号本来就不可信，等用户选了出路再说
    if (saveState.value === "conflict") return;
    if (batch.toVersion <= version.value) return;
    if (batch.fromVersion === version.value && !batch.truncated && applyBatch(batch)) return;
    void startCatchUp().then(result => {
      if (result === "reload" && !inConflict()) enterReloadConflict();
    });
  }

  async function runCatchUp(since: number): Promise<CatchUpResult> {
    const id = projectId.value;
    let response: CanvasOpsResponse;
    try {
      response = await useApi()<CanvasOpsResponse>(`/projects/${id}/canvas/ops?sinceVersion=${since}`);
    } catch {
      return "error";
    }
    if (id !== projectId.value) return "error";
    if (response.needsReload) return "reload";
    for (const raw of response.batches ?? []) {
      const batch = normalizeOpBatch(raw);
      if (!batch) return "reload";
      if (batch.toVersion <= version.value) continue;
      if (batch.fromVersion !== version.value || !applyBatch(batch)) return "reload";
    }
    return version.value >= Number(response.currentVersion ?? 0) ? "applied" : "reload";
  }

  /** 同一时刻只有一个补取；结束后处理忙时积下的批次。 */
  function startCatchUp(since: number = version.value): Promise<CatchUpResult> {
    if (!catchUpPromise) {
      // 比本地还新的起点没有意义（中间那段会被当成缺口）；更旧的起点无害（已有的批次按版本跳过）
      catchUpPromise = runCatchUp(Math.min(since, version.value)).finally(() => {
        catchUpPromise = null;
        drainDeferred();
      });
    }
    return catchUpPromise;
  }

  /**
   * 按 `sinceVersion` 补取并重放（重连后对账 / 事件不连续时）。`needsReload` 时**不自动重载**，
   * 进冲突态让用户选（本地改动按 AGT-07 规则存草稿）。
   */
  async function fetchOpsSince(since: number = version.value): Promise<CatchUpResult> {
    if (!hydrated.value || !projectId.value || saveState.value === "conflict") return "error";
    if (saveInFlight || resolving.value) {
      deferredResync = true;
      return "deferred";
    }
    const result = await startCatchUp(since);
    // 等待期间可能已被别的路径置成冲突态（TS 看不出 await 之后状态会变，所以经函数读）
    if (result === "reload" && !inConflict()) enterReloadConflict();
    return result;
  }

  function drainDeferred() {
    if (busy()) return;
    const batches = deferredBatches.sort((a, b) => a.fromVersion - b.fromVersion);
    const resync = deferredResync;
    deferredBatches = [];
    deferredResync = false;
    for (const batch of batches) applyRemoteOps(batch);
    if (resync) void fetchOpsSince();
  }

  function revertRemote(entry: RemoteUndoEntry) {
    const previous = document();
    mergeDocument(previous, applyInverse(previous, entry.inverse));
    // 撤销 Agent 的一批没有对应的「重做」：重做要把那批操作再放一遍，而它之后可能已经过时
    redoStack.value = [];
    if (lastRemoteBatch.value?.batchId === entry.batchId) lastRemoteBatch.value = null;
    scheduleSave();
  }

  /**
   * 撤销某一批 Agent 操作（提示条上的「撤销」）。只在它仍是撤销栈顶时可用 ——
   * 之后还有别的改动的话，跳过它们去撤这一批会把那些改动的前提抽掉。
   */
  function undoAgentBatch(batchId: string): boolean {
    const top = undoStack.value[undoStack.value.length - 1];
    if (top?.kind !== "remote" || top.batchId !== batchId) return false;
    undo();
    return true;
  }

  /**
   * 用户改了 Agent / MCP 建的节点（参数 / 标题 / 位置）：置 `userTouched = true`，随下一次保存落库。
   * 之后 Agent / MCP 不再改它（服务端所有权规则）。`agent` 与 `origin` 存在哪个写哪个（两个都在两个都写），
   * 只作用于机器建的节点、且只置一次。
   *
   * ⚠️ 只能从用户亲手操作的入口调（拖动、改标题、ParamPanel 控件），**不能**放进 `updateParams`：
   *    挂载补默认值也走它（AGT-11 记过），放进去的话打开画布就把机器节点全标成「用户改过」。
   */
  function markTouched(nodeId: string): boolean {
    const node = nodes.value.find(item => item.id === nodeId);
    if (!node) return false;
    const next = withTouched(node.data);
    if (!next) return false;
    if (next.agent) node.data.agent = next.agent as CanvasNode["data"]["agent"];
    if (next.origin) node.data.origin = next.origin as CanvasNode["data"]["origin"];
    return true;
  }

  /** 改节点标题（画框改名等）：可撤销、标记用户改过、排队保存。 */
  function updateTitle(nodeId: string, title: string) {
    const node = nodes.value.find(item => item.id === nodeId);
    const next = title.trim();
    if (!node || !next || next === node.data.title) return;
    snapshot();
    node.data.title = next;
    markTouched(nodeId);
    scheduleSave();
  }

  /**
   * 落一条连线并返回其 ID。服务端连线校验（依赖组件里的 mode 推导）仍留在 CanvasBoard，
   * 这里只负责落边：order 缺省接在目标已有输入之后（视频首尾帧 / 参考图顺序靠它）。
   */
  /**
   * 删除一条连线（画布上双击连线 → 点剪刀）。可撤销；目标节点剩下的入边按原顺序重排成 1..N，
   * 否则 order 留出空号，首尾帧 / 参考图的「图N」编号会错位（§5.4：order 必须从 1 起连续）。
   */
  function removeEdge(edgeId: string) {
    if (!edges.value.some(edge => edge.id === edgeId)) return;
    snapshot();
    edges.value = normalizeEdgeOrders(edges.value.filter(edge => edge.id !== edgeId));
    scheduleSave();
  }

  function addEdge(source: string, target: string, options: AddEdgeOptions = {}) {
    if (options.recordHistory !== false) snapshot();
    let id = options.id ?? `e_${source}_${target}_${Date.now().toString(36)}`;
    // 同一毫秒里连出多条时自动生成的 ID 会撞，撞了就补序号（显式 ID 的调用方自己负责去重）
    if (!options.id) {
      const base = id;
      for (let suffix = 2; edges.value.some(edge => edge.id === id); suffix += 1) id = `${base}_${suffix}`;
    }
    edges.value.push({
      id,
      source,
      sourceHandle: options.sourceHandle ?? "out",
      target,
      targetHandle: options.targetHandle ?? "in",
      animated: true,
      data: { order: options.order ?? nextInputOrder(edges.value, target), operation: options.operation }
    });
    if (options.persist !== false) scheduleSave();
    return id;
  }

  /**
   * 新节点 ID。`addNode` / `duplicateNode` 与「先拿 ID 再建对象」的调用方（导演台）共用这一个生成器（DRT-16）。
   *
   * ⚠️ 同一毫秒里连生成两个会撞（复制节点此前就是 `n_${时间}`，一毫秒内连按两次 ⌘D 得到同一个 ID），
   *    撞了就补序号 —— 与 addEdge 的做法相同。
   */
  function newNodeId() {
    const base = `n_${Date.now().toString(36)}_${nodes.value.length + 1}`;
    let id = base;
    for (let suffix = 2; nodes.value.some(node => node.id === id); suffix += 1) id = `${base}_${suffix}`;
    return id;
  }

  function addNode(nodeType: string, title: string, params: JsonObject = {}, options: AddNodeOptions = {}) {
    // ⚠️ 指定的 ID 已被占用时拒绝，而不是悄悄换一个：调用方已经拿它建了对象（导演台场景按它绑定节点），
    //    换掉的话新节点会指向一个绑在别处的场景，打开时还要再分叉一次。
    if (options.id && nodes.value.some(node => node.id === options.id)) {
      throw new Error(`节点 ID 已存在：${options.id}`);
    }
    if (options.recordHistory !== false) snapshot();
    const index = nodes.value.length + 1;
    const id = options.id || newNodeId();
    nodes.value.push({
      id,
      type: "aitv",
      position: options.position ?? newNodePosition(index),
      width: nodeDefaultWidth(nodeType, params),
      data: { nodeType, title: options.exactTitle ? title : `${title}节点 ${index}`, params }
    });
    selectedNodeId.value = id;
    if (options.persist !== false) scheduleSave();
    return id;
  }

  /**
   * 从一个已出图的节点派生预设节点：新建 + 连边 + 选中，源图成为它的输入。
   *
   * <p>节点上方的加工条与面板里的预设选择器都走这里 —— 两处各写一份必然漂移，
   * 而连边上的 operation 一旦漏掉，"基于当前图像"的预设就拿不到源图，白烧一次积分。
   */
  function addPresetNode(
    sourceId: string,
    preset: { id?: string; name?: string; params?: unknown; dimensions?: unknown },
    operation: string
  ) {
    const source = nodes.value.find(node => node.id === sourceId);
    if (!source) return "";
    // 同一源节点连续派生时向下错开（IMG-UX-06）：第一个仍在右侧 +420 同一行，之后每个往下一格 ——
    // 不错开的话人物调节、全景、九宫格派生出来的节点叠在同一个位置（IMGT-19 浏览器证据 A14）。
    return spawnChildNode(sourceId, preset.name ?? "图片", presetNodeParams(source.data.params, preset), operation,
      { stagger: true });
  }

  /**
   * 派生子节点的公共部分（九宫格预设与图片工具条共用，IMGD-25）：建 image 节点 + 连边（order=1）+ 选中 + 保存，
   * 整个动作一步撤销。
   *
   * ⚠️ 位置：缺省与九宫格一致（源节点右侧 +420px、同一行）；`stagger` 为 true 时同一源节点已有 n 个下游时
   *    向下错开 n × (节点高 + 40)（IMG-UX-06）。九宫格保持原行为（不错开），免得改动已有交互。
   */
  function spawnChildNode(sourceId: string, title: string, params: JsonObject, operation: string, options: { stagger?: boolean } = {}) {
    const source = nodes.value.find(node => node.id === sourceId);
    if (!source) return "";
    snapshot();
    const children = edges.value.filter(edge => edge.source === sourceId && nodes.value.some(node => node.id === edge.target)).length;
    const position = options.stagger
      ? imageToolSpawnPosition(source.position, children, Number(source.height) || nodeDefaultHeight(source.data.nodeType))
      : presetNodePosition(source.position);
    const targetId = addNode("image", title, params, {
      position,
      exactTitle: true,
      recordHistory: false,
      persist: false
    });
    addEdge(sourceId, targetId, { order: 1, operation, recordHistory: false, persist: false });
    selectedNodeId.value = targetId;
    scheduleSave();
    return targetId;
  }

  /**
   * 从已出图的节点派生一个**工具子节点**（docs/agent/imagetools IMGD-22/25）：`params.tool` / `params.toolOptions`
   * 记工具与选项，连线 order=1（源图），同一源节点连续派生时向下错开，选中新节点并保存。
   *
   * - `generate=true`（编辑器发送，IMGK-5）：建好立即调 `generation.generate`；
   * - `generate=false`（「高清」入口）：只建节点，用户在子节点的面板上点发送。
   *
   * ⚠️ 不继承源节点的模型：源节点的模型（如图片生成模型）多半没绑这个工具的操作，带过去报价就是「模型不支持该操作」。
   *    工具的模型来自 `/image-tools` 的 `models`，由调用方传 `model`；缺省时节点上不带模型，面板自己选。
   * ⚠️ `toolSourceMediaId` 记下派生那一刻的源图（IMG-D-03）：上游换了图时面板据此提示重新识别（IMG-F-21）。
   *
   * 返回新节点 ID（源节点不存在时为空串）；生成失败不回滚节点（节点已经建好、用户可以在上面重试），错误放在 `error`。
   */
  async function spawnToolNode(
    sourceId: string,
    tool: ImageToolCode,
    toolOptions: ImageToolOptions,
    options: { title: string; generate: boolean; model?: string }
  ): Promise<{ nodeId: string; generationId?: string; error?: unknown }> {
    const source = nodes.value.find(node => node.id === sourceId);
    if (!source) return { nodeId: "" };
    const sourceMediaId = source.data.generation?.mediaId;
    const params: JsonObject = {
      tool,
      toolOptions: cloneJson(toolOptions) as JsonObject,
      ...(sourceMediaId ? { toolSourceMediaId: sourceMediaId } : {}),
      ...(options.model ? { model: options.model } : {})
    };
    // 连线上的操作码：工具都是图生图（2026-10-31 方案变更），与受理请求的 operation 同一个值
    const nodeId = spawnChildNode(sourceId, options.title, params, IMAGE_TOOL_OPERATION, { stagger: true });
    if (!nodeId || !options.generate) return { nodeId };
    if (!options.model) return { nodeId, error: new Error("该工具还没有可用的模型") };
    try {
      const request = imageToolRequest({
        projectId: projectId.value,
        nodeId,
        tool,
        options: toolOptions,
        modelCode: options.model,
        inputs: generationInputs(nodes.value, edges.value, nodeId),
        params
      });
      const generationId = await useGenerationStore().generate(nodeId, request);
      scheduleSave();
      return { nodeId, generationId };
    } catch (error) {
      return { nodeId, error };
    }
  }

  function removeNode(id: string) {
    snapshot();
    const resetPlan = scriptEntryResetPlan(nodes.value, edges.value, id);
    const removedIds = new Set([id, ...resetPlan.sourceIds]);
    const resetTargetIds = new Set(resetPlan.targetIds);
    nodes.value = nodes.value
      .filter(node => !removedIds.has(node.id))
      .map((node) => {
        if (resetTargetIds.has(node.id)) {
          return {
            ...node,
            data: {
              ...node.data,
              title: "脚本生成器",
              params: resetScriptEntryParams(node.data.params)
            }
          };
        }
        if (node.data.params.scriptSourceNode === true
          && resetTargetIds.has(String(node.data.params.scriptNodeId ?? ""))) {
          return {
            ...node,
            data: { ...node.data, params: detachScriptSourceParams(node.data.params) }
          };
        }
        return node;
      });
    edges.value = normalizeEdgeOrders(edges.value.filter(edge => !removedIds.has(edge.source)
      && !removedIds.has(edge.target)
      && !resetTargetIds.has(edge.target)));
    selectedNodeId.value = null;
    selectedNodeIds.value = selectedNodeIds.value.filter(nodeId => !removedIds.has(nodeId));
    scheduleSave();
  }

  /**
   * 复制节点，返回副本的 ID（源节点不存在时为 undefined）。
   *
   * ⚠️ params 是深拷贝：引用外部对象的节点（导演台的 `sceneId`）复制出来与原节点共用同一份对象，
   *    调用方拿到新 ID 后要自己分叉（CanvasBoard 的 `duplicateCanvasNode`，DRD-43）。
   */
  function duplicateNode(id: string): string | undefined {
    const source = nodes.value.find(node => node.id === id);
    if (!source) return undefined;
    snapshot();
    // ⚠️ 副本是用户自己建的节点，不继承 agent / origin 归属 —— 继承的话 Agent / MCP 会把它当成自己的去改
    const data = clone(source.data);
    delete data.agent;
    delete data.origin;
    const copyId = newNodeId();
    nodes.value.push({
      ...clone(source),
      id: copyId,
      position: { x: source.position.x + 42, y: source.position.y + 42 },
      data: { ...data, title: `${source.data.title} 副本`, generation: undefined }
    });
    scheduleSave();
    return copyId;
  }

  function updateParams(id: string, params: JsonObject) {
    const node = nodes.value.find(item => item.id === id);
    if (!node) return;
    node.data.params = params;
    scheduleSave();
  }

  /**
   * 节点按内容调宽度（视频节点按产物比例摆，竖屏变窄）。
   *
   * ⚠️ **不排保存、不进撤销栈**：这是展示随内容自适应，不是用户的编辑 —— 打开画布就触发一次保存的话，
   *    多人同时打开同一张画布会互相撞版本。宽度跟着下一次真正的保存落库即可。
   */
  function fitNodeWidth(id: string, width: number) {
    const node = nodes.value.find(item => item.id === id);
    if (!node || node.width === width) return;
    node.width = width;
  }

  function updateModelOverrides(id: string, overrides: JsonObject) {
    const node = nodes.value.find(item => item.id === id);
    if (!node) return;
    node.data.modelOverrides = overrides;
  }

  function updateGeneration(id: string, generation: GenerationView) {
    const node = nodes.value.find(item => item.id === id);
    if (!node) return;
    node.data.generation = { ...node.data.generation, ...generation };
  }

  function reload() {
    return load(projectId.value);
  }

  return {
    projectId,
    projectName,
    version,
    nodes,
    edges,
    viewport,
    selectedNodeId,
    selectedNodeIds,
    saveState,
    conflict,
    draftAvailable,
    resolving,
    undoStack,
    redoStack,
    hydrated,
    lastRemoteBatch,
    onGenerationLinked,
    highlightedNodeIds,
    applyRemoteOps,
    fetchOpsSince,
    undoAgentBatch,
    markTouched,
    updateTitle,
    load,
    reload,
    rename,
    applyTemplate,
    save,
    document,
    resolveConflict,
    restoreDraft,
    discardDraft,
    scheduleSave,
    snapshot,
    undo,
    redo,
    newNodeId,
    addNode,
    addEdge,
    removeEdge,
    addPresetNode,
    spawnChildNode,
    spawnToolNode,
    removeNode,
    duplicateNode,
    updateParams,
    fitNodeWidth,
    updateModelOverrides,
    updateGeneration
  };
});
