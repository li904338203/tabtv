import { describe, expect, it } from "vitest";
import {
  accountContact,
  buildAccountMenu,
  buildCreditMenu,
  creditBalanceOf,
  creditBreakdown,
  formatLocalDate,
  localDayDiff,
  maskContact,
  membershipAction,
  membershipLine,
  membershipView,
  unreadBadge
} from "./accountPanel";

describe("maskContact", () => {
  it("masks plain phone numbers in the middle", () => {
    expect(maskContact("17612347101")).toBe("176****7101");
  });
  it("keeps already-masked values from the server untouched", () => {
    expect(maskContact("176****7101")).toBe("176****7101");
    expect(maskContact("a***b@x.com")).toBe("a***b@x.com");
  });
  it("masks plain emails but keeps the domain", () => {
    expect(maskContact("alice@example.com")).toBe("a****e@example.com");
    expect(maskContact("ab@example.com")).toBe("a****@example.com");
  });
  it("returns empty for missing values and never leaks short strings", () => {
    expect(maskContact(null)).toBe("");
    expect(maskContact("  ")).toBe("");
    expect(maskContact("12345")).toBe("****");
  });
  it("prefers phone over email", () => {
    expect(accountContact({ phone: "176****7101", email: "a***b@x.com" })).toBe("176****7101");
    expect(accountContact({ phone: null, email: "a***b@x.com" })).toBe("a***b@x.com");
    expect(accountContact(null)).toBe("");
  });
});

describe("local dates", () => {
  it("formats with the local calendar date, not UTC", () => {
    expect(formatLocalDate(new Date(2026, 0, 5, 0, 30))).toBe("2026-01-05");
    expect(formatLocalDate("not-a-date")).toBe("");
  });
  it("counts calendar days, not 24h blocks", () => {
    expect(localDayDiff(new Date(2026, 9, 17, 23, 0), new Date(2026, 9, 18, 8, 0))).toBe(1);
    expect(localDayDiff(new Date(2026, 9, 18, 1, 0), new Date(2026, 9, 18, 23, 0))).toBe(0);
    expect(localDayDiff(new Date(2026, 7, 6), new Date(2026, 9, 18))).toBe(73);
  });
});

describe("membershipView", () => {
  const now = new Date(2026, 9, 18, 10, 0);

  it("shows the active membership with its local expiry date", () => {
    const view = membershipView({ levelName: "标准版", expireAt: new Date(2026, 10, 1, 12).toISOString() }, [], now);
    expect(view).toEqual({ kind: "active", levelName: "标准版", expireDate: "2026-11-01", daysLeft: 14 });
    expect(membershipLine(view)).toBe("标准版 · 2026-11-01 到期");
    expect(membershipAction(view)).toBe("续费");
  });

  it("reports how many days ago the latest membership expired", () => {
    const view = membershipView(null, [
      { levelName: "标准版", expireAt: new Date(2026, 7, 6, 9).toISOString(), status: "expired" },
      { levelName: "旧版", expireAt: new Date(2025, 0, 1).toISOString(), status: "expired" }
    ], now);
    expect(view).toEqual({ kind: "expired", levelName: "标准版", daysAgo: 73 });
    expect(membershipLine(view)).toBe("标准版已过期 73 天");
    expect(membershipAction(view)).toBe("开通会员");
  });

  it("says 'expired today' instead of '0 days'", () => {
    const view = membershipView(null, [{ levelName: "标准版", expireAt: new Date(2026, 9, 18, 2).toISOString(), status: "active" }], now);
    expect(membershipLine(view)).toBe("标准版今日已过期");
  });

  it("ignores refunded and superseded rows", () => {
    const view = membershipView(null, [
      { levelName: "退掉的", expireAt: new Date(2026, 8, 1).toISOString(), status: "refunded" },
      { levelName: "被顶替的", expireAt: new Date(2026, 8, 2).toISOString(), status: "superseded" }
    ], now);
    expect(view).toEqual({ kind: "none" });
    expect(membershipLine(view)).toBe("暂未开通会员");
  });

  it("does not treat a stale current row as active", () => {
    const view = membershipView({ levelName: "标准版", expireAt: new Date(2026, 9, 1).toISOString() }, null, now);
    expect(view.kind).toBe("none");
  });
});

