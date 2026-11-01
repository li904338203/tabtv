import type { components } from "@tabtv/api-client";
import { tokenSubject } from "@tabtv/api-client";
import { watch as watchSource } from "vue";
import {
  isAwaitingUser,
  isWorking,
  type AgentCard,
  type AgentMessage,
  type AgentSession,
  type AgentSessionState,
  type AgentStep,
  type AgentTurn
} from "~/utils/agentState";

/**
 * 画布 Agent 的会话状态（AGT-27，开发 03 §5）。面板（AGT-28）与输入框（AGT-29）只读这里。
 *
 * ⚠️ **HTTP 是事实来源，事件只是「该去对账了」的信号**（AG-16、GO-26）：
 *  - `agent.state / step / card` → `reconcile()`（节流 300ms，**尾随执行**：最后一个事件之后一定会对一次账）；
 *  - `agent.delta` → 只追加到 `streaming`（纯展示缓存），**不发请求**；正式消息到了就删掉对应条目；
 *  - 重连之后、页面重新可见时 → `reconcile()`（断线期间的推送不会补发）。
 *  任何界面状态都必须能**只靠 HTTP 重建**：刷新页面后与事件驱动的结果一致。
 * ⚠️ 对账失败**不清空本地状态**：一次网络抖动不该让眼前的消息消失。
 * ⚠️ `streaming` 只是展示缓存，绝不能当作消息持久化的来源。
 */
export type AgentSessionDetail = components["schemas"]["AgentSessionDetail"];
export type AgentMessagePage = components["schemas"]["AgentMessagePage"];
export type AgentWriteResult = components["schemas"]["AgentWriteResult"];
export type AgentSubmitResult = components["schemas"]["AgentSubmitResult"];
export type AgentStopResult = components["schemas"]["AgentStopResult"];
export type AgentCancelResult = components["schemas"]["AgentCancelResult"];
export type AgentTurnLedger = components["schemas"]["AgentTurnLedger"];
export type AgentRefInput = components["schemas"]["AgentRefInput"];
export type AgentTask = components["schemas"]["AgentTask"];
export type AgentCreateSessionRequest = components["schemas"]["AgentCreateSessionRequest"];
export type AgentPrefs = Record<string, unknown>;

/** 发给服务端的引用：**只有 kind 与 id**（AG-03、GO-27：不带 URL / objectKey）。 */
export type AgentRef = { kind: AgentRefInput["kind"]; id: string };

/** 当前会话的视图（开发 03 §5 的 `current`）。 */
export type AgentCurrent = {
  id: string;
  session: AgentSession;
  state: AgentSessionState;
  todo: unknown[];
  approvedBudget: number;
  /** 额度已占用（预留 + 已扣的报价合计）。 */
  spent: number;
  /** 失败已退款而归还额度的报价合计（单列，不从 spent 里减）。 */
  refunded: number;
  /** 会话在等的那张卡片（待处理卡片里最新的一张）。 */
  pendingCardId: string | null;
  pendingCards: AgentCard[];
  latestTurn: AgentTurn | null;
  isInitiator: boolean;
  stopRequested: boolean;
};

export type SendOptions = {
  skillCode?: string;
  /** 本轮起生效的会话级偏好（服务端写进会话）；不传不改。 */
  prefs?: AgentPrefs;
  /** 新会话的标题（缺省由服务端取首条消息前 20 字）。 */
  title?: string;
  /** 显式指定幂等键（一般不传：同一次发送意图的重试自动复用同一个键）。 */
  idempotencyKey?: string;
};

export type PanelPrefs = { open: boolean; width: number; floating: boolean; floatRight: number; floatTop: number };

export const PANEL_MIN_WIDTH = 320;
export const PANEL_MAX_WIDTH = 640;
export const PANEL_DEFAULT_WIDTH = 400;
/**
 * 悬浮卡片的默认位置（2026-11-01 按用户给的参考图）：离窗口右边 16、离顶部 72 —— 在画布顶栏（高 56）下面，
 * 不压住顶栏右侧的按钮。卡片高度在样式里另给，底下给右下角的「Agent 新增了 N 个节点」提示留出位置。
 */
