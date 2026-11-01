import type { components } from "@tabtv/api-client";
import { VERSION_CONFLICT, errorCode } from "~/utils/asyncState";
import { conflictDetails, type ConflictDetails } from "~/utils/canvasConflict";
import { documentConflictText } from "~/utils/documentNode";
import { directorAutoKeyframe } from "~/utils/directorAnimation";
import { directorTimelineErrorText } from "~/utils/directorTimeline";
import { directorCanEdit } from "~/utils/directorRole";
import {
  directorMigrateScene,
  directorTouch,
  directorValidateScene,
  type DirectorObject,
  type DirectorScene
} from "~/utils/directorScene";

/**
 * 3D 导演台的场景 store（DRT-14，dev/03 §5）：编辑器的**唯一数据源**。引擎只投影这里的场景，不持有数据。
 *
 * 负责：载入、防抖自动保存（六种保存状态）、撤销 / 重做、版本冲突、离线停存与恢复后对账、实时事件的自动采纳、
 * 截图 / 关闭前的 flush、AI 提案的锁定与应用、只读。全部规则在 `director.test.ts` 里覆盖，不依赖 WebGL。
 *
 * ⚠️ 场景正文不进画布的 canvasData（DRK-9）：这里直接读写 `/projects/{pid}/director-scenes/{sid}`，
 *    有自己的乐观锁（`version`），与画布的版本号无关。
 * ⚠️ 本 store 只管场景 JSON，不取媒体：`useAuthenticatedMedia` 注册 onScopeDispose，在 store 方法里调用是错的。
 */

type SceneResponse = components["schemas"]["DirectorScene"];
type SceneSummary = components["schemas"]["DirectorSceneSummary"];
export type DirectorMediaRecord = components["schemas"]["DirectorMediaRecord"];

/** 保存状态（DR-F-150）。`idle` = 已保存。`conflict` 与 `error` 的处置完全不同，不能合并。 */
export type DirectorSaveState = "idle" | "dirty" | "saving" | "error" | "conflict" | "offline";
export type DirectorEditorKind = "user" | "agent" | "mcp";

/**
 * 冲突详情：画布冲突的形状，外加 `currentSource`（文案函数 `documentConflictText` 读它，DRD-46）。
 * 后端的冲突详情里 `currentSource` 与 `editorKind` 同值、两个都给。
 */
export type DirectorConflict = ConflictDetails & {
  currentSource?: DirectorEditorKind;
  agentSessionId?: string;
  mcpGrantId?: string;
};

type BuildResult = components["schemas"]["DirectorBuildResult"];

/**
 * AI 搭景的提案：`DirectorBuildResult`（DRT-19 的生成类型）摊平成一层 —— 完整的建议场景 + 变更清单 + 默认值说明。
 * 非空时编辑锁定（DR-F-102）。变更清单、说明与版本号的类型都取自契约（不手写 DTO）；
 * 正文用编辑模型 `DirectorScene`（store 与引擎都按它读写）。
 */
export type DirectorProposal = Omit<BuildResult["proposal"], "scene"> & {
  scene: DirectorScene;
  /** 搭景时基于的场景版本：应用时版本已经变了，就说明期间场景被改过，这份方案作废。 */
  baseVersion: BuildResult["baseVersion"];
  callId?: BuildResult["callId"];
  modelCode?: BuildResult["modelCode"];
};

/** 本地草稿：冲突、失败、离线时把本地场景存进 sessionStorage，任何路径都不静默丢弃用户的改动（DR-N-12）。 */
export type DirectorDraft = {
  projectId: string;
  sceneId: string;
  /** 草稿基于的服务端版本（排障用）。 */
  baseVersion: number;
  savedAt: string;
  scene: DirectorScene;
};

/** 最近一次自动采纳的远端更新（编辑器据此提示「场景被 X 更新」，DR-F-155）。 */
export type DirectorRemoteNotice = { version: number; editorName: string; editorKind: DirectorEditorKind; seq: number };

/** 只读的原因：`role` 角色不允许编辑 · `forbidden` 后端回了 403 · `newer` 场景是比前端新的结构版本。 */
export type DirectorReadonlyReason = "role" | "forbidden" | "newer" | null;

/** flush 失败：带着当时的保存状态，截图与关闭据此说清该怎么处理（DR-F-73、DR-F-154）。 */
export class DirectorFlushError extends Error {
  constructor(public readonly state: DirectorSaveState, message: string) {
    super(message);
    this.name = "DirectorFlushError";
  }
}

/** 改动之后多久自动保存（与画布同一量级；拖动中途不提交，只在结束时提交一次）。 */
export const DIRECTOR_SAVE_DEBOUNCE_MS = 600;
/** 撤销上限：60 步，且快照总字节不超过 20 MB（DRD-36），超了从最旧的丢。 */
export const DIRECTOR_UNDO_LIMIT = 60;
export const DIRECTOR_UNDO_BYTES = 20 * 1024 * 1024;
/** 请求失败而浏览器仍报「在线」时（网关断了、DNS 抖动），多久之后自己对一次账：这时不会有 online 事件。 */
export const DIRECTOR_OFFLINE_RETRY_MS = 5000;

const SUBJECT = "这个场景";

