import type { ResolvedTheme, ThemePreference } from "~/utils/theme";
import {
  THEME_STORAGE_KEY,
  effectiveTheme,
  nextThemePreference
} from "~/utils/theme";

/**
 * 亮暗主题运行时。判定逻辑在 utils/theme.ts（纯函数，可测），
 * 这里只负责状态、DOM 落地与持久化。
 */
export function useTheme() {
  // 首屏由 app.head 的前置脚本写好 data-theme，
  // 这里读回来作为初值，避免 hydration 后又闪一次。
  const preference = useState<ThemePreference>("theme-preference", () => null);
  const systemPrefersLight = useState<boolean>("theme-system-light", () => false);
  // 画布等全屏工作区恒定暗色，期间钉住主题、忽略用户偏好
  const pinned = useState<ResolvedTheme | null>("theme-pinned", () => null);

  const resolved = computed(() =>
    effectiveTheme(pinned.value, preference.value, systemPrefersLight.value)
  );

  function apply(next: ResolvedTheme) {
    if (!import.meta.client) return;
    document.documentElement.dataset.theme = next;
    // Element Plus 的暗色变量挂在 html.dark 上，与 data-theme 并存
    document.documentElement.classList.toggle("dark", next === "dark");
  }

  function set(next: ThemePreference) {
    preference.value = next;
    if (!import.meta.client) return;
    if (next) localStorage.setItem(THEME_STORAGE_KEY, next);
    // 清除偏好后重新跟随系统
    else localStorage.removeItem(THEME_STORAGE_KEY);
    apply(resolved.value);
  }

  function toggle() {
    set(nextThemePreference(resolved.value));
  }

  /** 钉住主题（画布进入时调用），返回解除函数。 */
  function pin(next: ResolvedTheme) {
    pinned.value = next;
    apply(next);
    return () => {
      pinned.value = null;
      apply(resolved.value);
    };
  }

  return { preference, resolved, systemPrefersLight, pinned, set, toggle, apply, pin };
}
