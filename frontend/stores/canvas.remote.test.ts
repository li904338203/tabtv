import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { ref, toRaw } from "vue";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { draftKey } from "~/utils/canvasConflict";

/**
 * 画布 store 的远端重放（AGT-11）。注入方式同 `canvas.test.ts`（vitest 里没有 Nuxt / Pinia 自动导入）。
 *
 * ⚠️ 这里钉的都是「错了也不报错」的行为：整页重载导致的闪烁、Ctrl+Z 静默删掉 Agent 的节点、
 *    自动重存把用户的改动或 Agent 的改动丢掉一边、用户改过的节点 Agent 还在改。
 */

type Call = { path: string; method: string; body?: unknown };
type Handler = (call: Call) => unknown;
type Json = Record<string, unknown>;

type StoreNode = { id: string; type?: string; position: { x: number; y: number }; data: { title: string; params: Json; agent?: Json; origin?: Json; members?: string[] } };
type Store = {
  version: { value: number };
  nodes: { value: StoreNode[] };
  edges: { value: Array<{ id: string; source: string; target: string }> };
  saveState: { value: string };
  conflict: { value: null | { message?: string; currentEditorName: string } };
  undoStack: { value: Array<{ kind: string; batchId?: string }> };
  redoStack: { value: unknown[] };
  lastRemoteBatch: { value: null | { batchId: string; nodeIds: string[]; actorKind?: string; clientName?: string } };
  highlightedNodeIds: { value: string[] };
  load: (id: string) => Promise<void>;
  save: () => Promise<void>;
  document: () => Json;
  addNode: (type: string, title: string, params?: Json, options?: Json) => string;
  updateParams: (id: string, params: Json) => void;
  undo: () => void;
  applyRemoteOps: (raw: unknown) => void;
  fetchOpsSince: (version?: number) => Promise<string>;
  undoAgentBatch: (batchId: string) => boolean;
  markTouched: (nodeId: string) => boolean;
};

const json = <T>(value: T): T => JSON.parse(JSON.stringify(value)) as T;

function conflictError(details: Json) {
  return Object.assign(new Error("版本冲突"), { statusCode: 409, data: { code: 40902, details } });
}

const baseDocument = {
  schemaVersion: 1,
  viewport: { x: 0, y: 0, zoom: 1 },
  nodes: [
    { id: "n_1", type: "text", title: "用户的词", position: { x: 0, y: 0 }, size: { w: 330, h: 180 }, params: { prompt: "猫" } },
    { id: "a_1", type: "image", title: "Agent 的图", position: { x: 400, y: 0 }, size: { w: 330, h: 180 }, params: { prompt: "旧" }, agent: { sessionId: "7001" } }
  ],
  edges: []
};

const project = (canvasData: unknown, version = 3) => ({ id: "p1", name: "项目", version, canvasData: json(canvasData) });

/** 事件形状（api/events/canvas-ops-applied.schema.json）。 */
function event(fromVersion: number, ops: unknown[], extra: Json = {}) {
  return {
    type: "canvas.ops_applied",
    occurredAt: "2026-10-15T00:00:00Z",
    workspaceId: "1",
    projectId: "p1",
    batchId: `b${fromVersion}`,
    fromVersion,
    toVersion: fromVersion + 1,
    actor: { kind: "agent", userId: "1001", agentSessionId: "7001" },
    truncated: false,
    ops,
    ...extra
  };
}

const addNodeOp = (id: string, x = 800) => ({
  kind: "add_node",
  node: { id, type: "text", title: `新 ${id}`, params: {}, position: { x, y: 0 }, size: { w: 330, h: 180 }, agent: { sessionId: "7001" } }
});

