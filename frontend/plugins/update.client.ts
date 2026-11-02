import { hostApp } from "~/utils/desktopBridge";

/** 接上宿主的「发现新版本」事件（TT-T-31）。没有宿主（浏览器直接开开发服务器）时什么都不做。 */
export default defineNuxtPlugin(() => {
  if (!hostApp()) return;
  useUpdateStore().attach();
});
