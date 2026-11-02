import { readFileSync, readdirSync, statSync } from "node:fs";
import { join, sep } from "node:path";

import { describe, expect, it } from "vitest";

import { LIST_THUMB_WIDTH, absoluteMediaUrl, authorizedMediaRequest, projectCoverUrl } from "./authorizedMedia";

// ⚠️ 统一成 / 分隔：下面按 endsWith("stores/workspace.ts") 之类放行，Windows 上的 \ 路径永远匹配不上（TT-T-29 CI 抓出）。
const APP = join(__dirname, "..").split(sep).join("/");

/**
 * ⚠️ 已知遗留的直链下载 —— 登记在这里，而不是让扫描放过它们。**不许往这里加新的。**
 * 唯一的一条（组节点批量下载把 `outputUrl` 直接塞进 `<a href>`，必然 401）已在 CD-77 修掉：
 * 现在先 fetch 成 blob 再存，清单清空，扫描守住它不再回来。
 */
const KNOWN_DIRECT_LINKS: string[] = [];
const API_BASE = "/api/v1";

/**
 * ⚠️ 这一组在移植老前端时**丢过一次测试**：原用例长在被删掉的那份前端上
 * （`app/features/media/download.test.ts`），行为活了下来、验证没有。
 */

describe("受保护媒体必须带鉴权头取（AC-22 / FE-06、GAP-18）", () => {
  it("有 token 就带上 Bearer", () => {
    expect(authorizedMediaRequest("/generations/9/outputs/0", "tok", API_BASE).headers)
      .toEqual({ Authorization: "Bearer tok" });
  });

  it("⚠️ 还必须带 X-Workspace-Id —— 漏了是 400 不是 401", () => {
    // `/api/v1/media/{id}` 在工作区中间件里就要这个头。老系统没有工作区概念，
    // 所以移植过来的每一处取图都漏了它 —— 表现是"图片节点生成成功却不显示"。
    expect(authorizedMediaRequest("/media/42", "tok", API_BASE, "96209082733559810").headers)
      .toEqual({ Authorization: "Bearer tok", "X-Workspace-Id": "96209082733559810" });
  });

  it("没有工作区时不发空头", () => {
    expect(authorizedMediaRequest("/media/42", "tok", API_BASE, undefined).headers)
      .toEqual({ Authorization: "Bearer tok" });
    expect(authorizedMediaRequest("/media/42", "tok", API_BASE, "").headers)
      .toEqual({ Authorization: "Bearer tok" });
  });

  it("没有 token 时不发空头", () => {
    // ⚠️ `Bearer undefined` 会被服务端当成一个**坏 token** 拒掉，
    //    而不是当成匿名请求 —— 公开精选那条路就再也取不到图了。
    expect(authorizedMediaRequest("/showcase/9/media", undefined, API_BASE).headers).toBeUndefined();
    expect(authorizedMediaRequest("/showcase/9/media", "", API_BASE).headers).toBeUndefined();
  });

  it("相对地址补前缀，绝对地址与已带 /api/ 的原样", () => {
    expect(absoluteMediaUrl("/generations/9/outputs/0", API_BASE)).toBe("/api/v1/generations/9/outputs/0");
    expect(absoluteMediaUrl("generations/9/outputs/0", API_BASE)).toBe("/api/v1/generations/9/outputs/0");
    expect(absoluteMediaUrl("/api/v1/x", API_BASE)).toBe("/api/v1/x");
    expect(absoluteMediaUrl("https://cdn.example.com/a.png", API_BASE)).toBe("https://cdn.example.com/a.png");
  });
});

function vueAndTsFiles(dir: string, out: string[] = []): string[] {
  for (const name of readdirSync(dir)) {
    if (["node_modules", ".nuxt", ".output", "dist", ".turbo"].includes(name)) continue;
    const path = join(dir, name);
    if (statSync(path).isDirectory()) vueAndTsFiles(path, out);
    else if (/\.(vue|ts)$/.test(name) && !name.endsWith(".test.ts")) out.push(path.split(sep).join("/"));
  }
  return out;
}

