import { describe, expect, it } from "vitest";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import {
  CREATE_BADGE_UNTIL,
  CREATE_PATH,
  MARKETING_PATH,
  isCreateActive,
  isMarketingActive,
  marketingFailureState,
  showCreateBadge,
  showMarketingEntry
} from "./sidebarNav";

describe("isCreateActive", () => {
  it("/create 与其子页高亮", () => {
    expect(isCreateActive("/create")).toBe(true);
    expect(isCreateActive("/create/x")).toBe(true);
  });

  it("前缀相同但不是子页的路径、其他产品都不高亮", () => {
    expect(isCreateActive("/created")).toBe(false);
    expect(isCreateActive("/video")).toBe(false);
    expect(isCreateActive("/")).toBe(false);
  });

  it("入口路径固定为 /create", () => {
    expect(CREATE_PATH).toBe("/create");
  });
});

describe("showCreateBadge", () => {
  it("截止日前显示、截止那一刻起不显示", () => {
    const until = new Date("2026-11-14T00:00:00Z");
    expect(showCreateBadge(new Date("2026-11-13T23:59:59Z"), until)).toBe(true);
    expect(showCreateBadge(new Date("2026-11-14T00:00:00Z"), until)).toBe(false);
    expect(showCreateBadge(new Date("2026-12-01T00:00:00Z"), until)).toBe(false);
  });

  it("默认截止日是上线日 + 30 天", () => {
    expect(showCreateBadge(new Date("2026-10-15T12:00:00+08:00"))).toBe(true);
    expect(CREATE_BADGE_UNTIL.getTime() - new Date("2026-10-15T00:00:00+08:00").getTime())
      .toBe(30 * 24 * 3600 * 1000);
  });
});

describe("isMarketingActive（MKT-F-01）", () => {
  it("/marketing 与其子页高亮", () => {
    expect(isMarketingActive("/marketing")).toBe(true);
    expect(isMarketingActive("/marketing/x")).toBe(true);
  });

  it("前缀相同但不是子页的路径、「创造」与其他页不高亮", () => {
    expect(isMarketingActive("/marketingx")).toBe(false);
    expect(isMarketingActive("/create")).toBe(false);
    expect(isMarketingActive("/")).toBe(false);
    // 反过来「创造」也不被营销页点亮
    expect(isCreateActive("/marketing")).toBe(false);
  });

  it("入口路径固定为 /marketing", () => {
    expect(MARKETING_PATH).toBe("/marketing");
  });

  it("营销不在产品清单（进了就能被产品开关关掉）", () => {
    const store = readFileSync(fileURLToPath(new URL("../stores/products.ts", import.meta.url)), "utf8");
    expect(store).not.toMatch(/marketing/i);
    expect(store).not.toContain("营销");
  });
});

describe("营销入口可见性（总开关 vs Agent 开关）", () => {
  it("只有 404 算总开关关闭；断网 / 5xx / 未知都不是定论", () => {
    expect(marketingFailureState(404)).toBe("disabled");
    expect(marketingFailureState(500)).toBe("error");
    expect(marketingFailureState(503)).toBe("error");
    expect(marketingFailureState(undefined)).toBe("error");
  });

  it("只在确认关闭时隐藏；加载中、出错、available:false（仍是 ready）都照常显示", () => {
    expect(showMarketingEntry("disabled")).toBe(false);
    for (const state of ["idle", "loading", "ready", "error"] as const) {
      expect(showMarketingEntry(state)).toBe(true);
    }
  });
});

describe("根路径（2026-11-01）", () => {
  it("打开 / 直接重定向到创造页：路由级重定向，不是挂载后再跳", () => {
    const page = readFileSync(fileURLToPath(new URL("../pages/index.vue", import.meta.url)), "utf8");
    expect(page).toContain(`definePageMeta({ redirect: "${CREATE_PATH}" })`);
    expect(page, "根路径不该再有自己的内容").not.toMatch(/useApi|navigateTo\(/);
  });
});
