import { describe, expect, it } from "vitest";
import { IMAGE_TOOL_FALLBACK_CONFIG, imageToolDefaultOptions, imageToolWireOptions } from "./imageTools";
import {
  imageToolRelightApplyPreset,
  imageToolRelightClampBrightness,
  imageToolRelightColorIndex,
  imageToolRelightColorLabel,
  imageToolRelightCone,
  imageToolRelightIsBack,
  imageToolRelightMatchPreset,
  imageToolRelightNormalizeColor,
  imageToolRelightOptionsOf,
  imageToolRelightProject,
  imageToolRelightStep,
  imageToolRelightUnproject,
  imageToolRelightWithDirection
} from "./imageToolRelight";

const presets = IMAGE_TOOL_FALLBACK_CONFIG.lightPresets;
const colors = IMAGE_TOOL_FALLBACK_CONFIG.namedColors;
const defaults = imageToolDefaultOptions("relight");

describe("光源球投影", () => {
  it("正面视角：前方在圆心、右侧在右缘、左侧在左缘、顶部在上方，后方在背面不可见", () => {
    expect(imageToolRelightProject(0, 0, "front")).toEqual({ x: 0, y: 0, visible: true });
    expect(imageToolRelightProject(90, 0, "front")).toMatchObject({ x: 1, y: 0 });
    expect(imageToolRelightProject(270, 0, "front")).toMatchObject({ x: -1, y: 0 });
    const top = imageToolRelightProject(0, 80, "front");
    expect(top.y).toBeLessThan(-0.9);
    expect(top.visible).toBe(true);
    expect(imageToolRelightProject(180, 0, "front")).toEqual({ x: 0, y: 0, visible: false });
  });

  it("透视视角：前方光落在圆心下方、后方光在上方且被遮住 —— 两个半球分得开", () => {
    const front = imageToolRelightProject(0, 0, "perspective");
    const back = imageToolRelightProject(180, 0, "perspective");
    expect(front.y).toBeGreaterThan(0.2);
    expect(front.visible).toBe(true);
    expect(back.y).toBeLessThan(-0.2);
    expect(back.visible).toBe(false);
  });

  it("投影 → 反算回到原来的整数角度（两种视角、前后两个半球）", () => {
    for (const view of ["front", "perspective"] as const) {
      for (const [azimuth, elevation] of [[0, 0], [90, 0], [270, 0], [30, 20], [315, -40], [180, 0], [150, 10], [200, -25]]) {
        const point = imageToolRelightProject(azimuth!, elevation!, view);
        const back = !point.visible;
        expect(imageToolRelightUnproject(point, view, { back }), `${view} ${azimuth}/${elevation}`).toEqual({ azimuth, elevation });
      }
    }
  });

  it("拖出球外收回到圆周；极点沿用原方位角；输出总是整数", () => {
    const outside = imageToolRelightUnproject({ x: 3, y: 0 }, "front");
    expect(outside).toEqual({ azimuth: 90, elevation: 0 });
    expect(imageToolRelightUnproject({ x: 0, y: -1 }, "front", { fallbackAzimuth: 45 })).toEqual({ azimuth: 45, elevation: 90 });
    const odd = imageToolRelightUnproject({ x: 0.123, y: -0.456 }, "perspective");
    expect(Number.isInteger(odd.azimuth) && Number.isInteger(odd.elevation)).toBe(true);
  });

  it("isBack 跟着投影的可见性走", () => {
    expect(imageToolRelightIsBack(180, 0, "front")).toBe(true);
    expect(imageToolRelightIsBack(0, 0, "front")).toBe(false);
  });
});

