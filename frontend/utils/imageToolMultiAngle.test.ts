import { describe, expect, it } from "vitest";
import {
  multiAngleCameraDistance,
  multiAngleFromScreen,
  multiAngleGrid,
  multiAngleHorizontalText,
  multiAngleKeyStep,
  multiAngleOptionsOf,
  multiAngleOrbit,
  multiAngleProject,
  multiAnglePromptClamp,
  multiAnglePromptLength,
  multiAngleStateFromParams,
  multiAngleVerticalText,
  multiAngleWrap
} from "./imageToolMultiAngle";

describe("multiAngleProject", () => {
  it("正面平视在看得见的一面、略低于球心（从斜上方看）", () => {
    const point = multiAngleProject(0, 0);
    expect(point.x).toBeCloseTo(0);
    expect(point.y).toBeGreaterThan(0);
    expect(point.depth).toBeGreaterThan(0);
  });

  it("90° 在屏幕右侧、270° 在左侧、180° 在背面", () => {
    expect(multiAngleProject(90, 0).x).toBeCloseTo(1);
    expect(multiAngleProject(270, 0).x).toBeCloseTo(-1);
    expect(multiAngleProject(180, 0).depth).toBeLessThan(0);
  });

  it("俯拍越高越靠上，正上方在球心上方", () => {
    expect(multiAngleProject(0, 45).y).toBeLessThan(multiAngleProject(0, 0).y);
    expect(multiAngleProject(0, 90).y).toBeLessThan(0);
  });

  it("不倾斜时就是正视投影", () => {
    const point = multiAngleProject(0, 30, 0);
    expect(point.x).toBeCloseTo(0);
    expect(point.y).toBeCloseTo(-0.5);
  });
});

describe("multiAngleFromScreen", () => {
  it("投影再反算回到原机位（看得见的那一面）", () => {
    for (const [h, v] of [[0, 0], [30, 10], [300, 45], [60, -20], [0, 75]] as const) {
      const point = multiAngleProject(h, v);
      expect(point.depth).toBeGreaterThanOrEqual(0);
      expect(multiAngleFromScreen(point.x, point.y)).toEqual({ horizontal: h, vertical: v });
    }
  });

  it("球外的点夹到轮廓上，俯仰夹在 -30~90", () => {
    const result = multiAngleFromScreen(5, 0);
    expect(result.horizontal).toBeGreaterThan(60);
    expect(result.horizontal).toBeLessThan(120);
    expect(multiAngleFromScreen(0, 1).vertical).toBe(-30);
  });

  it("非法输入回到正中", () => {
    expect(multiAngleFromScreen(Number.NaN, Number.NaN).horizontal).toBe(0);
  });
});

describe("multiAngleOrbit", () => {
  it("往右拖水平角增大，越过 360 绕回；往上拖更俯；切回自定义", () => {
    const start = { preset: "back" as const, horizontal: 350, vertical: 0, zoom: 5 };
    const next = multiAngleOrbit(start, (20 * Math.PI / 180) * 100, -(10 * Math.PI / 180) * 100, 100);
    expect(next).toEqual({ preset: "custom", horizontal: 10, vertical: 10, zoom: 5 });
  });

  it("俯仰拖过头夹在范围内", () => {
    expect(multiAngleOrbit({ horizontal: 0, vertical: 0 }, 0, 10_000, 100).vertical).toBe(-30);
    expect(multiAngleOrbit({ horizontal: 0, vertical: 0 }, 0, -10_000, 100).vertical).toBe(90);
  });
});

describe("multiAngleKeyStep", () => {
  const base = { preset: "dutch" as const, horizontal: 0, vertical: 0, zoom: 5 };
  it("方向键微调 5°，左键绕回 355", () => {
    expect(multiAngleKeyStep(base, "ArrowLeft")).toMatchObject({ preset: "custom", horizontal: 355 });
    expect(multiAngleKeyStep(base, "ArrowUp")).toMatchObject({ preset: "custom", vertical: 5 });
  });
  it("Shift 与箭头按钮同一个大步进", () => {
    expect(multiAngleKeyStep(base, "ArrowRight", true)).toMatchObject({ horizontal: 45 });
    expect(multiAngleKeyStep(base, "ArrowDown", true)).toMatchObject({ vertical: -15 });
  });
  it("Home 回正面平视，其他键不处理", () => {
    expect(multiAngleKeyStep({ horizontal: 120, vertical: 40, zoom: 3 }, "Home")).toMatchObject({ horizontal: 0, vertical: 0, zoom: 3 });
    expect(multiAngleKeyStep(base, "Enter")).toBeNull();
  });
});

describe("multiAngleWrap / 距离 / 文案", () => {
  it("取模", () => {
    expect(multiAngleWrap(360)).toBe(0);
    expect(multiAngleWrap(-45)).toBe(315);
  });
  it("景别越近相机越贴近主体", () => {
    expect(multiAngleCameraDistance(0)).toBe(1);
    expect(multiAngleCameraDistance(10)).toBeLessThan(multiAngleCameraDistance(5));
  });
  it("方位与俯仰说明", () => {
    expect(multiAngleHorizontalText(0)).toBe("正面");
    expect(multiAngleHorizontalText(90)).toBe("右侧");
    expect(multiAngleHorizontalText(180)).toBe("背面");
    expect(multiAngleHorizontalText(350)).toBe("正面");
    expect(multiAngleVerticalText(45)).toBe("俯拍");
    expect(multiAngleVerticalText(-20)).toBe("仰拍");
    expect(multiAngleVerticalText(0)).toBe("平视");
  });
});

describe("multiAngleGrid", () => {
  it("有前有背两类线段，且都是合法 path", () => {
    const segments = multiAngleGrid(100, 100, 80);
    expect(segments.some(item => item.back)).toBe(true);
    expect(segments.some(item => !item.back)).toBe(true);
    for (const item of segments) expect(item.d).toMatch(/^M[\d.-]+,[\d.-]+(L[\d.-]+,[\d.-]+)+$/);
  });
});

describe("节点参数 ↔ 编辑器状态", () => {
  it("子节点带上次的值；有补充提示词即开关开着（IMG-F-38）", () => {
    const state = multiAngleStateFromParams({ toolOptions: { preset: "back", horizontal: 180, vertical: 0, zoom: 7, prompt: "保留雨夜" } });
    expect(state).toEqual({ options: { preset: "back", horizontal: 180, vertical: 0, zoom: 7 }, promptOn: true, promptText: "保留雨夜" });
  });

  it("没有 toolOptions 时回到默认（0°、0°、景别 5、自定义、关提示词）", () => {
    expect(multiAngleStateFromParams({})).toEqual({ options: { preset: "custom", horizontal: 0, vertical: 0, zoom: 5 }, promptOn: false, promptText: "" });
  });

  it("开关关着不带 prompt；开着带截断后的", () => {
    const options = { preset: "custom" as const, horizontal: 10, vertical: 0, zoom: 5 };
    expect(multiAngleOptionsOf({ options, promptOn: false, promptText: "不该发" })).not.toHaveProperty("prompt");
    expect(multiAngleOptionsOf({ options, promptOn: true, promptText: "  " })).not.toHaveProperty("prompt");
    expect(multiAngleOptionsOf({ options, promptOn: true, promptText: "加一点雾" }).prompt).toBe("加一点雾");
  });

  it("提示词按码点截到 200", () => {
    const long = "😀".repeat(210);
    expect(multiAnglePromptLength(multiAnglePromptClamp(long))).toBe(200);
    expect(multiAnglePromptClamp("短")).toBe("短");
  });
});
