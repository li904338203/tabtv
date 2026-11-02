/**
 * 视频播放失败时要不要提示「可能缺少解码插件」（TT-T-28）。纯函数，平台与错误码由调用方给。
 *
 * Linux 上 WebKitGTK 播放 H.264 要 GStreamer 的 avdec_h264（gst-libav）。缺它时视频区域是黑的、没有任何报错，
 * 用户只会觉得「生成的视频坏了」—— 而产物其实完好。macOS / Windows 的 WebView 自带解码，不提示。
 *
 * ⚠️ 只认「格式不支持」与「解码失败」两种错误：网络错误、用户中止与解码插件无关，提示了只会把人引去装不需要的包。
 * ⚠️ 文案写「可能」：MEDIA_ERR_SRC_NOT_SUPPORTED 也可能来自媒体地址本身的问题（dev/05 TTR-1）。
 */

// HTMLMediaElement 的 MediaError.code（规范值；测试环境里没有 MediaError 全局，故写成常量）。
export const MEDIA_ERR_ABORTED = 1;
export const MEDIA_ERR_NETWORK = 2;
export const MEDIA_ERR_DECODE = 3;
export const MEDIA_ERR_SRC_NOT_SUPPORTED = 4;

/** 自检页里这一项的锚点（与后端 Check.key = video_decoder 对应）。 */
export const VIDEO_DECODER_DIAG_PATH = "/settings/diagnostics#diag-video_decoder";

export const VIDEO_DECODER_HINT =
  "视频无法播放，可能是系统缺少 H.264 解码插件（GStreamer libav）。视频文件本身是完好的，安装插件后重启应用即可播放。";

/** 由 userAgent 判断是不是 Linux 桌面（Android 的 UA 里也有 Linux，要排除）。 */
export function isLinuxDesktop(userAgent: string): boolean {
  return /Linux/i.test(userAgent) && !/Android/i.test(userAgent);
}

/** (是否 Linux 桌面, MediaError.code) → 要不要提示。 */
export function shouldHintVideoDecoder(linuxDesktop: boolean, code: number | undefined): boolean {
  if (!linuxDesktop) return false;
  return code === MEDIA_ERR_DECODE || code === MEDIA_ERR_SRC_NOT_SUPPORTED;
}
