import { readdirSync, readFileSync, statSync } from "node:fs";
import { join, relative } from "node:path";
import { describe, expect, it } from "vitest";
import {
  IMAGE_TOOL_FALLBACK_CONFIG,
  IMAGE_TOOL_PRESET_OFFLINE,
  imageToolCount,
  imageToolDefaultOptions,
  imageToolDisabledReason,
  imageToolDisplayBoxToImage,
  imageToolDisplayToImage,
  imageToolEmotionAt,
  imageToolEmotionByName,
  imageToolEmotionGrid,
  imageToolFit,
  imageToolImageBoxToDisplay,
  imageToolIsNew,
  imageToolOfParams,
  imageToolPermissionBlocker,
  imageToolPresetLookup,
  imageToolPresetToOptions,
  imageToolRequest,
  imageToolRoleLabels,
  imageToolRoleLimitReason,
  imageToolSetAngle,
  imageToolSortBlockers,
  imageToolSpawnPosition,
  imageToolStepAngle,
  imageToolsConfigOf,
  imageToolTouchedRoles,
  imageToolUnavailableBlocker,
  imageToolWireOptions,
  imageToolZoomLabel,
  type ImageToolEditorRole
} from "./imageTools";

const config = IMAGE_TOOL_FALLBACK_CONFIG;

describe("配置与兜底", () => {
  it("服务端下发的字段优先，缺的字段逐项用兜底（不是整份替换）", () => {
    const merged = imageToolsConfigOf({ maxRoles: 3, multiAnglePresets: [{ code: "custom", name: "自定", horizontal: 10, vertical: 0, zoom: 4 }] });
    expect(merged.maxRoles).toBe(3);
    expect(merged.multiAnglePresets).toHaveLength(1);
    expect(merged.emotions).toHaveLength(25);
    expect(merged.portraitPreset).toBe("portrait_tuning");
    expect(imageToolsConfigOf(null)).toEqual(config);
  });

  it("空数组当没下发（老服务端的空字段不能把控件清空）", () => {
    expect(imageToolsConfigOf({ upscaleLevels: [] }).upscaleLevels.map(item => item.code)).toEqual(["standard", "fine", "ultra"]);
  });

  it("工具码识别只认四个工具", () => {
    expect(imageToolOfParams({ tool: "upscale" })).toBe("upscale");
    expect(imageToolOfParams({ tool: "img2img" })).toBeUndefined();
    expect(imageToolOfParams(undefined)).toBeUndefined();
  });

  it("NEW 由工具自己的配置决定（宽松读未知字段）", () => {
    const merged = imageToolsConfigOf({ tools: [{ code: "relight", name: "打光", spawn: "打光", isNew: true } as never] });
    expect(imageToolIsNew(merged, "relight")).toBe(true);
    expect(imageToolIsNew(merged, "upscale")).toBe(false);
  });

  it("预设下线时照样给入口并说明原因", () => {
    const presets = [{ code: "portrait_tuning", name: "人像" }];
    expect(imageToolPresetLookup(presets, "portrait_tuning", p => p.code)).toEqual({ preset: presets[0], reason: null });
    expect(imageToolPresetLookup(presets, "panorama_720", p => p.code)).toEqual({ reason: IMAGE_TOOL_PRESET_OFFLINE });
  });
});

describe("默认选项", () => {
  it("四个工具各自的默认值（重置参数也回到这里）", () => {
    expect(imageToolDefaultOptions("multi_angle")).toEqual({ preset: "custom", horizontal: 0, vertical: 0, zoom: 5 });
    expect(imageToolDefaultOptions("relight")).toEqual({ smart: false, preset: "front", azimuth: 0, elevation: 0, brightness: 50, color: null, rim: false });
    expect(imageToolDefaultOptions("expression_edit", { sourceMediaId: "m1" })).toEqual({ sourceMediaId: "m1", count: 1, roles: [] });
    expect(imageToolDefaultOptions("upscale")).toEqual({ level: "standard" });
  });

  it("多角度默认值跟着配置里的 custom 走", () => {
    const merged = imageToolsConfigOf({ multiAnglePresets: [{ code: "custom", name: "自定义", horizontal: 15, vertical: 5, zoom: 4 }] });
    expect(imageToolDefaultOptions("multi_angle", { config: merged })).toEqual({ preset: "custom", horizontal: 15, vertical: 5, zoom: 4 });
  });
});

