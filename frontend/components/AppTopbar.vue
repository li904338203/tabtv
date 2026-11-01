<script setup lang="ts">
import { Bell, Crown, LogIn, Menu, Moon, Sun, Zap } from "@lucide/vue";
import AppAccountPanel from "~/components/AppAccountPanel.vue";

const auth = useAuthStore();
const notifications = useNotificationStore();
// 红点只在登录后才有意义；轮询也只在挂载着布局的页面上跑，卸载时必须停掉
onMounted(() => notifications.start());
onBeforeUnmount(() => notifications.stop());
watch(() => auth.isAuthenticated, () => void notifications.refresh());
const unreadLabel = computed(() => (notifications.unread > 99 ? "99+" : String(notifications.unread)));
const mobileOpen = useState("platform-sidebar-mobile-open", () => false);
const { resolved: theme, toggle: toggleTheme } = useTheme();
const themeLabel = computed(() => (theme.value === "dark" ? "切换到白天模式" : "切换到黑夜模式"));
const creditLabel = computed(() => auth.user ? auth.creditBalance.toLocaleString("zh-CN") : "—");
</script>

<template>
  <AppSidebar />
  <header class="portal-topbar">
    <div class="portal-context">
      <button type="button" class="portal-mobile-menu" title="打开导航" aria-label="打开导航" @click="mobileOpen = true"><Menu :size="18" /></button>
    </div>
    <nav class="portal-actions" aria-label="用户导航">
      <button type="button" class="portal-action icon-only" :aria-label="themeLabel" :title="themeLabel" @click="toggleTheme">
        <Sun v-if="theme === 'dark'" :size="17" />
        <Moon v-else :size="17" />
      </button>
      <NuxtLink
        to="/notifications"
        class="portal-action icon-only portal-action-badged"
        :aria-label="notifications.unread > 0 ? `通知，${notifications.unread} 条未读` : '通知'"
        :title="notifications.unread > 0 ? `${notifications.unread} 条未读消息` : '通知'"
      >
        <Bell :size="17" />
        <em v-if="notifications.unread > 0" class="portal-badge">{{ unreadLabel }}</em>
      </NuxtLink>
      <NuxtLink to="/membership" class="portal-action gold"><Crown :size="16" /><span>会员</span></NuxtLink>
      <NuxtLink to="/credits" class="portal-action"><Zap :size="16" /><span>{{ creditLabel }}</span></NuxtLink>
      <!-- 头像悬停弹出账号面板（个人中心、订单、模式切换、水印、MCP、通知、退出都在里面） -->
      <AppAccountPanel v-if="auth.isAuthenticated" />
      <NuxtLink v-else to="/login" class="portal-action"><LogIn :size="16" /><span>登录</span></NuxtLink>
    </nav>
  </header>
</template>