describe("全站没有把受保护端点塞进 href/src（GAP-18）", () => {
  // ⚠️ 这条是**源码扫描**而不是单元测试，因为要防的正是"某个页面又抄了一份直链"。
  //    单测只能证明那个函数没写错，证明不了没有第二条路。
  it("没有任何 href/src 直接绑到 /api/ 上", () => {
    const offenders: string[] = [];
    for (const file of vueAndTsFiles(APP)) {
      const text = readFileSync(file, "utf8");
      // `:href="'/api/...'"` / `href="/api/..."` / `:src="\`/api/...\`"` 三种写法
      for (const m of text.matchAll(/:?(?:href|src)\s*=\s*["'`][^"'`]*\/api\/v1\//g)) {
        offenders.push(`${file.replace(APP, "")} → ${m[0]}`);
      }
    }
    expect(offenders, `直链受保护端点一律 401，界面上只是一张裂图：\n${offenders.join("\n")}`)
      .toEqual([]);
  });

  it("⚠️ 没有任何地方自己拼媒体请求头", () => {
    // 起因：应用里有六处在取受保护媒体，**六处全都只带了 Authorization**，
    // 漏了 `X-Workspace-Id` —— 而漏掉的表现是 400，界面上只是图不显示。
    // 各抄一份的话漏掉哪一处都**不会有编译错误**，所以用扫描钉死：
    // 手拼 `Authorization: Bearer` 的地方只许是 utils/authorizedMedia.ts 自己。
    const offenders: string[] = [];
    for (const file of vueAndTsFiles(APP)) {
      if (file.endsWith("utils/authorizedMedia.ts")) continue;
      // useApi 走的是另一条路（它自己就带两个头），这里只看媒体那条。
      if (file.endsWith("composables/useApi.ts")) continue;
      // ⚠️ 唯一的正当例外：`/workspaces` 是**引导调用** —— 它要回答的正是
      //    "该用哪个工作区"，按定义此刻还发不出 X-Workspace-Id。
      //    （它也不能走 useApi：那会和工作区 store 形成循环依赖。）
      if (file.endsWith("stores/workspace.ts")) continue;
      const text = readFileSync(file, "utf8");
      if (/Authorization.{0,4}:.{0,4}`Bearer \$\{/.test(text)) {
        offenders.push(file.replace(APP, ""));
      }
    }
    expect(offenders, `这些地方自己拼了鉴权头，漏 X-Workspace-Id 就是一片不显示的图：\n${offenders.join("\n")}`)
      .toEqual([]);
  });

  it("⚠️ JS 里也不许把受保护端点赋给 href/src", () => {
    // 上面那条只看模板绑定，**漏了 JS 赋值**：`link.href = String(item.outputUrl)` 这种写法
    // 同样不带鉴权头、同样 401，而它躲过了模板扫描（组节点批量下载就是这样漏掉的）。
    // 产出地址（outputUrl / cover.url）一律是受保护端点，只许 fetch 成 blob 再用。
    const offenders: string[] = [];
    for (const file of vueAndTsFiles(APP)) {
      const text = readFileSync(file, "utf8");
      for (const m of text.matchAll(/\.(?:href|src)\s*=\s*(?:String\()?[\w.?]*(?:outputUrl|\.url)\b/g)) {
        offenders.push(`${file.replace(APP, "")} → ${m[0]}`);
      }
    }
    expect(offenders, `这些地方把受保护端点直接赋给了 href/src（一律 401）：\n${offenders.join("\n")}`)
      .toEqual(KNOWN_DIRECT_LINKS);
  });

  it("每个 createObjectURL 的文件里都要有 revokeObjectURL", () => {
    // ⚠️ 漏掉 revoke 不报错：每刷一次列表就在内存里留一份完整图片，
    //    翻几页就是几百 MB，然后标签页被浏览器杀掉。
    const leaking: string[] = [];
    for (const file of vueAndTsFiles(APP)) {
      const text = readFileSync(file, "utf8");
      if (text.includes("createObjectURL") && !text.includes("revokeObjectURL")) {
        leaking.push(file.replace(APP, ""));
      }
    }
    expect(leaking, `这些文件建了 object URL 却从不释放：\n${leaking.join("\n")}`).toEqual([]);
  });
});

/**
 * ⚠️ 起因：工作台 / AI 视频页 / 全部项目的封面**一律是占位图**。两层原因叠在一起：
 *  1. 读的是 `coverMediaId`（只有手动设过才有，几乎总是空）；
 *  2. 就算有值，也把**媒体 ID 当成了 URL** —— 拼出 `/api/v1/96467992686100480` → 404。
 * 两处都不报错，表现都只是一张占位图。
 */
describe("项目卡片封面地址", () => {
  it("读的是展示封面，不是显式封面", () => {
    // 显式封面为空、展示封面有值 —— 这正是「项目出过图但没人手动设封面」的常态。
    expect(projectCoverUrl({ coverMediaId: null, displayCoverMediaId: "96467992686100480" } as never))
      .toBe("/media/96467992686100480?w=320");
  });

  it("⚠️ 给出的是地址，不是裸 ID", () => {
    const url = projectCoverUrl({ displayCoverMediaId: "42" });
    expect(url).toMatch(/^\/media\/42\?w=\d+$/);
    // 交给取图函数之后拼出来的是媒体端点，而不是 `/api/v1/42`。
    expect(absoluteMediaUrl(url!, API_BASE)).toBe("/api/v1/media/42?w=320");
  });

  it("列表默认要缩略图 —— 原图一张几 MB，一屏十几张会把标签页撑爆", () => {
    expect(projectCoverUrl({ displayCoverMediaId: "42" })).toContain(`w=${LIST_THUMB_WIDTH}`);
    expect(projectCoverUrl({ displayCoverMediaId: "42" }, 640)).toBe("/media/42?w=640");
  });

  it("没有封面时返回 undefined（显示占位），不拼出一个取不到的地址", () => {
    expect(projectCoverUrl({ displayCoverMediaId: null })).toBeUndefined();
    expect(projectCoverUrl({ displayCoverMediaId: "" })).toBeUndefined();
    expect(projectCoverUrl({})).toBeUndefined();
    expect(projectCoverUrl(null)).toBeUndefined();
  });
});
