import { readFileSync } from "node:fs";
import { computed, ref, toRaw, type Ref } from "vue";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { DirectorScene } from "~/utils/directorScene";

/**
 * 导演台场景 store（DRT-14）。vitest 里没有 Nuxt / Pinia 的自动导入，按 store 用到的全局函数注入替身
 * （同 stores/canvas.test.ts）。`window` 用 EventTarget 代替，好派发 online / offline 事件。
 */

type Call = { path: string; method: string; body?: unknown };
type Handler = (call: Call) => unknown;

const DEFAULT_SCENE = JSON.parse(
  readFileSync(new URL("../../api/fixtures/director/scenes/valid-default.json", import.meta.url), "utf8")
) as DirectorScene;

const SCENE_PATH = "/projects/p1/director-scenes/s1";
const LIST_PATH = "/projects/p1/director-scenes";

function sceneResp(version: number, scene: DirectorScene = DEFAULT_SCENE, extra: Record<string, unknown> = {}) {
  return {
    id: "s1", projectId: "p1", canvasNodeId: "n_dir", schemaVersion: 1, version,
    renderFingerprint: `fp${version}`, scene: JSON.parse(JSON.stringify(scene)), records: [],
    lastEditorId: "u2", lastEditorName: "苏夏", editorKind: "user",
    createdAt: "2026-10-26T00:00:00Z", updatedAt: "2026-10-26T01:00:00Z", ...extra
  };
}

function summary(version: number, extra: Record<string, unknown> = {}) {
  return {
    id: "s1", canvasNodeId: "n_dir", version, renderFingerprint: `fp${version}`, outputCameraId: "obj_camera_1",
    lastEditorName: "苏夏", editorKind: "user", updatedAt: "2026-10-26T02:00:00Z", ...extra
  };
}

function bizError(statusCode: number, code: number | undefined, details: Record<string, unknown> = {}, traceId: string | undefined = "t1") {
  return Object.assign(new Error("请求失败"), { statusCode, data: { code, traceId, details } });
}
const conflictError = (details: Record<string, unknown>) => bizError(409, 40902, details);
/** useApi 对「请求根本没到服务端」的失败：状态码 500，但没有业务码也没有 traceId。 */
const networkError = () => Object.assign(new Error("请求失败"), { statusCode: 500, data: { code: undefined, traceId: undefined } });

function deferred<T>() {
  let resolve!: (v: T) => void;
  let reject!: (e: unknown) => void;
  const promise = new Promise<T>((res, rej) => { resolve = res; reject = rej; });
  return { promise, resolve, reject };
}

type Store = {
  scene: Ref<DirectorScene | null>;
  serverVersion: Ref<number>;
  renderFingerprint: Ref<string>;
  saveState: Ref<string>;
  conflict: Ref<null | Record<string, unknown>>;
  lastError: Ref<null | { message: string; code?: number; path?: string }>;
  proposal: Ref<unknown>;
  pendingDraft: Ref<unknown>;
  remoteNotice: Ref<null | { version: number; editorName: string; editorKind: string; seq: number }>;
  undoStack: Ref<string[]>;
  redoStack: Ref<string[]>;
  readonly: Ref<boolean>;
  readonlyReason: Ref<string | null>;
  playhead: Ref<number>;
  playing: Ref<boolean>;
  load: (pid: string, sid: string) => Promise<void>;
  commit: (mutator: (s: DirectorScene) => DirectorScene, options?: { touched?: string[] }) => boolean;
  undo: () => boolean;
  redo: () => boolean;
  save: () => Promise<void>;
  flush: () => Promise<number>;
  resolveConflict: (mode: "adopt" | "overwrite") => Promise<string>;
  handleRemoteUpdate: (frame: Record<string, unknown>) => void;
  setProposal: (p: unknown) => boolean;
  applyProposal: () => Promise<boolean>;
  discardProposal: () => void;
  requestBuild: (input: { prompt: string; mediaIds: string[] }) => Promise<{ baseVersion: number; scene: DirectorScene; defaults: string[] }>;
  reset: () => void;
};

type Env = {
  calls: Call[];
  session: Map<string, string>;
  role: Ref<string>;
  nav: { onLine: boolean };
  win: EventTarget;
  failDraftWrites: { value: boolean };
};

