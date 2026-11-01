import { readFileSync } from "node:fs";
import { join } from "node:path";

import { describe, expect, it } from "vitest";
import { reactive } from "vue";
import type { CanvasEdge, CanvasNode } from "~/types/canvas";
import type { ScriptCanvasReference } from "./canvas";
import {
  CONTROL_TYPES,
  editedGenerationText,
  acceptInputsOf,
  canvasMediaRole,
  connectionRejection,
  applyImagePreset,
  clearImagePreset,
  cloneJson,
  connectedNodePosition,
  connectedVideoMode,
  estimateRejectionText,
  connectionTargetOptions,
  generationInputReady,
  generationInputs,
  imagePreviewHeight,
  isPickerPreset,
  isVideoModeAvailable,
  liftLegacySize,
  normalizeLegacyParams,
  outputMediaIdOf,
  presetDimensionsOf,
  mergeMaterialReferences,
  mergeObjects,
  nextInputOrder,
  newNodePosition,
  presetNodeParams,
  presetCodeOf,
  presetGroupOf,
  presetOptionDefaults,
  presetsInMenuOrder,
  shouldSpawnPresetNode,
  normalizeEdgeOrders,
  operationForNode,
  orderedTargetEdges,
  reconcileParams,
  modelReconcileRules,
  uploadedReferenceOf,
  canvasRoleCharacters,
  videoPreviewFrame,
  aspectFromRatio,
  formatVideoClock,
  generationStatusLabel,
  generatingLabel,
  videoSeekTime,
  frameCaptureNodePosition,
  referenceResource,
  roleCountsOf,
  unwrapOptionObjects,
  upstreamNotReadyText,
  referenceResources,
  resetScriptEntryParams,
  scriptCanOpen,
  scriptCanGenerateFromPanel,
  scriptEntryMode,
  scriptEntryResetPlan,
  scriptHasStoryboard,
  scriptTranslatedInput,
  scriptReferencesForEntryMode,
  videoModeForInputs,
  videoModeOptions,
  scriptCanvasReferences,
  scriptRequestReferences,
  insertTextAt,
  upstreamShotTextOf
} from "./canvas";

const node = (nodeType: string, params: Record<string, unknown> = {}): CanvasNode => ({
  id: "target",
  type: "aitv",
  position: { x: 0, y: 0 },
  data: { nodeType, title: "测试节点", params }
});

const edge = (id: string, order: number, operation?: string): CanvasEdge => ({
  id,
  source: `source-${id}`,
  target: "target",
  data: { order, operation }
});