describe("多角度", () => {
  it("七个预设各自设出对应三元组，补充提示词保留", () => {
    const expected: Record<string, [number, number, number]> = {
      custom: [0, 0, 5], fisheye: [0, 0, 6], dutch: [30, 10, 5], high_front: [0, 45, 5],
      low_front: [0, -30, 5], birds_eye: [0, 75, 1], back: [180, 0, 5]
    };
    for (const [code, [h, v, z]] of Object.entries(expected)) {
      expect(imageToolPresetToOptions(config.multiAnglePresets, code, { prompt: "雨夜" })).toEqual({ preset: code, horizontal: h, vertical: v, zoom: z, prompt: "雨夜" });
    }
  });

  it("预设表由参数给（不写死）；未知预设原样返回", () => {
    const table = [{ code: "back", name: "背", horizontal: 170, vertical: 3, zoom: 7 }];
    expect(imageToolPresetToOptions(table, "back")).toMatchObject({ horizontal: 170, vertical: 3, zoom: 7 });
    expect(imageToolPresetToOptions(table, "nope", { preset: "custom", horizontal: 9 })).toEqual({ preset: "custom", horizontal: 9 });
  });

  it("动滑杆切回自定义并夹到范围内", () => {
    const back = imageToolPresetToOptions(config.multiAnglePresets, "back");
    expect(imageToolSetAngle(back, "zoom", 7)).toMatchObject({ preset: "custom", zoom: 7, horizontal: 180 });
    expect(imageToolSetAngle(back, "vertical", -60).vertical).toBe(-30);
    expect(imageToolSetAngle(back, "vertical", 120).vertical).toBe(90);
    expect(imageToolSetAngle(back, "horizontal", 400).horizontal).toBe(360);
  });

  it("箭头步进：环绕 ±45° 取模，俯仰 ±15° 夹边", () => {
    const start = { preset: "custom" as const, horizontal: 0, vertical: 85, zoom: 5 };
    expect(imageToolStepAngle(start, "left").horizontal).toBe(315);
    expect(imageToolStepAngle({ ...start, horizontal: 315 }, "right").horizontal).toBe(0);
    expect(imageToolStepAngle(start, "up").vertical).toBe(90);
    expect(imageToolStepAngle({ ...start, vertical: -20 }, "down").vertical).toBe(-30);
    expect(imageToolStepAngle({ ...start, preset: "back" as never }, "up").preset).toBe("custom");
  });

  it("景别分档边界（IMG-PARAM-21）", () => {
    const cases: Array<[number, string]> = [[0, "大远景"], [1, "大远景"], [2, "远景"], [3, "远景"], [4, "中景"], [6, "中景"], [7, "近景"], [8, "近景"], [9, "特写"], [10, "特写"], [11, "特写"]];
    for (const [zoom, name] of cases) expect(imageToolZoomLabel(zoom, config.zoomBands), String(zoom)).toBe(name);
  });
});