function setup(handler: Handler, { role = "editor" } = {}): Env {
  const calls: Call[] = [];
  const session = new Map<string, string>();
  const roleRef = ref(role);
  const nav = { onLine: true };
  const win = new EventTarget();
  const failDraftWrites = { value: false };
  vi.stubGlobal("ref", ref);
  vi.stubGlobal("computed", computed);
  vi.stubGlobal("toRaw", toRaw);
  vi.stubGlobal("defineStore", (_id: string, setupFn: () => unknown) => () => setupFn());
  vi.stubGlobal("useApi", () => async (path: string, options: { method?: string; body?: unknown } = {}) => {
    const call = { path, method: options.method ?? "GET", body: options.body };
    calls.push(call);
    return handler(call);
  });
  vi.stubGlobal("useWorkspaceStore", () => ({ get current() { return { role: roleRef.value }; } }));
  vi.stubGlobal("useAuthStore", () => ({ user: { id: "u1" } }));
  vi.stubGlobal("sessionStorage", {
    getItem: (k: string) => session.get(k) ?? null,
    setItem: (k: string, v: string) => {
      if (failDraftWrites.value) throw new Error("QuotaExceededError");
      session.set(k, v);
    },
    removeItem: (k: string) => { session.delete(k); }
  });
  vi.stubGlobal("navigator", nav);
  vi.stubGlobal("window", win);
  return { calls, session, role: roleRef, nav, win, failDraftWrites };
}

async function loadedStore(handler: Handler, options: { role?: string } = {}) {
  const env = setup(handler, options);
  const { useDirectorStore } = await import("./director");
  const store = useDirectorStore() as unknown as Store;
  await store.load("p1", "s1");
  return { store, ...env };
}

/** 一次真改动：把输出机位改个名字（不碰 AI 对象的 source）。 */
function rename(name: string) {
  return (s: DirectorScene): DirectorScene => ({
    ...s,
    objects: (s.objects ?? []).map((o) => (o.kind === "camera" ? { ...o, name } : o))
  });
}

const puts = (calls: Call[]) => calls.filter((c) => c.method === "PUT");
const json = (v: unknown) => JSON.parse(JSON.stringify(v));
const DRAFT_KEY = "seeaitv:director-draft:s1";

beforeEach(() => {
  vi.resetModules();
  vi.useFakeTimers();
});
afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

describe("director store：保存", () => {
  it("director store：改动 600ms 后保存一次", async () => {
    const { store, calls } = await loadedStore((call) => (call.method === "PUT" ? summary(4) : sceneResp(3)));
    expect(store.commit(rename("主机位"))).toBe(true);
    expect(store.saveState.value).toBe("dirty");
    await vi.advanceTimersByTimeAsync(599);
    expect(puts(calls)).toHaveLength(0);
    await vi.advanceTimersByTimeAsync(1);
    expect(puts(calls)).toHaveLength(1);
    const put = puts(calls)[0]!;
    expect(put.path).toBe(SCENE_PATH);
    expect(put.body).toMatchObject({ version: 3 });
    expect(json((put.body as { scene: DirectorScene }).scene).objects[0].name).toBe("主机位");
    expect(store.serverVersion.value).toBe(4);
    expect(store.renderFingerprint.value).toBe("fp4");
    expect(store.saveState.value).toBe("idle");
  });

  it("director store：保存中再改动会补存一次", async () => {
    const first = deferred<unknown>();
    let n = 0;
    const { store, calls } = await loadedStore((call) => {
      if (call.method !== "PUT") return sceneResp(3);
      n += 1;
      return n === 1 ? first.promise : summary(5);
    });
    store.commit(rename("A"));
    await vi.advanceTimersByTimeAsync(600);
    expect(store.saveState.value).toBe("saving");
    store.commit(rename("B"));
    expect(store.saveState.value).toBe("saving");
    first.resolve(summary(4));
    await vi.advanceTimersByTimeAsync(0);
    expect(puts(calls)).toHaveLength(2);
    // 补存带的是第一次保存回来的新版本，提交的是最新的本地内容。
    expect(puts(calls)[1]!.body).toMatchObject({ version: 4 });
    expect(json((puts(calls)[1]!.body as { scene: DirectorScene }).scene).objects[0].name).toBe("B");
    expect(store.serverVersion.value).toBe(5);
    expect(store.saveState.value).toBe("idle");
  });

  it("director store：保存前校验，坏正文不发请求", async () => {
    const { store, calls } = await loadedStore(() => sceneResp(3));
    store.commit((s) => ({ ...s, outputCameraId: "obj_missing" }));
    await vi.advanceTimersByTimeAsync(600);
    expect(puts(calls)).toHaveLength(0);
    expect(store.saveState.value).toBe("error");
    expect(store.lastError.value?.path).toBe("outputCameraId");
  });
});

