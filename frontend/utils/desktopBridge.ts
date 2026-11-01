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
}

export function hostApp(): HostApp | null {
  if (typeof window === "undefined") return null;
  const go = (window as unknown as { go?: { app?: { App?: HostApp } } }).go;
  return go?.app?.App ?? null;
}

/** 启动错误页的路由。 */
export const STARTUP_ERROR_PATH = "/startup-error";