// ---------------------------------------------------------------- 草稿（sessionStorage，按标签页隔离）

type DraftStorage = Pick<Storage, "getItem" | "setItem" | "removeItem">;

export function directorDraftKey(sceneId: string): string {
  return `seeaitv:director-draft:${sceneId}`;
}

function sessionStore(): DraftStorage | undefined {
  try {
    return typeof globalThis.sessionStorage === "undefined" ? undefined : globalThis.sessionStorage;
  } catch {
    // 部分浏览器在禁用存储时访问 sessionStorage 本身就会抛。
    return undefined;
  }
}

/** 写草稿；写不进去返回 false —— 调用方据此**拒绝**任何会丢本地改动的动作。 */
function writeDirectorDraft(draft: DirectorDraft): boolean {
  const storage = sessionStore();
  if (!storage || !draft.sceneId) return false;
  try {
    storage.setItem(directorDraftKey(draft.sceneId), JSON.stringify(draft));
    return true;
  } catch {
    return false;
  }
}

function readDirectorDraft(sceneId: string): DirectorDraft | null {
  const storage = sessionStore();
  if (!storage || !sceneId) return null;
  try {
    const raw = storage.getItem(directorDraftKey(sceneId));
    if (!raw) return null;
    const parsed = JSON.parse(raw) as Partial<DirectorDraft>;
    if (parsed?.sceneId !== sceneId || !parsed.scene || typeof parsed.scene !== "object") return null;
    return parsed as DirectorDraft;
  } catch {
    return null;
  }
}

function removeDirectorDraft(sceneId: string): void {
  const storage = sessionStore();
  if (!storage || !sceneId) return;
  try {
    storage.removeItem(directorDraftKey(sceneId));
  } catch {
    // 删不掉只是多留一份草稿。
  }
}

// ---------------------------------------------------------------- 纯函数

/**
 * 「场景被 X 更新」的提示文案（DR-F-155 / DR-AC-46 的原话）。三种来源同一个句式：Agent、外部 AI 客户端、人名；
 * 机器写入时不报人名 —— Agent / MCP 也带着用户本人的 ID，报人名会说成「被你自己更新」。
 */
export function directorRemoteNoticeText(notice: DirectorRemoteNotice | null | undefined): string {
  if (!notice) return "";
  if (notice.editorKind === "agent") return "场景被 Agent 更新";
  if (notice.editorKind === "mcp") return "场景被外部 AI 客户端更新";
  return notice.editorName ? `场景被 ${notice.editorName} 更新` : "场景已在别处更新";
}

/** 冲突条的说明（复用文档节点的分流与措辞，DRD-46）。 */
export function directorConflictText(details: DirectorConflict | null | undefined): string {
  return documentConflictText((details ?? {}) as Record<string, unknown>, SUBJECT);
}

function editorKindOf(v: unknown): DirectorEditorKind | undefined {
  return v === "user" || v === "agent" || v === "mcp" ? v : undefined;
}

/** 从 useApi 抛出的错误里取冲突详情（加上 currentSource / 会话 / 授权）。 */
function parseConflict(cause: unknown): DirectorConflict {
  const base = conflictDetails(cause);
  const raw = (cause as { data?: { details?: Record<string, unknown> } })?.data?.details ?? {};
  const source = editorKindOf(raw.currentSource) ?? base.editorKind;
  return {
    ...base,
    ...(source ? { currentSource: source, editorKind: base.editorKind ?? source } : {}),
    agentSessionId: typeof raw.agentSessionId === "string" ? raw.agentSessionId : "",
    mcpGrantId: typeof raw.mcpGrantId === "string" ? raw.mcpGrantId : ""
  };
}

function statusOf(cause: unknown): number | undefined {
  const s = (cause as { statusCode?: unknown })?.statusCode;
  return typeof s === "number" ? s : undefined;
}

function traceIdOf(cause: unknown): string | undefined {
  const t = (cause as { data?: { traceId?: unknown } })?.data?.traceId;
  return typeof t === "string" && t ? t : undefined;
}

function navigatorOffline(): boolean {
  return (globalThis as { navigator?: { onLine?: boolean } }).navigator?.onLine === false;
}

/**
 * 是不是「请求根本没到服务端」：浏览器报离线，或者错误里既没有业务码也没有 traceId（没有响应体）。
 * ⚠️ `useApi` 对没有响应的失败也给 statusCode 500，只看状态码分不出来；后端的真 500 一定带业务码与 traceId。
 */
function isNetworkFailure(cause: unknown): boolean {
  return navigatorOffline() || (errorCode(cause) === undefined && traceIdOf(cause) === undefined);
}

function messageOf(cause: unknown, fallback: string): string {
  const m = (cause as { message?: unknown })?.message;
  return typeof m === "string" && m ? m : fallback;
}

/** 快照占用的字节：JS 字符串是 UTF-16，按每个码元 2 字节算内存（DRD-36 的 20 MB 是内存上限）。 */
function snapshotBytes(snapshot: string): number {
  return snapshot.length * 2;
}