describe("director store：冲突", () => {
  const otherSaved = { currentVersion: 5, currentEditorName: "苏夏", bySelf: false, editorKind: "user", currentSource: "user" };

  it("director store：40902 进冲突并写草稿", async () => {
    const { store, session } = await loadedStore((call) => {
      if (call.method === "PUT") throw conflictError(otherSaved);
      return sceneResp(3);
    });
    store.commit(rename("我的"));
    await vi.advanceTimersByTimeAsync(600);
    expect(store.saveState.value).toBe("conflict");
    expect(store.conflict.value).toMatchObject({ currentVersion: 5, currentEditorName: "苏夏", currentSource: "user" });
    const draft = JSON.parse(session.get(DRAFT_KEY) ?? "null");
    expect(draft?.scene.objects[0].name).toBe("我的");
    const { directorConflictText } = await import("./director");
    expect(directorConflictText(store.conflict.value as never)).toBe("苏夏 修改了这个场景");
  });

  it("director store：本人另一个窗口的冲突说「你在另一个窗口」（DR-AC-12）", async () => {
    const { store } = await loadedStore((call) => {
      if (call.method === "PUT") throw conflictError({ ...otherSaved, bySelf: true, currentEditorName: "我" });
      return sceneResp(3);
    });
    store.commit(rename("x"));
    await vi.advanceTimersByTimeAsync(600);
    const { directorConflictText } = await import("./director");
    expect(directorConflictText(store.conflict.value as never)).toBe("你在另一个窗口修改过这个场景");
  });

  it("director store：40905 不算冲突，进 error", async () => {
    const { store } = await loadedStore((call) => {
      if (call.method === "PUT") throw bizError(409, 40905);
      return sceneResp(3);
    });
    store.commit(rename("x"));
    await vi.advanceTimersByTimeAsync(600);
    expect(store.saveState.value).toBe("error");
    expect(store.conflict.value).toBeNull();
  });

  it("director store：载入最新清空撤销栈", async () => {
    let version = 3;
    const { store, calls, session } = await loadedStore((call) => {
      if (call.method === "PUT") throw conflictError(otherSaved);
      return sceneResp(version, DEFAULT_SCENE, { lastEditorName: "苏夏" });
    });
    store.commit(rename("A"));
    store.commit(rename("B"));
    store.undo();
    expect(store.undoStack.value.length).toBe(1);
    expect(store.redoStack.value.length).toBe(1);
    await store.save();
    expect(store.saveState.value).toBe("conflict");
    version = 5;
    expect(await store.resolveConflict("adopt")).toBe("resolved");
    expect(calls.at(-1)).toMatchObject({ method: "GET", path: SCENE_PATH });
    expect(store.undoStack.value).toEqual([]);
    expect(store.redoStack.value).toEqual([]);
    expect(store.serverVersion.value).toBe(5);
    expect(store.saveState.value).toBe("idle");
    expect(json(store.scene.value).objects[0].name).toBe("机位 1");
    // 自己的改动没有丢：草稿还在，界面可以询问是否恢复。
    expect(session.get(DRAFT_KEY)).toBeTruthy();
    expect(store.pendingDraft.value).toBeTruthy();
  });

  it("director store：草稿写不进去时拒绝载入最新", async () => {
    const { store, calls, failDraftWrites } = await loadedStore((call) => {
      if (call.method === "PUT") throw conflictError(otherSaved);
      return sceneResp(3);
    });
    store.commit(rename("A"));
    await store.save();
    failDraftWrites.value = true;
    const before = calls.length;
    await expect(store.resolveConflict("adopt")).rejects.toThrow(/草稿/);
    expect(calls.length).toBe(before);
    expect(store.saveState.value).toBe("conflict");
  });

  it("director store：以我为准先取版本再提交本地内容", async () => {
    let conflicted = false;
    const { store, calls, session } = await loadedStore((call) => {
      if (call.method === "PUT") {
        if (!conflicted) {
          conflicted = true;
          throw conflictError(otherSaved);
        }
        return summary(6);
      }
      if (call.path === LIST_PATH) return { items: [summary(5)] };
      return sceneResp(3);
    });
    store.commit(rename("我的"));
    await store.save();
    expect(store.saveState.value).toBe("conflict");
    expect(await store.resolveConflict("overwrite")).toBe("resolved");
    const tail = calls.slice(-2);
    expect(tail[0]).toMatchObject({ method: "GET", path: LIST_PATH });
    expect(tail[1]).toMatchObject({ method: "PUT", path: SCENE_PATH });
    expect(tail[1]!.body).toMatchObject({ version: 5 });
    expect(json((tail[1]!.body as { scene: DirectorScene }).scene).objects[0].name).toBe("我的");
    expect(store.serverVersion.value).toBe(6);
    expect(store.saveState.value).toBe("idle");
    expect(session.get(DRAFT_KEY)).toBeUndefined();
  });

  it("director store：flush 在冲突时 reject", async () => {
    const { store } = await loadedStore((call) => {
      if (call.method === "PUT") throw conflictError(otherSaved);
      return sceneResp(3);
    });
    store.commit(rename("x"));
    await expect(store.flush()).rejects.toMatchObject({ state: "conflict" });
  });

  it("director store：flush 立即保存并返回最新版本", async () => {
    const { store, calls } = await loadedStore((call) => (call.method === "PUT" ? summary(4) : sceneResp(3)));
    expect(await store.flush()).toBe(3);
    store.commit(rename("x"));
    expect(await store.flush()).toBe(4);
    expect(puts(calls)).toHaveLength(1);
    // 防抖被取消：之后不会再多存一次。
    await vi.advanceTimersByTimeAsync(1000);
    expect(puts(calls)).toHaveLength(1);
  });
});

