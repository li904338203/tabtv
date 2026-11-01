import { describe, expect, it } from "vitest";
import {
  TIDY_COLUMN_GAP,
  TIDY_GROUP_GAP,
  TIDY_ROW_GAP,
  tidyCanvasLayout,
  type TidyEdge,
  type TidyNode,
  type TidyPoint
} from "./canvasTidy";

function node(id: string, x = 0, y = 0, width = 300, height = 200, members?: string[]): TidyNode {
  return { id, x, y, width, height, ...(members ? { members } : {}) };
}

function edges(...pairs: string[]): TidyEdge[] {
  return pairs.map((pair) => {
    const [source, target] = pair.split(">");
    return { source, target };
  });
}

type Rect = { id: string; x: number; y: number; width: number; height: number };

function rects(nodes: TidyNode[], result: Map<string, TidyPoint>): Map<string, Rect> {
  return new Map(nodes.map((item) => {
    const at = result.get(item.id)!;
    return [item.id, { id: item.id, x: at.x, y: at.y, width: item.width, height: item.height }];
  }));
}

const centerY = (rect: Rect) => rect.y + rect.height / 2;
const overlaps = (a: Rect, b: Rect) => a.x < b.x + b.width && b.x < a.x + a.width && a.y < b.y + b.height && b.y < a.y + a.height;

function expectNoOverlap(all: Rect[]) {
  for (let i = 0; i < all.length; i++) {
    for (let j = i + 1; j < all.length; j++) {
      expect(overlaps(all[i], all[j]), `${all[i].id} 与 ${all[j].id} 重叠`).toBe(false);
    }
  }
}

function tidyAgain(nodes: TidyNode[], result: Map<string, TidyPoint>): TidyNode[] {
  return nodes.map(item => ({ ...item, ...result.get(item.id)! }));
}

describe("整理画布：连在一起的节点按连线从左往右分列（2026-11-01）", () => {
  it("一条链：一列一个、列间距固定，中线对齐（连线是直的）", () => {
    const nodes = [node("a", 500, 900, 300, 200), node("b", 0, 0, 340, 360), node("c", 900, 300, 260, 120)];
    const at = rects(nodes, tidyCanvasLayout(nodes, edges("a>b", "b>c")));
    const [a, b, c] = ["a", "b", "c"].map(id => at.get(id)!);
    expect(b.x - (a.x + a.width)).toBe(TIDY_COLUMN_GAP);
    expect(c.x - (b.x + b.width)).toBe(TIDY_COLUMN_GAP);
    expect(Math.abs(centerY(a) - centerY(b))).toBeLessThanOrEqual(1);
    expect(Math.abs(centerY(b) - centerY(c))).toBeLessThanOrEqual(1);
  });

  it("一个分出三个：三个下游在同一列竖着摞、间距固定，上游在它们正中", () => {
    const nodes = [node("a", 0, 0), node("b", 600, 0), node("c", 600, 50), node("d", 600, 100)];
    const at = rects(nodes, tidyCanvasLayout(nodes, edges("a>b", "a>c", "a>d")));
    const [a, b, c, d] = ["a", "b", "c", "d"].map(id => at.get(id)!);
    expect(new Set([b.x, c.x, d.x]).size).toBe(1);
    // 原来的上下顺序保持：b 在 c 上面、c 在 d 上面
    expect(c.y - (b.y + b.height)).toBe(TIDY_ROW_GAP);
    expect(d.y - (c.y + c.height)).toBe(TIDY_ROW_GAP);
    expect(Math.abs(centerY(a) - centerY(c))).toBeLessThanOrEqual(1);
  });

  it("下游一律在上游右边，整张图没有重叠", () => {
    const nodes = [
      node("script", 0, 0, 320, 260), node("asset1", 0, 0, 300, 220), node("asset2", 0, 0, 300, 220),
      node("ref", 0, 0, 280, 180), node("shot1", 0, 0, 300, 240), node("shot2", 0, 0, 300, 240),
      node("video", 0, 0, 360, 300), node("cut", 0, 0, 340, 260)
    ];
    const links = edges(
      "script>asset1", "script>asset2", "asset1>shot1", "asset2>shot2", "ref>shot2",
      "shot1>video", "shot2>video", "video>cut", "script>cut"
    );
    const at = rects(nodes, tidyCanvasLayout(nodes, links));
    for (const link of links) {
      const from = at.get(link.source)!;
      const to = at.get(link.target)!;
      expect(to.x, `${link.source} → ${link.target}`).toBeGreaterThanOrEqual(from.x + from.width + TIDY_COLUMN_GAP);
    }
    expectNoOverlap([...at.values()]);
  });

  it("没有上游的节点放在它下游紧左边那一列，不挤到最左列拉长线", () => {
    const nodes = [node("a"), node("b"), node("c"), node("d"), node("late", 0, 600)];
    const at = rects(nodes, tidyCanvasLayout(nodes, edges("a>b", "b>c", "c>d", "late>d")));
    expect(at.get("late")!.x).toBe(at.get("c")!.x);
  });

  it("两个上游汇到一个下游：保持原来的上下顺序", () => {
    const nodes = [node("upper", 0, 0), node("lower", 0, 800), node("join", 500, 400)];
    const at = rects(nodes, tidyCanvasLayout(nodes, edges("lower>join", "upper>join")));
    expect(at.get("upper")!.y).toBeLessThan(at.get("lower")!.y);
  });

  it("有交叉时调顺序把交叉去掉", () => {
    // a1 在上、a2 在下，但 a1 连的是下面的 b2、a2 连的是上面的 b1：原样排会交叉
    const nodes = [node("a1", 0, 0), node("a2", 0, 400), node("b1", 600, 0), node("b2", 600, 400), node("c", 1200, 0)];
    const at = rects(nodes, tidyCanvasLayout(nodes, edges("a1>b2", "a2>b1", "b1>c", "b2>c")));
    const [a1, a2, b1, b2] = ["a1", "a2", "b1", "b2"].map(id => at.get(id)!);
    expect(Math.sign(a1.y - a2.y)).toBe(Math.sign(b2.y - b1.y));
  });

  it("连线成环也排得出来：不死循环、位置都是有限数、不重叠", () => {
    const nodes = [node("a", 0, 0), node("b", 400, 0), node("c", 800, 0), node("d", 1200, 0)];
    const result = tidyCanvasLayout(nodes, edges("a>b", "b>c", "c>a", "c>d"));
    expect(result.size).toBe(4);
    for (const point of result.values()) expect(Number.isFinite(point.x) && Number.isFinite(point.y)).toBe(true);
    expectNoOverlap([...rects(nodes, result).values()]);
  });
});

