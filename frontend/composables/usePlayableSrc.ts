import { hostApp } from "~/utils/desktopBridge";
import { needsBlobPlayback } from "~/utils/playableSrc";
import { isLinuxDesktop } from "~/utils/videoPlaybackHint";

/**
 * 直接引用地址的视频在 Linux 桌面上换成 blob 地址（TT-T-16，dev/01 R-13）；其余平台原样返回。
 *
 * 给「模板里要拿 `<video>` 元素 ref」的地方用（`components/media/PlayableVideo.vue` 是组件版，套上之后 ref 拿到的是组件）。
 * WebKitGTK 用 Wails 的自定义协议播不了直接地址（error 4），先取成 blob 再播正常、可拖动。
 *
 * ⚠️ object URL 换地址与卸载时都要 revoke；取 blob 失败时退回直接地址（播不出来有 TT-T-28 的解码提示兜底）。
 */
export function usePlayableSrc(source: () => string | null | undefined) {
  const linux = Boolean(import.meta.client && hostApp() && isLinuxDesktop(navigator.userAgent));
  const src = ref<string | undefined>(linux ? undefined : source() ?? undefined);
  let objectUrl = "";
  let seq = 0;

  function release() {
    if (objectUrl) URL.revokeObjectURL(objectUrl);
    objectUrl = "";
  }

  watch(source, async (next) => {
    const mine = ++seq;
    release();
    if (!needsBlobPlayback(linux, next)) {
      src.value = next ?? undefined;
      return;
    }
    src.value = undefined;
    try {
      const res = await fetch(next!);
      if (!res.ok) throw new Error(String(res.status));
      const url = URL.createObjectURL(await res.blob());
      if (mine !== seq) {
        URL.revokeObjectURL(url); // 取回来时地址已经换了（或已卸载）
        return;
      }
      objectUrl = url;
      src.value = url;
    } catch {
      if (mine === seq) src.value = next ?? undefined;
    }
  }, { immediate: true });

  onScopeDispose(() => {
    seq++;
    release();
  });
  return src;
}
