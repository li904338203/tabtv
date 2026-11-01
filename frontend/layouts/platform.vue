<script setup lang="ts">
/**
 * 平台布局：侧边导航 + 顶部栏 + 内容区。
 *
 * 收敛此前每个页面各写一遍的 `<div class="portal-shell"><AppTopbar />…` ——
 * 重复十份的直接后果是改导航要改十个文件，漏一个就出现一张没有侧栏的孤儿页面。
 *
 * 不套这个布局的只有两类：登录页（未登录不该看到导航），
 * 以及画布这种全屏沉浸式编辑页（自己接管整个视口）。
 */
</script>

<template>
  <div class="portal-shell">
    <AppTopbar />
    <slot />
  </div>
</template>
