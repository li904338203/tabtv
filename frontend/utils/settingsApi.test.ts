import { describe, expect, it } from "vitest";
import { settingsApiError, settingsMediaUrl } from "./settingsApi";

describe("settingsApiError", () => {
  it("从 createError 的形状里取业务码与 details", () => {
    const e = { message: "版本已变化", statusCode: 409, data: { code: 40902, details: { issues: [] } } };
    expect(settingsApiError(e)).toEqual({ message: "版本已变化", code: 40902, status: 409, details: { issues: [] } });
  });
  it("认不出的错误给兜底文案", () => {
    expect(settingsApiError(undefined)).toEqual({ message: "请求失败", code: 0, status: 0, details: undefined });
    expect(settingsApiError(new Error("")).message).toBe("请求失败");
  });
});

describe("settingsMediaUrl", () => {
  it("空 ID 不给地址", () => {
    expect(settingsMediaUrl("")).toBe("");
    expect(settingsMediaUrl("0")).toBe("");
    expect(settingsMediaUrl("12")).toBe("/api/v1/admin/media/12");
  });
});
