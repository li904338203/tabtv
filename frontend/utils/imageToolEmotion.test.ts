import { describe, expect, it } from "vitest";
import { readdirSync, readFileSync, statSync } from "node:fs";
import { join } from "node:path";
import type { ImageToolEditorRole } from "./imageTools";
import { IMAGE_TOOL_FALLBACK_CONFIG, imageToolEmotionAt, imageToolWireOptions } from "./imageTools";
import {
  imageToolEmotionAddRole,
  imageToolEmotionCount,
  imageToolEmotionFaceCrop,
  imageToolEmotionHeadFallback,
  imageToolEmotionInputHint,
  imageToolEmotionMove,
  imageToolEmotionOptions,
  imageToolEmotionOrdered,
  imageToolEmotionPickSelected,
  imageToolEmotionQuoteOptions,
  imageToolEmotionRemoveRole,
  imageToolEmotionRestore,
  imageToolEmotionSetRole,
  imageToolEmotionSourceChanged
} from "./imageToolEmotion";

const role = (id: string, x: number, extra: Partial<ImageToolEditorRole> = {}): ImageToolEditorRole => ({
  id, box: { x, y: 10, w: 100, h: 200 }, arousal: 0, warmth: 0, touched: false, ...extra
});

describe("坐标盘方向键", () => {
  it("上激动下平静、左亲近右疏离，到边不绕回", () => {
    expect(imageToolEmotionMove({ arousal: 0, warmth: 0 }, "ArrowUp")).toEqual({ arousal: 1, warmth: 0 });
    expect(imageToolEmotionMove({ arousal: 0, warmth: 0 }, "ArrowDown")).toEqual({ arousal: -1, warmth: 0 });
    expect(imageToolEmotionMove({ arousal: 0, warmth: 0 }, "ArrowLeft")).toEqual({ arousal: 0, warmth: 1 });
    expect(imageToolEmotionMove({ arousal: 0, warmth: 0 }, "ArrowRight")).toEqual({ arousal: 0, warmth: -1 });
    expect(imageToolEmotionMove({ arousal: 2, warmth: -2 }, "ArrowUp")).toEqual({ arousal: 2, warmth: -2 });
    expect(imageToolEmotionMove({ arousal: 2, warmth: -2 }, "ArrowRight")).toEqual({ arousal: 2, warmth: -2 });
    expect(imageToolEmotionMove({ arousal: 2, warmth: -2 }, "Home")).toEqual({ arousal: 0, warmth: 0 });
    expect(imageToolEmotionMove({ arousal: 0, warmth: 0 }, "Tab")).toBeNull();
  });

  it("左上角是欣喜若狂、右下角是心灰意冷（与服务端表同向）", () => {
    const emotions = IMAGE_TOOL_FALLBACK_CONFIG.emotions;
    let cell = { arousal: 0, warmth: 0 };
    for (const key of ["ArrowUp", "ArrowUp", "ArrowLeft", "ArrowLeft"]) cell = imageToolEmotionMove(cell, key)!;
    expect(imageToolEmotionAt(emotions, cell.arousal, cell.warmth)?.name).toBe("欣喜若狂");
    for (const key of ["ArrowDown", "ArrowDown", "ArrowDown", "ArrowDown", "ArrowRight", "ArrowRight", "ArrowRight", "ArrowRight"]) cell = imageToolEmotionMove(cell, key)!;
    expect(imageToolEmotionAt(emotions, cell.arousal, cell.warmth)?.name).toBe("心灰意冷");
  });
});

