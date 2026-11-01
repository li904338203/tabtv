/** 页码条里的一格：页码本身，或者代表被折叠掉那一段的省略号。 */
export type PageItem = number | "gap";

export function totalPageCount(total: number, size: number) {
  if (!Number.isFinite(total) || total <= 0 || size <= 0) return 1;
  return Math.max(1, Math.ceil(total / size));
}

/**
 * 生成页码条：首末页常驻，当前页附近展开，中间折叠成省略号。
 *
 * <p>全列出来在数据量大时会把页头挤爆（425 件作品就是 18 页），
 * 而只显示「上一页/下一页」又会让用户没法跳到末尾。这个折中在任何数据量下宽度都稳定。
 *
 * @param current 当前页，越界时会被夹回合法范围
 */
export function pageItems(current: number, total: number): PageItem[] {
  const pages = totalPageCount(total, 1) && total > 0 ? total : 1;
  const active = Math.min(Math.max(1, current), pages);
  // 七页以内全列：折叠反而比直接显示更占地方，也更难点
  if (pages <= 7) return Array.from({ length: pages }, (_, index) => index + 1);

  const wanted = new Set([1, pages, active, active - 1, active + 1]);
  const visible = [...wanted].filter(value => value >= 1 && value <= pages).sort((a, b) => a - b);

  const items: PageItem[] = [];
  visible.forEach((value, index) => {
    const previous = visible[index - 1];
    // 只有真的跳过了页码才插省略号；差值恰好为 2 时直接把中间那页显示出来更有用
    if (index > 0 && previous !== undefined) {
      if (value - previous === 2) items.push(previous + 1);
      else if (value - previous > 2) items.push("gap");
    }
    items.push(value);
  });
  return items;
}
