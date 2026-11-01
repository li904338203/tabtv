import { readdirSync, readFileSync } from "node:fs";
import { join, resolve } from "node:path";
import { describe, expect, it } from "vitest";

/**
 * 用户端排除项断言（MKT-T-32 / MKT-AC-23）：营销 Agent 不做「每日热点趋势」（MK-4）与
 * 数字人 / 口型驱动（MK-5）—— 侧栏、营销页、模板墙、营销输入框、快捷芯片的数据源里都不能出现它们的入口或文案。
 *
 * ⚠️ 扫的是**去掉注释后**的源码：注释里写「不做每日热点」恰恰是在钉住这条规则，不能算违规。
 * ⚠️ 只扫营销相关的文件（侧栏 + 营销页 + 营销组件 + 营销纯函数）：老首页的注释与文案里有历史上的「数字人」产品说明，
 *    那是另一回事（FULL-01 由管理端 NAV 断言与后端路由断言钉住）。
 * ⚠️ 达人口播的「不保证口型与语音逐字同步」是**选项内容**（库里的运营文案），不在代码里；这里不会误伤它。
 *
 * MKT-T-39（MK-23~25、MKT-AC-37 / 45）：角色只做非真人、不做公开、音色不克隆 —— 再加「真人角色」「扫码」「可见范围」
 * 「克隆」「复刻」。⚠️ 唯一允许出现「克隆」的是 MKT-F-104 要求的那句**否定说明**「也不会用于声音克隆」：
 * 扫描前只摘掉这一整句（`ALLOWED_NEGATIONS`），写成「支持声音克隆」之类的照样会红。
 */
const ROOT = resolve(__dirname, "..");

const EXCLUDED = [
  "每日热点", "热点", "热榜", "数字人", "口型", "digital_human", "lip_sync", "lipsync", "hot_trend", "daily_hot",
  // MKT-T-39：真人角色 / 扫码认证 / 可见范围 / 声音克隆
  "真人角色", "扫码", "可见范围", "克隆", "复刻",
  // MKT-T-43：真人认证 / 实名（MK-23）。⚠️ 英文 `visibility` 不进前端表：CSS 的 `visibility: hidden` 是合法写法，
  // 字段名由后端 `TestContract_不含排除项关键词` 扫契约键钉住。「非真人形象」（上传说明）不命中这两个词。
  "真人认证", "实名"
];

/** 允许出现的否定说明（逐字）：MKT-F-104 要求如实写明「不会用于声音克隆」。 */
const ALLOWED_NEGATIONS = ["不会发送给生成模型，也不会用于声音克隆"];

function withoutNegations(source: string): string {
  return ALLOWED_NEGATIONS.reduce((code, sentence) => code.split(sentence).join(""), source);
}

function stripComments(source: string): string {
  return source
    .replace(/<!--[\s\S]*?-->/g, "")
    .replace(/\/\*[\s\S]*?\*\//g, "")
    // 行注释：`//` 前不是 `:`（避开 http://）
    .replace(/(^|[^:])\/\/.*$/gm, "$1");
}

function listFiles(dir: string, filter: (name: string) => boolean): string[] {
  return readdirSync(join(ROOT, dir)).filter(filter).map(name => `${dir}/${name}`);
}

const isSource = (name: string) => /\.(vue|ts)$/.test(name) && !name.includes(".test.");

const FILES = [
  "components/AppSidebar.vue",
  "utils/sidebarNav.ts",
  "pages/marketing.vue",
  "utils/skillEntries.ts",
  ...listFiles("components/marketing", isSource),
  ...listFiles("components/agent/composer/marketing", isSource),
  ...listFiles("utils", name => isSource(name) && name.startsWith("marketing"))
];

describe("营销排除项（MKT-AC-23）", () => {
  it("扫描范围非空（绊线自检：目录改名时这里先红，而不是断言恒绿）", () => {
    expect(FILES.length).toBeGreaterThan(10);
    expect(FILES).toContain("components/marketing/TemplateWall.vue");
  });

  it("侧栏、营销页、营销组件与快捷芯片数据源里没有每日热点 / 数字人 / 口型驱动的入口或文案", () => {
    const hits: string[] = [];
    for (const file of FILES) {
      const code = withoutNegations(stripComments(readFileSync(join(ROOT, file), "utf8")));
      for (const word of EXCLUDED) {
        if (code.toLowerCase().includes(word)) hits.push(`${file}: ${word}`);
      }
    }
    expect(hits).toEqual([]);
  });

  it("角色弹窗在扫描范围内，且否定说明之外不允许出现「克隆」（MKT-AC-45）", () => {
    expect(FILES).toContain("components/agent/composer/marketing/CharacterPickerDialog.vue");
    expect(FILES).toContain("components/agent/composer/marketing/CharacterEditorDialog.vue");
    expect(withoutNegations("说明：不会发送给生成模型，也不会用于声音克隆。")).not.toContain("克隆");
    expect(withoutNegations("支持声音克隆")).toContain("克隆");
  });

  it("新增的商品 / 角色弹窗都在扫描范围内，且上传说明「非真人形象」不被误伤（MKT-T-43）", () => {
    for (const name of ["ProductPickerDialog.vue", "ProductEditorDialog.vue", "MarketingSlots.vue"]) {
      expect(FILES).toContain(`components/agent/composer/marketing/${name}`);
    }
    expect(FILES).toContain("utils/marketingLibrary.ts");
    const note = "请上传非真人形象（卡通、拟人、吉祥物、产品拟人等），不得上传他人肖像";
    expect(EXCLUDED.filter(word => note.includes(word))).toEqual([]);
  });

  it("去注释不会把正文吃掉（http:// 保留；注释里的说明被去掉）", () => {
    expect(stripComments("const a = \"http://x\"; // 每日热点")).toBe("const a = \"http://x\"; ");
    expect(stripComments("/* 数字人 */ x <!-- 口型 --> y")).toBe(" x  y");
  });
});
