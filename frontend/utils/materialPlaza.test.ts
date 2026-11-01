import { describe, expect, it } from "vitest";
import {
  canvasCameraMove,
  materialPlazaCoverUrl,
  materialPlazaEmptyText,
  materialPlazaQuery
} from "./materialPlaza";

describe("素材广场", () => {
  it("查询参数按新后端的名字给，空关键词不带", () => {
    expect(materialPlazaQuery("  ", false)).toEqual({});
    expect(materialPlazaQuery(" 推 ", true)).toEqual({ keyword: "推", favorited: "true" });
  });

  it("空状态把「没收藏」「搜不到」「目录是空的」分开说", () => {
    expect(materialPlazaEmptyText("camera", { keyword: "旋转", favorites: true })).toBe("没有找到「旋转」");
    expect(materialPlazaEmptyText("camera", { keyword: "", favorites: true })).toContain("还没有收藏的运镜");
    expect(materialPlazaEmptyText("effect", { keyword: "", favorites: false })).toContain("后台「风格与特效」");
  });

  it("封面按列表缩略图取，已经带参数的不再追加", () => {
    expect(materialPlazaCoverUrl("/api/v1/media/12")).toBe("/api/v1/media/12?w=320");
    expect(materialPlazaCoverUrl("/api/v1/media/12?w=160")).toBe("/api/v1/media/12?w=160");
    expect(materialPlazaCoverUrl(undefined)).toBeUndefined();
  });

  it("节点上的运镜：对象与字符串都认，此前三组选项的老对象按没选处理", () => {
    expect(canvasCameraMove({ code: "push_in", name: "镜头前推" })).toEqual({ code: "push_in", name: "镜头前推" });
    expect(canvasCameraMove("orbit")).toEqual({ code: "orbit", name: "orbit" });
    expect(canvasCameraMove({ movement: "推近", shot: "特写" })).toBeUndefined();
    expect(canvasCameraMove(undefined)).toBeUndefined();
    expect(canvasCameraMove({ code: " ", name: "x" })).toBeUndefined();
  });
});
