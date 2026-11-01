<script setup lang="ts">
import PageCrumb from "~/components/common/PageCrumb.vue";
import type { components } from '~/types/legacy';
import { Bell, ChevronLeft, ChevronRight, Crown, FolderHeart, KeyRound, LogOut, ReceiptText, Stamp, UserRound, Zap } from "@lucide/vue";
import { pageItems, totalPageCount } from "~/utils/pagination";

definePageMeta({ layout: "platform", middleware: "auth" });
type OrderPage = components["schemas"]["PageResultOrderResp"];

const auth = useAuthStore();
// 订单只在这一处能看到，size=5 又没有翻页，等于第 6 笔以后就查不到了
const ORDER_PAGE_SIZE = 8;
const orders = ref<OrderPage>({ list: [], total: 0, page: 1, size: ORDER_PAGE_SIZE });
const orderPage = ref(1);
const ordersLoading = ref(false);
const orderPages = computed(() => totalPageCount(orders.value.total ?? 0, ORDER_PAGE_SIZE));
const orderVisiblePages = computed(() => pageItems(orderPage.value, orderPages.value));

async function loadOrders() {
  ordersLoading.value = true;
  try {
    orders.value = await useApi()<OrderPage>(`/orders?page=${orderPage.value}&size=${ORDER_PAGE_SIZE}`);
  } finally {
    ordersLoading.value = false;
  }
}

function goToOrderPage(next: number) {
  const target = Math.min(Math.max(1, next), orderPages.value);
  if (target === orderPage.value) return;
  orderPage.value = target;
  void loadOrders();
}

const membershipLabel = computed(() => ({
  standard: "标准",
  advanced: "进阶",
  premium: "高级",
  luxury: "豪华",
  supreme: "至尊"
}[auth.membership?.levelCode ?? ""] ?? "免费用户"));

function money(cents?: number) {
  return `¥${((cents ?? 0) / 100).toLocaleString("zh-CN", { maximumFractionDigits: 2 })}`;
}

function productLabel(type?: string) {
  return type === "membership" ? "会员方案" : "积分包";
}

function statusLabel(status?: string) {
  return { unpaid: "待支付", paid: "已支付", canceled: "已关闭", refunded: "已退款" }[status ?? ""] ?? status;
}

/**
 * AI 水印开关（GAP-13）。字段是「关闭标志」`isAiWatermarkOff`，开关语义是「开启水印」，中间取反一次。
 * ⚠️ 状态未知（/auth/me 还没回来或没给这个字段）时开关禁用 —— 猜一个初值再让用户点，等于替他改了设置。
 */
// ⚠️ 挂载之后才读：服务端渲染时还没有资料（中间件只在客户端取 /auth/me），渲染出的是禁用态；
//    客户端水合时资料往往已经到了，Vue 对**属性**的水合不一致只告警不修正 —— 开关会一直卡在禁用。
const mounted = ref(false);
onMounted(() => { mounted.value = true; });
const watermarkOff = computed(() => (mounted.value && typeof auth.user?.isAiWatermarkOff === "boolean" ? auth.user.isAiWatermarkOff : null));
const watermarkSaving = ref(false);
const watermarkError = ref("");

async function toggleWatermark() {
  if (watermarkOff.value === null || watermarkSaving.value) return;
  const next = !watermarkOff.value;
  watermarkSaving.value = true;
  watermarkError.value = "";
  try {
    await auth.setWatermarkOff(next);
  } catch (cause) {
    watermarkError.value = cause instanceof Error ? cause.message : "保存失败，请稍后重试";
  } finally {
    watermarkSaving.value = false;
  }
}

async function logout() {
  await auth.logout();
  await navigateTo("/login");
}

onMounted(async () => {
  await Promise.all([auth.loadProfile(), loadOrders()]);
});
</script>

