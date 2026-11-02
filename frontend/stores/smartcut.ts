import type { components } from "@tabtv/api-client";
import { VERSION_CONFLICT, errorCode } from "~/utils/asyncState";
import { roleAllowsScope } from "~/utils/roleScope";
import { smartcutValidateContent, smartcutApplyIdMap, smartcutRemapSnapshots, smartcutMapId } from "~/utils/smartcutContent";
import { smartcutContentOf, smartcutRelayout, smartcutSameContent, smartcutTempId } from "~/utils/smartcutModel";
import { smartcutConflictDetails, type SmartcutConflict, type SmartcutEditorKind, type SmartcutPlanKind } from "~/utils/smartcutPlan";
import {
  smartcutClaimGenerationFrame,
  smartcutGenerationOutputKind,
  smartcutPlaceGenerated,
  type SmartcutClipTool
} from "~/utils/smartcutClipGen";

/**
 * 智能剪辑的剪辑 store（docs/agent/smartcut SCT-19，dev/03 §7，SCD-43）：剪辑台的**唯一状态来源**。
 *
 * 规则照导演台 store（`stores/director.ts`）写，实现独立（两边节奏不同，抽公共基类会让一边的修复牵连另一边）：
 * 防抖 600ms 整份保存、同一时刻只一个 PUT、冲突按业务码 40902 判、任何失败先写 sessionStorage 草稿、
 * 撤销栈存整份 JSON（60 步 / 20 MB）、离线停存且恢复后先对账、实时事件只当对账信号。
 *
 * 与导演台不同的几处：
 * - 保存的请求体是 `TimelineContent`（{@link smartcutContentOf}），响应带完整时间线与 `idMap`：
 *   成功后按 `idMap` 改写本地内容、撤销栈、重做栈与选中项里的临时 ID —— 不改写的话撤销一步就回到带临时 ID 的内容，
 *   再保存时服务端把它们当成新对象又建一遍（片段翻倍，SC-AC-10）；
 * - 本地改动之后立即重算落点（{@link smartcutRelayout}），保存回来之后以服务端的时间线为准；
 * - AI 剪辑、方案切换、快照恢复、片段回填的结果已经在服务端写好了，本地用 {@link applyServerTimeline} 记一步撤销。
 *
 * ⚠️ 本 store 不取媒体（`useAuthenticatedMedia` 注册 onScopeDispose，在 store 里调用是错的）。
 */

type Timeline = components["schemas"]["Timeline"];
type SaveResult = components["schemas"]["TimelineSaveResult"];
type Summary = components["schemas"]["SmartcutSummary"];
type SnapshotMeta = components["schemas"]["TimelineSnapshotMeta"];
type RestoreResult = components["schemas"]["TimelineRestoreResult"];
type PlanRequest = components["schemas"]["SmartcutPlanRequest"];
type PlanResult = components["schemas"]["SmartcutPlanResult"];
type SyncResult = components["schemas"]["SmartcutSyncResult"];
type Quote = components["schemas"]["SmartcutQuote"];
type TranscribeResult = components["schemas"]["SmartcutTranscribeResult"];
type TranslateResult = components["schemas"]["SmartcutTranslateResult"];
/** 识别的选项（SCT-36，契约 transcribeSmartcut 的请求体，不含 dryRun）。 */
export type SmartcutTranscribeOptions = {
  clipIds?: string[];
  modelCode: string;
  keepFillers: boolean;
  language?: string;
  subtitles: boolean;
};
export type SmartcutStoreInput = components["schemas"]["SmartcutInput"];
export type SmartcutStoreConfig = components["schemas"]["SmartcutConfig"];
export type SmartcutStoreGeneration = components["schemas"]["SmartcutGeneration"];
/**
 * 一次片段级生成的请求（SCT-40）：工具、作用的片段（重拍 / 去字幕 / 音效）或位置（补镜的 `afterClipId`、参考帧的
 * `refFromClipId`），以及请求体里的其余字段。只存在内存里：失败后「重试」按它重新打开确认框（重新报价、换一个新键）。
 */
export type SmartcutClipRequest = {
  tool: SmartcutClipTool;
  clipId?: string;
  afterClipId?: string;
  refFromClipId?: string;
  body: Record<string, unknown>;
};
/** 一次排版的结果：PNG 的 object URL、它在画面里的像素框、白名单字体里缺的字（SCD-46）。 */
export type SmartcutTypesetEntry = { url: string; box: { x: number; y: number; w: number; h: number }; missing: string };

/** 保存状态。`idle` = 已保存。`conflict` 与 `error` 的处置完全不同，不能合并。 */
export type SmartcutSaveState = "idle" | "dirty" | "saving" | "error" | "conflict" | "offline";

/** 只读的原因：`role` 角色不允许编辑 · `forbidden` 后端回了 403（打开期间角色被改成只读）。 */
export type SmartcutReadonlyReason = "role" | "forbidden" | null;

/** 本地草稿：冲突、失败、离线时把本地时间线存进 sessionStorage，任何路径都不静默丢弃用户的改动。 */
export type SmartcutDraft = {
  projectId: string;
  timelineId: string;
  /** 草稿基于的服务端版本。 */
  baseVersion: number;
  savedAt: string;
  timeline: Timeline;
};

/** 载入时发现的草稿：`serverChanged` = 草稿之后服务端又被改过，恢复它会覆盖那些改动（界面要说出来）。 */
export type SmartcutPendingDraft = SmartcutDraft & { serverChanged: boolean };

/** 最近一次自动采纳的远端更新（界面据此提示「时间线被 X 更新」）。`seq` 让同一句话也能再弹。 */
export type SmartcutStoreNotice = {
  version: number;
  editorKind: SmartcutEditorKind;
  editorName: string;
  bySelf: boolean;
  reason: string;
  seq: number;
};

/** 选中项：片段 / 字幕 / 文字层（轨道里各自的 ID）。不进内容、不进撤销栈。 */
export type SmartcutSelection = { kind: "clip" | "subtitle" | "text"; ids: string[] } | null;

/** AI 剪辑的状态（结果卡、方案切换、撤销这轮 AI 用）。 */
export type SmartcutPlanState = {
  callId: string;
  stage: string;
  /** 这一轮是剪一版（plan）还是追加指令（edit）；切换方案沿用原来的。 */
  kind: "plan" | "edit";
  /** 剪辑方式（结果卡的兜底说明按它说）；追加指令时为空。 */
  mode: string;
  variants: PlanResult["variants"];
  appliedVariant: number | null;
  /** 写入前存下的快照：「撤销这轮 AI」恢复到它。 */
  snapshotId: string;
  /** AI 写入之后的服务端版本：之后版本又涨了就说明有了手动改动，撤销这轮 AI 前要先确认。 */
  appliedVersion: number;
  changes: string[];
};

/** 进行中的 AI 调用（请求发出到拿到响应）：阶段由 `smartcut.updated` 推进，`callId` 在第一帧事件里拿到。 */
export type SmartcutPlanRunning = { kind: SmartcutPlanKind; stage: string; callId: string; startedAt: number };

/**
 * AI 剪辑期间时间线被改过（40902）：方案已经生成并存在服务端的调用记录里，「仍然应用」用 `callId` 强制写入
 * （`/plan/{callId}/apply` + `force`，不重新调模型）。
 */
export type SmartcutPlanConflict = { kind: SmartcutPlanKind; callId: string; variant: number; details: SmartcutConflict };

/** flush 失败：带着当时的保存状态，收起 / 关闭 / 导出 / AI 剪辑据此说清该怎么处理。 */
export class SmartcutFlushError extends Error {
  constructor(public readonly state: SmartcutSaveState, message: string) {
    super(message);
    this.name = "SmartcutFlushError";
  }
}

