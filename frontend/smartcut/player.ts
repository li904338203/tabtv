/**
 * 整页只有一个活的智能剪辑播放器（SCR-4，dev/03 §4.1）：节点卡上的成片播放、剪辑台的预览，后开始的那个让前一个停下并释放。
 *
 * 浏览器对同时播放的媒体元素有限制（解码器数量、自动播放策略），两个都出声时用户也听不清。剪辑台的引擎自己还有一层
 * （`PlaybackEngine` 的 `playback.active`），这里管的是「节点卡的 <video> 与剪辑台之间」。
 */
type Stop = () => void;

let active: { token: symbol; stop: Stop } | null = null;

/** 开始播放前调用：先停掉别的播放器。返回释放函数（停播、卸载时调用；已经被别人顶掉时调用是无害的）。 */
export function smartcutClaimPlayer(stop: Stop): () => void {
  const previous = active;
  const token = Symbol("smartcut-player");
  active = { token, stop };
  if (previous) {
    try {
      previous.stop();
    } catch {
      // 前一个播放器已经卸载：停不了也无所谓
    }
  }
  return () => {
    if (active?.token === token) active = null;
  };
}
