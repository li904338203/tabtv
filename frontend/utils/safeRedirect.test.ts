import { describe, expect, it } from "vitest";
import { loginPath, safeRedirect } from "./safeRedirect";

describe("safeRedirect（防开放重定向）", () => {
  it("接受站内相对路径，保留查询与锚点", () => {
    expect(safeRedirect("/create")).toBe("/create");
    expect(safeRedirect("/video/canvas/1?session=2#x")).toBe("/video/canvas/1?session=2#x");
    expect(safeRedirect(["/create", "/other"])).toBe("/create");
  });

  it("拒绝绝对地址、协议相对地址与伪协议", () => {
    expect(safeRedirect("https://evil.com")).toBe("/");
    expect(safeRedirect("http://evil.com/create")).toBe("/");
    expect(safeRedirect("//evil.com")).toBe("/");
    expect(safeRedirect("//evil.com/create")).toBe("/");
    expect(safeRedirect("javascript:alert(1)")).toBe("/");
    expect(safeRedirect("create")).toBe("/");
  });

  it("拒绝反斜杠与控制字符（浏览器会把它们纠错成外站）", () => {
    expect(safeRedirect("/\\evil.com")).toBe("/");
    expect(safeRedirect("\\\\evil.com")).toBe("/");
    expect(safeRedirect("/\tevil.com")).toBe("/");
    expect(safeRedirect("/create\r\nLocation: https://evil.com")).toBe("/");
  });

  it("不回跳到登录页自己，非字符串用兜底", () => {
    expect(safeRedirect("/login")).toBe("/");
    expect(safeRedirect("/login?redirect=/create")).toBe("/");
    expect(safeRedirect(undefined)).toBe("/");
    expect(safeRedirect(null, "/create")).toBe("/create");
    expect(safeRedirect(42)).toBe("/");
  });

  it("loginPath 只带站内回跳", () => {
    expect(loginPath("/create")).toBe("/login?redirect=%2Fcreate");
    expect(loginPath("https://evil.com")).toBe("/login");
    expect(loginPath("/")).toBe("/login");
  });
});