describe("情绪", () => {
  it("25 格名称与坐标互查，中心「淡然自若」", () => {
    expect(config.emotions).toHaveLength(25);
    expect(imageToolEmotionAt(config.emotions, 0, 0)?.name).toBe("淡然自若");
    expect(imageToolEmotionAt(config.emotions, 2, 2)?.name).toBe("欣喜若狂");
    expect(imageToolEmotionAt(config.emotions, -2, -2)?.name).toBe("心灰意冷");
    expect(imageToolEmotionAt(config.emotions, 2, -2)?.name).toBe("怒不可遏");
    expect(imageToolEmotionAt(config.emotions, 3, 0)).toBeUndefined();
    for (const item of config.emotions) {
      expect(imageToolEmotionByName(config.emotions, item.name)).toEqual({ arousal: item.arousal, warmth: item.warmth });
      expect(item.image).toBe(`/imagetools/emotion/${item.arousal}_${item.warmth}.svg`);
    }
    expect(new Set(config.emotions.map(item => item.name)).size).toBe(25);
  });

  it("坐标盘行 = 激动→平静，列 = 亲近→疏离", () => {
    const grid = imageToolEmotionGrid(config.emotions);
    expect(grid[0]![0]?.name).toBe("欣喜若狂");
    expect(grid[0]![4]?.name).toBe("怒不可遏");
    expect(grid[2]![2]?.name).toBe("淡然自若");
    expect(grid[4]![0]?.name).toBe("安详满足");
  });

  const roles: ImageToolEditorRole[] = [
    { id: "b", box: { x: 400, y: 10, w: 80, h: 80 }, arousal: 1, warmth: 1, touched: true },
    { id: "a", box: { x: 10, y: 10, w: 80, h: 80 }, arousal: 0, warmth: 0, touched: false },
    { id: "m", box: { x: 200, y: 10, w: 60, h: 60 }, arousal: -2, warmth: 2, touched: true }
  ];

  it("只发 touched 角色，并剥掉界面字段", () => {
    expect(imageToolTouchedRoles(roles)).toEqual([
      { id: "b", box: { x: 400, y: 10, w: 80, h: 80 }, arousal: 1, warmth: 1 },
      { id: "m", box: { x: 200, y: 10, w: 60, h: 60 }, arousal: -2, warmth: 2 }
    ]);
  });

  it("角色从左到右编号", () => {
    expect(imageToolRoleLabels(roles)).toEqual({ a: "角色1", m: "角色2", b: "角色3" });
  });

  it("角色上限（IMG-F-22）", () => {
    expect(imageToolRoleLimitReason(5, 6)).toBeNull();
    expect(imageToolRoleLimitReason(6, 6)).toBe("最多同时调节 6 个角色");
  });
});

describe("显示坐标 ↔ 原图坐标", () => {
  // 原图 2000×1000 放进 400×400：缩放 0.2，图在中间，上下各留 100
  const fit = imageToolFit(400, 400, 2000, 1000)!;

  it("contain：取较小缩放比，留白平均分到两侧", () => {
    expect(fit).toEqual({ offsetX: 0, offsetY: 100, scale: 0.2, naturalWidth: 2000, naturalHeight: 1000 });
    expect(imageToolFit(0, 400, 2000, 1000)).toBeNull();
    expect(imageToolFit(400, 400, 0, 0)).toBeNull();
  });

  it("减居中偏移、除缩放比、夹到图内", () => {
    expect(imageToolDisplayToImage({ x: 200, y: 200 }, fit)).toEqual({ x: 1000, y: 500 });
    expect(imageToolDisplayToImage({ x: -10, y: 50 }, fit)).toEqual({ x: 0, y: 0 });
    expect(imageToolDisplayToImage({ x: 500, y: 390 }, fit)).toEqual({ x: 2000, y: 1000 });
  });

  it("拖框换算成原图像素框，方向任意；过小丢弃", () => {
    expect(imageToolDisplayBoxToImage({ x: 60, y: 160 }, { x: 20, y: 120 }, fit)).toEqual({ x: 100, y: 100, w: 200, h: 200 });
    // 拖出图外的部分夹回图内
    expect(imageToolDisplayBoxToImage({ x: 380, y: 280 }, { x: 420, y: 360 }, fit)).toEqual({ x: 1900, y: 900, w: 100, h: 100 });
    // 显示 4px = 原图 20px < 24 → 丢弃
    expect(imageToolDisplayBoxToImage({ x: 10, y: 110 }, { x: 14, y: 200 }, fit)).toBeNull();
  });

  it("原图框 → 显示矩形（画角色框）", () => {
    expect(imageToolImageBoxToDisplay({ x: 100, y: 100, w: 200, h: 200 }, fit)).toEqual({ left: 20, top: 120, width: 40, height: 40 });
  });
});

