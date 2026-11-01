import { describe, expect, it } from "vitest";
import { pageItems, totalPageCount } from "./pagination";

describe("pagination helpers", () => {
  /** 425 件作品 / 每页 24 = 18 页，最后一页 17 条；算错一页就会有作品永远打不开。 */
  it("counts pages including the partial last one", () => {
    expect(totalPageCount(425, 24)).toBe(18);
    expect(totalPageCount(24, 24)).toBe(1);
    expect(totalPageCount(25, 24)).toBe(2);
  });

  /** 空列表也要有 1 页，否则页码条会渲染出 0 页这种不存在的状态。 */
  it("never reports fewer than one page", () => {
    expect(totalPageCount(0, 24)).toBe(1);
    expect(totalPageCount(-1, 24)).toBe(1);
    expect(totalPageCount(10, 0)).toBe(1);
  });

  it("lists every page when they fit", () => {
    expect(pageItems(1, 5)).toEqual([1, 2, 3, 4, 5]);
    expect(pageItems(3, 7)).toEqual([1, 2, 3, 4, 5, 6, 7]);
  });

  it("folds the middle once there are too many pages", () => {
    expect(pageItems(1, 18)).toEqual([1, 2, "gap", 18]);
    expect(pageItems(9, 18)).toEqual([1, "gap", 8, 9, 10, "gap", 18]);
    expect(pageItems(18, 18)).toEqual([1, "gap", 17, 18]);
  });

  /** 只差一页时显示那一页比显示省略号有用：点省略号什么也不会发生。 */
  it("shows the single skipped page instead of an ellipsis", () => {
    expect(pageItems(3, 18)).toEqual([1, 2, 3, 4, "gap", 18]);
  });

  /** 越界页码要夹回合法范围，不能渲染出 0 或 19 这种点不开的页。 */
  it("clamps an out-of-range current page", () => {
    expect(pageItems(0, 18)).toEqual([1, 2, "gap", 18]);
    expect(pageItems(99, 18)).toEqual([1, "gap", 17, 18]);
  });
});
