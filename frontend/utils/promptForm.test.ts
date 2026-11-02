import { describe, expect, it } from "vitest";
import { activePromptRows, promptInitialContent, insertPromptPlaceholder, normalizePromptSpec, promptPlaceholdersOf, promptIssues, promptSampleVars } from "./promptForm";

const spec = normalizePromptSpec({ code: "storyboard_gen.system", name: "分镜", required: ["story"], optional: ["style"], maxChars: 50, builtin: "内置{{story}}", hasBuiltin: true });

describe("normalizePromptSpec", () => {
  it("缺字段兜成空数组，不让一个字段拖垮整页", () => {
    const s = normalizePromptSpec({ code: "x", required: null });
    expect(s.required).toEqual([]);
    expect(s.optional).toEqual([]);
    expect(s.name).toBe("x");
    expect(s.hasBuiltin).toBe(false);
  });
});

describe("promptPlaceholdersOf", () => {
  it("去重排序", () => {
    expect(promptPlaceholdersOf("{{b}}{{a}}{{ b }}").names).toEqual(["a", "b"]);
  });
  it("未闭合 / 空 / 多余都报错", () => {
    expect(promptPlaceholdersOf("{{a").error).toContain("未闭合");
    expect(promptPlaceholdersOf("{{}}").error).toContain("空占位符");
    expect(promptPlaceholdersOf("a}}").error).toContain("多余");
  });
});

describe("promptIssues", () => {
  it("超上限、未知变量、缺必需变量逐条报", () => {
    expect(promptIssues("x".repeat(51) + "{{story}}", spec)).toContain("超过上限 50");
    expect(promptIssues("{{story}}{{nope}}", spec)).toContain("未知占位符：nope");
    expect(promptIssues("{{style}}", spec)).toContain("缺少必需占位符：story");
    expect(promptIssues("  ", spec)).toBe("内容不能为空");
    expect(promptIssues("{{story}}", spec)).toBe("");
    expect(promptIssues("x", undefined)).toBe("未知的提示词用途");
  });
  it("按码点计数（中文与表情不算多字）", () => {
    expect(promptIssues("😀".repeat(40) + "{{story}}", spec)).toBe("");
  });
});

describe("insertPromptPlaceholder / promptSampleVars", () => {
  it("插在光标处并给出新光标", () => {
    expect(insertPromptPlaceholder("ab", 1, "x")).toEqual({ content: "a{{x}}b", cursor: 6 });
    expect(insertPromptPlaceholder("ab", 99, "x").content).toBe("ab{{x}}");
  });
  it("样例变量给全（含可选的）", () => {
    expect(promptSampleVars(spec, { story: "s" })).toEqual({ story: "s", style: "" });
  });
});

describe("覆盖行与初始内容", () => {
  const rows = [
    { code: "storyboard_gen.system", status: "published", version: 2, content: "线上", draftContent: "草稿" },
    { code: "storyboard_gen.system", modelCode: "m", status: "archived", version: 1, content: "旧" }
  ];
  it("归档的行不算生效", () => {
    expect(activePromptRows(rows, "storyboard_gen.system")).toHaveLength(1);
  });
  it("有覆盖行取草稿；没有从内置原文起步", () => {
    expect(promptInitialContent(rows[0]!, spec)).toBe("草稿");
    expect(promptInitialContent({ ...rows[0]!, draftContent: "" }, spec)).toBe("线上");
    expect(promptInitialContent(null, spec)).toBe("内置{{story}}");
    expect(promptInitialContent(rows[1]!, spec)).toBe("内置{{story}}");
  });
});
