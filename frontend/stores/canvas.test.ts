import { ref, toRaw } from "vue";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { draftKey } from "~/utils/canvasConflict";

/**
 * 画布 store（AGT-07）。vitest 里没有 Nuxt / Pinia 的自动导入，按 store 用到的全局函数注入替身
 * （同 stores/workspace.test.ts）。
 *
 * ⚠️ 往返测试是 AGT-11 的前提：漏掉一处未知字段，Agent 写入的 `agent` 元数据就会被
 *    用户的下一次自动保存抹掉，而这不报任何错。
 */

type Call = { path: string; method: string; body?: unknown };
type Handler = (call: Call) => unknown;

const legacyDocument = {
  schemaVersion: 1,
  viewport: { x: 10, y: 20, zoom: 0.8 },
  nodes: [
    {
      id: "n_1",
      type: "text",
      title: "文本节点 1",
      position: { x: 0, y: 0 },
      size: { w: 330, h: 180 },
      params: { prompt: "一只猫", model: "gpt" },
      result: { id: "g1", status: "success", outputType: "text" },
      foo: 1
    },
    {
      id: "a_s1_1",
      type: "video",
      title: "视频",
      position: { x: 500, y: 0 },
      size: { w: 390, h: 180 },
      params: { mode: "img2video" },
      agent: { sessionId: "s1", stepId: "st1", sourceDocId: "d1", userTouched: false }
    },
    {
      id: "n_3",
      type: "image",
      title: "图片",
      position: { x: 0, y: 300 },
      size: { w: 330, h: 180 },
      params: {}
    }
  ],
  edges: [
    { id: "e1", source: "n_3", sourceHandle: "out", target: "a_s1_1", targetHandle: "in", order: 1, operation: "img2video", bar: 2 }
  ],
  baz: 3,
  agentRuns: [{ id: "r1" }]
};

function conflictError(details: Record<string, unknown>) {
  return Object.assign(new Error("版本冲突"), { statusCode: 409, data: { code: 40902, details } });
}

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

type Store = {
  version: { value: number };
  nodes: { value: Array<{ id: string; data: { params: Record<string, unknown>; title: string } }> };
  edges: { value: Array<{ id: string; target: string; data?: { order: number; operation?: string } }> };
  selectedNodeIds: { value: string[] };
  saveState: { value: string };
  conflict: { value: null | { bySelf: boolean; currentEditorName: string } };
  draftAvailable: { value: boolean };
  undoStack: { value: unknown[] };
  load: (id: string) => Promise<void>;
  save: () => Promise<void>;
  document: () => unknown;
  addNode: (type: string, title: string, params?: Record<string, unknown>, options?: Record<string, unknown>) => string;
  newNodeId: () => string;
  duplicateNode: (id: string) => string | undefined;
  addEdge: (source: string, target: string, options?: Record<string, unknown>) => string;
  removeNode: (id: string) => void;
  removeEdge: (id: string) => void;
  updateParams: (id: string, params: Record<string, unknown>) => void;
  undo: () => void;
  resolveConflict: (mode: "adopt" | "overwrite") => Promise<string>;
  restoreDraft: () => Promise<boolean>;
  discardDraft: () => void;
};

async function loadedStore(handler: Handler) {
  const env = setup(handler);
  const { useCanvasStore } = await import("./canvas");
  const store = useCanvasStore() as unknown as Store;
  await store.load("p1");
  return { store, ...env };
}

const project = (canvasData: unknown, version = 3) => ({ id: "p1", name: "项目", version, canvasData: JSON.parse(JSON.stringify(canvasData)) });
const json = (value: unknown) => JSON.parse(JSON.stringify(value));

beforeEach(() => {
  vi.resetModules();
  vi.useFakeTimers();
});
afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

