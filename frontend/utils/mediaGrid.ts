/**
 * 媒体卡片网格的共用规则（MKT-T-15，营销 dev/03 §5「复用」；B-7）。
 *
 * 精选作品墙（`components/showcase/ShowcaseGrid.vue`）与营销模板墙（`components/marketing/TemplateWall.vue`）
 * 是同一种东西：按断点排列的封面卡 + 悬停静音预览。此前断点与悬停播放写在 ShowcaseGrid 里，
 * 模板墙若再抄一份，两面墙迟早一个 4 列一个 3 列 —— 而两边各自看都「没错」。
 *
 * ⚠️ 断点最终落在两个组件的 CSS `@media` 里（CSS 读不到 JS 常量）；`mediaGrid.test.ts` 读两个组件的源码，
 *    逐条核对它们的 `@media (min-width: …)` 与这里的 `MEDIA_GRID_BREAKPOINTS` 一致 —— 改了一边不改另一边会红。
 */

/** 断点（从宽到窄）：≥1440 4 列、≥1024 3 列、≥640 2 列，更窄 1 列。 */
export const MEDIA_GRID_BREAKPOINTS: ReadonlyArray<{ minWidth: number; columns: number }> = [
  { minWidth: 1440, columns: 4 },
  { minWidth: 1024, columns: 3 },
  { minWidth: 640, columns: 2 }
];

/** 给定视口宽度时的列数（与 CSS 同一组断点；边界取 `>=`，同 `min-width`）。 */
export function mediaGridColumns(viewportWidth: number): number {
  for (const bp of MEDIA_GRID_BREAKPOINTS) {
    if (viewportWidth >= bp.minWidth) return bp.columns;
  }
  return 1;
}

/**
 * 悬停播放只在「能悬停的精确指针」设备上启用（营销 dev/03 §9）：触屏没有悬停，
 * 一次点按会先触发 mouseenter 再触发 click —— 视频开始播的同时卡片的点击动作也发生了，两件事抢同一个手势。
 * 传入 `matchMedia`（测试里给假的）；取不到时不启用。
 */
export function canHoverPlay(matchMedia: ((query: string) => { matches: boolean }) | undefined): boolean {
  if (!matchMedia) return false;
  try {
    return matchMedia("(hover: hover) and (pointer: fine)").matches;
  } catch {
    return false;
  }
}

/** 悬停预览要用到的那一点视频元素能力（测试里给假的）。 */
export type HoverVideo = { play(): Promise<void> | void; pause(): void; currentTime: number };

/**
 * 开始悬停预览：静音由 `<video muted>` 保证。
 * ⚠️ `play()` 的拒绝必须吞掉：鼠标快速划过时 pause 会打断还没开始的 play，浏览器以 AbortError 拒绝 —— 那不是错误。
 */
export function startHoverPreview(video: HoverVideo): void {
  try {
    const pending = video.play();
    if (pending && typeof (pending as Promise<void>).catch === "function") void (pending as Promise<void>).catch(() => undefined);
  } catch {
    // 某些环境 play() 同步抛出（如元素已卸载）：悬停预览是锦上添花，静默放弃。
  }
}

/**
 * 离开即停并回到封面帧。`resetTo` 是封面帧时刻：作品墙用 `#t=0.1` 取首帧当封面，所以回到 0.1；
 * 模板墙有独立封面图、视频只在悬停时盖上去，回到 0。
 */
export function stopHoverPreview(video: HoverVideo, resetTo = 0): void {
  video.pause();
  try {
    video.currentTime = resetTo;
  } catch {
    // 元数据还没到时设 currentTime 会抛：下次悬停从头播即可。
  }
}

/** 三联封面最多几张（MKT-F-31）。 */
export const MAX_COVER_PANELS = 3;

/**
 * 三联封面画几格、取哪几张：`coverCount` 张，不足三张**按实际张数等分**（不复制同一张凑满三格、
 * 也不留空格）；超过三张只取前三张。0 张返回空数组，由调用方画占位。
 * `widthPercent` 是每格宽度占比（等分），供样式直接用。
 */
export function coverPanels(coverCount: number, max = MAX_COVER_PANELS): { indexes: number[]; widthPercent: number } {
  const count = Math.max(0, Math.min(Math.floor(Number.isFinite(coverCount) ? coverCount : 0), max));
  if (!count) return { indexes: [], widthPercent: 0 };
  return { indexes: Array.from({ length: count }, (_, i) => i), widthPercent: 100 / count };
}
