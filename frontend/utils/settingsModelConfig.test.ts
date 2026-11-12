import { describe, expect, it } from "vitest";
import { modelConfigFileName, modelConfigOutcome, modelConfigPreviewLines, parseModelConfigText, type ModelConfigReport } from "./settingsModelConfig";

const report = (over: Partial<ModelConfigReport> = {}): ModelConfigReport => ({
  dryRun: true,
  problems: [],
  providers: [
    { code: "openai", name: "OpenAI", action: "update", status: "enabled", hasKey: true, ok: false },
    { code: "relay", name: "中转", action: "create", status: "enabled", hasKey: true, ok: false }
  ],
  models: [{ code: "m1", name: "模型一", action: "create", status: "enabled", ok: false }],
  storage: "none",
  localOnly: { providers: [], models: [] },
  restartRequired: false,
  ...over
});

describe("settingsModelConfig", () => {
  it("文件名带日期", () => {
    expect(modelConfigFileName(new Date(2026, 10, 5))).toBe("tabtv-model-config-20261105.json");
  });

  it("只认模型配置文件，别的设置文件说清去哪导", () => {
    expect(parseModelConfigText("{")).toEqual({ ok: false, error: "文件不是合法的 JSON" });
    expect(parseModelConfigText("[]").ok).toBe(false);
    const r = parseModelConfigText(JSON.stringify({ kind: "skills" }));
    expect(r.ok).toBe(false);
    expect(!r.ok && r.error).toContain("技能");
    expect(parseModelConfigText(JSON.stringify({ kind: "model-config" })).ok).toBe(true);
  });

  it("预览：新建 / 覆盖分开数，说清 Key 与本机多出来的", () => {
    const lines = modelConfigPreviewLines(report({ storage: "set", storageEndpoint: "https://s3", localOnly: { providers: ["x"], models: ["a", "b"] } }));
    expect(lines[0]).toBe("厂商 2 个：新建 1，覆盖 1");
    expect(lines[1]).toBe("模型 1 个：新建 1，覆盖 0");
    expect(lines[2]).toContain("2 个厂商的 API Key");
    expect(lines.join("\n")).toContain("重启");
    expect(lines.join("\n")).toContain("1 个厂商、2 个模型不在文件里");
  });

  it("没有 Key 时说明保留本机的", () => {
    const r = report();
    r.providers.forEach(p => { p.hasKey = false; });
    expect(modelConfigPreviewLines(r)[2]).toContain("保留");
  });

  it("结果：失败与提醒各自带名字", () => {
    const r = report({ dryRun: false, problems: ["坏了"] });
    r.providers[0]!.ok = true;
    r.providers[1]!.ok = true;
    r.models[0]!.error = "已导入但没有启用：缺价格";
    r.providers[0]!.warnings = ["没有停用：仍有模型"];
    const o = modelConfigOutcome(r);
    expect(o.ok).toBe(2);
    expect(o.failed).toEqual([
      { label: "模型 模型一", message: "已导入但没有启用：缺价格" },
      { label: "配置", message: "坏了" }
    ]);
    expect(o.warned).toEqual([{ label: "厂商 OpenAI", message: "没有停用：仍有模型" }]);
  });
});
