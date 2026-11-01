<script setup lang="ts">
/**
 * 根路径就是「创造」页（2026-11-01 按用户要求）：打开 `/`、点左上角 logo 都直接进 `/create`（utils/sidebarNav 的 CREATE_PATH）。
 *
 * 此前这里是「工作台」（问候语、快速开始、最近项目、灵感与作品）。它的内容别处都有：最近项目在「AI 视频」（/video），
 * 精选作品在「创造」与「AI 视频」两页；原来的页面在 git 历史里（2026-11-01 之前的 pages/index.vue）。
 * 用路由级重定向而不是挂载后再 navigateTo：服务端渲染与站内跳转都直接落到 /create，不会先闪一下空页。
 */
definePageMeta({ redirect: "/create" });
</script>

<template>
  <div />
</template>
