/**
 * 与 Wails 宿主的桥（TT-T-03 起）。
 *
 * Wails 把绑定对象挂在 `window.go.<包名>.<类型名>` 上。浏览器里直接打开开发服务器时没有宿主，
 * 一律返回 null —— 页面照常可用，只是拿不到宿主能力。
 */
export interface StartupError {
  reason: string;
  detail: string;
  dataDir: string;
  logDir: string;
}

interface HostApp {
  GetStartupError(): Promise<StartupError | null>;
  RetryStartup(): Promise<StartupError | null>;
  OpenDataDir?(): Promise<void>;
  OpenLogDir?(): Promise<void>;
}

export function hostApp(): HostApp | null {
  if (typeof window === "undefined") return null;
  const go = (window as unknown as { go?: { app?: { App?: HostApp } } }).go;
  return go?.app?.App ?? null;
}

/** 启动错误页的路由。 */
export const STARTUP_ERROR_PATH = "/startup-error";

/** 宿主推来的实时事件名（与 Go 侧 app.EventName 一致）。 */
export const HOST_EVENT = "tabtv:event";

interface HostRuntime {
  EventsOn(name: string, cb: (...data: unknown[]) => void): () => void;
}

/**
 * 订阅宿主推来的实时事件（dev/01 TTD-3）。每条事件是 `{topic, payload}`。
 * 没有宿主时返回 null（浏览器直接打开开发服务器）。
 */
export function onHostEvent(cb: (topic: string, payload: unknown) => void): (() => void) | null {
  if (typeof window === "undefined") return null;
  const rt = (window as unknown as { runtime?: HostRuntime }).runtime;
  if (!rt?.EventsOn) return null;
  return rt.EventsOn(HOST_EVENT, (...data: unknown[]) => {
    const ev = data[0] as { topic?: unknown; payload?: unknown } | undefined;
    if (ev && typeof ev.topic === "string") cb(ev.topic, ev.payload);
  });
}
