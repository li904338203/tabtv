import { describe, expect, it, vi } from "vitest";
import { BLOB_CACHE_DEFAULT_MAX_BYTES, BlobCache } from "./BlobCache";

function blob(bytes: number): Blob {
  return new Blob([new Uint8Array(bytes)]);
}

/** URL 用计数器造（测试环境没有浏览器的 createObjectURL），revoke 记下来。 */
function harness(maxBytes = 100) {
  let n = 0;
  const revoked: string[] = [];
  const cache = new BlobCache({
    maxBytes,
    createObjectURL: () => `blob:${++n}`,
    revokeObjectURL: url => revoked.push(url)
  });
  return { cache, revoked };
}

function deferred<T>() {
  let resolve!: (v: T) => void;
  let reject!: (e: unknown) => void;
  const promise = new Promise<T>((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, resolve, reject };
}

describe("BlobCache", () => {
  it("缺省上限 300 MB（SC-PARAM-23）", () => {
    expect(new BlobCache().maxBytes).toBe(BLOB_CACHE_DEFAULT_MAX_BYTES);
    expect(BLOB_CACHE_DEFAULT_MAX_BYTES).toBe(300 * 1024 * 1024);
  });

  it("超上限从最久没用过的开始淘汰，淘汰时 revoke", () => {
    const { cache, revoked } = harness(100);
    const a = cache.put("a", blob(40));
    const b = cache.put("b", blob(40));
    cache.get("a"); // a 刚用过，b 成了最久没用的
    cache.put("c", blob(40));
    expect(cache.has("a")).toBe(true);
    expect(cache.has("b")).toBe(false);
    expect(revoked).toEqual([b.objectUrl]);
    expect(cache.bytes).toBe(80);
    expect(a.objectUrl).toBe("blob:1");
  });

  it("被 pin 的不淘汰；全部取消 pin 之后才可淘汰（取消函数只减一次）", () => {
    const { cache, revoked } = harness(100);
    const a = cache.put("a", blob(60));
    const unpin1 = cache.pin("a");
    const unpin2 = cache.pin("a");
    cache.put("b", blob(60)); // 超上限，但 a 被 pin、b 是刚放进来的
    expect(cache.has("a") && cache.has("b")).toBe(true);
    unpin1();
    unpin1();
    expect(cache.has("a")).toBe(true);
    unpin2();
    expect(cache.has("a")).toBe(false);
    expect(revoked).toEqual([a.objectUrl]);
    expect(cache.bytes).toBe(60);
    expect(cache.pin("missing")).toBeTypeOf("function");
  });

  it("单独一项就超上限时也留着（正要播的不能刚取回来就扔），下一项进来时被挤掉", () => {
    const { cache } = harness(100);
    cache.put("big", blob(150));
    expect(cache.has("big")).toBe(true);
    cache.put("small", blob(10));
    expect(cache.has("big")).toBe(false);
  });

  it("同一个键并发加载单飞：loader 只调一次，两处拿到同一份", async () => {
    const { cache } = harness();
    const d = deferred<Blob | null>();
    const loader = vi.fn(() => d.promise);
    const p1 = cache.load("k", loader);
    const p2 = cache.load("k", loader);
    expect(cache.isLoading("k")).toBe(true);
    d.resolve(blob(10));
    const [e1, e2] = await Promise.all([p1, p2]);
    expect(loader).toHaveBeenCalledTimes(1);
    expect(e1).toBe(e2);
    expect(cache.isLoading("k")).toBe(false);
    expect(await cache.load("k", loader)).toBe(e1);
    expect(loader).toHaveBeenCalledTimes(1);
  });

  it("loader 回 null（预览版还没好）不缓存，下次重新取", async () => {
    const { cache } = harness();
    const loader = vi.fn(async () => null);
    expect(await cache.load("k", loader)).toBeNull();
    expect(cache.has("k")).toBe(false);
    expect(cache.isLoading("k")).toBe(false);
    await cache.load("k", loader);
    expect(loader).toHaveBeenCalledTimes(2);
  });

  it("加载失败（含同步抛错）不缓存，原样抛出，下次重新取", async () => {
    const { cache } = harness();
    const boom = vi.fn((): Promise<Blob | null> => {
      throw new Error("网络断了");
    });
    await expect(cache.load("k", boom)).rejects.toThrow("网络断了");
    expect(cache.isLoading("k")).toBe(false);
    expect(cache.has("k")).toBe(false);
    const ok = await cache.load("k", async () => blob(5));
    expect(ok?.bytes).toBe(5);
  });

  it("clear 全部 revoke；clear 之前发出的加载回来后不再入缓存", async () => {
    const { cache, revoked } = harness();
    const a = cache.put("a", blob(10));
    const b = cache.put("b", blob(10));
    const d = deferred<Blob | null>();
    const late = cache.load("late", () => d.promise);
    cache.clear();
    expect(revoked).toEqual([a.objectUrl, b.objectUrl]);
    expect(cache.size).toBe(0);
    expect(cache.bytes).toBe(0);
    d.resolve(blob(10));
    expect(await late).toBeNull();
    expect(cache.has("late")).toBe(false);
  });

  it("remove 删除并 revoke；put 同键替换旧的", () => {
    const { cache, revoked } = harness();
    const a1 = cache.put("a", blob(10));
    const a2 = cache.put("a", blob(20));
    expect(revoked).toEqual([a1.objectUrl]);
    expect(cache.bytes).toBe(20);
    expect(cache.remove("a")).toBe(true);
    expect(cache.remove("a")).toBe(false);
    expect(revoked).toEqual([a1.objectUrl, a2.objectUrl]);
  });
});
