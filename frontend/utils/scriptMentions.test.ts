import { describe, expect, it } from "vitest";
import { inferScriptMentionType, pickMentionSuggestions, segmentScriptMentions } from "./scriptMentions";

describe("script mention rendering", () => {
  it("uses a known asset name without consuming adjacent prose", () => {
    const segments = segmentScriptMentions("@唐僧彻底失控，指着@孙悟空。", [
      { id: "1", name: "唐僧", assetType: "character" },
      { id: "2", name: "孙悟空", assetType: "character" }
    ]);

    expect(segments.filter(segment => segment.mention).map(segment => segment.name)).toEqual(["唐僧", "孙悟空"]);
    expect(segments.map(segment => segment.text).join("")).toBe("@唐僧彻底失控，指着@孙悟空。");
  });

  it("respects explicit asset types", () => {
    const [mention] = segmentScriptMentions("@道具:金箍棒 落在地面。", []);
    expect(mention).toMatchObject({ mention: true, name: "金箍棒", type: "prop" });
  });

  it("infers scenes and props from their context", () => {
    expect(inferScriptMentionType("白虎岭荒野", "众人来到@白虎岭荒野。", 4)).toBe("scene");
    expect(inferScriptMentionType("金箍棒", "悟空手持@金箍棒。", 4)).toBe("prop");
  });
});

describe("@ 候选按类型分配名额", () => {
  const assets = [
    ...Array.from({ length: 9 }, (_, index) => ({
      id: `c${index}`, name: `角色${index}`, assetType: "character"
    })),
    { id: "s1", name: "白虎岭", assetType: "scene" },
    { id: "p1", name: "金箍棒", assetType: "prop" },
    { id: "p2", name: "九齿钉耙", assetType: "prop" }
  ];

  it("角色再多也不能把场景和道具挤没", () => {
    // 资产按 asset_type 排序取回，整体截断前 N 个会全是角色 —— 用户会以为只支持人物
    const picked = pickMentionSuggestions(assets, "");
    const types = new Set(picked.map(asset => asset.assetType));

    expect(types).toEqual(new Set(["character", "scene", "prop"]));
    expect(picked.filter(asset => asset.assetType === "character")).toHaveLength(6);
    expect(picked.map(asset => asset.name)).toContain("白虎岭");
    expect(picked.map(asset => asset.name)).toContain("九齿钉耙");
  });

  it("按顺序分组：人物 → 场景 → 道具", () => {
    const picked = pickMentionSuggestions(assets, "", 2);

    expect(picked.map(asset => asset.assetType))
      .toEqual(["character", "character", "scene", "prop", "prop"]);
  });

  it("关键词跨类型过滤", () => {
    expect(pickMentionSuggestions(assets, "虎").map(asset => asset.name)).toEqual(["白虎岭"]);
    expect(pickMentionSuggestions(assets, "耙").map(asset => asset.name)).toEqual(["九齿钉耙"]);
  });
});

describe("本镜实体的无 @ 字样也按提及高亮（CD-78）", () => {
  it("分镜契约漏了 @ 那段时间生成的描述：本镜实体的名字照样高亮、类型取实体的", () => {
    const segments = segmentScriptMentions("远景：唐僧骑马缓行，白骨洞外阴风阵阵", [], [
      { name: "唐僧", type: "character" },
      { name: "白骨洞", type: "scene" }
    ]);
    expect(segments.filter(segment => segment.mention).map(segment => [segment.name, segment.type]))
      .toEqual([["唐僧", "character"], ["白骨洞", "scene"]]);
    // 原文一个字不丢
    expect(segments.map(segment => segment.text).join("")).toBe("远景：唐僧骑马缓行，白骨洞外阴风阵阵");
  });

  it("只认本镜自己的实体：别的名字照旧是普通文字；带 @ 的照旧按 @ 解析", () => {
    const segments = segmentScriptMentions("@孙悟空 看着唐僧", [{ id: "a1", name: "孙悟空", assetType: "character" }], []);
    expect(segments.filter(segment => segment.mention).map(segment => [segment.name, segment.assetId])).toEqual([["孙悟空", "a1"]]);
  });

  it("名字有包含关系时取长的（「白骨精·老妇」不被拆成「白骨精」）", () => {
    const segments = segmentScriptMentions("白骨精·老妇提着篮子", [], [
      { name: "白骨精", type: "character" },
      { name: "白骨精·老妇", type: "character" }
    ]);
    expect(segments.filter(segment => segment.mention).map(segment => segment.name)).toEqual(["白骨精·老妇"]);
  });
});
