import { computed, reactive, ref } from "vue";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

/**
 * 实时连接 store。vitest 里没有 Nuxt / Pinia 的自动导入：按 store 用到的全局函数注入替身，
 * 并用一个可编程的假 WebSocket 驱动帧序列。
 *
 * defineStore 的替身用 `reactive()` 包一层：与 Pinia 一样把 ref 解包（`store.state` 直接是字符串），
 * 并做成单例（同一个 id 多次 use 拿到同一个实例）。
 */
class FakeSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSING = 2;
  static CLOSED = 3;
  static instances: FakeSocket[] = [];

  readyState = FakeSocket.CONNECTING;
  sent: Array<Record<string, unknown>> = [];
  onopen: ((e: unknown) => void) | null = null;
  onmessage: ((e: { data: string }) => void) | null = null;
  onclose: ((e: unknown) => void) | null = null;
  onerror: ((e: unknown) => void) | null = null;

  constructor(public url: string) {
    FakeSocket.instances.push(this);
  }

  send(data: string) {
    this.sent.push(JSON.parse(data) as Record<string, unknown>);
  }

  close() {
    if (this.readyState === FakeSocket.CLOSED) return;
    this.readyState = FakeSocket.CLOSED;
    this.onclose?.({});
  }

  /** 握手成功（触发 onopen → 客户端发 auth 首帧）。 */
  open() {
    this.readyState = FakeSocket.OPEN;
    this.onopen?.({});
  }

  receive(frame: unknown) {
    this.onmessage?.({ data: typeof frame === "string" ? frame : JSON.stringify(frame) });
  }

  /** 服务端/网络把连接断了（非客户端主动）。 */
  drop() {
    this.close();
  }

  /** 发出去的帧里某一类型的全部。 */
  framesOf(type: string) {
    return this.sent.filter(f => f.type === type);
  }
}

type AuthStub = {
  accessToken: string;
  refreshAccessToken: ReturnType<typeof vi.fn>;
  withTokenRefresh: (send: (token: string) => Promise<unknown>) => Promise<unknown>;
};

/** 注入 store 共用的全局替身。返回假 auth，便于各用例改续期行为。 */
function stubRealtimeGlobals(extra: Record<string, unknown> = {}) {
  FakeSocket.instances = [];
  const instances = new Map<string, unknown>();
  const auth: AuthStub = {
    accessToken: "A1",
    refreshAccessToken: vi.fn(async () => {
      auth.accessToken = "A2";
      return "A2";
    }),
    withTokenRefresh: send => send(auth.accessToken),
  };
  vi.stubGlobal("ref", ref);
  vi.stubGlobal("computed", computed);
  vi.stubGlobal("defineStore", (id: string, setupFn: () => Record<string, unknown>) => () => {
    if (!instances.has(id)) instances.set(id, reactive(setupFn()));
    return instances.get(id);
  });
  vi.stubGlobal("useRuntimeConfig", () => ({ public: { apiBase: "/api/v1", wsUrl: "", wsApiPort: "8180" } }));
  vi.stubGlobal("useAuthStore", () => auth);
  vi.stubGlobal("WebSocket", FakeSocket);
  vi.stubGlobal("window", { location: { protocol: "http:", hostname: "localhost", host: "localhost:3000" } });
  for (const [name, value] of Object.entries(extra)) vi.stubGlobal(name, value);
  return { auth };
}

type RealtimeStore = {
  state: string;
  isSubscribed: (topic: string) => boolean;
  activeTopics: () => string[];
  subscribe: (topic: string) => () => void;
  on: (type: string, handler: (frame: Record<string, unknown>) => void) => () => void;
  onReconnected: (handler: () => void) => () => void;
  onDisconnected: (handler: () => void) => () => void;
  connect: () => void;
  disconnect: () => void;
};

async function load() {
  const { useRealtimeStore } = await import("./realtime");
  return useRealtimeStore() as unknown as RealtimeStore;
}

/** 连上并鉴权通过，返回这条 socket。 */
function handshake(rt: RealtimeStore) {
  rt.connect();
  const ws = FakeSocket.instances.at(-1)!;
  ws.open();
  ws.receive({ type: "auth.ok", userId: "1", sessionId: "s" });
  return ws;
}

beforeEach(() => {
  vi.resetModules();
  vi.useFakeTimers();
});
afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