describe("未知字段往返", () => {
  it("加载再保存保留节点未知字段与 agent", async () => {
    const { store, calls } = await loadedStore((call) => {
      if (call.method === "PUT") return { version: 4 };
      return project(legacyDocument);
    });
    // 文档逐字一致（含 foo / agent / 连线 bar / 顶层 baz、agentRuns）
    expect(json(store.document())).toEqual(legacyDocument);
    await store.save();
    const put = calls.find(call => call.method === "PUT");
    expect(json((put?.body as { canvasData: unknown }).canvasData)).toEqual(legacyDocument);
    expect(store.version.value).toBe(4);
  });

  it("保留连线与文档顶层未知字段（改动之后也保留）", async () => {
    const { store } = await loadedStore(() => project(legacyDocument));
    store.updateParams("n_1", { prompt: "两只猫" });
    const doc = json(store.document());
    expect(doc.baz).toBe(3);
    expect(doc.agentRuns).toEqual([{ id: "r1" }]);
    expect(doc.edges[0].bar).toBe(2);
    expect(doc.nodes[0].foo).toBe(1);
    expect(doc.nodes[0].params).toEqual({ prompt: "两只猫" });
    expect(doc.nodes[1].agent.sessionId).toBe("s1");
  });
});

describe("连线与多选", () => {
  it("addEdge 默认 order 接在最后且记一步撤销", async () => {
    const { store } = await loadedStore(() => project(legacyDocument));
    const before = store.undoStack.value.length;
    const id = store.addEdge("n_1", "a_s1_1", { operation: "ref2video" });
    const edge = store.edges.value.find(item => item.id === id);
    expect(edge?.data).toEqual({ order: 2, operation: "ref2video" });
    expect(store.undoStack.value.length).toBe(before + 1);
    // 同一毫秒再连一条：ID 不撞
    const second = store.addEdge("n_1", "a_s1_1", { recordHistory: false, persist: false });
    expect(second).not.toBe(id);
    expect(store.edges.value.find(item => item.id === second)?.data?.order).toBe(3);
    store.undo();
    expect(store.edges.value.some(item => item.id === id)).toBe(false);
  });

  it("removeEdge 删掉连线、剩余入边 order 重排成 1..N，且可撤销", async () => {
    const { store } = await loadedStore(() => project(legacyDocument));
    const first = store.edges.value.find(item => item.target === "a_s1_1")?.id as string;
    const second = store.addEdge("n_1", "a_s1_1", { recordHistory: false, persist: false });
    expect(store.edges.value.find(item => item.id === second)?.data?.order).toBe(2);
    const before = store.undoStack.value.length;
    store.removeEdge(first);
    expect(store.edges.value.some(item => item.id === first)).toBe(false);
    // 不留空号：首尾帧 / 参考图的「图N」靠 order 连续
    expect(store.edges.value.find(item => item.id === second)?.data?.order).toBe(1);
    expect(store.undoStack.value.length).toBe(before + 1);
    store.undo();
    expect(store.edges.value.some(item => item.id === first)).toBe(true);
    // 不存在的连线：什么都不做，也不记撤销
    const count = store.undoStack.value.length;
    store.removeEdge("nope");
    expect(store.undoStack.value.length).toBe(count);
  });

  it("removeNode 同步剔除 selectedNodeIds", async () => {
    const { store } = await loadedStore(() => project(legacyDocument));
    store.selectedNodeIds.value = ["n_1", "n_3"];
    store.removeNode("n_3");
    expect(store.selectedNodeIds.value).toEqual(["n_1"]);
  });
});

describe("节点 ID（DRT-16）", () => {
  it("canvas store：addNode 接受指定 id", async () => {
    const { store } = await loadedStore(() => project(legacyDocument));
    const id = store.newNodeId();
    // newNodeId 只是取号，不占位：节点还没加进来
    expect(store.nodes.value.some(node => node.id === id)).toBe(false);
    expect(store.addNode("director", "3D 导演台", { sceneId: "77" }, { id })).toBe(id);
    expect(store.nodes.value.find(node => node.id === id)?.data.params).toEqual({ sceneId: "77" });
    // 已被占用的 ID 拒绝，不悄悄换一个（调用方已经拿它建了场景）
    expect(() => store.addNode("director", "3D 导演台", {}, { id })).toThrow(/已存在/);
    expect(store.nodes.value.filter(node => node.id === id)).toHaveLength(1);
  });

  it("canvas store：duplicateNode 返回新 id", async () => {
    const { store } = await loadedStore(() => project(legacyDocument));
    const first = store.duplicateNode("n_1");
    // 同一毫秒里（假时钟下 Date.now 不动）删一个再复制：节点数回到原值，按「时间 + 序号」取号会撞上，撞了补序号
    store.removeNode("n_3");
    const second = store.duplicateNode("n_1");
    expect(first).toBeTruthy();
    expect(second).toBe(`${first}_2`);
    const copy = store.nodes.value.find(node => node.id === first);
    expect(copy?.data.title).toBe("文本节点 1 副本");
    expect(copy?.data.params).toEqual({ prompt: "一只猫", model: "gpt" });
    expect(store.duplicateNode("不存在")).toBeUndefined();
  });
});

