import type { components } from "@tabtv/api-client";

/**
 * 剪辑台用的预览素材（docs/agent/smartcut SCT-16 / SCT-20）：每个媒体的预览版、拼图、波形、节拍，
 * 来自 `GET /media/{mediaId}/preview-assets`。
 *
 * - 按媒体缓存（`useState`，剪辑台关了再开不重复取）；同一个媒体并发要求时只发一次请求；
 * - 还在生成（`queued` / `running`）的，按 3 → 5 → 10 秒…… 退避重取，最多 12 次（约 3 分钟）：
 *   分析完成的实时信号（`smartcut.updated` 的 `analysis`）到了就立刻重取，这里的轮询只是兜底；
 * - 取不到（404、断网）的不重试也不报错：时间线照常显示，缩略图位置画占位。
 *
 * ⚠️ 这里只给元数据（拼图、预览版的**媒体 ID**、峰值、节拍），不取图本身：图要 fetch 成 blob 再给 object URL，
 *    那必须在组件作用域里做（`useAuthenticatedMedia` 卸载时释放），放进全局缓存的话没有人负责 revoke。
 */

export type SmartcutPreviewAssets = components["schemas"]["PreviewAssets"];

const PENDING = new Set(["queued", "running"]);
const RETRY_DELAYS_MS = [3000, 5000, 10000, 15000, 20000, 20000, 20000, 30000, 30000, 30000, 30000, 30000];

const inflight = new Map<string, Promise<SmartcutPreviewAssets | null>>();
const timers = new Map<string, ReturnType<typeof setTimeout>>();
const attempts = new Map<string, number>();

/** 还有没有在生成的派生素材（还要再取一次）。 */
export function smartcutPreviewPending(assets: SmartcutPreviewAssets | null | undefined): boolean {
  return Object.values(assets?.status ?? {}).some(state => PENDING.has(state));
}

export function useSmartcutPreviewAssets() {
  const assets = useState<Record<string, SmartcutPreviewAssets>>("smartcut:preview-assets", () => ({}));

  function get(mediaId: string | null | undefined): SmartcutPreviewAssets | undefined {
    return mediaId ? assets.value[mediaId] : undefined;
  }

  async function fetchOne(mediaId: string): Promise<SmartcutPreviewAssets | null> {
    const running = inflight.get(mediaId);
    if (running) return running;
    const task = (async () => {
      try {
        const res = await useApi()<SmartcutPreviewAssets>(`/media/${mediaId}/preview-assets`);
        assets.value = { ...assets.value, [mediaId]: res };
        schedule(mediaId, res);
        return res;
      } catch {
        return null;
      } finally {
        inflight.delete(mediaId);
      }
    })();
    inflight.set(mediaId, task);
    return task;
  }

  /** 还在生成的，过一会儿再取一次（退避）；生成完了就不再取。 */
  function schedule(mediaId: string, res: SmartcutPreviewAssets) {
    clearTimeout(timers.get(mediaId));
    timers.delete(mediaId);
    if (!smartcutPreviewPending(res)) {
      attempts.delete(mediaId);
      return;
    }
    const n = attempts.get(mediaId) ?? 0;
    if (n >= RETRY_DELAYS_MS.length) return;
    attempts.set(mediaId, n + 1);
    timers.set(mediaId, setTimeout(() => {
      timers.delete(mediaId);
      void fetchOne(mediaId);
    }, RETRY_DELAYS_MS[n]));
  }

  /** 保证这些媒体的预览素材在缓存里（已有的不重取）。 */
  async function ensure(mediaIds: readonly string[]): Promise<void> {
    const missing = [...new Set(mediaIds)].filter(id => id && !assets.value[id]);
    await Promise.allSettled(missing.map(id => fetchOne(id)));
  }

  /** 收到「分析完成」的信号时立即重取（不等退避）。 */
  function refresh(mediaId: string): Promise<SmartcutPreviewAssets | null> {
    attempts.delete(mediaId);
    return fetchOne(mediaId);
  }

  /** 剪辑台关闭时停掉兜底轮询（缓存留着，下次打开秒开）。 */
  function stopPolling() {
    for (const timer of timers.values()) clearTimeout(timer);
    timers.clear();
    attempts.clear();
  }

  return { assets, get, ensure, refresh, stopPolling };
}

const peaksCache = new Map<string, Uint8Array>();
const PEAKS_CACHE_LIMIT = 64;

/**
 * 峰值（base64 的 uint8 数组）解码，按媒体缓存：时间线上同一条配乐常被切成好几段，每段各解一次的话
 * 拖动时每一帧都在重复解码。超过 64 个媒体时丢最早的（一个媒体几十 KB，不必常驻）。
 */
export function smartcutPeaksOf(mediaId: string, values: string): Uint8Array {
  const key = `${mediaId}:${values.length}`;
  const cached = peaksCache.get(key);
  if (cached) return cached;
  let bytes: Uint8Array;
  try {
    bytes = Uint8Array.from(atob(values), ch => ch.charCodeAt(0));
  } catch {
    bytes = new Uint8Array();
  }
  peaksCache.set(key, bytes);
  if (peaksCache.size > PEAKS_CACHE_LIMIT) peaksCache.delete(peaksCache.keys().next().value as string);
  return bytes;
}