describe("canvas configuration helpers", () => {
  it("fits generated images to their natural ratio without overflowing the node", () => {
    expect(imagePreviewHeight(330, 1920, 1080)).toBe(186);
    expect(imagePreviewHeight(330, 1024, 1024)).toBe(330);
    expect(imagePreviewHeight(330, 900, 1600)).toBe(520);
    expect(imagePreviewHeight(0, 1024, 1024)).toBe(180);
  });

  it("选预设写的是平铺的 aspect_ratio / quality，不再是老的 size 对象", () => {
    // ⚠️ 新后端与上游协议读的是平铺键。写进 size 对象的话，预设的画幅**一个字都发不出去**，
    //    出图照旧按默认画幅（而界面上看起来预设生效了）。
    expect(applyImagePreset(
      { prompt: "portrait", size: { ratio: "1:1", quality: "low" } },
      { id: "1001", params: { presetCode: "cinematic", ratio: "16:9", clarity: "2K", style: "cinematic" } }
    )).toEqual({
      prompt: "portrait",
      preset: "cinematic",
      // 老预设的 ratio 换成 aspect_ratio；老 size 里的画质拆平后保留；clarity（摆设）丢掉
      aspect_ratio: "16:9",
      quality: "low",
      style: "cinematic"
    });
    // 新后端的预设：画幅照老系统**以预设为准**（25 宫格画在 1:1 上挤不下）
    expect(applyImagePreset(
      { prompt: "街景", aspect_ratio: "1:1", quality: "high" },
      { code: "storyboard_25grid", presetGroup: "storyboard", params: { aspect_ratio: "16:9", quality: "standard" } }
    )).toEqual({ prompt: "街景", preset: "storyboard_25grid", aspect_ratio: "16:9", quality: "standard" });
  });

  it("预设码认新后端的顶层 code，也认老后端的 params.presetCode", () => {
    // ⚠️ 新后端把预设码升到了顶层 `code`（老后端藏在 `params.presetCode`）。
    //    只读老位置的话 `preset` 恒为空 —— 服务端按"没选预设"处理，
    //    配方词一句都不拼，出来的图只是画幅变了（GAP-01 的老毛病）。
    expect(applyImagePreset({}, {
      code: "character_sheet",
      presetGroup: "sheet",
      params: { aspect_ratio: "16:9" }
    }).preset).toBe("character_sheet");
    expect(presetCodeOf({ code: "character_sheet" })).toBe("character_sheet");
    expect(presetCodeOf({ id: "1001", params: { presetCode: "cinematic" } })).toBe("cinematic");
    // 分组字段同样两边不同：读错的话所有预设都落进"其他"那一栏。
    expect(presetGroupOf({ presetGroup: "sheet" })).toBe("sheet");
    expect(presetGroupOf({ group: "tuning" })).toBe("tuning");
    expect(presetGroupOf({})).toBe("");
  });

  it("seeds dimension defaults and drops them when switching presets", () => {
    const tuning = applyImagePreset({ prompt: "人像" }, {
      id: "1106",
      params: { presetCode: "portrait_tuning", ratio: "adaptive" },
      dimensions: [
        { key: "skin", default: "natural", options: [{ value: "clear" }, { value: "natural" }] },
        { key: "sharpness", default: "standard", options: [{ value: "soft" }, { value: "standard" }] }
      ]
    });
    expect(tuning.presetOptions).toEqual({ skin: "natural", sharpness: "standard" });

    // 换成单选型预设必须把维度摘掉：旧档位键在新预设里不存在，留着会一直触发未知档位告警
    const sheet = applyImagePreset(tuning, {
      id: "1111",
      params: { presetCode: "character_sheet", ratio: "16:9" }
    });
    expect(sheet.presetOptions).toBeUndefined();
    expect(sheet.preset).toBe("character_sheet");
  });

  it("预设维度认新后端的 code/name，也认老后端的 key/label", () => {
    // ⚠️ 只认老名字的话新后端的维度键恒为空：调节面板里每一档都点不亮，
    //    presetOptions 永远发不出去，用户调了「强度」而画面没有任何变化。
    const dimensions = [
      { code: "fusion", name: "人景融合", default: "natural",
        options: [{ value: "light", label: "轻度对齐", prompt: "人物与…" }, { value: "natural", label: "自然融合" }] },
      { key: "skin", label: "肤质", options: [{ value: "clear", label: "通透" }] },
      { name: "没有代号的维度会被丢掉", options: [{ value: "x" }] }
    ];
    expect(presetDimensionsOf(dimensions)).toEqual([
      // 选项里的 prompt 不带出来：拼词在服务端
      { code: "fusion", name: "人景融合", default: "natural",
        options: [{ value: "light", label: "轻度对齐" }, { value: "natural", label: "自然融合" }] },
      { code: "skin", name: "肤质", default: undefined, options: [{ value: "clear", label: "通透" }] }
    ]);
    expect(presetOptionDefaults(dimensions)).toEqual({ fusion: "natural", skin: "clear" });
    expect(presetDimensionsOf("not-an-array")).toEqual([]);
  });

  it("工具组（tool）不是玩法，两个选择器都不列", () => {
    expect(isPickerPreset({ code: "storyboard_4grid", presetGroup: "storyboard" })).toBe(true);
    expect(isPickerPreset({ code: "inpaint", presetGroup: "tool" })).toBe(false);
    // 老后端的预设没有 tool 这一组，照常列出
    expect(isPickerPreset({ id: "1001", group: "sheet" })).toBe(true);
  });

  it("九宫格平铺下拉照老系统全局按 sort 排，不按组名的字母序", () => {
    const presets = [
      { code: "character_face_sheet", presetGroup: "sheet", sort: 110 },
      { code: "panorama_720", presetGroup: "space", sort: 90 },
      { code: "blocking_storyboard", presetGroup: "storyboard", sort: 10 },
      { code: "portrait_tuning", presetGroup: "tuning", sort: 70 },
      { code: "no_sort_goes_last", presetGroup: "sheet" }
    ];
    expect(presetsInMenuOrder(presets).map(presetCodeOf)).toEqual([
      "blocking_storyboard", "portrait_tuning", "panorama_720", "character_face_sheet", "no_sort_goes_last"
    ]);
    // 不改动入参（它是 store 里的响应式数组）
    expect(presets[0]!.code).toBe("character_face_sheet");
  });

  it("老前端的 size 对象拆平，已有的平铺值不被覆盖", () => {
    expect(liftLegacySize({ prompt: "p", size: { ratio: "4:3", quality: "high", clarity: "4K" } }))
      .toEqual({ prompt: "p", aspect_ratio: "4:3", quality: "high" });
    // 平铺值是用户在新面板上选的，优先于老对象
    expect(liftLegacySize({ aspect_ratio: "9:16", size: { ratio: "4:3" } })).toEqual({ aspect_ratio: "9:16" });
    // 顶层的老 ratio / clarity 同样归一
    expect(liftLegacySize({ ratio: "21:9", clarity: "2K" })).toEqual({ aspect_ratio: "21:9" });
    // 不是对象的 size 不是老「尺寸」，原样保留
    expect(liftLegacySize({ size: "1024x1024" })).toEqual({ size: "1024x1024" });
  });

  it("打开节点时的归一：没有要改的就返回原对象；误写的 video_mode 只清不采纳", () => {
    const clean = { prompt: "p", aspect_ratio: "16:9", mode: "img2video" };
    // 返回同一个对象 —— 调用方据此判断要不要写回（写回会触发一次保存）
    expect(normalizeLegacyParams(clean)).toBe(clean);
    // video_mode 记的是默认值不是用户的选择：清掉，mode 不动
    expect(normalizeLegacyParams({ mode: "img2video", video_mode: "text2video" })).toEqual({ mode: "img2video" });
    expect(normalizeLegacyParams({ size: { ratio: "4:3" } })).toEqual({ aspect_ratio: "4:3" });
  });

  it("被整个存下的选项对象还原成值；正经的对象与数组一个不碰", () => {
    const presetOptions = { skin: "natural", fusion: "deep" };
    const reference = [{ id: "1", type: "image", name: "a.png" }];
    expect(unwrapOptionObjects({
      duration: { label: "10 秒", value: "10" },
      count: { value: 4, label: "4 张" },
      presetOptions,
      reference,
      // 没有 value 的、或者多出别的键的，不是选项对象
      camera: { label: "近景" },
      other: { value: "1", extra: "y" },
      prompt: "猴王"
    })).toEqual({
      duration: "10",
      count: "4",
      presetOptions,
      reference,
      camera: { label: "近景" },
      other: { value: "1", extra: "y" },
      prompt: "猴王"
    });
    // 打开节点时的归一也做这一步（库里 3 个视频节点的时长就是这个形状）
    expect(normalizeLegacyParams({ mode: "text2video", duration: { label: "10 秒", value: "10" } }))
      .toEqual({ mode: "text2video", duration: "10" });
  });

  it("falls back to the first option when a dimension has no default", () => {
    expect(presetOptionDefaults([
      { key: "fusion", options: [{ value: "light" }, { value: "deep" }] }
    ])).toEqual({ fusion: "light" });
    expect(presetOptionDefaults(undefined)).toEqual({});
  });

  it("clears preset and dimensions but keeps the chosen size", () => {
    expect(clearImagePreset({
      prompt: "街景",
      preset: "multicam_9grid",
      presetOptions: { skin: "natural" },
      size: { ratio: "16:9", clarity: "4K" }
    })).toEqual({ prompt: "街景", size: { ratio: "16:9", clarity: "4K" } });
  });

  it("spawns a downstream node only when the image node already produced an image", () => {
    const node = (generation?: Record<string, unknown>, nodeType = "image") => ({
      id: "n1",
      position: { x: 0, y: 0 },
      data: { nodeType, title: "图片", params: {}, generation }
    }) as never;

    expect(shouldSpawnPresetNode(node({ status: "success", previewUrl: "blob:x" }))).toBe(true);
    // 还没出图就地应用，不派生：此时没有源图可供加工
    expect(shouldSpawnPresetNode(node(undefined))).toBe(false);
    expect(shouldSpawnPresetNode(node({ status: "running", previewUrl: "blob:x" }))).toBe(false);
    // success 但没产出地址说明结果不可用，同样不该拿它当输入
    expect(shouldSpawnPresetNode(node({ status: "success" }))).toBe(false);
    expect(shouldSpawnPresetNode(node({ status: "success", previewUrl: "blob:x" }, "video"))).toBe(false);
  });

  it("carries the model into the spawned node but never the old prompt", () => {
    const params = presetNodeParams(
      { model: "gpt_image_2_direct", prompt: "唐代女官", size: { ratio: "1:1" } },
      { id: "1102", params: { presetCode: "storyboard_25grid", ratio: "16:9", clarity: "4K" } }
    );

    expect(params.model).toBe("gpt_image_2_direct");
    // 原图的描述换到新玩法多半不成立："唐代女官"配九宫格会变成九个女官
    expect(params.prompt).toBeUndefined();
    expect(params.preset).toBe("storyboard_25grid");
    expect(params.aspect_ratio).toBe("16:9");
    expect(params.size).toBeUndefined();
    expect(params.clarity).toBeUndefined();
  });

  it("merges nested model overrides without losing defaults", () => {
    expect(mergeObjects(
      { size: { clarity: "2K", ratio: "16:9" } },
      { size: { clarity: "1K" } }
    )).toEqual({ size: { clarity: "1K", ratio: "16:9" } });
  });

  it("corrects values no longer supported by a selected model", () => {
    expect(reconcileParams(
      { size: { clarity: "4K", ratio: "16:9" }, count: 4 },
      {
        size: { clarity: { options: ["1K", "2K"], default: "1K" } },
        count: { options: [1, 2], default: 1 }
      }
    )).toEqual({ size: { clarity: "1K", ratio: "16:9" }, count: 1 });
  });

  it("切模型后参数之间的约束也要成立：1080p 下 21:9 改回默认画幅", () => {
    const overrides = {
      resolution: { options: [{ label: "720P", value: "720p" }, { label: "1080P", value: "1080p" }], default: "720p" },
      aspect_ratio: {
        options: ["16:9", "9:16", "1:1", "4:3", "3:4", "21:9"].map(value => ({ label: value, value })),
        default: "16:9",
        restrictBy: { resolution: { "1080p": ["16:9", "9:16", "1:1"] } }
      }
    };
    expect(reconcileParams({ resolution: "1080p", aspect_ratio: "21:9" }, overrides))
      .toEqual({ resolution: "1080p", aspect_ratio: "16:9" });
    expect(reconcileParams({ resolution: "720p", aspect_ratio: "21:9" }, overrides))
      .toEqual({ resolution: "720p", aspect_ratio: "21:9" });
  });

  it("只带约束、没带选项的覆盖不会把字符串参数改写成对象", () => {
    expect(reconcileParams(
      { resolution: "1080p", aspect_ratio: "16:9" },
      { aspect_ratio: { restrictBy: { resolution: { "1080p": ["16:9"] } } } }
    )).toEqual({ resolution: "1080p", aspect_ratio: "16:9" });
  });

  it("reconciles reactive canvas params as JSON data", () => {
    const params = reactive({ size: { clarity: "4K", ratio: "16:9" } });
    expect(reconcileParams(params, {
      size: { clarity: { options: ["1K", "2K"], default: "1K" } }
    })).toEqual({ size: { clarity: "1K", ratio: "16:9" } });
  });

  it("clones reactive node snapshots as detached JSON data", () => {
    const snapshot = reactive([{ id: "node-1", data: { prompt: "原始提示词" } }]);
    const copy = cloneJson(snapshot);
    snapshot[0]!.data.prompt = "已修改";
    expect(copy[0]?.data.prompt).toBe("原始提示词");
  });

  it("认得全部 11 种控件，未知控件一律跳过", () => {
    expect(CONTROL_TYPES.size).toBe(11);
    expect(CONTROL_TYPES.has("select")).toBe(true);
    // ⚠️ `text` 是新后端才有的控件（negative_prompt 用它）。
    //    它不在表里的时候，DynamicControl 会静默跳过 —— 那个参数就彻底没有入口了。
    expect(CONTROL_TYPES.has("text")).toBe(true);
    expect(CONTROL_TYPES.has("future_control")).toBe(false);
  });

  it("normalizes persisted reference resources to a protected same-origin URL", () => {
    expect(referenceResource({
      id: "88",
      type: "image",
      contentType: "image/png",
      name: "scene.png",
      size: 1024,
      url: "https://evil.example/steal-token"
    })).toEqual({
      id: "88",
      type: "image",
      contentType: "image/png",
      name: "scene.png",
      size: 1024,
      url: "/media/88"
    });
    expect(referenceResource({ id: "not-an-id", type: "image" })).toBeUndefined();
  });

  /** 参考放开到多张后存的是数组，但老画布的 canvas_data 不迁移，单个对象必须继续认。 */
  it("reads uploaded references from both the legacy single object and the array form", () => {
    const one = { id: "88", type: "image", contentType: "image/png", name: "a.png", size: 8 };
    const two = { id: "89", type: "image", contentType: "image/png", name: "b.png", size: 9 };

    expect(referenceResources(one).map(item => item.id)).toEqual(["88"]);
    expect(referenceResources([one, two]).map(item => item.id)).toEqual(["88", "89"]);
    expect(referenceResources(undefined)).toEqual([]);
  });

  /** 重复 id 会在生成输入里占两个角标，上游也会白收两张一样的图。 */
  it("从画布 @ 引用来的参考保留来源节点（DRT-20），上传的没有这个字段", () => {
    expect(referenceResource({ id: "7101", type: "image", name: "全景素材", fromNodeId: "img" })?.fromNodeId).toBe("img");
    expect(referenceResource({ id: "7101", type: "image" })).not.toHaveProperty("fromNodeId");
    expect(referenceResource({ id: "7101", type: "image", fromNodeId: "" })).not.toHaveProperty("fromNodeId");
  });

  it("drops duplicate and invalid references", () => {
    const one = { id: "88", type: "image", contentType: "image/png", name: "a.png", size: 8 };

    expect(referenceResources([one, one]).map(item => item.id)).toEqual(["88"]);
    expect(referenceResources([one, { id: "nope", type: "image" }]).map(item => item.id)).toEqual(["88"]);
  });
});

