<script setup lang="ts">
/**
 * OAuth 同意页与页面审批共用的外壳（MCPT-13）：与登录页同一套视觉（背景图 + 居中面板），
 * **不套 platform 布局** —— 没有侧栏与顶栏，免得用户以为自己在站内某个页面上操作，
 * 而这两页做的都是「替一个外部客户端做决定」。
 */
import { ShieldCheck } from "@lucide/vue";

defineProps<{ badge?: string }>();
</script>

<template>
  <main class="auth-page mcp-gate-page">
    <div class="auth-backdrop" aria-hidden="true"><img src="/media/show-green.jpg" alt=""></div>
    <section class="auth-panel mcp-gate-panel">
      <header class="auth-brand-row">
        <span class="auth-brand"><img class="brand-mark" src="/brand/logo-mark.png" alt="" draggable="false"><strong><img class="brand-word" src="/brand/logo-word.png" alt="tabtv" draggable="false"></strong></span>
        <span class="auth-security"><ShieldCheck :size="13" />{{ badge || "安全确认" }}</span>
      </header>
      <slot />
    </section>
  </main>
</template>

<style scoped>
.mcp-gate-page { place-items: center; overflow-y: auto; }
.mcp-gate-panel { width: min(560px, 100%); overflow: visible; }
</style>
