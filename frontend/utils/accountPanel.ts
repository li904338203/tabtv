/**
 * 顶栏头像悬停面板（账号面板）的纯函数：打码、会员到期、积分拆分、菜单项。
 *
 * 判定放这里而不是组件里：用户端 Vitest 没有 DOM 环境，组件只负责渲染与交互。
 *
 * ⚠️ 原则是「拿不到就不显示」，**不造数字**：接口失败时返回 null / 空数组，
 *    由组件把那一块整个藏起来。显示一个 0 比不显示更糟 —— 用户会以为自己真的没有积分。
 */

/** 一天的毫秒数。 */
const DAY_MS = 86_400_000;

/**
 * 手机号 / 邮箱打码，中间换成 `****`。
 *
 * ⚠️ 新后端 `/auth/me` 返回的本来就是**已脱敏**的值（`138****1234`、`a***b@x.com`），
 *    再打一遍会把它打成全星号 —— 所以含 `*` 的原样返回，只对明文兜底。
 */
export function maskContact(value: string | null | undefined): string {
  const text = (value ?? "").trim();
  if (!text) return "";
  if (text.includes("*")) return text;
  const at = text.indexOf("@");
  if (at > 0) {
    const name = text.slice(0, at);
    const domain = text.slice(at);
    if (name.length <= 2) return `${name.slice(0, 1)}****${domain}`;
    return `${name.slice(0, 1)}****${name.slice(-1)}${domain}`;
  }
  if (text.length < 7) return "****";
  return `${text.slice(0, 3)}****${text.slice(-4)}`;
}

/** 面板上展示的账号：手机号优先，没有再用邮箱；都没有返回空串（组件不显示那一行）。 */
export function accountContact(user: { phone?: string | null; email?: string | null } | null | undefined): string {
  return maskContact(user?.phone) || maskContact(user?.email);
}

/** 本地时区的 `YYYY-MM-DD`。⚠️ 不用 toISOString：那是 UTC 日期，东八区凌晨会差一天。 */
export function formatLocalDate(iso: string | Date): string {
  const d = iso instanceof Date ? iso : new Date(iso);
  if (Number.isNaN(d.getTime())) return "";
  const pad = (n: number) => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}`;
}

/**
 * 两个时刻之间相隔的**本地自然日**数（b 晚于 a 为正）。
 *
 * ⚠️ 按本地日历日算，不按 24 小时整除：昨晚 23:00 过期、今早 08:00 看，
 *    用户的理解是「过期 1 天」，而 9 小时整除出来是 0。
 *    用 Date(y,m,d) 取本地零点再相减，夏令时的 23/25 小时日靠 round 吸收。
 */
export function localDayDiff(a: Date, b: Date): number {
  const da = new Date(a.getFullYear(), a.getMonth(), a.getDate()).getTime();
  const db = new Date(b.getFullYear(), b.getMonth(), b.getDate()).getTime();
  return Math.round((db - da) / DAY_MS);
}

export type MembershipLike = {
  levelName?: string;
  levelCode?: string;
  expireAt?: string;
  status?: string;
};

export type MembershipView =
  | { kind: "active"; levelName: string; expireDate: string; daysLeft: number }
  | { kind: "expired"; levelName: string; daysAgo: number }
  | { kind: "none" };

/**
 * 会员卡要显示的状态。
 *
 * - 有当前有效会员 → active（`/membership` 的 `current` 只在 `expireAt > now` 时出现）；
 * - 没有但历史里有过期的 → expired，取**最近一次到期**的那条；
 * - 都没有 → none。
 *
 * ⚠️ 历史里的 `refunded` / `superseded` 不算「过期」：前者是退掉的、后者是被升级顶替的
 *    （到期日可能还在未来）。拿它们说「已过期 N 天」是在描述一件没发生过的事。
 */
export function membershipView(
  current: MembershipLike | null | undefined,
  history: MembershipLike[] | null | undefined,
  now: Date
): MembershipView {
  if (current?.expireAt) {
    const expire = new Date(current.expireAt);
    if (!Number.isNaN(expire.getTime()) && expire.getTime() > now.getTime()) {
      return {
        kind: "active",
        levelName: current.levelName || "会员",
        expireDate: formatLocalDate(expire),
        daysLeft: Math.max(0, localDayDiff(now, expire))
      };
    }
  }
  const past = (history ?? [])
    .filter(m => m.status !== "refunded" && m.status !== "superseded" && m.expireAt)
    .map(m => ({ m, t: new Date(m.expireAt!).getTime() }))
    .filter(x => !Number.isNaN(x.t) && x.t <= now.getTime())
    .sort((x, y) => y.t - x.t)[0];
  if (past) {
    return {
      kind: "expired",
      levelName: past.m.levelName || "会员",
      daysAgo: Math.max(0, localDayDiff(new Date(past.t), now))
    };
  }
  return { kind: "none" };
}

/** 会员卡的一句话。 */
export function membershipLine(view: MembershipView): string {
  if (view.kind === "active") return `${view.levelName} · ${view.expireDate} 到期`;
  if (view.kind === "expired") {
    return view.daysAgo === 0 ? `${view.levelName}今日已过期` : `${view.levelName}已过期 ${view.daysAgo} 天`;
  }
  return "暂未开通会员";
}

/** 会员卡按钮文案：有效时是续费，其余是开通。 */
export function membershipAction(view: MembershipView): string {
  return view.kind === "active" ? "续费" : "开通会员";
}

export type CreditGrantLike = { source?: string; remaining?: number };
export type CreditPart = { source: string; label: string; amount: number };

/**
 * 积分批次来源的中文名（取值见迁移 00003 的 `ck_grant_source`）。
 * 顺序即展示顺序：用户最关心的（会员赠送、购买）排前面。
 */
const SOURCE_LABELS: ReadonlyArray<readonly [string, string]> = [
  ["membership", "会员赠送"],
  ["purchase", "充值购买"],
  ["register", "注册赠送"],
  ["promotion", "活动赠送"],
  ["expiry_compensation", "过期补偿"],
  ["admin_adjust", "人工调整"]
];

/**
 * 按批次来源把余额拆开。
 *
 * ⚠️ 只合计 `remaining > 0` 的批次；未知来源归「其他」而不是丢掉 ——
 *    丢掉的话各行之和对不上总额，而用户无从知道差额去哪了。
 * 没有批次信息（接口没给）时返回空数组，组件就只显示总额。
 */
export function creditBreakdown(grants: CreditGrantLike[] | null | undefined): CreditPart[] {
  if (!grants?.length) return [];
  const sums = new Map<string, number>();
  for (const g of grants) {
    const remaining = Math.trunc(g.remaining ?? 0);
    if (remaining <= 0) continue;
    const known = SOURCE_LABELS.some(([code]) => code === g.source);
    const key = known ? g.source! : "other";
    sums.set(key, (sums.get(key) ?? 0) + remaining);
  }
  const parts: CreditPart[] = [];
  for (const [code, label] of SOURCE_LABELS) {
    const amount = sums.get(code);
    if (amount) parts.push({ source: code, label, amount });
  }
  const other = sums.get("other");
  if (other) parts.push({ source: "other", label: "其他", amount: other });
  return parts;
}

/** 未读角标文案：超过 99 显示 99+，0 不显示。 */
export function unreadBadge(count: number): string {
  if (!count || count <= 0) return "";
  return count > 99 ? "99+" : String(count);
}

export type AccountMenuItem =
  | { key: string; kind: "link"; label: string; to: string; hint?: string; badge?: string }
  | { key: "theme"; kind: "theme"; label: string }
  | { key: "logout"; kind: "logout"; label: string };

/**
 * 面板底部的菜单。
 *
 * - 订单没有独立页面，落在账号中心的「订单记录」区块（锚点 `#orders`）；
 * - 水印开关同样在账号中心（锚点 `#watermark`）。状态拿不到（`null`）时不给提示，
 *   而不是猜一个「已开启」。
 */
