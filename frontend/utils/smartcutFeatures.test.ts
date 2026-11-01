import { describe, expect, it } from "vitest";
import { smartcutAiEnabledFrom } from "./smartcutFeatures";
import { smartcutGuideStepsFor } from "./smartcutStudio";

describe("smartcutFeatures", () => {
  it("只有明确打开才算开（缺省关）", () => {
    expect([true, "true", "1", 1].map(smartcutAiEnabledFrom)).toEqual([true, true, true, true]);
    expect([undefined, false, "false", "", 0, "0", null].map(smartcutAiEnabledFrom)).toEqual([false, false, false, false, false, false, false]);
  });
  it("AI 关着时引导不讲 AI 剪辑", () => {
    expect(smartcutGuideStepsFor(false).map(s => s.title)).toEqual(["时间线怎么用", "导出"]);
    expect(smartcutGuideStepsFor(true).map(s => s.title)).toEqual(["时间线怎么用", "AI 剪辑", "导出"]);
  });
});
