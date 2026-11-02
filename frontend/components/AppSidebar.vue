<script setup lang="ts">
import {
  Blocks,
  FilePenLine,
  FolderHeart,
  Megaphone,
  PanelLeftClose,
  PanelLeftOpen,
  ScanFace,
  Settings,
  Sparkles,
  Video,
  X
} from "@lucide/vue";
import type { Component } from "vue";
import {
  CREATE_PATH,
  MARKETING_PATH,
  isCreateActive,
  isMarketingActive,
  showCreateBadge,
  showMarketingEntry
} from "~/utils/sidebarNav";

const route = useRoute();
const products = useProductsStore();
// 营销总开关关着（/marketing/config 404）时隐藏入口；Agent 关着（available:false）照常显示（MKT-T-04 B-5）
const marketing = useMarketingConfig();
const collapsed = useState("platform-sidebar-collapsed", () => false);
const mobileOpen = useState("platform-sidebar-mobile-open", () => false);

// 角标只在上线首月显示；取一次即可，不必随时间响应式刷新。
const createBadge = showCreateBadge(new Date());

/** 图标线条比默认（2）细一档：导航里图标是辅助，粗线条会比文字还抢眼。 */
const ICON_STROKE = 1.75;

const productIcons: Record<string, Component> = {
  video: Video,
  writing: FilePenLine,
  avatar: ScanFace
};

function iconFor(code: string) {
  return productIcons[code] ?? Blocks;
}

function active(path: string, code?: string) {
  if (path === "/") return route.path === "/";
  // 画布是视频域的沉浸式子页面，停在那里时 AI 视频菜单要保持激活。
  // /canvas/ 是迁移前的旧地址，重定向生效前的一瞬间也要认。
  // /video 下的所有子页（画布、全部项目）都保持 AI 视频菜单激活
  if (code === "video" && (route.path.startsWith("/video/") || route.path.startsWith("/canvas/"))) {
    return true;
  }
  return route.path === path || route.path.startsWith(`${path}/`);
}

function closeMobile() {
  mobileOpen.value = false;
}

function toggleCollapsed() {
  collapsed.value = !collapsed.value;
  if (import.meta.client) localStorage.setItem("aitv-sidebar-collapsed", String(collapsed.value));
}

onMounted(() => {
  collapsed.value = localStorage.getItem("aitv-sidebar-collapsed") === "true";
  void products.load();
  void marketing.load();
});

watch(() => route.fullPath, closeMobile);
</script>

<template>
  <aside class="platform-sidebar" :class="{ collapsed, open: mobileOpen }" aria-label="平台导航">
    <div class="sidebar-brand-row">
      <NuxtLink to="/" class="sidebar-brand" aria-label="tabtv 首页">
        <img class="brand-mark" src="/brand/logo-mark.png" alt="" draggable="false"><strong><img class="brand-word" src="/brand/logo-word.png" alt="tabtv" draggable="false"></strong>
      </NuxtLink>
      <!--
        收起 / 展开放在右上角（对标小云雀）。收起后它叠在 logo 上、悬停时才出现 ——
        窄栏只有 68px，放不下两个并排的图标。
      -->
      <button
        type="button"
        class="sidebar-collapse-toggle"
        :title="collapsed ? '展开导航' : '收起导航'"
        :aria-label="collapsed ? '展开导航' : '收起导航'"
        :aria-expanded="!collapsed"
        @click="toggleCollapsed"
      >
        <PanelLeftOpen v-if="collapsed" :size="18" :stroke-width="ICON_STROKE" />
        <PanelLeftClose v-else :size="18" :stroke-width="ICON_STROKE" />
      </button>
      <button type="button" class="sidebar-mobile-close" title="关闭导航" aria-label="关闭导航" @click="closeMobile"><X :size="18" /></button>
    </div>

    <nav class="sidebar-navigation">
      <!--
        「创造」**不是产品模块**：写死在产品循环之前，不进 stores/products.ts 的清单 ——
        进了清单的话「产品开关」就能把它关掉，而它关掉之后 AI 视频本身还在（设计 02 §1）。
      -->
      <NuxtLink
        :to="CREATE_PATH"
        :class="['product-link', 'product-create', { active: isCreateActive(route.path) }]"
        title="创造"
      >
        <Sparkles :size="18" :stroke-width="ICON_STROKE" />
        <span>创造</span>
        <small v-if="createBadge" class="sidebar-badge-new">NEW</small>
      </NuxtLink>
      <!--
        「营销 Agent」单列在「创造」正下方（MK-1、MKT-F-01），同样写死、不进产品清单：
        它发送后落到 AI 视频的画布项目，进了清单就能被「产品开关」关掉而 AI 视频还在。
      -->
      <NuxtLink
        v-if="showMarketingEntry(marketing.state.value)"
        :to="MARKETING_PATH"
        :class="['product-link', 'product-marketing', { active: isMarketingActive(route.path) }]"
        title="营销 Agent"
      >
        <Megaphone :size="18" :stroke-width="ICON_STROKE" />
        <span>营销 Agent</span>
      </NuxtLink>

      <!-- 「创造」顶替了原「工作台」的首位（2026-10-20）；分组之间用细线分开（产品 / 内容），读屏仍能读到分组名 -->
      <div class="sidebar-divider" role="separator" aria-label="产品" />
      <NuxtLink
        v-for="product in products.visibleProducts"
        :key="product.code"
        :to="product.entryPath"
        :class="['product-link', `product-${product.code}`, { active: active(product.entryPath, product.code) }]"
        :title="product.name"
      >
        <component :is="iconFor(product.code)" :size="18" :stroke-width="ICON_STROKE" />
        <span>{{ product.name }}</span>
        <small v-if="product.status === 'preview'">预览</small>
      </NuxtLink>

      <div class="sidebar-divider" role="separator" aria-label="内容" />
      <NuxtLink to="/works" :class="{ active: active('/works') }" title="我的作品">
        <FolderHeart :size="18" :stroke-width="ICON_STROKE" /><span>我的作品</span>
      </NuxtLink>
    </nav>

    <!-- 桌面版（TT-T-13 / TT-UX-3）：任务中心、MCP、会员与积分入口删除；原「会员与积分」的位置换成「设置」 -->
    <div class="sidebar-account-navigation">
      <NuxtLink to="/settings" :class="{ active: active('/settings') }" title="设置">
        <Settings :size="18" :stroke-width="ICON_STROKE" /><span>设置</span>
      </NuxtLink>
    </div>
  </aside>
  <button v-if="mobileOpen" type="button" class="sidebar-backdrop" aria-label="关闭导航" @click="closeMobile" />
</template>
