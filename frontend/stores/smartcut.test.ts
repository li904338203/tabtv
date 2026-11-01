import { computed, ref, toRaw, type Ref } from "vue";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { components } from "@tabtv/api-client";
import { smartcutConflictText, smartcutRemoteNoticeText } from "~/utils/smartcutPlan";

/**
 * 智能剪辑的剪辑 store（SCT-19）。vitest 里没有 Nuxt / Pinia 的自动导入，按 store 用到的全局函数注入替身
 * （同 stores/director.test.ts）。`window` 用 EventTarget 代替，好派发 online / offline 事件。
 */

type Timeline = components["schemas"]["Timeline"];
type TimelineClip = components["schemas"]["TimelineClip"];
type Call = { path: string; method: string; body?: unknown; headers?: Record<string, string> };
type Handler = (call: Call) => unknown;

const TL = "/timelines/t1";
const CONTENT = "/timelines/t1/content";
const LIST = "/projects/p1/smartcuts";
const DRAFT_KEY = "seeaitv:smartcut-draft:t1";

function clip(id: string, mediaId: string, outMs: number, extra: Partial<TimelineClip> = {}): TimelineClip {
  return { id, idx: 0, mediaId, sourceInMs: 0, sourceOutMs: outMs, startMs: 0, durationMs: outMs, speed: 100, volume: 100, origin: "user", ...extra };
}

function timelineResp(version: number, videoClips: TimelineClip[] = [clip("21", "77", 4000), clip("22", "78", 3000)], extra: Partial<Timeline> = {}): Timeline {
  let cursor = 0;
  const placed = videoClips.map((c, i) => {
    const out = { ...c, idx: i + 1, startMs: cursor, durationMs: c.sourceOutMs - c.sourceInMs };
    cursor += out.durationMs;
    return out;
  });
  return {
    id: "t1", projectId: "p1", name: "剪辑", version, aspectRatio: "16:9", fps: 30, width: 1920, height: 1080,
    status: "editing", canvasNodeId: "n_sc", isArranged: true, lastEditorKind: "user",
    tracks: [
      { id: "11", kind: "video", idx: 1, isMuted: false, volume: 100, clips: placed },
      { id: "12", kind: "audio", idx: 2, isMuted: false, volume: 100, role: "music", clips: [] },
      { id: "13", kind: "subtitle", idx: 3, isMuted: false, volume: 100, lang: "zh-CN", clips: [] }
    ],
    subtitles: [], texts: [], totalMs: cursor, effectiveMs: cursor,
    ...extra
  };
}

function saveResp(version: number, videoClips?: TimelineClip[], idMap: Record<string, string> = {}) {
  return { timeline: timelineResp(version, videoClips), idMap };
}

