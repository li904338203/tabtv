<script setup lang="ts">
/**
 * 积分中心：余额（将过期 / 永久）、积分包、积分流水。
 *
 * 2026-11-01 整页重接新后端（此前按老接口的形状读：余额全是 0、积分包是几张空卡片、流水为空，而且不报错）：
 * - 余额 `GET /credits` → `{ balance, grants }`，「将过期 / 永久」按批次有没有到期时间拆（utils/creditsPage）；
 * - 积分包取 `GET /storefront` 的 `packages`（价格是分）；
 * - 流水 `GET /credits/transactions` 是游标分页、没有总条数：用 useInfiniteList 滚到底自动加载，
 *   原来的「流水总数」与页码随之去掉（拿不到的数不显示，不造）。
 * ⚠️ 「购买」仍走 useCheckout —— 下单接口还没有重接新后端（见 composables/useCheckout.ts），本次不动。
 */
import PageCrumb from "~/components/common/PageCrumb.vue";
import { ArrowDownLeft, ArrowUpRight, CalendarClock, CreditCard, Infinity as InfinityIcon, Zap } from "@lucide/vue";
import type { PagedResponse } from "~/composables/useInfiniteList";
import {
  creditSummary,
  creditTransactionDetails,
  creditTransactionTitle,
  type CreditGrant,
  type CreditPackage,
  type CreditTransaction
} from "~/utils/creditsPage";

definePageMeta({ layout: "platform", middleware: "auth" });

const auth = useAuthStore();
/** 余额与积分包取到了没有：没取到时余额显示「—」、不显示 0（显示 0 用户会以为自己真的没有积分）。 */
const loaded = ref(false);
const balance = ref(0);
const grants = ref<CreditGrant[]>([]);
const packages = ref<CreditPackage[]>([]);
const summary = computed(() => creditSummary(grants.value));
const nearestExpiryText = computed(() => {
  const expireAt = summary.value.nearestExpireAt;
  return expireAt ? `${new Date(expireAt).toLocaleDateString("zh-CN")} 最近到期` : "暂无到期批次";
});
const error = ref("");
const notice = ref("");
const { processingId, checkout } = useCheckout();

const TRANSACTION_PAGE_SIZE = 20;
const transactions = useInfiniteList<CreditTransaction>({
  pageSize: TRANSACTION_PAGE_SIZE,
  fetchPage: async (_page, cursor) => {
    const query = new URLSearchParams({ limit: String(TRANSACTION_PAGE_SIZE) });
    if (cursor) query.set("cursor", cursor);
    try {
      return await useApi()<PagedResponse<CreditTransaction>>(`/credits/transactions?${query}`);
    } catch (cause) {
      error.value = cause instanceof Error ? cause.message : "积分流水加载失败";
      return null;
    }
  }
});

async function load() {
  try {
    const [credits, storefront] = await Promise.all([
      useApi()<{ balance?: number; grants?: CreditGrant[] }>("/credits"),
      useApi()<{ packages?: CreditPackage[] }>("/storefront")
    ]);
    balance.value = typeof credits.balance === "number" ? credits.balance : 0;
    grants.value = Array.isArray(credits.grants) ? credits.grants : [];
    packages.value = Array.isArray(storefront.packages) ? storefront.packages : [];
    loaded.value = true;
  } catch (cause) {
    error.value = cause instanceof Error ? cause.message : "积分信息加载失败";
  }
}

/** 价格是分；角分照实显示（¥9.9 不能四舍五入成 ¥10）。 */
function money(cents?: number) {
  return `¥${((cents ?? 0) / 100).toLocaleString("zh-CN", { maximumFractionDigits: 2 })}`;
}

function formatTime(iso: string) {
  return new Date(iso).toLocaleString("zh-CN");
}

