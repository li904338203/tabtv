import { readFileSync } from "node:fs";
import { join } from "node:path";

import { describe, expect, it } from "vitest";
import { applyDefaultByUpstream, generationQuantities, nodeParamLayout, paramKey, positiveIntParam, upstreamDefaultOf } from "./paramLayout";

/** 新后端 `/canvas/config` 下发的图片/视频/音频参数（seed/param.go 的绑定，字段原样）。 */
const IMAGE = [
  { code: "prompt", group: "main", sort: 1 },
  { code: "preset", group: "main", sort: 2, config: { domainFrom: "imagePresets" } },
  { code: "reference", group: "main", sort: 3 },
  { code: "model", group: "main", sort: 4, config: { domainFrom: "models" } },
  { code: "quality", group: "format", sort: 5 },
  { code: "aspect_ratio", group: "format", sort: 6 },
  { code: "count", group: "format", sort: 7 },
  { code: "style_ref", group: "format", sort: 8, config: { domainFrom: "materials", materialType: "style" } },
  { code: "negative_prompt", group: "advanced", sort: 9 },
  { code: "seed", group: "advanced", sort: 10 }
];
const VIDEO = [
  { code: "video_mode", group: "main", sort: 1, config: { domainFrom: "videoModes" } },
  { code: "prompt", group: "main", sort: 2 },
  { code: "model", group: "main", sort: 3, config: { domainFrom: "models" } },
  { code: "duration", group: "format", sort: 4 },
  { code: "resolution", group: "format", sort: 5 },
  { code: "aspect_ratio", group: "format", sort: 6 },
  { code: "effect_ref", group: "format", sort: 7, config: { domainFrom: "materials", materialType: "effect" } },
  { code: "seed", group: "advanced", sort: 8 }
];
const AUDIO = [
  { code: "prompt", group: "main", sort: 1 },
  { code: "voice", group: "main", sort: 2 },
  { code: "model", group: "main", sort: 3 },
  { code: "speed", group: "format", sort: 4 }
];
const codes = (items: Array<{ code?: string }>) => items.map(item => item.code);

describe("节点参数版式（照老系统）", () => {
  it("图片：顶部「参考」；底部「模型 → 尺寸弹窗 → 图片预设」；画质/比例/数量收进弹窗", () => {
    const layout = nodeParamLayout("image", IMAGE);
    expect(codes(layout.top)).toEqual(["reference"]);
    // 后端 sort 里预设排在模型前面，底部栏的次序却是老系统定稿的「模型在前」
    expect(codes(layout.bottom)).toEqual(["model", "preset"]);
    expect(codes(layout.size)).toEqual(["quality", "aspect_ratio", "count"]);
    // 尺寸弹窗插在图片预设之前
    expect(layout.sizeBefore).toBe("preset");
    expect(codes(layout.advanced)).toEqual(["negative_prompt", "seed"]);
  });

  it("视频：底部「模型 → 生成模式 → 尺寸弹窗」，弹窗放在末尾", () => {
    const layout = nodeParamLayout("video", VIDEO);
    expect(codes(layout.bottom)).toEqual(["model", "video_mode"]);
    expect(codes(layout.size)).toEqual(["duration", "resolution", "aspect_ratio"]);
    expect(layout.sizeBefore).toBe("");
    expect(codes(layout.advanced)).toEqual(["seed"]);
  });

  it("后端没有读取方的 style_ref / effect_ref 哪儿都不出现（选了等于没选）", () => {
    for (const layout of [nodeParamLayout("image", IMAGE), nodeParamLayout("video", VIDEO)]) {
      const all = [...layout.top, ...layout.bottom, ...layout.size, ...layout.advanced].map(item => item.code);
      expect(all).not.toContain("style_ref");
      expect(all).not.toContain("effect_ref");
    }
  });

  it("提示词由面板的文本框单独渲染，不进任何一栏", () => {
    const layout = nodeParamLayout("image", IMAGE);
    expect([...layout.top, ...layout.bottom, ...layout.size].map(item => item.code)).not.toContain("prompt");
  });

  it("音频没有尺寸弹窗：语速仍在底部栏", () => {
    const layout = nodeParamLayout("audio", AUDIO);
    expect(codes(layout.bottom)).toEqual(["model", "voice", "speed"]);
    expect(layout.size).toEqual([]);
    expect(layout.sizeBefore).toBe("");
  });

  it("组子项排掉数量：一个子项就是一张图", () => {
    expect(codes(nodeParamLayout("image", IMAGE, { exclude: ["count"] }).size)).toEqual(["quality", "aspect_ratio"]);
  });

  it("配置可以比前端新：认不出的规格项照样进弹窗；没有 group 时按 config.advanced 兜底", () => {
    const layout = nodeParamLayout("image", [
      ...IMAGE,
      { code: "hdr", group: "format", sort: 11 },
      { code: "legacy_flag", sort: 12, config: { advanced: true } },
      { code: "legacy_main", sort: 13 }
    ]);
    expect(codes(layout.size)).toContain("hdr");
    expect(codes(layout.advanced)).toContain("legacy_flag");
    expect(codes(layout.bottom)).toContain("legacy_main");
  });

  it("视频模式的画布键是 mode（前端十来处读的都是它），其余参数键即代号", () => {
    expect(paramKey("video_mode")).toBe("mode");
    expect(paramKey("mode")).toBe("mode");
    expect(paramKey("aspect_ratio")).toBe("aspect_ratio");
    expect(paramKey(undefined)).toBe("");
  });
});