describe("creditBreakdown", () => {
  it("groups remaining credits by source in a fixed order", () => {
    expect(creditBreakdown([
      { source: "purchase", remaining: 20 },
      { source: "membership", remaining: 5 },
      { source: "membership", remaining: 3 },
      { source: "register", remaining: 0 }
    ])).toEqual([
      { source: "membership", label: "会员赠送", amount: 8 },
      { source: "purchase", label: "充值购买", amount: 20 }
    ]);
  });
  it("keeps unknown sources as 'other' so the sum still matches", () => {
    const parts = creditBreakdown([{ source: "mystery", remaining: 7 }, { source: "promotion", remaining: 1 }]);
    expect(parts).toEqual([
      { source: "promotion", label: "活动赠送", amount: 1 },
      { source: "other", label: "其他", amount: 7 }
    ]);
  });
  it("returns nothing when grants are unavailable", () => {
    expect(creditBreakdown(undefined)).toEqual([]);
    expect(creditBreakdown([])).toEqual([]);
  });
});

describe("buildAccountMenu", () => {
  it("lists the menu in order with badge and watermark hint", () => {
    const menu = buildAccountMenu({ unread: 2, watermarkOff: false });
    expect(menu.map(i => i.key)).toEqual(["account", "orders", "theme", "watermark", "mcp", "notifications", "logout"]);
    expect(menu.find(i => i.key === "notifications")).toMatchObject({ to: "/notifications", badge: "2" });
    expect(menu.find(i => i.key === "watermark")).toMatchObject({ to: "/account#watermark", hint: "已开启" });
    expect(menu.find(i => i.key === "account")).toMatchObject({ to: "/account" });
  });
  it("omits badge when nothing is unread and the hint when watermark state is unknown", () => {
    const menu = buildAccountMenu({ unread: 0, watermarkOff: null });
    expect(menu.find(i => i.key === "notifications")).not.toHaveProperty("badge", expect.anything());
    expect(menu.find(i => i.key === "watermark")).toMatchObject({ hint: undefined });
    expect(buildAccountMenu({ unread: 0, watermarkOff: true }).find(i => i.key === "watermark")).toMatchObject({ hint: "已关闭" });
  });
  it("does not include excluded entries", () => {
    const labels = buildAccountMenu({ unread: 0, watermarkOff: null }).map(i => i.label).join();
    expect(labels).not.toMatch(/权益兑换|哩布|账号设置/);
  });
  it("caps the unread badge", () => {
    expect(unreadBadge(0)).toBe("");
    expect(unreadBadge(120)).toBe("99+");
  });
});

describe("积分面板（2026-11-01）", () => {
  it("底部入口只有系统里真有的去处", () => {
    expect(buildCreditMenu().map(item => [item.label, item.kind === "link" ? item.to : ""])).toEqual([
      ["积分明细", "/credits"],
      ["会员与订阅", "/membership"],
      ["订单记录", "/account#orders"]
    ]);
  });

  it("余额按契约读 balance；老结构的 total 兜底；都没有是「不知道」而不是 0", () => {
    expect(creditBalanceOf({ balance: 95036, grants: [] })).toBe(95036);
    expect(creditBalanceOf({ balance: 0, grants: [] })).toBe(0);
    expect(creditBalanceOf({ total: 10, permanent: 10, expiring: [] })).toBe(10);
    expect(creditBalanceOf({ grants: [] })).toBeUndefined();
    expect(creditBalanceOf({ balance: "12" })).toBeUndefined();
    expect(creditBalanceOf(null)).toBeUndefined();
  });
});
