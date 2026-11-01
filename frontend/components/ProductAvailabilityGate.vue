<script setup lang="ts">
const props = defineProps<{ code: string }>();
const products = useProductsStore();
const product = computed(() => products.find(props.code));
const disabled = computed(() => product.value?.status === "disabled");

onMounted(() => void products.load());
</script>

<template>
  <slot v-if="!disabled" />
  <section v-else class="product-unavailable">
    <strong>{{ product?.name || '当前产品' }}已关闭</strong>
    <span>该产品当前不接受新的访问，请返回首页选择其他创作空间。</span>
    <!-- 首页即创造页（2026-11-01 起 / 重定向到 /create，原来的「工作台」没有了） -->
    <NuxtLink to="/">返回首页</NuxtLink>
  </section>
</template>
