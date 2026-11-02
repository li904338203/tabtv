import { describe, expect, it } from "vitest";
import { MODE_META, directTriggerParts, modelBadges, modelOptionNote, prefTriggerParts, ratioIconBox } from "./composerUi";

describe("统一输入框展示", () => {
  it("画幅图标长边固定、短边按比例且不小于 4px，解析不出给正方形", () => {
    expect(ratioIconBox("16:9")).toEqual({ width: 14, height: 8 });
    expect(ratioIconBox("9:16")).toEqual({ width: 8, height: 14 });
    expect(ratioIconBox("1:1")).toEqual({ width: 14, height: 14 });
    expect(ratioIconBox("21:9", 14).height).toBeGreaterThanOrEqual(4);
    expect(ratioIconBox("auto")).toEqual({ width: 14, height: 14 });
  });

  it("偏好胶囊：未选为智能匹配，选了显示模型名，目录里没有时显示编码", () => {
    expect(prefTriggerParts({}, [])).toEqual({ model: "智能匹配模型", tail: "创作偏好", custom: false });
    expect(prefTriggerParts({ video: { model: "sd2" } }, [{ code: "sd2", name: "Seedance 2.0" }]).model).toBe("Seedance 2.0");
    expect(prefTriggerParts({ video: { model: "gone" } }, []).model).toBe("gone");
  });

  it("直接生成胶囊：显示当前模式所选模型名，没选为「自定义模型」", () => {
    expect(directTriggerParts(undefined, [])).toEqual({ model: "自定义模型", tail: "创作偏好", custom: false });
    expect(directTriggerParts("g1", [{ code: "g1", name: "Grok Imagine Video" }])).toEqual({ model: "Grok Imagine Video", tail: "创作偏好", custom: true });
    expect(directTriggerParts("gone", []).model).toBe("gone");
  });

  it("没有会员体系：不出会员角标（TT-T-14）", () => {
    expect(modelBadges({ minMemberLevelId: "3" })).toEqual([]);
    expect(modelBadges({})).toEqual([]);
  });

  it("模型说明行：不可用写原因，可用不写价格", () => {
    expect(modelOptionNote({ code: "a", name: "A", available: false, reason: "未配置" })).toBe("不可用：未配置");
    expect(modelOptionNote({ code: "a", name: "A", available: true })).toBe("");
    expect(modelOptionNote({ code: "r", name: "R", available: true }, true)).toBe("");
  });

  it("每个模式都有说明", () => {
    for (const meta of Object.values(MODE_META)) expect(meta.description).toBeTruthy();
  });
});