export const PANEL_FLOAT_DEFAULT = { right: 16, top: 72 } as const;
/** 悬浮卡片离窗口四边至少留多少（拖动时夹住，窗口缩小时样式里也按它夹）。 */
export const PANEL_FLOAT_MARGIN = 8;
/** 事件 → 对账的节流窗口。 */
export const RECONCILE_THROTTLE_MS = 300;
/** 对账时最多往前补几页（防止一个异常游标让对账变成无限翻页）。 */
const MAX_BACKFILL_PAGES = 5;
const PANEL_KEY_PREFIX = "seeaitv.agent.panel";
const AGENT_EVENTS = ["agent.state", "agent.step", "agent.card"] as const;

export function clampPanelWidth(width: unknown): number {
  const value = Number(width);
  if (!Number.isFinite(value)) return PANEL_DEFAULT_WIDTH;
  return Math.min(PANEL_MAX_WIDTH, Math.max(PANEL_MIN_WIDTH, Math.round(value)));
}

/**
 * 本地存储里的面板偏好（按用户）：开合、宽度、是否悬浮、悬浮位置；读不到 / 损坏时给默认值。
 * 悬浮是 2026-11-01 加的：此前存下的偏好没有这几项，按停靠、默认位置处理。
 */
export function decodePanelPrefs(raw: string | null | undefined): PanelPrefs {
  const fallback: PanelPrefs = {
    open: false, width: PANEL_DEFAULT_WIDTH, floating: false, floatRight: PANEL_FLOAT_DEFAULT.right, floatTop: PANEL_FLOAT_DEFAULT.top
  };
  if (!raw) return fallback;
  try {
    const value = JSON.parse(raw) as { open?: unknown; width?: unknown; floating?: unknown; floatRight?: unknown; floatTop?: unknown };
    const offset = (input: unknown, fallbackValue: number) => {
      const number = Number(input);
      return input !== null && input !== undefined && input !== "" && Number.isFinite(number) ? Math.max(PANEL_FLOAT_MARGIN, Math.round(number)) : fallbackValue;
    };
    return {
      open: value.open === true,
      width: clampPanelWidth(value.width ?? PANEL_DEFAULT_WIDTH),
      floating: value.floating === true,
      floatRight: offset(value.floatRight, PANEL_FLOAT_DEFAULT.right),
      floatTop: offset(value.floatTop, PANEL_FLOAT_DEFAULT.top)
    };
  } catch {
    return fallback;
  }
}

/**
 * 拖动悬浮卡片时的落点：`right` 是卡片右缘离窗口右边的距离，`top` 是离窗口顶部的距离；
 * 夹到卡片整个留在窗口里、四边至少留 PANEL_FLOAT_MARGIN。窗口比卡片还小时贴右上角（头部右侧的按钮总还点得到）。
 */
export function clampFloatPosition(
  position: { right: number; top: number },
  card: { width: number; height: number },
  viewport: { width: number; height: number }
): { right: number; top: number } {
  const maxRight = Math.max(PANEL_FLOAT_MARGIN, viewport.width - card.width - PANEL_FLOAT_MARGIN);
  const maxTop = Math.max(PANEL_FLOAT_MARGIN, viewport.height - card.height - PANEL_FLOAT_MARGIN);
  return {
    right: Math.round(Math.min(maxRight, Math.max(PANEL_FLOAT_MARGIN, position.right))),
    top: Math.round(Math.min(maxTop, Math.max(PANEL_FLOAT_MARGIN, position.top)))
  };
}

/** 按 seq 合并两批消息：同 id 以新的为准，按 seq 升序，seq 重复只留一条（重复到达的事件 / 对账）。 */
export function mergeMessages(current: AgentMessage[], incoming: AgentMessage[]): AgentMessage[] {
  const bySeq = new Map<number, AgentMessage>();
  for (const message of current) bySeq.set(message.seq, message);
  for (const message of incoming) bySeq.set(message.seq, message);
  return [...bySeq.values()].sort((a, b) => a.seq - b.seq);
}

function byId<T extends { id: string }>(target: Record<string, T>, items: T[] | undefined): Record<string, T> {
  if (!items?.length) return target;
  const next = { ...target };
  for (const item of items) next[item.id] = item;
  return next;
}

function minSeq(messages: AgentMessage[]): number | undefined {
  return messages.length ? messages[0]!.seq : undefined;
}

