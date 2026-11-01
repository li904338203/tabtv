import { readdirSync, readFileSync, statSync } from "node:fs";
import { join, resolve } from "node:path";
import { describe, expect, it } from "vitest";

/**
 * 3D 导演台的排除项断言（DRT-32，DR-F-190~196、DRK-14）：03 §9 明确不做的能力，在导演台的界面与引擎里
 * 一个入口都不能有 —— 服务端渲染截图、上传自定义模型 / 导出 GLB、宫格截图、编辑器内在线状态、
 * 用参考图生成 3D 世界、相机轨迹文件导出、物理与布娃娃 / 手绘轨迹 / 景深 / HDRI 背景。
 *
 * ⚠️ 扫的是**去掉注释后**的源码（同 marketingExclusion）：注释里写「没有生成 3D 世界的入口（DRK-14）」
 *    恰恰是在钉住这条规则，不能算违规。
 * ⚠️ 只扫源码（.vue / .ts / .js / .mjs，测试文件除外）。`director/licenses.json` 与 `public/director/**`
 *    （素模、姿势数据、MediaPipe 的 wasm 与模型）是数据不是入口：许可清单里的文字来自上游包与素材，
 *    不由我们措辞，扫它只会让人为一段别人写的说明去改断言；它们的合规由 directorLicenses.test.ts 管。
 * ⚠️ 两张表：界面说法（EXCLUDED）+ 代码标识（EXCLUDED_IDENTIFIERS）。只扫中文说法的话，界面换个词就绕过去了；
 *    真要做这些能力，绕不开 three 的导出器 / 环境贴图加载器 / 景深后期这些标识。
 *
 * ⚠️ 素模是 GLB（`GLTFLoader` 读自带的文件）—— 「读 GLB」是正常能力，不在表里；被排除的是
 *    「用户上传模型」与「导出 GLB」（DR-F-191）。
 */
const ROOT = resolve(__dirname, "..");

/** 03 §9（DR-F-190~196）在界面上的说法。空格写成任意空白（「导出GLB」「导出 GLB」都算），不分大小写。 */
const EXCLUDED = [
  "服务端渲染", // DR-F-190
  "上传模型", "导出 GLB", // DR-F-191
  "宫格", // DR-F-192
  "生成 3D 世界", "AI 生成背景", // DR-F-194 / DRK-14
  "相机轨迹导出", // DR-F-195
  "布娃娃", "手绘轨迹", "景深", "HDRI" // DR-F-196
];

/**
 * 同一批能力在代码里绕不开的标识（不分大小写，按子串比：`usePresence`、`THREE.GLTFExporter` 都要命中）：
 * - `GLTFExporter`：导出 GLB；`FBXLoader` / `OBJLoader`：读用户的自定义模型（DR-F-191）；
 * - `presence`：编辑器内在线状态（DR-F-193；画布页本身也没接，DRB-9）；
 * - `CameraCtrl`：相机轨迹文件格式（DR-F-195）；
 * - `ragdoll`：布娃娃物理；`BokehPass`：景深后期；`RGBELoader` / `HDRLoader` / `EXRLoader`：HDRI 环境贴图（DR-F-196）。
 */
const EXCLUDED_IDENTIFIERS = [
  "GLTFExporter", "FBXLoader", "OBJLoader", "presence", "CameraCtrl", "ragdoll", "BokehPass",
  "RGBELoader", "HDRLoader", "EXRLoader"
];

/**
 * 允许出现的否定说明（逐字整句摘掉，写成「支持……」照样会红）。
 * 目前**一条都没有**：导演台界面上没有任何一句提到这些能力的文案，唯一的提及在 SceneSettings.vue 的注释里
 * （「没有『生成 3D 世界』的入口」），已由去注释处理。以后若需求要求在界面上如实说明「不支持 ……」
 * （同营销的 MKT-F-104），把那一整句原样加在这里，并写明对应的需求编号。
 */
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
 * 界面说法的匹配：空格当任意空白，不分大小写；两端是英文字母的加词边界 ——
 * 否则 `HDRI` 会命中 `pathDrift` 这类标识（小写后 pat·hdri·ft）。
 */
const PHRASES = EXCLUDED.map(word => {
  const body = word.split(/\s+/).map(escapeRegExp).join("\\s*");
  const head = /^[A-Za-z]/.test(word) ? "\\b" : "";
  const tail = /[A-Za-z]$/.test(word) ? "\\b" : "";
  return { word, re: new RegExp(head + body + tail, "i") };
});

/** 一段（已去注释的）源码里命中的排除项。 */
function findHits(code: string): string[] {
  const lower = code.toLowerCase();
  return [
    ...PHRASES.filter(p => p.re.test(code)).map(p => p.word),
    ...EXCLUDED_IDENTIFIERS.filter(id => lower.includes(id.toLowerCase()))
  ];
}