async function purchase(item: CreditPackage) {
  if (!item.id) return;
  error.value = "";
  notice.value = "";
  try {
    await checkout("credit_package", item.id);
    await Promise.all([load(), auth.loadProfile(), transactions.reset()]);
    notice.value = `${(item.credits ?? 0).toLocaleString("zh-CN")} 积分已到账`;
  } catch (cause) {
    error.value = cause instanceof Error ? cause.message : "订单处理失败";
  }
}

onMounted(async () => {
  await Promise.all([load(), auth.loadProfile(), transactions.reset()]);
});
</script>

<template>
  <main class="portal-page credits-page">
    <header class="portal-page-head compact"><div><PageCrumb fallback="/membership"><span class="page-kicker"><Zap :size="15" />积分中心</span></PageCrumb><h1>{{ loaded ? balance.toLocaleString('zh-CN') : '—' }}</h1><p>当前可用积分</p></div></header>
    <section v-if="loaded" class="balance-band two">
      <div><span><CalendarClock :size="16" />将过期</span><strong>{{ summary.expiring.toLocaleString('zh-CN') }}</strong><small>{{ nearestExpiryText }}</small></div>
      <div><span><InfinityIcon :size="16" />永久积分</span><strong>{{ summary.permanent.toLocaleString('zh-CN') }}</strong><small>购买积分永久有效</small></div>
    </section>

    <p v-if="notice" class="portal-notice success">{{ notice }}</p><p v-if="error" class="portal-notice error">{{ error }}</p>
    <div id="packages" class="section-head portal-section-head"><div><h2>积分包</h2><p>购买后立即进入永久余额</p></div></div>
    <section class="credit-package-grid">
      <article v-for="item in packages" :key="item.id" class="credit-package-card">
        <span class="package-icon"><Zap :size="20" /></span><div><strong>{{ (item.credits ?? 0).toLocaleString('zh-CN') }}</strong><span>积分</span></div><p>{{ item.name }}</p><footer><strong>{{ money(item.priceCents) }}</strong><button type="button" :disabled="Boolean(processingId)" @click="purchase(item)"><CreditCard :size="15" />{{ processingId === item.id ? '处理中' : '购买' }}</button></footer>
      </article>
      <div v-if="loaded && !packages.length" class="portal-empty">暂无可购买的积分包</div>
    </section>

    <div class="section-head portal-section-head"><div><h2>积分流水</h2><p>赠送、购买、消耗与退还</p></div></div>
    <section class="transaction-list">
      <article v-for="item in transactions.items.value" :key="item.id" class="transaction-row">
        <span class="transaction-icon" :class="{ positive: (item.change ?? 0) > 0 }"><ArrowDownLeft v-if="(item.change ?? 0) > 0" :size="17" /><ArrowUpRight v-else :size="17" /></span>
        <!-- 明细（2026-11-01）：项目（点了打开那张画布）· 会话 / 时间线 · 模型 · 时间 · 原因，服务端按流水的引用关出来 -->
        <div>
          <strong>{{ creditTransactionTitle(item) }}</strong>
          <span class="transaction-meta">
            <template v-for="(detail, index) in creditTransactionDetails(item, formatTime)" :key="detail.key">
              <template v-if="index"> · </template>
              <NuxtLink v-if="detail.to" :to="detail.to" class="transaction-project">{{ detail.text }}</NuxtLink>
              <template v-else>{{ detail.text }}</template>
            </template>
          </span>
        </div>
        <strong :class="{ positive: (item.change ?? 0) > 0 }">{{ (item.change ?? 0) > 0 ? '+' : '' }}{{ (item.change ?? 0).toLocaleString('zh-CN') }}</strong><small>余额 {{ (item.balanceAfter ?? 0).toLocaleString('zh-CN') }}</small>
      </article>
      <div v-if="transactions.finished.value && !transactions.items.value.length" class="portal-empty">暂无积分流水</div>
      <!-- 滚到这里自动取下一页（游标分页，没有页码） -->
      <div :ref="el => (transactions.sentinel.value = el as HTMLElement)" class="transaction-sentinel">
        <span v-if="transactions.loading.value">加载中…</span>
      </div>
    </section>
  </main>
</template>
