<script setup lang="ts">
import PageCrumb from "~/components/common/PageCrumb.vue";
import { ListTodo } from "@lucide/vue";

definePageMeta({ layout: "platform", middleware: "auth" });
</script>

<template>
  <main class="portal-page notifications-page">
    <header class="portal-page-head compact"><div><PageCrumb><span class="page-kicker"><ListTodo :size="15" />任务中心</span></PageCrumb><h1>生成任务</h1><p>后续按产品统一展示排队、运行和失败任务</p></div></header>
    <section class="notification-empty"><ListTodo :size="24" /><strong>当前没有运行中的任务</strong></section>
  </main>
</template>
