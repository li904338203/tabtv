<script setup lang="ts">
/**
 * 设置（TT-T-17，TT-F-25~37）：桌面版新增的页面，沿用平台外壳与设计 token（TT-UX-2）。
 * 左侧分区导航 + 右侧子页（嵌套路由）。原管理端的配置能力在这里给本机使用者自己用。
 */
import { SETTINGS_SECTIONS, activeSettingsSection } from "~/utils/settingsNav";

definePageMeta({ layout: "platform" });

const route = useRoute();
const active = computed(() => activeSettingsSection(route.path));
</script>

<template>
  <main class="portal-page settings-page">
    <header class="portal-page-head compact">
      <div>
        <h1>设置</h1>
        <p>模型、技能与本机环境 · 所有数据只保存在这台电脑上</p>
      </div>
    </header>
    <div class="settings-body">
      <nav class="settings-nav" aria-label="设置分区">
        <NuxtLink
          v-for="section in SETTINGS_SECTIONS"
          :key="section.key"
          :to="section.path"
          :class="{ active: active === section.key }"
        >
          <strong>{{ section.label }}</strong>
          <small>{{ section.hint }}</small>
        </NuxtLink>
      </nav>
      <section class="settings-content">
        <NuxtPage />
      </section>
    </div>
  </main>
</template>

<style scoped>
.settings-body { display: grid; grid-template-columns: 220px minmax(0, 1fr); gap: 28px; margin-top: 24px; }
.settings-nav { display: flex; flex-direction: column; gap: 4px; position: sticky; top: 24px; align-self: start; }
.settings-nav a { display: flex; flex-direction: column; gap: 2px; padding: 10px 12px; border-radius: 8px; color: var(--portal-text-soft); text-decoration: none; }
.settings-nav a small { color: var(--portal-faint); font-size: 11px; }
.settings-nav a:hover { background: var(--portal-hover); }
.settings-nav a.active { background: var(--portal-active); color: var(--portal-text); }
.settings-content { min-width: 0; }
@media (max-width: 860px) {
  .settings-body { grid-template-columns: 1fr; }
  .settings-nav { position: static; flex-direction: row; flex-wrap: wrap; }
  .settings-nav a small { display: none; }
}
</style>
