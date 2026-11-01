import { describe, expect, it } from "vitest";
import { pickerPlacement } from "./pickerPlacement";

describe("pickerPlacement", () => {
  it("上方放得下就向上，高度取理想值", () => {
    expect(pickerPlacement({ above: 600, below: 100, preferred: 300 })).toEqual({ side: "top", maxHeight: 300 });
  });

  it("上方放不下而下方放得下就向下（截图里被顶栏裁掉的情形）", () => {
    expect(pickerPlacement({ above: 150, below: 500, preferred: 300 })).toEqual({ side: "bottom", maxHeight: 300 });
  });

  it("两边都放不下：上方还够用且不比下方小就留在上方，高度压到可用空间", () => {
    expect(pickerPlacement({ above: 236, below: 200, preferred: 300 })).toEqual({ side: "top", maxHeight: 220 });
  });

  it("两边都放不下：去空间大的一侧", () => {
    expect(pickerPlacement({ above: 120, below: 250, preferred: 300 })).toEqual({ side: "bottom", maxHeight: 234 });
  });

  it("空间为负（输入框已在顶栏之下被遮住）也不给负高度", () => {
    const p = pickerPlacement({ above: -40, below: -10, preferred: 300 });
    expect(p.maxHeight).toBeGreaterThan(0);
  });
});