describe("方向键", () => {
  it("← / → 朝屏幕左右移动：背面时也不会反向", () => {
    expect(imageToolRelightStep({ azimuth: 0, elevation: 0 }, "left")).toEqual({ azimuth: 345, elevation: 0 });
    expect(imageToolRelightStep({ azimuth: 0, elevation: 0 }, "right")).toEqual({ azimuth: 15, elevation: 0 });
    // 光在正后方：左移应该到左后（195），不是右后（165）
    expect(imageToolRelightStep({ azimuth: 180, elevation: 0 }, "left")).toEqual({ azimuth: 195, elevation: 0 });
    expect(imageToolRelightStep({ azimuth: 180, elevation: 0 }, "right")).toEqual({ azimuth: 165, elevation: 0 });
  });

  it("到了最左 / 最右就停；正对面时经过前方", () => {
    expect(imageToolRelightStep({ azimuth: 270, elevation: 0 }, "left")).toEqual({ azimuth: 270, elevation: 0 });
    expect(imageToolRelightStep({ azimuth: 280, elevation: 0 }, "left")).toEqual({ azimuth: 270, elevation: 0 });
    expect(imageToolRelightStep({ azimuth: 90, elevation: 0 }, "left")).toEqual({ azimuth: 75, elevation: 0 });
    expect(imageToolRelightStep({ azimuth: 270, elevation: 0 }, "right")).toEqual({ azimuth: 285, elevation: 0 });
  });

  it("↑ / ↓ 调仰角并夹在 ±90", () => {
    expect(imageToolRelightStep({ azimuth: 0, elevation: 80 }, "up")).toEqual({ azimuth: 0, elevation: 90 });
    expect(imageToolRelightStep({ azimuth: 0, elevation: -80 }, "down")).toEqual({ azimuth: 0, elevation: -90 });
    expect(imageToolRelightStep({ azimuth: 0, elevation: 0 }, "up")).toEqual({ azimuth: 0, elevation: 15 });
  });
});

describe("六方向按钮", () => {
  it("预设顺序与服务端 LightPresets 一致（两行三列：左侧 / 顶部 / 右侧 / 前方 / 底部 / 后方）", () => {
    expect(presets.map(item => [item.code, item.azimuth, item.elevation])).toEqual([
      ["left", 270, 0], ["top", 0, 80], ["right", 90, 0], ["front", 0, 0], ["bottom", 0, -80], ["back", 180, 0]
    ]);
  });

  it("每个按钮写固定值并高亮自己", () => {
    for (const preset of presets) {
      const next = imageToolRelightApplyPreset(defaults, presets, preset.code);
      expect(next).toMatchObject({ preset: preset.code, azimuth: preset.azimuth, elevation: preset.elevation });
      expect(imageToolRelightMatchPreset(presets, next.azimuth!, next.elevation!)).toBe(preset.code);
    }
  });

  it("拖到不匹配任何预设的位置：按钮全不亮且 preset 字段被去掉；拖回恰好等于预设时重新亮起", () => {
    const dragged = imageToolRelightWithDirection({ ...defaults, preset: "front" }, presets, { azimuth: 33, elevation: 12 });
    expect(imageToolRelightMatchPreset(presets, 33, 12)).toBeNull();
    expect(dragged.preset).toBeUndefined();
    expect(dragged).toMatchObject({ azimuth: 33, elevation: 12 });
    const back = imageToolRelightWithDirection(dragged, presets, { azimuth: 450, elevation: 0 });
    expect(back).toMatchObject({ preset: "right", azimuth: 90, elevation: 0 });
  });

  it("360° 与 0° 视为同一方向；未知预设码原样返回", () => {
    expect(imageToolRelightMatchPreset(presets, 360, 0)).toBe("front");
    const options = { ...defaults, azimuth: 12 };
    expect(imageToolRelightApplyPreset(options, presets, "nope")).toBe(options);
  });
});

