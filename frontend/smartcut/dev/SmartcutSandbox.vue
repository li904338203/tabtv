<script setup lang="ts">
/**
 * 智能剪辑的开发用沙盒（SCT-20，dev/03 §3）：只在开发态经 `nuxt.config.ts` 的 `pages:extend` 注册为 `/__smartcut-sandbox`，
 * 生产构建里没有这个路由。画布接入（SCT-23）之前，剪辑台的浏览器检查都在这里做。
 *
 * - 时间线、素材条、预览素材、排版、保存都走真实的接口路径（`/api/v1/timelines/...`），由检查脚本拦截返回 fixture
 *   （`docs/agent/smartcut/evidence/_shared/smartcut-mock.mjs`）：开发服务器上并没有项目 700；
 * - 地址参数 `?project=700&timeline=701&title=…` 可换；`?readonly=1` 时检查脚本把角色给成 viewer；
 * - `window.__smartcutSandbox` 给检查脚本：读 store、重开剪辑台。
 *
 * ⚠️ 不碰用户的真实画布：这里没有任何画布与节点接口。
 */
import { defineAsyncComponent } from "vue";

const route = useRoute();
const projectId = computed(() => String(route.query.project ?? "700"));
const timelineId = computed(() => String(route.query.timeline ?? "701"));
const title = computed(() => String(route.query.title ?? "沙盒 · 智能剪辑"));
const open = ref(true);
const openCount = ref(0);
const Studio = defineAsyncComponent(() => import("~/components/smartcut/SmartcutStudio.vue"));
const store = useSmartcutStore();

function reopen() {
  open.value = true;
  openCount.value += 1;
}

onMounted(() => {
  (window as unknown as { __smartcutSandbox?: unknown }).__smartcutSandbox = {
    store,
    reopen,
    close: () => { open.value = false; },
    isOpen: () => open.value
  };
});
</script>

<template>
  <div class="sandbox">
    <p>智能剪辑沙盒（只在开发态存在）。剪辑台已{{ open ? "打开" : "关闭" }}。</p>
    <button v-if="!open" type="button" data-testid="sandbox-open" @click="reopen">打开剪辑台</button>
    <!-- 只在客户端挂：剪辑台在画布上也是点了才挂（服务端渲染出来的只读文案与客户端对不上会报水合不一致） -->
    <ClientOnly>
      <component
        :is="Studio"
        v-if="open"
        :key="openCount"
        :project-id="projectId"
        :timeline-id="timelineId"
        node-id="n_sc"
        :node-title="title"
        @close="open = false"
        @collapse="open = false"
      />
    </ClientOnly>
  </div>
</template>

<style scoped>
.sandbox { padding: 24px; color: var(--portal-text); }
</style>
