import { describe, expect, it } from "vitest";
import { buildExport, exportFileName, importSummary, parseImport, planImport } from "./settingsTransfer";

const file = (o: unknown) => JSON.stringify(o);

describe("parseImport", () => {
  it("导出的文件能原样导回", () => {
    const f = buildExport("skills", [{ code: "a", name: "A" }], new Date("2026-11-02T00:00:00Z"));
    const r = parseImport(file(f), "skills");
    expect(r).toEqual({ ok: true, items: [{ code: "a", name: "A" }] });
  });
  it("非法文件的错误文案", () => {
    const cases: Array<[string, string]> = [
      ["{oops", "不是合法的 JSON 文件"],
      [file({ items: [] }), "不是 tabtv 设置导出的文件"],
      [file({ format: "tabtv.settings", kind: "presets", version: 1, items: [] }), "这是「图片预设」的导出文件，不能导入到「Agent 技能」"],
      [file({ format: "tabtv.settings", kind: "what", version: 1, items: [] }), "文件类型「what」认不出"],
      [file({ format: "tabtv.settings", kind: "skills", version: 9, items: [] }), "比本应用新"],
      [file({ format: "tabtv.settings", kind: "skills", version: 1 }), "没有 items 列表"],
      [file({ format: "tabtv.settings", kind: "skills", version: 1, items: [] }), "一条都没有"],
      [file({ format: "tabtv.settings", kind: "skills", version: 1, items: [3] }), "第 1 项不是对象"],
      [file({ format: "tabtv.settings", kind: "skills", version: 1, items: [{ name: "x" }] }), "第 1 项缺少 code"],
      [file({ format: "tabtv.settings", kind: "skills", version: 1, items: [{ code: "a" }, { code: " a " }] }), "第 2 项的 code「a」与前面重复"]
    ];
    for (const [text, want] of cases) {
      const r = parseImport(text, "skills");
      expect(r.ok).toBe(false);
      if (!r.ok) expect(r.error).toContain(want);
    }
  });
  it("提示词按用途 + 模型去重", () => {
    const f = (items: unknown[]) => file({ format: "tabtv.settings", kind: "prompts", version: 1, items });
    expect(parseImport(f([{ code: "a" }, { code: "a", modelCode: "m" }]), "prompts").ok).toBe(true);
    const r = parseImport(f([{ code: "a", modelCode: "m" }, { code: "a", modelCode: "m" }]), "prompts");
    expect(r.ok).toBe(false);
    if (!r.ok) expect(r.error).toContain("a/m");
  });
  it("营销角色按名称做主键", () => {
    const r = parseImport(file({ format: "tabtv.settings", kind: "marketing-characters", version: 1, items: [{ code: "x" }] }), "marketing-characters");
    expect(r.ok).toBe(false);
    if (!r.ok) expect(r.error).toContain("缺少 name");
  });
});

describe("planImport / importSummary / exportFileName", () => {
  it("按 key 分出新建与覆盖", () => {
    const plan = planImport([{ code: "a" }, { code: "b" }], [{ code: "a", id: "1" }], e => e.code, "code");
    expect(plan.creates).toEqual([{ code: "b" }]);
    expect(plan.updates[0]?.current.id).toBe("1");
  });
  it("失败逐条列出", () => {
    expect(importSummary(1, 2, [])).toBe("新建 1 条，覆盖 2 条");
    expect(importSummary(0, 0, [{ key: "a", message: "坏了" }])).toContain("1 条失败：a（坏了）");
  });
  it("文件名带日期", () => {
    expect(exportFileName("presets", new Date(2026, 10, 2))).toBe("tabtv-presets-20261102.json");
  });
});
