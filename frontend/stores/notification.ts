import { defineStore } from "pinia";

/** `GET /notifications/unread-count` 的响应（契约里是内联对象，没有具名 schema）。 */
type UnreadCount = { unread: number };

/**
 * 顶栏未读红点。
 *
 * 单独抽个 store 而不是让每个页面各查各的：铃铛在布局里、列表在页面里，
 * 页面标已读之后必须能立刻把布局上的数字改掉，共享状态是最省事的做法。
 *
 * 轮询而不是走 WebSocket：`/ws/generation` 只推生成事件，公告与审核结果不经过它；
 * 为一个数字再开一条长连接不划算。60 秒的延迟对站内消息完全够用。
 */
export const useNotificationStore = defineStore("notification", () => {
  const REFRESH_INTERVAL = 60_000;
  const unread = ref(0);
  let timer: ReturnType<typeof setInterval> | undefined;

  async function refresh() {
    const auth = useAuthStore();
    // 未登录时这个接口必然 401，不要用可预期的失败去刷错误日志
    if (!auth.isAuthenticated) {
      unread.value = 0;
      return;
    }
    try {
      const response = await useApi()<UnreadCount>("/notifications/unread-count");
      // ⚠️ 字段是 unread（2026-11-02 修）：原先读 total，恒为 0 —— 顶栏红点、未读数、「全部已读」按钮从来不出现。
      unread.value = response.unread ?? 0;
    } catch {
      // 红点拿不到不该打断任何操作，保持上一次的数字即可
    }
  }

  /** 本地先减，等下一次 refresh 校正 —— 点开一条就等一个往返才变数字太迟钝。 */
  function decrement() {
    unread.value = Math.max(0, unread.value - 1);
  }

  function start() {
    if (timer) return;
    void refresh();
    timer = setInterval(() => void refresh(), REFRESH_INTERVAL);
  }

  function stop() {
    if (!timer) return;
    clearInterval(timer);
    timer = undefined;
  }

  return { unread, refresh, decrement, start, stop };
});
