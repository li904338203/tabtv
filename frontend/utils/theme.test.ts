import { describe, expect, it } from "vitest";
import {
  effectiveTheme,
  nextThemePreference,
  normalizeThemePreference,
  resolveTheme
} from "./theme";

describe("theme preference helpers", () => {
  it("keeps only valid preferences and treats dirty storage as unset", () => {
    expect(normalizeThemePreference("light")).toBe("light");
    expect(normalizeThemePreference("dark")).toBe("dark");
    expect(normalizeThemePreference("DARK")).toBeNull();
    expect(normalizeThemePreference("")).toBeNull();
    expect(normalizeThemePreference(null)).toBeNull();
    expect(normalizeThemePreference(undefined)).toBeNull();
    expect(normalizeThemePreference(1)).toBeNull();
  });

  it("follows the system when no preference was chosen", () => {
    expect(resolveTheme(null, true)).toBe("light");
    expect(resolveTheme(null, false)).toBe("dark");
  });

  it("lets an explicit preference win over the system", () => {
    expect(resolveTheme("dark", true)).toBe("dark");
    expect(resolveTheme("light", false)).toBe("light");
  });

  it("falls back to dark when the system preference is unknown", () => {
    // matchMedia 不可用时按 false 传入，需保持改造前的暗色观感
    expect(resolveTheme(null, false)).toBe("dark");
  });
});

describe("pinned theme", () => {
  it("overrides both preference and system while pinned", () => {
    expect(effectiveTheme("dark", "light", true)).toBe("dark");
    expect(effectiveTheme("dark", null, true)).toBe("dark");
  });

  it("restores preference-driven resolution once released", () => {
    expect(effectiveTheme(null, "light", false)).toBe("light");
    expect(effectiveTheme(null, null, true)).toBe("light");
    expect(effectiveTheme(null, null, false)).toBe("dark");
  });
});

describe("toggle", () => {
  it("always produces the opposite explicit preference", () => {
    expect(nextThemePreference("dark")).toBe("light");
    expect(nextThemePreference("light")).toBe("dark");
  });

  it("makes a system-following user explicit on first toggle", () => {
    // 跟随系统(亮) → 点击后应显式变暗，而非再次跟随系统
    const current = resolveTheme(null, true);
    expect(nextThemePreference(current)).toBe("dark");
  });
});
