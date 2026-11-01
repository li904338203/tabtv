import { authorizedMediaRequest } from "~/utils/authorizedMedia";
import { smartcutParseTypesetBox, smartcutParseTypesetMissing, smartcutTypesetKey, type SmartcutTypesetInput } from "~/utils/smartcutTypeset";
import type { SmartcutTypesetEntry } from "~/stores/smartcut";

/**
 * 服务端排版（SCD-46，SCT-21 / SCT-22）：`POST /smartcut/typeset` 回透明 PNG，响应头给位置框与缺字。
 * 结果按内容键缓存在剪辑 store（`typesetCache`），剪辑台关闭时一并释放 object URL。
 *
 * - **停手 400ms 才请求**（`schedule`）：每敲一个字就请求的话，一句字幕打完要排版十几次；
 * - 同一个键并发请求只发一次；失败的键 10 秒内不再重试（接口 400 时不会自己好，反复请求只是刷错误）；
 * - PNG 是二进制，`useApi` 只认 JSON，这里自己带鉴权与 `X-Workspace-Id`（同受保护媒体的唯一来源 `authorizedMediaRequest`）。
 */

const DEBOUNCE_MS = 400;
const FAIL_BACKOFF_MS = 10000;
const inflight = new Map<string, Promise<SmartcutTypesetEntry | null>>();
const failedAt = new Map<string, number>();
const timers = new Map<string, ReturnType<typeof setTimeout>>();

export function useSmartcutTypeset() {
  const store = useSmartcutStore();
  const auth = useAuthStore();
  const workspace = useWorkspaceStore();
  const config = useRuntimeConfig();

  function entry(input: SmartcutTypesetInput, width: number, height: number): SmartcutTypesetEntry | undefined {
    return store.typesetEntry(smartcutTypesetKey(input, width, height));
  }

  /** 立即排版（已有缓存直接返回）。失败返回 null，不抛：预览照常用临时显示。 */
  async function typeset(input: SmartcutTypesetInput, width: number, height: number): Promise<SmartcutTypesetEntry | null> {
    const key = smartcutTypesetKey(input, width, height);
    const cached = store.typesetEntry(key);
    if (cached) return cached;
    const running = inflight.get(key);
    if (running) return running;
    const failed = failedAt.get(key);
    if (failed && Date.now() - failed < FAIL_BACKOFF_MS) return null;
    const task = (async () => {
      try {
        const body = { kind: input.kind, text: input.text, width, height, ...(input.style ? { style: input.style } : {}), ...(input.kind === "text" && input.box ? { box: input.box } : {}) };
        const res = await auth.withTokenRefresh((token) => {
          const init = authorizedMediaRequest("/smartcut/typeset", token, String(config.public.apiBase), workspace.currentId);
          return fetch(init.url, { method: "POST", headers: { ...(init.headers ?? {}), "Content-Type": "application/json" }, body: JSON.stringify(body) });
        });
        const box = smartcutParseTypesetBox(res.headers.get("X-Typeset-Box"));
        if (!res.ok || !box) {
          failedAt.set(key, Date.now());
          return null;
        }
        const blob = await res.blob();
        // 剪辑台在这次请求期间关掉了：不进缓存，当场释放（缓存里的由 store 关闭时统一释放）
        if (!store.loaded) return null;
        const result: SmartcutTypesetEntry = {
          url: URL.createObjectURL(blob),
          box,
          missing: smartcutParseTypesetMissing(res.headers.get("X-Typeset-Missing")).join("")
        };
        store.setTypeset(key, result);
        failedAt.delete(key);
        const kept = store.typesetEntry(key);
        if (kept?.url !== result.url) URL.revokeObjectURL(result.url);
        return kept ?? null;
      } catch {
        failedAt.set(key, Date.now());
        return null;
      } finally {
        inflight.delete(key);
      }
    })();
    inflight.set(key, task);
    return task;
  }

  /** 停手 400ms 后排版（同一个 `slot` 里新的输入顶掉旧的：打字时只排最后那一版）。 */
  function schedule(slot: string, input: SmartcutTypesetInput, width: number, height: number) {
    clearTimeout(timers.get(slot));
    if (store.typesetEntry(smartcutTypesetKey(input, width, height))) {
      timers.delete(slot);
      return;
    }
    timers.set(slot, setTimeout(() => {
      timers.delete(slot);
      void typeset(input, width, height);
    }, DEBOUNCE_MS));
  }

  function cancelAll() {
    for (const timer of timers.values()) clearTimeout(timer);
    timers.clear();
  }

  return { entry, typeset, schedule, cancelAll };
}