const isSource = (name: string) => /\.(vue|ts|js|mjs)$/.test(name) && !/\.(test|spec)\./.test(name);

function listFiles(dir: string, filter: (name: string) => boolean): string[] {
  return readdirSync(join(ROOT, dir)).filter(filter).map(name => `${dir}/${name}`);
}

/** 递归列出目录下的源码（`components/director/props`、`director/engine` 这类子目录一并扫）。 */
function walk(dir: string): string[] {
  return readdirSync(join(ROOT, dir)).flatMap(name => {
    const path = `${dir}/${name}`;
    if (statSync(join(ROOT, path)).isDirectory()) return walk(path);
    return isSource(name) ? [path] : [];
  });
}

const FILES = [
  ...walk("components/director"),
  ...listFiles("components/canvas/nodes", name => name.startsWith("Director") && isSource(name)),
  "components/canvas/DirectorSourceBadge.vue",
  ...listFiles("utils", name => name.startsWith("director") && isSource(name)),
  ...walk("director"),
  // 卡片清单之外顺带扫的两处：编辑器的 store 与导演台的组合式函数（同样是导演台专属的代码）。
  "stores/director.ts",
  ...listFiles("composables", name => name.startsWith("useDirector") && isSource(name))
];

describe("directorExclusion", () => {
  it("扫描文件数不少于 15", () => {
    // 绊线：目录改名或过滤写错时这里先红，而不是「没扫到文件」让下一条恒绿。
    expect(FILES.length).toBeGreaterThan(15);
    for (const file of [
      "components/director/DirectorStudio.vue", "components/director/SceneSettings.vue",
      "components/director/props/CameraProps.vue", "components/canvas/nodes/DirectorNodeBody.vue",
      "components/canvas/DirectorSourceBadge.vue", "utils/directorScene.ts", "director/engine/DirectorEngine.ts",
      "director/workers/pose.worker.ts", "stores/director.ts"
    ]) {
      expect(FILES).toContain(file);
    }
    expect(FILES.some(file => file.includes(".test."))).toBe(false);
    expect(FILES).not.toContain("director/licenses.json");
  });

  it("导演台文件里没有排除项关键词", () => {
    const hits: string[] = [];
    for (const file of FILES) {
      const code = withoutNegations(stripComments(readFileSync(join(ROOT, file), "utf8")));
      for (const word of findHits(code)) hits.push(`${file}: ${word}`);
    }
    expect(hits).toEqual([]);
  });

  it("注释里钉规则的说明不算违规，正文里出现就算（SceneSettings 的 DRK-14 注释）", () => {
    const raw = readFileSync(join(ROOT, "components/director/SceneSettings.vue"), "utf8");
    expect(findHits(raw)).toContain("生成 3D 世界");
    expect(findHits(stripComments(raw))).toEqual([]);
  });

  it("匹配规则：空白与大小写不影响命中，英文词按词边界、标识按子串", () => {
    expect(findHits("支持导出GLB")).toEqual(["导出 GLB"]);
    expect(findHits("用参考图生成3D世界")).toEqual(["生成 3D 世界"]);
    expect(findHits("ai  生成背景")).toEqual(["AI 生成背景"]);
    expect(findHits("加载 hdri 背景")).toEqual(["HDRI"]);
    expect(findHits("const pathDrift = 1; // HDRI")).toEqual(["HDRI"]);
    expect(findHits(stripComments("const pathDrift = 1; // HDRI"))).toEqual([]);
    expect(findHits("import { GLTFExporter } from \"three/examples/jsm/exporters/GLTFExporter.js\"")).toEqual(["GLTFExporter"]);
    expect(findHits("const { peers } = usePresence(projectId)")).toEqual(["presence"]);
    // 正常能力不误伤：读自带素模的 GLB、深度图、物理键位、浏览器在线状态。
    expect(findHits("new GLTFLoader().loadAsync(`${base}${code}.glb`); 深度图；物理键位；浏览器仍报「在线」")).toEqual([]);
  });

  it("否定说明只按整句豁免：写成「支持……」照样会红", () => {
    expect(ALLOWED_NEGATIONS).toEqual([]);
    expect(findHits(withoutNegations("支持导出 GLB"))).toEqual(["导出 GLB"]);
  });

  it("去注释不会把正文吃掉（http:// 保留；注释里的说明被去掉）", () => {
    expect(stripComments("const a = \"http://x\"; // 景深")).toBe("const a = \"http://x\"; ");
    expect(stripComments("/* 宫格 */ x <!-- 布娃娃 --> y")).toBe(" x  y");
  });
});
