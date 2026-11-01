import { readdirSync, readFileSync, statSync } from "node:fs";
import { join, resolve } from "node:path";
import { describe, expect, it } from "vitest";

/**
 * 智能剪辑的排除项断言（SCT-44，SC-AC-72，03 §4.18 SC-F-180~189、C-8）：明确不做的能力，在剪辑台、节点卡与
 * 智能剪辑的工具函数里一个入口都不能有 —— 会员专属、导出工程文件（OTIO / 剪映草稿）、曲库、剪辑台里生成配音、
 * 画中画 / 关键帧动画 / 调色 / 贴纸、浏览器端导出、4K、复杂排版语种、营销成片、导出自动登记作品，
 * 以及全站排除的数字人 / 口型 / 声音克隆 / 可见范围（FULL-01，后端由 cmd/api/exclusion_test.go 管）。
 *
 * ⚠️ 扫的是**去掉注释后**的源码（同 directorExclusion / marketingExclusion）：注释里写「剪辑台里不生成配音（SCK-6）」
 *    恰恰是在钉住这条规则，不能算违规。
 * ⚠️ 两张表：界面说法（EXCLUDED，按子串、空白不敏感）+ 代码标识（EXCLUDED_IDENTIFIERS，英文按**整词**）。
 *    英文不按整词的话 `OTIO` 会命中每个文件里的 `prefers-reduced-motion`（SCT-44 校准时实测）。
 * ⚠️ 「剪辑台里只生成配乐和音效」这类**如实说明不支持**的界面文案若以后需要，把那一整句加进 ALLOWED_NEGATIONS，
 *    并写明需求编号；写成「支持……」照样会红。
 */
const ROOT = resolve(__dirname, "..");

/** 03 §4.18 与 C-8 在界面上的说法。空格写成任意空白，不分大小写。 */
const EXCLUDED = [
  "会员专属", "工程文件", "剪映草稿", // SC-F-183
  "曲库", "生成配音", // SC-F-184
  "画中画", "关键帧动画", "调色", "贴纸", // SC-F-185
  "浏览器端导出", "4K", // SC-F-186
  "阿拉伯文", "泰文", // SC-F-187
  "营销成片", // SC-F-188
  "自动登记作品", // SC-F-189
  "数字人", "口型", "声音克隆", "可见范围" // FULL-01 / C-8
];

/**
 * 同一批能力在代码里绕不开的标识（不分大小写，按整词）：
 * - `OTIO` / `jianying` / `capcut`：工程文件格式（SC-F-183）；
 * - `MediaRecorder` / `VideoEncoder` / `ffmpeg.wasm` 的包名 `@ffmpeg/ffmpeg`：浏览器端导出（SC-F-186）；
 * - `2160p`：4K（SC-F-186）；
 * - `lipsync` / `voiceclone` / `visibility`：全站排除（C-8，与后端同一张表）。
 */
const EXCLUDED_IDENTIFIERS = [
  "OTIO", "jianying", "capcut", "MediaRecorder", "VideoEncoder", "@ffmpeg/ffmpeg", "2160p", "lipsync", "voiceclone",
  "visibility"
];

/** 允许出现的否定说明（逐字整句摘掉）。目前**一条都没有**：界面上没有提到这些能力的文案。 */
const ALLOWED_NEGATIONS: readonly string[] = [];

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

function escapeRegExp(s: string): string {
  return s.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
}

/**
 * 两端是英文字母或数字的加词边界（`4K` 不该命中 `4KB`，`OTIO` 不该命中 `motion`）；
 * 中文逐字之间允许空白（「会员 专属」照样命中）。
 */
function wordRe(word: string): RegExp {
  const parts = /\s/.test(word) ? word.split(/\s+/) : /[\u4e00-\u9fff]/.test(word) ? [...word] : [word];
  const body = parts.map(escapeRegExp).join("\\s*");
  const head = /^[A-Za-z0-9]/.test(word) ? "(?<![A-Za-z0-9])" : "";
  const tail = /[A-Za-z0-9]$/.test(word) ? "(?![A-Za-z0-9])" : "";
  return new RegExp(head + body + tail, "i");
}

const PATTERNS = [...EXCLUDED, ...EXCLUDED_IDENTIFIERS].map(word => ({ word, re: wordRe(word) }));

/** 一段（已去注释的）源码里命中的排除项。 */
function findHits(code: string): string[] {
  return PATTERNS.filter(p => p.re.test(code)).map(p => p.word);
}