describe("受理请求的数量与时长", () => {
  it("选项值是字符串也要认出来 —— 否则按 1 张扣费、上游却出 4 张", () => {
    expect(positiveIntParam("4")).toBe(4);
    expect(positiveIntParam(" 10 ")).toBe(10);
    expect(positiveIntParam(2)).toBe(2);
    expect(positiveIntParam(2.7)).toBe(2);
    for (const bad of ["0", "-1", "abc", "", null, undefined, {}, Number.NaN]) {
      expect(positiveIntParam(bad)).toBeUndefined();
    }
  });

  it("数量与时长和 params 里发给上游的是同一个数；时长只对视频有意义", () => {
    expect(generationQuantities("image", { count: "4", aspect_ratio: "16:9" })).toEqual({ count: 4 });
    expect(generationQuantities("video", { duration: "10" })).toEqual({ count: 1, durationSec: 10 });
    // 视频没选时长：不带 durationSec，由服务端按默认计价（不猜一个数）
    expect(generationQuantities("video", {})).toEqual({ count: 1 });
    expect(generationQuantities("image", { duration: "10" })).toEqual({ count: 1 });
  });
});

/**
 * ⚠️ 上面的判定只有**被用上**才有意义：画布节点与组子项各写一份版式、两个选择器各判一次
 *    「哪些是玩法」的话，漏掉的那一份不会有任何编译错误 —— 表现只是某处又平铺了、
 *    或者九宫格下拉里冒出九个加工工具。
 */
describe("版式与预设判据的调用方", () => {
  const read = (path: string) => readFileSync(join(__dirname, "..", path), "utf8");

  it("画布节点面板与组子项面板都走 nodeParamLayout，不各写一份", () => {
    for (const file of ["components/canvas/ParamPanel.vue", "components/canvas/GroupImageNodePanel.vue"]) {
      const source = read(file);
      expect(source, file).toContain("nodeParamLayout(");
      // 尺寸弹窗只有新 API：老的 size 对象接口（:size / :count-config）在新后端下是空的
      //    （图标的 `:size="15"` 是尺寸像素，不在此列）
      expect(source, file).not.toMatch(/:count-config=|:size="(?:data\.params|draft)\.size"|@update:size=/);
    }
  });

  it("「图片预设」弹窗与工具条「九宫格」下拉用同一个判据排掉工具组", () => {
    for (const file of ["components/canvas/ImagePresetControl.vue", "components/canvas/ImageNodeToolbar.vue"]) {
      expect(read(file), file).toContain("isPickerPreset");
    }
  });

  it("视频模式控件写的是 mode，不是 video_mode", () => {
    for (const file of ["components/canvas/ParamPanel.vue", "components/canvas/GroupImageNodePanel.vue"]) {
      expect(read(file), file).not.toMatch(/updateParam\(param\.code \?\? 'video_mode'/);
      expect(read(file), file).toContain("updateParam('mode', $event)");
    }
  });
});

describe("按上游类型补默认参数（DRT-16，DRD-45）", () => {
  const imageType = {
    params: [
      { code: "model" },
      { code: "preset", config: { domainFrom: "imagePresets", defaultByUpstream: { director: "whitebox_to_real" } } }
    ]
  };
  const videoType = { params: [{ code: "video_mode", config: { defaultByUpstream: { director: "ref2video" } } }] };
  const presets = [{
    code: "whitebox_to_real",
    params: {},
    dimensions: [{ code: "texture", default: "realistic", options: [{ value: "realistic" }, { value: "anime" }] }]
  }];

  it("applyDefaultByUpstream：video_mode 写到 mode", () => {
    // ⚠️ 写到 video_mode 的话模式下拉看着变了，操作仍按 mode 算
    expect(applyDefaultByUpstream({ mode: "text2video" }, videoType, "director")).toEqual({ mode: "ref2video" });
  });

  it("applyDefaultByUpstream：图片预设整套落下（预设码 + 维度默认档），与在选择器里选中一样", () => {
    expect(applyDefaultByUpstream({ aspect_ratio: "16:9" }, imageType, "director", presets)).toEqual({
      aspect_ratio: "16:9",
      preset: "whitebox_to_real",
      presetOptions: { texture: "realistic" }
    });
  });

  it("upstreamDefaultOf：按参数代号（video_mode 也认 mode）取配置的默认值，没配时为空串", () => {
    expect(upstreamDefaultOf(imageType.params, "preset", "director")).toBe("whitebox_to_real");
    expect(upstreamDefaultOf(videoType.params, "video_mode", "director")).toBe("ref2video");
    expect(upstreamDefaultOf(videoType.params, "mode", "director")).toBe("ref2video");
    expect(upstreamDefaultOf(videoType.params, "video_mode", "image")).toBe("");
    expect(upstreamDefaultOf(undefined, "preset", "director")).toBe("");
  });

  it("applyDefaultByUpstream：没有配置时不改参数", () => {
    const params = { mode: "img2video" };
    // 上游不是导演台、目标类型没有这条配置、或者配置的预设已下线（目录里查不到）—— 都原样返回
    expect(applyDefaultByUpstream(params, videoType, "image")).toBe(params);
    expect(applyDefaultByUpstream(params, { params: [{ code: "video_mode" }] }, "director")).toBe(params);
    expect(applyDefaultByUpstream(params, undefined, "director")).toBe(params);
    expect(applyDefaultByUpstream(params, imageType, "director", [])).toBe(params);
    expect(applyDefaultByUpstream(params, { params: [{ code: "x", config: { defaultByUpstream: { director: "" } } }] }, "director")).toBe(params);
  });
});
