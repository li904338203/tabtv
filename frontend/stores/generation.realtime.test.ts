import { computed, reactive, ref } from "vue";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

/**
 * 生成 store 的实时行为钉子（AGT-06）：连接抽到 realtime store 之后，生成状态回推的行为不能变。
 *
 *  - auth.ok 后订 `workspace:{当前工作区}`，帧形状逐字钉住；
 *  - subscribe.ok 里**真的有**这个 topic 才算在线并对账（被拒的照样回 subscribe.ok，CD-73）；
 *  - 只对本页在跟踪的任务对账（别人的、别的项目的、已落定的都丢）；
 *  - 断线 / 重连后对账全部跟踪中的任务；切换工作区退订旧 topic。
 *
 * 用真实的 realtime store + 假 WebSocket；Nuxt 自动导入的全局函数一律注入替身。
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

  open() {
    this.readyState = FakeSocket.OPEN;
    this.onopen?.({});
  }

  receive(frame: unknown) {
    this.onmessage?.({ data: JSON.stringify(frame) });
  }

  framesOf(type: string) {
    return this.sent.filter(f => f.type === type);
  }
}

type GenerationStore = {
  connectionState: string;
  open: (projectId: string) => void;
  close: () => void;
  restore: () => Promise<void>;
};

function setup(currentId = "W1") {
  FakeSocket.instances = [];
  const instances = new Map<string, unknown>();
  const workspace = reactive({ currentId });
  const auth = {
    accessToken: "A1",
    refreshAccessToken: vi.fn(async () => "A2"),
    withTokenRefresh: (send: (token: string) => Promise<unknown>) => send("A1"),
  };
  // 画布上两个运行中的任务：g1 在 node-a、g2 在 node-b
  const canvas = {
    nodes: [
      { id: "node-a", data: { nodeType: "image", generation: { id: "g1", status: "running" } } },
      { id: "node-b", data: { nodeType: "image", generation: { id: "g2", status: "queued" } } },
    ],
    updateGeneration: vi.fn(),
    scheduleSave: vi.fn(),
    // 远端批次挂上任务（link_generation）时的回调：测试里手动触发
    linkedListeners: [] as Array<(item: { nodeId: string; generationId: string }) => void>,
    onGenerationLinked(listener: (item: { nodeId: string; generationId: string }) => void) {
      canvas.linkedListeners.push(listener);
      return () => {
        canvas.linkedListeners = canvas.linkedListeners.filter(item => item !== listener);
      };
    },
  };
  const api = vi.fn(async (url: string) => {
    // 契约形状（`GET /credits` → `{ balance, grants }`）；此前这里按老结构 `{ total, … }` 造，掩盖了读错字段
    if (url === "/credits") return { balance: 10, grants: [] };
    const id = url.split("/")[2];
    return { id, status: "running", progress: 10 };
  });
  vi.stubGlobal("ref", ref);
  vi.stubGlobal("computed", computed);
  vi.stubGlobal("defineStore", (id: string, setupFn: () => Record<string, unknown>) => () => {
    if (!instances.has(id)) instances.set(id, reactive(setupFn()));
    return instances.get(id);
  });
  vi.stubGlobal("useRuntimeConfig", () => ({ public: { apiBase: "/api/v1", wsUrl: "", wsApiPort: "8180" } }));
  vi.stubGlobal("useAuthStore", () => auth);
  vi.stubGlobal("useCanvasStore", () => canvas);
  vi.stubGlobal("useWorkspaceStore", () => workspace);
  vi.stubGlobal("useApi", () => api);
  vi.stubGlobal("WebSocket", FakeSocket);
  vi.stubGlobal("window", { location: { protocol: "http:", hostname: "localhost", host: "localhost:3000" } });
  return { workspace, api, canvas };
}

async function load() {
  const { useRealtimeStore } = await import("./realtime");
  vi.stubGlobal("useRealtimeStore", useRealtimeStore);
  const { useGenerationStore } = await import("./generation");
  return useGenerationStore() as unknown as GenerationStore;
}

/** 打开画布、恢复跟踪、连上并鉴权通过。 */
async function openCanvas(generation: GenerationStore) {
  generation.open("p1");
  await generation.restore();
  const ws = FakeSocket.instances.at(-1)!;
  ws.open();
  ws.receive({ type: "auth.ok", userId: "1", sessionId: "s" });
  return ws;
}

