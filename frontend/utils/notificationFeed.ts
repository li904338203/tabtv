import type { components } from "@tabtv/api-client";

/**
 * 消息中心与后端契约（`/notifications`、`/notifications/read`、`/notifications/read-all`、`unread-count`）的映射。
 *
 * ⚠️ 2026-11-02 重接：页面原先照老系统的形状写（`read` / `link` / `page&size&unreadOnly` /
 * `{notificationIds}`），而新后端是 `isRead` / `linkUrl` / 两段游标 / `{id, source}` —— 结果是每条都显示未读、
 * 点了不跳转、标已读被拒、翻页与「只看未读」都不生效，未读数还读错了字段（`total` 而非 `unread`），
 * 「全部已读」按钮因此从来不出现。而这些都**不报错**。所以映射集中在这里并有测试钉住。
 */
export type Message = components["schemas"]["Message"];

type FeedPage = { items: Message[]; nextCursorAt?: string; nextCursorId?: string };

/** 两段游标（时间 + id）合成一个字符串交给 useInfiniteList；空串 = 第一页。 */
export function notificationFeedPath(cursor: string): string {
  if (!cursor) return "/notifications";
  const [at, id] = cursor.split("|");
  return `/notifications?cursorAt=${encodeURIComponent(at ?? "")}&cursorId=${encodeURIComponent(id ?? "")}`;
}

/** 后端返回 → useInfiniteList 的形状。⚠️ 两段游标任一为空即没有下一页（给 null，不能给空串以外的东西让它空转）。 */
export function notificationFeedPage(r: FeedPage): { items: Message[]; nextCursor: string | null } {
  const more = Boolean(r.nextCursorAt && r.nextCursorId);
  return { items: r.items ?? [], nextCursor: more ? `${r.nextCursorAt}|${r.nextCursorId}` : null };
}

/** 标一条已读的请求体。⚠️ `source` 必填：公告与个人通知两张表的 id 在同一个空间里，无从区分。 */
export function markReadBody(m: Pick<Message, "id" | "source">): { id: string; source: string } {
  return { id: m.id, source: m.source };
}

/** 点开一条要跳去哪里：只认站内相对地址（以 / 开头），外链与空串都不跳。 */
export function messageLink(m: Pick<Message, "linkUrl">): string {
  const url = (m.linkUrl ?? "").trim();
  return url.startsWith("/") && !url.startsWith("//") ? url : "";
}
