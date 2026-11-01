/**
 * 3D 导演台的三方依赖与素材许可绊线（DRT-09，DR-N-15，DR-AC-48）。
 *
 * ⚠️ 许可一律读 node_modules 里的真实 `license` 字段，不手抄：抄错的许可比没有清单更糟
 *    （同 CLAUDE.md §5.24「会撒谎的矩阵」）。
 * ⚠️ 只允许 MIT / Apache-2.0 / MPL-2.0（03 C-4）：GPL / AGPL / 无许可 / 非商用一律不进。
 *    唯一的例外是服务端排版字体（智能剪辑 SCT-13）可以是 OFL-1.1 —— 见 assetLicenseAllowed。
 */
import { existsSync, readdirSync, readFileSync, realpathSync, statSync } from "node:fs";
import { dirname, join, relative } from "node:path";
import { fileURLToPath } from "node:url";
import { describe, expect, it } from "vitest";

const webRoot = fileURLToPath(new URL("..", import.meta.url));
const manifest = JSON.parse(readFileSync(join(webRoot, "director/licenses.json"), "utf8")) as {
  packages: { name: string; version: string; license: string; url: string; note: string }[];
  assets: { path: string; license: string; source: string; attribution: string }[];
};
const pkg = JSON.parse(readFileSync(join(webRoot, "package.json"), "utf8")) as {
  dependencies?: Record<string, string>;
  devDependencies?: Record<string, string>;
};
const declared = { ...pkg.dependencies, ...pkg.devDependencies };

const ALLOWED = new Set(["MIT", "Apache-2.0", "MPL-2.0"]);
/**
 * 字体可以是 OFL-1.1（智能剪辑 03 C-3：OFL 只允许用于字体），而且**只限**服务端排版字体的目录与字体文件：
 * 对所有素材放开的话，一张 OFL 的图片、一段 OFL 的代码也能混进来，而这条绊线照样是绿的。
 */
const FONT_DIR = "backend/assets/fonts/";
function assetLicenseAllowed(a: { path: string; license: string }): boolean {
  if (ALLOWED.has(a.license)) return true;
  return a.license === "OFL-1.1" && a.path.startsWith(FONT_DIR) && /\.(otf|ttf)$/.test(a.path);
}
/** 导演台专用的依赖：出现在 package.json 里就必须在清单里（DRT-10 / 23 / 25 / 28 引入时同步补条目）。 */
const DIRECTOR_PACKAGES = [
  "three", "@types/three", "camera-controls", "three-viewport-gizmo", "three-mesh-bvh",
  "@mediapipe/tasks-vision", "mediabunny", "@sparkjsdev/spark", "@gltf-transform/core", "@gltf-transform/extensions",
  "@gltf-transform/functions"
];

function installedLicense(name: string): string | undefined {
  const path = join(webRoot, "node_modules", name, "package.json");
  if (!existsSync(path)) return undefined;
  const meta = JSON.parse(readFileSync(path, "utf8")) as { license?: string | { type?: string } };
  return typeof meta.license === "string" ? meta.license : meta.license?.type;
}

/**
 * 按 Node 的解析规则找包目录（pnpm 下是 store 里的真实路径）：从 fromDir 往上逐级找 node_modules/<name>。
 * 平台专用的可选依赖在别的平台上没有安装，找不到时跳过。
 */
function packageDir(name: string, fromDir: string): string | undefined {
  for (let dir = realpathSync(fromDir); ; dir = dirname(dir)) {
    const candidate = join(dir, "node_modules", name);
    if (existsSync(join(candidate, "package.json"))) return realpathSync(candidate);
    if (dirname(dir) === dir) return undefined;
  }
}

function filesUnder(dir: string): string[] {
  if (!existsSync(dir)) return [];
  return readdirSync(dir).flatMap((name) => {
    const path = join(dir, name);
    return statSync(path).isDirectory() ? filesUnder(path) : [path];
  });
}