function summary(version: number, extra: Record<string, unknown> = {}) {
  return {
    timelineId: "t1", canvasNodeId: "n_sc", version, isArranged: true, totalMs: 7000, effectiveMs: 7000, clipCount: 2,
    inputsStale: false, renderStale: false, strip: [], updatedAt: "2026-10-28T02:00:00Z", ...extra
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
  timeline: Ref<Timeline | null>;
  serverVersion: Ref<number>;
  saveState: Ref<string>;
  conflict: Ref<null | Record<string, unknown>>;
  lastError: Ref<null | { message: string; code?: number; path?: string }>;
  pendingDraft: Ref<null | Record<string, unknown>>;
  remoteNotice: Ref<null | { version: number; editorKind: string; editorName: string; bySelf: boolean; reason: string; seq: number }>;
  undoStack: Ref<string[]>;
  redoStack: Ref<string[]>;
  readonly: Ref<boolean>;
  readonlyReason: Ref<string | null>;
  unsaved: Ref<boolean>;
  selection: Ref<null | { kind: string; ids: string[] }>;
  plan: Ref<null | Record<string, unknown>>;
  aiRound: Ref<null | { snapshotId: string; editedAfter: boolean }>;
  snapshots: Ref<unknown[]>;
  inputs: Ref<unknown[]>;
  load: (pid: string, tid: string) => Promise<void>;
  commit: (mutator: (t: Timeline) => Timeline) => boolean;
  undo: () => boolean;
  redo: () => boolean;
  save: () => Promise<void>;
  flush: () => Promise<number>;
  retry: () => Promise<void>;
  resolveConflict: (mode: "adopt" | "overwrite") => Promise<string>;
  restoreDraft: () => Promise<boolean>;
  handleRemoteUpdate: (frame: Record<string, unknown>) => void;
  runPlan: (req: Record<string, unknown>) => Promise<unknown>;
  undoAiRound: () => Promise<unknown[]>;
  refreshSnapshots: () => Promise<void>;
  select: (sel: { kind: string; ids: string[] } | null) => void;
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

/** 没写到的接口给个默认值：素材条空、配置接口还不存在（404）、没有快照。 */
function defaults(call: Call): unknown {
  if (call.path.endsWith("/inputs")) return { items: [], fingerprint: "f0" };
  if (call.path === "/smartcut/config") throw bizError(404, 40401);
  if (call.path.endsWith("/snapshots")) return { items: [] };
  return undefined;
}

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
  vi.stubGlobal("useApi", () => async (path: string, options: { method?: string; body?: unknown; headers?: Record<string, string> } = {}) => {
    const call = { path, method: options.method ?? "GET", body: options.body === undefined ? undefined : JSON.parse(JSON.stringify(options.body)),
      ...(options.headers ? { headers: options.headers } : {}) };
    calls.push(call);
    const out = handler(call);
    return out === undefined ? defaults(call) : out;
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
  const { useSmartcutStore } = await import("./smartcut");
  const store = useSmartcutStore() as unknown as Store;
  await store.load("p1", "t1");
  await vi.advanceTimersByTimeAsync(0);
  return { store, ...env };
}

/** 一次真改动：给第一段改音量。 */
function setVolume(volume: number) {
  return (t: Timeline): Timeline => ({
    ...t,
    tracks: t.tracks.map(tr => (tr.kind === "video" ? { ...tr, clips: tr.clips.map((c, i) => (i === 0 ? { ...c, volume } : c)) } : tr))
  });
}

/** 在画面轨末尾加一段（临时 ID）。 */
function appendClip(id: string, outMs = 2000) {
  return (t: Timeline): Timeline => ({
    ...t,
    tracks: t.tracks.map(tr => (tr.kind === "video" ? { ...tr, clips: [...tr.clips, clip(id, "79", outMs)] } : tr))
  });
}

function setClipOut(id: string, outMs: number) {
  return (t: Timeline): Timeline => ({
    ...t,
    tracks: t.tracks.map(tr => ({ ...tr, clips: tr.clips.map(c => (c.id === id ? { ...c, sourceOutMs: outMs } : c)) }))
  });
}

const puts = (calls: Call[]) => calls.filter(c => c.method === "PUT");
type PutBody = { version: number; tracks: Array<{ kind: string; clips: Array<Record<string, unknown>> }> };
const videoClipsOf = (body: unknown) => (body as PutBody).tracks.find(t => t.kind === "video")!.clips;

beforeEach(() => {
  vi.resetModules();
  vi.useFakeTimers();
});
afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

describe("smartcut store：载入", () => {
  it("smartcut store：载入时间线并带上素材条；配置接口不存在不挡载入", async () => {
    const { store, calls } = await loadedStore((call) => {
      if (call.path === TL) return timelineResp(3);
      if (call.path.endsWith("/inputs")) return { items: [{ no: "1", sourceNodeId: "n1", sourceNodeType: "video", kind: "video", status: "ready", used: true }], fingerprint: "fp" };
      return undefined;
    });
    expect(store.serverVersion.value).toBe(3);
    expect(store.saveState.value).toBe("idle");
    expect(store.inputs.value).toHaveLength(1);
    expect(calls.map(c => c.path)).toEqual(expect.arrayContaining([TL, "/projects/p1/smartcuts/t1/inputs", "/smartcut/config", "/timelines/t1/snapshots"]));
  });

  it("smartcut store：本标签页有比服务端新的草稿时给恢复选项；内容相同的草稿直接删掉", async () => {
    const env = setup(call => (call.path === TL ? timelineResp(3) : undefined));
    const draftTimeline = setVolume(40)(timelineResp(3));
    env.session.set(DRAFT_KEY, JSON.stringify({ projectId: "p1", timelineId: "t1", baseVersion: 3, savedAt: "2026-10-28T03:00:00Z", timeline: draftTimeline }));
    const { useSmartcutStore } = await import("./smartcut");
    const store = useSmartcutStore() as unknown as Store;
    await store.load("p1", "t1");
    expect(store.pendingDraft.value).toMatchObject({ baseVersion: 3, serverChanged: false });
    expect(await store.restoreDraft()).toBe(true);
    expect(store.timeline.value!.tracks[0]!.clips[0]!.volume).toBe(40);
    // 恢复是一步可撤回的改动
    expect(store.undoStack.value).toHaveLength(1);

    store.reset();
    env.session.set(DRAFT_KEY, JSON.stringify({ projectId: "p1", timelineId: "t1", baseVersion: 3, savedAt: "2026-10-28T03:00:00Z", timeline: timelineResp(3) }));
    await store.load("p1", "t1");
    expect(store.pendingDraft.value).toBeNull();
    expect(env.session.has(DRAFT_KEY)).toBe(false);
  });

  it("smartcut store：等待期间换了时间线，晚到的结果丢弃", async () => {
    const first = deferred<Timeline>();
    const env = setup(call => (call.path === TL ? first.promise : call.path === "/timelines/t2" ? { ...timelineResp(9), id: "t2" } : undefined));
    const { useSmartcutStore } = await import("./smartcut");
    const store = useSmartcutStore() as unknown as Store;
    const p1 = store.load("p1", "t1");
    await store.load("p1", "t2");
    first.resolve(timelineResp(3));
    await p1;
    expect(store.serverVersion.value).toBe(9);
    expect(env.calls.some(c => c.path === TL)).toBe(true);
  });
});

describe("smartcut store：保存", () => {
  it("smartcut store：改动 600ms 后整份保存一次，成功后 serverVersion 换成服务端的（SC-AC-08）", async () => {
    const { store, calls } = await loadedStore(call => (call.method === "PUT" ? saveResp(4) : call.path === TL ? timelineResp(3) : undefined));
    expect(store.commit(setVolume(50))).toBe(true);
    expect(store.saveState.value).toBe("dirty");
    expect(store.unsaved.value).toBe(true);
    await vi.advanceTimersByTimeAsync(599);
    expect(puts(calls)).toHaveLength(0);
    await vi.advanceTimersByTimeAsync(1);
    expect(puts(calls)).toHaveLength(1);
    const put = puts(calls)[0]!;
    expect(put.path).toBe(CONTENT);
    expect(put.body).toMatchObject({ version: 3, aspectRatio: "16:9", fps: 30, width: 1920, height: 1080 });
    const sent = videoClipsOf(put.body);
    expect(sent[0]).toMatchObject({ id: "21", volume: 50 });
    // 画面轨落点由服务端算：不发 startMs，也不发 idx / durationMs 这些服务端字段
    expect(sent[0]).not.toHaveProperty("startMs");
    expect(sent[0]).not.toHaveProperty("idx");
    expect(sent[0]).not.toHaveProperty("durationMs");
    expect(store.serverVersion.value).toBe(4);
    expect(store.saveState.value).toBe("idle");
    expect(store.unsaved.value).toBe(false);
  });

  it("smartcut store：连续改动只发一次；保存中再改会再存一次，带回来的新版本", async () => {
    const first = deferred<unknown>();
    let n = 0;
    const { store, calls } = await loadedStore((call) => {
      if (call.path === TL) return timelineResp(3);
      if (call.method !== "PUT") return undefined;
      n += 1;
      return n === 1 ? first.promise : saveResp(5);
    });
    store.commit(setVolume(10));
    await vi.advanceTimersByTimeAsync(300);
    store.commit(setVolume(20));
    await vi.advanceTimersByTimeAsync(600);
    expect(puts(calls)).toHaveLength(1);
    expect(store.saveState.value).toBe("saving");
    store.commit(setVolume(30));
    expect(store.saveState.value).toBe("saving");
    first.resolve(saveResp(4));
    await vi.advanceTimersByTimeAsync(0);
    expect(puts(calls)).toHaveLength(2);
    expect(puts(calls)[1]!.body).toMatchObject({ version: 4 });
    expect(videoClipsOf(puts(calls)[1]!.body)[0]).toMatchObject({ volume: 30 });
    expect(store.serverVersion.value).toBe(5);
    expect(store.saveState.value).toBe("idle");
  });

  it("smartcut store：本地改动立即重算落点（拖完后面的段跟着移），不等保存", async () => {
    const { store } = await loadedStore(call => (call.path === TL ? timelineResp(3) : undefined));
    store.commit(setClipOut("21", 1000));
    const clips = store.timeline.value!.tracks[0]!.clips;
    expect(clips[1]!.startMs).toBe(1000);
    expect(store.timeline.value!.totalMs).toBe(4000);
  });

  it("smartcut store：新片段在保存返回前又被改，两次保存后只有一个新片段，撤销栈里的临时 ID 被改写（SC-AC-10）", async () => {
    const first = deferred<unknown>();
    let n = 0;
    const { store, calls } = await loadedStore((call) => {
      if (call.path === TL) return timelineResp(3);
      if (call.method !== "PUT") return undefined;
      n += 1;
      return n === 1 ? first.promise : saveResp(5, [clip("21", "77", 4000), clip("22", "78", 3000), clip("23", "79", 1500)]);
    });
    store.commit(appendClip("tmp-x"));
    store.select({ kind: "clip", ids: ["tmp-x"] });
    await vi.advanceTimersByTimeAsync(600);
    expect(videoClipsOf(puts(calls)[0]!.body).map(c => c.id)).toEqual(["21", "22", "tmp-x"]);
    // 保存返回前用户又改了这段
    store.commit(setClipOut("tmp-x", 1500));
    first.resolve(saveResp(4, [clip("21", "77", 4000), clip("22", "78", 3000), clip("23", "79", 2000)], { "tmp-x": "23" }));
    await vi.advanceTimersByTimeAsync(0);
    expect(puts(calls)).toHaveLength(2);
    const second = videoClipsOf(puts(calls)[1]!.body);
    expect(second.map(c => c.id)).toEqual(["21", "22", "23"]);
    expect(second[2]).toMatchObject({ sourceOutMs: 1500 });
    expect(store.selection.value).toEqual({ kind: "clip", ids: ["23"] });
    for (const snap of store.undoStack.value) expect(snap).not.toContain("tmp-x");
    // 撤销仍然生效：回到改长度之前，ID 是正式的
    expect(store.undo()).toBe(true);
    const clips = store.timeline.value!.tracks[0]!.clips;
    expect(clips.map(c => c.id)).toEqual(["21", "22", "23"]);
    expect(clips[2]).toMatchObject({ sourceOutMs: 2000 });
  });

  it("smartcut store：保存前校验，坏内容不发请求并给出字段路径", async () => {
    const { store, calls } = await loadedStore(call => (call.path === TL ? timelineResp(3) : undefined));
    store.commit(t => ({ ...t, tracks: t.tracks.map(tr => (tr.kind === "video" ? { ...tr, clips: tr.clips.map((c, i) => (i === 1 ? { ...c, speed: 1000 } : c)) } : tr)) }));
    await vi.advanceTimersByTimeAsync(600);
    expect(puts(calls)).toHaveLength(0);
    expect(store.saveState.value).toBe("error");
    expect(store.lastError.value?.path).toBe("tracks[0].clips[1].speed");
  });

  it("smartcut store：撤销与重做各一步，内容逐字段还原", async () => {
    const { store } = await loadedStore(call => (call.method === "PUT" ? saveResp(4) : call.path === TL ? timelineResp(3) : undefined));
    const before = JSON.stringify(store.timeline.value);
    store.commit(setVolume(70));
    expect(store.undo()).toBe(true);
    expect(JSON.stringify(store.timeline.value)).toBe(before);
    expect(store.redo()).toBe(true);
    expect(store.timeline.value!.tracks[0]!.clips[0]!.volume).toBe(70);
    // mutator 原样返回 = 什么都没改：不压空撤销步
    expect(store.commit(t => t)).toBe(false);
  });
});

describe("smartcut store：失败的五种结果", () => {
  it("smartcut store：40902 进冲突并写草稿；载入最新清空撤销栈", async () => {
    let tlVersion = 3;
    const { store, session } = await loadedStore((call) => {
      if (call.method === "PUT") throw conflictError({ currentVersion: 5, currentEditorName: "苏夏", bySelf: false, editorKind: "user" });
      if (call.path === TL) return timelineResp(tlVersion);
      return undefined;
    });
    store.commit(setVolume(50));
    await vi.advanceTimersByTimeAsync(600);
    expect(store.saveState.value).toBe("conflict");
    expect(store.conflict.value).toMatchObject({ currentVersion: 5, currentEditorName: "苏夏", bySelf: false });
    expect(session.has(DRAFT_KEY)).toBe(true);
    // 冲突时再改不发请求，只刷新草稿
    store.commit(setVolume(60));
    await vi.advanceTimersByTimeAsync(600);
    tlVersion = 5;
    expect(await store.resolveConflict("adopt")).toBe("resolved");
    expect(store.serverVersion.value).toBe(5);
    expect(store.undoStack.value).toHaveLength(0);
    expect(store.saveState.value).toBe("idle");
    // 刚写的草稿留给界面询问是否恢复
    expect(store.pendingDraft.value).not.toBeNull();
  });

  it("smartcut store：以我为准取当前版本后重存", async () => {
    let n = 0;
    const { store, calls } = await loadedStore((call) => {
      if (call.path === TL) return timelineResp(3);
      if (call.path === LIST) return { items: [summary(6)] };
      if (call.method === "PUT") {
        n += 1;
        if (n === 1) throw conflictError({ currentVersion: 6, bySelf: true, editorKind: "user" });
        return saveResp(7);
      }
      return undefined;
    });
    store.commit(setVolume(50));
    await vi.advanceTimersByTimeAsync(600);
    expect(store.conflict.value).toMatchObject({ bySelf: true });
    // SC-AC-09：同一用户另一个标签页导致的冲突
    expect(smartcutConflictText(store.conflict.value as never)).toBe("你在另一个窗口修改过这条时间线");
    expect(await store.resolveConflict("overwrite")).toBe("resolved");
    expect(puts(calls)[1]!.body).toMatchObject({ version: 6 });
    expect(store.serverVersion.value).toBe(7);
    expect(store.saveState.value).toBe("idle");
  });

  it("smartcut store：草稿写不进去时拒绝处理冲突", async () => {
    const { store, failDraftWrites } = await loadedStore((call) => {
      if (call.method === "PUT") throw conflictError({ currentVersion: 5 });
      return call.path === TL ? timelineResp(3) : undefined;
    });
    store.commit(setVolume(50));
    await vi.advanceTimersByTimeAsync(600);
    failDraftWrites.value = true;
    await expect(store.resolveConflict("adopt")).rejects.toThrow(/草稿/);
    expect(store.saveState.value).toBe("conflict");
  });

  it("smartcut store：403 进只读并停存，草稿保留", async () => {
    const { store, session, calls } = await loadedStore((call) => {
      if (call.method === "PUT") throw bizError(403, 40301);
      return call.path === TL ? timelineResp(3) : undefined;
    });
    store.commit(setVolume(50));
    await vi.advanceTimersByTimeAsync(600);
    expect(store.readonlyReason.value).toBe("forbidden");
    expect(store.saveState.value).toBe("error");
    expect(session.has(DRAFT_KEY)).toBe(true);
    expect(store.commit(setVolume(60))).toBe(false);
    await vi.advanceTimersByTimeAsync(600);
    expect(puts(calls)).toHaveLength(1);
  });

  it("smartcut store：请求没到服务端进离线；40001 带字段路径；其它错误进 error，都写草稿", async () => {
    let mode: "network" | "invalid" | "server" = "network";
    const { store, session, nav } = await loadedStore((call) => {
      if (call.method === "PUT") {
        if (mode === "network") throw networkError();
        if (mode === "invalid") throw bizError(400, 40001, { path: "subtitles[2].endMs" });
        throw bizError(500, 50000);
      }
      if (call.path === LIST) return { items: [summary(3)] };
      return call.path === TL ? timelineResp(3) : undefined;
    });
    nav.onLine = false;
    store.commit(setVolume(50));
    await vi.advanceTimersByTimeAsync(600);
    expect(store.saveState.value).toBe("offline");
    expect(session.has(DRAFT_KEY)).toBe(true);
    session.clear();

    nav.onLine = true;
    mode = "invalid";
    await store.retry();
    expect(store.saveState.value).toBe("error");
    expect(store.lastError.value).toMatchObject({ code: 40001, path: "subtitles[2].endMs" });
    expect(session.has(DRAFT_KEY)).toBe(true);
    session.clear();

    mode = "server";
    await store.retry();
    expect(store.saveState.value).toBe("error");
    expect(store.lastError.value?.code).toBe(50000);
    expect(session.has(DRAFT_KEY)).toBe(true);
  });
});

describe("smartcut store：离线对账", () => {
  async function offlineStore(serverVersion: number) {
    let failPuts = true;
    let tlGets = 0;
    const env = await loadedStore((call) => {
      if (call.path === LIST) return { items: [summary(serverVersion)] };
      if (call.method === "PUT") {
        if (failPuts) throw networkError();
        return saveResp(4);
      }
      if (call.path !== TL) return undefined;
      // 第一次是载入（版本 3），之后是对账后的「载入最新」（服务端已经到了 serverVersion）
      tlGets += 1;
      return timelineResp(tlGets === 1 ? 3 : serverVersion);
    });
    return { ...env, allowPuts: () => { failPuts = false; } };
  }

  it("smartcut store：恢复后版本没变就补存", async () => {
    const { store, calls, nav, win, allowPuts } = await offlineStore(3);
    store.commit(setVolume(50));
    nav.onLine = false;
    await vi.advanceTimersByTimeAsync(600);
    expect(store.saveState.value).toBe("offline");
    allowPuts();
    nav.onLine = true;
    win.dispatchEvent(new Event("online"));
    await vi.advanceTimersByTimeAsync(0);
    expect(puts(calls).at(-1)!.body).toMatchObject({ version: 3 });
    expect(store.saveState.value).toBe("idle");
    expect(store.serverVersion.value).toBe(4);
  });

  it("smartcut store：恢复后版本变了且本地有改动，直接进冲突（不发注定 409 的请求）", async () => {
    const { store, calls, nav, win } = await offlineStore(5);
    store.commit(setVolume(50));
    nav.onLine = false;
    await vi.advanceTimersByTimeAsync(600);
    const before = puts(calls).length;
    nav.onLine = true;
    win.dispatchEvent(new Event("online"));
    await vi.advanceTimersByTimeAsync(0);
    expect(store.saveState.value).toBe("conflict");
    expect(store.conflict.value).toMatchObject({ currentVersion: 5 });
    expect(puts(calls)).toHaveLength(before);
  });

  it("smartcut store：恢复后版本变了但本地没改动，载入最新", async () => {
    const { store, nav, win } = await offlineStore(5);
    nav.onLine = false;
    win.dispatchEvent(new Event("offline"));
    expect(store.saveState.value).toBe("offline");
    nav.onLine = true;
    win.dispatchEvent(new Event("online"));
    await vi.advanceTimersByTimeAsync(0);
    expect(store.saveState.value).toBe("idle");
    expect(store.serverVersion.value).toBe(5);
  });
});

describe("smartcut store：远端更新", () => {
  const frame = (version: number, extra: Record<string, unknown> = {}) => ({
    type: "timeline.updated", occurredAt: "2026-10-28T03:00:00Z", projectId: "p1", timelineId: "t1", canvasNodeId: "n_sc",
    version, reason: "save", editorKind: "user", editorId: "u2", editorName: "苏夏", ...extra
  });

  it("smartcut store：自己保存的回声不被当成远端更新（保存中先缓存）", async () => {
    const first = deferred<unknown>();
    const { store, calls } = await loadedStore((call) => {
      if (call.method === "PUT") return first.promise;
      return call.path === TL ? timelineResp(3) : undefined;
    });
    store.commit(setVolume(50));
    await vi.advanceTimersByTimeAsync(600);
    // 事件先于响应到达
    store.handleRemoteUpdate(frame(4, { editorId: "u1" }));
    first.resolve(saveResp(4));
    await vi.advanceTimersByTimeAsync(0);
    expect(store.saveState.value).toBe("idle");
    expect(store.remoteNotice.value).toBeNull();
    expect(calls.filter(c => c.path === TL)).toHaveLength(1);
  });

  it("smartcut store：本地有未保存的改动时进冲突", async () => {
    const { store } = await loadedStore(call => (call.path === TL ? timelineResp(3) : undefined));
    store.commit(setVolume(50));
    store.handleRemoteUpdate(frame(4, { editorKind: "agent", editorId: "u1" }));
    await vi.advanceTimersByTimeAsync(0);
    expect(store.saveState.value).toBe("conflict");
    // Agent 也带着本人的 ID，但不能算「你在另一个窗口」
    expect(store.conflict.value).toMatchObject({ bySelf: false, editorKind: "agent" });
  });

  it("smartcut store：没有本地改动时重新取并采纳，清空撤销栈，提示加一", async () => {
    let version = 3;
    const { store } = await loadedStore(call => (call.method === "PUT" ? saveResp(4) : call.path === TL ? timelineResp(version) : undefined));
    store.commit(setVolume(50));
    await vi.advanceTimersByTimeAsync(600);
    expect(store.undoStack.value).toHaveLength(1);
    version = 6;
    store.handleRemoteUpdate(frame(6, { editorKind: "agent", editorName: "" }));
    await vi.advanceTimersByTimeAsync(0);
    expect(store.serverVersion.value).toBe(6);
    expect(store.undoStack.value).toHaveLength(0);
    expect(store.remoteNotice.value).toMatchObject({ version: 6, editorKind: "agent", seq: 1 });
    expect(smartcutRemoteNoticeText(store.remoteNotice.value)).toBe("时间线被 Agent 更新");
    // 版本不比本地新的帧忽略
    store.handleRemoteUpdate(frame(6));
    await vi.advanceTimersByTimeAsync(0);
    expect(store.remoteNotice.value?.seq).toBe(1);
  });
});

describe("smartcut store：AI 写入", () => {
  const aiTimeline = () => timelineResp(4, [clip("22", "78", 3000), clip("21", "77", 2000, { origin: "ai" })], { lastEditorKind: "ai" });

  it("smartcut store：AI 剪辑先 flush 再请求，结果记一步撤销；事件先于响应到达也不当成远端更新", async () => {
    let plan: ((call: Call) => unknown) | null = null;
    const { store, calls } = await loadedStore((call) => {
      if (call.method === "PUT") return saveResp(4);
      if (call.path.endsWith("/plan")) return plan!(call);
      return call.path === TL ? timelineResp(3) : undefined;
    });
    const before = JSON.stringify(store.timeline.value);
    plan = (call) => {
      expect(call.body).toMatchObject({ mode: "mix", variants: 1, baseVersion: 3 });
      // 服务端先推事件再回响应
      store.handleRemoteUpdate({ type: "timeline.updated", timelineId: "t1", version: 4, reason: "plan", editorKind: "ai", editorId: "u1" });
      return { callId: "c1", appliedVariant: 0, variants: [{ index: 0, summary: "混剪", totalMs: 5000, usedClips: ["1", "2"], warnings: [], usedFallback: false }], timeline: aiTimeline(), snapshotId: "s9" };
    };
    await store.runPlan({ mode: "mix", variants: 1 });
    await vi.advanceTimersByTimeAsync(0);
    expect(store.serverVersion.value).toBe(4);
    expect(store.timeline.value!.tracks[0]!.clips.map(c => c.id)).toEqual(["22", "21"]);
    expect(store.undoStack.value).toHaveLength(1);
    expect(store.remoteNotice.value).toBeNull();
    expect(calls.filter(c => c.path === TL)).toHaveLength(1);
    expect(store.aiRound.value).toEqual({ snapshotId: "s9", editedAfter: false });
    // 本地撤销一步就回到 AI 之前（随后照常整份保存）
    store.undo();
    expect(JSON.stringify(store.timeline.value!.tracks)).toBe(JSON.stringify(JSON.parse(before).tracks));
    await vi.advanceTimersByTimeAsync(600);
    expect(puts(calls).at(-1)!.body).toMatchObject({ version: 4 });
  });

  it("smartcut store：撤销这轮 AI 调快照恢复，缺素材的段原样交给界面", async () => {
    const { store, calls } = await loadedStore((call) => {
      if (call.path.endsWith("/plan")) return { callId: "c1", appliedVariant: 0, variants: [], timeline: aiTimeline(), snapshotId: "s9" };
      if (call.path === "/timelines/t1/snapshots/s9/restore") {
        expect(call.body).toEqual({ baseVersion: 4 });
        return { timeline: timelineResp(5), snapshotId: "s10", missing: [{ clipId: "22", mediaId: "78", trackKind: "video" }] };
      }
      return call.path === TL ? timelineResp(3) : undefined;
    });
    await store.runPlan({ mode: "mix", variants: 1 });
    const missing = await store.undoAiRound();
    expect(missing).toEqual([{ clipId: "22", mediaId: "78", trackKind: "video" }]);
    expect(store.serverVersion.value).toBe(5);
    expect(store.plan.value).toBeNull();
    expect(calls.some(c => c.path === "/timelines/t1/snapshots/s9/restore")).toBe(true);
  });

  it("smartcut store：刷新后从快照列表找到最近一次 AI 快照，之后有手动改动时标出来（SC-AC-33）", async () => {
    let items = [
      { id: "s12", version: 6, reason: "replace_changed", createdAt: "2026-10-28T03:00:00Z" },
      { id: "s11", version: 5, reason: "ai_edit", createdAt: "2026-10-28T02:00:00Z" },
      { id: "s10", version: 2, reason: "ai_plan", createdAt: "2026-10-28T01:00:00Z" }
    ];
    const { store } = await loadedStore((call) => {
      if (call.path.endsWith("/snapshots")) return { items };
      return call.path === TL ? timelineResp(8) : undefined;
    });
    // 原位替换这类快照跳过：撤销会把它一起撤掉，所以标「之后改过」让界面先确认。
    expect(store.aiRound.value).toEqual({ snapshotId: "s11", editedAfter: true });
    // 最近一次是「恢复」：这轮 AI 已经撤销（或被别的恢复盖过），不再给「撤销这轮 AI」—— 要再往前去「历史」里挑。
    items = [{ id: "s13", version: 8, reason: "restore", createdAt: "2026-10-28T04:00:00Z" }, ...items];
    await store.refreshSnapshots();
    expect(store.aiRound.value).toBeNull();
  });

  it("smartcut store：撤销这轮 AI 之后，快照列表还没刷新回来时也不能再撤一次", async () => {
    const list = deferred<unknown>();
    const { store } = await loadedStore((call) => {
      if (call.path.endsWith("/plan")) return { callId: "c1", appliedVariant: 0, variants: [], timeline: aiTimeline(), snapshotId: "s9" };
      if (call.path === "/timelines/t1/snapshots/s9/restore") return { timeline: timelineResp(5), snapshotId: "s10", missing: [] };
      if (call.path.endsWith("/snapshots")) return list.promise;
      return call.path === TL ? timelineResp(3) : undefined;
    });
    await store.runPlan({ mode: "mix", variants: 1 });
    store.snapshots.value = [{ id: "s9", version: 3, reason: "ai_plan", createdAt: "2026-10-28T01:00:00Z" }] as never;
    await store.undoAiRound();
    expect(store.aiRound.value).toBeNull();
  });

  it("smartcut store：AI 结果到达时本地有还没存上的改动，进冲突而不是覆盖", async () => {
    const gate = deferred<unknown>();
    const { store } = await loadedStore((call) => {
      if (call.path.endsWith("/plan")) return gate.promise;
      return call.path === TL ? timelineResp(3) : undefined;
    });
    const running = store.runPlan({ mode: "mix", variants: 1 });
    await vi.advanceTimersByTimeAsync(0);
    store.commit(setVolume(5));
    gate.resolve({ callId: "c1", variants: [], timeline: aiTimeline(), snapshotId: "s9" });
    await running;
    expect(store.saveState.value).toBe("conflict");
    expect(store.timeline.value!.tracks[0]!.clips[0]!.volume).toBe(5);
  });
});

describe("smartcut store：AI 剪辑的交互（SCT-31）", () => {
  type AiStore = Store & {
    planRunning: Ref<null | { kind: string; stage: string; callId: string }>;
    planConflict: Ref<null | { kind: string; callId: string; variant: number; details: Record<string, unknown> }>;
    lastPlanRequest: Ref<null | Record<string, unknown>>;
    handleSmartcutFrame: (frame: Record<string, unknown>) => void;
    runEdit: (instruction: string) => Promise<unknown>;
    applyVariant: (variant: number, force?: boolean) => Promise<unknown>;
    forceApply: () => Promise<unknown>;
    replaceChanged: () => Promise<{ replacedClipIds: string[] }>;
  };
  const aiTimeline = (version = 4) => timelineResp(version, [clip("22", "78", 3000), clip("21", "77", 2000, { origin: "ai" })], { lastEditorKind: "ai" });
  const variants = (n: number) => Array.from({ length: n }, (_, index) => ({ index, status: "success", summary: "", totalMs: 5000, usedClips: ["1"], warnings: [], usedFallback: false }));
  const planResult = (version = 4, n = 1, extra: Record<string, unknown> = {}) => ({ callId: "c1", appliedVariant: 0, variants: variants(n), timeline: aiTimeline(version), snapshotId: "s9", ...extra });

  it("smartcut store：进行中有阶段；第一帧事件认下 callId，同一时间线上别的调用不串进来；回来后收起", async () => {
    const gate = deferred<unknown>();
    const { store } = await loadedStore((call) => {
      if (call.path.endsWith("/plan")) return gate.promise;
      return call.path === TL ? timelineResp(3) : undefined;
    });
    const s = store as unknown as AiStore;
    const running = store.runPlan({ mode: "board", variants: 1, spec: { targetSec: 15 } });
    await vi.advanceTimersByTimeAsync(0);
    expect(s.planRunning.value).toMatchObject({ kind: "plan", stage: "sync", callId: "" });
    await expect(store.runPlan({ mode: "mix", variants: 1 })).rejects.toThrow(/进行中/);
    s.handleSmartcutFrame({ type: "smartcut.updated", timelineId: "t1", callId: "c1", stage: "understand", status: "running" });
    expect(s.planRunning.value).toMatchObject({ callId: "c1", stage: "understand" });
    s.handleSmartcutFrame({ type: "smartcut.updated", timelineId: "t1", callId: "c2", stage: "apply", status: "running" });
    s.handleSmartcutFrame({ type: "smartcut.updated", timelineId: "other", callId: "c1", stage: "apply", status: "running" });
    expect(s.planRunning.value!.stage).toBe("understand");
    s.handleSmartcutFrame({ type: "smartcut.updated", timelineId: "t1", callId: "c1", stage: "plan", status: "running" });
    expect(s.planRunning.value!.stage).toBe("plan");
    gate.resolve(planResult());
    await running;
    expect(s.planRunning.value).toBeNull();
    expect(store.plan.value).toMatchObject({ kind: "plan", mode: "board", callId: "c1", snapshotId: "s9" });
    expect(s.lastPlanRequest.value).toEqual({ mode: "board", variants: 1, spec: { targetSec: 15 } });
  });

  it("smartcut store：剪辑期间被改（40902 带 callId）记下来；仍然应用带 force 写入并记一步撤销", async () => {
    const { store, calls } = await loadedStore((call) => {
      if (call.path.endsWith("/plan")) throw conflictError({ currentVersion: 5, editorKind: "user", currentEditorName: "小王", callId: "c9" });
      if (call.path === "/projects/p1/smartcuts/t1/plan/c9/apply") return planResult(6, 1, { callId: "c9", snapshotId: "s3" });
      return call.path === TL ? timelineResp(3) : undefined;
    });
    const s = store as unknown as AiStore;
    await expect(store.runPlan({ mode: "mix", variants: 1 })).rejects.toMatchObject({ statusCode: 409 });
    expect(s.planRunning.value).toBeNull();
    expect(s.planConflict.value).toMatchObject({ kind: "plan", callId: "c9", variant: 0, details: { currentVersion: 5, currentEditorName: "小王" } });
    await s.forceApply();
    expect(calls.find(c => c.path.endsWith("/plan/c9/apply"))!.body).toEqual({ variant: 0, baseVersion: 5, force: true });
    expect(store.serverVersion.value).toBe(6);
    expect(store.undoStack.value).toHaveLength(1);
    expect(s.planConflict.value).toBeNull();
    expect(store.plan.value).toMatchObject({ callId: "c9", kind: "plan", mode: "mix", snapshotId: "s3" });
  });

  it("smartcut store：没带 callId 的冲突不给「仍然应用」（只能重新剪）", async () => {
    const { store } = await loadedStore((call) => {
      if (call.path.endsWith("/plan")) throw conflictError({ currentVersion: 5 });
      return call.path === TL ? timelineResp(3) : undefined;
    });
    const s = store as unknown as AiStore;
    await expect(store.runPlan({ mode: "mix", variants: 1 })).rejects.toBeTruthy();
    expect(s.planConflict.value).toBeNull();
    await expect(s.forceApply()).rejects.toThrow(/没有待应用/);
  });

  it("smartcut store：追加指令走 /edit，结果记一步撤销、标成追加修改；空指令不发请求", async () => {
    const { store, calls } = await loadedStore((call) => {
      if (call.path.endsWith("/edit")) {
        expect(call.body).toEqual({ instruction: "第三段换短一点", baseVersion: 3 });
        return planResult(4, 1, { changes: ["第 3 段：只取前 60%（4 秒 → 2.4 秒）"] });
      }
      return call.path === TL ? timelineResp(3) : undefined;
    });
    const s = store as unknown as AiStore;
    await expect(s.runEdit("   ")).rejects.toThrow(/先写下/);
    expect(calls.some(c => c.path.endsWith("/edit"))).toBe(false);
    await s.runEdit("  第三段换短一点 ");
    expect(store.plan.value).toMatchObject({ kind: "edit", changes: ["第 3 段：只取前 60%（4 秒 → 2.4 秒）"] });
    expect(store.undoStack.value).toHaveLength(1);
    expect(store.aiRound.value).toEqual({ snapshotId: "s9", editedAfter: false });
  });

  it("smartcut store：切换方案撞上冲突时记下要切的那一套，仍然应用写回同一套", async () => {
    const { store, calls } = await loadedStore((call) => {
      if (call.path.endsWith("/plan")) return planResult(4, 3);
      if (call.path === "/projects/p1/smartcuts/t1/plan/c1/apply") {
        const body = call.body as { force?: boolean };
        if (!body.force) throw conflictError({ currentVersion: 6, editorKind: "agent", callId: "c1" });
        return planResult(7, 3, { appliedVariant: 2 });
      }
      return call.path === TL ? timelineResp(3) : undefined;
    });
    const s = store as unknown as AiStore;
    await store.runPlan({ mode: "mix", variants: 3 });
    await expect(s.applyVariant(2)).rejects.toBeTruthy();
    expect(s.planConflict.value).toMatchObject({ kind: "variant", variant: 2 });
    await s.forceApply();
    expect(calls.filter(c => c.path.endsWith("/plan/c1/apply")).at(-1)!.body).toEqual({ variant: 2, baseVersion: 6, force: true });
    expect(store.plan.value).toMatchObject({ appliedVariant: 2, kind: "plan", mode: "mix" });
  });

  it("smartcut store：只替换变了的那几段：带版本同步，取回时间线记一步撤销，期间的回声不当成远端更新", async () => {
    let gets = 0;
    const { store, calls } = await loadedStore((call) => {
      if (call.path === TL) return ++gets === 1 ? timelineResp(3) : aiTimeline(4);
      if (call.path.endsWith("/sync")) {
        const body = call.body as { mode: string; baseVersion?: number };
        if (body.mode !== "replaceChanged") return { inputs: [], appendedClipIds: [], disconnectedClipIds: [], replacedClipIds: [], unusedCount: 0, fingerprint: "f0", timelineVersion: 3 };
        expect(body.baseVersion).toBe(3);
        store.handleRemoteUpdate({ type: "timeline.updated", timelineId: "t1", version: 4, reason: "sync", editorKind: "user", editorId: "u1" });
        return { inputs: [{ no: "1", sourceNodeId: "n1", sourceNodeType: "video", kind: "video", status: "ready", used: true, changed: false }],
          appendedClipIds: [], disconnectedClipIds: [], replacedClipIds: ["21"], shortened: [{ clipId: "21", fromMs: 4000, toMs: 2000 }],
          unusedCount: 0, fingerprint: "f1", timelineVersion: 4 };
      }
      return undefined;
    });
    const s = store as unknown as AiStore;
    const res = await s.replaceChanged();
    await vi.advanceTimersByTimeAsync(0);
    expect(res.replacedClipIds).toEqual(["21"]);
    expect(store.serverVersion.value).toBe(4);
    expect(store.undoStack.value).toHaveLength(1);
    expect(store.remoteNotice.value).toBeNull();
    expect(calls.filter(c => c.path === TL)).toHaveLength(2);
    expect(store.inputs.value).toHaveLength(1);
  });
});

describe("smartcut store：只读", () => {
  it("smartcut store：viewer 打开时 commit 被拒绝，不发保存请求（SC-AC-66）", async () => {
    const { store, calls } = await loadedStore(call => (call.path === TL ? timelineResp(3) : undefined), { role: "viewer" });
    expect(store.readonly.value).toBe(true);
    expect(store.readonlyReason.value).toBe("role");
    expect(store.commit(setVolume(50))).toBe(false);
    await vi.advanceTimersByTimeAsync(1000);
    expect(puts(calls)).toHaveLength(0);
    await expect(store.runPlan({ mode: "mix", variants: 1 })).rejects.toThrow(/只读/);
  });
});

describe("smartcut store：跨过一次保存的操作", () => {
  it("smartcut store：临时 ID 保存后仍能换算成正式 ID（拖动中途保存完成的情形）", async () => {
    const { store } = await loadedStore((call) => {
      if (call.method === "PUT") return saveResp(4, [clip("21", "77", 4000), clip("22", "78", 3000), clip("23", "79", 2000)], { "tmp-x": "23" });
      return call.path === TL ? timelineResp(3) : undefined;
    });
    store.commit(appendClip("tmp-x"));
    await vi.advanceTimersByTimeAsync(600);
    const s = store as unknown as { currentId: (id: string) => string };
    expect(s.currentId("tmp-x")).toBe("23");
    expect(s.currentId("21")).toBe("21");
  });
});

describe("smartcut store：识别字幕与翻译（SCT-36）", () => {
  type SubStore = Store & {
    quoteTranscribe: (opts: Record<string, unknown>) => Promise<unknown>;
    transcribe: (opts: Record<string, unknown>, key: string) => Promise<{ items: unknown[] }>;
    translate: (opts: { subtitleTrackId: string; targetLang: string; mode: "newTrack" | "replace" }) => Promise<{ translated: number }>;
    generations: Ref<Record<string, unknown>>;
  };
  const translated = (version: number) => timelineResp(version, undefined, {
    subtitles: [{ id: "91", trackId: "14", startMs: 0, endMs: 1000, text: "Hello" }],
    tracks: [...timelineResp(version).tracks, { id: "14", kind: "subtitle", idx: 4, isMuted: false, volume: 100, lang: "en", clips: [] }]
  });

  it("smartcut store：翻译先存再翻、带刚存上的版本；写好的时间线采纳并记一步撤销；一条都没译出来时什么都不动", async () => {
    let fail = false;
    const { store, calls } = await loadedStore((call) => {
      if (call.path.endsWith("/translate")) {
        return fail ? { trackId: "13", translated: 0, failed: [{ index: 1, reason: "AI 的译文无法解析" }] }
          : { trackId: "14", translated: 1, failed: [], timeline: translated(4) };
      }
      return call.path === TL ? timelineResp(3) : undefined;
    });
    const s = store as unknown as SubStore;
    const res = await s.translate({ subtitleTrackId: "13", targetLang: "en", mode: "newTrack" });
    expect(res.translated).toBe(1);
    expect(calls.find(c => c.path.endsWith("/translate"))!.body).toEqual({ subtitleTrackId: "13", targetLang: "en", mode: "newTrack", baseVersion: 3 });
    expect(store.serverVersion.value).toBe(4);
    expect(store.undoStack.value).toHaveLength(1);
    expect(store.timeline.value!.subtitles.map(x => x.text)).toEqual(["Hello"]);
    fail = true;
    await s.translate({ subtitleTrackId: "13", targetLang: "ja", mode: "replace" });
    expect(store.serverVersion.value).toBe(4);
    expect(store.undoStack.value).toHaveLength(1);
  });

  it("smartcut store：识别报价带 dryRun；受理带 Idempotency-Key、不带 dryRun，受理的任务记进进度", async () => {
    const { store, calls } = await loadedStore((call) => {
      if (call.path.endsWith("/transcribe")) {
        const body = call.body as { dryRun?: boolean };
        if (body.dryRun) return { credits: 20, modelCode: "volc", blockers: [], items: 1, durationSec: 10 };
        return { items: [{ generationId: "g1", kind: "asr", status: "queued", progress: 0, credits: 20, createdAt: "" }], waiting: [], skipped: [], failed: [] };
      }
      return call.path === TL ? timelineResp(3) : undefined;
    });
    const s = store as unknown as SubStore;
    const opts = { modelCode: "volc", keepFillers: false, subtitles: true, clipIds: ["21"] };
    await s.quoteTranscribe(opts);
    await s.transcribe(opts, "smartcut-asr-k1");
    const [quote, accept] = calls.filter(c => c.path === "/projects/p1/smartcuts/t1/transcribe");
    expect(quote!.body).toEqual({ clipIds: ["21"], modelCode: "volc", keepFillers: false, subtitles: true, dryRun: true });
    expect(accept!.body).toEqual({ clipIds: ["21"], modelCode: "volc", keepFillers: false, subtitles: true });
    expect(accept!.headers).toEqual({ "Idempotency-Key": "smartcut-asr-k1" });
    expect(s.generations.value.g1).toMatchObject({ kind: "asr", status: "queued" });
  });
});

// ---------------------------------------------------------------- 片段级生成（SCT-40）

type Generation = components["schemas"]["SmartcutGeneration"];
type ClipStore = Store & {
  generations: Ref<Record<string, Generation>>;
  generationsCursor: Ref<string | null>;
  clipRequests: Ref<Record<string, unknown>>;
  quoteClipTool: (req: Record<string, unknown>) => Promise<unknown>;
  runClipTool: (req: Record<string, unknown>, key: string) => Promise<Generation>;
  handleGenerationFrame: (frame: Record<string, unknown>) => void;
  refreshGenerations: (more?: boolean) => Promise<void>;
  placeGeneration: (generationId: string, atMs: number) => Promise<string>;
};

const REGEN = "/projects/p1/smartcuts/t1/clips/21/regenerate";
const GEN_LIST = "/projects/p1/smartcuts/t1/generations";
const regenGen = (extra: Partial<Generation> = {}): Generation => ({
  generationId: "g1", kind: "regen", status: "queued", progress: 0, credits: 15, clipId: "21", mediaId: "77",
  createdAt: "2026-10-28T10:00:00Z", ...extra
});

describe("smartcut store：片段级生成", () => {
  it("smartcut store：报价带 dryRun；受理带 Idempotency-Key、不带 dryRun，片段进入生成中（不进撤销栈、不保存）", async () => {
    const { store, calls } = await loadedStore((call) => {
      if (call.path === REGEN) return (call.body as { dryRun?: boolean }).dryRun ? { credits: 15, modelCode: "vid", blockers: [], items: 1 } : regenGen();
      return call.path === TL ? timelineResp(3) : undefined;
    });
    const s = store as unknown as ClipStore;
    await s.quoteClipTool({ tool: "regenerate", clipId: "21", body: { prompt: "换成黄昏" } });
    const gen = await s.runClipTool({ tool: "regenerate", clipId: "21", body: { prompt: "换成黄昏" } }, "smartcut-regenerate-k1");
    const [quote, accept] = calls.filter(c => c.path === REGEN);
    expect(quote!.body).toEqual({ prompt: "换成黄昏", dryRun: true });
    expect(accept!.body).toEqual({ prompt: "换成黄昏" });
    expect(accept!.headers).toEqual({ "Idempotency-Key": "smartcut-regenerate-k1" });
    expect(gen.generationId).toBe("g1");
    expect(s.generations.value.g1).toMatchObject({ status: "queued", clipId: "21" });
    expect(s.clipRequests.value.g1).toMatchObject({ tool: "regenerate", clipId: "21" });
    expect(store.timeline.value!.tracks[0]!.clips[0]!.pendingGenerationId).toBe("g1");
    // 「生成中」只是本地的标记：不是一次编辑
    expect(store.undoStack.value).toHaveLength(0);
    await vi.advanceTimersByTimeAsync(2000);
    expect(puts(calls)).toHaveLength(0);
  });

  it("smartcut store：generation.update 只认记录里的任务；失败清掉生成中，晚到的非终态事件不改回去", async () => {
    const { store } = await loadedStore((call) => {
      if (call.path === REGEN) return regenGen();
      return call.path === TL ? timelineResp(3) : undefined;
    });
    const s = store as unknown as ClipStore;
    await s.runClipTool({ tool: "regenerate", clipId: "21", body: { prompt: "x" } }, "k-1");
    // 别的任务（画布节点上的生成）不认
    s.handleGenerationFrame({ type: "generation.update", generationId: "g9", status: "running", progress: 50 });
    expect(Object.keys(s.generations.value)).toEqual(["g1"]);
    s.handleGenerationFrame({ type: "generation.update", generationId: "g1", status: "running", progress: 40 });
    expect(s.generations.value.g1).toMatchObject({ status: "running", progress: 40 });
    s.handleGenerationFrame({ type: "generation.update", generationId: "g1", status: "failed", progress: 40,
      error: { code: "provider_rejected", message: "上游拒绝：提示词违规" } });
    expect(s.generations.value.g1).toMatchObject({ status: "failed", error: "上游拒绝：提示词违规" });
    expect(store.timeline.value!.tracks[0]!.clips[0]!.pendingGenerationId).toBeUndefined();
    s.handleGenerationFrame({ type: "generation.update", generationId: "g1", status: "running", progress: 60 });
    expect(s.generations.value.g1!.status).toBe("failed");
  });

  it("smartcut store：回填后的 timeline.updated 让本地重取时间线（不在前端自己替换片段）", async () => {
    let version = 3;
    const { store, calls } = await loadedStore((call) => {
      if (call.path === REGEN) return regenGen();
      if (call.path === TL) return version === 3 ? timelineResp(3) : timelineResp(4, [clip("21", "99", 4000), clip("22", "78", 3000)]);
      return undefined;
    });
    const s = store as unknown as ClipStore;
    await s.runClipTool({ tool: "regenerate", clipId: "21", body: { prompt: "x" } }, "k-1");
    s.handleGenerationFrame({ type: "generation.update", generationId: "g1", status: "success", progress: 100 });
    // 成功本身不改片段：等服务端回填
    expect(store.timeline.value!.tracks[0]!.clips[0]!.mediaId).toBe("77");
    version = 4;
    store.handleRemoteUpdate({ type: "timeline.updated", timelineId: "t1", version: 4, editorKind: "system", reason: "clip_fill" });
    await vi.advanceTimersByTimeAsync(0);
    expect(calls.filter(c => c.path === TL)).toHaveLength(2);
    expect(store.timeline.value!.tracks[0]!.clips[0]!.mediaId).toBe("99");
    expect(store.serverVersion.value).toBe(4);
  });

  it("smartcut store：生成记录按游标翻页；产物放进时间线是一次编辑", async () => {
    const { store, calls } = await loadedStore((call) => {
      if (call.path.startsWith(GEN_LIST)) {
        return call.path.includes("cursor=")
          ? { items: [{ generationId: "g0", kind: "fill", status: "success", progress: 100, credits: 15, createdAt: "2026-10-28T09:00:00Z", outputMediaId: "m7", landed: false }] }
          : { items: [{ generationId: "g2", kind: "music", status: "success", progress: 100, credits: 30, createdAt: "2026-10-28T11:00:00Z", outputMediaId: "m8", landed: false }], nextCursor: "c1" };
      }
      if (call.path === "/media/m8/info") return { id: "m8", mime: "audio/mpeg", kind: "audio", sizeBytes: 1, width: 0, height: 0, durationMs: 5000 };
      return call.path === TL ? timelineResp(3) : undefined;
    });
    const s = store as unknown as ClipStore;
    // 载入时取过第一页
    expect(s.generations.value.g2).toMatchObject({ kind: "music", landed: false });
    expect(s.generationsCursor.value).toBe("c1");
    await s.refreshGenerations(true);
    expect(calls.some(c => c.path === `${GEN_LIST}?limit=30&cursor=c1`)).toBe(true);
    expect(s.generations.value.g0).toBeDefined();
    expect(s.generationsCursor.value).toBe("");
    expect(await s.placeGeneration("g2", 1000)).toBe("");
    const music = store.timeline.value!.tracks.find(t => t.kind === "audio" && t.role === "music")!;
    expect(music.clips).toHaveLength(1);
    expect(music.clips[0]).toMatchObject({ mediaId: "m8", startMs: 1000, sourceOutMs: 5000, origin: "user" });
    expect(store.undoStack.value).toHaveLength(1);
    expect(await s.placeGeneration("nope", 0)).toContain("没有可以放进时间线的产物");
  });
});
