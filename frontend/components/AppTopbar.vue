<script setup lang="ts">
import { Menu, Moon, Settings, Sun } from "@lucide/vue";

// 桌面版（TT-T-13）：去掉通知、会员、积分与账号面板；原账号面板的位置换成「设置」入口（TT-UX-3）。
const mobileOpen = useState("platform-sidebar-mobile-open", () => false);
const { resolved: theme, toggle: toggleTheme } = useTheme();
const themeLabel = computed(() => (theme.value === "dark" ? "切换到白天模式" : "切换到黑夜模式"));
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
      <NuxtLink to="/settings" class="portal-action icon-only" aria-label="设置" title="设置"><Settings :size="17" /></NuxtLink>
    </nav>
  </header>
</template>
