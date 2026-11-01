#!/usr/bin/env node
/**
 * 3D 导演台的包体积与懒加载检查（DRT-09，DR-AC-49，DRD-33）。
 *
 * 读生产构建的客户端清单，从画布页沿**静态**依赖走一遍：
 *   1. 静态依赖里不得出现 three（导演台只能经 defineAsyncComponent 懒加载）；
 *   2. 静态 chunk 的 gzip 总和相对基线增长不得超过 20 KB。
 *
 * 用法：先 `pnpm --filter @tabtv/web build`，再
 *   node scripts/director-bundle-check.mjs            # 比对基线
 *   node scripts/director-bundle-check.mjs --record   # 写基线（只在引入导演台依赖之前跑一次）
 *
 * ⚠️ 读的是 `.output/server/chunks/build/client.precomputed.mjs`，不是 `.nuxt/dist/server/client.manifest.mjs`：
 *    后者会被正在运行的 `nuxt dev` 覆盖成开发态清单（只有 @vite/client），读它等于什么都没检查。
 * ⚠️ 判断 three 进没进主包看 chunk **内容**，不看模块名：Rollup 会把多个模块合进一个 chunk，
 *    three 被并进某个业务 chunk 时，按模块名找不到它。three 的报错文案（`THREE.WebGLRenderer`）压缩后仍在。
 * ⚠️ 清单结构不符合预期时**非零退出**，不能当作通过：清单是构建内部产物，格式可能随 Nuxt 版本变。
 * ⚠️ 画布页不一定有以源文件命名的键：它的 chunk 还要向自己懒加载的子 chunk（导演台编辑器、提示词面板）导出别的绑定时，
 *    Rollup 不给它单独的门面 chunk，路由表里写成 `import("./XXXX.js").then(e => e.yy)`，清单的键就成了 `_XXXX.js`
 *    （批次 4 起如此）。这时按路由路径在产物里找到那个 chunk —— 找不到同样非零退出。
 */