describe("canvas connection helpers", () => {
  it("derives reference-menu availability from configured connection rules", () => {
    const options = connectionTargetOptions("image", [
      { code: "text", kind: "generator" },
      { code: "image", kind: "generator" },
      { code: "video", kind: "generator" },
      { code: "audio", kind: "generator" },
      { code: "style", kind: "material" }
    ], [
      { sourceType: "image", targetType: "text", isAllowed: true, defaultOperation: "img2prompt" },
      { sourceType: "image", targetType: "image", isAllowed: true, defaultOperation: "img2img" },
      { sourceType: "image", targetType: "video", isAllowed: true, defaultOperation: "img2video" },
      { sourceType: "image", targetType: "audio", isAllowed: false, defaultOperation: "text2audio" }
    ]);

    expect(options.map(option => [option.nodeType.code, option.allowed, option.operation])).toEqual([
      ["text", true, "img2prompt"],
      ["image", true, "img2img"],
      ["video", true, "img2video"],
      ["audio", false, undefined]
    ]);
  });

  it("prepares a compatible video mode and places a new node past the drop point", () => {
    expect(connectedVideoMode("video", "img2video", "image")).toBe("img2video");
    expect(connectedVideoMode("video", undefined, "audio")).toBe("ref2video");
    expect(connectedVideoMode("image", "img2img", "image")).toBeUndefined();
    expect(connectedNodePosition({ x: 300.4, y: 220.4 })).toEqual({ x: 372, y: 110 });
  });

  it("sorts multi-input edges and chooses the next order", () => {
    const edges = [edge("two", 2), edge("one", 1)];
    expect(orderedTargetEdges(edges, "target").map(item => item.data?.order)).toEqual([1, 2]);
    expect(nextInputOrder(edges, "target")).toBe(3);
  });

  it("builds trusted generation references in connection order", () => {
    const textSource = {
      ...node("text", { prompt: "不会代替生成结果" }),
      id: "source-text",
      data: {
        ...node("text").data,
        params: { prompt: "不会代替生成结果" },
        generation: { id: "101", status: "success", previewUrl: "blob:unsafe" }
      }
    };
    const imageSource = {
      ...node("image"),
      id: "source-image",
      data: {
        ...node("image").data,
        generation: { id: "102", status: "success", previewUrl: "blob:unsafe" }
      }
    };
    const edges: CanvasEdge[] = [
      { ...edge("image", 2), source: "source-image" },
      { ...edge("text", 1), source: "source-text" }
    ];

    expect(generationInputs([imageSource, textSource], edges, "target")).toEqual([
      { sourceNodeId: "source-text", kind: "text", order: 1, generationId: "101" },
      { sourceNodeId: "source-image", kind: "image", order: 2, generationId: "102" }
    ]);
  });

  it("文本节点的生成结果被用户改过：下游用改过的文字，重新生成之后改动失效", () => {
    const source = (params: Record<string, unknown>, generationId: string) => ({
      ...node("text"),
      id: "t",
      data: { ...node("text").data, params, generation: { id: generationId, status: "success", outputText: "原文" } }
    });
    const edges: CanvasEdge[] = [{ ...edge("text", 1), source: "t" }];
    // 改过（记着改的是 101 那次）→ 按文字
    expect(generationInputs([source({ text: "改过的", textEditedFor: "101" }, "101")], edges, "target"))
      .toEqual([{ sourceNodeId: "t", kind: "text", order: 1, text: "改过的" }]);
    // 重新生成成了 102 → 旧改动失效，按新生成引用
    expect(generationInputs([source({ text: "改过的", textEditedFor: "101" }, "102")], edges, "target"))
      .toEqual([{ sourceNodeId: "t", kind: "text", order: 1, generationId: "102" }]);
    expect(editedGenerationText({ text: "x", textEditedFor: "1" }, "1")).toBe("x");
    expect(editedGenerationText({ text: "x", textEditedFor: "1" }, "2")).toBeUndefined();
    expect(editedGenerationText({ text: "x" }, undefined)).toBeUndefined();
  });

  it("副本里的节点（结果被解绑成 sourceGenerationId）按媒体 ID 声明成参考，而不是永远「未就绪」", () => {
    // 复制项目时服务端把 result.id 改写成 sourceGenerationId（GAP-16），真实库里的形状就是这样：
    const copied = {
      ...node("image"),
      id: "copied",
      data: {
        ...node("image").data,
        generation: { status: "success", progress: 100, outputUrl: "/api/v1/media/96467992686100480", outputType: "image", sourceGenerationId: "96467657053700096" } as never
      }
    };
    const [input] = generationInputs([copied], [{ ...edge("image", 1), source: "copied" }], "target");
    expect(input).toEqual({ sourceNodeId: "copied", kind: "image", order: 1, referenceId: "96467992686100480" });
    // 修复前：没有 id 就什么都不带 → 未就绪 → 派生节点不报价、发送按钮灰着
    expect(generationInputReady(input!)).toBe(true);

    // 本节点自己的产出仍然走 generationId（同项目），不改成参考
    const own = { ...copied, id: "own", data: { ...copied.data, generation: { id: "5", status: "success", outputUrl: "/api/v1/media/7", mediaId: "7" } } };
    expect(generationInputs([own], [{ ...edge("image", 1), source: "own" }], "target")[0])
      .toEqual({ sourceNodeId: "own", kind: "image", order: 1, generationId: "5" });
    // 还没成功的不算就绪，哪怕带着上一次的地址
    const running = { ...copied, id: "running", data: { ...copied.data, generation: { status: "running", outputUrl: "/api/v1/media/7" } } };
    expect(generationInputReady(generationInputs([running], [{ ...edge("image", 1), source: "running" }], "target")[0]!)).toBe(false);
  });

  it("产物的媒体 ID：存了就用，没存的按契约写明的地址格式取", () => {
    expect(outputMediaIdOf({ mediaId: "42", outputUrl: "/api/v1/media/7" })).toBe("42");
    expect(outputMediaIdOf({ outputUrl: "/api/v1/media/96467992686100480" })).toBe("96467992686100480");
    expect(outputMediaIdOf({ outputUrl: "/api/v1/media/7?w=320" })).toBe("7");
    // 不是受保护媒体端点的地址不猜
    expect(outputMediaIdOf({ outputUrl: "blob:http://localhost/abc" })).toBeUndefined();
    expect(outputMediaIdOf({ outputUrl: "https://cdn.example.com/api/v1/media/7" })).toBeUndefined();
    expect(outputMediaIdOf(undefined)).toBeUndefined();
  });

  it("受理的输入换成契约的来源类型 —— 原样发 kind=image 服务端一律「不支持的输入类型」", () => {
    expect(acceptInputsOf([
      { sourceNodeId: "t", kind: "text", order: 1, text: "  补充剧情 " },
      { sourceNodeId: "g", kind: "image", order: 2, generationId: "11" },
      { sourceNodeId: "c", kind: "image", order: 3, referenceId: "22" },
      { sourceNodeId: "self", kind: "video", order: 4, referenceId: "33" },
      { sourceNodeId: "s", kind: "style", order: 5, materialCode: "ink" },
      // 没选素材的风格节点：丢掉，不发空码（空码会让整个节点受理失败）
      { sourceNodeId: "e", kind: "effect", order: 6 },
      // 上游还没产出的：不发
      { sourceNodeId: "p", kind: "image", order: 7 }
    ])).toEqual([
      { sourceNodeId: "t", kind: "text", order: 1, text: "补充剧情" },
      { sourceNodeId: "g", kind: "generation", order: 2, generationId: "11" },
      { sourceNodeId: "c", kind: "reference", order: 3, referenceId: "22" },
      { sourceNodeId: "self", kind: "reference", order: 4, referenceId: "33" },
      { sourceNodeId: "s", kind: "material", order: 5, materialCode: "ink", materialType: "style" }
    ]);
  });

  it("报价的角色计数与服务端受理时一致：素材不算，脚本给的文本算 text", () => {
    expect(roleCountsOf([
      { sourceNodeId: "a", kind: "image", order: 1, generationId: "1" },
      { sourceNodeId: "b", kind: "image", order: 2, referenceId: "2" },
      { sourceNodeId: "c", kind: "text", order: 3, text: "x" },
      { sourceNodeId: "d", kind: "script", order: 4, text: "y" },
      { sourceNodeId: "e", kind: "style", order: 5, materialCode: "ink" }
    ])).toEqual({ image: 2, text: 2 });
  });

  it("allows manual text but blocks unfinished media references", () => {
    const textSource = { ...node("text", { prompt: "  手写提示词  " }), id: "source-text" };
    const imageSource = { ...node("image"), id: "source-image" };
    const inputs = generationInputs([textSource, imageSource], [
      { ...edge("text", 1), source: "source-text" },
      { ...edge("image", 2), source: "source-image" }
    ], "target");

    expect(inputs[0]).toEqual({ sourceNodeId: "source-text", kind: "text", order: 1, text: "手写提示词" });
    expect(generationInputReady(inputs[0]!)).toBe(true);
    expect(generationInputReady(inputs[1]!)).toBe(false);
  });

  it("builds script entry snapshots from ordered text and character upstream nodes", () => {
    const textSource: CanvasNode = {
      ...node("text", { prompt: "补充剧情" }),
      id: "script-text",
      data: {
        ...node("text").data,
        title: "剧本文本",
        params: { prompt: "补充剧情" },
        generation: { id: "text-generation", status: "success", outputText: "夜晚的办公室" }
      }
    };
    const roleSource: CanvasNode = {
      ...node("image", { prompt: "穿黑色外套的女主角" }),
      id: "script-role",
      data: {
        ...node("image").data,
        title: "角色 1",
        params: { prompt: "穿黑色外套的女主角" },
        generation: { id: "role-generation", status: "success", previewUrl: "blob:role" }
      }
    };
    const edges: CanvasEdge[] = [
      { ...edge("role", 2), source: "script-role" },
      { ...edge("text", 1), source: "script-text" }
    ];

    const references = scriptCanvasReferences([textSource, roleSource], edges, "target");
    expect(references.map(reference => [reference.type, reference.order, reference.ready])).toEqual([
      ["text", 1, true],
      ["image", 2, true]
    ]);
    expect(scriptRequestReferences(references)).toEqual([
      { nodeId: "script-text", type: "text", title: "剧本文本", text: "夜晚的办公室", generationId: "text-generation" },
      { nodeId: "script-role", type: "image", title: "角色 1", text: "穿黑色外套的女主角", generationId: "role-generation" }
    ]);
  });

  it("opens AI workbenches only after storyboard shots exist, while manual drafts remain editable", () => {
    expect(scriptHasStoryboard({ scriptShotCount: 2 })).toBe(true);
    expect(scriptHasStoryboard({ scriptShotCount: "1" })).toBe(true);
    expect(scriptHasStoryboard({ scriptShotCount: 0 })).toBe(false);
    expect(scriptHasStoryboard({ scriptId: "legacy-script" })).toBe(false);
    expect(scriptCanOpen({ scriptId: "manual-script", sourceType: "manual", scriptShotCount: 0 })).toBe(true);
    expect(scriptCanOpen({ scriptId: "ai-script", sourceType: "script", scriptShotCount: 0 })).toBe(false);
    expect(scriptCanOpen({ scriptId: "ai-script", sourceType: "character", scriptShotCount: 1 })).toBe(true);
    expect(scriptCanOpen({ sourceType: "manual", scriptShotCount: 1 })).toBe(false);
  });

  it("prefixes the storyboard input only when translation is enabled", () => {
    const text = "唐僧师徒行至白虎岭";
    const prefixed = scriptTranslatedInput(text, true);

    expect(prefixed).toContain("请先将以下内容准确转换为适合影视分镜生成的中文提示词");
    expect(prefixed).toContain(text);
    // 关闭开关或空输入时原样返回，不能凭空加前缀
    expect(scriptTranslatedInput(text, false)).toBe(text);
    expect(scriptTranslatedInput("  ", true)).toBe("");
    expect(scriptTranslatedInput("  " + text + "  ", true)).toContain(text);
  });

  it("allows a direct prompt without upstream references and enforces explicit entry requirements", () => {
    expect(scriptCanGenerateFromPanel("", "gpt-5.6-sol", false, "西游记三打白骨精")).toBe(true);
    expect(scriptCanGenerateFromPanel("", "gpt-5.6-sol", false, "   ")).toBe(false);
    expect(scriptCanGenerateFromPanel("direct", "gpt-5.6-sol", true, "西游记三打白骨精")).toBe(true);
    expect(scriptCanGenerateFromPanel("text_reference", "gpt-5.6-sol", true, "")).toBe(true);
    expect(scriptCanGenerateFromPanel("character_reference", "gpt-5.6-sol", true, "双角色冒险故事")).toBe(true);
    expect(scriptCanGenerateFromPanel("character_reference", "gpt-5.6-sol", true, "")).toBe(false);
  });

  it("distinguishes direct scripts from scripts with an explicit text upstream", () => {
    expect(scriptEntryMode({}, [])).toBe("");
    expect(scriptEntryMode({ sourceType: "script" }, [])).toBe("direct");
    expect(scriptEntryMode({ sourceType: "script" }, ["text"])).toBe("text_reference");
    expect(scriptEntryMode({ sourceType: "character" }, ["image", "image"])).toBe("character_reference");
    expect(scriptEntryMode({ sourceType: "manual" }, [])).toBe("manual");
    expect(scriptEntryMode({ sourceType: "script", scriptEntryMode: "direct" }, ["text"])).toBe("direct");
  });

  it("scopes readiness checks to the entry mode's primary source", () => {
    // 这个筛选只用于"够不够、生成完没有"的校验，不决定请求内容
    const references: ScriptCanvasReference[] = [
      { edgeId: "text-edge", nodeId: "text-node", type: "text", order: 1, ready: true },
      { edgeId: "image-edge", nodeId: "image-node", type: "image", order: 2, ready: true }
    ];

    expect(scriptReferencesForEntryMode(references, "direct")).toEqual([]);
    expect(scriptReferencesForEntryMode(references, "manual")).toEqual([]);
    expect(scriptReferencesForEntryMode(references, "text_reference")).toEqual([references[0]]);
    expect(scriptReferencesForEntryMode(references, "character_reference")).toEqual([references[1]]);
  });

  it("sends every ready upstream reference regardless of entry mode", () => {
    // 曾按入口模式筛请求内容：sourceType 缺失时兜底成 manual，用户连上的文本节点
    // 就被整批丢掉 —— 线画着、界面显示已连接，模型却收不到任何上游内容，且无任何提示
    const references: ScriptCanvasReference[] = [
      { edgeId: "text-edge", nodeId: "text-node", type: "text", title: "剧本", text: "夜晚的办公室", order: 1, ready: true },
      { edgeId: "image-edge", nodeId: "image-node", type: "image", title: "角色", generationId: "g1", order: 2, ready: true },
      { edgeId: "pending-edge", nodeId: "pending-node", type: "image", title: "还没出图", order: 3, ready: false }
    ];

    const sent = scriptRequestReferences(references);

    // 已就绪的都带上，不管是文本还是图片
    expect(sent.map(reference => reference.nodeId)).toEqual(["text-node", "image-node"]);
    // 没就绪的丢掉：带上去只会让模型收到一个空引用
    expect(sent.map(reference => reference.nodeId)).not.toContain("pending-node");
  });

  it("resets a text-reference script entry when its required source is deleted", () => {
    const scriptNode: CanvasNode = {
      ...node("script", {
        sourceType: "script",
        scriptEntryMode: "text_reference",
        sourceText: "补充要求",
        scriptEntryInitialized: true,
        scriptStatus: "editing",
        keep: "value"
      }),
      id: "script-node"
    };
    const textNode: CanvasNode = {
      ...node("text", { scriptSourceNode: true, scriptNodeId: "script-node" }),
      id: "text-source"
    };
    const edges: CanvasEdge[] = [{ id: "text-script", source: "text-source", target: "script-node", data: { order: 1 } }];

    expect(scriptEntryResetPlan([scriptNode, textNode], edges, "text-source")).toEqual({
      targetIds: ["script-node"],
      sourceIds: []
    });
    expect(resetScriptEntryParams(scriptNode.data.params)).toEqual({ keep: "value" });
  });

  it("keeps the other role connected when either role source is deleted", () => {
    const scriptNode: CanvasNode = {
      ...node("script", { sourceType: "character", scriptEntryInitialized: true }),
      id: "script-node"
    };
    const roleOne: CanvasNode = {
      ...node("image", { scriptSourceNode: true, scriptNodeId: "script-node", roleIndex: 1 }),
      id: "role-one"
    };
    const roleTwo: CanvasNode = {
      ...node("image", { scriptSourceNode: true, scriptNodeId: "script-node", roleIndex: 2 }),
      id: "role-two"
    };
    const edges: CanvasEdge[] = [
      { id: "role-one-script", source: "role-one", target: "script-node", data: { order: 1 } },
      { id: "role-two-script", source: "role-two", target: "script-node", data: { order: 2 } }
    ];

    expect(scriptEntryResetPlan([scriptNode, roleOne, roleTwo], edges, "role-one")).toEqual({
      targetIds: [],
      sourceIds: []
    });
    expect(scriptEntryResetPlan([scriptNode, roleOne, roleTwo], edges, "role-two")).toEqual({
      targetIds: [],
      sourceIds: []
    });

    const remainingEdges = normalizeEdgeOrders(edges.filter(edge => edge.source !== "role-one"));
    expect(remainingEdges[0]?.data?.order).toBe(1);
    expect(scriptCanvasReferences([scriptNode, roleTwo], remainingEdges, "script-node")).toEqual([
      expect.objectContaining({ nodeId: "role-two", type: "image", order: 1 })
    ]);
    expect(nextInputOrder(remainingEdges, "script-node")).toBe(2);
  });

  it("accepts a manually connected image as a remaining character reference", () => {
    const scriptNode: CanvasNode = {
      ...node("script", { sourceType: "character", scriptEntryInitialized: true }),
      id: "script-node"
    };
    const autoRole: CanvasNode = {
      ...node("image", { scriptSourceNode: true, scriptNodeId: "script-node", roleIndex: 1 }),
      id: "auto-role"
    };
    const manualRole: CanvasNode = {
      ...node("image"),
      id: "manual-role",
      data: {
        ...node("image").data,
        title: "手动角色图片",
        generation: { id: "manual-generation", status: "success", previewUrl: "/manual-role.png" }
      }
    };
    const edges: CanvasEdge[] = [
      { id: "auto-script", source: "auto-role", target: "script-node", data: { order: 1 } },
      { id: "manual-script", source: "manual-role", target: "script-node", data: { order: 2 } }
    ];

    expect(scriptEntryResetPlan([scriptNode, autoRole, manualRole], edges, "auto-role")).toEqual({
      targetIds: [],
      sourceIds: []
    });
    const remainingEdges = normalizeEdgeOrders(edges.filter(edge => edge.source !== "auto-role"));
    expect(scriptCanvasReferences([scriptNode, manualRole], remainingEdges, "script-node")).toEqual([
      expect.objectContaining({
        nodeId: "manual-role",
        generationId: "manual-generation",
        previewUrl: "/manual-role.png",
        ready: true
      })
    ]);
  });

  it("resets a pending character entry after its final image reference is deleted", () => {
    const scriptNode: CanvasNode = {
      ...node("script", { sourceType: "character", scriptEntryInitialized: true }),
      id: "script-node"
    };
    const role: CanvasNode = { ...node("image"), id: "role-one" };
    const edges: CanvasEdge[] = [
      { id: "role-script", source: "role-one", target: "script-node", data: { order: 1 } }
    ];

    expect(scriptEntryResetPlan([scriptNode, role], edges, "role-one")).toEqual({
      targetIds: ["script-node"],
      sourceIds: []
    });
  });

  it("removes generated upstream placeholders together with their script node", () => {
    const scriptNode: CanvasNode = { ...node("script"), id: "script-node" };
    const sourceNode: CanvasNode = {
      ...node("text", { scriptSourceNode: true, scriptNodeId: "script-node" }),
      id: "text-source"
    };

    expect(scriptEntryResetPlan([scriptNode, sourceNode], [], "script-node")).toEqual({
      targetIds: [],
      sourceIds: ["text-source"]
    });
  });

  it("uses video mode before the connection default", () => {
    expect(operationForNode(node("video", { mode: "first_last_frame" }), [edge("one", 1, "img2video")])).toBe("first_last_frame");
    expect(operationForNode(node("video", { mode: "image_reference" }), [])).toBe("img2video");
  });

  it("图片工具子节点一律 img2img，不沿用老节点入边上的工具码", () => {
    expect(operationForNode(node("image", { tool: "upscale", toolOptions: { level: "fine" } }), [edge("image", 1, "upscale")])).toBe("img2img");
    expect(operationForNode(node("image", { tool: "expression_edit" }), [])).toBe("img2img");
  });

  it("uses image-to-image when an image node has an uploaded reference", () => {
    expect(operationForNode(node("image", {
      reference: { id: "88", type: "image", contentType: "image/png", name: "scene.png", size: 8 }
    }), [])).toBe("img2img");
  });

  it("keeps image-to-image when a text connection also supplies the prompt", () => {
    expect(operationForNode(node("image", {
      reference: { id: "88", type: "image", contentType: "image/png", name: "scene.png", size: 8 }
    }), [edge("text", 1, "text2img")])).toBe("img2img");
  });

  it("uses image-to-image when several references are uploaded", () => {
    expect(operationForNode(node("image", {
      reference: [
        { id: "88", type: "image", contentType: "image/png", name: "a.png", size: 8 },
        { id: "89", type: "image", contentType: "image/png", name: "b.png", size: 9 }
      ]
    }), [])).toBe("img2img");
  });

  it("activates video modes from their required inputs", () => {
    expect(isVideoModeAvailable("text2video", [])).toBe(true);
    expect(isVideoModeAvailable("text2video", ["image"])).toBe(false);
    expect(isVideoModeAvailable("img2video", ["audio"])).toBe(false);
    expect(isVideoModeAvailable("image_reference", ["image"])).toBe(true);
    expect(isVideoModeAvailable("img2video", ["image", "image"])).toBe(false);
    expect(isVideoModeAvailable("first_last_frame", ["image"])).toBe(false);
    expect(isVideoModeAvailable("first_last_frame", ["image", "image"])).toBe(true);
    expect(isVideoModeAvailable("ref2video", ["audio", "image"])).toBe(true);
    // DRK-12（3D 导演台）：全能参考收参考视频，某个模型收不收由服务端报价按能力矩阵把关。
    expect(isVideoModeAvailable("ref2video", ["image", "video"])).toBe(true);
    expect(isVideoModeAvailable("ref2video", ["video"])).toBe(true);
    // 其他模式的媒体规则不变：图生视频仍然只收一张图。
    expect(isVideoModeAvailable("img2video", ["video"])).toBe(false);
    expect(isVideoModeAvailable("video2video", ["video"])).toBe(true);
    expect(isVideoModeAvailable("video2video", ["image"])).toBe(false);
    expect(isVideoModeAvailable("img2video", ["image", "effect"])).toBe(true);
  });

  it("switches an existing video node to the mode required by new inputs", () => {
    expect(videoModeForInputs("text2video", ["image"])).toBe("img2video");
    expect(videoModeForInputs("img2video", ["image", "image"])).toBe("first_last_frame");
    expect(videoModeForInputs("text2video", ["text", "image"])).toBe("ref2video");
    expect(videoModeForInputs("text2video", ["video"])).toBe("video2video");
    // 视频与图片混在一起时只有全能参考能收（DRK-12）；单独一段视频仍是视频生视频。
    expect(videoModeForInputs("text2video", ["image", "video"])).toBe("ref2video");
    expect(videoModeForInputs("ref2video", ["image"])).toBe("ref2video");
    expect(videoModeForInputs("img2video", [])).toBe("text2video");
  });

  it("keeps video mode labels when model overrides use string values", () => {
    const base = [
      { label: "文生视频", value: "text2video" },
      { label: "视频生视频", value: "video2video" }
    ];

    expect(videoModeOptions(base, ["video2video"], ["video"])).toEqual([
      { label: "视频生视频", value: "video2video", disabled: false }
    ]);
  });

  it("merges material node ids by connection order and clears stale references", () => {
    const style = {
      ...node("style", { styleId: "41001" }),
      id: "style-source"
    };
    const target = node("image", {
      prompt: "test",
      styleId: "stale",
      effectId: "stale",
      reference: { id: "88", type: "image" }
    });
    const materialEdge: CanvasEdge = {
      id: "style-edge",
      source: "style-source",
      target: "target",
      data: { order: 1 }
    };

    expect(mergeMaterialReferences(target.data.params, [style, target], [materialEdge], "target"))
      .toEqual({ prompt: "test", styleId: "41001" });
    expect(mergeMaterialReferences(target.data.params, [style, target], [], "target"))
      .toEqual({ prompt: "test" });
  });

  it("places new nodes in a stable visible two-column grid", () => {
    expect(newNodePosition(1)).toEqual({ x: 180, y: 100 });
    expect(newNodePosition(2)).toEqual({ x: 570, y: 100 });
    expect(newNodePosition(3)).toEqual({ x: 180, y: 460 });
  });
});

