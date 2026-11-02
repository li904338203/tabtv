import type { HostUpdateInfo } from "~/utils/desktopBridge";

/**
 * 「关于」页检查更新后的展示（TT-T-31），纯函数。
 *
 * ⚠️ 按 `mode` 给按钮，不按平台名猜：同是 Linux，AppImage 能应用内安装、.deb 不能（没有权限改系统目录）；
 *    某个版本缺本平台的包时宿主也会回 manual —— 给一个点了必失败的安装按钮比不给更糟。
 */
export type UpdateAction = "install" | "open-page" | "none";

export function updateAction(info: HostUpdateInfo | null): UpdateAction {
  if (!info?.available) return "none";
  if (info.mode === "auto") return "install";
  return info.pageUrl ? "open-page" : "none";
}

export function updateSummary(info: HostUpdateInfo | null): string {
  if (!info) return "";
  if (info.available) return `发现新版本 ${info.latest}（当前 ${info.current}）`;
  return info.message || "已是最新版本";
}