describe("整理画布：画框、孤立节点、位置", () => {
  it("画框连同框里的节点整块挪：相对位置不变，框外的下游排在框右边", () => {
    const nodes = [
      node("frame", 100, 100, 800, 500, ["m1", "m2"]),
      node("m1", 140, 180, 300, 200),
      node("m2", 520, 220, 300, 200),
      node("out", 0, 2000, 300, 200)
    ];
    const result = tidyCanvasLayout(nodes, edges("m2>out", "m1>m2"));
    const frame = result.get("frame")!;
    expect({ x: result.get("m1")!.x - frame.x, y: result.get("m1")!.y - frame.y }).toEqual({ x: 40, y: 80 });
    expect({ x: result.get("m2")!.x - frame.x, y: result.get("m2")!.y - frame.y }).toEqual({ x: 420, y: 120 });
    expect(result.get("out")!.x).toBe(frame.x + 800 + TIDY_COLUMN_GAP);
  });

  it("没有连线的节点排成网格放在连好的那组下面，互不重叠", () => {
    const nodes = [
      node("a", 0, 0), node("b", 900, 0),
      ...["l1", "l2", "l3", "l4", "l5"].map((id, index) => node(id, index * 50, 300 + index * 30, 260, 180))
    ];
    const at = rects(nodes, tidyCanvasLayout(nodes, edges("a>b")));
    const groupBottom = Math.max(at.get("a")!.y + 200, at.get("b")!.y + 200);
    for (const id of ["l1", "l2", "l3", "l4", "l5"]) expect(at.get(id)!.y).toBeGreaterThanOrEqual(groupBottom + TIDY_GROUP_GAP);
    expectNoOverlap([...at.values()]);
    // 不是排成一长条：至少分了两行
    expect(new Set(["l1", "l2", "l3", "l4", "l5"].map(id => at.get(id)!.y)).size).toBeGreaterThan(1);
  });

  it("排完整体还在原来的左上角，画布不跳走", () => {
    const nodes = [node("a", 1000, 2000), node("b", 1500, 2600), node("c", 3000, 2100), node("lonely", 1200, 4000)];
    const result = tidyCanvasLayout(nodes, edges("a>b", "b>c"));
    expect(Math.min(...[...result.values()].map(point => point.x))).toBe(1000);
    expect(Math.min(...[...result.values()].map(point => point.y))).toBe(2000);
  });

  it("开着网格吸附：每个节点的左上角都落在格点上", () => {
    const nodes = [node("a", 13, 7, 310, 205), node("b", 400, 90, 290, 170), node("c", 800, 300, 330, 260), node("d", 50, 900)];
    const result = tidyCanvasLayout(nodes, edges("a>b", "a>c"), { snap: 22 });
    for (const point of result.values()) {
      expect(point.x % 22).toBe(0);
      expect(point.y % 22).toBe(0);
    }
  });

  it("整理过的再整理一次不会再动", () => {
    const nodes = [
      node("s", 37.5, 12.25, 320, 260), node("x1", 400, 0, 300, 230), node("x2", 400, 300, 300, 170),
      node("y", 900, 100, 360, 300), node("z", 1400, 80, 280, 200), node("free1", 0, 900, 260, 180),
      node("free2", 300, 920, 260, 180), node("frame", 1800, 0, 500, 400, ["inner"]), node("inner", 1850, 60, 300, 200)
    ];
    const links = edges("s>x1", "s>x2", "x1>y", "x2>y", "y>z", "z>inner");
    const first = tidyCanvasLayout(nodes, links);
    const second = tidyCanvasLayout(tidyAgain(nodes, first), links);
    expect(Object.fromEntries(second)).toEqual(Object.fromEntries(first));
  });

  it("空画布什么都不返回；只有一个节点时它不动", () => {
    expect(tidyCanvasLayout([], []).size).toBe(0);
    expect(tidyCanvasLayout([node("only", 120, 340)], []).get("only")).toEqual({ x: 120, y: 340 });
  });
});
