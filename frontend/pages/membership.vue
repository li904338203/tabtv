<script setup lang="ts">
import PageCrumb from "~/components/common/PageCrumb.vue";
import { Check, Crown, Sparkles, Zap } from "@lucide/vue";

definePageMeta({ layout: "platform", middleware: "auth" });
/**
 * ⚠️ **新后端的商品目录是扁平的**：`/storefront` 返回
 * `{plans:[{id,levelCode,levelName,levelSort,period,priceCents,giftCredits}], packages, balance}`，
 * 而这个页面要的是**按等级分组**（`level.plans[]`，每个 plan 带 `price` / `creditsGrant`）。
 *
 * 不是字段改名，是结构不同 —— 照老类型读的话 `catalog.levels` 恒为
 * undefined，页面**一个套餐都不显示，而且不报错**（v-for 遍历空数组）。
 *
 * 这里把扁平列表**就地折成分组**，不改模板：改模板等于把"后端换了个形状"
 * 这件事散进 UI 的十几处，而那些地方将来还要再改回去。
 */
type StorePlan = {
  id?: string
  levelCode?: string
  levelName?: string
  levelSort?: number
  period?: string
  priceCents?: number
  giftCredits?: number
}
type Storefront = { plans?: StorePlan[]; packages?: unknown[]; balance?: number }
type Plan = { id?: string; period?: string; price?: number; creditsGrant?: number; models?: unknown[] }
type Level = { code?: string; name?: string; sort?: number; plans?: Plan[] }
type Catalog = { levels: Level[] }
type Membership = { levelCode?: string; levelSort?: number; levelName?: string; expireAt?: string; status?: string; availableModels?: unknown[] }

const auth = useAuthStore();
const catalog = ref<Catalog>({ levels: [] });
const current = ref<Membership>({ availableModels: [] });
const period = ref("year");
const notice = ref("");
const error = ref("");
const { processingId, checkout } = useCheckout();
const periods = [
  { code: "year", label: "年付" },
  { code: "quarter", label: "季付" },
  { code: "month", label: "月付" }
];

/** 把扁平的 plans 折成「等级 → 该等级的各周期套餐」。 */
function groupByLevel(store: Storefront): Catalog {
  const byCode = new Map<string, Level>();
  for (const p of store.plans ?? []) {
    const code = p.levelCode ?? "";
    let lv = byCode.get(code);
    if (!lv) {
      lv = { code, name: p.levelName, sort: p.levelSort, plans: [] };
      byCode.set(code, lv);
    }
    lv.plans!.push({
      id: p.id,
      period: p.period,
      // 模板里的 `money()` 收的是**分**，而新后端给的就是分 —— 直接传，不要先除 100。
      price: p.priceCents,
      creditsGrant: p.giftCredits,
      models: [],
    });
  }
  // 按等级排序：不排的话 Map 的插入序取决于后端返回顺序，
  // 而套餐卡片的左右位置会随之变动 —— 用户每次进来看到的顺序不一样。
  return { levels: [...byCode.values()].sort((a, b) => (a.sort ?? 0) - (b.sort ?? 0)) };
}

async function load() {
  const [store, membership] = await Promise.all([
    useApi()<Storefront>("/storefront"),
    useApi()<Record<string, unknown>>("/membership")
  ]);
  catalog.value = groupByLevel(store);
  // ⚠️ `/membership` 没有会员时返回 `{history:[]}`（没有 current 字段），
  //    不能直接整个赋过去 —— 那样 `current.levelSort` 是 undefined，
  //    而按钮文案会全部退回"选择 XX"，看不出"已经是这个等级了"。
  current.value = (membership.current ?? {}) as Membership;
}

function selectedPlan(plans: Plan[] | undefined) {
  return plans?.find(plan => plan.period === period.value);
}

function money(cents?: number) {
  return `¥${((cents ?? 0) / 100).toLocaleString("zh-CN", { maximumFractionDigits: 0 })}`;
}

function actionLabel(levelSort?: number, levelName?: string) {
  if (!current.value.levelSort) return `选择${levelName}`;
  if ((levelSort ?? 0) < current.value.levelSort) return "不可降级";
  if (levelSort === current.value.levelSort) return "续订当前方案";
  return `升级至${levelName}`;
}

async function purchase(plan: Plan | undefined) {
  if (!plan?.id) return;
  error.value = "";
  notice.value = "";
  try {
    await checkout("membership", plan.id);
    await Promise.all([load(), auth.loadProfile()]);
    notice.value = "会员权益已更新";
  } catch (cause) {
    error.value = cause instanceof Error ? cause.message : "订单处理失败";
  }
}

onMounted(async () => {
  await Promise.all([load(), auth.loadProfile()]);
});
</script>

<template>
  <main class="portal-page membership-page">
    <header class="portal-page-head">
      <div><PageCrumb><span class="page-kicker"><Crown :size="15" />会员中心</span></PageCrumb><h1>选择创作额度</h1><p>模型权限与积分额度随等级同步生效</p></div>
      <div class="current-membership"><span>当前方案</span><strong>{{ current.levelName || '免费用户' }}</strong><small>{{ current.expireAt ? `${new Date(current.expireAt).toLocaleDateString('zh-CN')} 到期` : '暂无有效会员' }}</small></div>
    </header>

    <div class="period-switch" role="tablist"><button v-for="item in periods" :key="item.code" type="button" :class="{ active: period === item.code }" @click="period = item.code">{{ item.label }}</button></div>
    <p v-if="notice" class="portal-notice success">{{ notice }}</p><p v-if="error" class="portal-notice error">{{ error }}</p>

    <section class="membership-grid">
      <article v-for="level in catalog.levels" :key="level.code" class="membership-card" :class="{ featured: level.code === 'premium', current: level.code === current.levelCode }">
        <header><div><span class="level-index">0{{ Math.max(1, (level.sort ?? 10) / 10) }}</span><h2>{{ level.name }}</h2></div><Crown v-if="level.code === current.levelCode" :size="18" /></header>
        <div class="plan-price"><strong>{{ money(selectedPlan(level.plans)?.price) }}</strong><span>/ {{ period === 'year' ? '年' : period === 'quarter' ? '季' : '月' }}</span></div>
        <div class="plan-credit"><Zap :size="16" /><strong>{{ (selectedPlan(level.plans)?.creditsGrant ?? 0).toLocaleString('zh-CN') }}</strong><span>积分一次到账</span></div>
        <ul><li><Check :size="15" />{{ selectedPlan(level.plans)?.models?.length ?? 0 }} 个可用模型</li><li><Check :size="15" />失败、取消自动退还</li><li><Sparkles :size="15" />配置驱动画布全能力</li></ul>
        <button type="button" class="purchase-button" :disabled="Boolean(processingId) || Boolean(current.levelSort && (level.sort ?? 0) < current.levelSort)" @click="purchase(selectedPlan(level.plans))">{{ processingId === selectedPlan(level.plans)?.id ? '处理中' : actionLabel(level.sort, level.name) }}</button>
      </article>
    </section>
  </main>
</template>
