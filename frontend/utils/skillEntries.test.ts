import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { describe, expect, it } from "vitest";
import { MAX_SKILL_ENTRIES, isBatchSkill, quickSkillEntries } from "./skillEntries";

const skill = (code: string, sort: number, isQuickEntry = true) => ({ code, name: code, sort, isQuickEntry });

describe("技能快捷入口（MKT-F-20~22）", () => {
  it("只取勾了快捷入口的，按 sort 升序，最多 6 个", () => {
    const list = [
      skill("h", 8), skill("a", 1), skill("x", 0, false), skill("c", 3), skill("b", 2),
      skill("g", 7), skill("e", 5), skill("d", 4), skill("f", 6)
    ];
    expect(MAX_SKILL_ENTRIES).toBe(6);
    expect(quickSkillEntries(list).map(item => item.code)).toEqual(["a", "b", "c", "d", "e", "f"]);
  });

  it("同 sort 保持目录顺序；不改入参数组", () => {
    const list = [skill("z", 1), skill("y", 1), skill("w", 0)];
    const before = list.map(item => item.code);
    expect(quickSkillEntries(list).map(item => item.code)).toEqual(["w", "z", "y"]);
    expect(list.map(item => item.code)).toEqual(before);
  });

  it("可传更小的上限；没有快捷入口时为空", () => {
    expect(quickSkillEntries([skill("a", 1), skill("b", 2)], 1).map(item => item.code)).toEqual(["a"]);
    expect(quickSkillEntries([skill("a", 1, false)])).toEqual([]);
  });

  it("芯片只来自服务端目录：两个页面都不写死数字、不追加「每日热点趋势」之类的入口（MKT-AC-23）", () => {
    for (const page of ["../pages/create.vue", "../pages/marketing.vue"]) {
      const source = readFileSync(resolve(__dirname, page), "utf8");
      expect(source).toContain("quickSkillEntries(");
      expect(source).not.toMatch(/MAX_SKILL_ENTRIES\s*=/);
      expect(source).not.toContain("热点");
    }
  });

  it("批量技能读目录里的 isBatch，不按 code 认（MKT-T-32）", () => {
    const list = [
      { code: "renamed_batch", isBatch: true },
      { code: "mk_batch_variants", isBatch: false },
      { code: "mk_product_ad", isBatch: false }
    ];
    expect(isBatchSkill("renamed_batch", list)).toBe(true);
    // 旧的硬编码 code 不再有特权：标志说不是就不是
    expect(isBatchSkill("mk_batch_variants", list)).toBe(false);
    expect(isBatchSkill("mk_product_ad", list)).toBe(false);
    // 目录里没有 / 目录未加载 / 没选技能：按不是批量
    expect(isBatchSkill("unknown", list)).toBe(false);
    expect(isBatchSkill("renamed_batch", undefined)).toBe(false);
    expect(isBatchSkill(undefined, list)).toBe(false);
    expect(isBatchSkill("", list)).toBe(false);
  });

  it("源码里不再写死批量技能的 code", () => {
    for (const file of ["./skillEntries.ts", "../components/agent/composer/AgentModeComposer.vue"]) {
      const source = readFileSync(resolve(__dirname, file), "utf8");
      expect(source).not.toMatch(/["'`]mk_batch_variants["'`]|["'`]mk_material_fission["'`]/);
    }
  });
});
