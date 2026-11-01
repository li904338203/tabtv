/**
 * 整理画布（2026-11-01，画布左下角工具条 / ⌥⇧F）：把节点按连线从左往右分列排整齐。
 * 纯函数，单测见 `canvasTidy.test.ts`。
 *
 * - **连在一起的一组**（连通块）按连线方向分列：上游在左、下游在右。没有上游的节点放在它下游紧左边的那一列，
 *   而不是统统挤在最左列、拉出一根横穿好几列的长线。同一列里按上下游的位置排，尽量让连线拉直、少交叉。
 * - 一组一组从上往下摞，保持原来的上下顺序；**没有任何连线的节点**排成网格放在最下面。
 * - **画框（AGT-11）连同它框住的节点当成一整块**：框里的相对位置不动。拆开排的话，框里的节点会被排到框外面去。
 * - 连线成环（画布上允许，运行前预览才拦）时按原来的左右位置断开，不会死循环。
 * - 排完整体平移回原来的左上角：画布不会「跳走」。
 * - 整理过的再整理一次不会再动（单测钉着）：用户手快多按一次，不该把画布又打乱一遍。
 */

export type TidyNode = {
  id: string;
  x: number;
  y: number;
  width: number;
  height: number;
  /** 画框框住的节点 ID（只有画框给）：画框和这些节点当成一整块挪。 */
  members?: readonly string[];
};

export type TidyEdge = { source: string; target: string };
export type TidyPoint = { x: number; y: number };

/** 相邻两列之间（连线从这里过）。 */
export const TIDY_COLUMN_GAP = 120;
/** 同一列里上下相邻的两块之间。 */
export const TIDY_ROW_GAP = 48;
/** 上下相邻的两组之间。 */
export const TIDY_GROUP_GAP = 120;
/** 没有连线的节点排网格时的间距。 */
export const TIDY_GRID_GAP = 40;

/** 一起挪的一块：单个节点，或者画框连同框里的节点。 */
type Block = {
  ids: string[];
  /** 原来的外框。 */
  x: number;
  y: number;
  width: number;
  height: number;
  /** 排好后外框的左上角（相对排版原点）。 */
  left: number;
  top: number;
};

type Graph = { succ: Set<number>[]; pred: Set<number>[] };

function buildBlocks(nodes: readonly TidyNode[]): { blocks: Block[]; blockOf: Map<string, number> } {
  const byId = new Map(nodes.map(node => [node.id, node]));
  const blockOf = new Map<string, number>();
  const groups: TidyNode[][] = [];
  const claim = (members: TidyNode[]) => {
    for (const member of members) blockOf.set(member.id, groups.length);
    groups.push(members);
  };
  for (const node of nodes) {
    if (!node.members || blockOf.has(node.id)) continue;
    const members = [node];
    for (const id of node.members) {
      const member = byId.get(id);
      // 画框不当别的画框的成员；一个节点被两个画框框住时归先出现的那个
      if (!member || member.members || blockOf.has(id) || members.includes(member)) continue;
      members.push(member);
    }
    claim(members);
  }
  for (const node of nodes) if (!blockOf.has(node.id)) claim([node]);

  const blocks = groups.map((members): Block => {
    const left = Math.min(...members.map(node => node.x));
    const top = Math.min(...members.map(node => node.y));
    const right = Math.max(...members.map(node => node.x + node.width));
    const bottom = Math.max(...members.map(node => node.y + node.height));
    return { ids: members.map(node => node.id), x: left, y: top, width: right - left, height: bottom - top, left: 0, top: 0 };
  });
  return { blocks, blockOf };
}

/** 原来的阅读顺序：先上后下；大致在同一行的（上沿差不到前一块半个高度）再从左到右。 */
function readingOrder(list: readonly number[], blocks: readonly Block[]): number[] {
  const byY = [...list].sort((a, b) => blocks[a].y - blocks[b].y || blocks[a].x - blocks[b].x || a - b);
  const rows: number[][] = [];
  for (const at of byY) {
    const row = rows[rows.length - 1];
    const head = row ? blocks[row[0]] : undefined;
    if (row && head && blocks[at].y < head.y + head.height / 2) row.push(at);
    else rows.push([at]);
  }
  return rows.flatMap(row => row.sort((a, b) => blocks[a].x - blocks[b].x || a - b));
}