const isSource = (name: string) => /\.(vue|ts|js|mjs)$/.test(name) && !/\.(test|spec)\./.test(name);

function listFiles(dir: string, filter: (name: string) => boolean): string[] {
  return readdirSync(join(ROOT, dir)).filter(filter).map(name => `${dir}/${name}`);
}

/** 递归列出目录下的源码（剪辑台的 timeline / panels / dialogs / preview 子目录一并扫）。 */
function walk(dir: string): string[] {
  return readdirSync(join(ROOT, dir)).flatMap(name => {
    const path = `${dir}/${name}`;
    if (statSync(join(ROOT, path)).isDirectory()) return walk(path);
    return isSource(name) ? [path] : [];
  });
}

const FILES = [
  ...walk("components/smartcut"),
  ...listFiles("components/canvas/nodes", name => name.startsWith("Smartcut") && isSource(name)),
  ...listFiles("components/canvas", name => name.startsWith("Smartcut") && isSource(name)),
  ...listFiles("utils", name => name.startsWith("smartcut") && isSource(name)),
  ...listFiles("stores", name => name.startsWith("smartcut") && isSource(name)),
  ...listFiles("composables", name => name.startsWith("useSmartcut") && isSource(name)),
  // 预览引擎、播放器与开发态的沙盒页（/__smartcut-sandbox，nuxt.config 里按开发态注册）。
  ...walk("smartcut")
];

describe("smartcutExclusion", () => {
  it("扫描文件数不少于 50", () => {
    // 绊线：目录改名或过滤写错时这里先红，而不是「没扫到文件」让下一条恒绿。
    expect(FILES.length).toBeGreaterThan(50);
    for (const file of [
      "components/smartcut/SmartcutStudio.vue", "components/smartcut/panels/ExportPanel.vue",
      "components/smartcut/dialogs/MusicForm.vue", "components/smartcut/timeline/TimelineView.vue",
      "components/canvas/nodes/SmartcutPromptPanel.vue", "utils/smartcutInputs.ts", "utils/smartcutPublish.ts",
      "stores/smartcut.ts", "smartcut/player.ts", "smartcut/dev/SmartcutSandbox.vue"
    ]) {
      expect(FILES).toContain(file);
    }
    expect(FILES.some(file => file.includes(".test."))).toBe(false);
  });

  it("智能剪辑文件里没有排除项关键词", () => {
    const hits: string[] = [];
    for (const file of FILES) {
      const code = withoutNegations(stripComments(readFileSync(join(ROOT, file), "utf8")));
      for (const word of findHits(code)) hits.push(`${file}: ${word}`);
    }
    expect(hits).toEqual([]);
  });

  it("注释里钉规则的说明不算违规，正文里出现就算（MusicForm 的 SCK-6、ExportPanel 的 SCK-9）", () => {
    const music = readFileSync(join(ROOT, "components/smartcut/dialogs/MusicForm.vue"), "utf8");
    expect(findHits(music)).toContain("生成配音");
    expect(findHits(stripComments(music))).toEqual([]);
    const exportPanel = readFileSync(join(ROOT, "components/smartcut/panels/ExportPanel.vue"), "utf8");
    expect(findHits(exportPanel)).toContain("自动登记作品");
    expect(findHits(stripComments(exportPanel))).toEqual([]);
  });

  it("匹配规则：空白与大小写不影响命中，英文按整词", () => {
    expect(findHits("支持 4K 导出")).toEqual(["4K"]);
    expect(findHits("大小 4KB")).toEqual([]);
    expect(findHits("@media (prefers-reduced-motion: reduce)")).toEqual([]);
    expect(findHits("导出 otio 工程")).toEqual(["OTIO"]);
    expect(findHits("会员 专属")).toEqual(["会员专属"]);
    expect(findHits("new MediaRecorder(stream)")).toEqual(["MediaRecorder"]);
    expect(findHits("const characterVisibility = 1")).toEqual([]);
    expect(findHits("visibility: hidden")).toEqual(["visibility"]);
  });

  it("否定说明只按整句豁免：写成「支持……」照样会红", () => {
    expect(ALLOWED_NEGATIONS).toEqual([]);
    expect(findHits(withoutNegations("支持生成配音"))).toEqual(["生成配音"]);
  });

  it("去注释不会把正文吃掉（http:// 保留；注释里的说明被去掉）", () => {
    expect(stripComments("const a = \"http://x\"; // 曲库")).toBe("const a = \"http://x\"; ");
    expect(stripComments("/* 画中画 */ x <!-- 贴纸 --> y")).toBe(" x  y");
  });
});