describe("director store：离线（DR-F-153）", () => {
  it("director store：离线不保存", async () => {
    const { store, calls, nav, win } = await loadedStore(() => sceneResp(3));
    nav.onLine = false;
    win.dispatchEvent(new Event("offline"));
    expect(store.saveState.value).toBe("offline");
    store.commit(rename("x"));
    await vi.advanceTimersByTimeAsync(5000);
    expect(puts(calls)).toHaveLength(0);
    expect(store.saveState.value).toBe("offline");
    await expect(store.flush()).rejects.toMatchObject({ state: "offline" });
  });

  it("director store：恢复后版本未变则补存", async () => {
    const { store, calls, nav, win } = await loadedStore((call) => {
      if (call.method === "PUT") return summary(4);
      if (call.path === LIST_PATH) return { items: [summary(3)] };
      return sceneResp(3);
    });
    nav.onLine = false;
    win.dispatchEvent(new Event("offline"));
    store.commit(rename("x"));
    nav.onLine = true;
    win.dispatchEvent(new Event("online"));
    await vi.advanceTimersByTimeAsync(0);
    const tail = calls.slice(-2);
    expect(tail[0]).toMatchObject({ method: "GET", path: LIST_PATH });
    expect(tail[1]).toMatchObject({ method: "PUT" });
    expect(store.serverVersion.value).toBe(4);
    expect(store.saveState.value).toBe("idle");
  });

  it("director store：恢复后版本已变直接进冲突且不发 PUT", async () => {
    const { store, calls, nav, win } = await loadedStore((call) => {
      if (call.path === LIST_PATH) return { items: [summary(4, { lastEditorName: "老周", editorKind: "agent" })] };
      return sceneResp(3);
    });
    nav.onLine = false;
    win.dispatchEvent(new Event("offline"));
    store.commit(rename("x"));
    nav.onLine = true;
    win.dispatchEvent(new Event("online"));
    await vi.advanceTimersByTimeAsync(0);
    expect(store.saveState.value).toBe("conflict");
    expect(puts(calls)).toHaveLength(0);
    expect(store.conflict.value).toMatchObject({ currentVersion: 4, currentEditorName: "老周", editorKind: "agent", currentSource: "agent" });
  });

  it("director store：对账失败回到 error", async () => {
    const { store, calls, nav, win } = await loadedStore((call) => {
      if (call.path === LIST_PATH) throw bizError(500, 50001);
      return sceneResp(3);
    });
    nav.onLine = false;
    win.dispatchEvent(new Event("offline"));
    store.commit(rename("x"));
    nav.onLine = true;
    win.dispatchEvent(new Event("online"));
    await vi.advanceTimersByTimeAsync(0);
    expect(store.saveState.value).toBe("error");
    expect(puts(calls)).toHaveLength(0);
  });

  it("director store：请求没到服务端而浏览器报在线时进 offline，过一会儿自己对账", async () => {
    let failPut = true;
    const { store, calls } = await loadedStore((call) => {
      if (call.method === "PUT") {
        if (failPut) throw networkError();
        return summary(4);
      }
      if (call.path === LIST_PATH) return { items: [summary(3)] };
      return sceneResp(3);
    });
    store.commit(rename("x"));
    await vi.advanceTimersByTimeAsync(600);
    expect(store.saveState.value).toBe("offline");
    failPut = false;
    await vi.advanceTimersByTimeAsync(5000);
    expect(calls.some((c) => c.path === LIST_PATH)).toBe(true);
    expect(store.saveState.value).toBe("idle");
    expect(store.serverVersion.value).toBe(4);
  });
});