/** 保序回归（PAV）：在「单调不减」的约束下离 values 最近（最小二乘）的序列。 */
function isotonic(values: readonly number[]): number[] {
  const pools: { mean: number; size: number }[] = [];
  for (const value of values) {
    let pool = { mean: value, size: 1 };
    while (pools.length && pools[pools.length - 1].mean > pool.mean) {
      const last = pools.pop()!;
      const size = last.size + pool.size;
      pool = { mean: (last.mean * last.size + pool.mean * pool.size) / size, size };
    }
    pools.push(pool);
  }
  return pools.flatMap(pool => Array<number>(pool.size).fill(pool.mean));
}

/**
 * 一列从上往下摞：顺序不变、互不重叠（间隔 TIDY_ROW_GAP），整体离各自想要的上沿（`desired`，NaN = 没有要求）尽量近。
 *
 * 令 top_i = offset_i + w_i（offset 是紧挨着摞时离第一块的距离），「保序且不重叠」⇔ w 单调不减，
 * 于是「离想要的位置最近」就是对 desired − offset 做保序回归。
 */
function stackColumn(heights: readonly number[], desired: readonly number[]): number[] {
  const offsets: number[] = [];
  let sum = 0;
  for (const height of heights) {
    offsets.push(sum);
    sum += height + TIDY_ROW_GAP;
  }
  // 整列都没有要求：以 0 为中心摞起来
  if (!desired.some(Number.isFinite)) return offsets.map(offset => offset - (sum - TIDY_ROW_GAP) / 2);
  // 没有要求的块紧跟着前一块摞（排在最前面的几块紧贴着后一块）
  const want = [...desired];
  for (let i = 1; i < want.length; i++) {
    if (!Number.isFinite(want[i]) && Number.isFinite(want[i - 1])) want[i] = want[i - 1] + heights[i - 1] + TIDY_ROW_GAP;
  }
  for (let i = want.length - 2; i >= 0; i--) {
    if (!Number.isFinite(want[i])) want[i] = want[i + 1] - heights[i] - TIDY_ROW_GAP;
  }
  const shifts = isotonic(want.map((value, i) => value - offsets[i]));
  return offsets.map((offset, i) => offset + shifts[i]);
}

