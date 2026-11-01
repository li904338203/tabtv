// 输入框里技能（/）与 @ 资源弹层的摆放：默认向上弹，上方放不下就向下弹，
// 两边都放不下时取空间大的一侧并把高度压到可用空间内。
//
// 起因：弹层原先固定向上、高度写死。输入框在页面靠上的位置时（创造页滚动后、
// 画布右侧窄面板），弹层会钻到顶栏下面被裁掉一截，用户看不到上半部分的选项。

export type PickerSide = "top" | "bottom";

export interface PickerPlacement {
  side: PickerSide;
  /** 弹层最大高度（px），已扣掉间距与安全边距。 */
  maxHeight: number;
}

export interface PickerSpace {
  /** 输入框上沿到可视区上界（顶栏下沿）的距离。 */
  above: number;
  /** 输入框下沿到视口底部的距离。 */
  below: number;
  /** 弹层理想高度。 */
  preferred: number;
  /** 弹层与输入框的间距 + 离边缘的安全距离。 */
  gap?: number;
  /** 再小就没法用了的高度：向上放得下这个高度就继续向上（避免来回翻）。 */
  minimum?: number;
}

export function pickerPlacement({ above, below, preferred, gap = 16, minimum = 160 }: PickerSpace): PickerPlacement {
  const up = Math.max(0, Math.floor(above - gap));
  const down = Math.max(0, Math.floor(below - gap));
  if (up >= preferred) return { side: "top", maxHeight: preferred };
  if (down >= preferred) return { side: "bottom", maxHeight: preferred };
  // 两边都放不下完整高度：上方还够用就留在上方（与习惯一致），否则去空间大的一侧。
  if (up >= minimum && up >= down) return { side: "top", maxHeight: up };
  if (down >= up) return { side: "bottom", maxHeight: Math.max(down, Math.min(minimum, preferred)) };
  return { side: "top", maxHeight: Math.max(up, Math.min(minimum, preferred)) };
}
