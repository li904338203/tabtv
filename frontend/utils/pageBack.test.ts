import { describe, expect, it } from "vitest";

import { backTarget } from "./pageBack";

describe("二级页的「返回」", () => {
  it("从站内点进来：退回上一页（保留来源页的筛选、翻页与滚动）", () => {
    expect(backTarget("/video/projects?page=3", "/")).toEqual({ mode: "history" });
    expect(backTarget("/", "/membership")).toEqual({ mode: "history" });
  });

  it("直接打开、刷新或新标签页：没有站内上一页，去页面的上级", () => {
    expect(backTarget(null, "/")).toEqual({ mode: "push", to: "/" });
    expect(backTarget(undefined, "/membership")).toEqual({ mode: "push", to: "/membership" });
    expect(backTarget("", "/")).toEqual({ mode: "push", to: "/" });
  });

  it("上一页是登录页：退回去只会在登录页上再跳一次，同样去上级", () => {
    expect(backTarget("/login", "/")).toEqual({ mode: "push", to: "/" });
    expect(backTarget("/login?redirect=%2Fmembership", "/")).toEqual({ mode: "push", to: "/" });
  });

  it("不认站外地址", () => {
    expect(backTarget("https://example.com/", "/")).toEqual({ mode: "push", to: "/" });
  });
});