/** 改动之后多久自动保存（拖动中途不提交，只在松手时提交一次）。 */
export const SMARTCUT_SAVE_DEBOUNCE_MS = 600;
/** 撤销上限：60 步，且快照总字节不超过 20 MB，超了从最旧的丢。 */
export const SMARTCUT_UNDO_LIMIT = 60;
export const SMARTCUT_UNDO_BYTES = 20 * 1024 * 1024;
/** 请求失败而浏览器仍报「在线」时（网关断了、DNS 抖动），多久之后自己对一次账。 */
export const SMARTCUT_OFFLINE_RETRY_MS = 5000;
/** 快照里算「AI 写入」的几种原因（刷新之后找最近一次 AI 快照，SC-AC-33）。 */
const AI_SNAPSHOT_REASONS: ReadonlySet<string> = new Set(["ai_plan", "ai_edit", "ai_variant"]);

// ---------------------------------------------------------------- 草稿（sessionStorage，按标签页隔离）

type DraftStorage = Pick<Storage, "getItem" | "setItem" | "removeItem">;

export function smartcutDraftKey(timelineId: string): string {
  return `seeaitv:smartcut-draft:${timelineId}`;
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
function writeDraft(draft: SmartcutDraft): boolean {
  const storage = sessionStore();
  if (!storage || !draft.timelineId) return false;
  try {
    storage.setItem(smartcutDraftKey(draft.timelineId), JSON.stringify(draft));
    return true;
  } catch {
    return false;
  }
}

function readDraft(timelineId: string): SmartcutDraft | null {
  const storage = sessionStore();
  if (!storage || !timelineId) return null;
  try {
    const raw = storage.getItem(smartcutDraftKey(timelineId));
    if (!raw) return null;
    const parsed = JSON.parse(raw) as Partial<SmartcutDraft>;
    if (parsed?.timelineId !== timelineId || !parsed.timeline || !Array.isArray(parsed.timeline.tracks)) return null;
    return parsed as SmartcutDraft;
  } catch {
    return null;
  }
}

function removeDraft(timelineId: string): void {
  const storage = sessionStore();
  if (!storage || !timelineId) return;
  try {
    storage.removeItem(smartcutDraftKey(timelineId));
  } catch {
    // 删不掉只是多留一份草稿。
  }
}

// ---------------------------------------------------------------- 错误分类

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

/** 快照占用的字节：JS 字符串是 UTF-16，按每个码元 2 字节算内存（20 MB 是内存上限）。 */
function snapshotBytes(snapshot: string): number {
  return snapshot.length * 2;
}

const EDITOR_KINDS: ReadonlySet<string> = new Set(["user", "ai", "agent", "mcp", "sync", "system"]);

function editorKindOf(v: unknown): SmartcutEditorKind {
  return typeof v === "string" && EDITOR_KINDS.has(v) ? v as SmartcutEditorKind : "user";
}

/** 时间线里还有没有这个对象（采纳远端内容之后，选中项里不在了的要去掉）。 */
function hasObject(t: Timeline, kind: NonNullable<SmartcutSelection>["kind"], id: string): boolean {
  if (kind === "clip") return t.tracks.some(track => (track.clips ?? []).some(clip => clip.id === id));
  if (kind === "subtitle") return (t.subtitles ?? []).some(sub => sub.id === id);
  return (t.texts ?? []).some(text => text.id === id);
}

// ---------------------------------------------------------------- store

export const useSmartcutStore = defineStore("smartcut", () => {
  const workspace = useWorkspaceStore();
  const auth = useAuthStore();

  const projectId = ref("");
  const timelineId = ref("");
  /** 本地编辑中的那份时间线（服务端的形状，落点由本地重算，保存后以服务端为准）。 */
  const timeline = ref<Timeline | null>(null);
  const serverVersion = ref(0);
  const saveState = ref<SmartcutSaveState>("idle");
  const conflict = ref<SmartcutConflict | null>(null);
  /** 最近一次失败的说明（`error` 态）；校验错误带 `path`（`tracks[1].clips[3].speed`）。 */
  const lastError = ref<{ message: string; code?: number; path?: string } | null>(null);
  const pendingDraft = ref<SmartcutPendingDraft | null>(null);
  const remoteNotice = ref<SmartcutStoreNotice | null>(null);
  const loaded = ref(false);
  /** 冲突动作进行中：期间自动保存与远端事件都先不处理。 */
  const resolving = ref(false);
  const undoStack = ref<string[]>([]);
  const redoStack = ref<string[]>([]);
  const forbidden = ref(false);

  /** 素材条（`GET .../inputs`，以服务端为准，SCR-11）。 */
  const inputs = ref<SmartcutStoreInput[]>([]);
  const inputsFingerprint = ref("");
  const inputsOmitted = ref(0);
  const config = ref<SmartcutStoreConfig | null>(null);
  const plan = ref<SmartcutPlanState | null>(null);
  const planRunning = ref<SmartcutPlanRunning | null>(null);
  const planConflict = ref<SmartcutPlanConflict | null>(null);
  /** 最近一次剪一版的请求（不含版本）：42217 / 素材不够的出路「允许放慢」等在它上面加开关重发。 */
  const lastPlanRequest = ref<Omit<PlanRequest, "baseVersion"> | null>(null);
  const snapshots = ref<SnapshotMeta[]>([]);
  // 刚恢复过的快照：快照列表刷新回来之前，它还排在最上面，不能再当成「可撤销的这轮 AI」。
  const restoredSnapshotId = ref("");
  /** 片段级生成与识别的进度（按 generationId，SCT-40 / SCT-33 写入）。 */
  const generations = ref<Record<string, SmartcutStoreGeneration>>({});
  /** 本次打开以来受理的片段级生成请求（按 generationId，重试用；不持久化）。 */
  const clipRequests = ref<Record<string, SmartcutClipRequest>>({});
  /** 生成记录的翻页游标：null = 还没取过，"" = 没有更多了。 */
  const generationsCursor = ref<string | null>(null);
  /** 排版结果缓存：内容哈希 → PNG 的 object URL 与位置框（SCD-46，SCT-21 读写）。剪辑台关闭时一并释放。 */
  const typesetCache = ref<Record<string, SmartcutTypesetEntry>>({});

  /** 播放头与选中项：**不进内容、不进撤销栈**（播放、拖动播放头、选中都不是编辑）。 */
  const playheadMs = ref(0);
  const playing = ref(false);
  const selection = ref<SmartcutSelection>(null);

  const readonlyReason = computed<SmartcutReadonlyReason>(() => {
    if (forbidden.value) return "forbidden";
    return roleAllowsScope(workspace.current?.role ?? undefined, "seeaitv:edit") ? null : "role";
  });
  /** 只读：所有改动入口都拒绝（SC-AC-66）。 */
  const readonly = computed(() => readonlyReason.value !== null);
  const canUndo = computed(() => undoStack.value.length > 0 && !readonly.value);
  const canRedo = computed(() => redoStack.value.length > 0 && !readonly.value);
  /** 本地有没有还没存上的改动（界面的「有未保存的改动」与关闭前的确认用）。 */
  const unsaved = ref(false);

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
  /**
   * 进行中的「服务端写入」请求数（AI 剪辑、方案切换、快照恢复、原位替换）。期间到达的 `timeline.updated` 与保存中一样先缓存：
   * 服务端先推事件再回响应，事件先到的话会被当成别人的改动自动采纳 —— 清空撤销栈、弹「被 AI 剪辑更新」，
   * 而这一步本该是用户自己能 ⌘Z 撤回的一步。
   */
  let writing = 0;
  let detach: Array<() => void> = [];
  let noticeSeq = 0;
  /** 每次载入加一：晚到的素材条 / 配置 / 快照响应按它丢弃。 */
  let loadSeq = 0;
  /**
   * 本次打开以来所有「临时 ID → 正式 ID」：拖动、对话框这类**跨过一次保存**的操作，开始时拿到的是临时 ID，
   * 松手时它已经被换掉了 —— 不换算的话松手那一下报「片段不存在」（SCT-20 浏览器自查时撞上）。
   */
  const idAliases = new Map<string, string>();

  function api<T>(path: string, options?: Parameters<ReturnType<typeof useApi>>[1]): Promise<T> {
    return useApi()<T>(path, options);
  }

  const timelinePath = () => `/timelines/${timelineId.value}`;
  const smartcutPath = () => `/projects/${projectId.value}/smartcuts/${timelineId.value}`;
  const listPath = () => `/projects/${projectId.value}/smartcuts`;
  const hasUnsaved = () => revision !== savedRevision;
  const syncUnsaved = () => { unsaved.value = hasUnsaved(); };
  /** 当前保存状态（返回值不被调用处的类型收窄影响：保存期间状态会变）。 */
  const currentState = (): SmartcutSaveState => saveState.value;

  // ---------------------------------------------------------------- 载入与替换

  /**
   * 用服务端的时间线替换本地。`clearHistory` 时清空撤销栈与重做栈：栈里是整份快照，采纳别人的内容之后还能撤销的话，
   * 一次 ⌘Z 就把同事、Agent 或 AI 的改动全部抹掉，而界面上看不出发生了什么（CLAUDE.md §5.16）。
   */
  function replaceWithServer(res: Timeline, clearHistory: boolean) {
    timeline.value = res;
    serverVersion.value = res.version;
    if (clearHistory) clearHistoryStacks();
    savedRevision = revision;
    syncUnsaved();
    saveState.value = navigatorOffline() ? "offline" : "idle";
    conflict.value = null;
    lastError.value = null;
    pruneSelection();
  }

  async function load(pid: string, tid: string): Promise<void> {
    reset();
    projectId.value = pid;
    timelineId.value = tid;
    const seq = ++loadSeq;
    const res = await api<Timeline>(timelinePath());
    // 等待期间又打开了别的时间线：这次的结果作废。
    if (seq !== loadSeq) return;
    replaceWithServer(res, true);
    const draft = readDraft(tid);
    if (draft && !smartcutSameContent(draft.timeline, res)) {
      pendingDraft.value = { ...draft, serverChanged: draft.baseVersion !== res.version };
    } else if (draft) {
      removeDraft(tid);
    }
    attachListeners();
    loaded.value = true;
    // 素材条、配置、快照不挡载入：任何一个取不到（例如 AI 剪辑还没上线时配置接口不存在）都只是那一块空着。
    // 生成记录也在这里取：重新打开剪辑台时，还在生成 / 失败了的片段级生成要能看到（状态只在内存里）。
    void Promise.allSettled([refreshInputs(), loadConfig(), refreshSnapshots(), refreshGenerations()]);
  }

  /** 剪辑台关闭时清空 state、撤销栈、计时器、监听与排版缓存。 */
  function reset() {
    cancelSave();
    clearOfflineRetry();
    for (const fn of detach) fn();
    detach = [];
    loadSeq += 1;
    restoredSnapshotId.value = "";
    projectId.value = "";
    timelineId.value = "";
    timeline.value = null;
    serverVersion.value = 0;
    saveState.value = "idle";
    conflict.value = null;
    lastError.value = null;
    pendingDraft.value = null;
    remoteNotice.value = null;
    loaded.value = false;
    resolving.value = false;
    forbidden.value = false;
    inputs.value = [];
    inputsFingerprint.value = "";
    inputsOmitted.value = 0;
    config.value = null;
    plan.value = null;
    planRunning.value = null;
    planConflict.value = null;
    lastPlanRequest.value = null;
    snapshots.value = [];
    generations.value = {};
    clipRequests.value = {};
    generationsCursor.value = null;
    releaseTypeset();
    playheadMs.value = 0;
    playing.value = false;
    selection.value = null;
    clearHistoryStacks();
    revision = 0;
    savedRevision = 0;
    unsaved.value = false;
    inFlight = null;
    saveAgain = false;
    pendingFrame = null;
    writing = 0;
    idAliases.clear();
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

  async function refreshInputs(): Promise<void> {
    if (!projectId.value || !timelineId.value) return;
    const seq = loadSeq;
    const res = await api<{ items: SmartcutStoreInput[]; omitted?: number; fingerprint: string }>(`${smartcutPath()}/inputs`);
    if (seq !== loadSeq) return;
    inputs.value = res.items ?? [];
    inputsOmitted.value = res.omitted ?? 0;
    inputsFingerprint.value = res.fingerprint ?? "";
  }

  /**
   * 按已保存的画布重新解析输入（SCD-49）。剪辑台开着时用 `listOnly`（只更新素材条，不动轨道）；
   * `autofill` 由画布侧调用，返回的结果里带新版本，随后的 `timeline.updated` 事件会把正文带过来（本地没有改动时自动采纳）；
   * `replaceChanged` 是用户在剪辑台里点的，交给 {@link replaceChanged}（写好之后记一步撤销）。
   */
  async function sync(mode: "listOnly" | "autofill" | "replaceChanged" = "listOnly"): Promise<SyncResult | null> {
    if (!loaded.value) return null;
    if (mode === "replaceChanged") return replaceChanged();
    const seq = loadSeq;
    const body: Record<string, unknown> = { mode };
    if (mode !== "listOnly" && readonly.value) return null;
    const res = await serverWrite(() => api<SyncResult>(`${smartcutPath()}/sync`, { method: "POST", body }));
    if (seq !== loadSeq) return null;
    inputs.value = res.inputs ?? [];
    inputsOmitted.value = res.omitted ?? 0;
    inputsFingerprint.value = res.fingerprint ?? "";
    return res;
  }

  async function loadConfig(): Promise<void> {
    const seq = loadSeq;
    const res = await api<SmartcutStoreConfig>("/smartcut/config");
    if (seq !== loadSeq) return;
    config.value = res;
  }

  async function refreshSnapshots(): Promise<void> {
    if (!timelineId.value) return;
    const seq = loadSeq;
    const res = await api<{ items: SnapshotMeta[] }>(`${timelinePath()}/snapshots`);
    if (seq !== loadSeq) return;
    snapshots.value = res.items ?? [];
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
    while (stack.length > SMARTCUT_UNDO_LIMIT || (total > SMARTCUT_UNDO_BYTES && stack.length > 1)) {
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

  function stackBytes(stack: readonly string[]): number {
    return stack.reduce((sum, s) => sum + snapshotBytes(s), 0);
  }

  // ---------------------------------------------------------------- 改动

  /**
   * 提交一次改动：先压一步撤销，再 `timeline = relayout(mutator(timeline))`，置 dirty，600ms 后保存。
   * 拖动中途不调它，只在松手时调一次（一次拖动 = 一步撤销 = 一次保存请求）。
   *
   * - mutator 必须是纯函数（`utils/smartcutContent.ts` 等），原样返回同一个对象表示「什么都没改」：不压空撤销步；
   * - 只读或还没载入时直接返回 false（SC-AC-66：不发保存请求）。
   */
  function commit(mutator: (t: Timeline) => Timeline): boolean {
    if (!loaded.value || !timeline.value || readonly.value) return false;
    const before = timeline.value;
    const next = mutator(before);
    if (next === before) return false;
    pushUndo(JSON.stringify(before));
    redoStack.value = [];
    redoBytes = 0;
    timeline.value = smartcutRelayout(next);
    pruneSelection();
    markChanged();
    return true;
  }

  function undo(): boolean {
    if (!canUndo.value || !timeline.value) return false;
    const next = [...undoStack.value];
    const snapshot = next.pop() as string;
    undoBytes -= snapshotBytes(snapshot);
    undoStack.value = next;
    pushRedo(JSON.stringify(timeline.value));
    timeline.value = smartcutRelayout(JSON.parse(snapshot) as Timeline);
    pruneSelection();
    markChanged();
    return true;
  }

  function redo(): boolean {
    if (!canRedo.value || !timeline.value) return false;
    const next = [...redoStack.value];
    const snapshot = next.pop() as string;
    redoBytes -= snapshotBytes(snapshot);
    redoStack.value = next;
    pushUndo(JSON.stringify(timeline.value));
    timeline.value = smartcutRelayout(JSON.parse(snapshot) as Timeline);
    pruneSelection();
    markChanged();
    return true;
  }

  /**
   * 本地有了新改动。冲突 / 离线时不发请求（冲突时必然再 409，离线时发不出去），只把草稿刷新到最新；
   * 保存中时保持「保存中」，结束后补存。
   */
  function markChanged() {
    revision += 1;
    syncUnsaved();
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
    }, SMARTCUT_SAVE_DEBOUNCE_MS);
  }

  function cancelSave() {
    clearTimeout(saveTimer);
    saveTimer = undefined;
  }

  function persistDraft(): boolean {
    if (!timelineId.value || !timeline.value) return false;
    return writeDraft({
      projectId: projectId.value, timelineId: timelineId.value, baseVersion: serverVersion.value,
      savedAt: new Date().toISOString(), timeline: timeline.value
    });
  }

  /** 选中项里不在时间线里的去掉（撤销、采纳远端内容之后）。 */
  function pruneSelection() {
    const sel = selection.value;
    const t = timeline.value;
    if (!sel || !t) return;
    const ids = sel.ids.filter(id => hasObject(t, sel.kind, id));
    if (ids.length !== sel.ids.length) selection.value = ids.length ? { kind: sel.kind, ids } : null;
  }

  /** 一个对象现在的 ID（临时 ID 保存后换成了正式 ID 的话给正式的）。 */
  function currentId(id: string): string {
    return idAliases.get(id) ?? id;
  }

  function select(next: SmartcutSelection) {
    selection.value = next && next.ids.length ? { kind: next.kind, ids: [...new Set(next.ids)] } : null;
  }

  function setPlayhead(ms: number) {
    const max = Math.max(0, timeline.value?.effectiveMs ?? 0);
    playheadMs.value = Math.max(0, Math.min(Math.round(ms), max));
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
        if (writing === 0) drainPendingFrame();
      }
    })();
    return inFlight;
  }

  /** 发一次 PUT；返回「成功了但保存期间又有改动、需要再存一次」。 */
  async function saveOnce(): Promise<boolean> {
    if (!loaded.value || !timeline.value || readonly.value || resolving.value) return false;
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
    const body = { ...smartcutContentOf(timeline.value), version: serverVersion.value };
    // 保存前校验（与 Go 同一套规则、同一个 path）：坏内容不发请求，否则只会换回一个 40001。
    const bad = smartcutValidateContent(body);
    if (bad) {
      saveState.value = "error";
      lastError.value = { message: bad.reason, path: bad.path };
      persistDraft();
      return false;
    }
    const sentRevision = revision;
    saveState.value = "saving";
    try {
      const res = await api<SaveResult>(`${timelinePath()}/content`, { method: "PUT", body });
      applySaveResult(res, sentRevision);
      lastError.value = null;
      if (revision === sentRevision) {
        saveState.value = "idle";
        removeDraft(timelineId.value);
        return false;
      }
      saveState.value = "dirty";
      return true;
    } catch (cause) {
      handleSaveFailure(cause);
      return false;
    }
  }

  /**
   * 保存成功：按 `idMap` 改写撤销栈、重做栈、选中项；保存期间没有新改动时直接换成服务端算好的时间线，
   * 有新改动时只把本地内容里的临时 ID 改写掉（新改动还没发出去，不能被服务端那份覆盖）。
   */
  function applySaveResult(res: SaveResult, sentRevision: number) {
    const idMap = res.idMap ?? {};
    for (const [from, to] of Object.entries(idMap)) idAliases.set(from, to);
    if (Object.keys(idMap).length) {
      undoStack.value = smartcutRemapSnapshots(undoStack.value, idMap);
      redoStack.value = smartcutRemapSnapshots(redoStack.value, idMap);
      undoBytes = stackBytes(undoStack.value);
      redoBytes = stackBytes(redoStack.value);
      const sel = selection.value;
      if (sel) selection.value = { kind: sel.kind, ids: sel.ids.map(id => smartcutMapId(id, idMap)) };
    }
    serverVersion.value = res.timeline.version;
    savedRevision = sentRevision;
    if (revision === sentRevision) {
      timeline.value = res.timeline;
    } else if (timeline.value) {
      timeline.value = smartcutApplyIdMap(timeline.value, idMap);
    }
    syncUnsaved();
    pruneSelection();
  }

  /**
   * 保存失败之后进哪个态。⚠️ 冲突按**业务码 40902** 判，不按 HTTP 409：40905（项目在回收站）也是 409，
   * 当成冲突的话用户点「以我为准」会永远失败。任何失败都先把本地时间线写进草稿。
   */
  function handleSaveFailure(cause: unknown) {
    const code = errorCode(cause);
    if (code === VERSION_CONFLICT) {
      enterConflict(smartcutConflictDetails(cause));
      return;
    }
    if (statusOf(cause) === 403) {
      // 后端兜底：角色在打开期间被改成只读。进只读并停存。
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

  function enterConflict(details: SmartcutConflict) {
    cancelSave();
    clearOfflineRetry();
    conflict.value = details;
    saveState.value = "conflict";
    persistDraft();
  }

  /**
   * 收起、关闭、导出、AI 剪辑之前调用：取消防抖，立即保存。成功时返回最新的服务端版本；
   * 冲突 / 失败 / 离线时 reject 并附原因 —— 导出与 AI 剪辑都要带「已保存的版本」，否则服务端回 40902。
   */
  async function flush(): Promise<number> {
    cancelSave();
    if (!loaded.value || !timeline.value) throw new SmartcutFlushError("error", "时间线还没有载入");
    if (saveState.value === "conflict") throw new SmartcutFlushError("conflict", "时间线有冲突，请先处理冲突");
    if (saveState.value === "offline") throw new SmartcutFlushError("offline", "网络已断开，恢复之后才能保存");
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
        throw new SmartcutFlushError("conflict", "时间线有冲突，请先处理冲突");
      case "offline":
        throw new SmartcutFlushError("offline", "网络已断开，恢复之后才能保存");
      default:
        throw new SmartcutFlushError(state, lastError.value?.message ?? "时间线没有保存成功");
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
   * 处理冲突（SC-AC-08）。两条路都先确认草稿写进了 sessionStorage，写不进去就**拒绝**（否则就是静默丢弃用户的改动）。
   *
   * - `adopt` 载入最新：GET 后替换，**清空撤销栈与重做栈**；刚写的草稿留给界面询问是否恢复；
   * - `overwrite` 以我为准：先取当前版本（摘要），再以它整份保存本地内容。期间又有人存了 → 回到冲突态，不自动循环重试。
   */
  async function resolveConflict(mode: "adopt" | "overwrite"): Promise<"resolved" | "conflict"> {
    if (saveState.value !== "conflict" || resolving.value || !loaded.value || !timeline.value) return "conflict";
    if (!persistDraft()) {
      throw new Error("浏览器无法保存本地草稿（存储空间不足或被禁用），为避免丢失你的改动，暂不能处理冲突");
    }
    cancelSave();
    resolving.value = true;
    try {
      if (mode === "adopt") {
        const res = await api<Timeline>(timelinePath());
        replaceWithServer(res, true);
        const draft = readDraft(timelineId.value);
        pendingDraft.value = draft && !smartcutSameContent(draft.timeline, res)
          ? { ...draft, serverChanged: draft.baseVersion !== res.version }
          : null;
        return "resolved";
      }
      const body = { ...smartcutContentOf(timeline.value), version: 0 };
      const bad = smartcutValidateContent(body);
      if (bad) throw new Error(`时间线不合法，不能保存：${bad.path}：${bad.reason}`);
      const current = await currentSummary();
      if (!current) throw new Error("时间线不存在或已被删除");
      const sentRevision = revision;
      try {
        const res = await api<SaveResult>(`${timelinePath()}/content`, {
          method: "PUT",
          body: { ...body, version: current.version }
        });
        applySaveResult(res, sentRevision);
        conflict.value = null;
        lastError.value = null;
        if (revision === sentRevision) {
          saveState.value = "idle";
          removeDraft(timelineId.value);
        } else {
          saveState.value = "dirty";
          scheduleSave();
        }
        return "resolved";
      } catch (cause) {
        if (errorCode(cause) === VERSION_CONFLICT) {
          enterConflict(smartcutConflictDetails(cause));
          return "conflict";
        }
        throw cause;
      }
    } finally {
      resolving.value = false;
      if (writing === 0) drainPendingFrame();
    }
  }

  /** 摘要里本时间线的那一条（版本对账用：比取整份时间线轻得多）。 */
  async function currentSummary(): Promise<Summary | undefined> {
    const list = await api<{ items: Summary[] }>(listPath());
    return (list.items ?? []).find(item => item.timelineId === timelineId.value);
  }

  /** 用载入时发现的草稿替换本地内容（撤销栈里留一步，可撤回），然后保存。 */
  async function restoreDraft(): Promise<boolean> {
    const draft = pendingDraft.value;
    if (!draft || saveState.value === "conflict") return false;
    pendingDraft.value = null;
    if (!commit(() => draft.timeline)) return false;
    await save();
    return true;
  }

  function discardDraft() {
    if (timelineId.value) removeDraft(timelineId.value);
    pendingDraft.value = null;
  }

  // ---------------------------------------------------------------- 离线

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
    }, SMARTCUT_OFFLINE_RETRY_MS);
  }

  function clearOfflineRetry() {
    clearTimeout(offlineTimer);
    offlineTimer = undefined;
  }

  function onOffline() {
    if (loaded.value) enterOffline();
  }

  /**
   * 恢复之后先对账，再决定补存还是进冲突：
   * - 服务端版本没变：有改动就补存；
   * - 变了：有改动直接进冲突（**不发那个注定 409 的请求**）；没有改动就载入最新（同远端更新的自动采纳）；
   * - 对账本身失败：回到 error，不盲发。
   */
  async function onOnline(manual = false): Promise<void> {
    clearOfflineRetry();
    if (!loaded.value || saveState.value !== "offline") return;
    if (navigatorOffline() && !manual) return;
    let current: Summary | undefined;
    try {
      current = await currentSummary();
    } catch (cause) {
      if (isNetworkFailure(cause)) {
        // 还是连不上：继续离线，过一会儿再对。
        if (!navigatorOffline()) scheduleOfflineRetry();
        return;
      }
      saveState.value = "error";
      lastError.value = { message: `网络恢复后对账失败：${messageOf(cause, "请求失败")}`, code: errorCode(cause) };
      if (hasUnsaved()) persistDraft();
      return;
    }
    if (!current) {
      saveState.value = "error";
      lastError.value = { message: "时间线不存在或已被删除" };
      if (hasUnsaved()) persistDraft();
      return;
    }
    if (current.version === serverVersion.value) {
      if (hasUnsaved()) {
        saveState.value = "dirty";
        await save();
      } else {
        saveState.value = "idle";
      }
      return;
    }
    if (hasUnsaved()) {
      enterConflict({ currentVersion: current.version, currentUpdatedAt: current.updatedAt ?? "", currentEditorName: "", bySelf: false });
      return;
    }
    saveState.value = "idle";
    await adoptRemote(null);
  }

  /**
   * 收起之后再打开：收起期间不在听事件，先对一次账 —— 服务端版本变了且本地没改动就载入最新，有改动就进冲突。
   * 对账失败不打扰（下一次保存会撞上冲突并说清是谁改的）。
   */
  async function reconcile(): Promise<void> {
    if (!loaded.value || saveState.value === "conflict" || saveState.value === "offline") return;
    let current: Summary | undefined;
    try {
      current = await currentSummary();
    } catch {
      return;
    }
    if (!current || current.version <= serverVersion.value) return;
    if (hasUnsaved()) {
      enterConflict({ currentVersion: current.version, currentUpdatedAt: current.updatedAt ?? "", currentEditorName: "", bySelf: false });
      return;
    }
    await adoptRemote(null);
  }

  // ---------------------------------------------------------------- 实时事件

  /**
   * 处理 `timeline.updated`（只作对账信号，不拿事件当正文，SCD-19）：
   * - 不是本时间线，或版本不比本地新：忽略（自己保存的回声在保存响应之后版本已经追上了）；
   * - 保存中 / 处理冲突中：先缓存这一帧，结束后再判断 —— 否则会把自己保存的回声当成别人的改动；
   * - 没有本地改动：GET → 替换 → 清空撤销栈，并提示「时间线被 X 更新」；
   * - 有本地改动：进冲突。
   */
  function handleRemoteUpdate(frame: Record<string, unknown>): void {
    if (frame?.type !== "timeline.updated" || !loaded.value) return;
    if (String(frame.timelineId ?? "") !== timelineId.value) return;
    const version = Number(frame.version);
    if (!Number.isFinite(version) || version <= serverVersion.value) return;
    if (inFlight || resolving.value || writing > 0) {
      if (!pendingFrame || version > Number(pendingFrame.version)) pendingFrame = frame;
      return;
    }
    void evaluateRemote(frame);
  }

  /** 包住一次服务端写入：期间的远端事件先缓存，结束后再判断（版本已经追上的就是自己的回声）。 */
  async function serverWrite<T>(fn: () => Promise<T>): Promise<T> {
    writing += 1;
    try {
      return await fn();
    } finally {
      writing -= 1;
      if (writing === 0 && !inFlight && !resolving.value) drainPendingFrame();
    }
  }

  function drainPendingFrame() {
    const frame = pendingFrame;
    pendingFrame = null;
    if (frame) void evaluateRemote(frame);
  }

  function frameBySelf(frame: Record<string, unknown>): boolean {
    const editorId = typeof frame.editorId === "string" ? frame.editorId : "";
    const me = String(auth.user?.id ?? "");
    // 事件不带 bySelf：只有「人写的、且是同一个用户」才算本人（AI / Agent 也带着本人的 ID，算本人会说成「你在另一个窗口」）。
    return editorKindOf(frame.editorKind) === "user" && !!editorId && editorId === me;
  }

  async function evaluateRemote(frame: Record<string, unknown>): Promise<void> {
    const version = Number(frame.version);
    if (version <= serverVersion.value || saveState.value === "conflict") return;
    const kind = editorKindOf(frame.editorKind);
    if (hasUnsaved()) {
      enterConflict({
        currentVersion: version,
        currentUpdatedAt: typeof frame.occurredAt === "string" ? frame.occurredAt : "",
        currentEditorName: typeof frame.editorName === "string" ? frame.editorName.trim() : "",
        bySelf: frameBySelf(frame),
        editorKind: kind
      });
      return;
    }
    if (saveState.value === "offline") return;
    await adoptRemote(frame);
  }

  /** 没有本地改动时载入最新（自动采纳），清空撤销栈并提示。期间用户又改了东西就改进冲突，不覆盖。 */
  async function adoptRemote(frame: Record<string, unknown> | null): Promise<void> {
    const startRevision = revision;
    let res: Timeline;
    try {
      res = await api<Timeline>(timelinePath());
    } catch {
      // 取不到最新就保持现状：事件只是信号，下一次保存会撞上冲突并说清是谁改的。
      return;
    }
    if (revision !== startRevision) {
      enterConflict({
        currentVersion: res.version, currentUpdatedAt: "", currentEditorName: "", bySelf: false,
        editorKind: editorKindOf(res.lastEditorKind ?? frame?.editorKind)
      });
      return;
    }
    if (res.version <= serverVersion.value) return;
    replaceWithServer(res, true);
    noticeSeq += 1;
    remoteNotice.value = {
      version: res.version,
      editorKind: editorKindOf(frame?.editorKind ?? res.lastEditorKind),
      editorName: typeof frame?.editorName === "string" ? frame.editorName.trim() : "",
      bySelf: frame ? frameBySelf(frame) : false,
      reason: typeof frame?.reason === "string" ? frame.reason : "",
      seq: noticeSeq
    };
  }

  /**
   * `smartcut.updated`：AI 剪辑的阶段、素材分析的进度（SCD-19）。只是信号：
   * 本次 AI 调用的阶段写进 `plan.stage`；其余（同步结果、素材分析完成）刷新素材条。
   */
  function handleSmartcutFrame(frame: Record<string, unknown>): void {
    if (frame?.type !== "smartcut.updated" || !loaded.value) return;
    if (String(frame.timelineId ?? "") !== timelineId.value) return;
    const callId = typeof frame.callId === "string" ? frame.callId : "";
    const stage = typeof frame.stage === "string" ? frame.stage : "";
    // 请求还在等响应：第一帧带着 callId 来，认下它，之后只认这一个（同一时间线上别人的调用不串进来）。
    const running = planRunning.value;
    if (callId && stage && running && (!running.callId || running.callId === callId)) {
      planRunning.value = { ...running, callId, stage };
      return;
    }
    if (callId && plan.value && plan.value.callId === callId && stage) {
      plan.value = { ...plan.value, stage };
      return;
    }
    if (!callId) void refreshInputs().catch(() => undefined);
  }

  // ---------------------------------------------------------------- 服务端写入（AI 剪辑、方案切换、快照恢复、片段回填）

  /**
   * 采纳一份**已经在服务端写好**的时间线（AI 剪辑、方案切换、快照恢复、片段回填的响应）。
   * `asUndoStep` 时把写入前的本地内容压一步撤销：⌘Z 就回到 AI 之前（撤销后的内容照常整份保存）。
   *
   * ⚠️ 本地有还没存上的改动时**不覆盖**，改进冲突：那些改动是在请求发出之后做的，静默换掉就是丢了用户的工作。
   * 返回是否已采纳。
   */
  function applyServerTimeline(res: Timeline, options: { asUndoStep?: boolean } = {}): boolean {
    if (!loaded.value || !timeline.value || res.version <= serverVersion.value) return false;
    if (hasUnsaved()) {
      enterConflict({
        currentVersion: res.version, currentUpdatedAt: "", currentEditorName: "", bySelf: false,
        editorKind: editorKindOf(res.lastEditorKind)
      });
      return false;
    }
    if (options.asUndoStep) {
      pushUndo(JSON.stringify(timeline.value));
      redoStack.value = [];
      redoBytes = 0;
    }
    revision += 1;
    replaceWithServer(res, false);
    return true;
  }

  /** 切换方案时选的那一套（冲突后「仍然应用」要写回同一套）。 */
  let pendingVariant = 0;

  /**
   * 一次 AI 调用（剪一版、追加指令、切换方案、仍然应用）：期间 `planRunning` 有值（阶段条、禁用发送），
   * 远端事件先缓存（同 serverWrite）。40902 且服务端带回了 `callId` 时记进 `planConflict`（「仍然应用」用它），
   * 错误照样抛给调用方，文案由 `smartcutPlanErrorText` 按业务码给。
   */
  async function runAi(kind: SmartcutPlanKind, request: () => Promise<PlanResult>, meta: { kind?: "plan" | "edit"; mode?: string } = {}): Promise<PlanResult> {
    const seq = loadSeq;
    planConflict.value = null;
    planRunning.value = { kind, stage: "sync", callId: "", startedAt: Date.now() };
    try {
      return await serverWrite(async () => {
        try {
          const res = await request();
          if (seq === loadSeq) adoptPlanResult(res, meta);
          return res;
        } catch (cause) {
          if (seq === loadSeq && errorCode(cause) === VERSION_CONFLICT) {
            const callId = (cause as { data?: { details?: { callId?: unknown } } })?.data?.details?.callId;
            if (typeof callId === "string" && callId) {
              planConflict.value = { kind, callId, variant: kind === "variant" ? pendingVariant : 0, details: smartcutConflictDetails(cause) };
            }
          }
          throw cause;
        }
      });
    } finally {
      if (seq === loadSeq) planRunning.value = null;
    }
  }

  function assertAiAllowed(what: string) {
    if (readonly.value) throw new SmartcutFlushError("error", `你在该工作区是只读，不能${what}`);
    if (planRunning.value) throw new SmartcutFlushError("error", "AI 剪辑进行中，请等它完成");
  }

  /**
   * AI 剪辑（SC-F-80）：先 flush（带的 `baseVersion` 要是刚存上的版本），再 `POST /plan`；结果直接写进了时间线，
   * 本地记一步撤销并记下快照（「撤销这轮 AI」）。失败原样抛出，文案由 `smartcutPlanErrorText` 按业务码给。
   */
  async function runPlan(req: Omit<PlanRequest, "baseVersion">): Promise<PlanResult> {
    assertAiAllowed("AI 剪辑");
    const baseVersion = await flush();
    lastPlanRequest.value = JSON.parse(JSON.stringify(req)) as Omit<PlanRequest, "baseVersion">;
    return runAi("plan", () => api<PlanResult>(`${smartcutPath()}/plan`, { method: "POST", body: { ...req, baseVersion } }),
      { kind: "plan", mode: req.mode });
  }

  /** 追加指令（SC-F-110）：在已剪好的时间线上接着提要求。结果卡说明改了哪几处、哪几段是你改过的没有动。 */
  async function runEdit(instruction: string): Promise<PlanResult> {
    assertAiAllowed("追加修改");
    const text = instruction.trim();
    if (!text) throw new SmartcutFlushError("error", "先写下要怎么改");
    const baseVersion = await flush();
    return runAi("edit", () => api<PlanResult>(`${smartcutPath()}/edit`, { method: "POST", body: { instruction: text.slice(0, 2000), baseVersion } }),
      { kind: "edit", mode: "" });
  }

  /** 多方案时切换到另一套（SC-F-112）：同样是服务端写好、本地记一步撤销。 */
  async function applyVariant(variant: number, force = false): Promise<PlanResult> {
    assertAiAllowed("切换方案");
    const current = plan.value;
    if (!current) throw new Error("没有可切换的方案");
    const baseVersion = await flush();
    pendingVariant = variant;
    return runAi("variant", () => api<PlanResult>(`${smartcutPath()}/plan/${current.callId}/apply`, {
      method: "POST", body: { variant, baseVersion, ...(force ? { force: true } : {}) }
    }));
  }

  /**
   * 「仍然应用」（SC-F-86）：AI 剪辑期间时间线被改过时，把已经生成的方案强制写进去（覆盖那些改动，写入前照样存快照，可撤销）。
   * 版本取冲突详情里的当前版本与本地已采纳的版本中较新的那个（期间别人的改动多半已经被自动采纳了）。
   */
  async function forceApply(): Promise<PlanResult> {
    const c = planConflict.value;
    if (!c) throw new Error("没有待应用的方案");
    assertAiAllowed("应用方案");
    const baseVersion = Math.max(await flush(), c.details.currentVersion ?? 0);
    pendingVariant = c.variant;
    const meta = c.kind === "edit" ? { kind: "edit" as const, mode: "" } : c.kind === "plan" ? { kind: "plan" as const, mode: lastPlanRequest.value?.mode ?? "" } : {};
    return runAi(c.kind, () => api<PlanResult>(`${smartcutPath()}/plan/${c.callId}/apply`, {
      method: "POST", body: { variant: c.variant, baseVersion, force: true }
    }), meta);
  }

  /** 关掉「仍然应用」的提示（用户选择不应用）。 */
  function dismissPlanConflict() {
    planConflict.value = null;
  }

  function adoptPlanResult(res: PlanResult, meta: { kind?: "plan" | "edit"; mode?: string } = {}) {
    if (res.timeline) applyServerTimeline(res.timeline, { asUndoStep: true });
    const previous = plan.value;
    plan.value = {
      callId: res.callId,
      stage: "done",
      kind: meta.kind ?? previous?.kind ?? "plan",
      mode: meta.mode ?? previous?.mode ?? "",
      variants: res.variants ?? [],
      appliedVariant: res.appliedVariant ?? null,
      snapshotId: res.snapshotId ?? "",
      appliedVersion: serverVersion.value,
      changes: res.changes ?? []
    };
    void refreshSnapshots().catch(() => undefined);
  }

  /**
   * 「只替换变了的那几段」（SCT-30）：同一来源节点的新产物原位换进片段，保留裁切、变速与转场。服务端写好之后
   * 取回时间线记一步撤销（与 AI 写入同一种处理）；随后到的 `timeline.updated` 是自己的回声，版本已经追上，忽略。
   */
  async function replaceChanged(): Promise<SyncResult> {
    if (readonly.value) throw new SmartcutFlushError("error", "你在该工作区是只读，不能替换素材");
    if (!loaded.value) throw new SmartcutFlushError("error", "时间线还没有载入");
    const baseVersion = await flush();
    const seq = loadSeq;
    return serverWrite(async () => {
      const res = await api<SyncResult>(`${smartcutPath()}/sync`, { method: "POST", body: { mode: "replaceChanged", baseVersion } });
      if (seq !== loadSeq) return res;
      inputs.value = res.inputs ?? [];
      inputsOmitted.value = res.omitted ?? 0;
      inputsFingerprint.value = res.fingerprint ?? "";
      if (res.timelineVersion > serverVersion.value) {
        const t = await api<Timeline>(timelinePath());
        if (seq === loadSeq) applyServerTimeline(t, { asUndoStep: true });
      }
      void refreshSnapshots().catch(() => undefined);
      return res;
    });
  }

  /**
   * 「撤销这轮 AI」要恢复的快照：本次会话的 AI 结果优先；刷新之后从快照列表里找最近一次 AI 快照（SC-AC-33）。
   * 返回快照与「之后有没有手动改过」（有的话界面先确认，恢复会把那些改动一起撤掉）。
   */
  const aiRound = computed<{ snapshotId: string; editedAfter: boolean } | null>(() => {
    const p = plan.value;
    if (p?.snapshotId) {
      return { snapshotId: p.snapshotId, editedAfter: serverVersion.value > p.appliedVersion || unsaved.value };
    }
    // 从最新的往回找：先碰到「恢复之前」说明最近一次动作是恢复（这轮 AI 已经撤销，或者被别的恢复盖过），没有可撤销的；
    // 原位替换、片段回填这类快照跳过（撤销会把它们一起撤掉，editedAfter 会让界面先确认）。
    let latest: SnapshotMeta | undefined;
    for (const snap of snapshots.value) {
      if (snap.reason === "restore" || snap.id === restoredSnapshotId.value) return null;
      if (AI_SNAPSHOT_REASONS.has(snap.reason)) {
        latest = snap;
        break;
      }
    }
    if (!latest) return null;
    // 快照存的是写入前的版本，AI 写入之后是它 + 1；再往上涨就是之后的手动改动。
    return { snapshotId: latest.id, editedAfter: serverVersion.value > latest.version + 1 || unsaved.value };
  });

  /**
   * 撤销这轮 AI（SC-F-145）：调快照恢复。恢复本身在服务端也存了一份快照（再撤销一次就回到恢复前），
   * 本地同样记一步撤销。`missing` 是快照引用、但素材已被删除的片段 —— 恢复照常生效，交给界面提示「第 N 段缺素材」。
   */
  async function undoAiRound(): Promise<RestoreResult["missing"]> {
    if (readonly.value) throw new SmartcutFlushError("error", "你在该工作区是只读，不能撤销 AI 剪辑");
    const round = aiRound.value;
    if (!round) throw new Error("没有可撤销的 AI 剪辑");
    return restoreSnapshot(round.snapshotId);
  }

  async function restoreSnapshot(snapshotId: string): Promise<RestoreResult["missing"]> {
    if (readonly.value) throw new SmartcutFlushError("error", "你在该工作区是只读，不能恢复历史版本");
    const baseVersion = await flush();
    const seq = loadSeq;
    return serverWrite(async () => {
      const res = await api<RestoreResult>(`${timelinePath()}/snapshots/${snapshotId}/restore`, {
        method: "POST", body: { baseVersion }
      });
      if (seq !== loadSeq) return res.missing ?? [];
      applyServerTimeline(res.timeline, { asUndoStep: true });
      plan.value = null;
      restoredSnapshotId.value = snapshotId;
      void refreshSnapshots().catch(() => undefined);
      return res.missing ?? [];
    });
  }

  // ---------------------------------------------------------------- 识别字幕与翻译（SCT-36）

  function transcribeBody(opts: SmartcutTranscribeOptions) {
    return {
      ...(opts.clipIds?.length ? { clipIds: opts.clipIds.map(id => currentId(id)) } : {}),
      modelCode: opts.modelCode, keepFillers: opts.keepFillers, subtitles: opts.subtitles,
      ...(opts.language ? { language: opts.language } : {})
    };
  }

  /** 识别报价（dryRun）：不收费、不写库。选中的片段先换成正式 ID（刚剪出来的那一段可能还是临时 ID）。 */
  async function quoteTranscribe(opts: SmartcutTranscribeOptions): Promise<Quote> {
    if (opts.clipIds?.length) await flush();
    return api<Quote>(`${smartcutPath()}/transcribe`, { method: "POST", body: { ...transcribeBody(opts), dryRun: true } });
  }

  /**
   * 受理识别：按模型收费，带 `Idempotency-Key`（同一次确认的重试复用同一个键，调用方给）。
   * 选了「生成字幕」时，已识别过的素材当场写字幕、其余识别完成后写 —— 都是服务端写进时间线再推 `timeline.updated`，
   * 所以先 flush（本地没存上的改动会让那次写入撞冲突），请求期间到的事件照「服务端写入」缓存、结束后再对账。
   */
  async function transcribe(opts: SmartcutTranscribeOptions, idempotencyKey: string): Promise<TranscribeResult> {
    if (readonly.value) throw new SmartcutFlushError("error", "你在该工作区是只读，不能识别语音");
    await flush();
    return serverWrite(async () => {
      const res = await api<TranscribeResult>(`${smartcutPath()}/transcribe`, {
        method: "POST", headers: { "Idempotency-Key": idempotencyKey }, body: transcribeBody(opts)
      });
      for (const item of res.items ?? []) setGeneration(item);
      return res;
    });
  }

  /**
   * 字幕翻译（SC-F-125）：先 flush（带的 `baseVersion` 要是刚存上的版本）；写好的时间线直接采纳并记一步撤销。
   * 一条都没译出来时服务端不改时间线，这里也什么都不动。
   */
  async function translate(opts: { subtitleTrackId: string; targetLang: string; mode: "newTrack" | "replace" }): Promise<TranslateResult> {
    assertAiAllowed("翻译字幕");
    const baseVersion = await flush();
    const seq = loadSeq;
    return serverWrite(async () => {
      const res = await api<TranslateResult>(`${smartcutPath()}/translate`, {
        method: "POST", body: { ...opts, subtitleTrackId: currentId(opts.subtitleTrackId), baseVersion }
      });
      if (seq === loadSeq && res.timeline) {
        applyServerTimeline(res.timeline, { asUndoStep: true });
        void refreshSnapshots().catch(() => undefined);
      }
      return res;
    });
  }

  /** 片段级生成与识别的进度（SCT-33 / SCT-40 写入）。 */
  function setGeneration(item: SmartcutStoreGeneration) {
    generations.value = { ...generations.value, [item.generationId]: item };
  }

  // ---------------------------------------------------------------- 片段级生成（SCT-40）

  function clipToolPath(req: SmartcutClipRequest): string {
    const clip = () => `${smartcutPath()}/clips/${currentId(req.clipId ?? "")}`;
    switch (req.tool) {
      case "regenerate":
        return `${clip()}/regenerate`;
      case "desubtitle":
        return `${clip()}/desubtitle`;
      case "sfx":
        return `${clip()}/sfx`;
      case "fill":
        return `${smartcutPath()}/fill`;
      case "music":
        return `${smartcutPath()}/music`;
    }
  }

  function clipToolBody(req: SmartcutClipRequest): Record<string, unknown> {
    const body: Record<string, unknown> = { ...req.body };
    if (req.afterClipId) body.afterClipId = currentId(req.afterClipId);
    if (req.refFromClipId) body.refFromClipId = currentId(req.refFromClipId);
    return body;
  }

  /**
   * 片段级生成的报价（dryRun，不收费、不写库）。先 flush：请求里的片段要用正式 ID，刚剪出来的那一段可能还是临时 ID；
   * 而且服务端按库里的时间线算区间与位置，本地没存上的裁切它看不见。
   */
  async function quoteClipTool(req: SmartcutClipRequest): Promise<Quote> {
    await flush();
    return api<Quote>(clipToolPath(req), { method: "POST", body: { ...clipToolBody(req), dryRun: true } });
  }

  /**
   * 受理一次片段级生成：按模型收费，带 `Idempotency-Key`（同一次确认的网络重发复用同一个键，调用方给）。
   * 重拍与去字幕受理之后这一段带「生成中」：服务端只写了标记、不推进版本，本地也标上（不进撤销栈、不触发保存）；
   * 完成后服务端回填并推 `timeline.updated`，这里不自己替换片段（SCR-13）。
   */
  async function runClipTool(req: SmartcutClipRequest, idempotencyKey: string): Promise<SmartcutStoreGeneration> {
    if (readonly.value) throw new SmartcutFlushError("error", "你在该工作区是只读，不能生成");
    await flush();
    const seq = loadSeq;
    const res = await api<SmartcutStoreGeneration>(clipToolPath(req), {
      method: "POST", headers: { "Idempotency-Key": idempotencyKey }, body: clipToolBody(req)
    });
    if (seq !== loadSeq) return res;
    setGeneration(res);
    clipRequests.value = { ...clipRequests.value, [res.generationId]: { ...req, ...(req.clipId ? { clipId: currentId(req.clipId) } : {}) } };
    if ((req.tool === "regenerate" || req.tool === "desubtitle") && res.clipId && (res.status === "queued" || res.status === "running")) {
      setLocalPending(res.clipId, res.generationId);
    }
    return res;
  }

  /** 本地标上 / 清掉「生成中」：不进撤销栈、不保存（这个字段不在整份保存的请求体里）。 */
  function setLocalPending(clipId: string, generationId: string | null) {
    const t = timeline.value;
    if (!t) return;
    let changed = false;
    const tracks = t.tracks.map((track) => {
      if (!track.clips?.some(c => c.id === clipId)) return track;
      return {
        ...track,
        clips: track.clips.map((c) => {
          if (c.id !== clipId) return c;
          if (generationId === null && !c.pendingGenerationId) return c;
          changed = true;
          const next = { ...c };
          if (generationId) next.pendingGenerationId = generationId;
          else delete next.pendingGenerationId;
          return next;
        })
      };
    });
    if (changed) timeline.value = { ...t, tracks };
  }

  /**
   * `generation.update`：按 generationId 认领（只认已经在记录里的那几条，不走画布的生成 store —— 它按节点认领，
   * 智能剪辑名下的任务没有节点）。失败 / 取消时清掉那一段的「生成中」；成功时等服务端回填的 `timeline.updated`。
   */
  function handleGenerationFrame(frame: Record<string, unknown>): void {
    if (!loaded.value) return;
    const next = smartcutClaimGenerationFrame(generations.value, frame);
    if (!next) return;
    setGeneration(next);
    if ((next.status === "failed" || next.status === "cancelled") && next.clipId) {
      const clip = timeline.value?.tracks.flatMap(tr => tr.clips ?? []).find(c => c.id === next.clipId);
      if (clip?.pendingGenerationId === next.generationId) setLocalPending(next.clipId, null);
    }
  }

  /**
   * 生成记录（`GET .../generations`，按时间倒序）：`more` 时接着上一页取。服务端的状态覆盖本地的（它更新或一样新）。
   * 打开剪辑台、实时通道重连之后都取一次第一页：断线期间错过的推送不会补发。
   */
  async function refreshGenerations(more = false): Promise<void> {
    if (!loaded.value) return;
    if (more && !generationsCursor.value) return;
    const seq = loadSeq;
    const query = new URLSearchParams({ limit: "30" });
    if (more && generationsCursor.value) query.set("cursor", generationsCursor.value);
    const res = await api<{ items: SmartcutStoreGeneration[]; nextCursor?: string }>(`${smartcutPath()}/generations?${query}`);
    if (seq !== loadSeq) return;
    const merged = { ...generations.value };
    for (const item of res.items ?? []) merged[item.generationId] = item;
    generations.value = merged;
    if (more || generationsCursor.value === null || !res.nextCursor) generationsCursor.value = res.nextCursor ?? "";
  }

  /**
   * 把生成记录里的一份产物放进时间线（SC-F-136：位置已经不在时，用户手动拖进来）：视频插在 `atMs` 最近的接缝，
   * 音频进对应用途的音频轨、起点 `atMs`。时长取媒体信息（列表里没有）。一次编辑、一步撤销。返回失败原因（空串 = 成功）。
   */
  async function placeGeneration(generationId: string, atMs: number): Promise<string> {
    const g = generations.value[generationId];
    const out = g ? smartcutGenerationOutputKind(g.kind) : null;
    if (!g || !out || !g.outputMediaId) return "这条记录没有可以放进时间线的产物";
    if (readonly.value) return "你在该工作区是只读，不能编辑";
    let info: { kind?: string; durationMs?: number };
    try {
      info = await api<{ kind?: string; durationMs?: number }>(`/media/${g.outputMediaId}/info`);
    } catch {
      return "产物的信息取不到，稍后再试";
    }
    const kind = info.kind === "image" ? "image" : info.kind === "audio" ? "audio" : "video";
    if ((out.kind === "audio") !== (kind === "audio")) return "产物的类型与记录对不上，稍后再试";
    let failure = "";
    commit((t) => {
      const r = smartcutPlaceGenerated(t, { mediaId: g.outputMediaId!, kind, durationMs: info.durationMs ?? 0, role: out.role },
        atMs, { clip: smartcutTempId(), track: smartcutTempId() });
      if (!r.ok) {
        failure = r.reason;
        return t;
      }
      return r.value;
    });
    return failure;
  }

  // ---------------------------------------------------------------- 排版缓存（SCD-46）

  function typesetEntry(key: string): SmartcutTypesetEntry | undefined {
    return typesetCache.value[key];
  }

  /** 存一次排版结果；同一个键的旧图释放掉。超过 400 条时丢最早的（一条几 KB 到几十 KB）。 */
  function setTypeset(key: string, entry: SmartcutTypesetEntry) {
    const prev = typesetCache.value[key];
    if (prev?.url === entry.url) return;
    if (prev) revokeUrl(prev.url);
    const merged = Object.entries({ ...typesetCache.value, [key]: entry });
    const dropped = merged.slice(0, Math.max(0, merged.length - 400));
    for (const [, old] of dropped) revokeUrl(old.url);
    typesetCache.value = Object.fromEntries(merged.slice(dropped.length));
  }

  function releaseTypeset() {
    for (const entry of Object.values(typesetCache.value)) revokeUrl(entry.url);
    typesetCache.value = {};
  }

  function revokeUrl(url: string) {
    const URLImpl = (globalThis as { URL?: { revokeObjectURL?: (u: string) => void } }).URL;
    try {
      URLImpl?.revokeObjectURL?.(url);
    } catch {
      // 释放失败只是多占一点内存。
    }
  }

  return {
    projectId, timelineId, timeline, serverVersion, saveState, conflict, lastError, pendingDraft, remoteNotice,
    loaded, resolving, undoStack, redoStack, inputs, inputsFingerprint, inputsOmitted, config, plan, planRunning, planConflict,
    lastPlanRequest, snapshots, generations, clipRequests, generationsCursor, typesetCache, playheadMs, playing, selection, unsaved,
    readonly, readonlyReason, canUndo, canRedo, aiRound,
    load, reset, commit, undo, redo, save, flush, retry, resolveConflict, restoreDraft, discardDraft,
    handleRemoteUpdate, handleSmartcutFrame, applyServerTimeline, runPlan, runEdit, applyVariant, forceApply, dismissPlanConflict,
    undoAiRound, restoreSnapshot, replaceChanged, quoteTranscribe, transcribe, translate,
    refreshInputs, refreshSnapshots, sync, reconcile, currentId, select, setPlayhead, setGeneration, typesetEntry, setTypeset,
    quoteClipTool, runClipTool, handleGenerationFrame, refreshGenerations, placeGeneration
  };
});
