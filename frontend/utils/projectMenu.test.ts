import { readFileSync } from "node:fs";
import { join } from "node:path";

import { describe, expect, it } from "vitest";

import { folderMenuMode } from "./projectMenu";

const APP = join(__dirname, "..");
const read = (path: string) => readFileSync(join(APP, path), "utf8");

describe("菜单里文件夹那一项", () => {
  it("已在文件夹里 → 只给「移出」", () => {
    expect(folderMenuMode("96209", 3)).toBe("move-out");
    // 哪怕一个别的文件夹都没有，也要能移出来
    expect(folderMenuMode("96209", 0)).toBe("move-out");
  });

  it("未分类且有文件夹 → 「移动至」子菜单", () => {
    expect(folderMenuMode(null, 2)).toBe("move-into");
    expect(folderMenuMode("", 2)).toBe("move-into");
    expect(folderMenuMode(undefined, 1)).toBe("move-into");
  });

  it("未分类且一个文件夹都没有 → 不出现（点开是空的子菜单更糟）", () => {
    expect(folderMenuMode(null, 0)).toBe("none");
  });
});

/**
 * ⚠️ 「几个页面的菜单一样」要靠**只有一份**来保证，而不是靠现在碰巧一样。
 *    这一组扫描钉住：列项目的页面（此前还有工作台首页，2026-11-01 起根路径改为重定向到创造页）都用同一个组件，
 *    删除确认与操作接口只写在一处。
 *    哪天有人在某个页面里又抄一份菜单（比如改个文案图省事），这里当场红。
 */
describe("项目卡片菜单只有一份", () => {
  // 根路径 2026-11-01 起直接重定向到创造页（不再列项目），列项目的只剩 AI 视频页与全部项目页
  const PAGES = ["pages/video/index.vue", "pages/video/projects.vue"];

  it.each(PAGES)("%s 的项目卡片用的是 ProjectCardMenu", (page) => {
    expect(read(page)).toContain("<ProjectCardMenu");
  });

  it.each(PAGES)("%s 的项目标题用的是 ProjectCardTitle（双击改名、回车/Esc/失焦三种收尾一致）", (page) => {
    expect(read(page)).toContain("<ProjectCardTitle");
  });

  it("删除确认文案只写在 useProjectActions 里", () => {
    // 这句话决定了用户以为删掉的东西还能不能找回来 —— 三处各写一句，
    // 早晚会有一处写成「无法恢复」（而项目其实是进了回收站）。
    for (const page of PAGES) {
      expect(read(page), `${page} 自己写了一份删除确认`).not.toContain("将移入回收站");
    }
    expect(read("composables/useProjectActions.ts")).toContain("将移入回收站");
  });

  it("页面不直接调项目的复制/删除接口", () => {
    for (const page of PAGES) {
      const text = read(page);
      expect(text, `${page} 绕过 useProjectActions 直接复制项目`).not.toMatch(/\/duplicate`/);
      expect(text, `${page} 绕过 useProjectActions 直接删除项目`)
        .not.toMatch(/`\/projects\/\$\{[^}]+\}`,\s*\{\s*method:\s*"DELETE"/);
    }
  });
});
