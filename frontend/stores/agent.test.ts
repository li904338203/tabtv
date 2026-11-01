import { computed, ref } from "vue";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

/**
 * Agent store 的对账行为（AGT-27）。vitest 里没有 Nuxt / Pinia 自动导入，全局函数一律注入替身；
 * 实时连接用假的 realtime（手动触发 `on` 注册的回调），HTTP 用按路径应答的假 `useApi`。
 *
 * 钉住的都是「错了不报错」的：
 *  - 一串事件只对一次账、且那一次在最后一个事件之后（否则漏掉最后的状态，或一秒几十个请求）；
 *  - delta 不发请求，正式消息到了就删掉流式条目（否则同一段文字显示两遍）；
 *  - 对账失败不清空本地（否则一次抖动眼前的消息全没了）；
 *  - 重试复用幂等键（否则超时重试会再建一轮、再扣一次对话费）。
 */
type Handler = (frame: Record<string, unknown>) => void;
type Call = { path: string; method: string; headers?: Record<string, string>; body?: unknown; query?: unknown };

function message(seq: number, role = "user", id = `m${seq}`) {
  return { id, seq, role, content: `c${seq}`, refs: [], toolCalls: null, toolCallId: null, turnId: "t1", cardId: null, createdAt: "2026-10-15T00:00:00Z" };
}

function session(id: string, state = "idle") {
  return {
    id, workspaceId: "w1", projectId: "p1", title: "会话", state, createdByUserId: "u1", initiatorName: "我",
    isInitiator: true, skillCodes: [], prefs: {}, todo: [], approvedBudget: 0, planDocVersion: null, stopRequested: false,
    lastActiveAt: "2026-10-15T00:00:00Z", createdAt: "2026-10-15T00:00:00Z", updatedAt: "2026-10-15T00:00:00Z"
  };
}

function detailOf(id: string, items: ReturnType<typeof message>[], extra: Record<string, unknown> = {}) {
  return {
    session: session(id, "planning"),
    budget: { approved: 0, spent: 0, refunded: 0 },
    pendingCards: [],
    latestTurn: null,
    messages: { items, steps: [], cards: [], turns: [], nextCursor: null },
    ...extra
  };
}

function setup() {
  const handlers = new Map<string, Set<Handler>>();
  const reconnected = new Set<() => void>();
  const subscribed: string[] = [];
  const released: string[] = [];
  const realtime = {
    on: (type: string, fn: Handler) => {
      if (!handlers.has(type)) handlers.set(type, new Set());
      handlers.get(type)!.add(fn);
      return () => handlers.get(type)!.delete(fn);
    },
    onReconnected: (fn: () => void) => {
      reconnected.add(fn);
      return () => reconnected.delete(fn);
    },
    subscribe: (topic: string) => {
      subscribed.push(topic);
      return () => released.push(topic);
    },
    activeTopics: () => [] as string[],
    connect: vi.fn(),
    disconnect: vi.fn()
  };
  const calls: Call[] = [];
  const state = {
    sessions: [session("s1"), session("s2")],
    detail: detailOf("s1", [message(1), message(2, "assistant")]) as unknown,
    failDetail: false,
    failPost: 0
  };
  const api = async (path: string, options: { method?: string; headers?: Record<string, string>; body?: unknown; query?: unknown } = {}) => {
    const method = options.method ?? "GET";
    calls.push({ path, method, headers: options.headers, body: options.body, query: options.query });
    if (method === "GET" && path === "/projects/p1/agent/sessions") return { items: state.sessions };
    if (method === "GET" && /^\/agent\/sessions\/[^/]+$/.test(path)) {
      if (state.failDetail) throw Object.assign(new Error("网关超时"), { data: {} });
      return state.detail;
    }
    if (method === "POST" && path.endsWith("/messages")) {
      if (state.failPost > 0) {
        state.failPost -= 1;
        throw Object.assign(new Error("网络超时"), { data: {} });
      }
      return { sessionId: "s1", projectId: "p1", state: "planning", turnId: "t9", messageId: "m9", queued: false, replayed: false };
    }
    if (method === "POST" && path === "/agent/sessions") {
      return { sessionId: "s3", projectId: "p1", state: "planning", turnId: "t1", messageId: "m1", queued: false, replayed: false };
    }
    throw new Error(`unexpected ${method} ${path}`);
  };
  const storage = new Map<string, string>();
  vi.stubGlobal("ref", ref);
  vi.stubGlobal("computed", computed);
  vi.stubGlobal("defineStore", (_id: string, setupFn: () => unknown) => {
    let instance: unknown;
    return () => (instance ??= setupFn());
  });
  vi.stubGlobal("useRealtimeStore", () => realtime);
  vi.stubGlobal("useAuthStore", () => ({ accessToken: "" }));
  vi.stubGlobal("useApi", () => api);
  vi.stubGlobal("localStorage", {
    getItem: (k: string) => storage.get(k) ?? null,
    setItem: (k: string, v: string) => { storage.set(k, v); },
    removeItem: (k: string) => { storage.delete(k); }
  });
  const emit = (type: string, frame: Record<string, unknown>) => {
    for (const fn of handlers.get(type) ?? []) fn({ type, ...frame });
  };
  return { calls, state, emit, reconnected, subscribed, released, handlers, storage };
}

