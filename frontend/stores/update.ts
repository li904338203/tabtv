import { hostApp, onHostUpdate, type HostUpdateInfo } from "~/utils/desktopBridge";

/**
 * 新版本提示（TT-T-31）：宿主每 24 小时检查一次，发现新版时推事件；顶栏显示一个入口，点进「关于」处理。
 * 用户在「关于」里手动检查的结果也写在这里，两处看到的是同一份。
 */
export const useUpdateStore = defineStore("update", () => {
  const info = ref<HostUpdateInfo | null>(null);
  const checking = ref(false);
  let attached = false;

  function attach() {
    if (attached) return;
    attached = true;
    onHostUpdate((next) => { info.value = next; });
  }

  async function check() {
    const host = hostApp();
    if (!host?.CheckUpdate) {
      info.value = null;
      return;
    }
    checking.value = true;
    try {
      info.value = await host.CheckUpdate();
    } finally {
      checking.value = false;
    }
  }

  return { info, checking, attach, check };
});
