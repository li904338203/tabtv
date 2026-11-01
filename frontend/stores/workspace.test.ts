import { computed, ref } from "vue";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

/**
 * 工作区列表的拉取（会话修复）。vitest 里没有 Nuxt / Pinia 的自动导入，按 store 用到的几个全局函数注入替身。
 *
 * ⚠️ 两条都是「错了也不报错」的：
 *  - 页面一次发出好几个请求时都会来等工作区，没有单飞就是好几个重复的 /workspaces；
 *  - 拉取途中换了账号（clear），回来的是上一个账号的列表 —— 用上它，每个请求都带着别人的工作区 ID（40302）。
 */
type Pending = { resolve: (items: unknown[]) => void };

function setup() {
  const pending: Pending[] = [];
  const store = new Map<string, string>();
  vi.stubGlobal("ref", ref);
  vi.stubGlobal("computed", computed);
  vi.stubGlobal("defineStore", (_id: string, setupFn: () => unknown) => () => setupFn());
  vi.stubGlobal("useRuntimeConfig", () => ({ public: { apiBase: "/api/v1" } }));
  vi.stubGlobal("useAuthStore", () => ({
    accessToken: "A1",
    withTokenRefresh: (send: (token: string | null) => Promise<unknown>) => send("A1"),
  }));
  vi.stubGlobal("$fetch", {
    raw: () => new Promise((resolve) => {
      pending.push({ resolve: (items) => resolve({ ok: true, status: 200, _data: items }) });
    }),
  });
  vi.stubGlobal("localStorage", {
    getItem: (k: string) => store.get(k) ?? null,
    setItem: (k: string, v: string) => { store.set(k, v); },
    removeItem: (k: string) => { store.delete(k); },
  });
  return { pending };
}

const personal = (id: string) => ({ id, name: "个人空间", kind: "personal", role: "owner", payerAccountId: "1" });

beforeEach(() => vi.resetModules());
afterEach(() => vi.unstubAllGlobals());

describe("工作区列表的拉取", () => {
  it("几个请求同时来等：只拉一次", async () => {
    const { pending } = setup();
    const { useWorkspaceStore } = await import("./workspace");
    const ws = useWorkspaceStore() as unknown as { load: () => Promise<void>; currentId: { value: string } };
    const all = Promise.all([ws.load(), ws.load(), ws.load()]);
    expect(pending).toHaveLength(1);
    pending[0].resolve([personal("w-1")]);
    await all;
    expect(ws.currentId.value).toBe("w-1");
    await ws.load(); // 已经拉到了：不再请求
    expect(pending).toHaveLength(1);
  });

  it("拉取途中换了账号：上一个账号的列表丢掉，重新拉", async () => {
    const { pending } = setup();
    const { useWorkspaceStore } = await import("./workspace");
    const ws = useWorkspaceStore() as unknown as { load: () => Promise<void>; clear: () => void; currentId: { value: string }; loaded: { value: boolean } };
    const stale = ws.load();
    ws.clear(); // 换账号
    const fresh = ws.load(); // ⚠️ 不能复用上一个账号那次进行中的请求
    expect(pending).toHaveLength(2);
    pending[0].resolve([personal("old-account")]);
    await stale;
    expect(ws.currentId.value).toBe("");
    expect(ws.loaded.value).toBe(false);
    pending[1].resolve([personal("new-account")]);
    await fresh;
    expect(ws.currentId.value).toBe("new-account");
  });
});
