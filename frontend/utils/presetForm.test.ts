import { describe, expect, it } from "vitest";
import { groupPresets, parsePresetDimensions, parsePresetParams, presetDraft, presetExportItem, presetPayload, validatePresetDraft } from "./presetForm";

const row = {
  code: "relight", name: "重新打光", presetGroup: "tool", spec: "retouch", inputMode: "image_required", icon: "sun",
  prompt: "重新打光", params: { presetCode: "relight" },
  dimensions: [{ code: "dir", name: "方向", default: "left", options: [{ value: "left", prompt: "左侧光" }, { value: "right" }] }],
  operation: "img2img", sort: 5, description: "说明", isNew: true, isEnabled: false, isBuiltin: true, isUserModified: true
};

describe("presetDraft / presetPayload 往返", () => {
  it("字段逐个对得上", () => {
    const p = presetPayload(presetDraft(row));
    expect(p).toEqual({
      code: "relight", name: "重新打光", presetGroup: "tool", spec: "retouch", inputMode: "image_required", icon: "sun",
      operation: "img2img", prompt: "重新打光", params: { presetCode: "relight" }, dimensions: row.dimensions,
      description: "说明", isNew: true, sort: 5
    });
  });
  it("没有维度时传 null；新建默认启用", () => {
    const d = presetDraft(null, 30);
    expect(d.isEnabled).toBe(true);
    expect(d.sort).toBe(30);
    expect(presetPayload({ ...d, prompt: "x" }).dimensions).toBeNull();
    expect(presetPayload({ ...d, paramsText: "" }).params).toEqual({});
  });
});

describe("JSON 字段", () => {
  it("参数默认必须是对象", () => {
    expect(parsePresetParams("[1]").error).toContain("JSON 对象");
    expect(parsePresetParams("{").error).toContain("不是合法的 JSON");
  });
  it("维度逐条校验", () => {
    expect(parsePresetDimensions("").value).toBeNull();
    expect(parsePresetDimensions("{}").error).toContain("JSON 数组");
    expect(parsePresetDimensions('[{"options":[]}]').error).toContain("缺少 code");
    expect(parsePresetDimensions('[{"code":"a","options":[]}]').error).toContain("至少要有一个选项");
    expect(parsePresetDimensions('[{"code":"a","options":[{"label":"x"}]}]').error).toContain("缺少 value");
    expect(parsePresetDimensions('[{"code":"a","default":"z","options":[{"value":"x"}]}]').error).toContain("不在选项里");
  });
});

describe("validatePresetDraft", () => {
  const ctx = { creating: true, list: [{ code: "relight" }], operations: ["img2img", "text2img"] };
  const base = presetDraft({ ...row, code: "new_one" });
  it("合法的过", () => expect(validatePresetDraft(base, ctx)).toBe(""));
  it("逐条报", () => {
    expect(validatePresetDraft({ ...base, code: "1x" }, ctx)).toContain("编码");
    expect(validatePresetDraft({ ...base, code: "relight" }, ctx)).toContain("已存在");
    expect(validatePresetDraft({ ...base, name: "" }, ctx)).toBe("名称不能为空");
    expect(validatePresetDraft({ ...base, presetGroup: "x" }, ctx)).toBe("请选择分组");
    expect(validatePresetDraft({ ...base, spec: "x" }, ctx)).toBe("请选择产出类型");
    expect(validatePresetDraft({ ...base, inputMode: "x" }, ctx)).toBe("请选择输入要求");
    expect(validatePresetDraft({ ...base, prompt: " " }, ctx)).toBe("配方提示词不能为空");
    expect(validatePresetDraft({ ...base, operation: "nope" }, ctx)).toContain("不存在");
    expect(validatePresetDraft({ ...base, paramsText: "[]" }, ctx)).toContain("JSON 对象");
    expect(validatePresetDraft({ ...base, code: "relight" }, { ...ctx, creating: false })).toBe("");
  });
});

describe("groupPresets", () => {
  it("按工作顺序不按字典序，组内按 sort，未知分组殿后", () => {
    const out = groupPresets([
      { code: "a", presetGroup: "space", sort: 1 },
      { code: "b", presetGroup: "sheet", sort: 9 },
      { code: "c", presetGroup: "sheet", sort: 2 },
      { code: "d", presetGroup: "zzz", sort: 0 },
      { code: "e", presetGroup: "tuning", sort: 0 }
    ]);
    expect(out.map(g => g.group)).toEqual(["sheet", "tuning", "space", "zzz"]);
    expect(out[0]!.items.map(i => i.code)).toEqual(["c", "b"]);
    expect(out[3]!.label).toBe("zzz");
  });
});

describe("presetExportItem", () => {
  it("去掉本机状态", () => {
    const e = presetExportItem(row);
    expect(e.isBuiltin).toBeUndefined();
    expect(e.isEnabled).toBeUndefined();
    expect(e.prompt).toBe("重新打光");
  });
});