describe("受理请求的调用方", () => {
  const read = (path: string) => readFileSync(join(__dirname, "..", path), "utf8");
  it("受理发的是契约形状的输入，报价发的是角色计数 —— 都走 utils 里那一份", () => {
    const store = read("stores/generation.ts");
    // ⚠️ 原样 `body: request` 的话 inputs 的 kind 是媒体类型，带图的生成全部被拒
    expect(store).not.toMatch(/body:\s*request\b/);
    expect(store).toContain("acceptInputsOf(request.inputs");
    expect(store).toContain("roleCountsOf(request.inputs");
  });
});

describe("智能剪辑作为上游（SCT-23，SCD-50）", () => {
  const smartcut = (id = "sc"): CanvasNode => ({
    id,
    type: "aitv",
    position: { x: 0, y: 0 },
    data: { nodeType: "smartcut", title: "新品混剪", params: { timelineId: "701", prompt: "剪一条" } }
  });
  const fromSmartcut = (order = 1): CanvasEdge => ({ id: `e${order}`, source: "sc", target: "target", data: { order } });

  it("canvasMediaRole：smartcut 映射为 video", () => {
    expect(canvasMediaRole("smartcut")).toBe("video");
  });

  it("generationInputs：有成片时给 video + referenceId（成片来自摘要，不在节点 params 里）", () => {
    const [input] = generationInputs([smartcut()], [fromSmartcut()], "target", { sc: "9001" });
    expect(input).toEqual({ sourceNodeId: "sc", kind: "video", order: 1, referenceId: "9001" });
    expect(generationInputReady(input!)).toBe(true);
    expect(acceptInputsOf([input!])).toEqual([{ sourceNodeId: "sc", kind: "reference", order: 1, referenceId: "9001" }]);
  });

  it("generationInputs：没有成片时未就绪，提示先导出", () => {
    const [input] = generationInputs([smartcut()], [fromSmartcut()], "target");
    expect(input).toEqual({ sourceNodeId: "sc", kind: "video", order: 1 });
    expect(generationInputReady(input!)).toBe(false);
    expect(acceptInputsOf([input!])).toEqual([]);
    expect(upstreamNotReadyText(smartcut())).toBe("请先在智能剪辑里导出成片：新品混剪");
  });

  it("roleCountsOf：智能剪辑计为 video", () => {
    expect(roleCountsOf(generationInputs([smartcut()], [fromSmartcut()], "target", { sc: "9001" }))).toEqual({ video: 1 });
  });
});

