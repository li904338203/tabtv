import { readFileSync, readdirSync, statSync } from "node:fs";
import { join } from "node:path";

import { describe, expect, it } from "vitest";

const APP = join(__dirname, "..");

function vueFiles(dir: string, out: string[] = []): string[] {
  for (const name of readdirSync(dir)) {
    if (["node_modules", ".nuxt", ".output", "dist", ".turbo"].includes(name)) continue;
    const path = join(dir, name);
    if (statSync(path).isDirectory()) vueFiles(path, out);
    else if (name.endsWith(".vue")) out.push(path);
  }
  return out;
}

/**
 * ⚠️ 起因：预览浮层在首页与 AI 视频页**各抄了一份一模一样的标记**，创作资产要预览时
 *    差一点就成了第三份。抄三份的话，哪天调一处（关闭方式、视频标签、下载按钮）另两处
 *    不会跟着变 —— 而三处各自看都没问题。现在只有 `MediaPreviewDialog` 一份。
 */
describe("媒体预览浮层只有一份", () => {
  it("preview-dialog 的标记只出现在 MediaPreviewDialog 里", () => {
    const offenders = vueFiles(APP)
      .filter(file => !file.endsWith("components/media/PreviewDialog.vue"))
      .filter(file => /class="[^"]*\bpreview-dialog\b/.test(readFileSync(file, "utf8")))
      .map(file => file.replace(APP, ""));
    expect(offenders, `这些页面自己写了一份预览浮层：\n${offenders.join("\n")}`).toEqual([]);
  });

  // 精选作品墙（ShowcaseGrid）随被排除的公开精选一起删除（TT-T-13）。
  it.each(["pages/works.vue", "components/canvas/BaseNode.vue"])("%s 用的是 MediaPreviewDialog", (page) => {
    expect(readFileSync(join(APP, page), "utf8")).toContain("<MediaPreviewDialog");
  });

  it("创作资产的下载在预览里，卡片上不再有下载按钮", () => {
    const text = readFileSync(join(APP, "pages/works.vue"), "utf8");
    const card = text.slice(text.indexOf('class="work-card"'), text.indexOf("</article>"));
    expect(card, "卡片上又出现了下载按钮").not.toMatch(/title="下载"|<Download\b/);
    const dialog = text.slice(text.indexOf("<MediaPreviewDialog"), text.indexOf("</MediaPreviewDialog>"));
    expect(dialog).toContain("downloadCurrent");
  });
});

/**
 * 预览的**状态**（逐项取原图、防串台、释放、下载命名）也只有一份：`useOutputPreview`。
 * ⚠️ 它原先写在 works.vue 里；画布节点要同样的预览时再抄一份，两处早晚分叉。
 */
describe("产物预览状态与画布节点的双击预览", () => {
  const read = (path: string) => readFileSync(join(APP, path), "utf8");

  it.each(["pages/works.vue", "components/canvas/BaseNode.vue"])("%s 用的是 useOutputPreview，不自带一份", (file) => {
    const text = read(file);
    expect(text).toContain("useOutputPreview()");
    expect(text, "又自己取了一遍产物列表").not.toMatch(/\/generations\/\$\{[^}]+\}\/outputs/);
  });

  it("节点里的浮层传送到 body —— 画布视口带 transform，fixed 浮层放在节点里会跟着画布缩放、铺不满屏", () => {
    const text = read("components/canvas/BaseNode.vue");
    const teleport = text.slice(text.indexOf('<Teleport to="body">'), text.indexOf("</Teleport>"));
    expect(teleport).toContain("<MediaPreviewDialog");
  });

  it("双击图片打开预览，且不冒泡到画布（否则 Vue Flow 顺手把视口放大一级）", () => {
    expect(read("components/canvas/BaseNode.vue")).toMatch(/class="node-image-output[^"]*"[^>]*@dblclick\.stop="openImagePreview"/);
  });

  it("浮层在捕获阶段独占键盘 —— 否则在预览里按 Delete 会删掉背后选中的节点", () => {
    const text = read("components/media/PreviewDialog.vue");
    expect(text).toContain('window.addEventListener("keydown", onKeydown, true)');
    expect(text).toMatch(/function onKeydown[\s\S]*?event\.stopPropagation\(\)/);
  });
});