/** 把 `touched` 里的 AI 对象改写为用户对象（DR-F-105）；一个都没变时原样返回同一个场景。 */
function touchObjects(scene: DirectorScene, ids: readonly string[]): DirectorScene {
  if (!ids.length || !scene.objects?.length) return scene;
  const set = new Set(ids);
  let changed = false;
  const objects = scene.objects.map((o: DirectorObject) => {
    if (!set.has(o.id)) return o;
    const t = directorTouch(o);
    if (t !== o) changed = true;
    return t;
  });
  return changed ? { ...scene, objects } : scene;
}

// ---------------------------------------------------------------- store

export const useDirectorStore = defineStore("director", () => {
  const workspace = useWorkspaceStore();
  const auth = useAuthStore();

  const projectId = ref("");
  const sceneId = ref("");
  const canvasNodeId = ref("");
  /** 本地编辑中的那份场景。 */
  const scene = ref<DirectorScene | null>(null);
  const serverVersion = ref(0);
  const renderFingerprint = ref("");
  const records = ref<DirectorMediaRecord[]>([]);
  const saveState = ref<DirectorSaveState>("idle");
  const conflict = ref<DirectorConflict | null>(null);
  /** 最近一次失败的说明（`error` 态）；结构错误带 `path`。 */
  const lastError = ref<{ message: string; code?: number; path?: string } | null>(null);
  const proposal = ref<DirectorProposal | null>(null);
  const lastEditorName = ref("");
  const editorKind = ref<DirectorEditorKind>("user");
  /** 载入时发现的本标签页草稿（比服务端新）：界面询问是否恢复（DRT-15）。 */
  const pendingDraft = ref<DirectorDraft | null>(null);
  const remoteNotice = ref<DirectorRemoteNotice | null>(null);
  const loaded = ref(false);
  /** 冲突动作进行中：期间自动保存与远端事件都先不处理。 */
  const resolving = ref(false);
  /** 撤销 / 重做栈：整份场景的 JSON 快照（DRD-36）。 */
  const undoStack = ref<string[]>([]);
  const redoStack = ref<string[]>([]);
  const forbidden = ref(false);
  const newerSchema = ref(false);
  /**
   * 播放头（DRT-24）：当前帧与是否在播。**不进场景、不进撤销栈**（DR-F-120）：播放、拖动播放头不是编辑。
   * 它决定自动打帧写在哪一帧（`commit` 里的 `directorAutoKeyframe`）。
   */
  const playhead = ref(0);
  const playing = ref(false);
  /** 自动打帧被上限拦下时的说明（界面弹一次）；`seq` 让同一句话也能再弹。 */
  const timelineNotice = ref<{ text: string; seq: number } | null>(null);

  const readonlyReason = computed<DirectorReadonlyReason>(() => {
    if (newerSchema.value) return "newer";
    if (forbidden.value) return "forbidden";
    return directorCanEdit(workspace.current?.role) ? null : "role";
  });
  /** 只读（DR-F-156）：所有改动入口都拒绝。 */
  const readonly = computed(() => readonlyReason.value !== null);
  const canUndo = computed(() => undoStack.value.length > 0 && !readonly.value && !proposal.value);
  const canRedo = computed(() => redoStack.value.length > 0 && !readonly.value && !proposal.value);

  let undoBytes = 0;
  let redoBytes = 0;
  /** 本地改动的计数：每次提交 / 撤销 / 重做加一。`savedRevision` 是已保存到服务端的那一次。 */
  let revision = 0;
  let savedRevision = 0;
  let saveTimer: ReturnType<typeof setTimeout> | undefined;
  let offlineTimer: ReturnType<typeof setTimeout> | undefined;
  /** 正在进行的保存（含保存中又有改动的补存）。同一时刻只有一个 PUT。 */
  let inFlight: Promise<void> | null = null;
  let saveAgain = false;
  /** 保存中收到的远端事件：保存结束后再判断（PUT 的响应与自己保存的回声谁先到不确定）。 */
  let pendingFrame: Record<string, unknown> | null = null;
  let detach: Array<() => void> = [];
  let noticeSeq = 0;

  function api<T>(path: string, options?: Parameters<ReturnType<typeof useApi>>[1]): Promise<T> {
    return useApi()<T>(path, options);
  }

  const scenePath = () => `/projects/${projectId.value}/director-scenes/${sceneId.value}`;
  const listPath = () => `/projects/${projectId.value}/director-scenes`;
  const hasUnsaved = () => revision !== savedRevision;
  /** 当前保存状态（返回值不被调用处的类型收窄影响：保存期间状态会变）。 */
  const currentState = (): DirectorSaveState => saveState.value;

  // ---------------------------------------------------------------- 载入与替换

  /**
   * 用服务端的场景替换本地。`clearHistory` 时清空撤销栈与重做栈：栈里是整份场景快照，采纳远端内容之后还能撤销的话，
   * 一次 ⌘Z 就把同事或 Agent 的改动全部抹掉，而界面上看不出发生了什么（CLAUDE.md §5.16）。
   */
  function applyServerScene(res: SceneResponse, clearHistory: boolean) {
    const migrated = directorMigrateScene(res.scene as unknown as DirectorScene);
    newerSchema.value = "error" in migrated;
    scene.value = "error" in migrated ? (res.scene as unknown as DirectorScene) : migrated.scene;
    canvasNodeId.value = res.canvasNodeId;
    serverVersion.value = res.version;
    renderFingerprint.value = res.renderFingerprint;
    records.value = res.records ?? [];
    lastEditorName.value = res.lastEditorName ?? "";
    editorKind.value = editorKindOf(res.editorKind) ?? "user";
    if (clearHistory) clearHistoryStacks();
    savedRevision = revision;
    saveState.value = navigatorOffline() ? "offline" : "idle";
    conflict.value = null;
    lastError.value = null;
  }

  async function load(pid: string, sid: string): Promise<void> {
    reset();
    projectId.value = pid;
    sceneId.value = sid;
    const res = await api<SceneResponse>(scenePath());
    // 等待期间又打开了别的场景：这次的结果作废。
    if (projectId.value !== pid || sceneId.value !== sid) return;
    applyServerScene(res, true);
    const draft = readDirectorDraft(sid);
    if (draft && Date.parse(draft.savedAt) > Date.parse(res.updatedAt)) pendingDraft.value = draft;
    attachListeners();
    loaded.value = true;
  }

  /** 编辑器关闭时清空 state、撤销栈、计时器与监听。 */
  function reset() {
    cancelSave();
    clearOfflineRetry();
    for (const fn of detach) fn();
    detach = [];
    projectId.value = "";
    sceneId.value = "";
    canvasNodeId.value = "";
    scene.value = null;
    serverVersion.value = 0;
    renderFingerprint.value = "";
    records.value = [];
    saveState.value = "idle";
    conflict.value = null;
    lastError.value = null;
    proposal.value = null;
    playhead.value = 0;
    playing.value = false;
    timelineNotice.value = null;
    lastEditorName.value = "";
    editorKind.value = "user";
    pendingDraft.value = null;
    remoteNotice.value = null;
    loaded.value = false;
    resolving.value = false;
    forbidden.value = false;
    newerSchema.value = false;
    clearHistoryStacks();
    revision = 0;
    savedRevision = 0;
    inFlight = null;
    saveAgain = false;
    pendingFrame = null;
  }

  function attachListeners() {
    const target = (globalThis as { window?: Pick<Window, "addEventListener" | "removeEventListener"> }).window;
    if (!target?.addEventListener) return;
    const online = () => { void onOnline(); };
    const offline = () => onOffline();
    target.addEventListener("online", online);
    target.addEventListener("offline", offline);
    detach.push(() => {
      target.removeEventListener("online", online);
      target.removeEventListener("offline", offline);
    });
  }

  // ---------------------------------------------------------------- 撤销栈

  function clearHistoryStacks() {
    undoStack.value = [];
    redoStack.value = [];
    undoBytes = 0;
    redoBytes = 0;
  }

  function pushBounded(stack: string[], snapshot: string, bytes: number): number {
    stack.push(snapshot);
    let total = bytes + snapshotBytes(snapshot);
    // 上限两条都要守：60 步，且总字节不超过 20 MB；超了从最旧的丢。
    while (stack.length > DIRECTOR_UNDO_LIMIT || (total > DIRECTOR_UNDO_BYTES && stack.length > 1)) {
      total -= snapshotBytes(stack.shift() as string);
    }
    return total;
  }

  function pushUndo(snapshot: string) {
    const next = [...undoStack.value];
    undoBytes = pushBounded(next, snapshot, undoBytes);
    undoStack.value = next;
  }

  function pushRedo(snapshot: string) {
    const next = [...redoStack.value];
    redoBytes = pushBounded(next, snapshot, redoBytes);
    redoStack.value = next;
  }

  // ---------------------------------------------------------------- 改动

  /**
   * 提交一次改动：先压一步撤销，再 `scene = mutator(scene)`（DRT-11 的不可变函数），置 dirty，600ms 后保存。
   * 拖动中途不调它，只在拖拽结束时调一次（DR-F-34）。
   *
   * - 只读或有提案时直接返回 false（DR-F-156、DR-F-102）；
   * - `touched` 里的 AI 对象改写为用户对象（DR-F-105）：之后的 AI 重新搭景不再替换它；
   * - mutator 原样返回同一个场景时什么都不做（不压一步空的撤销）。
   */
  function commit(mutator: (scene: DirectorScene) => DirectorScene, options: { touched?: readonly string[] } = {}): boolean {
    if (!loaded.value || !scene.value || readonly.value || proposal.value) return false;
    const before = scene.value;
    let next = mutator(before);
    if (next === before) return false;
    if (options.touched?.length) next = touchObjects(next, options.touched);
    // 自动打帧（DRT-24）：有轨道的对象被改了，改动写成当前帧的关键帧 —— 与改动本身同一步撤销
    const keyed = directorAutoKeyframe(before, next, playhead.value);
    next = keyed.scene;
    if (keyed.error) timelineNotice.value = { text: `改动没能写成关键帧：${directorTimelineErrorText(keyed.error)}`, seq: (timelineNotice.value?.seq ?? 0) + 1 };
    pushUndo(JSON.stringify(before));
    redoStack.value = [];
    redoBytes = 0;
    scene.value = next;
    markChanged();
    return true;
  }

  function undo(): boolean {
    if (!canUndo.value || !scene.value) return false;
    const next = [...undoStack.value];
    const snapshot = next.pop() as string;
    undoBytes -= snapshotBytes(snapshot);
    undoStack.value = next;
    pushRedo(JSON.stringify(scene.value));
    scene.value = JSON.parse(snapshot) as DirectorScene;
    markChanged();
    return true;
  }

  function redo(): boolean {
    if (!canRedo.value || !scene.value) return false;
    const next = [...redoStack.value];
    const snapshot = next.pop() as string;
    redoBytes -= snapshotBytes(snapshot);
    redoStack.value = next;
    pushUndo(JSON.stringify(scene.value));
    scene.value = JSON.parse(snapshot) as DirectorScene;
    markChanged();
    return true;
  }

  /**
   * 本地有了新改动。冲突 / 离线时不发请求（冲突时必然再 409，离线时发不出去），只把草稿刷新到最新；
   * 保存中时保持「保存中」，结束后补存。
   */
  function markChanged() {
    revision += 1;
    switch (saveState.value) {
      case "conflict":
      case "offline":
        persistDraft();
        return;
      case "saving":
        saveAgain = true;
        return;
      default:
        saveState.value = "dirty";
        scheduleSave();
    }
  }

  function scheduleSave() {
    cancelSave();
    if (readonly.value || saveState.value === "conflict" || saveState.value === "offline") return;
    saveTimer = setTimeout(() => {
      saveTimer = undefined;
      void save();
    }, DIRECTOR_SAVE_DEBOUNCE_MS);
  }

  function cancelSave() {
    clearTimeout(saveTimer);
    saveTimer = undefined;
  }

  function persistDraft(): boolean {
    if (!sceneId.value || !scene.value) return false;
    return writeDirectorDraft({
      projectId: projectId.value, sceneId: sceneId.value, baseVersion: serverVersion.value,
      savedAt: new Date().toISOString(), scene: scene.value
    });
  }

  // ---------------------------------------------------------------- 保存

  /**
   * 保存。同一时刻只有一个 `PUT`：两个并发 PUT 带同一个版本号，后到的那个必然撞上前一个。
   * 保存中又有改动的话，当前这次结束后补存一次（返回的 Promise 覆盖补存）。
   */
  function save(): Promise<void> {
    cancelSave();
    if (inFlight) {
      saveAgain = true;
      return inFlight;
    }
    inFlight = (async () => {
      try {
        for (;;) {
          saveAgain = false;
          const again = await saveOnce();
          if (!(again || saveAgain) || saveState.value !== "dirty") break;
        }
      } finally {
        inFlight = null;
        drainPendingFrame();
      }
    })();
    return inFlight;
  }

  /** 发一次 PUT；返回「成功了但保存期间又有改动、需要再存一次」。 */
  async function saveOnce(): Promise<boolean> {
    if (!loaded.value || !scene.value || readonly.value || resolving.value) return false;
    // 冲突时必然再 409、离线时发不出去：都不发请求，只把草稿刷新到最新（离线的出口是对账，见 onOnline）。
    if (saveState.value === "conflict" || saveState.value === "offline") {
      persistDraft();
      return false;
    }
    if (!hasUnsaved()) {
      if (saveState.value === "dirty" || saveState.value === "error") saveState.value = "idle";
      return false;
    }
    if (navigatorOffline()) {
      enterOffline();
      return false;
    }
    // 保存前校验（与后端同一套规则、同一个 path）：坏正文不发请求，否则只会换回一个 40001 / 41301。
    const check = directorValidateScene(scene.value);
    if (!check.ok) {
      saveState.value = "error";
      lastError.value = { message: check.message, path: check.path };
      persistDraft();
      return false;
    }
    const sentRevision = revision;
    saveState.value = "saving";
    try {
      const res = await api<SceneSummary>(scenePath(), {
        method: "PUT",
        body: { scene: scene.value, version: serverVersion.value }
      });
      // ⚠️ 保存的响应是摘要、不带正文（dev/02 R-2）：本地正文就是刚发出去的那份，只更新版本与指纹。
      applySummary(res);
      savedRevision = sentRevision;
      lastError.value = null;
      if (revision === sentRevision) {
        saveState.value = "idle";
        removeDirectorDraft(sceneId.value);
        return false;
      }
      saveState.value = "dirty";
      return true;
    } catch (cause) {
      handleSaveFailure(cause);
      return false;
    }
  }

  function applySummary(res: SceneSummary) {
    serverVersion.value = res.version;
    renderFingerprint.value = res.renderFingerprint;
    lastEditorName.value = res.lastEditorName ?? "";
    editorKind.value = editorKindOf(res.editorKind) ?? "user";
  }

  /**
   * 保存失败之后进哪个态。⚠️ 冲突按**业务码 40902** 判，不按 HTTP 409：40905（项目在回收站）也是 409，
   * 当成冲突的话用户点「以我为准」会永远失败。任何失败都先把本地场景写进草稿。
   */
  function handleSaveFailure(cause: unknown) {
    const code = errorCode(cause);
    if (code === VERSION_CONFLICT) {
      enterConflict(parseConflict(cause));
      return;
    }
    if (statusOf(cause) === 403) {
      // 后端兜底（DR-F-156）：角色在打开期间被改成只读，或本来就判错了。进只读并停存。
      forbidden.value = true;
      cancelSave();
      saveState.value = "error";
      lastError.value = { message: "你在该工作区是只读成员，改动没有保存", code };
      persistDraft();
      return;
    }
    if (isNetworkFailure(cause)) {
      enterOffline();
      return;
    }
    const path = (cause as { data?: { details?: { path?: unknown } } })?.data?.details?.path;
    saveState.value = "error";
    lastError.value = { message: messageOf(cause, "保存失败"), code, ...(typeof path === "string" ? { path } : {}) };
    persistDraft();
  }

  function enterConflict(details: DirectorConflict) {
    cancelSave();
    clearOfflineRetry();
    conflict.value = details;
    saveState.value = "conflict";
    persistDraft();
  }

  /**
   * 截图与关闭前调用（DR-F-73、DR-F-154）：取消防抖，立即保存。成功时返回最新的服务端版本；
   * 冲突 / 失败 / 离线时 reject 并附原因 —— 截图要用「已保存的版本」做 `sceneVersion`，否则服务端回 40902。
   */
  async function flush(): Promise<number> {
    cancelSave();
    if (!loaded.value || !scene.value) throw new DirectorFlushError("error", "场景还没有载入");
    if (saveState.value === "conflict") throw new DirectorFlushError("conflict", "场景有冲突，请先处理冲突");
    if (saveState.value === "offline") throw new DirectorFlushError("offline", "网络已断开，恢复之后才能保存");
    if (hasUnsaved()) {
      if (saveState.value === "error") saveState.value = "dirty";
      await save();
    } else if (inFlight) {
      await inFlight;
    }
    // ⚠️ 重新读一次状态：await 之后 TS 仍按开头的判断收窄 saveState.value，而它在保存期间可能已经变了。
    const state = currentState();
    if (!hasUnsaved() && state === "idle") return serverVersion.value;
    switch (state) {
      case "conflict":
        throw new DirectorFlushError("conflict", "场景有冲突，请先处理冲突");
      case "offline":
        throw new DirectorFlushError("offline", "网络已断开，恢复之后才能保存");
      default:
        throw new DirectorFlushError(state, lastError.value?.message ?? "场景没有保存成功");
    }
  }

  /** 失败或离线之后手动重试（界面上的「重试」）。 */
  async function retry(): Promise<void> {
    if (saveState.value === "offline") return onOnline(true);
    if (saveState.value !== "error" || readonly.value) return;
    saveState.value = hasUnsaved() ? "dirty" : "idle";
    lastError.value = null;
    if (hasUnsaved()) await save();
  }

  // ---------------------------------------------------------------- 冲突

  /**
   * 处理冲突（DR-AC-11）。两条路都先确认草稿写进了 sessionStorage，写不进去就**拒绝**（否则就是静默丢弃用户的改动）。
   *
   * - `adopt` 载入最新：GET 后替换，**清空撤销栈与重做栈**（DR-F-151）；刚写的草稿留给界面询问是否恢复；
   * - `overwrite` 以我为准：先取当前版本（摘要），再 PUT 本地内容。期间又有人存了 → 回到冲突态，不自动循环重试。
   */
  async function resolveConflict(mode: "adopt" | "overwrite"): Promise<"resolved" | "conflict"> {
    if (saveState.value !== "conflict" || resolving.value || !loaded.value || !scene.value) return "conflict";
    if (!persistDraft()) {
      throw new Error("浏览器无法保存本地草稿（存储空间不足或被禁用），为避免丢失你的改动，暂不能处理冲突");
    }
    cancelSave();
    resolving.value = true;
    try {
      if (mode === "adopt") {
        const res = await api<SceneResponse>(scenePath());
        applyServerScene(res, true);
        pendingDraft.value = readDirectorDraft(sceneId.value);
        return "resolved";
      }
      const check = directorValidateScene(scene.value);
      if (!check.ok) throw new Error(`场景不合法，不能保存：${check.path}：${check.message}`);
      const list = await api<{ items: SceneSummary[] }>(listPath());
      const current = list.items.find((s) => s.id === sceneId.value);
      if (!current) throw new Error("场景不存在或已被删除");
      const sentRevision = revision;
      try {
        const res = await api<SceneSummary>(scenePath(), {
          method: "PUT",
          body: { scene: scene.value, version: current.version }
        });
        applySummary(res);
        savedRevision = sentRevision;
        conflict.value = null;
        lastError.value = null;
        if (revision === sentRevision) {
          saveState.value = "idle";
          removeDirectorDraft(sceneId.value);
        } else {
          saveState.value = "dirty";
          scheduleSave();
        }
        return "resolved";
      } catch (cause) {
        if (errorCode(cause) === VERSION_CONFLICT) {
          enterConflict(parseConflict(cause));
          return "conflict";
        }
        throw cause;
      }
    } finally {
      resolving.value = false;
      drainPendingFrame();
    }
  }

  /** 用载入时发现的草稿替换本地场景（撤销栈里留一步，可撤回），然后保存。 */
  async function restoreDraft(): Promise<boolean> {
    const draft = pendingDraft.value;
    if (!draft || saveState.value === "conflict") return false;
    pendingDraft.value = null;
    if (!commit(() => draft.scene)) return false;
    await save();
    return true;
  }

  function discardDraft() {
    if (sceneId.value) removeDirectorDraft(sceneId.value);
    pendingDraft.value = null;
  }

  // ---------------------------------------------------------------- 离线（DR-F-153）

  function enterOffline() {
    cancelSave();
    if (saveState.value === "conflict") return;
    saveState.value = "offline";
    if (hasUnsaved()) persistDraft();
    // 浏览器仍报「在线」（网关断了、DNS 抖动）时不会有 online 事件：过一会儿自己对一次账。
    if (!navigatorOffline()) scheduleOfflineRetry();
  }

  function scheduleOfflineRetry() {
    clearOfflineRetry();
    offlineTimer = setTimeout(() => {
      offlineTimer = undefined;
      void onOnline();
    }, DIRECTOR_OFFLINE_RETRY_MS);
  }

  function clearOfflineRetry() {
    clearTimeout(offlineTimer);
    offlineTimer = undefined;
  }

  function onOffline() {
    if (loaded.value) enterOffline();
  }

  /**
   * 恢复之后先对账，再决定补存还是进冲突（DR-AC-13）：
   * - 服务端版本没变：有改动就补存；
   * - 变了：有改动直接进冲突（**不发那个注定 409 的请求**）；没有改动就载入最新（同远端更新的自动采纳）；
   * - 对账本身失败：回到 error，不盲发。
   */
  async function onOnline(manual = false): Promise<void> {
    clearOfflineRetry();
    if (!loaded.value || saveState.value !== "offline") return;
    if (navigatorOffline() && !manual) return;
    let summary: SceneSummary | undefined;
    try {
      const list = await api<{ items: SceneSummary[] }>(listPath());
      summary = list.items.find((s) => s.id === sceneId.value);
    } catch (cause) {
      saveState.value = "error";
      lastError.value = { message: `网络恢复后对账失败：${messageOf(cause, "请求失败")}`, code: errorCode(cause) };
      if (hasUnsaved()) persistDraft();
      return;
    }
    if (!summary) {
      saveState.value = "error";
      lastError.value = { message: "场景不存在或已被删除" };
      if (hasUnsaved()) persistDraft();
      return;
    }
    if (summary.version === serverVersion.value) {
      if (hasUnsaved()) {
        saveState.value = "dirty";
        await save();
      } else {
        saveState.value = "idle";
      }
      return;
    }
    if (hasUnsaved()) {
      enterConflict({
        currentVersion: summary.version, currentUpdatedAt: summary.updatedAt, currentEditorId: "",
        currentEditorName: summary.lastEditorName ?? "", bySelf: false,
        ...(editorKindOf(summary.editorKind) ? { editorKind: summary.editorKind, currentSource: summary.editorKind } : {})
      });
      return;
    }
    saveState.value = "idle";
    await adoptRemote();
  }

  // ---------------------------------------------------------------- 实时事件（DR-F-155）

  /**
   * 处理 `director.scene_updated`（只作对账信号，不拿事件当正文）。
   * - 不是本场景，或版本不比本地新：忽略；
   * - 保存中：先缓存这一帧，保存结束后再判断 —— 否则会把自己保存的回声当成别人的改动；
   * - 没有本地改动：GET → 替换 → 清空撤销栈，并提示「场景被 X 更新」；
   * - 有本地改动：进冲突。
   */
  function handleRemoteUpdate(frame: Record<string, unknown>): void {
    if (frame?.type !== "director.scene_updated" || !loaded.value) return;
    if (String(frame.sceneId ?? "") !== sceneId.value) return;
    const version = Number(frame.version);
    if (!Number.isFinite(version) || version <= serverVersion.value) return;
    if (inFlight || resolving.value) {
      if (!pendingFrame || version > Number(pendingFrame.version)) pendingFrame = frame;
      return;
    }
    void evaluateRemote(frame);
  }

  function drainPendingFrame() {
    const frame = pendingFrame;
    pendingFrame = null;
    if (frame) void evaluateRemote(frame);
  }

  async function evaluateRemote(frame: Record<string, unknown>): Promise<void> {
    const version = Number(frame.version);
    // 自己保存的回声：保存回来之后本地版本已经追上了它。
    if (version <= serverVersion.value || saveState.value === "conflict") return;
    const kind = editorKindOf(frame.editorKind);
    if (hasUnsaved()) {
      const editorId = typeof frame.editorId === "string" ? frame.editorId : "";
      const me = String(auth.user?.id ?? "");
      enterConflict({
        currentVersion: version,
        currentUpdatedAt: typeof frame.occurredAt === "string" ? frame.occurredAt : "",
        currentEditorId: editorId,
        currentEditorName: typeof frame.editorName === "string" ? frame.editorName.trim() : "",
        // 事件不带 bySelf：只有「人写的、且是同一个用户」才算本人（同后端口径）。
        bySelf: kind === "user" && !!editorId && editorId === me,
        ...(kind ? { editorKind: kind, currentSource: kind } : {})
      });
      return;
    }
    if (saveState.value === "offline") return;
    await adoptRemote();
  }

  /** 没有本地改动时载入最新（自动采纳），清空撤销栈并提示。期间用户又改了东西就改进冲突，不覆盖。 */
  async function adoptRemote(): Promise<void> {
    const startRevision = revision;
    let res: SceneResponse;
    try {
      res = await api<SceneResponse>(scenePath());
    } catch {
      // 取不到最新就保持现状：事件只是信号，下一次保存会撞上冲突并说清是谁改的。
      return;
    }
    if (revision !== startRevision) {
      enterConflict({
        currentVersion: res.version, currentUpdatedAt: res.updatedAt, currentEditorId: res.lastEditorId ?? "",
        currentEditorName: res.lastEditorName ?? "", bySelf: false,
        ...(editorKindOf(res.editorKind) ? { editorKind: res.editorKind, currentSource: res.editorKind } : {})
      });
      return;
    }
    if (res.version <= serverVersion.value) return;
    applyServerScene(res, true);
    // 基于旧场景算出来的提案已经作废：应用它会把刚载入的远端改动覆盖掉。
    proposal.value = null;
    noticeSeq += 1;
    remoteNotice.value = { version: res.version, editorName: res.lastEditorName ?? "",
      editorKind: editorKindOf(res.editorKind) ?? "user", seq: noticeSeq };
  }

  // ---------------------------------------------------------------- 截图记录（DRT-17）

  /**
   * 截图 / 预演上传撞上版本冲突（服务端回 40902，详情与场景保存同一套，dev/03 R-2 #2）：进冲突流程。
   * 返回是不是冲突 —— 不是的话调用方照常提示失败原因。
   */
  function handleUploadConflict(cause: unknown): boolean {
    if (errorCode(cause) !== VERSION_CONFLICT) return false;
    enterConflict(parseConflict(cause));
    return true;
  }

  /** 新截的记录插到最前面（记录按时间倒序）；同一条不重复。 */
  function addRecords(next: readonly DirectorMediaRecord[]) {
    const ids = new Set(next.map(record => record.id));
    records.value = [...[...next].reverse(), ...records.value.filter(record => !ids.has(record.id))];
  }

  /** 隐藏 / 取消隐藏一条记录（只是不在截图条里显示，文件不删，DR-F-75）。只读时不发请求。 */
  async function setRecordHidden(recordId: string, hidden: boolean): Promise<void> {
    if (readonly.value) return;
    const updated = await api<DirectorMediaRecord>(`${scenePath()}/records/${recordId}`, { method: "PATCH", body: { hidden } });
    records.value = records.value.map(record => (record.id === recordId ? { ...record, ...updated } : record));
  }

  // ---------------------------------------------------------------- AI 提案（DR-F-102、DRK-7）

  /**
   * 发起一次 AI 搭景（DRT-20，dev/03 §8）：先 `flush()`（带的 `sceneVersion` 要是刚存上的版本，DR-F-101），
   * 再 `POST .../build`。返回摊平后的提案，**不在这里设成当前提案** —— 用户可能已经点了「不再等待」，
   * 由调用方决定要不要 `setProposal`。失败原样抛出，文案由 `directorBuildErrorText` 按业务码给。
   */
  async function requestBuild(input: { prompt: string; mediaIds: readonly string[] }): Promise<DirectorProposal> {
    if (readonly.value) throw new DirectorFlushError("error", "你在该工作区是只读，不能 AI 搭景");
    const sceneVersion = await flush();
    const res = await api<BuildResult>(`${scenePath()}/build`, {
      method: "POST",
      body: { prompt: input.prompt, referenceMediaIds: [...input.mediaIds], sceneVersion }
    });
    return {
      scene: res.proposal.scene as DirectorScene,
      changes: res.proposal.changes,
      defaults: res.proposal.defaults,
      baseVersion: res.baseVersion,
      callId: res.callId,
      modelCode: res.modelCode
    };
  }

  /** 收到搭景提案：编辑锁定，直到应用或放弃。 */
  function setProposal(next: DirectorProposal): boolean {
    if (!loaded.value || readonly.value) return false;
    proposal.value = next;
    return true;
  }

  function discardProposal() {
    proposal.value = null;
  }

  /**
   * 应用提案：只记一步撤销（撤销一次就回到搭景前），然后立即保存。
   * ⚠️ 场景在搭景期间被改过（版本不是提案的 baseVersion，或本地还有没存的改动）时拒绝：
   *    提案是基于旧场景算的，应用它会把那些改动覆盖掉。
   */
  async function applyProposal(): Promise<boolean> {
    const p = proposal.value;
    if (!p || readonly.value || !scene.value) return false;
    if (p.baseVersion !== serverVersion.value || hasUnsaved()) {
      proposal.value = null;
      throw new Error("场景在 AI 搭景期间被修改过，这份方案已作废，请重新搭景");
    }
    proposal.value = null;
    if (!commit(() => p.scene)) return false;
    await save();
    return true;
  }

  return {
    projectId, sceneId, canvasNodeId, scene, serverVersion, renderFingerprint, records, saveState, conflict,
    lastError, proposal, lastEditorName, editorKind, pendingDraft, remoteNotice, loaded, resolving, undoStack, redoStack,
    playhead, playing, timelineNotice,
    readonly, readonlyReason, canUndo, canRedo,
    load, reset, commit, undo, redo, save, flush, retry, resolveConflict, restoreDraft, discardDraft,
    handleRemoteUpdate, setProposal, discardProposal, applyProposal,
    handleUploadConflict, addRecords, setRecordHidden, requestBuild
  };
});
