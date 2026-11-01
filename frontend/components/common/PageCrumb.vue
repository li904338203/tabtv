<script setup lang="ts">
import { ChevronLeft } from "@lucide/vue";
import { backTarget } from "~/utils/pageBack";

/**
 * 二级页标题上方的「< 返回 | 栏目名」。
 *
 * 样式沿用「全部项目」页已有的 `.crumb-back` / `.crumb-divider`，几处返回长得一样。
 * 去哪由 `utils/pageBack` 定：站内点进来就退回上一页，直接打开的去 `fallback`。
 */
const props = withDefaults(defineProps<{ fallback?: string }>(), { fallback: "/" });
const router = useRouter();

function goBack() {
  const target = backTarget(import.meta.client ? window.history.state?.back : null, props.fallback);
  if (target.mode === "history") router.back();
  else void router.push(target.to);
}
</script>

<template>
  <div class="page-crumb">
    <button type="button" class="crumb-back" title="返回上一页" @click="goBack"><ChevronLeft :size="15" />返回</button>
    <span class="crumb-divider" aria-hidden="true" />
    <slot />
  </div>
</template>
