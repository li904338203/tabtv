import { describe, expect, it } from "vitest";

import type { ScriptShot } from "~/types/script";
import {
  groupItemReferences,
  groupItemStatusLabel,
  groupMissingLabels,
  groupNeedsRepair,
  groupStatusLabel,
  mergeMentionEntities,
  mergeShotResp,
  normalizeGroupDetail,
  normalizeScriptDetail,
  planShotSync,
  rebaseScriptDetail,
  scriptCreateBody,
  scriptStoryInput,
  shotChanges,
  shotsLeftOut,
  storyboardBody,
  type GroupResp,
  type ScriptDetailResp
} from "./scriptContract";

describe("创建脚本的请求体", () => {
  it("按新契约发 canvasNodeId / rawInput（老契约的 nodeId / input.text 新后端不认 —— 建出一条节点 ID 为空的脚本）", () => {
    const body = scriptCreateBody({ projectId: "98862087912030208", nodeId: "n_mv708zjs_1", sourceType: "script", rawInput: " 三打白骨精 " });
    expect(body).toEqual({ projectId: "98862087912030208", canvasNodeId: "n_mv708zjs_1", sourceType: "script", rawInput: "三打白骨精" });
    expect(body).not.toHaveProperty("nodeId");
    expect(body).not.toHaveProperty("input");
  });
});

describe("剧情与补充上下文", () => {
  const textRef = { nodeId: "t1", type: "text" as const, title: "剧本", text: "第一回：白骨精三戏唐僧" };
  const roleA = { nodeId: "i1", type: "image" as const, title: "孙悟空", generationId: "1" };
  const roleB = { nodeId: "i2", type: "image" as const, title: "白骨精", generationId: "2" };

  it("直接输入：剧情就是用户写的；连了文本节点就当补充上下文", () => {
    expect(scriptStoryInput("direct", "三打白骨精", [])).toEqual({ rawInput: "三打白骨精", extraContext: "" });
    expect(scriptStoryInput("direct", "三打白骨精", [textRef]).extraContext).toContain("白骨精三戏唐僧");
  });

  it("文本参考：剧情是上游文本，用户写的是补充要求（反过来的话模型把要求当剧情）", () => {
    const out = scriptStoryInput("text_reference", "节奏紧凑，8 镜", [textRef]);
    expect(out.rawInput).toContain("白骨精三戏唐僧");
    expect(out.extraContext).toBe("补充要求：\n节奏紧凑，8 镜");
    // 没写补充要求：只有剧情
    expect(scriptStoryInput("text_reference", "", [textRef]).extraContext).toBe("");
  });

  it("角色参考：剧情是用户写的，两个角色以名字进补充上下文（参考图本身送不进纯文本模型）", () => {
    const out = scriptStoryInput("character_reference", "两人在山间相遇", [roleA, roleB]);
    expect(out.rawInput).toBe("两人在山间相遇");
    expect(out.extraContext).toContain("孙悟空");
    expect(out.extraContext).toContain("白骨精");
  });
});

describe("分镜请求体", () => {
  it("字段是 modelCode（老契约 model 新后端不认 —— 模型码根本没送到）；没有补充上下文就不带", () => {
    expect(storyboardBody("gpt_5_6_sol")).toEqual({ modelCode: "gpt_5_6_sol" });
    expect(storyboardBody("gpt_5_6_sol", " 补充 ")).toEqual({ modelCode: "gpt_5_6_sol", extraContext: "补充" });
  });
});