describe("禁用理由排序（权限 > 不可用 > 输入 > 能力校验）", () => {
  const permission = imageToolPermissionBlocker(false, true);
  const unavailable = imageToolUnavailableBlocker({ available: false, blockedReason: "该模型未绑定此操作", userFixable: false });
  const quote = { kind: "quote" as const, text: "x" };

  it("给最根本的那一条", () => {
    expect(imageToolDisabledReason([quote, unavailable, permission])?.kind).toBe("permission");
    expect(imageToolDisabledReason([quote, unavailable, null])?.kind).toBe("unavailable");
    expect(imageToolDisabledReason([quote, { kind: "input", text: "先选一个角色的情绪" }])?.kind).toBe("input");
    expect(imageToolDisabledReason([null, undefined, false])).toBeNull();
    expect(imageToolSortBlockers([quote, permission]).map(item => item.kind)).toEqual(["permission", "quote"]);
  });

  it("文案：只读说权限、运营问题标明需要运营", () => {
    expect(permission?.text).toBe("需要生成权限，你在该工作区是只读成员");
    expect(imageToolPermissionBlocker(true, false)).toBeNull();
    expect(unavailable).toEqual({ kind: "unavailable", text: "该模型未绑定此操作（需要运营配置）", userFixable: false });
    expect(imageToolUnavailableBlocker({ available: false, blockedReason: "需要先连一张图", userFixable: true })?.userFixable).toBe(true);
    expect(imageToolUnavailableBlocker({ available: true, userFixable: true })).toBeNull();
  });
});

describe("子节点位置（IMG-UX-06）", () => {
  it("右侧 +420，同一源节点第 n 个向下错开 n × (高 + 40)", () => {
    expect(imageToolSpawnPosition({ x: 100, y: 50 }, 0, 300)).toEqual({ x: 520, y: 50 });
    expect(imageToolSpawnPosition({ x: 100, y: 50 }, 2, 300)).toEqual({ x: 520, y: 730 });
  });
});

