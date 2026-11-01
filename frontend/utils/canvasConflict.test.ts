import { describe, expect, it } from "vitest";
import {
  adoptLabel,
  clockTime,
  conflictDetails,
  conflictSummary,
  draftKey,
  overwriteLabel,
  readDraft,
  removeDraft,
  writeDraft,
  type CanvasDraft
} from "./canvasConflict";

/** 与 useApi 抛出的错误同形：`{ data: { code, details } }` */
function conflictError(details?: Record<string, unknown>) {
  return { statusCode: 409, data: { code: 40902, details } };
}

function memoryStorage() {
  const map = new Map<string, string>();
  return {
    map,
    getItem: (key: string) => map.get(key) ?? null,
    setItem: (key: string, value: string) => { map.set(key, value); },
    removeItem: (key: string) => { map.delete(key); }
  };
}

const draft = (projectId: string): CanvasDraft => ({
  projectId,
  baseVersion: 3,
  savedAt: "2026-10-15T01:02:03Z",
  document: { schemaVersion: 1, viewport: { x: 0, y: 0, zoom: 1 }, nodes: [], edges: [] }
});

describe("冲突详情与文案", () => {
  it("bySelf 时文案指向我的另一个窗口", () => {
    const details = conflictDetails(conflictError({ currentVersion: 5, currentEditorName: "林澈", bySelf: true }));
    expect(details.bySelf).toBe(true);
    // ⚠️ 按布尔分流：即使带着名字，也说"我的另一个窗口"而不是"覆盖 林澈"
    expect(overwriteLabel(details)).toBe("以我为准（覆盖我在另一个窗口的修改）");
    expect(adoptLabel(details)).toBe("采纳另一个窗口的版本");
    expect(conflictSummary(details)).toContain("你在另一个窗口");
  });

  it("有昵称时写明覆盖谁", () => {
    const iso = "2026-10-15T06:07:00Z";
    const details = conflictDetails(conflictError({
      currentVersion: 7, currentUpdatedAt: iso, currentEditorId: "42", currentEditorName: " 林澈 ", bySelf: false
    }));
    expect(details).toEqual({ currentVersion: 7, currentUpdatedAt: iso, currentEditorId: "42", currentEditorName: "林澈", bySelf: false });
    expect(overwriteLabel(details)).toBe("以我为准（覆盖 林澈 的修改）");
    expect(adoptLabel(details)).toBe("采纳对方版本");
    expect(conflictSummary(details)).toBe(`林澈 在 ${clockTime(iso)} 修改了画布`);
  });

  it("详情缺失时用中性说法", () => {
    for (const cause of [conflictError(), new Error("x"), undefined, { data: { details: "坏值" } }]) {
      const details = conflictDetails(cause);
      expect(details.bySelf).toBe(false);
      expect(details.currentEditorName).toBe("");
      expect(overwriteLabel(details)).toBe("以我为准（覆盖服务器上的版本）");
      expect(conflictSummary(details)).toBe("画布已在别处被修改");
    }
    expect(overwriteLabel(null)).toBe("以我为准（覆盖服务器上的版本）");
    // 脏值不能被当成"是我"：那会让用户放心地覆盖掉同事的工作
    expect(conflictDetails(conflictError({ bySelf: "true" })).bySelf).toBe(false);
  });

  it("MCP 写入：不说成本人另一个窗口、也不说覆盖本人（MCPT-09）", () => {
    // MCP 写入时最后编辑者是授权所属的用户本人
    const details = conflictDetails(conflictError({ currentVersion: 5, currentEditorName: "林澈", editorKind: "mcp", bySelf: false }));
    expect(details.editorKind).toBe("mcp");
    expect(overwriteLabel(details)).toBe("以我为准（覆盖外部 AI 客户端的修改）");
    expect(conflictSummary(details)).toBe("外部 AI 客户端（代 林澈）修改了画布");
    expect(conflictDetails(conflictError({ editorKind: "robot" })).editorKind).toBeUndefined();
  });

  it("时间解析不了就不写时间", () => {
    expect(clockTime("不是时间")).toBe("");
    expect(conflictSummary(conflictDetails(conflictError({ currentUpdatedAt: "坏" })))).toBe("画布已在别处被修改");
  });
});

describe("本地草稿", () => {
  it("草稿键按项目区分", () => {
    expect(draftKey("p1")).not.toBe(draftKey("p2"));
    const storage = memoryStorage();
    expect(writeDraft(draft("p1"), storage)).toBe(true);
    expect(readDraft("p1", storage)?.baseVersion).toBe(3);
    expect(readDraft("p2", storage)).toBeNull();
    removeDraft("p1", storage);
    expect(readDraft("p1", storage)).toBeNull();
  });

  it("写不进去返回 false（调用方据此拒绝会丢改动的动作）", () => {
    const full = { ...memoryStorage(), setItem: () => { throw new Error("QuotaExceededError"); } };
    expect(writeDraft(draft("p1"), full)).toBe(false);
    expect(writeDraft(draft("p1"), undefined)).toBe(false);
  });

  it("坏草稿当没有", () => {
    const storage = memoryStorage();
    storage.setItem(draftKey("p1"), "{坏");
    expect(readDraft("p1", storage)).toBeNull();
    storage.setItem(draftKey("p1"), JSON.stringify({ ...draft("p2") }));
    expect(readDraft("p1", storage)).toBeNull();
  });
});