describe("3D 导演台作为上游（DRT-16）", () => {
  const director = (params: Record<string, unknown>, id = "dir"): CanvasNode => ({
    id,
    type: "aitv",
    position: { x: 0, y: 0 },
    data: { nodeType: "director", title: "3D 导演台节点 1", params }
  });
  const fromDirector = (order = 1): CanvasEdge => ({ id: `e${order}`, source: "dir", target: "target", data: { order } });

  it("canvasMediaRole：director 映射为 image", () => {
    expect(canvasMediaRole("director")).toBe("image");
    for (const type of ["text", "image", "video", "audio", "script", "style"]) expect(canvasMediaRole(type)).toBe(type);
  });

  it("generationInputs：导演台有截图时给 image + referenceId", () => {
    const [input] = generationInputs([director({ sceneId: "9", outputMediaId: "4401" })], [fromDirector()], "target");
    expect(input).toEqual({ sourceNodeId: "dir", kind: "image", order: 1, referenceId: "4401" });
    expect(generationInputReady(input!)).toBe(true);
    // 受理时是「参考」：服务端按媒体 ID 校验归属与文件头
    expect(acceptInputsOf([input!])).toEqual([{ sourceNodeId: "dir", kind: "reference", order: 1, referenceId: "4401" }]);
  });

  it("generationInputs：导演台没截图时未就绪", () => {
    for (const params of [{ sceneId: "9" }, { sceneId: "9", outputMediaId: "" }, { outputMediaId: "not-an-id" }]) {
      const [input] = generationInputs([director(params)], [fromDirector()], "target");
      expect(input).toEqual({ sourceNodeId: "dir", kind: "image", order: 1 });
      expect(generationInputReady(input!)).toBe(false);
      // 未就绪的输入不发给受理
      expect(acceptInputsOf([input!])).toEqual([]);
    }
  });

  it("roleCountsOf：导演台计为 image", () => {
    const inputs = generationInputs(
      [director({ outputMediaId: "1" }), { ...director({ outputMediaId: "2" }, "dir2") }],
      [fromDirector(1), { id: "e2", source: "dir2", target: "target", data: { order: 2 } }],
      "target"
    );
    expect(roleCountsOf(inputs)).toEqual({ image: 2 });
  });

  it("videoModeForInputs：导演台上游按图片参与判断", () => {
    // 修复前导演台被当成未知类型过滤掉：一张截图连进来，模式停在文生视频
    expect(videoModeForInputs("text2video", ["director"])).toBe("img2video");
    expect(videoModeForInputs("text2video", ["director", "image"])).toBe("first_last_frame");
    expect(videoModeForInputs("ref2video", ["director"])).toBe("ref2video");
    expect(isVideoModeAvailable("img2video", ["director"])).toBe(true);
    expect(isVideoModeAvailable("text2video", ["director"])).toBe(false);
    expect(connectedVideoMode("video", undefined, "director")).toBe("img2video");
    expect(connectedVideoMode("video", "ref2video", "director")).toBe("ref2video");
  });

  it("connectedVideoMode 预演源默认全能参考；普通视频源仍默认视频生视频（DRT-25，DR-F-128）", () => {
    const preview = { directorSource: { kind: "preview", mediaId: "9001" } };
    expect(connectedVideoMode("video", undefined, "video", preview)).toBe("ref2video");
    expect(connectedVideoMode("video", "video2video", "video", preview)).toBe("ref2video");
    expect(connectedVideoMode("video", undefined, "video")).toBe("video2video");
    expect(connectedVideoMode("video", undefined, "video", { directorSource: { kind: "capture" } })).toBe("video2video");
  });

  it("estimateRejectionText 只把 422 的业务拒绝原样给出（DRT-25，DR-F-128）", () => {
    const rejected = { statusCode: 422, message: "该模型此操作不接受 video 输入", data: { code: 42203 } };
    expect(estimateRejectionText(rejected)).toBe("该模型此操作不接受 video 输入");
    expect(estimateRejectionText({ statusCode: 422, message: " 该能力缺少可用价格 ", data: { code: 42205 } })).toBe("该能力缺少可用价格");
    // 断网、5xx、409 都不是「这个组合不行」：报价只是预估，不在节点上挂红字
    expect(estimateRejectionText({ statusCode: 503, message: "网络不稳定", data: {} })).toBe("");
    expect(estimateRejectionText({ statusCode: 409, message: "版本冲突", data: { code: 40902 } })).toBe("");
    expect(estimateRejectionText(new Error("boom"))).toBe("");
    expect(estimateRejectionText(null)).toBe("");
  });

  it("generationInputs：发布节点作为上游给 reference", () => {
    // 发布节点的 generation 是「已有产出」形态、没有 id（DRT-17）：下游按媒体 ID 声明参考
    const published: CanvasNode = {
      id: "pub", type: "aitv", position: { x: 0, y: 0 },
      data: { nodeType: "image", title: "机位 1 截图", params: { directorSource: { kind: "capture", sceneId: "9", recordId: "8", mediaId: "7001" } },
        generation: { status: "success", progress: 100, outputType: "image", mediaId: "7001", outputUrl: "/api/v1/media/7001" } }
    };
    const [input] = generationInputs([published], [{ id: "e", source: "pub", target: "target", data: { order: 1 } }], "target");
    expect(input).toEqual({ sourceNodeId: "pub", kind: "image", order: 1, referenceId: "7001" });
    expect(acceptInputsOf([input!])).toEqual([{ sourceNodeId: "pub", kind: "reference", order: 1, referenceId: "7001" }]);
  });

  it("upstreamNotReadyText：导演台上游提示先截图", () => {
    expect(upstreamNotReadyText(director({}))).toBe("请先在导演台截图：3D 导演台节点 1");
    expect(upstreamNotReadyText({ data: { nodeType: "image", title: "图片节点 2", params: {} } })).toBe("请先完成上游节点：图片节点 2");
    expect(upstreamNotReadyText(undefined)).toBe("请先完成上游节点：引用节点");
  });
});

