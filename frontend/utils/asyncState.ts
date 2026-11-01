/**
 * 两条「前端不能替服务端下结论」的判定 —— 抽成纯函数，各自可测。
 *
 * ⚠️ 它们原本内联在 `stores/canvas.ts` 与 `stores/generation.ts` 里。
 *    内联的代价不是不好读，是**测不到**：判定错了的两个方向都不报错，
 *    而其中一个方向会让用户**再花一次钱**。
 */

/** 画布保存的四种落点。`conflict` 与 `error` 的处置完全不同，不能合并。 */
export type CanvasSaveState = "saved" | "saving" | "conflict" | "error";

/** 新后端的版本冲突码（与老后端一致）。 */
export const VERSION_CONFLICT = 40902;

/**
 * 保存失败之后进哪个态。
 *
 * ⚠️ **40902 必须单独分出来**（GAP/FLOW-09、FE-03）：
 *  - `conflict` 的含义是"服务端上有比你新的版本" —— 本地草稿要**原样留着**，
 *    让用户选择重载还是以我为准；
 *  - `error` 是"这次没存上" —— 重试即可。
 *
 * 混成一个 `error` 的话，冲突会被当成普通失败去重试，而重试**必然再次 409**；
 * 反过来把网络错误当成冲突，用户会被要求在两份内容之间做一个并不存在的选择。
 *
 * ⚠️ 无论哪个态，**都不触碰本地 nodes/edges** —— 静默丢弃用户刚画的东西
 * 是这条规则要防的唯一后果。
 */
export function canvasSaveState(cause: unknown): CanvasSaveState {
  return errorCode(cause) === VERSION_CONFLICT ? "conflict" : "error";
}

/** 服务端判「当前状态不允许这个动作」（如：套模板时服务端的画布其实不是空的）。 */
export const INVALID_STATE_TRANSITION = 40905;

/**
 * 对空画布套模板失败之后，画布该进什么态；`undefined` 表示**不动**。
 *
 * ⚠️ 版本冲突（40902）与「服务端的画布其实不空」（40905 —— 别的窗口已经画了东西）
 *    说的是同一件事：**本地看到的是旧的**，进冲突态让用户重载。
 * ⚠️ 其余失败（模板已停用、网络）画布并没有保存失败 —— 标成「保存失败」
 *    会让用户以为刚画的东西丢了，而此时画布本来就是空的。
 */
export function templateApplyState(cause: unknown): "conflict" | undefined {
  const code = errorCode(cause);
  return code === VERSION_CONFLICT || code === INVALID_STATE_TRANSITION ? "conflict" : undefined;
}

/** 从 `useApi` 抛出的错误里取业务码；取不到返回 undefined（**不要返回 0**，那是个合法码位）。 */
export function errorCode(cause: unknown): number | undefined {
  if (!cause || typeof cause !== "object" || !("data" in cause)) return undefined;
  const data = (cause as { data?: { code?: unknown } }).data;
  return typeof data?.code === "number" ? data.code : undefined;
}

/** 轮询耗尽时写回节点的状态。 */
export type PollExhausted = { status: "unknown"; error: string };

/**
 * 浏览器轮询次数用完了 —— 这**不等于任务失败**（GAP-14、AC-13、FLOW-04、FE-04）。
 *
 * 浏览器停止查询与服务端停止工作是两件事：任务多半还在跑，产物随后会落库。
 *
 * ⚠️ 标成 `failed` 有两个后果，都很贵：
 *  1. 用户会去点重试，而重试是**新建一条生成、再扣一次费**；
 *  2. 前端无权判定失败 —— 那会与服务端的退款账目对不上
 *     （服务端认为成功、前端显示失败、而积分按成功扣了）。
 *
 * ⚠️ 措辞里**不能出现"失败"二字**，否则上面第 1 条照样会发生 —— 用户是按文案行动的。
 */
export function pollExhausted(): PollExhausted {
  return { status: "unknown", error: "仍在处理中，稍后刷新查看结果" };
}

/** 终态：到了就停止轮询（否则开着页面一晚上是上万次无谓请求）。 */
const TERMINAL = new Set(["success", "failed", "cancelled"]);

/**
 * ⚠️ `cancelled` 在新后端是**独立终态**，不再用 `failed + errorCode=cancelled` 表达。
 *    少认一个终态的表现不是报错，而是那个任务**一直轮询一个永远不再变的状态**，
 *    直到 180 次耗尽，然后显示"仍在处理中" —— 而它早就取消了。
 */
export function isTerminalStatus(status: string | undefined): boolean {
  return Boolean(status && TERMINAL.has(status));
}
