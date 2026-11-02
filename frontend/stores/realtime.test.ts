// @vitest-environment jsdom
import { computed, reactive, ref } from "vue";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

/**
 * 实时事件 store（TT-T-11）：事件经宿主的 Wails 事件通道送来（`window.runtime.EventsOn`），
 * 每条是 `{topic, payload}`。对外接口与来源系统一致。
 */
type Listener = (...data: unknown[]) => void;

function installHost() {
  const listeners = new Map<string, Set<Listener>>();
  (window as unknown as { runtime: unknown }).runtime = {
    EventsOn(name: string, cb: Listener) {
      if (!listeners.has(name)) listeners.set(name, new Set());
      listeners.get(name)!.add(cb);
      return () => listeners.get(name)!.delete(cb);
    }
  };
  return {
    emit(topic: string, payload: unknown) {
      for (const cb of listeners.get("tabtv:event") ?? []) cb({ topic, payload });
    },
    count: () => listeners.get("tabtv:event")?.size ?? 0
  };
}

async function load() {
  const instances = new Map<string, unknown>();
  vi.stubGlobal("ref", ref);
  vi.stubGlobal("computed", computed);
  vi.stubGlobal("defineStore", (id: string, setupFn: () => Record<string, unknown>) => () => {
    if (!instances.has(id)) instances.set(id, reactive(setupFn()));
    return instances.get(id);
  });
  const { useRealtimeStore } = await import("./realtime");
  return useRealtimeStore() as unknown as {
    state: string;
    isSubscribed: (t: string) => boolean;
    subscribe: (t: string) => () => void;
    on: (type: string, h: (f: Record<string, unknown>) => void) => () => void;
    onReconnected: (h: () => void) => () => void;
    connect: () => void;
    disconnect: () => void;
  };
}

beforeEach(() => vi.resetModules());
afterEach(() => {
  vi.unstubAllGlobals();
  delete (window as unknown as { runtime?: unknown }).runtime;
});

describe("实时事件 store", () => {
  it("只分发订了的 topic，按 type 分发，off 后不再收到", async () => {
    const host = installHost();
    const rt = await load();
    rt.connect();
    const got: unknown[] = [];
    const off = rt.on("generation.update", f => got.push(f.generationId));
    const release = rt.subscribe("workspace:1");
    host.emit("workspace:1", { type: "generation.update", generationId: "g1" });
    host.emit("workspace:2", { type: "generation.update", generationId: "g2" }); // 没订：丢
    host.emit("workspace:1", { type: "other", generationId: "g3" }); // 没人关心的 type：丢
    expect(got).toEqual(["g1"]);
    off();
    host.emit("workspace:1", { type: "generation.update", generationId: "g4" });
    expect(got).toEqual(["g1"]);
    release();
  });

  it("引用计数：最后一个释放才退订；release 重复调用只生效一次", async () => {
    const host = installHost();
    const rt = await load();
    rt.connect();
    const got: unknown[] = [];
    rt.on("x", f => got.push(f.n));
    const a = rt.subscribe("project:7");
    const b = rt.subscribe("project:7");
    a();
    a();
    host.emit("project:7", { type: "x", n: 1 });
    expect(got).toEqual([1]);
    b();
    host.emit("project:7", { type: "x", n: 2 });
    expect(got).toEqual([1]);
    expect(rt.isSubscribed("project:7")).toBe(false);
  });

  it("连上之后触发一次 onReconnected（订阅方据此对账），状态为 online", async () => {
    installHost();
    const rt = await load();
    const calls = vi.fn();
    rt.onReconnected(calls);
    rt.subscribe("workspace:1");
    rt.connect();
    await Promise.resolve();
    expect(calls).toHaveBeenCalledTimes(1);
    expect(rt.state).toBe("online");
    rt.connect(); // 重复 connect 不重复挂监听
    await Promise.resolve();
    expect(calls).toHaveBeenCalledTimes(1);
  });

  it("一个处理函数抛错不打断其余处理函数", async () => {
    const host = installHost();
    const rt = await load();
    rt.connect();
    rt.subscribe("workspace:1");
    const ok = vi.fn();
    rt.on("t", () => {
      throw new Error("boom");
    });
    rt.on("t", ok);
    host.emit("workspace:1", { type: "t" });
    expect(ok).toHaveBeenCalled();
  });

  it("没有宿主（浏览器直开开发服务器）时保持 offline，订阅方靠轮询", async () => {
    const rt = await load();
    rt.subscribe("workspace:1");
    rt.connect();
    expect(rt.state).toBe("offline");
    expect(rt.isSubscribed("workspace:1")).toBe(false);
  });

  it("disconnect 摘掉宿主监听", async () => {
    const host = installHost();
    const rt = await load();
    rt.connect();
    expect(host.count()).toBe(1);
    rt.disconnect();
    expect(host.count()).toBe(0);
    expect(rt.state).toBe("offline");
  });
});
