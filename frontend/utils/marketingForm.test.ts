import { describe, expect, it } from "vitest";
import {
  categoryIssue, characterDraft, characterExportItem, characterImportPayload, characterIssue, characterPayload,
  marketingCoverLimit, marketingLifecycleTag, marketingMakePrimary, optionDraft, optionExportItem, optionImportPayload, optionIssue, optionNotes,
  optionPayload, marketingIssuesOf
} from "./marketingForm";

const creative = {
  id: "1", kind: "creative", code: "unbox_show", name: "开箱", categoryCode: "show", description: "说明",
  coverMediaIds: ["11"], previewMediaId: "12", prompt: " 片段 ", outputTypes: ["video"], inputRequirements: { product: "required", weird: "x" },
  isAudioRequired: true, recommendedModels: ["m1"], recommendedParams: { aspectRatio: "9:16", durationSec: [5, 10], extra: 1 },
  defaultSkillCode: "s1", tags: ["口播"], startAt: "2026-11-01T00:00:00Z", endAt: null, sort: 20, isEnabled: true, version: 4
};

describe("选项往返", () => {
  it("推荐参数拆开再合回去，不认识的键不丢", () => {
    const d = optionDraft(creative);
    expect(d.aspectRatio).toBe("9:16");
    expect(d.durationMin).toBe(5);
    const p = optionPayload(d, "update");
    expect(p.recommendedParams).toEqual({ extra: 1, aspectRatio: "9:16", durationSec: [5, 10] });
    expect(p.prompt).toBe(" 片段 ");
    expect(p.inputRequirements).toEqual({ product: "required", weird: "x" });
    expect(p.version).toBe(4);
    expect(p.kind).toBeUndefined();
    expect(p.endAt).toBeNull();
  });
  it("风格没有出声与推荐项；新建带 kind+code", () => {
    const p = optionPayload(optionDraft({ ...creative, kind: "style" }), "create");
    expect(p.isAudioRequired).toBeUndefined();
    expect(p.recommendedModels).toBeUndefined();
    expect(p.kind).toBe("style");
    expect(p.code).toBe("unbox_show");
    expect(marketingCoverLimit("style")).toBe(1);
  });
  it("同一个时长只发一个数", () => {
    const d = { ...optionDraft(creative), durationMin: 5, durationMax: 5 };
    expect(optionPayload(d, "update").recommendedParams).toMatchObject({ durationSec: [5] });
  });
});

describe("选项校验", () => {
  const base = optionDraft(creative);
  it("逐条报", () => {
    expect(optionIssue(base, true)).toBe("");
    expect(optionIssue({ ...base, code: "ab" }, true)).toContain("3~64");
    expect(optionIssue({ ...base, name: "" }, false)).toBe("名称不能为空");
    expect(optionIssue({ ...base, name: "字".repeat(25) }, false)).toContain("最多 24 字");
    expect(optionIssue({ ...base, categoryCode: "" }, false)).toBe("请选择分类");
    expect(optionIssue({ ...base, outputTypes: [] }, false)).toContain("至少选一种");
    expect(optionIssue({ ...base, endAt: "2026-10-01T00:00:00Z" }, false)).toContain("结束时间");
    expect(optionIssue({ ...base, coverMediaIds: ["1", "2", "3", "4"] }, false)).toContain("最多 3 张");
    expect(optionIssue({ ...base, durationMin: 9, durationMax: 3 }, false)).toContain("下限");
  });
  it("没封面只提醒不挡", () => {
    expect(optionIssue({ ...base, coverMediaIds: [] }, false)).toBe("");
    expect(optionNotes({ ...base, coverMediaIds: [] })[0]).toContain("还没有封面");
  });
  it("投放状态与服务端问题", () => {
    expect(marketingLifecycleTag("live").label).toBe("在投");
    expect(marketingLifecycleTag("odd").label).toBe("odd");
    expect(marketingIssuesOf({ issues: [{ field: "coverMediaIds", message: "缺封面" }] })).toEqual(["coverMediaIds：缺封面"]);
  });
});

describe("选项导出导入", () => {
  it("导出不带媒体与版本；导入覆盖保留现有封面", () => {
    const e = optionExportItem(creative);
    expect(e.coverMediaIds).toBeUndefined();
    expect(e.version).toBeUndefined();
    expect(e.code).toBe("unbox_show");
    const upd = optionImportPayload(e, { coverMediaIds: ["99"], previewMediaId: null, version: 8 });
    expect(upd.coverMediaIds).toEqual(["99"]);
    expect(upd.version).toBe(8);
    const created = optionImportPayload(e, null);
    expect(created.kind).toBe("creative");
    expect(created.coverMediaIds).toEqual([]);
  });
});

describe("分类", () => {
  it("新建校验 code 与重复；名称必填", () => {
    const c = { id: "", kind: "hook" as const, code: "open", name: "开头", sort: 1, isEnabled: true };
    expect(categoryIssue(c, true, [])).toBe("");
    expect(categoryIssue({ ...c, code: "1" }, true, [])).toContain("2~48");
    expect(categoryIssue(c, true, [{ kind: "hook", code: "open" }])).toContain("已存在");
    expect(categoryIssue(c, true, [{ kind: "style", code: "open" }])).toBe("");
    expect(categoryIssue({ ...c, name: "" }, false, [])).toBe("分类名称不能为空");
  });
});

describe("预置角色", () => {
  const row = { id: "3", name: "小美", summary: "简介", imageMediaIds: ["1", "2"], appearance: "长发", voiceText: "温柔", voiceMediaId: "7", version: 2 };
  it("往返；音频空串发 null", () => {
    const p = characterPayload(characterDraft(row), "update");
    expect(p).toEqual({ name: "小美", summary: "简介", imageMediaIds: ["1", "2"], appearance: "长发", voiceText: "温柔", voiceMediaId: "7", version: 2 });
    expect(characterPayload({ ...characterDraft(row), voiceMediaId: "" }, "create").voiceMediaId).toBeNull();
  });
  it("名称规则", () => {
    expect(characterIssue(characterDraft(row))).toBe("");
    expect(characterIssue(characterDraft({ name: "a@b" }))).toContain("「@」");
    expect(characterIssue(characterDraft({ name: "" }))).toBe("名称不能为空");
    expect(characterIssue(characterDraft({ name: "a", imageMediaIds: Array.from({ length: 10 }, (_, i) => String(i)) }))).toContain("最多 9 张");
  });
  it("设主图", () => {
    expect(marketingMakePrimary(["a", "b", "c"], 2)).toEqual(["c", "a", "b"]);
    expect(marketingMakePrimary(["a"], 0)).toEqual(["a"]);
  });
  it("导出不带媒体；导入覆盖保留现有图片与音频", () => {
    const e = characterExportItem(row);
    expect(e).toEqual({ name: "小美", summary: "简介", appearance: "长发", voiceText: "温柔" });
    const upd = characterImportPayload(e, { imageMediaIds: ["1"], voiceMediaId: "7", version: 5 });
    expect(upd.imageMediaIds).toEqual(["1"]);
    expect(upd.version).toBe(5);
    expect(characterImportPayload(e, null).imageMediaIds).toEqual([]);
  });
});