/** 排一组连在一起的块：写进各块的 left / top（这一组的左上角是 0,0），返回这一组的宽高。 */
function layoutGroup(list: readonly number[], blocks: Block[], graph: Graph): { width: number; height: number } {
  const byX = (a: number, b: number) => blocks[a].x - blocks[b].x || blocks[a].y - blocks[b].y || a - b;

  // 1. 断环：按原来从左到右的顺序深度优先，指回「还在路上」的节点的那条线不参与分列
  const succ = new Map<number, number[]>(list.map(at => [at, []]));
  const pred = new Map<number, number[]>(list.map(at => [at, []]));
  const state = new Map<number, "open" | "done">();
  for (const root of [...list].sort(byX)) {
    if (state.has(root)) continue;
    state.set(root, "open");
    const stack = [{ at: root, next: [...graph.succ[root]].sort(byX), index: 0 }];
    while (stack.length) {
      const top = stack[stack.length - 1];
      if (top.index >= top.next.length) {
        state.set(top.at, "done");
        stack.pop();
        continue;
      }
      const to = top.next[top.index++];
      if (state.get(to) === "open") continue;
      succ.get(top.at)!.push(to);
      pred.get(to)!.push(top.at);
      if (!state.has(to)) {
        state.set(to, "open");
        stack.push({ at: to, next: [...graph.succ[to]].sort(byX), index: 0 });
      }
    }
  }

  // 2. 分列：最长路径（离最上游有几步）；没有上游的挪到它下游紧左边那一列
  const layer = new Map<number, number>();
  const waiting = new Map(list.map(at => [at, pred.get(at)!.length]));
  const queue = [...list].sort(byX).filter(at => waiting.get(at) === 0);
  for (let i = 0; i < queue.length; i++) {
    const at = queue[i];
    layer.set(at, Math.max(0, ...pred.get(at)!.map(from => layer.get(from)! + 1)));
    for (const to of succ.get(at)!) {
      waiting.set(to, waiting.get(to)! - 1);
      if (waiting.get(to) === 0) queue.push(to);
    }
  }
  for (const at of list) {
    if (!pred.get(at)!.length && succ.get(at)!.length) layer.set(at, Math.min(...succ.get(at)!.map(to => layer.get(to)!)) - 1);
  }
  const layerValues = [...new Set(layer.values())].sort((a, b) => a - b);
  const columns = layerValues.map(value => list.filter(at => layer.get(at) === value));

  // 3. 列内顺序：先按原来的上下位置；相邻两列间的连线有交叉时，按上下游的平均名次来回调几遍（重心法），
  //    **交叉变少了才换** —— 原来就不交叉的保持原来的上下顺序，整理过的再整理也不会换来换去
  const centerY = (at: number) => blocks[at].y + blocks[at].height / 2;
  for (const column of columns) column.sort((a, b) => centerY(a) - centerY(b) || byX(a, b));
  const crossings = () => {
    let count = 0;
    for (let l = 0; l + 1 < columns.length; l++) {
      const next = new Map(columns[l + 1].map((at, index) => [at, index]));
      const pairs: [number, number][] = [];
      columns[l].forEach((at, index) => {
        for (const to of succ.get(at)!) {
          const j = next.get(to);
          if (j !== undefined) pairs.push([index, j]);
        }
      });
      for (let a = 0; a < pairs.length; a++) {
        for (let b = a + 1; b < pairs.length; b++) {
          if ((pairs[a][0] - pairs[b][0]) * (pairs[a][1] - pairs[b][1]) < 0) count++;
        }
      }
    }
    return count;
  };
  let fewest = crossings();
  if (fewest > 0) {
    let best = columns.map(column => [...column]);
    const rank = new Map<number, number>();
    const updateRank = (column: number[]) => column.forEach((at, index) => rank.set(at, (index + 0.5) / column.length));
    columns.forEach(updateRank);
    const reorder = (column: number[], neighbors: (at: number) => number[]) => {
      const key = new Map(column.map((at) => {
        const around = neighbors(at);
        return [at, around.length ? around.reduce((total, other) => total + rank.get(other)!, 0) / around.length : rank.get(at)!];
      }));
      column.sort((a, b) => key.get(a)! - key.get(b)! || rank.get(a)! - rank.get(b)!);
      updateRank(column);
    };
    const keepIfFewer = () => {
      const count = crossings();
      if (count >= fewest) return;
      fewest = count;
      best = columns.map(column => [...column]);
    };
    for (let round = 0; round < 4 && fewest > 0; round++) {
      for (let l = 1; l < columns.length; l++) reorder(columns[l], at => pred.get(at)!);
      keepIfFewer();
      for (let l = columns.length - 2; l >= 0; l--) reorder(columns[l], at => succ.get(at)!);
      keepIfFewer();
    }
    best.forEach((column, l) => { columns[l] = column; });
  }

  // 4. 横向：一列一列往右排，列宽取这一列最宽的块，块在列里靠左
  let x = 0;
  for (const column of columns) {
    for (const at of column) blocks[at].left = x;
    x += Math.max(...column.map(at => blocks[at].width)) + TIDY_COLUMN_GAP;
  }

  // 5. 纵向：先从左往右只看上游放一遍，再来回看上下游各调两遍 —— 每块尽量落在相连块的平均高度上
  const center = new Map<number, number>();
  const place = (column: number[], neighbors: (at: number) => number[]) => {
    const desired = column.map((at) => {
      const placed = neighbors(at).filter(other => center.has(other));
      if (!placed.length) return Number.NaN;
      return placed.reduce((total, other) => total + center.get(other)!, 0) / placed.length - blocks[at].height / 2;
    });
    const tops = stackColumn(column.map(at => blocks[at].height), desired);
    column.forEach((at, index) => center.set(at, tops[index] + blocks[at].height / 2));
  };
  const both = (at: number) => [...pred.get(at)!, ...succ.get(at)!];
  for (const column of columns) place(column, at => pred.get(at)!);
  for (let round = 0; round < 2; round++) {
    for (let l = columns.length - 1; l >= 0; l--) place(columns[l], both);
    for (const column of columns) place(column, both);
  }

  const minTop = Math.min(...list.map(at => center.get(at)! - blocks[at].height / 2));
  for (const at of list) blocks[at].top = center.get(at)! - blocks[at].height / 2 - minTop;
  return {
    width: Math.max(...list.map(at => blocks[at].left + blocks[at].width)),
    height: Math.max(...list.map(at => blocks[at].top + blocks[at].height))
  };
}

