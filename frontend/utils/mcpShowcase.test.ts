import { readFileSync } from "node:fs";
import { describe, expect, it } from "vitest";
import { SHOWCASE_DEMOS, showcaseOf } from "./mcpShowcase";

// 工具清单取自生成物 api/mcp/tools.json（与运行时注册表同一个构造函数）。
interface ToolDef { name: string; methods?: { name: string; paid: boolean }[] }
const tools: ToolDef[] = JSON.parse(readFileSync(new URL("../../api/mcp/tools.json", import.meta.url), "utf8"));

function paidOf(ref: string): boolean | undefined {
  const [name, method = ""] = ref.split(".");
  const tool = tools.find((t) => t.name === name);
  if (!tool) return undefined;
  const m = (tool.methods ?? []).find((x) => x.name === method);
  return m?.paid;
}

describe("能力示意", () => {
  it("三个页签依次是生成、画布素材、脚本到成片", () => {
    expect(SHOWCASE_DEMOS.map((d) => d.key)).toEqual(["generate", "canvas", "film"]);
    expect(showcaseOf("nope").key).toBe("generate");
    expect(showcaseOf("film").key).toBe("film");
  });

  it("只出现真实存在的工具与动作", () => {
    for (const demo of SHOWCASE_DEMOS) {
      for (const step of demo.steps) {
        if (step.kind !== "call") continue;
        expect(paidOf(step.call.tool), `${demo.key}: ${step.call.tool}`).not.toBeUndefined();
      }
    }
  });

  it("每个花积分的调用之前都有一次确认，且标了花费；不花钱的不标", () => {
    for (const demo of SHOWCASE_DEMOS) {
      demo.steps.forEach((step, i) => {
        if (step.kind !== "call") return;
        const paid = paidOf(step.call.tool);
        if (paid) {
          expect(demo.steps[i - 1]?.kind, `${demo.key}: ${step.call.tool} 之前没有确认`).toBe("confirm");
          expect(step.call.cost).toBeTruthy();
        } else {
          expect(step.call.cost, `${demo.key}: ${step.call.tool} 不花钱却标了花费`).toBeUndefined();
        }
      });
    }
  });

  it("生成示意先报价、再确认、后生成", () => {
    const tools = showcaseOf("generate").steps.map((s) => (s.kind === "call" ? s.call.tool : "confirm"));
    const quote = tools.indexOf("quote");
    const confirm = tools.indexOf("confirm");
    const generate = tools.indexOf("generate");
    expect(quote).toBeGreaterThanOrEqual(0);
    expect(quote).toBeLessThan(confirm);
    expect(confirm).toBeLessThan(generate);
  });
});