export function buildAccountMenu(input: { unread: number; watermarkOff: boolean | null }): AccountMenuItem[] {
  const watermarkHint = input.watermarkOff === null ? undefined : input.watermarkOff ? "已关闭" : "已开启";
  const badge = unreadBadge(input.unread);
  return [
    { key: "account", kind: "link", label: "个人中心", to: "/account" },
    { key: "orders", kind: "link", label: "订单记录", to: "/account#orders" },
    { key: "theme", kind: "theme", label: "模式切换" },
    { key: "watermark", kind: "link", label: "AI 水印设置", to: "/account#watermark", hint: watermarkHint },
    { key: "mcp", kind: "link", label: "MCP 接入", to: "/mcp" },
    { key: "notifications", kind: "link", label: "通知", to: "/notifications", badge: badge || undefined },
    { key: "logout", kind: "logout", label: "退出登录" }
  ];
}

/**
 * 积分面板（画布顶栏 ⚡ 按钮悬停弹出，与账号面板同一个组件的 `variant="credits"`，2026-11-01）底部的入口。
 * 只放系统里真有的去处：积分明细与充值在 `/credits`，会员在 `/membership`，订单在账号中心的 `#orders`。
 * 参考产品里的「积分消耗顺序设置」「联系客服」我们没有对应能力，不放空入口。
 */
export function buildCreditMenu(): AccountMenuItem[] {
  return [
    { key: "credits", kind: "link", label: "积分明细", to: "/credits" },
    { key: "membership", kind: "link", label: "会员与订阅", to: "/membership" },
    { key: "orders", kind: "link", label: "订单记录", to: "/account#orders" }
  ];
}

/**
 * `GET /credits` 的余额（契约是 `{ balance, grants }`）。
 *
 * ⚠️ 此前四处按老结构读 `response.total`，新后端没有这个字段，读出来恒为 undefined：
 *    画布顶栏的 ⚡ 不显示数字，输入框与工具条的「积分不足」也永远判不出来，而且不报错。
 *    老结构的 `total` 留作兜底；两个都没有返回 undefined（调用方当作「不知道」，不当 0）。
 */
export function creditBalanceOf(response: unknown): number | undefined {
  if (!response || typeof response !== "object") return undefined;
  const value = response as { balance?: unknown; total?: unknown };
  if (typeof value.balance === "number" && Number.isFinite(value.balance)) return value.balance;
  if (typeof value.total === "number" && Number.isFinite(value.total)) return value.total;
  return undefined;
}
