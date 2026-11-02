import { describe, expect, it } from "vitest";
import { builtinBadge, canRestore, promptBadge } from "./builtinStatus";

describe("builtinBadge", () => {
  it("内置 / 内置已修改 / 自建", () => {
    expect(builtinBadge({ isBuiltin: true }).label).toBe("内置");
    expect(builtinBadge({ isBuiltin: true, isUserModified: true }).label).toBe("内置 · 已修改");
    expect(builtinBadge({ isBuiltin: true, isUserModified: true }).restorable).toBe(true);
    expect(builtinBadge({ isBuiltin: false, isUserModified: true }).state).toBe("custom");
    expect(builtinBadge({}).state).toBe("custom");
  });
  it("只有内置条目能恢复", () => {
    expect(canRestore({ isBuiltin: true })).toBe(true);
    expect(canRestore({ isBuiltin: false })).toBe(false);
  });
});

describe("promptBadge", () => {
  const spec = { code: "a", hasBuiltin: true };
  it("有已发布的覆盖行才算已修改；只有草稿或已归档仍是内置", () => {
    expect(promptBadge(spec, []).state).toBe("builtin");
    expect(promptBadge(spec, [{ code: "a", status: "draft", version: 0, content: "" }]).state).toBe("builtin");
    expect(promptBadge(spec, [{ code: "a", status: "archived", version: 1, content: "" }]).state).toBe("builtin");
    expect(promptBadge(spec, [{ code: "a", modelCode: "m", status: "published", version: 1, content: "" }]).state).toBe("modified");
    expect(promptBadge(spec, [{ code: "b", status: "published", version: 1, content: "" }]).state).toBe("builtin");
  });
  it("没有内置原文的用途按自建显示", () => {
    expect(promptBadge({ code: "a", hasBuiltin: false }, []).state).toBe("custom");
  });
});