describe("连线校验的结论（DRB-1）", () => {
  it("valid:false 拒绝并带服务端的原因；没给原因时用通用文案", () => {
    expect(connectionRejection({ valid: true, operation: "img2img" })).toBeNull();
    expect(connectionRejection({ valid: false, reason: "  首尾帧只收两张图  " })).toBe("首尾帧只收两张图");
    expect(connectionRejection({ valid: false })).toBe("此连接不受支持");
    expect(connectionRejection({ valid: false, reason: "   " })).toBe("此连接不受支持");
  });

  it("缺 valid 的响应按拒绝处理，不放行", () => {
    expect(connectionRejection({ operation: "img2img" })).toBe("此连接不受支持");
    expect(connectionRejection(undefined)).toBe("此连接不受支持");
  });

  it("每个连线校验的调用点都看 valid（只 catch 异常的话 valid:false 一律被当成放行）", () => {
    const read = (path: string) => readFileSync(join(__dirname, "..", path), "utf8");
    const sites: Array<[string, RegExp]> = [
      ["components/canvas/CanvasBoard.vue", /await validateConnection\(/g],
      ["components/canvas/ParamPanel.vue", /await validateVideoInputs\(/g]
    ];
    let total = 0;
    for (const [path, call] of sites) {
      const source = read(path);
      const calls = [...source.matchAll(call)];
      expect(calls.length, path).toBeGreaterThan(0);
      for (const match of calls) {
        total += 1;
        // 调用之后紧跟着的几行里必须先过 connectionRejection，再做任何改动
        const following = source.slice(match.index ?? 0, (match.index ?? 0) + 600);
        expect(following, `${path} 第 ${total} 处`).toMatch(/connectionRejection\(/);
      }
    }
    // connect() · createConnectedNode · 脚本源节点 · 视频特效节点：少一处就是又多了一个不看 valid 的入口
    expect(total).toBe(4);
  });
});

describe("视频节点按产物比例摆", () => {
  it("竖屏是竖的卡片、横屏是横的，宽高比不变", () => {
    expect(videoPreviewFrame(1080, 1920)).toEqual({ width: 270, height: 480 });
    expect(videoPreviewFrame(1280, 720)).toEqual({ width: 390, height: 219 });
    expect(videoPreviewFrame(720, 720)).toEqual({ width: 390, height: 390 });
    expect(videoPreviewFrame(3, 4)).toEqual({ width: 360, height: 480 });
  });

  it("宽高未知时就是此前固定的 16:9 横框", () => {
    expect(videoPreviewFrame()).toEqual({ width: 390, height: 219 });
    expect(videoPreviewFrame(0, 0)).toEqual({ width: 390, height: 219 });
  });

  it("特别细长的比例由最小宽 / 最小高兜住（剩下的留边），卡片不会窄到放不下标题", () => {
    expect(videoPreviewFrame(1, 3)).toEqual({ width: 220, height: 480 });
    expect(videoPreviewFrame(4, 1)).toEqual({ width: 390, height: 150 });
  });

  it("画幅参数换成比例；自适应、空值、坏值都没有", () => {
    expect(aspectFromRatio("9:16")).toEqual({ width: 9, height: 16 });
    expect(aspectFromRatio({ label: "21:9", value: "21:9" })).toEqual({ width: 21, height: 9 });
    expect(aspectFromRatio("adaptive")).toBeUndefined();
    expect(aspectFromRatio(undefined)).toBeUndefined();
    expect(aspectFromRatio("0:16")).toBeUndefined();
  });
});

describe("视频节点的控制条与截帧（2026-11-01）", () => {
  it("时间按 m:ss 显示，满一小时带小时；向下取整到秒，坏值按 0", () => {
    expect(formatVideoClock(0)).toBe("0:00");
    expect(formatVideoClock(5.9)).toBe("0:05");
    expect(formatVideoClock(30)).toBe("0:30");
    expect(formatVideoClock(754)).toBe("12:34");
    expect(formatVideoClock(3723)).toBe("1:02:03");
    expect(formatVideoClock(Number.NaN)).toBe("0:00");
    expect(formatVideoClock(Number.POSITIVE_INFINITY)).toBe("0:00");
    expect(formatVideoClock(-3)).toBe("0:00");
  });

  it("进度条上的位置换成播放时间，越过两端就夹在头尾", () => {
    const track = { left: 100, width: 200 };
    expect(videoSeekTime(200, track, 30)).toBe(15);
    expect(videoSeekTime(150, track, 30)).toBe(7.5);
    expect(videoSeekTime(40, track, 30)).toBe(0);
    expect(videoSeekTime(900, track, 30)).toBe(30);
  });

  it("量不到轨道宽度、时长未知或是 Infinity 时不跳（返回 0）", () => {
    expect(videoSeekTime(200, { left: 100, width: 0 }, 30)).toBe(0);
    expect(videoSeekTime(200, { left: 100, width: 200 }, 0)).toBe(0);
    expect(videoSeekTime(200, { left: 100, width: 200 }, Number.NaN)).toBe(0);
    expect(videoSeekTime(200, { left: 100, width: 200 }, Number.POSITIVE_INFINITY)).toBe(0);
  });

  it("截帧节点放在视频节点右侧一个身位（与派生节点同为 +420），那里空着就同一行", () => {
    const video = { x: 100, y: 50, width: 270, height: 530 };
    expect(frameCaptureNodePosition(video, [video], { width: 390, height: 300 })).toEqual({ x: 520, y: 50 });
  });

  it("那里已经有节点（上一次截的帧）就挪到它下面隔 40；连截几帧竖着排下去", () => {
    const video = { x: 100, y: 50, width: 270, height: 530 };
    const first = { x: 520, y: 50, width: 390, height: 560 };
    expect(frameCaptureNodePosition(video, [video, first], { width: 390, height: 560 })).toEqual({ x: 520, y: 650 });
    const second = { x: 520, y: 650, width: 390, height: 560 };
    // 节点顺序无关：先撞上第二个也会一路挪到两个的下面
    expect(frameCaptureNodePosition(video, [second, video, first], { width: 390, height: 560 })).toEqual({ x: 520, y: 1250 });
  });

  it("只避开真正重叠的节点：右边更远、或在上方的节点不影响落点", () => {
    const video = { x: 100, y: 50, width: 270, height: 530 };
    const farRight = { x: 1000, y: 50, width: 390, height: 300 };
    const above = { x: 520, y: -400, width: 390, height: 300 };
    expect(frameCaptureNodePosition(video, [video, farRight, above], { width: 390, height: 300 })).toEqual({ x: 520, y: 50 });
  });

  it("画面与控制条按需加载：截帧带着上传与建节点，不能进画布页首屏包", () => {
    const baseNode = readFileSync(join(__dirname, "..", "components/canvas/BaseNode.vue"), "utf8");
    expect(baseNode).toMatch(/defineAsyncComponent\(\(\) => import\("\.\/nodes\/VideoNodeBody\.vue"\)\)/);
    expect(baseNode).not.toMatch(/import\s+VideoNodeBody\s+from/);
  });
});

describe("参考上传与角色库", () => {
  it("新后端的上传响应（kind / mime / sizeBytes、没有文件名）能落成节点上的参考", () => {
    expect(uploadedReferenceOf({ id: "123", kind: "video", mime: "video/mp4", sizeBytes: 2048, checksum: "x" }, { name: "片段.mp4" }))
      .toEqual({ id: "123", type: "video", contentType: "video/mp4", name: "片段.mp4", size: 2048, url: "/media/123" });
  });

  it("老后端的响应形状照样认；缺 id 或类型认不出时当失败", () => {
    expect(uploadedReferenceOf({ id: "9", type: "image", contentType: "image/png", name: "a.png", size: 8 })?.name).toBe("a.png");
    expect(uploadedReferenceOf({ kind: "image", mime: "image/png" }, { name: "a.png" })).toBeUndefined();
    expect(uploadedReferenceOf({ id: "9", kind: "document" }, { name: "a.pdf" })).toBeUndefined();
  });

  it("角色只认对象形态：老画布存的画布节点 ID、没有主图的、重复的一律跳过", () => {
    expect(canvasRoleCharacters([
      "node-1",
      { kind: "marketing_character", id: "7", name: "小美", mediaId: "70" },
      { kind: "marketing_character", id: "7", name: "小美", mediaId: "70" },
      { kind: "asset", id: "8", name: "", mediaId: "80" },
      { kind: "asset", id: "9", name: "没图", mediaId: "" },
      { kind: "other", id: "10", name: "x", mediaId: "100" }
    ])).toEqual([
      { kind: "marketing_character", id: "7", name: "小美", mediaId: "70" },
      { kind: "asset", id: "8", name: "角色", mediaId: "80" }
    ]);
    expect(canvasRoleCharacters(undefined)).toEqual([]);
  });
});

describe("切模型时按节点配置 + 新模型覆盖校准", () => {
  const videoParams = [
    { code: "model", config: { domainFrom: "models" } },
    { code: "aspect_ratio", defaultValue: "16:9", config: { options: ["16:9", "9:16", "1:1"].map(value => ({ label: value, value })) } },
    { code: "duration", defaultValue: "5", config: { options: [{ label: "5 秒", value: "5" }, { label: "10 秒", value: "10" }] } },
    { code: "camera", config: { sections: [] } }
  ];

  it("新模型没有覆盖时，上一个模型留下的值按节点配置校正（可灵 Omni 的 adaptive 切到 Grok）", () => {
    const next = reconcileParams({ model: "grok", aspect_ratio: "adaptive", duration: "15" }, modelReconcileRules(videoParams, {}));
    expect(next).toEqual({ model: "grok", aspect_ratio: "16:9", duration: "5" });
  });

  it("新模型的覆盖照样生效，节点默认值不在可选项里时取覆盖的默认或第一项", () => {
    const omni = { aspect_ratio: { options: [{ label: "自适应", value: "adaptive" }] } };
    expect(reconcileParams({ aspect_ratio: "16:9" }, modelReconcileRules(videoParams, omni)).aspect_ratio).toBe("adaptive");
    const seedance = { duration: { options: ["4", "15", "30"].map(value => ({ label: value, value })), default: "15" } };
    expect(reconcileParams({ duration: "10" }, modelReconcileRules(videoParams, seedance)).duration).toBe("15");
  });

  it("懒加载的列表与没有选项的控件不校准", () => {
    const rules = modelReconcileRules(videoParams, {});
    expect(rules.model).toBeUndefined();
    expect(rules.camera).toBeUndefined();
  });
});

describe("镜头文字带到下游（DRT-26，DR-F-132）", () => {
  const node = (id: string, nodeType: string, title: string, params: Record<string, unknown> = {}) => ({ id, data: { nodeType, title, params } });
  const edge = (source: string, target: string, order: number) => ({ id: `${source}-${target}`, source, target, data: { order } });

  it("upstreamShotTextOf 只取导演台或发布节点", () => {
    const nodes = [
      node("dir", "director", "导演台 A", { outputMediaId: "7002", captures: [
        { cameraId: "cam1", mediaId: "7001", shotText: "中景 · 平视 · 50mm" },
        { cameraId: "cam2", mediaId: "7002", shotText: "全景 · 低角度仰拍 · 35mm" }
      ] }),
      node("pub", "image", "机位 1 截图", { directorSource: { kind: "capture", mediaId: "7003", shotText: "近景 · 俯拍 · 85mm" } }),
      node("img", "image", "普通图片", { prompt: "x" }),
      node("dir2", "director", "导演台 B", { outputMediaId: "7009", captures: [{ cameraId: "c", mediaId: "7008", shotText: "特写" }] }),
      node("down", "video", "下游", {})
    ];
    const edges = [edge("img", "down", 1), edge("pub", "down", 2), edge("dir", "down", 3), edge("dir2", "down", 4)];
    // 导演台取输出机位那张截图的文字；普通图片节点没有；输出机位没有截图摘要的导演台跳过
    expect(upstreamShotTextOf(nodes, edges, "down")).toEqual([
      { nodeId: "pub", title: "机位 1 截图", text: "近景 · 俯拍 · 85mm" },
      { nodeId: "dir", title: "导演台 A", text: "全景 · 低角度仰拍 · 35mm" }
    ]);
    expect(upstreamShotTextOf(nodes, edges, "img")).toEqual([]);
  });

  it("insertTextAt 插到光标处、没有光标时追加到末尾，前后补空格", () => {
    expect(insertTextAt("夜里的街道", "全景 · 平视", 2, 2)).toEqual({ value: "夜里 全景 · 平视 的街道", caret: 11 });
    expect(insertTextAt("夜里的街道", "全景")).toEqual({ value: "夜里的街道 全景", caret: 8 });
    expect(insertTextAt("", "全景")).toEqual({ value: "全景", caret: 2 });
    // 选中一段时替换掉它
    expect(insertTextAt("abc def", "X", 4, 7)).toEqual({ value: "abc X", caret: 5 });
  });
});

describe("生成状态的显示（2026-11-01）", () => {
  it("标题栏：服务端状态词换成中文，不再显示 running / success", () => {
    expect(generationStatusLabel({ status: "queued" })).toBe("排队中");
    expect(generationStatusLabel({ status: "running" })).toBe("生成中");
    expect(generationStatusLabel({ status: "success", progress: 100 })).toBe("已完成");
    expect(generationStatusLabel({ status: "failed" })).toBe("生成失败");
    expect(generationStatusLabel({ status: "cancelled" })).toBe("已取消");
    expect(generationStatusLabel({ status: "unknown" })).toBe("处理中");
    expect(generationStatusLabel(undefined)).toBe("");
    expect(generationStatusLabel({})).toBe("");
  });

  it("标题栏：本页点取消落成 failed + cancelled，显示已取消；词表外的值原样显示", () => {
    expect(generationStatusLabel({ status: "failed", errorCode: "cancelled" })).toBe("已取消");
    expect(generationStatusLabel({ status: "paused" })).toBe("paused");
  });

  it("标题栏：只在真有进度时带百分比（文本模型一直是 0，不显示「生成中 0%」）", () => {
    expect(generationStatusLabel({ status: "running", progress: 0 })).toBe("生成中");
    expect(generationStatusLabel({ status: "running", progress: 35 })).toBe("生成中 35%");
    expect(generationStatusLabel({ status: "running", progress: 100 })).toBe("生成中");
    expect(generationStatusLabel({ status: "queued", progress: 20 })).toBe("排队中");
  });

  it("生成中：拿不到进度时显示等了多久（文本 / 图片 / 视频节点同一套），不满 1 秒只显示状态", () => {
    expect(generatingLabel("running", 0, 400)).toEqual({ head: "生成中", detail: "" });
    expect(generatingLabel("running", 0, 12_300)).toEqual({ head: "生成中", detail: "12 秒" });
    expect(generatingLabel("running", undefined, 65_000)).toEqual({ head: "生成中", detail: "1 分 05 秒" });
    expect(generatingLabel("queued", 0, 5_000)).toEqual({ head: "排队中", detail: "5 秒" });
  });

  it("生成中：有进度就显示百分比；非法的等待时长按 0", () => {
    expect(generatingLabel("running", 42.4, 90_000)).toEqual({ head: "生成中", detail: "42%" });
    expect(generatingLabel("running", 0, Number.NaN)).toEqual({ head: "生成中", detail: "" });
    expect(generatingLabel("running", 0, -3000)).toEqual({ head: "生成中", detail: "" });
  });
});

describe("文本节点生成中的样式还在（2026-11-01 回归）", () => {
  it("BaseNode 用到的骨架行 / 底部胶囊在 canvas.css 里有样式", () => {
    // 10-30「Markdown 预览」那次把这两段样式随 .node-manual-text 一起删了，模板还在用：
    // 生成中成了左上角一行没样式的大字加一个浏览器默认的灰方块按钮，骨架行看不见，而且没有任何报错
    const read = (path: string) => readFileSync(join(__dirname, "..", path), "utf8");
    const baseNode = read("components/canvas/BaseNode.vue");
    const css = read("assets/css/canvas.css");
    for (const name of ["node-text-skeleton", "node-text-progress"]) {
      expect(baseNode).toContain(`class="${name}"`);
      expect(css).toMatch(new RegExp(`\\.${name}\\s*\\{`));
    }
  });
});