describe("director store：实时事件（DR-F-155）", () => {
  const frame = (version: number, extra: Record<string, unknown> = {}) => ({
    type: "director.scene_updated", projectId: "p1", sceneId: "s1", canvasNodeId: "n_dir", version,
    renderFingerprint: `fp${version}`, editorKind: "user", editorId: "u2", editorName: "苏夏",
    occurredAt: "2026-10-26T03:00:00Z", ...extra
  });

  it("director store：空闲时远端更新自动采纳", async () => {
    let version = 3;
    const { store, calls } = await loadedStore((call) => {
      if (call.method === "PUT") return summary(++version);
      return sceneResp(version, rename("远端改的")(DEFAULT_SCENE), { lastEditorName: "苏夏" });
    });
    store.commit(rename("x"));
    await vi.advanceTimersByTimeAsync(600);
    expect(store.undoStack.value.length).toBe(1);
    version = 5;
    store.handleRemoteUpdate(frame(5));
    await vi.advanceTimersByTimeAsync(0);
    expect(calls.at(-1)).toMatchObject({ method: "GET", path: SCENE_PATH });
    expect(store.serverVersion.value).toBe(5);
    expect(json(store.scene.value).objects[0].name).toBe("远端改的");
    expect(store.undoStack.value).toEqual([]);
    expect(store.remoteNotice.value).toMatchObject({ version: 5, editorName: "苏夏" });
    const { directorRemoteNoticeText } = await import("./director");
    expect(directorRemoteNoticeText(store.remoteNotice.value as never)).toBe("场景被 苏夏 更新");
  });

  it("director store：Agent / MCP 写的远端更新，提示「场景被 Agent 更新」（DR-AC-46）", async () => {
    for (const [kind, text] of [["agent", "场景被 Agent 更新"], ["mcp", "场景被外部 AI 客户端更新"]] as const) {
      let version = 3;
      const { store } = await loadedStore(() =>
        sceneResp(version, rename("Agent 摆的")(DEFAULT_SCENE), { editorKind: kind, lastEditorName: "苏夏" }));
      version = 4;
      store.handleRemoteUpdate(frame(4, { editorKind: kind }));
      await vi.advanceTimersByTimeAsync(0);
      expect(store.serverVersion.value).toBe(4);
      expect(json(store.scene.value).objects[0].name).toBe("Agent 摆的");
      expect(store.remoteNotice.value).toMatchObject({ version: 4, editorKind: kind });
      const { directorRemoteNoticeText } = await import("./director");
      // 机器写入不报人名：响应里的 lastEditorName 是代谁写的那个人，报出来会说成「被苏夏更新」。
      expect(directorRemoteNoticeText(store.remoteNotice.value as never)).toBe(text);
    }
  });

  it("director store：提示文案三种来源同一个句式", async () => {
    await loadedStore(() => sceneResp(1)); // 装上 store 依赖的自动导入，模块才能加载
    const { directorRemoteNoticeText } = await import("./director");
    const notice = (editorKind: string, editorName = "") => ({ version: 2, editorName, editorKind, seq: 1 }) as never;
    expect(directorRemoteNoticeText(notice("agent", "苏夏"))).toBe("场景被 Agent 更新");
    expect(directorRemoteNoticeText(notice("mcp"))).toBe("场景被外部 AI 客户端更新");
    expect(directorRemoteNoticeText(notice("user", "苏夏"))).toBe("场景被 苏夏 更新");
    expect(directorRemoteNoticeText(notice("user"))).toBe("场景已在别处更新");
    expect(directorRemoteNoticeText(null)).toBe("");
  });

  it("director store：不是本场景或版本不比本地新的事件忽略", async () => {
    const { store, calls } = await loadedStore(() => sceneResp(3));
    const before = calls.length;
    store.handleRemoteUpdate(frame(9, { sceneId: "other" }));
    store.handleRemoteUpdate(frame(3));
    store.handleRemoteUpdate({ ...frame(9), type: "canvas.ops_applied" });
    await vi.advanceTimersByTimeAsync(0);
    expect(calls.length).toBe(before);
  });

  it("director store：有改动时远端更新进冲突", async () => {
    const { store, calls, session } = await loadedStore(() => sceneResp(3));
    store.commit(rename("我的"));
    store.handleRemoteUpdate(frame(4));
    await vi.advanceTimersByTimeAsync(1000);
    expect(store.saveState.value).toBe("conflict");
    expect(store.conflict.value).toMatchObject({ currentVersion: 4, currentEditorName: "苏夏", bySelf: false });
    // 不采纳、也不发那个注定 409 的保存。
    expect(calls.filter((c) => c.method === "GET")).toHaveLength(1);
    expect(puts(calls)).toHaveLength(0);
    expect(session.get(DRAFT_KEY)).toBeTruthy();
  });

  it("director store：保存中收到自己的回声不误判", async () => {
    const pending = deferred<unknown>();
    const { store, calls } = await loadedStore((call) => (call.method === "PUT" ? pending.promise : sceneResp(3)));
    store.commit(rename("x"));
    await vi.advanceTimersByTimeAsync(600);
    // 事件比 PUT 的响应先到：版本 4 就是自己这次保存。
    store.handleRemoteUpdate(frame(4, { editorId: "u1", editorName: "我" }));
    pending.resolve(summary(4));
    await vi.advanceTimersByTimeAsync(0);
    expect(store.saveState.value).toBe("idle");
    expect(store.conflict.value).toBeNull();
    expect(calls.filter((c) => c.method === "GET")).toHaveLength(1);
  });

  it("director store：保存中收到别人的更新，保存结束后再采纳", async () => {
    const pending = deferred<unknown>();
    let version = 3;
    const { store, calls } = await loadedStore((call) => (call.method === "PUT" ? pending.promise : sceneResp(version)));
    store.commit(rename("x"));
    await vi.advanceTimersByTimeAsync(600);
    store.handleRemoteUpdate(frame(5));
    version = 5;
    pending.resolve(summary(4));
    await vi.advanceTimersByTimeAsync(0);
    expect(calls.at(-1)).toMatchObject({ method: "GET", path: SCENE_PATH });
    expect(store.serverVersion.value).toBe(5);
    expect(store.saveState.value).toBe("idle");
  });
});