function setup(handler: Handler) {
  const calls: Call[] = [];
  const session = new Map<string, string>();
  vi.stubGlobal("ref", ref);
  vi.stubGlobal("toRaw", toRaw);
  vi.stubGlobal("defineStore", (_id: string, setupFn: () => unknown) => () => setupFn());
  vi.stubGlobal("useApi", () => async (path: string, options: { method?: string; body?: unknown } = {}) => {
    const call = { path, method: options.method ?? "GET", body: options.body };
    calls.push(call);
    return handler(call);
  });
  vi.stubGlobal("sessionStorage", {
    getItem: (k: string) => session.get(k) ?? null,
    setItem: (k: string, v: string) => { session.set(k, v); },
    removeItem: (k: string) => { session.delete(k); }
  });
  return { calls, session };
}

async function loadedStore(handler: Handler, document: unknown = baseDocument) {
  const env = setup((call) => {
    if (call.method === "GET" && call.path === "/projects/p1") return project(document);
    return handler(call);
  });
  const { useCanvasStore } = await import("./canvas");
  const store = useCanvasStore() as unknown as Store;
  await store.load("p1");
  return { store, ...env };
}

const puts = (calls: Call[]) => calls.filter(call => call.method === "PUT");
const putDoc = (call: Call | undefined) => json((call?.body as { canvasData: Json }).canvasData) as { nodes: Json[] };
const projectReloads = (calls: Call[]) => calls.filter(call => call.method === "GET" && call.path === "/projects/p1").length;

beforeEach(() => {
  vi.resetModules();
  vi.useFakeTimers();
});
afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

