<script setup lang="ts">
/**
 * 直接引用地址的 `<video>`（TT-T-16）：渲染的就是一个原样的 `<video>`（属性、class、事件、作用域样式都透传），
 * 只是在 Linux 桌面上把直接地址换成 blob 地址（`usePlayableSrc`，dev/01 R-13）。
 * 给列表里逐项出现的视频用；需要拿 `<video>` 元素 ref 的地方直接用 `usePlayableSrc`。
 */
defineOptions({ inheritAttrs: false });
const props = defineProps<{ src?: string | null }>();
const resolved = usePlayableSrc(() => props.src);
</script>

<template>
  <video v-bind="$attrs" :src="resolved" />
</template>
