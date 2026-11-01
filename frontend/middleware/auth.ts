import { isDefinitiveRefreshFailure } from "@tabtv/api-client";
import { loginPath } from "~/utils/safeRedirect";

export default defineNuxtRouteMiddleware(async (to) => {
  // 登录后回到原来要去的页面（login.vue 读 `?redirect=`，只认站内路径）。
  const toLogin = loginPath(to.fullPath);
  const auth = useAuthStore();
  if (import.meta.server) {
    if (!auth.accessToken && !auth.refreshToken) return navigateTo(toLogin);
    return;
  }
  if (!auth.accessToken && auth.refreshToken) {
    try {
      await auth.refreshAccessToken();
    } catch (e) {
      // 明确被拒才清；断网时留着 refresh，下一次进来还能续（见 isDefinitiveRefreshFailure）
      if (isDefinitiveRefreshFailure(e)) auth.clearSession();
    }
  }
  if (!auth.isAuthenticated) {
    return navigateTo(toLogin);
  }
  if (!auth.user) {
    try {
      await auth.loadProfile();
    } catch {
      auth.clearSession();
      return navigateTo(toLogin);
    }
  }
});
