import { describe, expect, it } from "vitest";
import { outputVideoDuration, outputVideoTileSize } from "./outputVideo";

describe("outputVideoTileSize", () => {
  it("按真实比例：竖屏 9:16 是竖的，横屏 16:9 是横的", () => {
    expect(outputVideoTileSize(480, 854)).toEqual({ width: 240, height: 427 });
    expect(outputVideoTileSize(1280, 720)).toEqual({ width: 360, height: 203 });
    expect(outputVideoTileSize(1080, 1080)).toEqual({ width: 280, height: 280 });
  });

  it("特别细长的比例用最大高度兜住，比例不变", () => {
    const size = outputVideoTileSize(300, 1200);
    expect(size.height).toBe(460);
    expect(size.width / size.height).toBeCloseTo(0.25, 2);
  });

  it("宽高未知时按 16:9", () => {
    expect(outputVideoTileSize()).toEqual({ width: 360, height: 203 });
    expect(outputVideoTileSize(0, 0)).toEqual({ width: 360, height: 203 });
  });
});

describe("outputVideoDuration", () => {
  it("分:秒，秒补两位；未知不显示", () => {
    expect(outputVideoDuration(30080)).toBe("0:30");
    expect(outputVideoDuration(65_000)).toBe("1:05");
    expect(outputVideoDuration(0)).toBe("");
    expect(outputVideoDuration(undefined)).toBe("");
  });
});