describe("远端批次重放", () => {
  it("连续版本本地重放不重载且未变节点引用不变", async () => {
    const { store, calls } = await loadedStore(() => { throw new Error("不该发请求"); });
    const user = toRaw(store.nodes.value[0]!);
    const agentNode = toRaw(store.nodes.value[1]!);
    const nodesArray = toRaw(store.nodes.value);

    store.applyRemoteOps(event(3, [addNodeOp("a_2"), { kind: "update_node", nodeId: "a_1", patch: { title: "新标题", params: { prompt: "新" } } }]));
    store.applyRemoteOps(event(4, [{ kind: "move_node", nodeId: "a_2", position: { x: 900, y: 50 } }]));

    expect(store.version.value).toBe(5);
    expect(projectReloads(calls)).toBe(1); // 只有最初那次载入
    expect(calls).toHaveLength(1);
    // 不闪：数组与未变节点都是原来那个对象；被改的节点原地改字段
    expect(toRaw(store.nodes.value)).toBe(nodesArray);
    expect(toRaw(store.nodes.value[0]!)).toBe(user);
    expect(toRaw(store.nodes.value[1]!)).toBe(agentNode);
    expect(store.nodes.value[1]!.data.title).toBe("新标题");
    expect(store.nodes.value.map(node => node.id)).toEqual(["n_1", "a_1", "a_2"]);
    expect(store.nodes.value[2]!.position).toEqual({ x: 900, y: 50 });
    // 重放不保存：本地已经就是服务端那一版
    await vi.advanceTimersByTimeAsync(2000);
    expect(puts(calls)).toHaveLength(0);
    expect(store.lastRemoteBatch.value?.batchId).toBe("b4");
    expect(store.highlightedNodeIds.value).toEqual([]); // 1 秒高亮已过
  });

  it("重复或过期批次被丢弃", async () => {
    const { store, calls } = await loadedStore(() => { throw new Error("不该发请求"); });
    const batch = event(3, [addNodeOp("a_2")]);
    store.applyRemoteOps(batch);
    store.applyRemoteOps(batch);
    store.applyRemoteOps(event(2, [addNodeOp("a_old")]));
    store.applyRemoteOps({ ...event(4, [addNodeOp("a_x")]), projectId: "p_other" });
    store.applyRemoteOps({ type: "canvas.ops_applied", fromVersion: "坏帧" });
    expect(store.version.value).toBe(4);
    expect(store.nodes.value.map(node => node.id)).toEqual(["n_1", "a_1", "a_2"]);
    expect(calls).toHaveLength(1);
  });

  it("版本不连续时补取", async () => {
    const { store, calls } = await loadedStore((call) => {
      if (call.path.startsWith("/projects/p1/canvas/ops")) {
        return {
          currentVersion: 5,
          needsReload: false,
          batches: [
            { ...event(3, [addNodeOp("a_2")]), projectId: undefined, createdAt: "x" },
            { ...event(4, [addNodeOp("a_3", 1200)]), projectId: undefined, createdAt: "x" }
          ]
        };
      }
      throw new Error(`意外请求 ${call.path}`);
    });
    store.applyRemoteOps(event(4, [addNodeOp("a_3", 1200)])); // 漏了 3→4
    await vi.runAllTimersAsync();
    expect(calls.some(call => call.path === "/projects/p1/canvas/ops?sinceVersion=3")).toBe(true);
    expect(store.version.value).toBe(5);
    expect(store.nodes.value.map(node => node.id)).toEqual(["n_1", "a_1", "a_2", "a_3"]);
    expect(store.saveState.value).toBe("saved");
  });

  it("事件被截断时改走补取", async () => {
    const { store, calls } = await loadedStore(() => ({ currentVersion: 4, needsReload: false, batches: [event(3, [addNodeOp("a_2")])] }));
    store.applyRemoteOps({ ...event(3, []), ops: undefined, truncated: true });
    await vi.runAllTimersAsync();
    expect(calls.filter(call => call.path.includes("/canvas/ops"))).toHaveLength(1);
    expect(store.version.value).toBe(4);
    expect(store.nodes.value).toHaveLength(3);
  });

  it("needsReload 进入冲突态并存草稿", async () => {
    const { store, calls, session } = await loadedStore(() => ({ currentVersion: 6, needsReload: true, batches: [] }));
    store.updateParams("n_1", { prompt: "我刚改的" });
    const result = await store.fetchOpsSince(store.version.value);
    expect(result).toBe("reload");
    expect(store.saveState.value).toBe("conflict");
    expect(store.conflict.value?.message).toBe("画布有其他人的改动，需要载入最新版本");
    // 不自动重载；本地改动原样在画布上，并已存成草稿
    expect(projectReloads(calls)).toBe(1);
    expect(store.nodes.value[0]!.data.params).toEqual({ prompt: "我刚改的" });
    const draft = JSON.parse(session.get(draftKey("p1")) ?? "{}");
    expect(draft.document.nodes[0].params).toEqual({ prompt: "我刚改的" });
    await vi.advanceTimersByTimeAsync(2000);
    expect(puts(calls)).toHaveLength(0);
  });

  it("未保存改动与远端批次并存后基于新版本保存", async () => {
    const { store, calls } = await loadedStore((call) => {
      if (call.method === "PUT") return { version: 5 };
      throw new Error(`意外请求 ${call.path}`);
    });
    store.updateParams("n_1", { prompt: "两只猫" }); // 排队中的自动保存
    store.applyRemoteOps(event(3, [addNodeOp("a_2")]));
    await vi.advanceTimersByTimeAsync(1000);
    const sent = puts(calls);
    expect(sent).toHaveLength(1);
    expect((sent[0]!.body as { version: number }).version).toBe(4);
    const doc = putDoc(sent[0]);
    expect(doc.nodes.map(node => node.id)).toEqual(["n_1", "a_1", "a_2"]);
    expect(doc.nodes[0]!.params).toEqual({ prompt: "两只猫" });
    expect(store.version.value).toBe(5);
  });

  it("保存在途时到达的批次等保存回来再处理", async () => {
    let release: (value: unknown) => void = () => {};
    const { store, calls } = await loadedStore((call) => {
      if (call.method === "PUT") return new Promise((resolve) => { release = resolve; });
      throw new Error(`意外请求 ${call.path}`);
    });
    store.updateParams("n_1", { prompt: "两只猫" });
    await vi.advanceTimersByTimeAsync(800); // PUT 发出、未返回
    store.applyRemoteOps(event(4, [addNodeOp("a_2")])); // 建立在我这次保存（3→4）之上
    expect(store.nodes.value).toHaveLength(2);
    release({ version: 4 });
    await vi.runAllTimersAsync();
    // 没有误判成「不连续」去补取，也没有进冲突态
    expect(calls.some(call => call.path.includes("/canvas/ops"))).toBe(false);
    expect(store.version.value).toBe(5);
    expect(store.nodes.value.map(node => node.id)).toEqual(["n_1", "a_1", "a_2"]);
    expect(store.saveState.value).toBe("saved");
  });
});

