import { describe, expect, it } from "vitest";
import { effectivePrefs, mergePrefs, prefButtonText, prefModel, resolveReasoningModel, withPrefFields } from "./agentPrefs";

describe("Agent 模型偏好", () => {
  it("按节合并并保留不认识的键", () => {
    const merged = mergePrefs(
      { video: { model: "a", ratio: "9:16" }, futureKey: { x: 1 }, lastMode: "agent" },
      { video: { model: "b" }, reasoning: { model: "r" } }
    );
    expect(merged).toEqual({
      video: { model: "b", ratio: "9:16" },
      reasoning: { model: "r" },
      futureKey: { x: 1 },
      lastMode: "agent"
    });
  });

  it("改回智能匹配即去掉 model 字段", () => {
    const next = withPrefFields({ video: { model: "a", ratio: "16:9" } }, "video", { model: undefined });
    expect(next).toEqual({ video: { ratio: "16:9" } });
    expect(prefModel(next, "video")).toBeUndefined();
  });

  it("推理模型：所选不可用时退回第一个可用的", () => {
    const options = [
      { code: "x", available: false },
      { code: "y", available: true },
      { code: "z", available: true }
    ];
    expect(resolveReasoningModel({ reasoning: { model: "z" } }, options)).toBe("z");
    expect(resolveReasoningModel({ reasoning: { model: "x" } }, options)).toBe("y");
    expect(resolveReasoningModel({}, options)).toBe("y");
    expect(resolveReasoningModel({}, [])).toBeUndefined();
  });

  it("会话级偏好是完整快照：改回智能匹配不会被用户默认带回来", () => {
    const user = { video: { model: "a" } };
    const session = withPrefFields(user, "video", { model: undefined });
    expect(prefModel(effectivePrefs(user, session), "video")).toBeUndefined();
    expect(effectivePrefs(user, {})).toEqual(user);
    expect(effectivePrefs(user, null)).toEqual(user);
  });

  it("按钮文案", () => {
    expect(prefButtonText({}, [])).toBe("智能匹配模型｜创作偏好");
    expect(prefButtonText({ video: { model: "sd" } }, [{ code: "sd", name: "Seedance 2.0" }])).toBe("Seedance 2.0");
  });
});
