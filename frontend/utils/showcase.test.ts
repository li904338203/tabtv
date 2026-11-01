import { describe, expect, it } from "vitest";
import {
  SHOWCASE_FILTERS,
  buildShowcaseQuery,
  showcaseCardKind,
  showcaseCoverUrl,
  showcaseErrorText,
  showcaseMediaUrl
} from "./showcase";

describe("showcaseCoverUrl（2026-11-01）", () => {
  it("走公开的封面端点（视频是第一帧，不是整段视频），默认 320 宽", () => {
    expect(showcaseCoverUrl("123", "/api/v1")).toBe("/api/v1/showcase/123/cover?w=320");
    expect(showcaseCoverUrl("123", "/api/v1", 160)).toBe("/api/v1/showcase/123/cover?w=160");
  });
});

describe("showcaseMediaUrl", () => {
  it("走公开端点并拼出 ?w=320", () => {
    expect(showcaseMediaUrl("123", "/api/v1", 320)).toBe("/api/v1/showcase/123/media?w=320");
  });

  it("视频 / 音频不带宽度", () => {
    expect(showcaseMediaUrl("123", "/api/v1")).toBe("/api/v1/showcase/123/media");
  });

  it("不走受保护的 /media 端点", () => {
    expect(showcaseMediaUrl("123", "/api/v1", 320)).not.toMatch(/\/api\/v1\/media\//);
  });
});

describe("buildShowcaseQuery", () => {
  it("不带空 cursor、不带空 type", () => {
    expect(buildShowcaseQuery({ limit: 24, cursor: "", type: "" })).toBe("limit=24");
    expect(buildShowcaseQuery({ limit: 24 })).toBe("limit=24");
  });

  it("带游标", () => {
    expect(buildShowcaseQuery({ limit: 24, cursor: "987" })).toBe("limit=24&cursor=987");
  });

  it("带 type 时原样发给服务端（服务端筛，不是客户端筛）", () => {
    expect(buildShowcaseQuery({ limit: 24, type: "audio" })).toBe("limit=24&type=audio");
    expect(buildShowcaseQuery({ limit: 24, cursor: "9", type: "video" })).toBe("limit=24&cursor=9&type=video");
  });

  it("分类按钮的值就是服务端认的 type（全部 = 不带）", () => {
    expect(SHOWCASE_FILTERS.map(f => f.value)).toEqual(["", "video", "image", "audio"]);
    expect(SHOWCASE_FILTERS.map(f => f.label)).toEqual(["全部", "视频", "图片", "音频"]);
  });
});

describe("showcaseCardKind", () => {
  it("缺 type 时按图片渲染（出错由组件回落占位）", () => {
    expect(showcaseCardKind({ hasMedia: true })).toBe("image");
    expect(showcaseCardKind({ hasMedia: true, type: null })).toBe("image");
  });

  it("按服务端给的 type 渲染", () => {
    expect(showcaseCardKind({ hasMedia: true, type: "image" })).toBe("image");
    expect(showcaseCardKind({ hasMedia: true, type: "video" })).toBe("video");
    expect(showcaseCardKind({ hasMedia: true, type: "audio" })).toBe("audio");
  });

  it("other 与认不出的值渲染占位，不是裂图", () => {
    expect(showcaseCardKind({ hasMedia: true, type: "other" })).toBe("none");
    expect(showcaseCardKind({ hasMedia: true, type: "pdf" })).toBe("none");
  });

  it("没有媒体一律占位", () => {
    expect(showcaseCardKind({ hasMedia: false, type: "image" })).toBe("none");
  });
});

describe("showcaseErrorText", () => {
  it("断网说断网", () => {
    expect(showcaseErrorText(new Error("请求失败"), false)).toBe("网络已断开，恢复后点重试");
  });

  it("带出服务端原话", () => {
    expect(showcaseErrorText({ message: "参数错误" }, true)).toBe("精选作品加载失败：参数错误");
    expect(showcaseErrorText(undefined, true)).toBe("精选作品加载失败");
  });
});
