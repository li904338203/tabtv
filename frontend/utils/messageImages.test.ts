import { describe, expect, it } from "vitest";
import { userMessageImages } from "./messageImages";

const scene = {
  products: ["m1", "m2"],
  characters: [
    { kind: "media" as const, id: "c-main", characterId: "lib-c1", name: "小美" },
    { kind: "asset" as const, id: "asset-9" },
  ],
};

describe("userMessageImages", () => {
  it("商品库商品：显示会话快照里的全部商品图（引用里存的是商品库 ID，不是图）", () => {
    const refs = [{ kind: "marketing_product", id: "lib-p1", slot: "product", label: "裙子", order: 1 }];
    expect(userMessageImages(refs, scene)).toEqual([
      { key: "m1", mediaId: "m1", label: "裙子 · 第 1 张" },
      { key: "m2", mediaId: "m2", label: "裙子 · 第 2 张" },
    ]);
  });

  it("角色库角色：按 characterId 对上快照里的主图；资产角色不显示", () => {
    const refs = [{ kind: "marketing_character", id: "lib-c1", slot: "character", label: "" }, { kind: "asset", id: "asset-9" }];
    expect(userMessageImages(refs, scene)).toEqual([{ key: "c-main", mediaId: "c-main", label: "小美" }]);
  });

  it("素材：只显示图片；视频 / 节点不显示；同一张不重复", () => {
    const refs = [
      { kind: "media", id: "img-1", slot: "product", mediaKind: "image" },
      { kind: "media", id: "vid-1", slot: "source_video", mediaKind: "video" },
      { kind: "node", id: "n1", label: "节点" },
      { kind: "media", id: "img-1", mediaKind: "image" },
      { kind: "media", id: "old-1" },
    ];
    expect(userMessageImages(refs, null).map(i => [i.mediaId, i.label])).toEqual([["img-1", "商品图"], ["old-1", "素材"]]);
  });

  it("会话快照还没加载 / 引用不是数组：什么都不显示，不报错", () => {
    expect(userMessageImages([{ kind: "marketing_product", id: "lib-p1" }], undefined)).toEqual([]);
    expect(userMessageImages(null, scene)).toEqual([]);
    expect(userMessageImages([null, "x", 1], scene)).toEqual([]);
  });
});