describe("受理请求组装（不拼提示词）", () => {
  const inputs = [{ sourceNodeId: "src", kind: "image", order: 1, generationId: "g1" }];

  it("params.tool + toolOptions；operation 一律 img2img；字段名 modelCode", () => {
    const request = imageToolRequest({
      projectId: "p1", nodeId: "n2", tool: "multi_angle", modelCode: "fal_qwen_multi_angle",
      options: { preset: "back", horizontal: 180, vertical: 0, zoom: 5, prompt: "  " }, inputs
    });
    expect(request).toEqual({
      projectId: "p1", nodeId: "n2", operation: "img2img", modelCode: "fal_qwen_multi_angle",
      params: { model: "fal_qwen_multi_angle", tool: "multi_angle", toolOptions: { preset: "back", horizontal: 180, vertical: 0, zoom: 5 } },
      inputs, count: 1
    });
    expect(JSON.stringify(request)).not.toMatch(/additional_prompt|horizontal_angle/);
  });

  it("摘掉服务端写的键与九宫格配方；高清不带描述、只发 level", () => {
    const request = imageToolRequest({
      projectId: "p1", nodeId: "n2", tool: "upscale", modelCode: "img_a", inputs,
      options: { level: "ultra" },
      params: { upstream: { x: 1 }, output: {}, preset: "nine", presetOptions: {}, prompt: "旧描述", toolSourceMediaId: "m1" }
    });
    expect(request.operation).toBe("img2img");
    expect(request.params).toEqual({ toolSourceMediaId: "m1", model: "img_a", tool: "upscale", toolOptions: { level: "ultra" } });
  });

  it("高清：老节点上 Topaz 那一版的选项被规范成 level，未知值回「标准」", () => {
    expect(imageToolWireOptions("upscale", { provider: "topaz", model: "standard", factor: 4 } as never)).toEqual({ level: "standard" });
    expect(imageToolWireOptions("upscale", { level: "fine" })).toEqual({ level: "fine" });
  });

  it("打光智能模式只发 smart；情绪只发 touched 角色，张数随选项", () => {
    expect(imageToolWireOptions("relight", { smart: true, azimuth: 90, brightness: 80, rim: true })).toEqual({ smart: true });
    expect(imageToolWireOptions("relight", { smart: false, azimuth: 90, color: "" })).toEqual({ smart: false, azimuth: 90, color: null });
    const options = {
      sourceMediaId: "m1", resolution: "1k", count: 3,
      roles: [
        { id: "a", box: { x: 0, y: 0, w: 30, h: 30 }, arousal: 1, warmth: 0, touched: true },
        { id: "b", box: { x: 50, y: 0, w: 30, h: 30 }, arousal: 0, warmth: 0, touched: false }
      ]
    };
    const request = imageToolRequest({ projectId: "p1", nodeId: "n2", tool: "expression_edit", modelCode: "fal_qwen_edit", options: options as never, inputs });
    // 老节点上残留的 resolution 不再发出去：输出就是原图分辨率
    expect(request.params.toolOptions).toEqual({ sourceMediaId: "m1", count: 3, roles: [{ id: "a", box: { x: 0, y: 0, w: 30, h: 30 }, arousal: 1, warmth: 0 }] });
    expect(request.count).toBe(3);
    expect(imageToolCount("multi_angle", { horizontal: 0 })).toBe(1);
    expect(imageToolCount("expression_edit", { sourceMediaId: "m", count: 9, roles: [] })).toBe(4);
  });
});

/**
 * ⚠️ 四个编辑器只能经 defineAsyncComponent 引入（IMG-N-06、IMGD-21）：画布页首屏包体只剩约 5 KB 余量，
 *    任何一处静态 import 都会把编辑器（连同它的 SVG 球、坐标盘）打进画布页的首屏 chunk。
 */
describe("编辑器只异步引入（源码扫描）", () => {
  const root = join(__dirname, "..");
  const EDITORS = ["EmotionEditor", "MultiAngleEditor", "RelightEditor", "UpscalePanel"];
  const SKIP = new Set(["node_modules", ".nuxt", ".output", "dist", ".backup", "public"]);

  function sources(dir: string): string[] {
    return readdirSync(dir).flatMap((name) => {
      if (SKIP.has(name)) return [];
      const path = join(dir, name);
      if (statSync(path).isDirectory()) return sources(path);
      return /\.(vue|ts|tsx|js|mjs)$/.test(name) && !/\.test\.ts$/.test(name) ? [path] : [];
    });
  }

  it("没有任何文件静态 import 这四个组件；BaseNode 用 defineAsyncComponent 引入", () => {
    const offenders: string[] = [];
    for (const file of sources(root)) {
      const text = readFileSync(file, "utf8");
      for (const editor of EDITORS) {
        const statics = new RegExp(`import\\s+[\\w{}\\s,*]+\\s+from\\s+["'][^"']*/${editor}\\.vue["']`);
        if (statics.test(text)) offenders.push(`${relative(root, file)} → ${editor}`);
      }
    }
    expect(offenders).toEqual([]);
    const baseNode = readFileSync(join(root, "components/canvas/BaseNode.vue"), "utf8");
    for (const editor of EDITORS) {
      expect(baseNode).toMatch(new RegExp(`defineAsyncComponent\\(\\(\\) => import\\("\\./imagetools/${editor}\\.vue"\\)\\)`));
    }
  });
});