describe("director store：撤销栈（DRD-36）", () => {
  it("director store：撤销最多 60 步", async () => {
    const { store } = await loadedStore(() => sceneResp(3));
    for (let i = 0; i < 65; i += 1) store.commit(rename(`机位${i}`));
    expect(store.undoStack.value.length).toBe(60);
  });

  it("director store：撤销总字节超过 20MB 丢最旧的", async () => {
    const { store } = await loadedStore(() => sceneResp(3));
    const big = (tag: string) => (s: DirectorScene): DirectorScene => ({ ...s, note: tag + "x".repeat(2_000_000) });
    for (let i = 0; i < 7; i += 1) store.commit(big(String(i)));
    const bytes = store.undoStack.value.reduce((sum, s) => sum + s.length * 2, 0);
    expect(bytes).toBeLessThanOrEqual(20 * 1024 * 1024);
    // 6 份大快照放不下：最旧的（默认场景与第一份）被丢掉，剩下 5 份。
    expect(store.undoStack.value.length).toBe(5);
    expect(JSON.parse(store.undoStack.value[0]!).note.startsWith("1")).toBe(true);
  });

  it("director store：新提交清空重做栈", async () => {
    const { store } = await loadedStore(() => sceneResp(3));
    store.commit(rename("A"));
    store.commit(rename("B"));
    expect(store.undo()).toBe(true);
    expect(json(store.scene.value).objects[0].name).toBe("A");
    expect(store.redoStack.value.length).toBe(1);
    expect(store.redo()).toBe(true);
    expect(json(store.scene.value).objects[0].name).toBe("B");
    store.undo();
    store.commit(rename("C"));
    expect(store.redoStack.value).toEqual([]);
  });

  it("director store：用户改动 AI 对象后 source 变 user", async () => {
    const { store } = await loadedStore(() => sceneResp(3));
    const moved = (s: DirectorScene): DirectorScene => ({
      ...s,
      objects: (s.objects ?? []).map((o) => (o.id === "obj_character_1"
        ? { ...o, transform: { ...o.transform, position: { x: 1, y: 0, z: 0 } } } : o))
    });
    expect(json(store.scene.value).objects.find((o: { id: string }) => o.id === "obj_character_1").source).toBe("ai");
    store.commit(moved, { touched: ["obj_character_1"] });
    const objects = json(store.scene.value).objects as Array<{ id: string; source: string }>;
    expect(objects.find((o) => o.id === "obj_character_1")!.source).toBe("user");
    // 没被碰到的 AI 对象保持可替换。
    expect(objects.find((o) => o.id === "obj_camera_1")!.source).toBe("ai");
  });
});

