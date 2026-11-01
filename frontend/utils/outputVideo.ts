/**
 * Agent 面板里产出视频卡片的尺寸与时长文字（design/03 R-5）。
 *
 * 卡片**按视频真实比例**摆：竖屏 9:16 就是竖的（此前一律是 16:10 的横框，竖屏片子被裁成一条）。
 * 比例取自 `/media/{id}/info` 的宽高（视频的宽高取自 Worker 截的封面帧），都拿不到时按 16:9。
 */

/** 竖屏定宽、横屏定宽、近方形定宽；再用最大高度兜住特别细长的比例。 */
const PORTRAIT_WIDTH = 240;
const LANDSCAPE_WIDTH = 360;
const SQUARE_WIDTH = 280;
const MAX_HEIGHT = 460;

export function outputVideoTileSize(width?: number, height?: number): { width: number; height: number } {
  const ratio = width && height && width > 0 && height > 0 ? width / height : 16 / 9;
  let w = ratio < 0.95 ? PORTRAIT_WIDTH : ratio > 1.05 ? LANDSCAPE_WIDTH : SQUARE_WIDTH;
  if (w / ratio > MAX_HEIGHT) w = MAX_HEIGHT * ratio;
  return { width: Math.round(w), height: Math.round(w / ratio) };
}

/** 「0:30」「1:05」；未知（0 或没给）返回空串，不显示。 */
export function outputVideoDuration(ms?: number): string {
  if (!ms || ms <= 0) return "";
  const total = Math.round(ms / 1000);
  return `${Math.floor(total / 60)}:${String(total % 60).padStart(2, "0")}`;
}
