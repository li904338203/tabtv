/**
 * 预览素材的 blob 缓存（docs/agent/smartcut SCT-21，SCD-42、SC-PARAM-23）：LRU，按**字节**计，上限默认 300 MB。
 *
 * 为什么要自己管：媒体端点没有 Range，只能整份取成 blob 再用 object URL 播（`useAuthenticatedMedia` 同理）；
 * 预览版虽然只有 480p，一个两百段的时间线全留在内存里也有几个 GB。所以只缓存预览版、音频与排版图，
 * 超上限从最久没用过的开始淘汰，**淘汰时 revoke**（不 revoke 的话 blob 一直被 URL 引着，等于没淘汰）。
 *
 * - 同一个键并发加载**单飞**：两段用同一份素材时只取一次；
 * - 正在播的（pin）不淘汰：被淘汰的 URL 还挂在 `<video>` 上的话画面直接断掉；
 * - 加载失败、或 loader 回 null（预览版还没生成好）**不缓存**，下次再取；
 * - `clear()` 全部 revoke（剪辑台关闭时）。
 *
 * `createObjectURL` / `revokeObjectURL` 可注入：测试里没有浏览器的 URL 实现。
 */

/** SC-PARAM-23：预览缓存总量上限。 */
export const BLOB_CACHE_DEFAULT_MAX_BYTES = 300 * 1024 * 1024;

export type BlobCacheEntry = {
  key: string;
  /** object URL（blob: 开头）。⚠️ 不叫 url：全站的守卫测试（authorizedMedia.test）把「给 src 赋一个叫 url 的值」当成把受保护端点直接塞进 src。 */
  objectUrl: string;
  blob: Blob;
  bytes: number;
};

export type BlobCacheOptions = {
  maxBytes?: number;
  createObjectURL?: (blob: Blob) => string;
  revokeObjectURL?: (url: string) => void;
};

type Slot = BlobCacheEntry & { pins: number };

export class BlobCache {
  readonly maxBytes: number;
  /** Map 的插入顺序就是 LRU 顺序：每次使用都删了重插，最久没用的在最前面。 */
  private readonly entries = new Map<string, Slot>();
  private readonly loading = new Map<string, Promise<BlobCacheEntry | null>>();
  private readonly createURL: (blob: Blob) => string;
  private readonly revokeURL: (url: string) => void;
  private total = 0;
  /** clear 之后还在路上的加载回来时不再入缓存（否则关掉剪辑台后又冒出几个没人 revoke 的 URL）。 */
  private generation = 0;

  constructor(options: BlobCacheOptions = {}) {
    this.maxBytes = options.maxBytes && options.maxBytes > 0 ? options.maxBytes : BLOB_CACHE_DEFAULT_MAX_BYTES;
    this.createURL = options.createObjectURL ?? ((blob) => URL.createObjectURL(blob));
    this.revokeURL = options.revokeObjectURL ?? ((url) => URL.revokeObjectURL(url));
  }

  /** 当前缓存的总字节数。 */
  get bytes(): number {
    return this.total;
  }

  get size(): number {
    return this.entries.size;
  }

  has(key: string): boolean {
    return this.entries.has(key);
  }

  /** 正在加载吗（引擎据此区分「还在取」与「没有」）。 */
  isLoading(key: string): boolean {
    return this.loading.has(key);
  }

  /** 取一项并记为「刚用过」。 */
  get(key: string): BlobCacheEntry | undefined {
    const slot = this.entries.get(key);
    if (!slot) return undefined;
    this.entries.delete(key);
    this.entries.set(key, slot);
    return slot;
  }

  /**
   * 取不到就用 `loader` 加载（单飞）。loader 回 null 表示「现在没有」（比如预览版还在生成），不缓存、回 null；
   * loader 抛错时原样抛出，也不缓存 —— 下次调用会重新加载。
   */
  load(key: string, loader: () => Promise<Blob | null>): Promise<BlobCacheEntry | null> {
    const hit = this.get(key);
    if (hit) return Promise.resolve(hit);
    const pending = this.loading.get(key);
    if (pending) return pending;
    const task = this.fetchInto(key, loader, this.generation);
    this.loading.set(key, task);
    // 收尾登记在返回之前：调用方在失败后立刻重试时，「正在加载」已经清掉了（它的回调排在这之后）。
    const done = () => {
      if (this.loading.get(key) === task) this.loading.delete(key);
    };
    task.then(done, done);
    return task;
  }

  private async fetchInto(key: string, loader: () => Promise<Blob | null>, gen: number): Promise<BlobCacheEntry | null> {
    const blob = await loader();
    if (!blob || gen !== this.generation) return null;
    // 等待期间别的调用方已经放进来了（比如 put）：用已有的那份，不重复建 URL。
    const existing = this.get(key);
    if (existing) return existing;
    return this.insert(key, blob);
  }

  /** 直接放一份进来（已有同键的先替换并 revoke 旧的；被 pin 着的旧项也照样替换 —— 调用方明确要新的）。 */
  put(key: string, blob: Blob): BlobCacheEntry {
    this.remove(key);
    return this.insert(key, blob);
  }

  /**
   * 标记「正在用」：被 pin 的不淘汰。返回取消函数（可重复调用，只减一次）。键不存在时返回空操作。
   * 同一项可以被多处 pin（两个 `<video>` 播同一份素材），全部取消之后才可淘汰。
   */
  pin(key: string): () => void {
    const slot = this.entries.get(key);
    if (!slot) return () => {};
    slot.pins += 1;
    let done = false;
    return () => {
      if (done) return;
      done = true;
      slot.pins = Math.max(0, slot.pins - 1);
      // 最后一处也不用了：期间进来的新项都没法挤掉它，可能已经超上限 —— 顺手收一次。
      // 还有别处在用时什么都不变，不收（否则会去挤掉刚放进来、还没来得及 pin 的项）。
      if (slot.pins === 0 && this.entries.get(key) === slot) this.evict();
    };
  }

  /** 删除一项并 revoke（被 pin 的也删：调用方明确不要它了）。 */
  remove(key: string): boolean {
    const slot = this.entries.get(key);
    if (!slot) return false;
    this.entries.delete(key);
    this.total -= slot.bytes;
    this.revokeURL(slot.objectUrl);
    return true;
  }

  /** 全部 revoke 并清空；还在路上的加载回来后不再入缓存。 */
  clear(): void {
    this.generation += 1;
    for (const slot of this.entries.values()) this.revokeURL(slot.objectUrl);
    this.entries.clear();
    this.loading.clear();
    this.total = 0;
  }

  private insert(key: string, blob: Blob): BlobCacheEntry {
    const slot: Slot = { key, blob, bytes: blob.size, objectUrl: this.createURL(blob), pins: 0 };
    this.entries.set(key, slot);
    this.total += slot.bytes;
    this.evict(key);
    return slot;
  }

  /**
   * 超上限时从最久没用的开始淘汰，跳过被 pin 的与刚放进来的那一项（一项本身就超上限时也留着：
   * 正要播的东西不能刚取回来就扔掉；它会在下一次有新项进来时被挤掉）。
   */
  private evict(keep?: string): void {
    if (this.total <= this.maxBytes) return;
    for (const [key, slot] of this.entries) {
      if (this.total <= this.maxBytes) break;
      if (slot.pins > 0 || key === keep) continue;
      this.entries.delete(key);
      this.total -= slot.bytes;
      this.revokeURL(slot.objectUrl);
    }
  }
}