describe("脚本详情归一", () => {
  const resp: ScriptDetailResp = {
    id: "103557206128984064",
    projectId: "98862087912030208",
    canvasNodeId: "n_mv708zjs_1",
    sourceType: "script",
    currentStep: 1,
    status: "editing",
    rawInput: "三打白骨精",
    version: 3,
    shots: [
      { id: "s1", shotNo: 1, durationSec: 5, description: "白骨精化作村姑", imagePromptStatus: "ready", videoPromptStatus: "ready", version: 2 },
      { id: "s2", shotNo: 2, durationSec: 4, description: "悟空识破", imagePromptStatus: "", videoPromptStatus: "stale", version: 1 }
    ]
  };

  it("新契约的 id / canvasNodeId / rawInput / version 归到界面用的 scriptId / nodeId / input.text / contentVersion", () => {
    const detail = normalizeScriptDetail(resp, { name: "脚本节点 1" });
    expect(detail.scriptId).toBe("103557206128984064");
    expect(detail.nodeId).toBe("n_mv708zjs_1");
    expect(detail.name).toBe("脚本节点 1");
    expect(detail.input.text).toBe("三打白骨精");
    // 新后端不存参考节点：不知道就不给（给 [] 的话画布节点回写时会把参考配置清空）
    expect(detail.input.references).toBeUndefined();
    expect(detail.shots.map(shot => [shot.id, shot.contentVersion, shot.sort])).toEqual([["s1", 2, 1], ["s2", 1, 2]]);
    // 空状态按「待合成」显示，不是空串
    expect(detail.shots[1].imagePromptStatus).toBe("pending");
    expect(detail.promptsProgress).toEqual({ completed: 1, total: 2, status: "running" });
    // 分镜是同步的：没有「生成中」的服务端状态
    expect(detail.storyboardRunning).toBe(false);
  });

  it("资产与组是独立端点：没取就是空列表；引用以详情里的 assetIds 为准，资产的 shotIds 由它反推", () => {
    expect(normalizeScriptDetail(resp).assets).toEqual([]);
    const withRefs: ScriptDetailResp = {
      ...resp,
      shots: [{ ...resp.shots![0]!, assetIds: ["a1"] }, { ...resp.shots![1]!, assetIds: [] }]
    };
    const detail = normalizeScriptDetail(withRefs, {
      assets: [{ id: "a1", type: "character", name: "白骨精", status: "ready", url: "/api/v1/media/9", source: "ai" }],
      groups: [{ id: "g1", canvasNodeId: "n_g", groupType: "asset", name: "资产组", status: "success", version: 2 }]
    });
    expect(detail.shots[0].assetIds).toEqual(["a1"]);
    expect(detail.shots[1].assetIds).toEqual([]);
    // 新契约 type / url → 界面用的 assetType / imageUrl
    expect(detail.assets[0]).toMatchObject({ id: "a1", assetType: "character", imageUrl: "/api/v1/media/9", shotIds: ["s1"] });
    expect(detail.assetsProgress).toEqual({ completed: 1, total: 1, status: "done" });
    expect(detail.groups[0]).toMatchObject({ id: "g1", groupType: "asset", name: "资产组" });
  });

  it("只带镜头的响应（PATCH / 加删镜头）保留当前的资产与组，资产的 shotIds 跟着新镜头表重算", () => {
    const current = normalizeScriptDetail({ ...resp, shots: [{ ...resp.shots![0]!, assetIds: ["a1"] }] }, {
      assets: [{ id: "a1", type: "character", name: "白骨精", status: "ready" }],
      groups: [{ id: "g1", canvasNodeId: "n_g", groupType: "asset", status: "success", version: 1 }]
    });
    const next = rebaseScriptDetail(current, { ...resp, version: 4, shots: [{ ...resp.shots![1]!, assetIds: [] }] });
    expect(next.version).toBe(4);
    expect(next.assets.map(asset => [asset.id, asset.shotIds])).toEqual([["a1", []]]);
    expect(next.groups).toHaveLength(1);
  });

  it("单个镜头的响应不带引用：沿用当前的 assetIds，不要清空", () => {
    const shot = normalizeScriptDetail({ ...resp, shots: [{ ...resp.shots![0]!, assetIds: ["a1", "a2"] }] }).shots[0]!;
    const merged = mergeShotResp(shot, { ...resp.shots![0]!, version: 3, imagePromptStatus: "ready", imagePrompt: "新词" });
    expect(merged.assetIds).toEqual(["a1", "a2"]);
    expect(merged.contentVersion).toBe(3);
    expect(merged.imagePrompt).toBe("新词");
  });

  it("镜头为 null（新建的空脚本）也能归一", () => {
    const detail = normalizeScriptDetail({ ...resp, shots: null });
    expect(detail.shots).toEqual([]);
    expect(detail.shotsProgress.total).toBe(0);
  });
});