type Store = {
  openProject: (id: string, sessionId?: string) => Promise<void>;
  closeProject: () => void;
  send: (text: string, refs?: Array<{ kind: "node" | "media" | "asset"; id: string }>, opts?: Record<string, unknown>) => Promise<unknown>;
  reconcile: () => Promise<void>;
  newSession: () => void;
  setPanelWidth: (w: number) => void;
  setPanelFloating: (floating: boolean) => void;
  setFloatPosition: (right: number, top: number) => void;
  messages: { value: Array<{ id: string; seq: number }> };
  streaming: { value: Record<string, string> };
  current: { value: { id: string; state: string } | null };
  currentId: { value: string };
  syncError: { value: string };
  panelWidth: { value: number };
};

async function openStore(env: ReturnType<typeof setup>, sessionId?: string) {
  const { useAgentStore } = await import("./agent");
  const store = useAgentStore() as unknown as Store;
  await store.openProject("p1", sessionId);
  return store;
}

const detailCalls = (calls: Call[]) => calls.filter(call => call.method === "GET" && /^\/agent\/sessions\/[^/]+$/.test(call.path));

beforeEach(() => {
  vi.resetModules();
  vi.useFakeTimers();
});
afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

describe("Agent store", () => {
  it("打开项目时订阅、关闭时退订", async () => {
    const env = setup();
    const store = await openStore(env);
    expect(env.subscribed).toEqual(["project:p1"]);
    expect(store.currentId.value).toBe("s1"); // 最近活动的那一个
    expect(store.messages.value.map(m => m.seq)).toEqual([1, 2]);
    store.closeProject();
    expect(env.released).toEqual(["project:p1"]);
    expect(env.handlers.get("agent.state")?.size ?? 0).toBe(0);
  });

  it("指定的会话优先", async () => {
    const env = setup();
    const store = await openStore(env, "s2");
    expect(store.currentId.value).toBe("s2");
  });

  it("事件节流后只对账一次且尾随执行", async () => {
    const env = setup();
    await openStore(env);
    const before = detailCalls(env.calls).length;
    for (let i = 0; i < 10; i += 1) {
      env.emit("agent.step", { sessionId: "s1", turnId: "t1", stepId: `x${i}`, tool: "read_canvas", status: "running" });
      await vi.advanceTimersByTimeAsync(25);
    }
    // 最后一个事件到达时还没有发请求
    expect(detailCalls(env.calls).length - before).toBe(0);
    await vi.advanceTimersByTimeAsync(400);
    const after = detailCalls(env.calls).length - before;
    expect(after).toBeGreaterThanOrEqual(1);
    expect(after).toBeLessThanOrEqual(2);
  });

  it("delta 只追加不请求", async () => {
    const env = setup();
    const store = await openStore(env);
    const before = env.calls.length;
    env.emit("agent.delta", { sessionId: "s1", turnId: "t1", messageId: "m3", text: "你好" });
    env.emit("agent.delta", { sessionId: "s1", turnId: "t1", messageId: "m3", text: "，世界" });
    await vi.advanceTimersByTimeAsync(1000);
    expect(env.calls.length).toBe(before);
    expect(store.streaming.value).toEqual({ m3: "你好，世界" });
  });

  it("正式消息替换流式文本", async () => {
    const env = setup();
    const store = await openStore(env);
    env.emit("agent.delta", { sessionId: "s1", turnId: "t1", messageId: "m3", text: "你好" });
    env.state.detail = detailOf("s1", [message(1), message(2, "assistant"), message(3, "assistant", "m3")]);
    env.emit("agent.state", { sessionId: "s1", state: "idle", approvedBudget: 0, spent: 0 });
    await vi.advanceTimersByTimeAsync(400);
    expect(store.streaming.value).toEqual({});
    expect(store.messages.value.map(m => m.id)).toEqual(["m1", "m2", "m3"]);
    // 正式消息到了之后迟到的增量不再显示
    env.emit("agent.delta", { sessionId: "s1", turnId: "t1", messageId: "m3", text: "迟到" });
    expect(store.streaming.value).toEqual({});
  });

  it("重连后对账，重复到达的消息按 seq 去重", async () => {
    const env = setup();
    const store = await openStore(env);
    const before = detailCalls(env.calls).length;
    env.state.detail = detailOf("s1", [message(2, "assistant"), message(3, "assistant")]);
    for (const fn of env.reconnected) fn();
    await vi.advanceTimersByTimeAsync(0);
    expect(detailCalls(env.calls).length - before).toBe(1);
    expect(store.messages.value.map(m => m.seq)).toEqual([1, 2, 3]);
  });

  it("对账失败保留本地状态", async () => {
    const env = setup();
    const store = await openStore(env);
    const messages = store.messages.value;
    const current = store.current.value;
    env.state.failDetail = true;
    await store.reconcile();
    expect(store.messages.value).toBe(messages);
    expect(store.current.value).toEqual(current);
    expect(store.syncError.value).toBe("网关超时");
  });

  it("重试复用幂等键", async () => {
    const env = setup();
    const store = await openStore(env);
    env.state.failPost = 1;
    await expect(store.send("三打白骨精", [{ kind: "node", id: "n1" }])).rejects.toThrow();
    await store.send("三打白骨精", [{ kind: "node", id: "n1" }]);
    const posts = env.calls.filter(call => call.method === "POST" && call.path.endsWith("/messages"));
    expect(posts).toHaveLength(2);
    const first = posts[0]!.headers!["Idempotency-Key"];
    expect(first).toBeTruthy();
    expect(posts[1]!.headers!["Idempotency-Key"]).toBe(first);
    expect(posts[1]!.body).toEqual({ text: "三打白骨精", refs: [{ kind: "node", id: "n1" }] });
    // 新的一次发送：新键
    await store.send("三打白骨精", [{ kind: "node", id: "n1" }]);
    const third = env.calls.filter(call => call.method === "POST" && call.path.endsWith("/messages"))[2]!;
    expect(third.headers!["Idempotency-Key"]).not.toBe(first);
  });

  it("没有当前会话时发送即建会话", async () => {
    const env = setup();
    const store = await openStore(env);
    store.newSession();
    await store.send("新的想法", [], { skillCode: "story" });
    const create = env.calls.find(call => call.method === "POST" && call.path === "/agent/sessions")!;
    expect(create.body).toEqual({ projectId: "p1", message: { text: "新的想法", refs: [] }, skillCode: "story" });
    expect(create.headers!["Idempotency-Key"]).toBeTruthy();
    expect(store.currentId.value).toBe("s3");
  });

  it("其他项目的事件被丢弃", async () => {
    const env = setup();
    const store = await openStore(env);
    const before = env.calls.length;
    env.emit("agent.state", { sessionId: "other-project-session", state: "executing", approvedBudget: 0, spent: 0 });
    env.emit("agent.step", { sessionId: "other-project-session", turnId: "t", stepId: "s", tool: "x", status: "running" });
    env.emit("agent.delta", { sessionId: "other-project-session", turnId: "t", messageId: "mx", text: "别人的" });
    await vi.advanceTimersByTimeAsync(1000);
    expect(env.calls.length).toBe(before);
    expect(store.streaming.value).toEqual({});
  });

  it("面板宽度夹在 320~640 并按用户存本地", async () => {
    const env = setup();
    const store = await openStore(env);
    store.setPanelWidth(1000);
    expect(store.panelWidth.value).toBe(640);
    store.setPanelWidth(100);
    expect(store.panelWidth.value).toBe(320);
    expect([...env.storage.keys()].some(key => key.startsWith("seeaitv.agent.panel:"))).toBe(true);
  });

  it("悬浮开关与位置按用户存本地（2026-11-01）", async () => {
    const env = setup();
    const store = await openStore(env);
    store.setPanelFloating(true);
    store.setFloatPosition(120.4, 3);
    const saved = [...env.storage.entries()].find(([key]) => key.startsWith("seeaitv.agent.panel:"))?.[1];
    expect(JSON.parse(saved ?? "{}")).toMatchObject({ floating: true, floatRight: 120, floatTop: 8 });
  });

  it("面板偏好：此前存的（没有悬浮这几项）按停靠、默认位置读；悬浮与位置照读，坏值回默认", async () => {
    setup();
    const { decodePanelPrefs, PANEL_FLOAT_DEFAULT } = await import("./agent");
    expect(decodePanelPrefs(JSON.stringify({ open: true, width: 480 }))).toEqual({
      open: true, width: 480, floating: false, floatRight: PANEL_FLOAT_DEFAULT.right, floatTop: PANEL_FLOAT_DEFAULT.top
    });
    expect(decodePanelPrefs(JSON.stringify({ open: true, width: 480, floating: true, floatRight: 120, floatTop: 300 })))
      .toMatchObject({ floating: true, floatRight: 120, floatTop: 300 });
    expect(decodePanelPrefs(JSON.stringify({ floating: "yes", floatRight: "abc", floatTop: null })))
      .toMatchObject({ floating: false, floatRight: PANEL_FLOAT_DEFAULT.right, floatTop: PANEL_FLOAT_DEFAULT.top });
    expect(decodePanelPrefs(JSON.stringify({ floatRight: -50, floatTop: 2 }))).toMatchObject({ floatRight: 8, floatTop: 8 });
    expect(decodePanelPrefs("{坏")).toMatchObject({ open: false, floating: false });
  });

  it("拖动悬浮卡片：整张卡片留在窗口里、四边至少留 8；窗口比卡片还小时贴右上", async () => {
    setup();
    const { clampFloatPosition } = await import("./agent");
    const card = { width: 400, height: 680 };
    const viewport = { width: 1440, height: 900 };
    expect(clampFloatPosition({ right: 100, top: 100 }, card, viewport)).toEqual({ right: 100, top: 100 });
    expect(clampFloatPosition({ right: -40, top: -40 }, card, viewport)).toEqual({ right: 8, top: 8 });
    expect(clampFloatPosition({ right: 5000, top: 5000 }, card, viewport)).toEqual({ right: 1032, top: 212 });
    expect(clampFloatPosition({ right: 300, top: 300 }, card, { width: 300, height: 500 })).toEqual({ right: 8, top: 8 });
  });
});