describe("保存冲突", () => {
  it("保存冲突进冲突态并写草稿", async () => {
    const { store, session } = await loadedStore((call) => {
      if (call.method === "PUT") throw conflictError({ currentVersion: 5, currentEditorName: "林澈", bySelf: false });
      return project(legacyDocument);
    });
    store.addNode("image", "图片");
    await store.save();
    expect(store.saveState.value).toBe("conflict");
    expect(store.conflict.value?.currentEditorName).toBe("林澈");
    const draft = JSON.parse(session.get(draftKey("p1")) ?? "null");
    expect(draft.document.nodes).toHaveLength(4);
    expect(draft.document.baz).toBe(3);
    // 本地内容原样留着
    expect(store.nodes.value).toHaveLength(4);
  });

  it("以我为准用 canvas/state 的版本重存", async () => {
    let puts = 0;
    const { store, calls, session } = await loadedStore((call) => {
      if (call.path.endsWith("/canvas/state")) return { version: 9, bySelf: false, lastEditorId: "2" };
      if (call.method === "PUT") {
        puts += 1;
        if (puts === 1) throw conflictError({ currentVersion: 9, currentEditorName: "林澈" });
        return { version: 10 };
      }
      return project(legacyDocument);
    });
    store.addNode("image", "图片");
    await store.save();
    expect(store.saveState.value).toBe("conflict");
    expect(await store.resolveConflict("overwrite")).toBe("resolved");
    const last = calls.filter(call => call.method === "PUT").at(-1)!;
    expect((last.body as { version: number }).version).toBe(9);
    expect(json((last.body as { canvasData: { nodes: unknown[] } }).canvasData).nodes).toHaveLength(4);
    expect(store.version.value).toBe(10);
    expect(store.saveState.value).toBe("saved");
    expect(store.conflict.value).toBeNull();
    expect(session.has(draftKey("p1"))).toBe(false);
  });

  it("以我为准再次冲突回到冲突态不循环", async () => {
    let puts = 0;
    const { store } = await loadedStore((call) => {
      if (call.path.endsWith("/canvas/state")) return { version: 9 };
      if (call.method === "PUT") {
        puts += 1;
        throw conflictError({ currentVersion: 9 + puts, currentEditorName: puts === 1 ? "林澈" : "周岚" });
      }
      return project(legacyDocument);
    });
    await store.save();
    expect(await store.resolveConflict("overwrite")).toBe("conflict");
    expect(puts).toBe(2);
    expect(store.saveState.value).toBe("conflict");
    expect(store.conflict.value?.currentEditorName).toBe("周岚");
    // 冲突态下的自动保存不再发请求（不会自己循环重试）
    store.updateParams("n_1", { prompt: "改" });
    await vi.runAllTimersAsync();
    expect(puts).toBe(2);
  });

  it("采纳对方版本后可恢复草稿", async () => {
    let remote = project(legacyDocument, 3);
    let failNextPut = true;
    const { store, calls, session } = await loadedStore((call) => {
      if (call.method === "PUT") {
        if (failNextPut) {
          failNextPut = false;
          throw conflictError({ currentVersion: 5, currentEditorName: "林澈" });
        }
        return { version: 6 };
      }
      return remote;
    });
    const mine = store.addNode("image", "我的图");
    await store.save();
    expect(store.saveState.value).toBe("conflict");

    // 远端：同事删掉了 n_3，版本 5
    remote = project({ ...legacyDocument, nodes: legacyDocument.nodes.slice(0, 2), edges: [] }, 5);
    expect(await store.resolveConflict("adopt")).toBe("resolved");
    expect(store.saveState.value).toBe("saved");
    expect(store.version.value).toBe(5);
    expect(store.nodes.value.map(node => node.id)).toEqual(["n_1", "a_s1_1"]);
    expect(store.draftAvailable.value).toBe(true);
    expect(store.undoStack.value).toHaveLength(0); // 采纳远端即清撤销栈
    expect(session.has(draftKey("p1"))).toBe(true);

    expect(await store.restoreDraft()).toBe(true);
    expect(store.nodes.value.map(node => node.id)).toContain(mine);
    const last = calls.filter(call => call.method === "PUT").at(-1)!;
    expect((last.body as { version: number }).version).toBe(5);
    expect(store.saveState.value).toBe("saved");
    expect(store.draftAvailable.value).toBe(false);
    expect(session.has(draftKey("p1"))).toBe(false);
    // 恢复可撤销
    expect(store.undoStack.value).toHaveLength(1);
  });

  it("草稿写不进去时拒绝采纳（不静默丢改动）", async () => {
    const { store } = await loadedStore((call) => {
      if (call.method === "PUT") throw conflictError({ currentVersion: 5 });
      return project(legacyDocument);
    });
    const mine = store.addNode("image", "我的图");
    await store.save();
    vi.stubGlobal("sessionStorage", {
      getItem: () => null,
      setItem: () => { throw new Error("QuotaExceededError"); },
      removeItem: () => undefined
    });
    await expect(store.resolveConflict("adopt")).rejects.toThrow("草稿");
    expect(store.saveState.value).toBe("conflict");
    expect(store.nodes.value.some(node => node.id === mine)).toBe(true);
  });

  it("丢弃草稿", async () => {
    const { store, session } = await loadedStore((call) => {
      if (call.method === "PUT") throw conflictError({ currentVersion: 5 });
      return project(legacyDocument);
    });
    await store.save();
    await store.resolveConflict("adopt");
    store.discardDraft();
    expect(store.draftAvailable.value).toBe(false);
    expect(session.has(draftKey("p1"))).toBe(false);
  });
});

