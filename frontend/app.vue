<template>
  <!--
    必须包一层 NuxtLayout：只写 <NuxtPage /> 的话 layouts/ 目录会被整个忽略，
    页面里的 definePageMeta({ layout }) 静默失效 —— 表现是页面能打开、但没有侧栏，
    不报任何错。没声明 layout 的页面（登录页、画布页）因此保持全屏。
  -->
  <NuxtLayout>
    <NuxtPage />
  </NuxtLayout>
</template>