describe("保存冲突的自动处理", () => {
  it("冲突且只有 Agent 改动时自动重存", async () => {
    let attempt = 0;
    const { store, calls } = await loadedStore((call) => {
      if (call.method === "PUT") {
        attempt += 1;
        if (attempt === 1) throw conflictError({ currentVersion: 4, editorKind: "agent", agentSessionId: "7001", bySelf: false });
        return { version: 5 };
      }
      if (call.path === "/projects/p1/canvas/ops?sinceVersion=3") {
        return { currentVersion: 4, needsReload: false, batches: [event(3, [addNodeOp("a_2"), { kind: "update_node", nodeId: "a_1", patch: { title: "Agent 改的" } }])] };
      }
      throw new Error(`意外请求 ${call.path}`);
    });
    store.updateParams("n_1", { prompt: "用户改的" });
    await vi.runAllTimersAsync();
    const sent = puts(calls);
    expect(sent).toHaveLength(2);
    expect((sent[1]!.body as { version: number }).version).toBe(4);
    const doc = putDoc(sent[1]);
    // 两边的改动都在
    expect(doc.nodes.map(node => node.id)).toEqual(["n_1", "a_1", "a_2"]);
    expect(doc.nodes[0]!.params).toEqual({ prompt: "用户改的" });
    expect(doc.nodes[1]!.title).toBe("Agent 改的");
    expect(store.saveState.value).toBe("saved");
    expect(store.conflict.value).toBeNull();
    expect(store.version.value).toBe(5);
  });

  it("冲突中有真人改动时进入冲突态", async () => {
    const { store, calls } = await loadedStore((call) => {
      if (call.method === "PUT") throw conflictError({ currentVersion: 4, currentEditorName: "林澈", editorKind: "user" });
      throw new Error(`意外请求 ${call.path}`);
    });
    store.updateParams("n_1", { prompt: "用户改的" });
    await vi.runAllTimersAsync();
    expect(store.saveState.value).toBe("conflict");
    expect(store.conflict.value?.currentEditorName).toBe("林澈");
    expect(calls.some(call => call.path.includes("/canvas/ops"))).toBe(false);
    expect(puts(calls)).toHaveLength(1);
  });

  it("Agent 冲突但补取发现中间夹着真人保存 → 冲突态", async () => {
    const { store, calls } = await loadedStore((call) => {
      if (call.method === "PUT") throw conflictError({ currentVersion: 5, editorKind: "agent" });
      return { currentVersion: 5, needsReload: true, batches: [] };
    });
    store.updateParams("n_1", { prompt: "用户改的" });
    await vi.runAllTimersAsync();
    expect(store.saveState.value).toBe("conflict");
    expect(store.conflict.value?.message).toBe("画布有其他人的改动，需要载入最新版本");
    expect(puts(calls)).toHaveLength(1);
  });

  it("Agent 冲突反复出现时最多自动重存 3 次", async () => {
    let version = 3;
    const { store, calls } = await loadedStore((call) => {
      if (call.method === "PUT") throw conflictError({ currentVersion: version + 1, editorKind: "agent" });
      const since = Number(call.path.split("=")[1]);
      version = since + 1;
      return { currentVersion: version, needsReload: false, batches: [event(since, [addNodeOp(`a_${since}`)])] };
    });
    store.updateParams("n_1", { prompt: "用户改的" });
    await vi.runAllTimersAsync();
    expect(puts(calls)).toHaveLength(4);
    expect(store.saveState.value).toBe("conflict");
  });
});

