<script setup lang="ts">
/**
 * 旧画布地址的重定向。
 *
 * 画布迁到 /video/canvas/{id} 之后这个路由必须留着：用户的收藏、浏览历史，
 * 以及此前分享出去的链接都指向旧地址，直接删掉会全部 404。
 *
 * 用 301 而不是 302：这是永久搬迁，让浏览器与搜索引擎把旧地址换掉，
 * 而不是每次访问都先来这里绕一圈。
 */
const route = useRoute();

await navigateTo(`/video/canvas/${route.params.projectId}`, { redirectCode: 301, replace: true });
</script>

<template>
  <div />
</template>
