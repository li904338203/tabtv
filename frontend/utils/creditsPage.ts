/**
 * 积分中心页（`/credits`）的纯函数。
 *
 * 2026-11-01 整页重接新后端：此前这页一直按老接口的形状读 —— 余额读 `total / expiring / permanent`（新后端是
 * `{ balance, grants }`），积分包把 `/storefront` 整个对象当数组遍历（新后端是 `{ plans, packages, balance, … }`），
 * 流水按页码读 `{ list, total }`（新后端是游标 `{ items, nextCursor, hasMore }`）。结果余额全是 0、积分包是几张
 * 空卡片、流水是空的，而且**不报错**。
 *
 * 同日流水加上明细：服务端按 `refType` 关回业务表，带出项目、操作（文生图…）、模型、会话 / 时间线名
 * （billing/repo.ListTransactions），这里把它们排成一行能读的话。
 */

/** `GET /credits` 里的一个积分批次。 */
export type CreditGrant = { id?: string; source?: string; amount?: number; remaining?: number; expireAt?: string | null };

/** `GET /storefront` 里的一个积分包（发放的是永久积分，GO-04）。 */
export type CreditPackage = { id?: string; code?: string; name?: string; credits?: number; priceCents?: number };

/** `GET /credits/transactions` 的一条流水。项目 / 操作 / 模型 / 标题是展示用的关联，关不上就没有。 */
export type CreditTransaction = {
  id?: string;
  type?: string;
  change?: number;
  balanceAfter?: number;
  refType?: string;
  refId?: string;
  reason?: string;
  createdAt?: string;
  projectId?: string;
  projectName?: string;
  projectDeleted?: boolean;
  operationCode?: string;
  operationName?: string;
  modelName?: string;
  refTitle?: string;
};

export type CreditSummary = { expiring: number; nearestExpireAt: string | null; permanent: number };

/**
 * 余额按到期与否拆开：有到期时间的批次是「将过期」（并给出其中最早的到期日），没有的是「永久积分」。
 * 只算 `remaining > 0` 的批次。没有批次信息时两项都是 0、没有到期日。
 */
export function creditSummary(grants: readonly CreditGrant[] | null | undefined): CreditSummary {
  let expiring = 0;
  let permanent = 0;
  let nearest = Number.POSITIVE_INFINITY;
  let nearestExpireAt: string | null = null;
  for (const grant of grants ?? []) {
    const remaining = Math.trunc(grant.remaining ?? 0);
    if (remaining <= 0) continue;
    if (!grant.expireAt) {
      permanent += remaining;
      continue;
    }
    expiring += remaining;
    const time = new Date(grant.expireAt).getTime();
    if (!Number.isNaN(time) && time < nearest) {
      nearest = time;
      nearestExpireAt = grant.expireAt;
    }
  }
  return { expiring, nearestExpireAt, permanent };
}

const TYPE_LABELS: Record<string, string> = {
  grant: "积分到账",
  consume: "积分消耗",
  refund: "失败退还",
  expire: "到期清零",
  adjust: "人工调整"
};
/**
 * 按 `refType` 细分（账本里实际出现的取值：消耗是 generation / script_llm / agent_turn / render，
 * 到账是批次来源 purchase / membership / …，调整是 order_refund / admin_adjust）。
 */
const REF_LABELS: Record<string, Record<string, string>> = {
  consume: { generation: "生成消耗", script_llm: "提示词合成", agent_turn: "Agent 对话", render: "成片渲染", workflow_run: "整图运行" },
  grant: {
    purchase: "积分购买",
    membership: "会员赠送",
    register: "注册赠送",
    promotion: "活动赠送",
    expiry_compensation: "过期补偿",
    admin_adjust: "人工发放"
  },
  // 订单退款收回积分记的是 adjust（不是 refund —— 那在这张表里是「任务失败还给你」，方向相反）
  adjust: { order_refund: "订单退款回收", admin_adjust: "人工调整" }
};

/**
 * 流水的标题：类型说清方向，再按 `refType` 细分；生成带上操作名（「生成消耗 · 文生图」「失败退还 · 图生视频」）。
 * 不认识的类型原样显示，不藏。
 */
export function creditTransactionTitle(item: Pick<CreditTransaction, "type" | "refType" | "operationName">): string {
  const type = item.type ?? "";
  const label = REF_LABELS[type]?.[item.refType ?? ""] ?? TYPE_LABELS[type] ?? (type || "积分变动");
  return item.operationName ? `${label} · ${item.operationName}` : label;
}

export type CreditTransactionDetail = { key: string; text: string; to?: string };

/**
 * 标题下面那一行：项目（能打开就给链接）· 模型 · 会话 / 时间线 · 时间 · 原因。
 *
 * - 项目已删除：名字照给，后面注明，不给链接（打开会 404）；
 * - 原因与标题重复时不再写一遍：生成消耗的原因就是操作编码（text2img），提示词合成 / Agent 对话的原因就是类型名；
 *   退还与人工调账的原因（「模型服务暂时不可用」「订单 123」）才是有用的。
 */
export function creditTransactionDetails(item: CreditTransaction, formatTime: (iso: string) => string): CreditTransactionDetail[] {
  const details: CreditTransactionDetail[] = [];
  if (item.projectName) {
    const name = `项目「${item.projectName}」`;
    details.push(item.projectId && !item.projectDeleted
      ? { key: "project", text: name, to: `/video/canvas/${item.projectId}` }
      : { key: "project", text: item.projectDeleted ? `${name}（已删除）` : name });
  }
  if (item.refTitle) details.push({ key: "title", text: item.refType === "render" ? `时间线「${item.refTitle}」` : `会话「${item.refTitle}」` });
  if (item.modelName) details.push({ key: "model", text: item.modelName });
  if (item.createdAt) details.push({ key: "time", text: formatTime(item.createdAt) });
  const reason = item.reason?.trim();
  const title = creditTransactionTitle(item);
  if (reason && reason !== item.operationCode && reason !== item.operationName && !title.startsWith(reason)) {
    details.push({ key: "reason", text: reason });
  }
  return details;
}