describe("撤销与用户改动", () => {
  it("Agent 批次撤销为一步且清掉之前的普通快照", async () => {
    const { store, calls } = await loadedStore((call) => {
      if (call.method === "PUT") return { version: 5 };
      throw new Error(`意外请求 ${call.path}`);
    });
    store.addNode("text", "我的", {}, { exactTitle: true, persist: false }); // 一条普通快照
    expect(store.undoStack.value.map(entry => entry.kind)).toEqual(["snapshot"]);
    store.applyRemoteOps(event(3, [addNodeOp("a_2"), addNodeOp("a_3", 1200), { kind: "add_edge", edge: { id: "e_a", source: "n_1", target: "a_2", sourceHandle: "out", targetHandle: "in", order: 1 } }]));
    // 之前那条快照回到的是 Agent 写入之前的世界：作废
    expect(store.undoStack.value.map(entry => entry.kind)).toEqual(["remote"]);
    expect(store.undoAgentBatch("别的批次")).toBe(false);

    expect(store.undoAgentBatch("b3")).toBe(true);
    expect(store.nodes.value.map(node => node.id)).not.toContain("a_2");
    expect(store.nodes.value.map(node => node.id)).not.toContain("a_3");
    expect(store.edges.value).toHaveLength(0);
    expect(store.nodes.value.some(node => node.data.title === "我的")).toBe(true); // 用户自己的节点还在
    expect(store.undoStack.value).toHaveLength(0);
    store.undo(); // 再按一次：没有东西可撤，不会回到更早的世界
    expect(store.nodes.value.some(node => node.data.title === "我的")).toBe(true);

    await vi.runAllTimersAsync();
    const doc = putDoc(puts(calls)[0]);
    expect((puts(calls)[0]!.body as { version: number }).version).toBe(4);
    expect(doc.nodes.map(node => node.id)).not.toContain("a_2");
  });

  it("批次之后还有普通改动时，提示条上的撤销不可用；Ctrl+Z 逐步撤回", async () => {
    const { store } = await loadedStore(() => ({ version: 5 }));
    store.applyRemoteOps(event(3, [addNodeOp("a_2")]));
    store.addNode("text", "之后加的", {}, { exactTitle: true, persist: false });
    expect(store.undoAgentBatch("b3")).toBe(false);
    store.undo();
    expect(store.nodes.value.some(node => node.data.title === "之后加的")).toBe(false);
    expect(store.nodes.value.some(node => node.id === "a_2")).toBe(true);
    store.undo();
    expect(store.nodes.value.some(node => node.id === "a_2")).toBe(false);
  });

  it("markTouched 只作用于 Agent 节点", async () => {
    const { store, calls } = await loadedStore((call) => {
      if (call.method === "PUT") return { version: 4 };
      throw new Error(`意外请求 ${call.path}`);
    });
    expect(store.markTouched("n_1")).toBe(false);
    expect(store.nodes.value[0]!.data.agent).toBeUndefined();
    expect(store.markTouched("a_1")).toBe(true);
    expect(store.markTouched("a_1")).toBe(false);
    expect(store.nodes.value[1]!.data.agent).toEqual({ sessionId: "7001", userTouched: true });
    store.updateParams("a_1", { prompt: "我改的" });
    // 用户改过之后到达的 Agent 修改（服务端还不知道 userTouched）：本地以用户为准
    store.applyRemoteOps(event(3, [{ kind: "update_node", nodeId: "a_1", patch: { params: { prompt: "Agent 想改" } } }]));
    expect(store.nodes.value[1]!.data.params).toEqual({ prompt: "我改的" });
    await vi.runAllTimersAsync();
    const doc = putDoc(puts(calls).at(-1));
    expect(doc.nodes[1]!.agent).toEqual({ sessionId: "7001", userTouched: true });
    expect(doc.nodes[1]!.params).toEqual({ prompt: "我改的" });
  });
});

