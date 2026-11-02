import { computed, reactive, ref } from "vue";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

/**
 * 生成 store 的实时行为钉子（AGT-06）：连接抽到 realtime store 之后，生成状态回推的行为不能变。
 *
 *  - 订 `workspace:{当前工作区}`，接上宿主事件通道后对账全部跟踪中的任务；
 *  - 只对本页在跟踪的任务对账（别人的、别的项目的、别的工作区的、已落定的都丢）；
 *  - 切换工作区退订旧 topic。
 *
 * 用真实的 realtime store + 假宿主（Wails 事件通道，TT-T-11）；Nuxt 自动导入的全局函数一律注入替身。
 */
type Listener = (...data: unknown[]) => void;

/** 假宿主：`window.runtime.EventsOn` + 手动推事件（Go 侧经 Wails 事件通道送来的 {topic, payload}）。 */
function installHost() {
  const listeners = new Set<Listener>();
  const win = { runtime: { EventsOn: (_name: string, cb: Listener) => (listeners.add(cb), () => listeners.delete(cb)) } };
  vi.stubGlobal("window", win);
  return {
    emit(topic: string, payload: unknown) {
      for (const cb of [...listeners]) cb({ topic, payload });
    },
    count: () => listeners.size
  };
}

type GenerationStore = {
  connectionState: string;
  open: (projectId: string) => void;
  close: () => void;
  restore: () => Promise<void>;
};

function setup(currentId = "W1") {
  const host = installHost();
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
  return { workspace, api, canvas, host };
}

async function load() {
  const { useRealtimeStore } = await import("./realtime");
  vi.stubGlobal("useRealtimeStore", useRealtimeStore);
  const { useGenerationStore } = await import("./generation");
  return useGenerationStore() as unknown as GenerationStore;
}

/** 打开画布、恢复跟踪（接上宿主事件通道）。 */
async function openCanvas(generation: GenerationStore) {
  generation.open("p1");
  await generation.restore();
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
  it("订阅当前工作区 topic、接上宿主后在线并对账全部跟踪中的任务", async () => {
    const { api } = setup("W1");
    const generation = await load();
    await openCanvas(generation);
    await flush();
    expect(generation.connectionState).toBe("online");
    expect(detailCalls(api, "g1")).toBeGreaterThanOrEqual(1);
    expect(detailCalls(api, "g2")).toBeGreaterThanOrEqual(1);
  });

  it("只对跟踪中的任务、本项目、本工作区的事件对账", async () => {
    const { api, host } = setup("W1");
    const generation = await load();
    await openCanvas(generation);
    await flush();
    const g1 = detailCalls(api, "g1");
    const total = api.mock.calls.length;
    host.emit("workspace:W1", { type: "generation.update", generationId: "g9", projectId: "p1", status: "success" });
    host.emit("workspace:W1", { type: "generation.update", generationId: "g1", projectId: "p2", status: "success" });
    host.emit("workspace:W2", { type: "generation.update", generationId: "g1", projectId: "p1", status: "success" });
    await flush();
    expect(api.mock.calls.length).toBe(total);
    host.emit("workspace:W1", { type: "generation.update", generationId: "g1", projectId: "p1", status: "success" });
    await flush();
    expect(detailCalls(api, "g1")).toBe(g1 + 1);
    expect(api.mock.calls.length).toBe(total + 1);
  });

  it("切换工作区时退订旧 topic、订新 topic", async () => {
    const { workspace, api, host } = setup("W1");
    const generation = await load();
    await openCanvas(generation);
    await flush();
    workspace.currentId = "W2";
    generation.open("p1"); // 工作区切换后页面会重新打开画布（syncTopic）
    await flush();
    const before = detailCalls(api, "g1");
    host.emit("workspace:W1", { type: "generation.update", generationId: "g1", projectId: "p1" });
    await flush();
    expect(detailCalls(api, "g1")).toBe(before);
    host.emit("workspace:W2", { type: "generation.update", generationId: "g1", projectId: "p1" });
    await flush();
    expect(detailCalls(api, "g1")).toBe(before + 1);
  });

  it("没有工作区就不订（订了也是空的），如实显示离线", async () => {
    setup("");
    const generation = await load();
    await openCanvas(generation);
    expect(generation.connectionState).toBe("offline");
  });

  it("关闭画布：摘掉宿主监听", async () => {
    const { host } = setup("W1");
    const generation = await load();
    await openCanvas(generation);
    expect(host.count()).toBe(1);
    generation.close();
    expect(host.count()).toBe(0);
    expect(generation.connectionState).toBe("offline");
  });

  it("远端批次挂上任务（Agent 的 link_generation）：立刻接上跟踪，推送对账认得它", async () => {
    const { api, canvas, host } = setup("W1");
    const generation = await load();
    await openCanvas(generation);
    await flush();
    canvas.nodes.push({ id: "node-c", data: { nodeType: "video", generation: { id: "g3", status: "queued" } } });
    for (const listener of canvas.linkedListeners) listener({ nodeId: "node-c", generationId: "g3" });
    await flush();
    expect(detailCalls(api, "g3")).toBe(1);
    for (const listener of canvas.linkedListeners) listener({ nodeId: "node-c", generationId: "g3" });
    await flush();
    expect(detailCalls(api, "g3")).toBe(1);
    host.emit("workspace:W1", { type: "generation.update", generationId: "g3", projectId: "p1", status: "success" });
    await flush();
    expect(detailCalls(api, "g3")).toBe(2);
    generation.close();
    expect(canvas.linkedListeners).toHaveLength(0);
  });
});