/** 对某个任务发过几次详情请求（轮询 + 对账都算）。 */
function detailCalls(api: ReturnType<typeof setup>["api"], id: string) {
  return api.mock.calls.filter(([url]) => url === `/generations/${id}`).length;
}

const flush = () => vi.advanceTimersByTimeAsync(0);

beforeEach(() => {
  vi.resetModules();
  // 轮询每秒一次：冻住时间，只看推送 / 订阅触发的对账。
  vi.useFakeTimers();
});
afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

describe("生成 store 的实时回推", () => {
  it("订阅当前工作区 topic", async () => {
    setup("W1");
    const generation = await load();
    const ws = await openCanvas(generation);
    expect(ws.sent[0]).toEqual({ type: "auth", token: "A1" });
    expect(ws.framesOf("subscribe")).toEqual([{ type: "subscribe", topics: ["workspace:W1"] }]);
    expect(generation.connectionState).toBe("connecting");
  });

  it("subscribe.ok 含该 topic 才在线，并对账全部跟踪中的任务", async () => {
    const { api } = setup("W1");
    const generation = await load();
    const ws = await openCanvas(generation);
    await flush();
    const before = [detailCalls(api, "g1"), detailCalls(api, "g2")];
    ws.receive({ type: "subscribe.ok", topics: ["workspace:W1"], rejected: [] });
    await flush();
    expect(generation.connectionState).toBe("online");
    expect([detailCalls(api, "g1"), detailCalls(api, "g2")]).toEqual([before[0] + 1, before[1] + 1]);
  });

  it("被拒的 topic 也回 subscribe.ok：不算在线、不对账（退回轮询）", async () => {
    const { api } = setup("W1");
    const generation = await load();
    const ws = await openCanvas(generation);
    await flush();
    const before = detailCalls(api, "g1");
    ws.receive({ type: "subscribe.ok", topics: [], rejected: [{ topic: "workspace:W1", code: 40301 }] });
    await flush();
    expect(generation.connectionState).toBe("offline");
    expect(detailCalls(api, "g1")).toBe(before);
  });

  it("只对跟踪中的任务对账", async () => {
    const { api } = setup("W1");
    const generation = await load();
    const ws = await openCanvas(generation);
    ws.receive({ type: "subscribe.ok", topics: ["workspace:W1"], rejected: [] });
    await flush();
    const g1 = detailCalls(api, "g1");
    const total = api.mock.calls.length;
    // 本页没跟踪的任务（别人 / 别的标签页发起的）、别的项目的事件：一律不发请求
    ws.receive({ type: "generation.update", generationId: "g9", projectId: "p1", status: "success" });
    ws.receive({ type: "generation.update", generationId: "g1", projectId: "p2", status: "success" });
    await flush();
    expect(api.mock.calls.length).toBe(total);
    // 本页在跟踪的任务：去对账（结果以详情接口为准，不读事件里的 status）
    ws.receive({ type: "generation.update", generationId: "g1", projectId: "p1", status: "success" });
    await flush();
    expect(detailCalls(api, "g1")).toBe(g1 + 1);
    expect(api.mock.calls.length).toBe(total + 1);
  });

  it("重连后对账全部跟踪中的任务", async () => {
    const { api } = setup("W1");
    const generation = await load();
    const ws = await openCanvas(generation);
    ws.receive({ type: "subscribe.ok", topics: ["workspace:W1"], rejected: [] });
    await flush();
    const afterOnline = detailCalls(api, "g2");

    ws.close(); // 断线：先对一次账、退回轮询
    await flush();
    expect(generation.connectionState).toBe("offline");
    expect(detailCalls(api, "g2")).toBe(afterOnline + 1);

    await vi.advanceTimersByTimeAsync(1500); // 退避后重连
    const next = FakeSocket.instances[1];
    next.open();
    next.receive({ type: "auth.ok" });
    expect(next.framesOf("subscribe")).toEqual([{ type: "subscribe", topics: ["workspace:W1"] }]);
    const beforeOk = detailCalls(api, "g2");
    next.receive({ type: "subscribe.ok", topics: ["workspace:W1"], rejected: [] });
    await flush();
    expect(generation.connectionState).toBe("online");
    expect(detailCalls(api, "g2")).toBe(beforeOk + 1);
  });

  it("切换工作区时退订旧 topic", async () => {
    const { workspace } = setup("W1");
    const generation = await load();
    const ws = await openCanvas(generation);
    ws.receive({ type: "subscribe.ok", topics: ["workspace:W1"], rejected: [] });
    expect(generation.connectionState).toBe("online");
    workspace.currentId = "W2";
    expect(ws.framesOf("unsubscribe")).toEqual([{ type: "unsubscribe", topics: ["workspace:W1"] }]);
    expect(ws.framesOf("subscribe").at(-1)).toEqual({ type: "subscribe", topics: ["workspace:W2"] });
    expect(generation.connectionState).toBe("connecting");
    ws.receive({ type: "subscribe.ok", topics: ["workspace:W2"], rejected: [] });
    expect(generation.connectionState).toBe("online");
  });

  it("没有工作区就不订（订了也是空的），如实显示离线", async () => {
    setup("");
    const generation = await load();
    const ws = await openCanvas(generation);
    expect(ws.framesOf("subscribe")).toEqual([]);
    expect(generation.connectionState).toBe("offline");
  });

  it("关闭画布：断开连接且不再重连", async () => {
    setup("W1");
    const generation = await load();
    const ws = await openCanvas(generation);
    ws.receive({ type: "subscribe.ok", topics: ["workspace:W1"], rejected: [] });
    generation.close();
    expect(ws.readyState).toBe(FakeSocket.CLOSED);
    expect(generation.connectionState).toBe("offline");
    await vi.advanceTimersByTimeAsync(60_000);
    expect(FakeSocket.instances).toHaveLength(1);
  });

  it("远端批次挂上任务（Agent 的 link_generation）：立刻接上跟踪，推送对账认得它", async () => {
    const { api, canvas } = setup("W1");
    const generation = await load();
    const ws = await openCanvas(generation);
    ws.receive({ type: "subscribe.ok", topics: ["workspace:W1"], rejected: [] });
    await flush();
    // 重放之后节点上已有 result（canvas store 合进了 data.generation），然后通知回调
    canvas.nodes.push({ id: "node-c", data: { nodeType: "video", generation: { id: "g3", status: "queued" } } });
    for (const listener of canvas.linkedListeners) listener({ nodeId: "node-c", generationId: "g3" });
    await flush();
    expect(detailCalls(api, "g3")).toBe(1); // 开始轮询
    // 重复通知不再起第二条轮询
    for (const listener of canvas.linkedListeners) listener({ nodeId: "node-c", generationId: "g3" });
    await flush();
    expect(detailCalls(api, "g3")).toBe(1);
    // 推送只认跟踪中的任务：g3 已在跟踪，去对账
    ws.receive({ type: "generation.update", generationId: "g3", projectId: "p1", status: "success" });
    await flush();
    expect(detailCalls(api, "g3")).toBe(2);
    // 关掉画布后不再响应
    generation.close();
    expect(canvas.linkedListeners).toHaveLength(0);
  });
});
