<script setup lang="ts">
import PageCrumb from "~/components/common/PageCrumb.vue";
import { AlertTriangle, Bell, CheckCheck, CircleCheck, Loader, Megaphone, ShieldX } from "@lucide/vue";
import { markReadBody, messageLink, notificationFeedPage, notificationFeedPath, type Message } from "~/utils/notificationFeed";

definePageMeta({ layout: "platform", middleware: "auth" });

const PAGE_SIZE = 20;
const api = useApi();
const notifications = useNotificationStore();
const marking = ref(false);

/**
 * 消息列表（2026-11-02 重接新契约：两段游标、isRead、linkUrl，见 utils/notificationFeed.ts）。
 * 去掉了「只看未读」：后端没有这个筛选（原来的 unreadOnly 参数一直被忽略，那个标签点了等于没点），
 * 未读的有红点、整页一个「全部已读」就够了。
 */
const feed = useInfiniteList<Message>({
  pageSize: PAGE_SIZE,
  fetchPage: async (_page, cursor) => notificationFeedPage(await api(notificationFeedPath(cursor)))
});

/**
 * 图标按类型分。
 *
 * 失败与驳回刻意用不同的图标：一个是系统没做成（会退积分），一个是内容被判违规（不退），
 * 用户要能一眼分清，否则会把审核驳回当成故障反复重试。
 */
const ICONS: Record<string, typeof Bell> = {
  generation_success: CircleCheck,
  generation_failed: AlertTriangle,
  work_approved: CircleCheck,
  work_rejected: ShieldX,
  announcement: Megaphone
};

const TYPE_LABELS: Record<string, string> = {
  generation_success: "生成完成",
  generation_failed: "生成失败",
  work_approved: "审核通过",
  work_rejected: "审核未通过",
  announcement: "系统公告",
  system: "系统消息"
};

function iconOf(item: Message) {
  return ICONS[item.type ?? ""] ?? Bell;
}

function labelOf(item: Message) {
  return TYPE_LABELS[item.type ?? ""] ?? "系统消息";
}

function formatTime(value?: string) {
  if (!value) return "";
  return new Date(value).toLocaleString("zh-CN");
}

/** 页面上还有没有未读的：顶栏的数字可能稍晚（60 秒轮询），按钮可用与否两者取其一即可。 */
const hasUnread = computed(() => notifications.unread > 0 || feed.items.value.some(item => !item.isRead));

/**
 * 点开一条：标已读（只能标已读，没有「设为未读」），有站内链接就跳过去。
 *
 * 先本地置为已读再发请求：等一个往返才变灰会让人以为没点中，而这个操作失败的后果只是红点多留一会儿。
 */
async function open(item: Message) {
  if (!item.isRead) {
    item.isRead = true;
    notifications.decrement();
    try {
      await api("/notifications/read", { method: "POST", body: markReadBody(item) });
    } catch {
      // 标已读失败不打断跳转；下一次刷新会把真实状态带回来
    }
  }
  const link = messageLink(item);
  if (link) await navigateTo(link);
}

async function markAllRead() {
  if (marking.value || !hasUnread.value) return;
  marking.value = true;
  try {
    await api("/notifications/read-all", { method: "POST" });
    feed.items.value.forEach((item) => { item.isRead = true; });
    await notifications.refresh();
  } finally {
    marking.value = false;
  }
}

onMounted(() => {
  void notifications.refresh();
});
</script>

<template>
  <main class="portal-page notifications-page">
    <header class="portal-page-head compact">
      <div><PageCrumb><span class="page-kicker"><Bell :size="15" />通知中心</span></PageCrumb><h1>消息</h1><p>系统公告、生成结果与审核通知</p></div>
      <button
        type="button"
        class="portal-action"
        data-testid="mark-all-read"
        :disabled="marking || !hasUnread"
        :title="hasUnread ? '把所有消息标为已读' : '没有未读消息'"
        @click="markAllRead"
      ><CheckCheck :size="15" />全部已读<span v-if="notifications.unread > 0" class="notification-count">{{ notifications.unread }}</span></button>
    </header>

    <section class="notification-list">
      <article
        v-for="item in feed.items.value"
        :key="`${item.source}-${item.id}`"
        class="notification-row"
        :class="{ unread: !item.isRead, clickable: Boolean(messageLink(item)) || !item.isRead }"
        @click="open(item)"
      >
        <span class="notification-icon" :class="item.type"><component :is="iconOf(item)" :size="17" /></span>
        <div>
          <strong>{{ item.title }}<i v-if="!item.isRead" class="notification-dot" /></strong>
          <p v-if="item.content">{{ item.content }}</p>
          <span>{{ labelOf(item) }} · {{ formatTime(item.createdAt) }}</span>
        </div>
      </article>

      <div v-if="feed.loading.value" class="notification-loading"><Loader :size="16" class="spin" />加载中</div>
      <div v-else-if="!feed.items.value.length" class="notification-empty">
        <Bell :size="24" />
        <strong>暂无新通知</strong>
      </div>
      <!-- 哨兵始终渲染：包在 v-if 里的话空列表时它不存在，滚动加载永远不会被触发 -->
      <div :ref="el => (feed.sentinel.value = el as HTMLElement)" class="notification-sentinel" />
    </section>
  </main>
</template>
