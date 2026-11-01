/** 用户可选的主题偏好；null 表示未选择过，此时跟随操作系统。 */
export type ThemePreference = "light" | "dark" | null;
/** 实际生效的主题，用于写入 data-theme。 */
export type ResolvedTheme = "light" | "dark";

export const THEME_STORAGE_KEY = "aitv-theme";

/** 把任意来源的值收敛成合法偏好，脏数据一律当作未选择。 */
export function normalizeThemePreference(value: unknown): ThemePreference {
  return value === "light" || value === "dark" ? value : null;
}

/**
 * 偏好 + 系统值 → 实际主题。
 * 无法判断系统偏好时兜底 dark，保持改造前的观感。
 */
export function resolveTheme(
  preference: ThemePreference,
  systemPrefersLight: boolean
): ResolvedTheme {
  if (preference) return preference;
  return systemPrefersLight ? "light" : "dark";
}

/** 钉住的主题优先于用户偏好（画布等恒定暗色的全屏工作区用）。 */
export function effectiveTheme(
  pinned: ResolvedTheme | null,
  preference: ThemePreference,
  systemPrefersLight: boolean
): ResolvedTheme {
  return pinned ?? resolveTheme(preference, systemPrefersLight);
}

/** 点击切换按钮后应写入的偏好：始终产生与当前相反的显式偏好。 */
export function nextThemePreference(current: ResolvedTheme): ThemePreference {
  return current === "dark" ? "light" : "dark";
}
