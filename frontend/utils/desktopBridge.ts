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

/** 一次检查更新的结果（与 Go 侧 app.UpdateInfo 一致，TT-T-31）。 */
export interface HostUpdateInfo {
  current: string;
  commit: string;
  buildTime: string;
  latest: string;
  notes: string;
  pageUrl: string;
  available: boolean;
  /** auto = 应用内下载安装；manual = 只能打开下载页（macOS、Linux .deb）。 */
  mode: "auto" | "manual" | "";
  message: string;
}

interface HostApp {
  GetStartupError(): Promise<StartupError | null>;
  RetryStartup(): Promise<StartupError | null>;
  CheckUpdate?(): Promise<HostUpdateInfo>;
  /** 空串 = 已开始安装（应用随即退出）；否则是失败原因。 */
  InstallUpdate?(): Promise<string>;
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

/** 宿主推来的「发现新版本」事件名（与 Go 侧 app.UpdateEventName 一致）。 */
export const HOST_UPDATE_EVENT = "tabtv:update";

/** 订阅「发现新版本」。没有宿主时返回 null。 */
export function onHostUpdate(cb: (info: HostUpdateInfo) => void): (() => void) | null {
  if (typeof window === "undefined") return null;
  const rt = (window as unknown as { runtime?: HostRuntime }).runtime;
  if (!rt?.EventsOn) return null;
  return rt.EventsOn(HOST_UPDATE_EVENT, (...data: unknown[]) => {
    const info = data[0] as HostUpdateInfo | undefined;
    if (info && typeof info === "object") cb(info);
  });
}

/** 用系统浏览器打开地址（WebView 里直接跳转会把应用窗口变成浏览器）。 */
export function openExternal(url: string): void {
  if (typeof window === "undefined") return;
  const rt = (window as unknown as { runtime?: { BrowserOpenURL?: (url: string) => void } }).runtime;
  if (rt?.BrowserOpenURL) rt.BrowserOpenURL(url);
  else window.open(url, "_blank", "noopener");
}
