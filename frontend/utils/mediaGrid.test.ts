import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { describe, expect, it } from "vitest";
import {
  MEDIA_GRID_BREAKPOINTS,
  canHoverPlay,
  coverPanels,
  mediaGridColumns,
  startHoverPreview,
  stopHoverPreview
} from "./mediaGrid";
import { isFillableOption, nextFillRequest } from "./marketingFill";
import type { MarketingOption } from "./marketingOptions";

function source(relative: string) {
  return readFileSync(fileURLToPath(new URL(relative, import.meta.url)), "utf8");
}

/** 从组件源码里取出 `@media (min-width: Npx) { .xxx { grid-template-columns: repeat(K, …) } }`。 */
function cssBreakpoints(text: string, gridClass: string) {
  const pattern = new RegExp(String.raw`@media \(min-width: (\d+)px\) \{ \.${gridClass} \{ grid-template-columns: repeat\((\d+),`, "g");
  return [...text.matchAll(pattern)]
    .map(match => ({ minWidth: Number(match[1]), columns: Number(match[2]) }))
    .sort((a, b) => b.minWidth - a.minWidth);
}

describe("mediaGridColumns 断点", () => {
  it("≥1440 4 列、≥1024 3 列、≥640 2 列、更窄 1 列（边界取 >=）", () => {
    expect(mediaGridColumns(1920)).toBe(4);
    expect(mediaGridColumns(1440)).toBe(4);
    expect(mediaGridColumns(1439)).toBe(3);
    expect(mediaGridColumns(1024)).toBe(3);
    expect(mediaGridColumns(1023)).toBe(2);
    expect(mediaGridColumns(640)).toBe(2);
    expect(mediaGridColumns(639)).toBe(1);
    expect(mediaGridColumns(375)).toBe(1);
  });

  // 作品墙（ShowcaseGrid）随精选作品一起删除（TT-T-13），只剩模板墙。
  it("模板墙的 CSS 断点与 MEDIA_GRID_BREAKPOINTS 逐条一致（改一边另一边会红）", () => {
    expect(cssBreakpoints(source("../components/marketing/TemplateWall.vue"), "tpl-grid")).toEqual(MEDIA_GRID_BREAKPOINTS);
  });
});

describe("coverPanels 三联封面等分", () => {
  it("1~3 张按实际张数等分，不复制凑满", () => {
    expect(coverPanels(1)).toEqual({ indexes: [0], widthPercent: 100 });
    expect(coverPanels(2)).toEqual({ indexes: [0, 1], widthPercent: 50 });
    expect(coverPanels(3).indexes).toEqual([0, 1, 2]);
    expect(coverPanels(3).widthPercent).toBeCloseTo(33.333, 2);
  });

  it("超过三张只取前三张；0 张 / 非法值给空（调用方画占位）", () => {
    expect(coverPanels(5).indexes).toEqual([0, 1, 2]);
    expect(coverPanels(0)).toEqual({ indexes: [], widthPercent: 0 });
    expect(coverPanels(-1).indexes).toEqual([]);
    expect(coverPanels(Number.NaN).indexes).toEqual([]);
  });
});

describe("canHoverPlay 悬停播放", () => {
  it("精确指针才播放；触屏不悬停播放", () => {
    expect(canHoverPlay(query => ({ matches: query === "(hover: hover) and (pointer: fine)" }))).toBe(true);
    expect(canHoverPlay(() => ({ matches: false }))).toBe(false);
  });

  it("取不到 matchMedia 或它抛错时不启用", () => {
    expect(canHoverPlay(undefined)).toBe(false);
    expect(canHoverPlay(() => { throw new Error("boom"); })).toBe(false);
  });

  it("play 被打断（AbortError）不冒泡；离开回到封面帧", async () => {
    const calls: string[] = [];
    const video = {
      currentTime: 3,
      play: () => { calls.push("play"); return Promise.reject(new Error("AbortError")); },
      pause: () => { calls.push("pause"); }
    };
    startHoverPreview(video);
    await Promise.resolve();
    stopHoverPreview(video, 0.1);
    expect(calls).toEqual(["play", "pause"]);
    expect(video.currentTime).toBe(0.1);
    stopHoverPreview(video);
    expect(video.currentTime).toBe(0);
  });
});

describe("模板墙填入请求（MK-16）", () => {
  const creative = { kind: "creative", code: "c1", name: "达人口播" } as MarketingOption;

  it("同一张卡点两次是两个请求（seq 递增），不会被当成没变化吞掉", () => {
    const first = nextFillRequest(null, creative);
    const second = nextFillRequest(first, creative);
    expect(first.seq).toBe(1);
    expect(second.seq).toBe(2);
    // 上一个已被消费（null）时仍从记下的序号往后走
    expect(nextFillRequest(null, creative, second.seq).seq).toBe(3);
  });

  it("请求里只有选项本身，没有任何发送开关", () => {
    expect(Object.keys(nextFillRequest(null, creative)).sort()).toEqual(["option", "seq"]);
  });

  it("只接受创意（模板墙 = 创意库）", () => {
    expect(isFillableOption(creative)).toBe(true);
    expect(isFillableOption({ kind: "hook" })).toBe(false);
    expect(isFillableOption(null)).toBe(false);
  });
});