describe("director store：姿势（DRT-21）", () => {
  const HERO = "obj_character_1";
  const characterOf = (store: Store) => json(store.scene.value).objects.find((o: { id: string }) => o.id === HERO).character;

  it("stores/director 套用姿势记一步撤销", async () => {
    const { store } = await loadedStore(() => sceneResp(3));
    const { directorApplyPosePreset, directorSetPose } = await import("~/utils/directorPose");
    store.commit(s => directorSetPose(s, HERO, { boneOverrides: { Head: { x: 0, y: 25, z: 0 } } }), { touched: [HERO] });
    const steps = store.undoStack.value.length;
    expect(store.commit(s => directorApplyPosePreset(s, HERO, "sit_chair"), { touched: [HERO] })).toBe(true);
    expect(store.undoStack.value.length).toBe(steps + 1);
    expect(characterOf(store)).toMatchObject({ posePreset: "sit_chair", boneOverrides: {} });
    // 撤销一次回到套用之前（含手动调整）
    expect(store.undo()).toBe(true);
    expect(characterOf(store)).toMatchObject({ posePreset: "stand_neutral", boneOverrides: { Head: { x: 0, y: 25, z: 0 } } });
  });

  it("重置姿势清空 boneOverrides", async () => {
    const { store } = await loadedStore(() => sceneResp(3));
    const { directorApplyPosePreset, directorResetPose, directorSetPose } = await import("~/utils/directorPose");
    store.commit(s => directorApplyPosePreset(s, HERO, "squat"), { touched: [HERO] });
    store.commit(s => directorSetPose(s, HERO, { boneOverrides: { LeftLeg: { x: 40, y: 0, z: 0 } } }), { touched: [HERO] });
    expect(store.commit(s => directorResetPose(s, HERO), { touched: [HERO] })).toBe(true);
    expect(characterOf(store)).toMatchObject({ posePreset: "stand_neutral", boneOverrides: {} });
    // 已经是重置后的样子：再点一次不记撤销
    const steps = store.undoStack.value.length;
    expect(store.commit(s => directorResetPose(s, HERO), { touched: [HERO] })).toBe(false);
    expect(store.undoStack.value.length).toBe(steps);
  });
});

describe("director store：时间轴（DRT-24）", () => {
  const CAM = "obj_camera_1";
  const cameraTrack = (s: DirectorScene): DirectorScene => ({
    ...s,
    timeline: { fps: 30, durationFrames: 150, loop: false, tracks: [{ objectId: CAM, channel: "camera", keyframes: [
      { frame: 0, interpolation: "linear", value: { position: { x: 0, y: 1.5, z: 4 }, rotation: { x: 0, y: 0, z: 0 }, focalLengthMm: 35 } }
    ] }] }
  });
  const moveCamera = (x: number) => (s: DirectorScene): DirectorScene => ({
    ...s,
    objects: (s.objects ?? []).map((o) => (o.id === CAM ? { ...o, transform: { ...o.transform, position: { ...o.transform.position, x } } } : o))
  });
  const keyframesOf = (store: Store) => json(store.scene.value).timeline.tracks[0].keyframes.map((k: { frame: number }) => k.frame);

  it("stores/director 自动打帧记一步撤销", async () => {
    const { store } = await loadedStore(() => sceneResp(3));
    store.commit(cameraTrack);
    store.playhead.value = 45;
    const steps = store.undoStack.value.length;
    expect(store.commit(moveCamera(2), { touched: [CAM] })).toBe(true);
    expect(store.undoStack.value.length).toBe(steps + 1);
    expect(keyframesOf(store)).toEqual([0, 45]);
    // 撤销一次：关键帧与基础值一起回去
    store.undo();
    expect(keyframesOf(store)).toEqual([0]);
    expect(json(store.scene.value).objects.find((o: { id: string }) => o.id === CAM).transform.position.x).not.toBe(2);
  });

  it("播放状态不进撤销栈", async () => {
    const { store, calls } = await loadedStore(() => sceneResp(3));
    const steps = store.undoStack.value.length;
    store.playhead.value = 90;
    store.playing.value = true;
    store.playhead.value = 91;
    expect(store.undoStack.value.length).toBe(steps);
    await vi.advanceTimersByTimeAsync(2000);
    expect(puts(calls)).toHaveLength(0);
    // 换场景（载入）时播放头回到 0
    store.reset();
    expect([store.playhead.value, store.playing.value]).toEqual([0, false]);
  });
});

