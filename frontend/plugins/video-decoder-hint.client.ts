import { ElMessageBox } from "element-plus";
import { hostApp } from "~/utils/desktopBridge";
import { VIDEO_DECODER_DIAG_PATH, VIDEO_DECODER_HINT, isLinuxDesktop, shouldHintVideoDecoder } from "~/utils/videoPlaybackHint";

/**
 * Linux 上视频播放失败时提示一次「可能缺少解码插件」（TT-T-28）。
 *
 * ⚠️ 挂在 document 的**捕获阶段**而不是逐个组件：媒体元素的 error 事件不冒泡，但捕获阶段收得到 ——
 *    一处覆盖全部 `<video>`（含时间线、剪辑台里动态创建的），组件模板一行不改（TT-UX-1）。
 * ⚠️ 一次会话只提示一次：一屏素材全黑时每一个都弹，等于把人关在对话框里。
 */
export default defineNuxtPlugin(() => {
  if (!hostApp() || !isLinuxDesktop(navigator.userAgent)) return;
  let shown = false;
  document.addEventListener("error", (event) => {
    const el = event.target;
    if (shown || !(el instanceof HTMLVideoElement)) return;
    if (!shouldHintVideoDecoder(true, el.error?.code)) return;
    shown = true;
    ElMessageBox.confirm(VIDEO_DECODER_HINT, "视频无法播放", {
      confirmButtonText: "查看环境自检",
      cancelButtonText: "知道了",
      type: "warning"
    }).then(() => { void navigateTo(VIDEO_DECODER_DIAG_PATH); }).catch(() => {});
  }, true);
});
