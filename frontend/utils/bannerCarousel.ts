import type { components } from '~/types/legacy';
import type { components as ApiComponents } from "@tabtv/api-client";

export type HomeBanner = components["schemas"]["BannerResp"];
/** 新后端 `/home` 下发的 Banner（生成的契约类型，不手写）。 */
export type ApiHomeBanner = ApiComponents["schemas"]["HomeBanner"];

/**
 * 这条 Banner 显示哪张图：有封面用封面，否则用媒体本身（目前只有图片轮播）。
 * 返回的是**媒体 ID**，取图要走受保护端点（`<img src>` 不带鉴权头）。
 */
export function bannerImageId(banner: Pick<ApiHomeBanner, "coverMediaId" | "mediaObjectId">): string | undefined {
  return banner.coverMediaId || banner.mediaObjectId || undefined;
}

/**
 * 新契约 → 轮播组件期望的形状（`cover` / `link` / `mediaType`）。
 *
 * ⚠️ 此前页面直接读 `cover` / `link` —— 那是老后端的字段，新后端**从来不发**，
 *    于是后台配好的 Banner 在首页一张图都没有、点了也不跳。这里就地补形状而不改模板
 *    （同首页 `recentItems` 的做法）。
 * ⚠️ **图还没取到（或取不到）的不进轮播**：一张裂图占着首页最显眼的位置，
 *    比少一条 Banner 更糟；取回来之后它自然出现。
 */
export function toCarouselBanners(items: ApiHomeBanner[], imageUrls: Record<string, string>): HomeBanner[] {
  return items.flatMap((banner) => {
    const imageId = bannerImageId(banner);
    const cover = imageId ? imageUrls[imageId] : undefined;
    if (!cover) return [];
    return [{ id: banner.id, title: banner.title, link: banner.linkUrl || undefined, cover, mediaType: "image" }];
  });
}
export type BannerPosition = "previous" | "current" | "next";

export interface PositionedBanner {
  banner: HomeBanner;
  position: BannerPosition;
}

export function bannerTriplet(items: HomeBanner[], activeIndex: number): PositionedBanner[] {
  if (!items.length) return [];
  const current = ((activeIndex % items.length) + items.length) % items.length;
  if (items.length === 1) return [{ banner: items[current], position: "current" }];
  return [
    { banner: items[(current - 1 + items.length) % items.length], position: "previous" },
    { banner: items[current], position: "current" },
    { banner: items[(current + 1) % items.length], position: "next" }
  ];
}

export function isSafeHomeBannerLink(value?: string) {
  if (!value) return false;
  if (value.startsWith("/") && !value.startsWith("//")) return true;
  try {
    return new URL(value).protocol === "https:";
  } catch {
    return false;
  }
}

export function isVideoBanner(item?: HomeBanner) {
  return item?.mediaType === "video" && Boolean(item.videoUrl);
}