/** 没有连线的块排成网格：大致 16:10 的横长方块，组排得比这更宽时按组的宽度换行。 */
function layoutGrid(list: readonly number[], blocks: Block[], top: number, widest: number) {
  if (!list.length) return;
  const ordered = readingOrder(list, blocks);
  const averageWidth = ordered.reduce((total, at) => total + blocks[at].width, 0) / ordered.length;
  const perRow = Math.ceil(Math.sqrt(ordered.length * 1.6));
  const limit = Math.max(widest, perRow * averageWidth + (perRow - 1) * TIDY_GRID_GAP);
  let x = 0;
  let y = top;
  let rowHeight = 0;
  for (const at of ordered) {
    const block = blocks[at];
    if (x > 0 && x + block.width > limit + 0.5) {
      x = 0;
      y += rowHeight + TIDY_GRID_GAP;
      rowHeight = 0;
    }
    block.left = x;
    block.top = y;
    x += block.width + TIDY_GRID_GAP;
    rowHeight = Math.max(rowHeight, block.height);
  }
}

/**
 * 算出整理后每个节点的位置（左上角）。`snap` 给了就把每一块的左上角吸到这个格距上（画布开着网格吸附时）。
 * 返回所有节点的位置，没动的也在里面；调用方自己比较哪些变了。
 */
export function tidyCanvasLayout(
  nodes: readonly TidyNode[],
  edges: readonly TidyEdge[],
  options: { snap?: number } = {}
): Map<string, TidyPoint> {
  const result = new Map<string, TidyPoint>();
  if (!nodes.length) return result;
  const { blocks, blockOf } = buildBlocks(nodes);

  const graph: Graph = { succ: blocks.map(() => new Set()), pred: blocks.map(() => new Set()) };
  for (const edge of edges) {
    const from = blockOf.get(edge.source);
    const to = blockOf.get(edge.target);
    if (from === undefined || to === undefined || from === to) continue;
    graph.succ[from].add(to);
    graph.pred[to].add(from);
  }

  // 连通块（不看方向）
  const groupOf = new Array<number>(blocks.length).fill(-1);
  const groups: number[][] = [];
  for (let start = 0; start < blocks.length; start++) {
    if (groupOf[start] >= 0) continue;
    const list = [start];
    groupOf[start] = groups.length;
    for (let i = 0; i < list.length; i++) {
      for (const other of [...graph.succ[list[i]], ...graph.pred[list[i]]]) {
        if (groupOf[other] >= 0) continue;
        groupOf[other] = groups.length;
        list.push(other);
      }
    }
    groups.push(list);
  }
  const top = (list: number[]) => Math.min(...list.map(at => blocks[at].y));
  const left = (list: number[]) => Math.min(...list.map(at => blocks[at].x));
  const connected = groups.filter(list => list.length > 1).sort((a, b) => top(a) - top(b) || left(a) - left(b));
  const loose = groups.filter(list => list.length === 1).map(list => list[0]);

  let cursor = 0;
  let widest = 0;
  for (const list of connected) {
    const size = layoutGroup(list, blocks, graph);
    for (const at of list) blocks[at].top += cursor;
    cursor += size.height + TIDY_GROUP_GAP;
    widest = Math.max(widest, size.width);
  }
  layoutGrid(loose, blocks, cursor, widest);

  // 整体平移回原来的左上角；每一块整体挪（画框里的节点跟着框走）。
  // 原点和排版里的相对位置**分别**取整再相加：整理后最靠左上的那块正好落在原点上，再整理一次原点不变、位置也就不变
  const snap = options.snap && options.snap > 0 ? options.snap : 1;
  const align = (value: number) => Math.round(value / snap) * snap;
  const originX = align(Math.min(...blocks.map(block => block.x)));
  const originY = align(Math.min(...blocks.map(block => block.y)));
  const byId = new Map(nodes.map(node => [node.id, node]));
  for (const block of blocks) {
    const dx = originX + align(block.left) - block.x;
    const dy = originY + align(block.top) - block.y;
    for (const id of block.ids) {
      const node = byId.get(id)!;
      result.set(id, { x: Math.round(node.x + dx), y: Math.round(node.y + dy) });
    }
  }
  return result;
}
