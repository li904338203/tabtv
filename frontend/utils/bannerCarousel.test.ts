import { describe, expect, it } from "vitest";
import { bannerImageId, bannerTriplet, isSafeHomeBannerLink, isVideoBanner, toCarouselBanners } from "./bannerCarousel";

describe("homepage banner carousel helpers", () => {
  const items = [
    { id: "1", mediaType: "image" },
    { id: "2", mediaType: "video", videoUrl: "/video/2" },
    { id: "3", mediaType: "image" }
  ];

  it("wraps previous and next cards around the active banner", () => {
    expect(bannerTriplet(items, 0).map((item) => item.banner.id)).toEqual(["3", "1", "2"]);
    expect(bannerTriplet(items, 2).map((item) => item.banner.id)).toEqual(["2", "3", "1"]);
  });

  it("renders a single banner only once", () => {
    expect(bannerTriplet([items[0]], 0)).toEqual([{ banner: items[0], position: "current" }]);
  });

  it("accepts only internal and HTTPS destinations", () => {
    expect(isSafeHomeBannerLink("/canvas/9")).toBe(true);
    expect(isSafeHomeBannerLink("https://example.com/news")).toBe(true);
    expect(isSafeHomeBannerLink("//example.com/news")).toBe(false);
    expect(isSafeHomeBannerLink("javascript:alert(1)")).toBe(false);
  });

  it("requires a video URL before treating a banner as video", () => {
    expect(isVideoBanner(items[1])).toBe(true);
    expect(isVideoBanner({ mediaType: "video" })).toBe(false);
  });

  it("prefers the cover image and falls back to the media itself", () => {
    expect(bannerImageId({ coverMediaId: "c", mediaObjectId: "m" })).toBe("c");
    expect(bannerImageId({ coverMediaId: "", mediaObjectId: "m" })).toBe("m");
    expect(bannerImageId({})).toBeUndefined();
  });

  // ⚠️ 新后端发的是 linkUrl / mediaObjectId，老组件读 cover / link —— 不补形状的话首页一张图都没有。
  it("maps the new contract to the carousel shape and skips banners whose image is not loaded", () => {
    const raw = [
      { id: "1", title: "秋季", linkUrl: "/video", mediaObjectId: "m1", sort: 10 },
      { id: "2", title: "没有图", linkUrl: "", sort: 20 },
      { id: "3", title: "图还没取到", linkUrl: "", mediaObjectId: "m3", sort: 30 }
    ];
    expect(toCarouselBanners(raw, { m1: "blob:1" })).toEqual([
      { id: "1", title: "秋季", link: "/video", cover: "blob:1", mediaType: "image" }
    ]);
  });
});