describe("画框", () => {
  const frameDocument = {
    schemaVersion: 1,
    viewport: { x: 0, y: 0, zoom: 1 },
    nodes: [
      ...baseDocument.nodes,
      { id: "a_f", type: "frame", title: "第一场", position: { x: -20, y: -40 }, size: { w: 1200, h: 400 }, members: ["n_1", "a_1"], agent: { sessionId: "7001" } }
    ],
    edges: []
  };

  it("画框节点加载与保存往返", async () => {
    const { store } = await loadedStore(() => ({}), frameDocument);
    const frame = store.nodes.value[2]!;
    expect(frame.type).toBe("frame");
    expect(frame.data.members).toEqual(["n_1", "a_1"]);
    expect(json(store.document())).toEqual(frameDocument);
  });

  it("远端新增画框以 frame 类型出现", async () => {
    const { store } = await loadedStore(() => ({}));
    store.applyRemoteOps(event(3, [{ kind: "add_frame", frame: { id: "a_f2", type: "frame", title: "框", position: { x: 0, y: 0 }, size: { w: 640, h: 400 }, members: ["n_1"], agent: { sessionId: "7001" } } }]));
    const frame = store.nodes.value.find(node => node.id === "a_f2");
    expect(frame?.type).toBe("frame");
    expect((json(store.document()) as { nodes: Json[] }).nodes[2]).toEqual({ id: "a_f2", type: "frame", title: "框", position: { x: 0, y: 0 }, size: { w: 640, h: 400 }, members: ["n_1"], agent: { sessionId: "7001" } });
  });
});

describe("addNode 落库形状（api/fixtures/canvas-ops/addnode-shape.json）", () => {
  it("与 Agent 建出来的节点同形", async () => {
    const path = fileURLToPath(new URL("../../api/fixtures/canvas-ops/addnode-shape.json", import.meta.url));
    const shape = JSON.parse(readFileSync(path, "utf8")) as { cases: Array<{ addNode: { nodeType: string; title: string; params: Json }; node: Json }> };
    const { store } = await loadedStore(() => ({}), { schemaVersion: 1, viewport: { x: 0, y: 0, zoom: 1 }, nodes: [], edges: [] });
    for (const item of shape.cases) {
      const id = store.addNode(item.addNode.nodeType, item.addNode.title, item.addNode.params, { exactTitle: true, persist: false });
      const stored = (json(store.document()) as { nodes: Json[] }).nodes.find(node => node.id === id)!;
      const { id: _a, position: _b, ...actual } = stored;
      const { id: _c, position: _d, ...expected } = item.node;
      expect(actual, item.addNode.nodeType).toEqual(expected);
    }
  });
});

