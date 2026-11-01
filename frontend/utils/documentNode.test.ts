import { describe, expect, it } from "vitest";
import { classifyDocumentFailure, documentConflictText, documentFileName, newDocumentPath } from "./documentNode";

describe("documentNode", () => {
  it("新建路径落在 /notes/ 下且以 .md 结尾", () => {
    const path = newDocumentPath(1_700_000_000_000, () => 0.5);
    expect(path).toMatch(/^\/notes\/doc-[0-9a-z]+-[0-9a-z]{4}\.md$/);
    expect(newDocumentPath(1, () => 0.1)).not.toBe(newDocumentPath(1, () => 0.9));
  });

  it("文件名取最后一段", () => {
    expect(documentFileName("/plan.md")).toBe("plan.md");
    expect(documentFileName("/notes/角色.md")).toBe("角色.md");
  });

  it("冲突按业务码 40902 判，40905 不算冲突", () => {
    const conflict = classifyDocumentFailure({ statusCode: 409, message: "x", data: { code: 40902, details: { currentVersion: 3, bySelf: false, currentEditorName: "小李" } } });
    expect(conflict).toEqual({ kind: "conflict", message: "小李 修改了这份文档", currentVersion: 3 });
    const trashed = classifyDocumentFailure({ statusCode: 409, message: "回收站中的项目不能修改文档，请先恢复", data: { code: 40905 } });
    expect(trashed.kind).toBe("other");
    expect(trashed.message).toContain("回收站");
  });

  it("404 / 403 有各自的说法", () => {
    expect(classifyDocumentFailure({ statusCode: 404 }).kind).toBe("not_found");
    expect(classifyDocumentFailure({ statusCode: 403 }).message).toContain("只读");
  });

  it("冲突措辞按字段分流", () => {
    expect(documentConflictText({ bySelf: true, currentEditorName: "我" })).toBe("你在另一个窗口修改过这份文档");
    expect(documentConflictText({ currentSource: "agent" })).toBe("Agent 刚修改了这份文档");
    expect(documentConflictText({})).toBe("文档已被修改");
  });

  // DRD-46：默认措辞逐字不变（现有文档节点的调用点不传 subject）。
  it("documentConflictText：默认措辞不变", () => {
    expect(documentConflictText({ currentEditorName: " 小李 " })).toBe("小李 修改了这份文档");
    expect(documentConflictText({ currentSource: "agent", currentEditorName: "小李" })).toBe("小李 修改了这份文档");
    expect(documentConflictText({ currentSource: "user" })).toBe("文档已被修改");
  });

  it("documentConflictText：subject 为这个场景", () => {
    const s = "这个场景";
    expect(documentConflictText({ bySelf: true }, s)).toBe("你在另一个窗口修改过这个场景");
    expect(documentConflictText({ currentSource: "agent" }, s)).toBe("Agent 刚修改了这个场景");
    expect(documentConflictText({ currentEditorName: "苏夏" }, s)).toBe("苏夏 修改了这个场景");
    expect(documentConflictText({}, s)).toBe("场景已被修改");
  });

  // MCP 写入的最后编辑者是授权所属的用户本人：不能说成「林澈 修改了」。
  it("documentConflictText：mcp 来源说外部 AI 客户端", () => {
    expect(documentConflictText({ currentSource: "mcp", currentEditorName: "林澈" })).toBe("外部 AI 客户端刚修改了这份文档");
    expect(documentConflictText({ editorKind: "mcp", currentEditorName: "林澈" }, "这个场景")).toBe("外部 AI 客户端刚修改了这个场景");
    // 本人另一个窗口仍优先（服务端对 mcp 写入不会给 bySelf=true，这里只钉住分流顺序）。
    expect(documentConflictText({ bySelf: true, currentSource: "mcp" }, "这个场景")).toBe("你在另一个窗口修改过这个场景");
  });
});