/**
 * 派生子节点（docs/agent/imagetools IMGD-25/26）：九宫格的 addPresetNode 行为不变；工具子节点走同一段
 * spawnChildNode，额外记 `params.tool` / `toolOptions`、同一源节点连续派生向下错开、`generate` 开关决定是否立即生成。
 */
describe("派生子节点（九宫格 / 图片工具）", () => {
  type SpawnStore = Store & {
    updateGeneration: (id: string, view: Record<string, unknown>) => void;
    addPresetNode: (sourceId: string, preset: Record<string, unknown>, operation: string) => string;
    spawnToolNode: (sourceId: string, tool: string, options: Record<string, unknown>, opts: { title: string; generate: boolean; model?: string }) => Promise<{ nodeId: string; generationId?: string; error?: unknown }>;
    selectedNodeId: { value: string | null };
  };
  type PositionedNode = { id: string; position: { x: number; y: number }; data: { title: string; params: Record<string, unknown> } };

  async function spawnStore() {
    const env = await loadedStore(() => project(legacyDocument));
    const store = env.store as unknown as SpawnStore;
    // n_3 是一张已出图的图片（position 0,300）
    store.updateGeneration("n_3", { id: "g9", status: "success", mediaId: "m9" });
    return { ...env, store };
  }
  const nodeOf = (store: SpawnStore, id: string) => store.nodes.value.find(node => node.id === id) as unknown as PositionedNode;

  it("九宫格派生：第一个在右侧 +420 同一行、之后向下错开（不叠在一起）、连线 order=1 带操作、标题即预设名、选中", async () => {
    const { store } = await spawnStore();
    const first = store.addPresetNode("n_3", { name: "人像质感调节", params: { presetCode: "portrait_tuning" } }, "img2img");
    const second = store.addPresetNode("n_3", { name: "720全景", params: { presetCode: "panorama_720" } }, "img2img");
    // 源节点在 (0, 300)，夹具里已经带一个子节点：第一个派生就错开一格，第二个再往下一格。
    expect(nodeOf(store, first).position.x).toBe(420);
    expect(nodeOf(store, second).position.x).toBe(420);
    expect(nodeOf(store, second).position.y).toBeGreaterThan(nodeOf(store, first).position.y);
    expect(nodeOf(store, first).data.title).toBe("人像质感调节");
    const edge = store.edges.value.find(item => item.target === first);
    expect(edge?.data).toMatchObject({ order: 1, operation: "img2img" });
    expect(store.selectedNodeId.value).toBe(second);
  });

  it("工具子节点：params 记工具与选项、连线操作码是 img2img、同一源节点连续派生向下错开、不生成", async () => {
    const { store } = await spawnStore();
    const generate = vi.fn();
    vi.stubGlobal("useGenerationStore", () => ({ generate }));
    const first = await store.spawnToolNode("n_3", "upscale", { level: "standard" }, { title: "高清", generate: false, model: "img_std" });
    const second = await store.spawnToolNode("n_3", "multi_angle", { preset: "back", horizontal: 180, vertical: 0, zoom: 5 }, { title: "多角度", generate: false });
    const a = nodeOf(store, first.nodeId);
    const b = nodeOf(store, second.nodeId);
    expect(a.data.title).toBe("高清");
    expect(a.data.params).toEqual({ tool: "upscale", toolOptions: { level: "standard" }, toolSourceMediaId: "m9", model: "img_std" });
    expect(b.data.params).not.toHaveProperty("model");
    // n_3 原本已有一个下游（a_s1_1），所以第一个工具子节点就排在第二位
    expect(a.position).toEqual({ x: 420, y: 300 + 1 * (180 + 40) });
    expect(b.position).toEqual({ x: 420, y: 300 + 2 * (180 + 40) });
    expect(store.edges.value.find(item => item.target === first.nodeId)?.data).toMatchObject({ order: 1, operation: "img2img" });
    expect(store.selectedNodeId.value).toBe(second.nodeId);
    expect(generate).not.toHaveBeenCalled();
  });

  it("generate=true：建好立即受理，请求带 params.tool 与源图输入；没有模型时不受理并说明", async () => {
    const { store } = await spawnStore();
    const generate = vi.fn(async () => "gen-1");
    vi.stubGlobal("useGenerationStore", () => ({ generate }));
    const result = await store.spawnToolNode("n_3", "relight", { smart: true, azimuth: 90 }, { title: "打光", generate: true, model: "fal_edit" });
    expect(result).toEqual({ nodeId: result.nodeId, generationId: "gen-1" });
    expect(generate).toHaveBeenCalledTimes(1);
    const [nodeId, request] = generate.mock.calls[0] as unknown as [string, Record<string, unknown>];
    expect(nodeId).toBe(result.nodeId);
    expect(request).toMatchObject({
      projectId: "p1", operation: "img2img", modelCode: "fal_edit", count: 1,
      params: { tool: "relight", toolOptions: { smart: true }, model: "fal_edit" },
      inputs: [{ sourceNodeId: "n_3", kind: "image", order: 1, generationId: "g9" }]
    });

    const missing = await store.spawnToolNode("n_3", "relight", { smart: true }, { title: "打光", generate: true });
    expect(missing.nodeId).not.toBe("");
    expect(String((missing.error as Error).message)).toContain("模型");
    expect(generate).toHaveBeenCalledTimes(1);
  });

  it("源节点不存在时什么都不建", async () => {
    const { store } = await spawnStore();
    const before = store.nodes.value.length;
    expect(await store.spawnToolNode("nope", "upscale", {}, { title: "高清", generate: false })).toEqual({ nodeId: "" });
    expect(store.addPresetNode("nope", { name: "x" }, "img2img")).toBe("");
    expect(store.nodes.value.length).toBe(before);
  });
});