describe("MCP 来源（MCPT-09）", () => {
  const mcpOrigin = { kind: "mcp", grantId: "5001", clientName: "Claude Code" };
  const mcpEvent = (fromVersion: number, ops: unknown[], actor: Json = {}) => event(fromVersion, ops, {
    actor: { kind: "mcp", userId: "1001", agentSessionId: null, mcpGrantId: "5001", clientName: "Claude Code", ...actor }
  });
  const mcpAddNode = (id: string) => ({
    kind: "add_node",
    node: { id, type: "image", title: `外部 ${id}`, params: {}, position: { x: 800, y: 0 }, size: { w: 330, h: 180 }, origin: mcpOrigin }
  });

  it("MCP批次本地重放显示客户端名", async () => {
    const { store, calls } = await loadedStore(() => { throw new Error("不该发请求"); });
    store.applyRemoteOps(mcpEvent(3, [mcpAddNode("a_m1"), mcpAddNode("a_m2")]));
    expect(store.version.value).toBe(4);
    expect(store.nodes.value.map(node => node.id)).toEqual(["n_1", "a_1", "a_m1", "a_m2"]);
    expect(store.nodes.value[2]!.data.origin).toEqual(mcpOrigin);
    expect(store.nodes.value[2]!.data.agent).toBeUndefined();
    expect(store.lastRemoteBatch.value).toMatchObject({ actorKind: "mcp", clientName: "Claude Code", nodeIds: ["a_m1", "a_m2"] });
    // 补取接口不带客户端名：回落到节点上的 origin.clientName
    store.applyRemoteOps(mcpEvent(4, [{ kind: "update_node", nodeId: "a_m1", patch: { title: "改名" } }], { clientName: null }));
    expect(store.lastRemoteBatch.value).toMatchObject({ actorKind: "mcp", clientName: "Claude Code" });
    // 重放不保存；往返不丢 origin
    await vi.advanceTimersByTimeAsync(2000);
    expect(puts(calls)).toHaveLength(0);
    const doc = json(store.document()) as { nodes: Json[] };
    expect(doc.nodes[2]!.origin).toEqual(mcpOrigin);
  });

  it("冲突且editorKind为mcp时自动重存", async () => {
    let attempt = 0;
    const { store, calls } = await loadedStore((call) => {
      if (call.method === "PUT") {
        attempt += 1;
        // MCP 写入时最后编辑者是本人：服务端给 bySelf=false、editorKind=mcp
        if (attempt === 1) throw conflictError({ currentVersion: 4, currentEditorName: "林澈", editorKind: "mcp", mcpGrantId: "5001", bySelf: false });
        return { version: 5 };
      }
      if (call.path === "/projects/p1/canvas/ops?sinceVersion=3") {
        return { currentVersion: 4, needsReload: false, batches: [{ ...mcpEvent(3, [mcpAddNode("a_m1")], { clientName: null }), projectId: undefined }] };
      }
      throw new Error(`意外请求 ${call.path}`);
    });
    store.updateParams("n_1", { prompt: "用户改的" });
    await vi.runAllTimersAsync();
    const sent = puts(calls);
    expect(sent).toHaveLength(2);
    expect((sent[1]!.body as { version: number }).version).toBe(4);
    const doc = putDoc(sent[1]);
    // 不出冲突条；两边的改动都在，MCP 的节点没被覆盖掉
    expect(store.saveState.value).toBe("saved");
    expect(store.conflict.value).toBeNull();
    expect(doc.nodes.map(node => node.id)).toEqual(["n_1", "a_1", "a_m1"]);
    expect(doc.nodes[0]!.params).toEqual({ prompt: "用户改的" });
    expect(doc.nodes[2]!.origin).toEqual(mcpOrigin);
  });

  it("markTouched 对 MCP 节点写 origin.userTouched，宽松重放跳过它", async () => {
    const mcpDocument = {
      ...baseDocument,
      nodes: [...baseDocument.nodes, { id: "a_m1", type: "image", title: "外部的图", position: { x: 800, y: 0 }, size: { w: 330, h: 180 }, params: { prompt: "旧" }, origin: { ...mcpOrigin, future: 1 } }]
    };
    const { store, calls } = await loadedStore((call) => {
      if (call.method === "PUT") return { version: 5 };
      throw new Error(`意外请求 ${call.path}`);
    }, mcpDocument);
    expect(store.markTouched("a_m1")).toBe(true);
    expect(store.markTouched("a_m1")).toBe(false);
    expect(store.nodes.value[2]!.data.origin).toEqual({ ...mcpOrigin, future: 1, userTouched: true });
    store.updateParams("a_m1", { prompt: "我改的" });
    store.applyRemoteOps(mcpEvent(3, [{ kind: "update_node", nodeId: "a_m1", patch: { params: { prompt: "MCP 想改" } } }]));
    expect(store.nodes.value[2]!.data.params).toEqual({ prompt: "我改的" });
    await vi.runAllTimersAsync();
    const doc = putDoc(puts(calls).at(-1));
    expect(doc.nodes[2]!.origin).toEqual({ ...mcpOrigin, future: 1, userTouched: true });
    expect(doc.nodes[2]!.agent).toBeUndefined();
  });
});
