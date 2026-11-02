import { describe, expect, it } from "vitest";

import {
  controlOptions,
  currentValue,
  optionDisabled,
  optionLabel,
  optionSelected,
  optionValue,
  optionsSliderRange,
  restrictedOptionValues,
  restrictionFixes
} from "./controlOptions";

/**
 * 这组用例钉住的是一次真实故障：新后端把 `param_def.config.options` 从
 * **纯字符串数组**（老后端）换成了 **`[{label,value}]`**，而四个吃 options 的控件
 * 里有两个直接 `{{ option }}` —— 图片/视频节点的工具条上渲染出一排
 * `{ "label": "1:1", "value": "1:1" }`，把整条工具条撑出节点之外。
 *
 * ⚠️ 三个症状**没有一个会报错**，所以三个都要有断言：文案、选中态、写回的值。
 */
describe("控件选项归一", () => {
  const legacy = ["16:9", "9:16"];
  const current = [
    { label: "5 秒", value: "5" },
    { label: "10 秒", value: "10" }
  ];

  it("两种形态都取得出文案与值", () => {
    expect(optionLabel(legacy[0]!)).toBe("16:9");
    expect(optionValue(legacy[0]!)).toBe("16:9");
    expect(optionLabel(current[0]!)).toBe("5 秒");
    expect(optionValue(current[0]!)).toBe("5");
  });

  it("对象形态**绝不能**被 stringify 成文案", () => {
    // 这一条就是屏幕上那排花括号。
    expect(optionLabel(current[0]!)).not.toContain("{");
    expect(optionValue(current[0]!)).not.toContain("{");
  });

  it("选中态按归一后的值比，不比对象引用", () => {
    // ⚠️ `modelValue === option` 在对象形态下恒为 false —— 选中态永远不亮。
    expect(optionSelected(current[1]!, "10")).toBe(true);
    expect(optionSelected(current[1]!, "5")).toBe(false);
    expect(optionSelected(legacy[0]!, "16:9")).toBe(true);
  });

  it("数字选项按字符串比，避免类型不同而落空", () => {
    expect(optionValue({ label: "1 张", value: 1 })).toBe("1");
    expect(optionSelected({ label: "1 张", value: 1 }, 1)).toBe(true);
    expect(optionSelected({ label: "1 张", value: 1 }, "1")).toBe(true);
  });

  it("没有 label 时退回值本身，不留空白按钮", () => {
    expect(optionLabel({ value: "text2video" })).toBe("text2video");
    expect(optionLabel({ value: "x", label: "" })).toBe("x");
  });

  it("兼容 code 字段（模型/素材目录用的是 code 不是 value）", () => {
    expect(optionValue({ code: "kling_v2", name: "可灵 2.0" })).toBe("kling_v2");
    expect(optionLabel({ code: "kling_v2", name: "可灵 2.0" })).toBe("可灵 2.0");
  });

  it("未选时不命中任何一项", () => {
    expect(optionSelected(current[0]!, undefined)).toBe(false);
    expect(optionSelected(current[0]!, null)).toBe(false);
  });

  it("老数据里被整个存下的选项对象：按它的值显示与比较，不是「[object Object]」", () => {
    // 09-23 之前点一下会把整个选项对象写进 params；库里真实存着的就是这个形状。
    const stored = { label: "10 秒", value: "10" };
    expect(currentValue(stored)).toBe("10");
    expect(optionSelected({ label: "10 秒", value: "10" }, stored)).toBe(true);
    expect(optionSelected({ label: "5 秒", value: "5" }, stored)).toBe(false);
    expect(currentValue("16:9")).toBe("16:9");
    expect(currentValue(4)).toBe("4");
    expect(currentValue(undefined)).toBe("");
  });

  it("只有对象形态能被禁用", () => {
    expect(optionDisabled({ value: "first_last_frame", disabled: true })).toBe(true);
    expect(optionDisabled("16:9")).toBe(false);
  });

  it("配置缺失或形状不对时给空数组，不抛", () => {
    // 配置可以比前端新：拿不到选项要静默退化，不能让整个参数面板打不开。
    expect(controlOptions(undefined)).toEqual([]);
    expect(controlOptions({})).toEqual([]);
    expect(controlOptions({ options: "16:9" })).toEqual([]);
    expect(controlOptions({ options: current })).toHaveLength(2);
  });
});

/** 某些视频型号：1080p 下画幅只有三种（型号覆盖里的 restrictBy）。 */
const RATIO_CONFIG = {
  options: ["16:9", "9:16", "1:1", "4:3", "3:4", "21:9"].map(value => ({ label: value, value })),
  default: "16:9",
  restrictBy: { resolution: { "1080p": ["16:9", "9:16", "1:1"] } }
};

describe("参数之间的约束（restrictBy）", () => {
  it("1080p 时只剩三种画幅，720p 时不受限", () => {
    expect([...restrictedOptionValues(RATIO_CONFIG, { resolution: "1080p" }) ?? []]).toEqual(["16:9", "9:16", "1:1"]);
    expect(restrictedOptionValues(RATIO_CONFIG, { resolution: "720p" })).toBeNull();
    expect(restrictedOptionValues({ options: [] }, { resolution: "1080p" })).toBeNull();
  });

  it("老数据里存的是选项对象也认得出来", () => {
    expect(restrictedOptionValues(RATIO_CONFIG, { resolution: { label: "1080P", value: "1080p" } })?.has("4:3")).toBe(false);
  });

  it("切到 1080p 后 21:9 失效：校正回默认画幅", () => {
    expect(restrictionFixes({ resolution: "1080p", aspect_ratio: "21:9" }, { aspect_ratio: RATIO_CONFIG }))
      .toEqual({ aspect_ratio: "16:9" });
  });

  it("默认值也选不了时取第一个可选项；本来就合法、或还没取值的不动", () => {
    const config = { ...RATIO_CONFIG, default: "4:3" };
    expect(restrictionFixes({ resolution: "1080p", aspect_ratio: "21:9" }, { aspect_ratio: config }))
      .toEqual({ aspect_ratio: "16:9" });
    expect(restrictionFixes({ resolution: "1080p", aspect_ratio: "9:16" }, { aspect_ratio: RATIO_CONFIG })).toEqual({});
    expect(restrictionFixes({ resolution: "1080p" }, { aspect_ratio: RATIO_CONFIG })).toEqual({});
    expect(restrictionFixes({ resolution: "720p", aspect_ratio: "21:9" }, { aspect_ratio: RATIO_CONFIG })).toEqual({});
  });
});

describe("选项画成滑块", () => {
  const seconds = (lo: number, hi: number) => Array.from({ length: hi - lo + 1 }, (_, index) => {
    const value = String(lo + index);
    return { label: `${value} 秒`, value };
  });

  it("连续整数且够多：4~30 秒画成滑块", () => {
    expect(optionsSliderRange(seconds(4, 30))).toEqual({ min: 4, max: 30, step: 1 });
    expect(optionsSliderRange(["5", "10", "15", "20", "25", "30", "35"])).toEqual({ min: 5, max: 35, step: 5 });
  });

  it("档位少、不等差、不是整数时仍是按钮", () => {
    expect(optionsSliderRange(seconds(5, 6))).toBeNull();
    expect(optionsSliderRange(["4", "5", "8", "10", "15", "20", "30"])).toBeNull();
    expect(optionsSliderRange(["1:1", "16:9", "9:16", "4:3", "3:4", "21:9", "2:3"])).toBeNull();
    expect(optionsSliderRange(["1.5", "2", "2.5", "3", "3.5", "4", "4.5"])).toBeNull();
  });
});
