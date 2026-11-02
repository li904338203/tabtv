/**
 * Linux 桌面上直接引用的媒体地址要先取成 blob（TT-T-16，dev/01 R-13）。纯函数，平台判定由调用方给。
 *
 * WebKitGTK 用 Wails 的自定义协议播放 `/api/...` 视频会报 error 4，取成 blob 再用 object URL 播放正常、能拖动。
 * macOS / Windows 直接地址就能播（Range 206 正常），不多走一次整文件下载。
 *
 * ⚠️ 只处理「直接地址」：已经是 blob: / data: 的原样返回（受保护媒体本来就由 useAuthenticatedMedia 取成 blob）。
 */
export function needsBlobPlayback(linuxDesktop: boolean, src: string | null | undefined): boolean {
  if (!linuxDesktop || !src) return false;
  return !/^(blob|data):/i.test(src);
}
