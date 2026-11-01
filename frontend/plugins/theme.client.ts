import { THEME_STORAGE_KEY, normalizeThemePreference } from "~/utils/theme";

/**
 * 同步首屏已由 app.head 的内联脚本写入的 data-theme 到响应式状态，
 * 并在用户未手动选择时跟随系统偏好变化。
 */
export default defineNuxtPlugin(() => {
  const { preference, systemPrefersLight, resolved, apply } = useTheme();
  const query = window.matchMedia("(prefers-color-scheme: light)");

  preference.value = normalizeThemePreference(localStorage.getItem(THEME_STORAGE_KEY));
  systemPrefersLight.value = query.matches;
  apply(resolved.value);

  // 已手动选择过主题的用户不受系统切换影响
  query.addEventListener("change", (event) => {
    systemPrefersLight.value = event.matches;
    if (!preference.value) apply(resolved.value);
  });
});
