import { describe, expect, it } from "vitest";
import { isProjectPackageFile, projectImportMessage, projectPackageFileName } from "./projectPackage";

describe("projectPackage", () => {
  it("文件名与后端同一规则", () => {
    expect(projectPackageFileName("a/b:c")).toBe("a_b_c.tabtv.zip");
    expect(projectPackageFileName("  ")).toBe("project.tabtv.zip");
    expect(projectPackageFileName("长".repeat(100))).toBe("长".repeat(80) + ".tabtv.zip");
  });

  it("只认 zip", () => {
    expect(isProjectPackageFile("片子.tabtv.zip")).toBe(true);
    expect(isProjectPackageFile("x.ZIP")).toBe(true);
    expect(isProjectPackageFile("x.json")).toBe(false);
  });

  it("有 warnings 时说出来，最多列三条", () => {
    expect(projectImportMessage({ projectId: "1", name: "P（导入）" })).toEqual({ text: "已导入为「P（导入）」", warn: false });
    const m = projectImportMessage({ projectId: "1", name: "P", warnings: ["a", "b", "c", "d"] });
    expect(m.warn).toBe(true);
    expect(m.text).toContain("a；b；c等 4 条");
  });
});