function shot(id: string, extra: Partial<ScriptShot> = {}): ScriptShot {
  return {
    id, shotNo: 1, durationSec: 5, description: `镜头 ${id}`, shotSize: "中景", lighting: "", dialogue: "", soundEffect: "",
    cameraMove: "", imagePrompt: "", videoPrompt: "", imagePromptStatus: "pending", videoPromptStatus: "pending",
    contentVersion: 1, sort: 1, assetIds: [], entities: [], ...extra
  };
}

describe("镜头表保存拆成逐镜操作（新后端没有整表保存）", () => {
  it("没改的字段不发：一起发出去服务端照样把提示词标过期，用户白白再合成一次", () => {
    const base = shot("s1");
    expect(shotChanges(base, { ...base })).toEqual({});
    expect(shotChanges(base, { ...base, lighting: "逆光", durationSec: 8 })).toEqual({ lighting: "逆光", durationSec: 8 });
    // 时长按 1~60 的整数发（输入框里可能是空串或小数）
    expect(shotChanges(base, { ...base, durationSec: 99 })).toEqual({ durationSec: 60 });
  });

  it("删 / 建 / 改 / 排序各归各：新镜头插在前一镜之后，前一镜也可能是新镜头的草稿", () => {
    const baseline = [shot("s1"), shot("s2", { contentVersion: 4 }), shot("s3")];
    const draft = [
      shot("s1"),
      shot("draft-a", { description: "新镜头 A", durationSec: 6, lighting: "逆光" }),
      shot("draft-b", { description: "新镜头 B" }),
      shot("s2", { contentVersion: 4, dialogue: "改了对白" })
    ];
    const plan = planShotSync(baseline, draft);
    expect(plan.deletes).toEqual(["s3"]);
    expect(plan.creates).toEqual([
      { draftId: "draft-a", after: "s1", durationSec: 6, description: "新镜头 A", rest: { shotSize: "中景", lighting: "逆光" } },
      { draftId: "draft-b", after: "draft-a", durationSec: 5, description: "新镜头 B", rest: { shotSize: "中景" } }
    ]);
    // 改动带的是基线的版本（服务端上的那个）
    expect(plan.updates).toEqual([{ id: "s2", version: 4, changes: { dialogue: "改了对白" } }]);
    expect(plan.order).toEqual(["s1", "draft-a", "draft-b", "s2"]);
  });

  it("@ 新写的名字追加成实体；模型给的实体一个都不删（描述里大多没有 @）", () => {
    const existing = [{ name: "唐僧", type: "character" }];
    const next = mergeMentionEntities(existing, "唐僧回头，@白骨精 现身", [{ name: "白骨精", assetType: "character" }]);
    expect(next).toEqual([{ name: "唐僧", type: "character" }, { name: "白骨精", type: "character" }]);
    // 没有新 @：原样（不因为描述里没写 @唐僧 就把它删掉）
    expect(mergeMentionEntities(existing, "山路蜿蜒", [])).toEqual(existing);
  });
});