import { readFileSync, readdirSync, writeFileSync, existsSync, mkdirSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";
import { gzipSync } from "node:zlib";

const here = dirname(fileURLToPath(import.meta.url));
const webRoot = resolve(here, "..");
const repoRoot = resolve(webRoot, "../..");
const manifestPath = join(webRoot, ".output/server/chunks/build/client.precomputed.mjs");
const publicDir = join(webRoot, ".output/public/_nuxt");
const baselinePath = join(repoRoot, "docs/agent/director/evidence/bundle-baseline.json");

const PAGE_KEY = "pages/video/canvas/[projectId].vue";
/** 画布页在构建产物路由表里的路径（Nuxt 的写法）。 */
const ROUTE_PATH = 'path:"/video/canvas/:projectId()"';
const MAX_GROWTH_BYTES = 20 * 1024;
/** three 及三个 3D 库的特征串（压缩后仍保留的报错 / 类名文案）。 */
const THREE_SIGNATURES = [/THREE\.WebGLRenderer/, /THREE\.Object3D/, /three-mesh-bvh/, /ViewportGizmo/];

function fail(code, message) {
  console.error(`✖ director-bundle-check：${message}`);
  process.exit(code);
}

async function loadManifest() {
  if (!existsSync(manifestPath)) {
    fail(2, `找不到生产构建清单 ${manifestPath}，请先 pnpm --filter @tabtv/web build`);
  }
  const mod = await import(pathToFileURL(manifestPath).href);
  const data = mod.default;
  if (!data || typeof data !== "object" || !data.dependencies || typeof data.dependencies !== "object") {
    fail(2, "清单结构与预期不符：缺少 dependencies（Nuxt 版本变了？请核对后更新本脚本）");
  }
  return data.dependencies;
}

/** 收集一个模块的静态依赖文件（scripts + preload；prefetch 是动态 import，不算）。 */
function staticFiles(entry) {
  const files = new Set();
  for (const group of [entry.scripts, entry.preload]) {
    if (!group || typeof group !== "object") continue;
    for (const res of Object.values(group)) {
      if (res && typeof res.file === "string" && res.file.endsWith(".js")) files.add(res.file);
    }
  }
  return files;
}

/**
 * 画布页在清单里的键：先按源文件路径；没有的话在产物里找路由表中这一项懒加载的那个 chunk（见文件头）。
 * 只看路径之后很近的一段，免得错拿到下一条路由的 import。
 */
function findPageKey(deps) {
  if (deps[PAGE_KEY]) return PAGE_KEY;
  for (const file of readdirSync(publicDir).filter((f) => f.endsWith(".js"))) {
    const text = readFileSync(join(publicDir, file), "utf8");
    const at = text.indexOf(ROUTE_PATH);
    if (at < 0) continue;
    const match = /import\("\.\/([\w-]+\.js)"\)/.exec(text.slice(at, at + 400));
    if (match && deps[`_${match[1]}`]) return `_${match[1]}`;
  }
  return null;
}

function findEntryKey(deps) {
  return Object.keys(deps).find((k) => k.endsWith("nuxt/dist/app/entry.js"));
}

async function main() {
  const record = process.argv.includes("--record");
  const deps = await loadManifest();
  const pageKey = findPageKey(deps);
  if (!pageKey) fail(2, `清单里找不到画布页 ${PAGE_KEY}，产物的路由表里也没有 ${ROUTE_PATH}（路由改名了？）`);
  const page = deps[pageKey];
  const entryKey = findEntryKey(deps);
  if (!entryKey) fail(2, "清单里没有 Nuxt 应用入口 entry.js");

  const files = new Set([...staticFiles(deps[entryKey]), ...staticFiles(page)]);
  if (files.size === 0) fail(2, "画布页没有任何静态 chunk，清单结构可能变了");

  const rows = [];
  const offenders = [];
  for (const file of [...files].sort()) {
    const path = join(publicDir, file);
    if (!existsSync(path)) fail(2, `清单里的 chunk 不存在：${path}`);
    const body = readFileSync(path);
    rows.push({ file, gzip: gzipSync(body).length });
    const text = body.toString("utf8");
    const hit = THREE_SIGNATURES.find((re) => re.test(text));
    if (hit) offenders.push(`${file}（命中 ${hit}）`);
  }
  const total = rows.reduce((sum, r) => sum + r.gzip, 0);

  if (offenders.length > 0) {
    fail(1, `画布页的静态依赖里出现了 3D 库，导演台必须懒加载（DRD-33）：\n  ${offenders.join("\n  ")}`);
  }

  if (record) {
    mkdirSync(dirname(baselinePath), { recursive: true });
    const baseline = { recordedAt: new Date().toISOString(), page: PAGE_KEY, totalGzip: total, files: rows };
    writeFileSync(baselinePath, `${JSON.stringify(baseline, null, 2)}\n`);
    console.log(`✔ 已记录基线：${rows.length} 个静态 chunk，gzip 合计 ${(total / 1024).toFixed(1)} KB → ${baselinePath}`);
    return;
  }

  if (!existsSync(baselinePath)) fail(2, `没有基线文件 ${baselinePath}，先用 --record 记录`);
  const baseline = JSON.parse(readFileSync(baselinePath, "utf8"));
  const growth = total - baseline.totalGzip;
  if (growth > MAX_GROWTH_BYTES) {
    const before = new Set(baseline.files.map((r) => r.file));
    const added = rows.filter((r) => !before.has(r.file)).map((r) => `${r.file} ${(r.gzip / 1024).toFixed(1)} KB`);
    fail(1, `画布页静态 chunk 的 gzip 增长 ${(growth / 1024).toFixed(1)} KB，超过上限 20 KB。新增的 chunk：\n  ${added.join("\n  ") || "（无新增，是已有 chunk 变大）"}`);
  }
  console.log(`✔ director-bundle-check：画布页（${pageKey}）静态 chunk 不含 3D 库；gzip ${(total / 1024).toFixed(1)} KB，相对基线 ${(growth / 1024).toFixed(1)} KB（上限 20 KB）`);
}

await main();