describe("亮度与颜色", () => {
  it("亮度夹在 0~100 整数，非数字保持原值", () => {
    expect(imageToolRelightClampBrightness(120)).toBe(100);
    expect(imageToolRelightClampBrightness(-3)).toBe(0);
    expect(imageToolRelightClampBrightness("66%")).toBe(66);
    expect(imageToolRelightClampBrightness("49.6")).toBe(50);
    expect(imageToolRelightClampBrightness("", 70)).toBe(70);
    expect(imageToolRelightClampBrightness("abc", 30)).toBe(30);
  });

  it("颜色规范成小写 #rrggbb，空 / 非法 = 无", () => {
    expect(imageToolRelightNormalizeColor("#FFB347")).toBe("#ffb347");
    expect(imageToolRelightNormalizeColor("fb3")).toBe("#ffbb33");
    expect(imageToolRelightNormalizeColor("")).toBeNull();
    expect(imageToolRelightNormalizeColor("#12345")).toBeNull();
    expect(imageToolRelightNormalizeColor(null)).toBeNull();
  });

  it("色板 16 色、顺序与服务端 NamedColors 一致；查找不区分大小写", () => {
    expect(colors).toHaveLength(16);
    expect(colors.map(item => item.name)).toEqual(["暖白", "冷白", "琥珀", "橙", "红", "品红", "粉", "紫", "蓝", "青", "绿", "黄绿", "金黄", "月光蓝", "霓虹青", "夕阳红"]);
    expect(imageToolRelightColorIndex(colors, "#ffb347")).toBe(2);
    expect(imageToolRelightColorIndex(colors, "#123456")).toBe(-1);
    expect(imageToolRelightColorIndex(colors, null)).toBe(-1);
  });

  it("颜色说明：无 / 色板名 / 自定义", () => {
    expect(imageToolRelightColorLabel(colors, null)).toBe("无（保留原色温）");
    expect(imageToolRelightColorLabel(colors, "#FF3B30")).toBe("红");
    expect(imageToolRelightColorLabel(colors, "#123ABC")).toBe("自定义 #123abc");
  });
});

describe("节点选项回显与请求", () => {
  it("默认 = 前方、50%、无颜色、关轮廓光、关智能（IMG-F-44 的重置值）", () => {
    expect(defaults).toEqual({ smart: false, preset: "front", azimuth: 0, elevation: 0, brightness: 50, color: null, rim: false });
  });

  it("节点上只存了 {smart:true} 时其余回到默认；坏值不报错", () => {
    expect(imageToolRelightOptionsOf({ smart: true }, defaults, presets)).toEqual({ ...defaults, smart: true });
    expect(imageToolRelightOptionsOf(null, defaults, presets)).toEqual(defaults);
    expect(imageToolRelightOptionsOf({ azimuth: 90, elevation: 0, brightness: 999, color: "red!", rim: true }, defaults, presets))
      .toEqual({ smart: false, preset: "right", azimuth: 90, elevation: 0, brightness: 100, color: null, rim: true });
    const custom = imageToolRelightOptionsOf({ azimuth: 40, elevation: 5, color: "#ABCDEF" }, defaults, presets);
    expect(custom.preset).toBeUndefined();
    expect(custom.color).toBe("#abcdef");
  });

  it("智能模式打开时请求里只发 {smart:true}", () => {
    const options = { ...imageToolApplyCustom(), smart: true };
    expect(imageToolWireOptions("relight", options)).toEqual({ smart: true });
  });
});

describe("光锥", () => {
  it("顶点在光源、底边垂直于光线并以中心对称", () => {
    expect(imageToolRelightCone({ x: 0, y: 0 }, { x: 10, y: 0 }, 3)).toBe("0,0 10,3 10,-3");
  });

  it("光源与中心重合时不画", () => {
    expect(imageToolRelightCone({ x: 5, y: 5 }, { x: 5, y: 5 }, 3)).toBe("");
  });
});

function imageToolApplyCustom() {
  return imageToolRelightWithDirection({ ...defaults, brightness: 80, color: "#ff0000", rim: true }, presets, { azimuth: 40, elevation: 10 });
}