describe("组详情归一", () => {
  const resp: GroupResp = {
    id: "g1", projectId: "p1", canvasNodeId: "n_g", groupType: "storyboard_image", scriptId: "sc1", name: "分镜图组",
    status: "running", version: 3,
    items: [
      { id: "i1", itemNo: 1, shotId: "s1", generationId: "gen1", status: "success", params: { aspect_ratio: "16:9" } },
      { id: "i2", itemNo: 2, shotId: "s2", generationId: "gen2", status: "queued", errorMessage: "" }
    ]
  };

  it("新契约 id → groupId / itemId，镜号与景别由工作台补上（新后端的子项不带）", () => {
    const group = normalizeGroupDetail(resp, { shots: [shot("s1", { shotNo: 5, shotSize: "特写" })] });
    expect(group).toMatchObject({ groupId: "g1", canvasNodeId: "n_g", scriptId: "sc1", version: 3 });
    expect(group.items[0]).toMatchObject({ itemId: "i1", shotId: "s1", generationId: "gen1", outputType: "image" });
    expect(group.items[0]!.paramsSnapshot).toEqual({ aspect_ratio: "16:9", shotNo: 5, shotSize: "特写" });
  });

  it("刷新时沿用上一次的镜号与产物地址；换了一次生成（重试）就不再显示上一次的图", () => {
    const first = normalizeGroupDetail(resp, { shots: [shot("s1", { shotNo: 5 })] });
    first.items[0]!.outputUrl = "/api/v1/media/77";
    first.items[1]!.outputUrl = "/api/v1/media/78";
    const retried: GroupResp = { ...resp, items: [resp.items![0]!, { ...resp.items![1]!, generationId: "gen3" }] };
    const next = normalizeGroupDetail(retried, { previous: first });
    expect(next.items[0]!.paramsSnapshot).toMatchObject({ shotNo: 5 });
    expect(next.items[0]!.outputUrl).toBe("/api/v1/media/77");
    expect(next.items[1]!.outputUrl).toBeNull();
  });

  it("资产子项的产物就是资产图", () => {
    const group = normalizeGroupDetail(
      { ...resp, groupType: "asset", items: [{ id: "i9", itemNo: 1, assetId: "a1", status: "success" }] },
      { assets: [{ id: "a1", assetType: "scene", name: "白骨洞", imageUrl: "/api/v1/media/5", status: "ready", progress: 0, version: 1, shotIds: [] }] }
    );
    expect(group.items[0]).toMatchObject({ assetId: "a1", outputUrl: "/api/v1/media/5" });
    expect(group.items[0]!.paramsSnapshot).toMatchObject({ name: "白骨洞", assetType: "scene" });
  });
});

describe("组节点里存坏了的组（CD-81）", () => {
  // 早先轮询把接口原样写进了节点：没有 groupId，子项只有 id / itemNo
  const raw = {
    groupId: undefined,
    items: [{ id: "i1", itemNo: 1, shotId: "s1", status: "queued" }, { id: "i2", itemNo: 2, shotId: "s2", status: "queued" }]
  };

  it("groupId 为空或有子项缺 itemId 就要修（缺 itemId 时「选中哪一项」对每一项都成立）", () => {
    expect(groupNeedsRepair(raw)).toBe(true);
    expect(groupNeedsRepair({ groupId: "g1", items: raw.items })).toBe(true);
    expect(groupNeedsRepair({ groupId: "", items: [] })).toBe(true);
    expect(groupNeedsRepair({ groupId: "g1", items: [{ itemId: "i1" }, null] })).toBe(true);
  });

  it("归一化过的组不用修", () => {
    const group = normalizeGroupDetail({
      id: "g1", projectId: "p1", canvasNodeId: "n_g", groupType: "storyboard_image", scriptId: "sc1", status: "running", version: 1,
      items: [{ id: "i1", itemNo: 1, shotId: "s1", status: "queued" }]
    });
    expect(groupNeedsRepair(group)).toBe(false);
    expect(groupNeedsRepair({ groupId: "g1", items: undefined })).toBe(false);
  });

  it("拿坏了的组当 previous 沿用不到任何标签：缺镜号 / 资产名时要取一次脚本上下文", () => {
    const resp: GroupResp = {
      id: "g1", projectId: "p1", canvasNodeId: "n_g", groupType: "storyboard_image", scriptId: "sc1", status: "running", version: 1,
      items: [{ id: "i1", itemNo: 1, shotId: "s1", status: "queued" }, { id: "i2", itemNo: 2, shotId: "s7", status: "queued" }]
    };
    expect(groupMissingLabels(normalizeGroupDetail(resp))).toBe(true);
    const labelled = normalizeGroupDetail(resp, { shots: [shot("s1", { shotNo: 1 }), shot("s7", { shotNo: 7 })] });
    expect(groupMissingLabels(labelled)).toBe(false);
    expect(labelled.items.map(item => item.paramsSnapshot?.shotNo)).toEqual([1, 7]);
    // 之后的刷新沿用上一次的标签，不必再取
    expect(groupMissingLabels(normalizeGroupDetail(resp, { previous: labelled }))).toBe(false);

    const assetResp: GroupResp = { ...resp, groupType: "asset", items: [{ id: "i9", itemNo: 1, assetId: "a1", status: "success" }] };
    expect(groupMissingLabels(normalizeGroupDetail(assetResp))).toBe(true);
  });
});