describe("角色增删改", () => {
  it("设坐标即标 touched，只改那一个", () => {
    const roles = imageToolEmotionSetRole([role("a", 0), role("b", 200)], "b", 2, -1);
    expect(roles[0]).toMatchObject({ touched: false, arousal: 0 });
    expect(roles[1]).toMatchObject({ touched: true, arousal: 2, warmth: -1 });
    expect(imageToolEmotionSetRole(roles, "b", 9, -9)[1]).toMatchObject({ arousal: 2, warmth: -2 });
  });

  it("手动添加：未调整、manual- 前缀；满 6 个拒绝并说明", () => {
    const added = imageToolEmotionAddRole([role("a", 0)], { x: 5, y: 5, w: 40, h: 40 }, "1");
    expect(added.error).toBeNull();
    expect(added.id).toBe("manual-1");
    expect(added.roles[1]).toEqual({ id: "manual-1", box: { x: 5, y: 5, w: 40, h: 40 }, touched: false, arousal: 0, warmth: 0 });
    const six = Array.from({ length: 6 }, (_, i) => role(`r${i}`, i * 10));
    const full = imageToolEmotionAddRole(six, { x: 5, y: 5, w: 40, h: 40 }, "7");
    expect(full.error).toContain("6");
    expect(full.roles).toHaveLength(6);
  });

  it("删除后选中项：原来的还在就不动，否则选最左边的", () => {
    const roles = [role("right", 300), role("left", 0)];
    expect(imageToolEmotionPickSelected(roles, "right")).toBe("right");
    expect(imageToolEmotionPickSelected(imageToolEmotionRemoveRole(roles, "right"), "right")).toBe("left");
    expect(imageToolEmotionPickSelected([], "x")).toBe("");
    expect(imageToolEmotionOrdered(roles).map(item => item.id)).toEqual(["left", "right"]);
  });
});

describe("节点参数还原与源图变化", () => {
  it("存下的角色都还原成已调整的框；老节点的 resolution 忽略", () => {
    const state = imageToolEmotionRestore({
      sourceMediaId: "9", resolution: "1k", count: 3,
      roles: [{ id: "p2", box: { x: 600, y: 10, w: 100, h: 200 }, arousal: 2, warmth: -2 }, { id: "m", box: { x: 1, y: 1, w: 30, h: 30 }, arousal: -1, warmth: 1 }]
    });
    expect(state).not.toHaveProperty("resolution");
    expect(state.count).toBe(3);
    expect(state.roles.map(item => [item.id, item.touched])).toEqual([["p2", true], ["m", true]]);
  });

  it("坏值不打崩面板：张数夹到 1~4，丢掉没有框的角色，坐标夹到范围", () => {
    const state = imageToolEmotionRestore({ count: 99, roles: [{ id: "x" }, { box: { x: 0, y: 0, w: 30, h: 30 }, arousal: 7, warmth: "a" }] });
    expect(state.count).toBe(4);
    expect(state.roles).toHaveLength(1);
    expect(state.roles[0]).toMatchObject({ arousal: 2, warmth: 0, touched: true });
    expect(imageToolEmotionRestore(undefined).roles).toEqual([]);
    expect(imageToolEmotionCount("x")).toBe(1);
    expect(imageToolEmotionCount(0)).toBe(1);
  });

  it("源图变了才判变化；任一侧未知不判", () => {
    expect(imageToolEmotionSourceChanged({ toolOptions: { sourceMediaId: "1" } }, "2")).toBe(true);
    expect(imageToolEmotionSourceChanged({ toolOptions: { sourceMediaId: "1" } }, "1")).toBe(false);
    expect(imageToolEmotionSourceChanged({ toolSourceMediaId: "1", toolOptions: {} }, "2")).toBe(true);
    expect(imageToolEmotionSourceChanged({ toolOptions: {} }, "2")).toBe(false);
    expect(imageToolEmotionSourceChanged({ toolOptions: { sourceMediaId: "1" } }, undefined)).toBe(false);
  });
});