function maxSeq(messages: AgentMessage[]): number | undefined {
  return messages.length ? messages[messages.length - 1]!.seq : undefined;
}

function frameString(frame: Record<string, unknown>, key: string): string {
  const value = frame[key];
  return typeof value === "string" || typeof value === "number" ? String(value) : "";
}

/** 一次发送意图的指纹：内容一样就是「同一次点击的重试」，复用幂等键。 */
function sendFingerprint(sessionId: string, text: string, refs: AgentRef[], opts: SendOptions): string {
  return JSON.stringify([sessionId, text, refs.map(ref => `${ref.kind}:${ref.id}`), opts.skillCode ?? "", opts.prefs ?? null, opts.title ?? ""]);
}

function newIdempotencyKey(): string {
  return globalThis.crypto?.randomUUID?.() ?? `${Date.now().toString(36)}-${Math.random().toString(36).slice(2)}`;
}

export const useAgentStore = defineStore("agent", () => {
  const realtime = useRealtimeStore();
  const auth = useAuthStore();

  const projectId = ref("");
  /** 各项目的会话列表（最近活动在前，服务端排序）。 */
  const sessionsByProject = ref<Record<string, AgentSession[]>>({});
  /** 当前会话 ID；空串 = 新会话草稿（下一次发送会建会话）。 */
  const currentId = ref("");
  const detail = ref<AgentSessionDetail | null>(null);
  /** 当前会话的消息，按 seq 升序（含全部角色；展示前用 `isVisibleMessage` 过滤）。 */
  const messages = ref<AgentMessage[]>([]);
  const steps = ref<Record<string, AgentStep>>({});
  const cards = ref<Record<string, AgentCard>>({});
  const turns = ref<Record<string, AgentTurn>>({});
  /** 更早一页的游标；null = 没有更早的了。 */
  const olderCursor = ref<string | null>(null);
  /** `agent.delta` 累积：messageId → 已到达的文本（纯展示）。 */
  const streaming = ref<Record<string, string>>({});
  /** streaming 条目属于哪一轮（轮次结束而正式消息没落库时据此清掉）。 */
  const streamingTurn = new Map<string, string>();

  const loadingSessions = ref(false);
  const loadingDetail = ref(false);
  const loadingOlder = ref(false);
  const sending = ref(false);
  /** 最近一次对账失败的原话（成功即清空）；**失败不清空本地状态**。 */
  const syncError = ref("");

  /** 最近一次停止时服务端列出的未终态生成任务（「已提交的生成任务列出，各自可取消」，设计 03 §8）。 */
  const stoppedTasks = ref<AgentTask[]>([]);

  const panelOpen = ref(false);
  const panelWidth = ref(PANEL_DEFAULT_WIDTH);
  /** 悬浮：面板浮在画布上的一张卡片，不占画布宽度（2026-11-01）；否则停靠在画布右侧一栏。 */
  const panelFloating = ref(false);
  /** 悬浮卡片右缘离窗口右边、顶部离窗口顶部的距离（拖动时只改内存，松手再存本地）。 */
  const floatRight = ref<number>(PANEL_FLOAT_DEFAULT.right);
  const floatTop = ref<number>(PANEL_FLOAT_DEFAULT.top);

  const sessions = computed<AgentSession[]>(() => sessionsByProject.value[projectId.value] ?? []);

  const current = computed<AgentCurrent | null>(() => {
    const value = detail.value;
    if (!value || value.session.id !== currentId.value) return null;
    const pendingCards = value.pendingCards ?? [];
    const pending = pendingCards.length ? pendingCards[pendingCards.length - 1]!.id : null;
    return {
      id: value.session.id,
      session: value.session,
      state: value.session.state,
      todo: value.session.todo ?? [],
      approvedBudget: value.budget?.approved ?? value.session.approvedBudget ?? 0,
      spent: value.budget?.spent ?? 0,
      refunded: value.budget?.refunded ?? 0,
      pendingCardId: pending,
      pendingCards,
      latestTurn: value.latestTurn ?? null,
      isInitiator: value.session.isInitiator,
      stopRequested: value.session.stopRequested
    };
  });

  /**
   * 本项目的实时通道（状态条「连接已断开，正在重连」用）。⚠️ 只看 `project:{id}` 这个 topic 真的订上了没有：
   * 被拒的 topic 服务端照样回 subscribe.ok（CD-73），别的 topic 订上了也不代表 Agent 事件收得到。
   */
  const connection = computed<"online" | "connecting" | "offline">(() => {
    if (!projectId.value) return "offline";
    if (realtime.isSubscribed(`project:${projectId.value}`)) return "online";
    return realtime.state === "connecting" ? "connecting" : "offline";
  });

  /** 流式文本还在来（发送键据此变停止）。 */
  const isStreaming = computed(() => Object.keys(streaming.value).length > 0);
  /** 折叠竖条：有会话在后台干活（转圈）/ 有会话在等我（红点）。 */
  const anyWorking = computed(() => sessions.value.some(item => isWorking(item.state)));
  const anyAwaiting = computed(() => sessions.value.some(item => isAwaitingUser(item.state)));

  // ---------------------------------------------------------------- 面板开合（按用户存本地）

  function panelKey(): string {
    const subject = tokenSubject(auth.accessToken) || "anon";
    return `${PANEL_KEY_PREFIX}:${subject}`;
  }

  /** 已按哪个用户读过面板偏好：同一个用户只读一次，免得打开项目时把刚设的开合状态又盖回去。 */
  let panelSubject: string | null = null;

  function loadPanelPrefs() {
    if (typeof localStorage === "undefined") return;
    const subject = tokenSubject(auth.accessToken) || "anon";
    if (panelSubject === subject) return;
    panelSubject = subject;
    try {
      const prefs = decodePanelPrefs(localStorage.getItem(panelKey()));
      panelOpen.value = prefs.open;
      panelWidth.value = prefs.width;
      panelFloating.value = prefs.floating;
      floatRight.value = prefs.floatRight;
      floatTop.value = prefs.floatTop;
    } catch {
      // 读不到只是不记得上次的状态。
    }
  }

  function savePanelPrefs() {
    if (typeof localStorage === "undefined") return;
    try {
      const prefs: PanelPrefs = {
        open: panelOpen.value, width: panelWidth.value, floating: panelFloating.value, floatRight: floatRight.value, floatTop: floatTop.value
      };
      localStorage.setItem(panelKey(), JSON.stringify(prefs));
    } catch {
      // 隐私模式下写不进去：只是下次不记得。
    }
  }

  function setPanelOpen(open: boolean) {
    panelOpen.value = open;
    savePanelPrefs();
  }

  function togglePanel() {
    setPanelOpen(!panelOpen.value);
  }

  function setPanelWidth(width: number) {
    panelWidth.value = clampPanelWidth(width);
    savePanelPrefs();
  }

  /** 悬浮 / 停靠切换：位置保留上一次拖到的地方。 */
  function setPanelFloating(floating: boolean) {
    panelFloating.value = floating;
    savePanelPrefs();
  }

  /** 悬浮卡片的位置（调用方先按窗口夹好，见 clampFloatPosition）。 */
  function setFloatPosition(right: number, top: number) {
    floatRight.value = Math.max(PANEL_FLOAT_MARGIN, Math.round(right));
    floatTop.value = Math.max(PANEL_FLOAT_MARGIN, Math.round(top));
    savePanelPrefs();
  }

  // ---------------------------------------------------------------- HTTP

  function api() {
    return useApi();
  }

  function sessionsPath(id: string) {
    return `/projects/${encodeURIComponent(id)}/agent/sessions`;
  }

  function sessionPath(id: string) {
    return `/agent/sessions/${encodeURIComponent(id)}`;
  }

  /** 拉本项目的会话列表。失败时保留已有列表。 */
  async function refreshSessions(): Promise<AgentSession[]> {
    const id = projectId.value;
    if (!id) return [];
    loadingSessions.value = true;
    try {
      const response = await api()<{ items: AgentSession[] }>(sessionsPath(id), { query: { limit: 50 } });
      if (projectId.value !== id) return response.items ?? [];
      sessionsByProject.value = { ...sessionsByProject.value, [id]: response.items ?? [] };
      return response.items ?? [];
    } finally {
      loadingSessions.value = false;
    }
  }

  /** 把会话详情里的会话写回列表（标题、状态随详情更新，不必再拉一次列表）。 */
  function upsertSummary(session: AgentSession) {
    const id = session.projectId || projectId.value;
    const list = sessionsByProject.value[id] ?? [];
    const index = list.findIndex(item => item.id === session.id);
    const next = index >= 0 ? list.map(item => (item.id === session.id ? session : item)) : [session, ...list];
    sessionsByProject.value = { ...sessionsByProject.value, [id]: next };
  }

  function mergePage(page: AgentMessagePage | undefined) {
    if (!page) return;
    messages.value = mergeMessages(messages.value, page.items ?? []);
    steps.value = byId(steps.value, page.steps);
    cards.value = byId(cards.value, page.cards);
    turns.value = byId(turns.value, page.turns);
  }

  /** 正式消息到了、或它那一轮已经结束：删掉对应的流式条目（否则界面出现两份同样的文字）。 */
  function pruneStreaming() {
    const ids = Object.keys(streaming.value);
    if (!ids.length) return;
    const landed = new Set(messages.value.map(message => message.id));
    const drop = ids.filter(id => {
      const turnId = streamingTurn.get(id);
      const turn = turnId ? turns.value[turnId] ?? (detail.value?.latestTurn?.id === turnId ? detail.value.latestTurn : undefined) : undefined;
      const finished = turn && turn.status !== "running" && turn.status !== "pending";
      return landed.has(id) || Boolean(finished);
    });
    if (!drop.length) return;
    for (const id of drop) streamingTurn.delete(id);
    streaming.value = Object.fromEntries(Object.entries(streaming.value).filter(([id]) => !drop.includes(id)));
  }

  let reconcileSeq = 0;
  /** 已写入的最新一次对账序号：慢的旧响应晚到时不覆盖新的会话详情（消息按 seq 合并，不受影响）。 */
  let appliedSeq = 0;
  let reconcileInFlight: Promise<void> | null = null;

  /**
   * 对账：`GET /agent/sessions/{id}`（状态、额度、待处理卡片、最近一页消息）+ 按 seq 合并。
   * 最近一页与本地已有的之间有缺口时往前补（断线期间来了很多消息）。
   * ⚠️ 失败不清空本地状态，只记 `syncError`。
   */
  async function reconcile(): Promise<void> {
    const id = currentId.value;
    if (!id) return;
    const seq = ++reconcileSeq;
    const run = (async () => {
      try {
        const value = await api()<AgentSessionDetail>(sessionPath(id));
        if (currentId.value !== id) return;
        const knownMax = maxSeq(messages.value);
        mergePage(value.messages);
        // 补缺口：本地最大 seq 与这一页的最小 seq 之间还有没拿到的消息。
        let cursor = value.messages?.nextCursor ?? null;
        let pageMin = minSeq(value.messages?.items ?? []);
        let guard = 0;
        while (knownMax !== undefined && cursor && pageMin !== undefined && pageMin > knownMax + 1 && guard < MAX_BACKFILL_PAGES) {
          guard += 1;
          const older = await api()<AgentMessagePage>(`${sessionPath(id)}/messages`, { query: { cursor } });
          if (currentId.value !== id) return;
          mergePage(older);
          cursor = older.nextCursor ?? null;
          pageMin = minSeq(older.items ?? []);
        }
        if (knownMax === undefined) olderCursor.value = value.messages?.nextCursor ?? null;
        if (seq > appliedSeq) {
          appliedSeq = seq;
          detail.value = value;
          upsertSummary(value.session);
        }
        if (value.latestTurn) turns.value = byId(turns.value, [value.latestTurn]);
        pruneStreaming();
        if (seq === reconcileSeq) syncError.value = "";
      } catch (cause) {
        if (currentId.value === id) syncError.value = errorText(cause);
      }
    })();
    reconcileInFlight = run;
    try {
      await run;
    } finally {
      if (reconcileInFlight === run) reconcileInFlight = null;
    }
  }

  // ---------------------------------------------------------------- 节流（尾随执行）

  let throttleTimer: ReturnType<typeof setTimeout> | undefined;
  /** 节流窗口里 / 对账进行中又来了信号：结束后再对一次。 */
  let dirty = false;

  /**
   * 事件 → 对账，节流 300ms 且**尾随执行**：窗口内的一串事件只对一次账，且那一次发生在最后一个事件之后。
   * 对账进行中又来了事件：等它结束再补一次（不并发两个对账，也不漏掉最后那个事件）。
   */
  function requestReconcile() {
    dirty = true;
    if (throttleTimer) return;
    throttleTimer = setTimeout(fireThrottled, RECONCILE_THROTTLE_MS);
  }

  async function fireThrottled() {
    if (reconcileInFlight) {
      await reconcileInFlight.catch(() => undefined);
    }
    throttleTimer = undefined;
    if (!dirty) return;
    dirty = false;
    await reconcile();
    if (dirty && !throttleTimer) throttleTimer = setTimeout(fireThrottled, RECONCILE_THROTTLE_MS);
  }

  let listTimer: ReturnType<typeof setTimeout> | undefined;
  function requestListRefresh() {
    if (listTimer) return;
    listTimer = setTimeout(() => {
      listTimer = undefined;
      void refreshSessions().catch(() => undefined);
    }, RECONCILE_THROTTLE_MS);
  }

  function clearTimers() {
    clearTimeout(throttleTimer);
    clearTimeout(listTimer);
    throttleTimer = undefined;
    listTimer = undefined;
    dirty = false;
  }

  // ---------------------------------------------------------------- 事件

  /** 事件只处理属于**本项目会话列表**里的会话；其余（别的项目、不认识的会话）一律丢弃。 */
  function knownSession(sessionId: string): boolean {
    return Boolean(sessionId) && sessions.value.some(item => item.id === sessionId);
  }

  function onAgentSignal(frame: Record<string, unknown>) {
    const sessionId = frameString(frame, "sessionId");
    if (!knownSession(sessionId) && sessionId !== currentId.value) return;
    if (sessionId === currentId.value) requestReconcile();
    // 会话下拉里的转圈 / 红点：状态变了就（节流地）刷新列表。
    if (frame.type === "agent.state" && sessionId !== currentId.value) requestListRefresh();
  }

  function onDelta(frame: Record<string, unknown>) {
    const sessionId = frameString(frame, "sessionId");
    if (!sessionId || sessionId !== currentId.value) return;
    const messageId = frameString(frame, "messageId");
    const text = typeof frame.text === "string" ? frame.text : "";
    if (!messageId || !text) return;
    // 正式消息已经到了：迟到的增量丢掉（否则同一段文字显示两遍）。
    if (messages.value.some(message => message.id === messageId)) return;
    const turnId = frameString(frame, "turnId");
    if (turnId) streamingTurn.set(messageId, turnId);
    streaming.value = { ...streaming.value, [messageId]: (streaming.value[messageId] ?? "") + text };
  }

  let detach: Array<() => void> = [];
  let releaseTopic: (() => void) | undefined;

  function onVisibility() {
    if (typeof document !== "undefined" && document.visibilityState === "visible") void reconcile();
  }

  function attach() {
    if (detach.length) return;
    detach = [
      ...AGENT_EVENTS.map(type => realtime.on(type, frame => onAgentSignal(frame))),
      realtime.on("agent.delta", frame => onDelta(frame)),
      realtime.onReconnected(() => {
        void reconcile();
        requestListRefresh();
      })
    ];
    if (typeof document !== "undefined") {
      document.addEventListener("visibilitychange", onVisibility);
      detach.push(() => document.removeEventListener("visibilitychange", onVisibility));
    }
  }

  function resetSessionView() {
    detail.value = null;
    messages.value = [];
    steps.value = {};
    cards.value = {};
    turns.value = {};
    olderCursor.value = null;
    stoppedTasks.value = [];
    streaming.value = {};
    streamingTurn.clear();
    syncError.value = "";
    reconcileSeq += 1;
  }

  // ---------------------------------------------------------------- 打开 / 关闭项目

  /**
   * 打开画布页的 Agent：订阅 `project:{id}`、拉会话列表、选中会话（`sessionId` 优先，否则最近活动的）并对账。
   * ⚠️ 只读不写：打开页面不建会话、不改任何数据。
   */
  async function openProject(id: string, sessionId?: string | null): Promise<void> {
    if (!id) return;
    loadPanelPrefs();
    if (projectId.value !== id) {
      closeProject();
      projectId.value = id;
      releaseTopic = realtime.subscribe(`project:${id}`);
      attach();
      realtime.connect();
    }
    let list: AgentSession[] = sessions.value;
    try {
      list = await refreshSessions();
    } catch (cause) {
      syncError.value = errorText(cause);
    }
    if (projectId.value !== id) return;
    // 指定的会话不在列表里（列表只取最近 50 个、或列表没拉到）也照样打开：详情接口会校验归属。
    const target = sessionId || list[0]?.id || "";
    if (target) await switchSession(target);
    else newSession();
  }

  function closeProject() {
    for (const off of detach) off();
    detach = [];
    releaseTopic?.();
    releaseTopic = undefined;
    clearTimers();
    projectId.value = "";
    currentId.value = "";
    resetSessionView();
    pendingSend = null;
    // 别处还订着 topic（生成、画布协作）就不断开连接；没人用了才断。
    if (realtime.activeTopics().length === 0) realtime.disconnect();
  }

  // ---------------------------------------------------------------- 会话

  async function switchSession(id: string): Promise<void> {
    if (!id) {
      newSession();
      return;
    }
    if (currentId.value !== id) {
      currentId.value = id;
      resetSessionView();
    }
    loadingDetail.value = true;
    try {
      await reconcile();
    } finally {
      loadingDetail.value = false;
    }
  }

  /** 新会话草稿：清空当前视图，下一次发送时建会话。正在运行的会话不受影响，继续在后台跑。 */
  function newSession() {
    currentId.value = "";
    resetSessionView();
  }

  /** 建会话（带首条消息）。成功后切到这个会话。 */
  async function createSession(input: AgentCreateSessionRequest, idempotencyKey: string = newIdempotencyKey()): Promise<AgentWriteResult> {
    const result = await api()<AgentWriteResult>("/agent/sessions", {
      method: "POST",
      headers: { "Idempotency-Key": idempotencyKey },
      body: input
    });
    if (projectId.value && result.projectId === projectId.value) {
      currentId.value = result.sessionId;
      resetSessionView();
      await Promise.all([reconcile(), refreshSessions().catch(() => undefined)]);
    }
    return result;
  }

  /** 进行中的发送意图：内容不变的重试复用同一个幂等键，成功后清掉。 */
  let pendingSend: { fingerprint: string; key: string } | null = null;

  /**
   * 发消息。没有当前会话时建会话（首条消息随建会话一起发）。
   *
   * ⚠️ `Idempotency-Key`：同一次点击的重试（内容相同）**复用同一个键** —— 网络超时而服务端其实已收到时，
   *    重试只会拿回同一轮，不会再建一轮、再扣一次对话费；内容一变就是新的意图，换新键。
   * ⚠️ 失败时抛出原错误，由输入框保留正文 / 胶囊 / 附件并显示原话。
   */
  async function send(text: string, refs: AgentRef[] = [], opts: SendOptions = {}): Promise<AgentWriteResult> {
    const body = {
      text: text.trim(),
      refs: refs.map(ref => ({ kind: ref.kind, id: ref.id })),
      ...(opts.skillCode ? { skillCode: opts.skillCode } : {}),
      ...(opts.prefs ? { prefs: opts.prefs } : {})
    };
    const fingerprint = sendFingerprint(currentId.value, body.text, body.refs, opts);
    const key = opts.idempotencyKey
      ?? (pendingSend?.fingerprint === fingerprint ? pendingSend.key : newIdempotencyKey());
    pendingSend = { fingerprint, key };
    sending.value = true;
    try {
      let result: AgentWriteResult;
      if (!currentId.value) {
        if (!projectId.value) throw new Error("还没有打开项目");
        result = await createSession({
          projectId: projectId.value,
          ...(opts.title ? { title: opts.title } : {}),
          message: { text: body.text, refs: body.refs },
          ...(body.skillCode ? { skillCode: body.skillCode } : {}),
          ...(body.prefs ? { prefs: body.prefs } : {})
        }, key);
      } else {
        result = await api()<AgentWriteResult>(`${sessionPath(currentId.value)}/messages`, {
          method: "POST",
          headers: { "Idempotency-Key": key },
          body
        });
        await reconcile();
      }
      if (pendingSend?.key === key) pendingSend = null;
      return result;
    } finally {
      sending.value = false;
    }
  }

  async function submitCard(cardId: string, submission: Record<string, unknown>): Promise<AgentSubmitResult> {
    const id = currentId.value;
    try {
      return await api()<AgentSubmitResult>(`${sessionPath(id)}/cards/${encodeURIComponent(cardId)}/submit`, {
        method: "POST",
        body: { submission }
      });
    } finally {
      // 成败都对一次账：409 价格已更新时卡片的新报价、作废的卡片都要拉回来。
      if (currentId.value === id) await reconcile();
    }
  }

  /** 停止：服务端写停止标记，返回本会话未终态的生成任务（可各自取消）。 */
  async function stop(): Promise<AgentStopResult> {
    const id = currentId.value;
    const result = await api()<AgentStopResult>(`${sessionPath(id)}/stop`, { method: "POST" });
    if (currentId.value === id) {
      stoppedTasks.value = result.tasks ?? [];
      await reconcile();
    }
    return result;
  }

  async function cancel(opts: { cancelTasks?: boolean } = {}): Promise<AgentCancelResult> {
    const id = currentId.value;
    const result = await api()<AgentCancelResult>(`${sessionPath(id)}/cancel`, {
      method: "POST",
      body: { cancelTasks: opts.cancelTasks === true }
    });
    if (currentId.value === id) await reconcile();
    return result;
  }

  /** 改标题 / 会话级偏好（只影响本会话后续轮次）。 */
  async function patchSession(patch: { title?: string; prefs?: AgentPrefs }): Promise<AgentSession> {
    const id = currentId.value;
    const session = await api()<AgentSession>(sessionPath(id), { method: "PATCH", body: patch });
    upsertSummary(session);
    if (detail.value?.session.id === session.id) detail.value = { ...detail.value, session };
    return session;
  }

  /** 往前翻一页更早的消息（默认接着已有的游标）。 */
  async function loadMoreMessages(cursor: string | null = olderCursor.value): Promise<void> {
    const id = currentId.value;
    if (!id || !cursor || loadingOlder.value) return;
    loadingOlder.value = true;
    try {
      const page = await api()<AgentMessagePage>(`${sessionPath(id)}/messages`, { query: { cursor } });
      if (currentId.value !== id) return;
      mergePage(page);
      olderCursor.value = page.nextCursor ?? null;
    } finally {
      loadingOlder.value = false;
    }
  }

  async function turnLedger(turnId: string): Promise<AgentTurnLedger> {
    return await api()<AgentTurnLedger>(`${sessionPath(currentId.value)}/turns/${encodeURIComponent(turnId)}/ledger`);
  }

  // 切换账号：面板偏好按用户存，换人之后重新读一次。
  watchSource(() => auth.accessToken, (next, prev) => {
    if (tokenSubject(next) !== tokenSubject(prev)) loadPanelPrefs();
  });
  loadPanelPrefs();

  return {
    // state
    projectId,
    sessionsByProject,
    sessions,
    currentId,
    current,
    detail,
    messages,
    steps,
    cards,
    turns,
    olderCursor,
    streaming,
    isStreaming,
    anyWorking,
    anyAwaiting,
    loadingSessions,
    loadingDetail,
    loadingOlder,
    sending,
    syncError,
    stoppedTasks,
    connection,
    panelOpen,
    panelWidth,
    panelFloating,
    floatRight,
    floatTop,
    // actions
    openProject,
    closeProject,
    refreshSessions,
    switchSession,
    newSession,
    createSession,
    send,
    submitCard,
    stop,
    cancel,
    patchSession,
    loadMoreMessages,
    turnLedger,
    reconcile,
    setPanelOpen,
    togglePanel,
    setPanelWidth,
    setPanelFloating,
    setFloatPosition
  };
});

/** 错误原话（断网说断网）。 */
function errorText(cause: unknown): string {
  if (typeof navigator !== "undefined" && navigator.onLine === false) return "网络已断开，恢复后会自动同步";
  const message = typeof cause === "object" && cause !== null && "message" in cause
    ? String((cause as { message?: unknown }).message ?? "")
    : "";
  return message && message !== "请求失败" ? message : "同步没有成功，稍后会再试";
}