describe("director store：提案与只读", () => {
  const proposalScene = rename("AI 机位")(DEFAULT_SCENE);
  const proposal = (baseVersion = 3) => ({ scene: proposalScene, changes: { added: [], updated: ["obj_camera_1"], removed: [] }, defaults: [], baseVersion });

  it("director store：有提案时拒绝 commit", async () => {
    const { store } = await loadedStore(() => sceneResp(3));
    store.commit(rename("A"));
    expect(store.setProposal(proposal())).toBe(true);
    expect(store.commit(rename("B"))).toBe(false);
    expect(store.undo()).toBe(false);
  });

  it("director store：应用提案只记一步撤销", async () => {
    const { store, calls } = await loadedStore((call) => (call.method === "PUT" ? summary(4) : sceneResp(3)));
    const before = json(store.scene.value);
    store.setProposal(proposal());
    expect(await store.applyProposal()).toBe(true);
    expect(store.proposal.value).toBeNull();
    expect(store.undoStack.value.length).toBe(1);
    expect(puts(calls)).toHaveLength(1);
    expect(json(store.scene.value).objects[0].name).toBe("AI 机位");
    store.undo();
    expect(json(store.scene.value)).toEqual(before);
  });

  it("discardProposal 不改场景", async () => {
    const { store, calls } = await loadedStore(() => sceneResp(3));
    const before = json(store.scene.value);
    store.setProposal(proposal());
    store.discardProposal();
    expect(store.proposal.value).toBeNull();
    expect(json(store.scene.value)).toEqual(before);
    expect(store.undoStack.value.length).toBe(0);
    await vi.advanceTimersByTimeAsync(2000);
    expect(puts(calls)).toHaveLength(0);
  });

  it("requestBuild 先 flush 再请求，带刚存上的版本；返回提案但不自己设成当前提案", async () => {
    const { store, calls } = await loadedStore((call) => {
      if (call.method === "PUT") return summary(4);
      if (call.path.endsWith("/build")) {
        return { baseVersion: 4, callId: "c1", modelCode: "m1", proposal: { scene: proposalScene, changes: { added: [], updated: [], removed: [] }, defaults: ["机位按 35mm"] } };
      }
      return sceneResp(3);
    });
    store.commit(rename("A"));
    const result = await store.requestBuild({ prompt: "古代集市", mediaIds: ["11", "12"] });
    const order = calls.filter(c => c.method !== "GET").map(c => `${c.method} ${c.path.split("/").pop()}`);
    expect(order).toEqual(["PUT s1", "POST build"]);
    expect(calls.find(c => c.path.endsWith("/build"))?.body).toEqual({ prompt: "古代集市", referenceMediaIds: ["11", "12"], sceneVersion: 4 });
    expect(result.baseVersion).toBe(4);
    expect(result.defaults).toEqual(["机位按 35mm"]);
    // 用户可能已经点了「不再等待」：设不设成当前提案由调用方决定
    expect(store.proposal.value).toBeNull();
  });

  it("director store：场景在搭景期间被改过时提案作废", async () => {
    const { store } = await loadedStore(() => sceneResp(3));
    store.setProposal(proposal(2));
    await expect(store.applyProposal()).rejects.toThrow(/作废/);
    expect(store.proposal.value).toBeNull();
  });

  it("director store：只读时 commit 返回 false", async () => {
    const { store, calls } = await loadedStore(() => sceneResp(3), { role: "viewer" });
    expect(store.readonly.value).toBe(true);
    expect(store.readonlyReason.value).toBe("role");
    expect(store.commit(rename("x"))).toBe(false);
    await vi.advanceTimersByTimeAsync(1000);
    expect(puts(calls)).toHaveLength(0);
  });

  it("director store：403 后进只读", async () => {
    const { store, calls, session } = await loadedStore((call) => {
      if (call.method === "PUT") throw bizError(403, 40301);
      return sceneResp(3);
    });
    store.commit(rename("x"));
    await vi.advanceTimersByTimeAsync(600);
    expect(store.readonly.value).toBe(true);
    expect(store.readonlyReason.value).toBe("forbidden");
    expect(store.commit(rename("y"))).toBe(false);
    await vi.advanceTimersByTimeAsync(1000);
    expect(puts(calls)).toHaveLength(1);
    expect(session.get(DRAFT_KEY)).toBeTruthy();
  });

  it("director store：场景是更新的结构版本时只读", async () => {
    const { store } = await loadedStore(() => sceneResp(3, { ...DEFAULT_SCENE, schemaVersion: 2 }));
    expect(store.readonlyReason.value).toBe("newer");
    expect(store.commit(rename("x"))).toBe(false);
  });
});