describe("组与子项的状态文案", () => {
  it("组状态不把服务端的词直接露给用户；partial_success 单列", () => {
    expect(groupStatusLabel("running")).toBe("执行中");
    expect(groupStatusLabel("partial_success")).toBe("部分完成");
    expect(groupStatusLabel("failed")).toBe("失败");
    // 词表外的值原样显示：配置可以比前端新
    expect(groupStatusLabel("archived")).toBe("archived");
  });

  it("子项状态含取消与镜头已修改", () => {
    expect(groupItemStatusLabel("queued")).toBe("排队中");
    expect(groupItemStatusLabel("cancelled")).toBe("已停止");
    expect(groupItemStatusLabel("stale")).toBe("镜头已修改");
    expect(groupItemStatusLabel(undefined)).toBe("待生成");
  });
});

describe("组子项的参考图（CD-82）", () => {
  const assets = [
    { id: "a1", assetType: "character", name: "唐僧", mediaId: "m1", status: "ready", progress: 0, version: 1, shotIds: [] },
    { id: "a2", assetType: "scene", name: "白骨洞", mediaId: "m2", status: "ready", progress: 0, version: 1, shotIds: [] }
  ];

  it("新后端的输入只带 ID：按 order 编号、认出是哪个资产，地址走受保护的媒体端点", () => {
    // 故意乱序：显示的编号必须与提示词里的「图N」同一套（按 order），不能按数组下标
    const refs = groupItemReferences([
      { kind: "reference", order: 2, referenceId: "m2" },
      { kind: "reference", order: 1, referenceId: "m1" },
      { kind: "text", order: 3, text: "补充描述" }
    ], assets);
    expect(refs.map(ref => [ref.index, ref.name, ref.assetType, ref.url])).toEqual([
      [1, "唐僧", "character", "/api/v1/media/m1"],
      [2, "白骨洞", "scene", "/api/v1/media/m2"]
    ]);
  });

  it("上游产物（视频组的首帧）要另问地址；认不出的媒体照样列出，只是没有名字", () => {
    const refs = groupItemReferences([
      { kind: "generation", order: 1, generationId: "g9" },
      { kind: "reference", order: 2, referenceId: "m-unknown" }
    ], assets);
    expect(refs[0]).toMatchObject({ index: 1, generationId: "g9", url: "", name: "" });
    expect(refs[1]).toMatchObject({ index: 2, mediaId: "m-unknown", url: "/api/v1/media/m-unknown", name: "" });
  });

  it("老形状（旧画布里的 type:image）照样认；没有 ID 的、不是数组的不出错", () => {
    expect(groupItemReferences([{ type: "image", referenceId: "m1" }], assets)[0]).toMatchObject({ name: "唐僧" });
    expect(groupItemReferences([{ kind: "reference" }, null, "x"], assets)).toEqual([]);
    expect(groupItemReferences(undefined, assets)).toEqual([]);
  });
});

describe("选了却没进组的镜头", () => {
  it("服务端跳过的镜头要说出来（缺资产设定图或提示词没就绪），按镜号升序", () => {
    const shots = [shot("s1", { shotNo: 1 }), shot("s2", { shotNo: 2 }), shot("s3", { shotNo: 3 }), shot("s4", { shotNo: 4 })];
    const group = { items: [{ shotId: "s2" }, { shotId: "s4" }] as never[] };
    expect(shotsLeftOut(["s3", "s1", "s2"], shots, group)).toEqual([1, 3]);
    expect(shotsLeftOut(["s2"], shots, group)).toEqual([]);
  });
});