<template>
  <main class="portal-page account-page">
    <header class="account-profile">
      <span class="account-avatar">{{ auth.user?.nickname?.slice(0, 1) || 'A' }}</span>
      <div><PageCrumb><span class="page-kicker"><UserRound :size="15" />账号中心</span></PageCrumb><h1>{{ auth.user?.nickname || 'AITv 用户' }}</h1><p>ID {{ auth.user?.id }}</p></div>
    </header>

    <section class="account-summary">
      <NuxtLink to="/membership"><span><Crown :size="18" /></span><div><small>当前会员</small><strong>{{ membershipLabel }}</strong><p>{{ auth.membership?.expireAt ? `${new Date(auth.membership.expireAt).toLocaleDateString('zh-CN')} 到期` : '选择适合的创作额度' }}</p></div><ChevronRight :size="18" /></NuxtLink>
      <NuxtLink to="/credits"><span><Zap :size="18" /></span><div><small>积分余额</small><strong>{{ auth.creditBalance.toLocaleString('zh-CN') }}</strong><p>查看永久与将过期批次</p></div><ChevronRight :size="18" /></NuxtLink>
      <NuxtLink to="/works"><span><FolderHeart :size="18" /></span><div><small>个人内容</small><strong>我的作品</strong><p>管理图片、视频和音频</p></div><ChevronRight :size="18" /></NuxtLink>
    </section>

    <section id="watermark" class="account-watermark">
      <span class="account-watermark-icon"><Stamp :size="18" /></span>
      <div>
        <strong>AI 水印</strong>
        <p>开启时，渲染出的成片带「AI 生成」标识。关闭后，之后渲染的成片不再带 AI 标识；已经渲染好的成片不受影响。</p>
        <p v-if="watermarkError" class="account-watermark-error">{{ watermarkError }}</p>
      </div>
      <button
        type="button"
        role="switch"
        class="account-switch"
        :class="{ on: watermarkOff === false }"
        :aria-checked="watermarkOff === false"
        aria-label="开启 AI 水印"
        :title="watermarkOff === null ? '正在读取水印设置' : (watermarkOff ? '已关闭，点击开启' : '已开启，点击关闭')"
        :disabled="watermarkOff === null || watermarkSaving"
        @click="toggleWatermark"
      ><span /></button>
    </section>

    <div id="orders" class="section-head portal-section-head"><div><h2>订单记录</h2><p>会员与积分包支付记录</p></div><span class="order-count">{{ orders.total ?? 0 }} 笔</span></div>
    <section class="account-order-list">
      <article v-for="item in orders.list" :key="item.orderId" class="account-order-row">
        <span class="order-icon"><ReceiptText :size="17" /></span>
        <div><strong>{{ productLabel(item.type) }}</strong><small>{{ item.createdAt ? new Date(item.createdAt).toLocaleString('zh-CN') : '' }} · {{ item.orderId }}</small></div>
        <strong>{{ money(item.amount) }}</strong>
        <span class="order-status" :class="item.status">{{ statusLabel(item.status) }}</span>
      </article>
      <div v-if="!orders.list?.length" class="portal-empty">暂无订单</div>
    </section>

    <nav v-if="orderPages > 1" class="works-pager" aria-label="订单分页">
      <button type="button" title="上一页" aria-label="上一页" :disabled="orderPage <= 1 || ordersLoading" @click="goToOrderPage(orderPage - 1)">
        <ChevronLeft :size="16" />
      </button>
      <template v-for="(item, index) in orderVisiblePages" :key="`${item}-${index}`">
        <span v-if="item === 'gap'" class="works-pager-gap">…</span>
        <button
          v-else
          type="button"
          :class="{ active: item === orderPage }"
          :aria-current="item === orderPage ? 'page' : undefined"
          :disabled="ordersLoading"
          @click="goToOrderPage(item)"
        >{{ item }}</button>
      </template>
      <button type="button" title="下一页" aria-label="下一页" :disabled="orderPage >= orderPages || ordersLoading" @click="goToOrderPage(orderPage + 1)">
        <ChevronRight :size="16" />
      </button>
    </nav>

    <section class="account-menu">
      <NuxtLink to="/notifications"><Bell :size="18" /><span><strong>通知</strong><small>系统通知与生成结果</small></span><ChevronRight :size="17" /></NuxtLink>
      <NuxtLink to="/account/mcp"><KeyRound :size="18" /><span><strong>MCP 访问</strong><small>让外部 AI 工具使用你的项目</small></span><ChevronRight :size="17" /></NuxtLink>
      <button type="button" class="danger" @click="logout"><LogOut :size="18" /><span><strong>退出登录</strong><small>清除当前设备会话</small></span><ChevronRight :size="17" /></button>
    </section>
  </main>
</template>
