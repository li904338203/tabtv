import { describe, expect, it } from "vitest";
import {
  CREATE_DRAFT_KEY,
  CREATE_DRAFT_TTL_MS,
  decodeCreateDraft,
  encodeCreateDraft,
  saveCreateDraft,
  takeCreateDraft
} from "./createDraft";

function memoryStorage(initial: Record<string, string> = {}) {
  const data = new Map(Object.entries(initial));
  return {
    data,
    getItem: (key: string) => data.get(key) ?? null,
    setItem: (key: string, value: string) => { data.set(key, value); },
    removeItem: (key: string) => { data.delete(key); }
  };
}

const NOW = 1_800_000_000_000;

describe("创造页未登录草稿", () => {
  it("往返：正文、模式、技能、附件 mediaId", () => {
    const draft = { mode: "agent" as const, text: "三打白骨精", skill: { code: "story_topic", name: "故事选题" }, mediaIds: ["101", "202"] };
    expect(decodeCreateDraft(encodeCreateDraft(draft, NOW), NOW + 1000)).toEqual(draft);
    expect(decodeCreateDraft(encodeCreateDraft({ mode: "image", text: "x", mediaIds: [] }, NOW), NOW)).toEqual({ mode: "image", text: "x", mediaIds: [] });
  });

  it("只存 ID：非数字的附件引用（URL 等）丢弃", () => {
    const raw = encodeCreateDraft({ mode: "agent", text: "x", mediaIds: ["101", "https://evil.com/a.png", "1e3"] }, NOW);
    expect(decodeCreateDraft(raw, NOW)?.mediaIds).toEqual(["101"]);
    expect(raw).not.toContain("evil");
  });

  it("超过 24 小时丢弃", () => {
    const raw = encodeCreateDraft({ mode: "agent", text: "旧的", mediaIds: [] }, NOW);
    expect(decodeCreateDraft(raw, NOW + CREATE_DRAFT_TTL_MS - 1)).toBeDefined();
    expect(decodeCreateDraft(raw, NOW + CREATE_DRAFT_TTL_MS + 1)).toBeUndefined();
  });

  it("损坏 JSON、形状不对、未知模式都丢弃", () => {
    expect(decodeCreateDraft("{bad", NOW)).toBeUndefined();
    expect(decodeCreateDraft("[]", NOW)).toBeUndefined();
    expect(decodeCreateDraft(JSON.stringify({ savedAt: NOW, mode: "writing", text: "x" }), NOW)).toBeUndefined();
    expect(decodeCreateDraft(JSON.stringify({ savedAt: NOW, mode: "agent", text: 1 }), NOW)).toBeUndefined();
    expect(decodeCreateDraft(JSON.stringify({ mode: "agent", text: "无时间戳" }), NOW)).toBeUndefined();
    expect(decodeCreateDraft(null, NOW)).toBeUndefined();
  });

  it("取出即清掉，只恢复一次；过期的也清掉", () => {
    const storage = memoryStorage();
    saveCreateDraft(storage, { mode: "agent", text: "你好", mediaIds: [] }, NOW);
    expect(takeCreateDraft(storage, NOW)?.text).toBe("你好");
    expect(storage.data.has(CREATE_DRAFT_KEY)).toBe(false);
    expect(takeCreateDraft(storage, NOW)).toBeUndefined();

    saveCreateDraft(storage, { mode: "agent", text: "旧", mediaIds: [] }, NOW);
    expect(takeCreateDraft(storage, NOW + CREATE_DRAFT_TTL_MS + 1)).toBeUndefined();
    expect(storage.data.has(CREATE_DRAFT_KEY)).toBe(false);
  });

  it("存储抛错不影响使用", () => {
    const broken = {
      getItem: () => { throw new Error("denied"); },
      setItem: () => { throw new Error("denied"); },
      removeItem: () => { throw new Error("denied"); }
    };
    expect(() => saveCreateDraft(broken, { mode: "agent", text: "x", mediaIds: [] })).not.toThrow();
    expect(takeCreateDraft(broken)).toBeUndefined();
  });
});
