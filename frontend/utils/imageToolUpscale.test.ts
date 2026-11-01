import { readFileSync } from "node:fs";
import { join } from "node:path";
import { describe, expect, it } from "vitest";
import { IMAGE_TOOL_FALLBACK_CONFIG } from "~/utils/imageTools";
import {
  imageToolUpscaleLevels,
  imageToolUpscaleModelCode,
  imageToolUpscaleMoveLevel,
  imageToolUpscaleOptionsOf
} from "~/utils/imageToolUpscale";

describe("高清：增强程度", () => {
  it("兜底配置是 标准 / 精细 / 极致 三档", () => {
    expect(imageToolUpscaleLevels(IMAGE_TOOL_FALLBACK_CONFIG.upscaleLevels)).toEqual([
      { code: "standard", name: "标准" }, { code: "fine", name: "精细" }, { code: "ultra", name: "极致" }
    ]);
  });

  it("配置决定名称与顺序；表外的码不显示；空表回固定三档", () => {
    expect(imageToolUpscaleLevels([{ code: "ultra", name: "最强" }, { code: "x4", name: "4 倍" }, { code: "standard", name: "常规" }]))
      .toEqual([{ code: "ultra", name: "最强" }, { code: "standard", name: "常规" }]);
    expect(imageToolUpscaleLevels([]).map(row => row.code)).toEqual(["standard", "fine", "ultra"]);
    expect(imageToolUpscaleLevels(undefined)).toHaveLength(3);
  });

  it("方向键在几档之间移动并绕回；Home / End；其它键返回 null", () => {
    const levels = imageToolUpscaleLevels(undefined);
    expect(imageToolUpscaleMoveLevel("standard", "ArrowRight", levels)).toBe("fine");
    expect(imageToolUpscaleMoveLevel("ultra", "ArrowRight", levels)).toBe("standard");
    expect(imageToolUpscaleMoveLevel("standard", "ArrowLeft", levels)).toBe("ultra");
    expect(imageToolUpscaleMoveLevel("fine", "Home", levels)).toBe("standard");
    expect(imageToolUpscaleMoveLevel("fine", "End", levels)).toBe("ultra");
    expect(imageToolUpscaleMoveLevel("fine", "Tab", levels)).toBeNull();
    expect(imageToolUpscaleMoveLevel("fine", "ArrowRight", [])).toBeNull();
  });
});

describe("高清：选项归一", () => {
  it("空值给「标准」", () => {
    expect(imageToolUpscaleOptionsOf(undefined)).toEqual({ level: "standard" });
  });

  it("合法值原样保留", () => {
    expect(imageToolUpscaleOptionsOf({ level: "ultra" })).toEqual({ level: "ultra" });
  });

  it("老节点上 Topaz 那一版的 provider / model / factor 丢掉，未知 level 回「标准」", () => {
    expect(imageToolUpscaleOptionsOf({ provider: "topaz", model: "high_fidelity", factor: 4 })).toEqual({ level: "standard" });
    expect(imageToolUpscaleOptionsOf({ level: "8k" })).toEqual({ level: "standard" });
  });
});

describe("高清：目录模型码", () => {
  const models = [{ code: "img_a", name: "A" }, { code: "img_b", name: "B" }];

  it("节点上的仍可用就沿用", () => {
    expect(imageToolUpscaleModelCode("img_b", models)).toBe("img_b");
  });

  it("不在列表里或没有时取第一个", () => {
    expect(imageToolUpscaleModelCode("gone", models)).toBe("img_a");
    expect(imageToolUpscaleModelCode(undefined, models)).toBe("img_a");
  });

  it("列表还没取到时沿用节点上的", () => {
    expect(imageToolUpscaleModelCode("img_b", undefined)).toBe("img_b");
    expect(imageToolUpscaleModelCode(undefined, [])).toBe("");
  });
});

describe("高清面板源码约束", () => {
  const source = readFileSync(join(__dirname, "../components/canvas/imagetools/UpscalePanel.vue"), "utf8");
  const template = source.slice(source.indexOf("<template>"));

  it("没有提示词输入框（IMG-F-58）", () => {
    expect(template).not.toMatch(/<textarea|type="text"|contenteditable/);
    expect(template).not.toMatch(/提示词/);
  });

  it("方案变更后没有服务商 / 倍数 / 上限；模型 + 增强程度两项", () => {
    expect(template).not.toMatch(/服务商|倍数|上限|topaz/i);
    expect(source).not.toMatch(/\bfactor\b|provider:|16384/);
    expect(template).toContain("upscale-model");
    expect(template).toContain("增强程度");
  });
});