describe("请求与报价选项", () => {
  const state = { count: 2, roles: [role("b", 300), role("a", 0, { touched: true, arousal: 1, warmth: 2 })] };

  it("发送只带 touched 的角色，且没有界面字段", () => {
    const options = imageToolEmotionOptions("9", state);
    expect(options).not.toHaveProperty("resolution");
    expect(options.roles).toEqual([{ id: "a", box: { x: 0, y: 10, w: 100, h: 200 }, arousal: 1, warmth: 2 }]);
    expect(imageToolWireOptions("expression_edit", options)).toEqual(options);
  });

  it("报价：有 touched 用 touched；没有时借最左的角色只为算积分；没有角色不报价", () => {
    expect(imageToolEmotionQuoteOptions("9", state)?.roles.map(item => item.id)).toEqual(["a"]);
    const untouched = { ...state, roles: [role("b", 300), role("a", 0)] };
    expect(imageToolEmotionQuoteOptions("9", untouched)?.roles.map(item => item.id)).toEqual(["a"]);
    expect(imageToolEmotionQuoteOptions("9", { ...state, roles: [] })).toBeNull();
    expect(imageToolEmotionQuoteOptions("", state)).toBeNull();
  });

  it("发送前置条件的说明", () => {
    expect(imageToolEmotionInputHint([])).toBe("先在画面上框选要调整表情的人");
    expect(imageToolEmotionInputHint([role("a", 0)])).toBe("先选一个角色的情绪");
    expect(imageToolEmotionInputHint([role("a", 0, { touched: true })])).toBe("");
  });
});

describe("脸部小图裁切", () => {
  it("竖长框取顶部、按缩略图比例换算", () => {
    const crop = imageToolEmotionFaceCrop({ x: 100, y: 100, w: 200, h: 600 }, { width: 2000, height: 1000 }, { width: 640, height: 320 })!;
    expect(crop.side).toBeCloseTo(140 * 0.32);
    expect(crop.sx).toBeCloseTo((200 * 0.32) - crop.side / 2);
    expect(crop.sy).toBeCloseTo(112 * 0.32);
  });

  it("方形框居中取短边；夹到图内；尺寸未知返回 null", () => {
    const crop = imageToolEmotionFaceCrop({ x: 950, y: 950, w: 100, h: 100 }, { width: 1000, height: 1000 }, { width: 1000, height: 1000 })!;
    expect(crop).toEqual({ sx: 900, sy: 900, side: 100 });
    expect(imageToolEmotionFaceCrop({ x: 0, y: 0, w: 10, h: 10 }, { width: 0, height: 0 }, { width: 10, height: 10 })).toBeNull();
  });
});

describe("示意头像产物", () => {
  const dir = join(__dirname, "..", "public", "imagetools", "emotion");

  it("25 张齐全、每张 ≤ 6KB，路径与 fallback 一致", () => {
    const files = readdirSync(dir).filter(name => name.endsWith(".svg")).sort();
    expect(files).toHaveLength(25);
    for (let a = -2; a <= 2; a += 1) {
      for (let w = -2; w <= 2; w += 1) {
        const name = imageToolEmotionHeadFallback(a, w).split("/").pop()!;
        expect(files).toContain(name);
        expect(statSync(join(dir, name)).size).toBeLessThanOrEqual(6 * 1024);
        expect(readFileSync(join(dir, name), "utf8").startsWith("<svg")).toBe(true);
      }
    }
  });
});

describe("情绪编辑器源码约束（2026-10-31 方案变更）", () => {
  const editor = readFileSync(join(__dirname, "..", "components", "canvas", "imagetools", "EmotionEditor.vue"), "utf8");
  const canvas = readFileSync(join(__dirname, "..", "components", "canvas", "imagetools", "EmotionFaceCanvas.vue"), "utf8");

  it("没有自动人物识别与分辨率选项", () => {
    for (const source of [editor, canvas]) {
      expect(source).not.toMatch(/\/people|detectPeople|重试识别|正在识别|resolution|分辨率"/);
    }
  });

  it("默认进「画面」页签；空态提示框选", () => {
    expect(editor).toMatch(/const view = ref<"head" \| "image">\("image"\)/);
    expect(canvas).toContain("在画面上框选要调整表情的人");
  });
});