describe("directorLicenses", () => {
  it("清单里的每个包都在 package.json 且版本一致", () => {
    for (const p of manifest.packages) {
      expect(declared[p.name], `${p.name} 不在 package.json 里`).toBeDefined();
      expect(declared[p.name], `${p.name} 的版本与清单不一致（package.json 里要写死、不带 ^）`).toBe(p.version);
    }
  });

  it("许可只允许 MIT / Apache-2.0 / MPL-2.0（服务端字体另可 OFL-1.1）", () => {
    for (const p of manifest.packages) expect(ALLOWED.has(p.license), `${p.name}: ${p.license}`).toBe(true);
    for (const a of manifest.assets) expect(assetLicenseAllowed(a), `${a.path}: ${a.license}`).toBe(true);
  });

  it("OFL 只对服务端字体放行；fetch-fonts.sh 下载的每个字体都有署名条目", () => {
    expect(assetLicenseAllowed({ path: "director/models/a.glb", license: "OFL-1.1" })).toBe(false);
    expect(assetLicenseAllowed({ path: `${FONT_DIR}SOURCES.txt`, license: "OFL-1.1" })).toBe(false);
    expect(assetLicenseAllowed({ path: `${FONT_DIR}x.otf`, license: "GPL-3.0" })).toBe(false);
    expect(assetLicenseAllowed({ path: `${FONT_DIR}x.otf`, license: "OFL-1.1" })).toBe(true);
    // 字体不进 git（体积），清单与下载脚本必须对得上：脚本换了字体而清单没跟上，许可页就署错了名。
    const script = readFileSync(join(webRoot, "../build/fetch-fonts.sh"), "utf8");
    const fonts = [...script.matchAll(/^([\w.-]+\.(?:otf|ttf))\|/gm)].map((m) => FONT_DIR + m[1]);
    expect(fonts.length, "fetch-fonts.sh 里一个字体都没解析出来").toBeGreaterThan(0);
    const listed = new Set(manifest.assets.map((a) => a.path));
    for (const font of fonts) expect(listed.has(font), `${font} 由 fetch-fonts.sh 下载，但不在许可清单里`).toBe(true);
    for (const a of manifest.assets.filter((x) => x.path.startsWith(FONT_DIR))) {
      expect(fonts.includes(a.path), `${a.path} 在许可清单里，但 fetch-fonts.sh 并不下载它`).toBe(true);
    }
  });

  it("清单许可与 node_modules 的 license 字段一致", () => {
    for (const p of manifest.packages) {
      expect(installedLicense(p.name), `${p.name} 的 node_modules 许可`).toBe(p.license);
    }
  });

  it("package.json 里导演台相关的包都在清单里", () => {
    const listed = new Set(manifest.packages.map((p) => p.name));
    for (const name of DIRECTOR_PACKAGES) {
      if (declared[name]) expect(listed.has(name), `${name} 在 package.json 里但不在许可清单里`).toBe(true);
    }
  });

  // ⚠️ 传递依赖也要在白名单里：`@gltf-transform/functions`（MIT）经 ndarray-pixels 带进 sharp 与
  // LGPL-3.0 的 libvips —— 只看直接依赖的话这条绊线是绿的（DRT-10 实测）。
  it("导演台依赖的传递依赖许可也在白名单里", () => {
    const seen = new Map<string, string>();
    const walk = (name: string, fromDir: string) => {
      const dir = packageDir(name, fromDir);
      if (!dir || seen.has(name)) return;
      const meta = JSON.parse(readFileSync(join(dir, "package.json"), "utf8")) as {
        license?: string | { type?: string };
        dependencies?: Record<string, string>;
        optionalDependencies?: Record<string, string>;
      };
      seen.set(name, typeof meta.license === "string" ? meta.license : (meta.license?.type ?? "(无)"));
      for (const dep of [...Object.keys(meta.dependencies ?? {}), ...Object.keys(meta.optionalDependencies ?? {})]) {
        walk(dep, dir);
      }
    };
    for (const p of manifest.packages) walk(p.name, webRoot);
    const bad = [...seen].filter(([, license]) => !ALLOWED.has(license));
    expect(bad, `传递依赖里有白名单外的许可：${JSON.stringify(bad)}`).toEqual([]);
    expect(seen.has("sharp"), "不该再带进 sharp（LGPL 的 libvips）").toBe(false);
  });

  // ⚠️ 自托管的 MediaPipe wasm 必须与锁定版本的 JS 配套（DRT-23）：升级了包却没重跑
  //    `director-assets.mjs mediapipe` 的话，识别在运行时才失败，构建与其余测试照样是绿的。
  it("自托管的 MediaPipe wasm 与 node_modules 里锁定版本的逐字节相同", () => {
    const hosted = join(webRoot, "public/director/mediapipe/wasm");
    const files = existsSync(hosted) ? readdirSync(hosted) : [];
    expect(files.sort()).toEqual(["vision_wasm_module_internal.js", "vision_wasm_module_internal.wasm"]);
    const installed = packageDir("@mediapipe/tasks-vision", webRoot);
    expect(installed, "@mediapipe/tasks-vision 没有安装").toBeDefined();
    for (const name of files) {
      const same = readFileSync(join(hosted, name)).equals(readFileSync(join(installed as string, "wasm", name)));
      expect(same, `public/director/mediapipe/wasm/${name} 与 node_modules 里的不一致：重跑 node scripts/director-assets.mjs mediapipe`).toBe(true);
    }
  });

  it("public/director 下每个文件都有素材条目", () => {
    const root = join(webRoot, "public/director");
    const listed = new Set(manifest.assets.map((a) => a.path));
    for (const file of filesUnder(root)) {
      const path = relative(join(webRoot, "public"), file).split("\\").join("/");
      const covered = listed.has(path) || [...listed].some((p) => p.endsWith("/*") && path.startsWith(p.slice(0, -1)));
      expect(covered, `public/${path} 没有素材许可条目`).toBe(true);
    }
  });
});
