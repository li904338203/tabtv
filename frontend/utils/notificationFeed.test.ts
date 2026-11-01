import { describe, expect, it } from "vitest";
import { markReadBody, messageLink, notificationFeedPage, notificationFeedPath } from "./notificationFeed";

describe("消息中心契约映射", () => {
  it("第一页不带游标；之后带两段游标", () => {
    expect(notificationFeedPath("")).toBe("/notifications");
    expect(notificationFeedPath("2026-11-01T05:48:26Z|123")).toBe("/notifications?cursorAt=2026-11-01T05%3A48%3A26Z&cursorId=123");
  });

  it("两段游标都有才有下一页", () => {
    const item = { id: "1", source: "notification" as const, isRead: false, title: "t", content: "", type: "x", createdAt: "", linkUrl: "", refId: "", refType: "" };
    expect(notificationFeedPage({ items: [item], nextCursorAt: "a", nextCursorId: "9" }).nextCursor).toBe("a|9");
    expect(notificationFeedPage({ items: [item], nextCursorAt: "", nextCursorId: "" }).nextCursor).toBeNull();
    expect(notificationFeedPage({ items: [item] }).nextCursor).toBeNull();
  });

  it("标已读带 id 与 source（两张表的 id 无从区分）", () => {
    expect(markReadBody({ id: "7", source: "announcement" })).toEqual({ id: "7", source: "announcement" });
  });

  it("只跳站内地址", () => {
    expect(messageLink({ linkUrl: "/video/canvas/1" })).toBe("/video/canvas/1");
    expect(messageLink({ linkUrl: "https://evil.example" })).toBe("");
    expect(messageLink({ linkUrl: "//evil.example" })).toBe("");
    expect(messageLink({ linkUrl: "" })).toBe("");
  });
});