describe("实时连接 store", () => {
  it("握手：首帧发 auth（token 不入 URL），连接地址跟着页面主机名走", async () => {
    stubRealtimeGlobals();
    const rt = await load();
    rt.connect();
    const ws = FakeSocket.instances[0];
    expect(ws.url).toBe("ws://localhost:8180/ws/events");
    expect(rt.state).toBe("connecting");
    ws.open();
    expect(ws.sent[0]).toEqual({ type: "auth", token: "A1" });
    // 重复 connect 不开第二条连接
    rt.connect();
    expect(FakeSocket.instances).toHaveLength(1);
  });

  it("引用计数：最后一个释放才退订", async () => {
    stubRealtimeGlobals();
    const rt = await load();
    const ws = handshake(rt);
    const a = rt.subscribe("workspace:9");
    const b = rt.subscribe("workspace:9");
    expect(ws.framesOf("subscribe")).toEqual([{ type: "subscribe", topics: ["workspace:9"] }]);
    ws.receive({ type: "subscribe.ok", topics: ["workspace:9"], rejected: [] });
    a();
    expect(ws.framesOf("unsubscribe")).toEqual([]);
    expect(rt.isSubscribed("workspace:9")).toBe(true);
    b();
    expect(ws.framesOf("unsubscribe")).toEqual([{ type: "unsubscribe", topics: ["workspace:9"] }]);
    expect(rt.isSubscribed("workspace:9")).toBe(false);
    expect(rt.activeTopics()).toEqual([]);
  });

  it("release 重复调用只生效一次", async () => {
    stubRealtimeGlobals();
    const rt = await load();
    const ws = handshake(rt);
    const a = rt.subscribe("workspace:9");
    rt.subscribe("workspace:9");
    a();
    a(); // ⚠️ 第二次不能再减计数，否则把另一处的订阅也减没了
    expect(ws.framesOf("unsubscribe")).toEqual([]);
    expect(rt.activeTopics()).toEqual(["workspace:9"]);
  });

  it("按 type 分发且 off 后不再收到", async () => {
    stubRealtimeGlobals();
    const rt = await load();
    const ws = handshake(rt);
    const got: unknown[] = [];
    const other: unknown[] = [];
    const off = rt.on("generation.update", f => got.push(f));
    rt.on("workflow.update", f => other.push(f));
    ws.receive({ type: "generation.update", generationId: "1" });
    expect(got).toEqual([{ type: "generation.update", generationId: "1" }]);
    expect(other).toEqual([]);
    off();
    ws.receive({ type: "generation.update", generationId: "2" });
    expect(got).toHaveLength(1);
  });

  it("内部帧（auth.* / subscribe.ok / subscription.revoked / pong）不对外分发", async () => {
    stubRealtimeGlobals();
    const rt = await load();
    const ws = handshake(rt);
    const got: unknown[] = [];
    for (const type of ["auth.ok", "auth.error", "subscribe.ok", "subscription.revoked", "pong"]) rt.on(type, f => got.push(f));
    ws.receive({ type: "pong" });
    ws.receive({ type: "subscribe.ok", topics: [], rejected: [] });
    ws.receive({ type: "subscription.revoked", topics: [] });
    expect(got).toEqual([]);
  });

  it("非法 JSON 帧被丢弃不抛错", async () => {
    stubRealtimeGlobals();
    const rt = await load();
    const ws = handshake(rt);
    const got: unknown[] = [];
    rt.on("generation.update", f => got.push(f));
    expect(() => ws.receive("{not json")).not.toThrow();
    expect(() => ws.receive("null")).not.toThrow();
    expect(() => ws.receive("42")).not.toThrow();
    expect(got).toEqual([]);
  });

  it("一个处理函数抛错不打断其余处理函数", async () => {
    stubRealtimeGlobals();
    const rt = await load();
    const ws = handshake(rt);
    const got: unknown[] = [];
    rt.on("generation.update", () => {
      throw new Error("boom");
    });
    rt.on("generation.update", f => got.push(f));
    ws.receive({ type: "generation.update" });
    expect(got).toHaveLength(1);
  });

  it("鉴权前订阅只记账，auth.ok 后一帧发出全部 topic", async () => {
    stubRealtimeGlobals();
    const rt = await load();
    rt.subscribe("workspace:9");
    rt.subscribe("project:5");
    rt.connect();
    const ws = FakeSocket.instances[0];
    ws.open();
    expect(ws.framesOf("subscribe")).toEqual([]);
    ws.receive({ type: "auth.ok" });
    expect(ws.framesOf("subscribe")).toEqual([{ type: "subscribe", topics: ["workspace:9", "project:5"] }]);
    expect(rt.state).toBe("connecting");
    ws.receive({ type: "subscribe.ok", topics: ["workspace:9", "project:5"], rejected: [] });
    expect(rt.state).toBe("online");
  });

  it("重连后重发全部 topic 且 subscribe.ok 之后才触发 onReconnected", async () => {
    stubRealtimeGlobals();
    const rt = await load();
    const log: string[] = [];
    rt.onReconnected(() => log.push("reconnected"));
    rt.onDisconnected(() => log.push("disconnected"));
    rt.subscribe("workspace:9");
    rt.subscribe("project:5");
    const first = handshake(rt);
    first.receive({ type: "subscribe.ok", topics: ["workspace:9", "project:5"], rejected: [] });
    expect(log).toEqual(["reconnected"]);

    first.drop();
    expect(log).toEqual(["reconnected", "disconnected"]);
    expect(rt.state).toBe("offline");
    expect(rt.isSubscribed("workspace:9")).toBe(false);
    await vi.advanceTimersByTimeAsync(1500);
    const second = FakeSocket.instances[1];
    expect(second).toBeDefined();
    second.open();
    second.receive({ type: "auth.ok" });
    // 全部 topic 已重发，但 subscribe.ok 还没到：不能触发
    expect(second.framesOf("subscribe")).toEqual([{ type: "subscribe", topics: ["workspace:9", "project:5"] }]);
    expect(log).toEqual(["reconnected", "disconnected"]);
    second.receive({ type: "subscribe.ok", topics: ["workspace:9", "project:5"], rejected: [] });
    expect(log).toEqual(["reconnected", "disconnected", "reconnected"]);
    expect(rt.isSubscribed("project:5")).toBe(true);
  });

  it("断网照常退避：1.5 秒、3 秒……订上之后清零", async () => {
    stubRealtimeGlobals();
    const rt = await load();
    rt.connect();
    FakeSocket.instances[0].drop();
    await vi.advanceTimersByTimeAsync(1499);
    expect(FakeSocket.instances).toHaveLength(1);
    await vi.advanceTimersByTimeAsync(1);
    expect(FakeSocket.instances).toHaveLength(2);
    FakeSocket.instances[1].drop();
    await vi.advanceTimersByTimeAsync(2999);
    expect(FakeSocket.instances).toHaveLength(2);
    await vi.advanceTimersByTimeAsync(1);
    expect(FakeSocket.instances).toHaveLength(3);
  });

  it("auth.error 后先续期再重连（新连接用新 token）", async () => {
    const { auth } = stubRealtimeGlobals();
    const rt = await load();
    rt.connect();
    const ws = FakeSocket.instances[0];
    ws.open();
    ws.receive({ type: "auth.error", code: 40101, message: "凭证无效" });
    ws.drop();
    expect(auth.refreshAccessToken).toHaveBeenCalledWith("A1");
    await vi.advanceTimersByTimeAsync(1500);
    const next = FakeSocket.instances[1];
    next.open();
    expect(next.sent[0]).toEqual({ type: "auth", token: "A2" });
  });

  it("续期被明确拒绝不再重连", async () => {
    const { auth } = stubRealtimeGlobals();
    auth.refreshAccessToken.mockRejectedValue({ status: 401 });
    const rt = await load();
    rt.connect();
    const ws = FakeSocket.instances[0];
    ws.open();
    ws.receive({ type: "auth.error" });
    ws.drop();
    await vi.advanceTimersByTimeAsync(60_000);
    expect(FakeSocket.instances).toHaveLength(1);
  });

  it("续期因断网失败（不是明确拒绝）照常重连", async () => {
    const { auth } = stubRealtimeGlobals();
    auth.refreshAccessToken.mockRejectedValue(new TypeError("Failed to fetch"));
    const rt = await load();
    rt.connect();
    const ws = FakeSocket.instances[0];
    ws.open();
    ws.receive({ type: "auth.error" });
    ws.drop();
    await vi.advanceTimersByTimeAsync(1500);
    expect(FakeSocket.instances).toHaveLength(2);
  });

  it("被拒的 topic 不算在线", async () => {
    stubRealtimeGlobals();
    const rt = await load();
    rt.subscribe("workspace:9");
    const ws = handshake(rt);
    ws.receive({ type: "subscribe.ok", topics: [], rejected: [{ topic: "workspace:9", code: 40301 }] });
    expect(rt.isSubscribed("workspace:9")).toBe(false);
    expect(rt.state).toBe("offline");
  });

  it("subscription.revoked 移除该 topic", async () => {
    stubRealtimeGlobals();
    const rt = await load();
    rt.subscribe("workspace:9");
    rt.subscribe("project:5");
    const ws = handshake(rt);
    ws.receive({ type: "subscribe.ok", topics: ["workspace:9", "project:5"], rejected: [] });
    ws.receive({ type: "subscription.revoked", topics: ["workspace:9"], reason: "member_removed" });
    expect(rt.isSubscribed("workspace:9")).toBe(false);
    expect(rt.isSubscribed("project:5")).toBe(true);
    expect(rt.state).toBe("online");
  });

  it("主动断开不重连、不触发 onDisconnected", async () => {
    stubRealtimeGlobals();
    const rt = await load();
    const disconnected = vi.fn();
    rt.onDisconnected(disconnected);
    handshake(rt);
    rt.disconnect();
    await vi.advanceTimersByTimeAsync(60_000);
    expect(FakeSocket.instances).toHaveLength(1);
    expect(disconnected).not.toHaveBeenCalled();
    expect(rt.state).toBe("offline");
  });

  it("没有 access token 不连", async () => {
    const { auth } = stubRealtimeGlobals();
    auth.accessToken = "";
    const rt = await load();
    rt.connect();
    expect(FakeSocket.instances).toHaveLength(0);
  });
});
